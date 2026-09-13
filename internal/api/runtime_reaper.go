package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Q0 PR-2: Worker-slot occupancy, task execution token, and the workspace
// reaper. Plan references: D1 (slot), D5 (execution token), 2.1-1 (token
// write ordering + cross-storage reconciliation), 2.2-1/2 (reaper lease
// criterion + grace base), 2.1-3 (lifecycle + deployment drain).

// runtimeReaperInterval is the reaper loop cadence.
const runtimeReaperInterval = 60 * time.Second

// runtimeReaperLeaseGrace is how long past lease expiry a running run is left
// alone before the reaper force-fails it. Grace counts from the LEASE EXPIRY,
// not from node disconnection: a healthy node renews every 30s to now+90s, so
// total observed reap latency after node death ≈ lease(90s) + grace(180s) ≈
// 270s. Tune with that formula in mind.
const runtimeReaperLeaseGrace = 180 * time.Second

// runOccupiesWorkerSlot is the single source of truth for "this run holds its
// Worker's execution slot": running with an unexpired lease and a persisted
// slot_class that is not readonly. Shared by the claim path, the scheduler's
// agent-busy checks (via runtimeRunBlocksAgent), and the reaper. Queued runs
// never occupy a slot.
func runOccupiesWorkerSlot(run controldb.RuntimeRun, now time.Time) bool {
	return controldb.RunOccupiesWorkerSlot(run, now)
}

// runtimeRunBlocksAgent keeps its historical name/semantics for dispatch
// gating (a queued run means "already dispatched, don't dispatch again") but
// slot occupancy is now decided exclusively by runOccupiesWorkerSlot.
func runtimeRunBlocksAgent(run controldb.RuntimeRun, now time.Time) bool {
	switch strings.TrimSpace(run.Status) {
	case "queued":
		return true
	case "running":
		return runOccupiesWorkerSlot(run, now)
	default:
		return false
	}
}

// ── Task execution token (D5) ────────────────────────────────────────────────

// setTaskActiveRuntimeRun stamps the task with the run that is about to
// execute it. Call ordering (plan 2.1-1): the run INSERT must succeed first;
// if this task write fails the caller MUST treat the run as undispatchable
// (GPT 收口 6-1: the stamp is the run's ticket — a run without a stamped task
// must not reach a node, or its finish would be dropped by the fence and the
// task would wedge in_progress with no token and no transition). Stamp and
// clear share one mutex so an old run's clear can never interleave between a
// new dispatch's read and write (GPT fix 3: the token is a real fence, not
// advisory).
func (s *Server) setTaskActiveRuntimeRun(project, agent, taskID, runID string) error {
	if s == nil || s.ts == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return fmt.Errorf("runtime token stamp: task and run are required")
	}
	s.runtimeTaskTokenMu.Lock()
	defer s.runtimeTaskTokenMu.Unlock()
	task, err := s.ts.GetTask(project, agent, taskID)
	if err != nil || task == nil {
		return fmt.Errorf("runtime token stamp: task %s not found: %w", taskID, err)
	}
	if task.ActiveRuntimeRunID == runID {
		return nil
	}
	task.ActiveRuntimeRunID = runID
	task.UpdatedAt = time.Now().UTC()
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		return fmt.Errorf("runtime token stamp: task write failed: %w", err)
	}
	return nil
}

// clearTaskActiveRuntimeRunIfRun clears the task's execution token only when
// it still names runID — the "last conditional update wins" guarantee behind
// reaper-vs-finish races: whichever side runs second finds the field already
// changed and does nothing. The check and the write are serialized under the
// same mutex as the stamp, so a stale clear that read the field before a new
// stamp cannot clobber the new dispatch's fence.
func (s *Server) clearTaskActiveRuntimeRunIfRun(project, agent, taskID, runID string) bool {
	if s == nil || s.ts == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return false
	}
	s.runtimeTaskTokenMu.Lock()
	defer s.runtimeTaskTokenMu.Unlock()
	task, err := s.ts.GetTask(project, agent, taskID)
	if err != nil || task == nil {
		return false
	}
	if task.ActiveRuntimeRunID != runID {
		return false
	}
	task.ActiveRuntimeRunID = ""
	task.UpdatedAt = time.Now().UTC()
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		slog.Warn("runtime task token clear failed; reconciling on next reaper pass", "run", runID, "task", taskID, "error", err)
		return false
	}
	return true
}

// clearStaleTaskRuntimeToken is the reaper-side reconciliation for the
// cross-storage window (plan 2.1-1 direction 2): a task whose token points at
// a run that is already terminal or no longer exists is stale. Before
// releasing the token the transition the run never got to apply is REPLAYED
// through the fenced section (GPT 收口 4d recovery path) — a reaped run drives
// the unified infra backoff, a succeeded run completes the task, a failed run
// applies backoff or done_failed — so a persist failure in any finish path
// converges on the next sweep instead of leaving the task half-transitioned.
// Only after the transition succeeds (or reports there was nothing to do) is
// the token cleared and audited.
func (s *Server) clearStaleTaskRuntimeToken(workspaceID, project, agent string, task *entity.Task) bool {
	if s == nil || s.ts == nil || task == nil {
		return false
	}
	runID := strings.TrimSpace(task.ActiveRuntimeRunID)
	if runID == "" {
		return false
	}
	run, found, err := s.controlDB.RuntimeRunByID(workspaceID, runID)
	if err != nil {
		// Query failure: fail-closed, retry next cycle.
		return false
	}
	if found && (run.Status == "queued" || run.Status == "running") {
		return false
	}
	// Replay the run's task transition under the fence. The token still names
	// runID (checked above within this same pass), so the fence check inside
	// transitions the task exactly when the token survives — if a concurrent
	// finish snuck in first, NotOurs leaves everything untouched.
	transitioned := false
	if task.Status.IsTerminal() {
		transitioned = true // already converged by a previous pass
	} else {
		outcome := s.replayTerminalRunTaskTransition(workspaceID, project, agent, run, found, task)
		switch outcome {
		case fencedTransitionApplied, fencedTransitionSkipped:
			// The replay itself released the fence in its own write.
			transitioned = true
		case fencedTransitionTaskMissing:
			// The task was gone by replay time — the fence died with it.
			transitioned = true
		case fencedTransitionNotOurs:
			// A concurrent actor took the task — re-check: if the token is
			// gone the task is someone else's business now.
			if cur, err := s.ts.GetTask(project, agent, task.ID); err == nil && cur != nil && cur.ActiveRuntimeRunID == "" {
				transitioned = true
			}
		}
	}
	if !transitioned {
		return false
	}
	if !s.clearTaskActiveRuntimeRunIfRun(project, agent, task.ID, runID) {
		// Token already released by the replay's write — that is success.
		if cur, err := s.ts.GetTask(project, agent, task.ID); err == nil && cur != nil && cur.ActiveRuntimeRunID == "" {
			s.auditLog(auditLogInput{
				WorkspaceID:  workspaceID,
				Action:       "task.runtime_token_orphan",
				ResourceType: "task",
				ResourceID:   task.ID,
				Summary:      "Replayed the run's task transition; execution token already released by the replay",
				After: map[string]any{
					"runId":       runID,
					"runTerminal": found,
				},
			})
			return true
		}
		return false
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "task.runtime_token_orphan",
		ResourceType: "task",
		ResourceID:   task.ID,
		Summary:      "Cleared stale runtime execution token after replaying the run's task transition",
		After: map[string]any{
			"runId":       runID,
			"runTerminal": found,
		},
	})
	return true
}

// replayTerminalRunTaskTransition re-applies the task transition a terminal
// run should have driven but whose fenced persist failed (or the finish never
// arrived, e.g. reaped run). It reuses the exact mutations of the finish and
// reaper paths — via transitionReapedTask for reaped runs and a finish-shaped
// mutate for the rest — so recovery and the original path cannot diverge.
// project/agent are the DISCOVERY key of the task (GPT 收口 6-4): the run's
// stored agent identity can drift (rename/alias), but the task was found
// under the caller's key — releasing its fence must address the same key.
func (s *Server) replayTerminalRunTaskTransition(workspaceID, project, agent string, run controldb.RuntimeRun, found bool, task *entity.Task) fencedTaskTransitionOutcome {
	taskID := strings.TrimSpace(run.TaskID)
	if taskID == "" && task != nil {
		// The run row is gone (or lost its identity) — the task that carries
		// the token is the authority on what this run was executing.
		taskID = strings.TrimSpace(task.ID)
	}
	if !found {
		// Run row already cleaned up: nothing defines the intended outcome —
		// just release the fence, addressed by the task's discovery key.
		return s.fencedTaskTransition(workspaceID, project, agent, taskID, run.ID, nil)
	}
	if strings.EqualFold(strings.TrimSpace(run.ErrorCode), "lease_expired") {
		// The reaped-path transition re-resolves the task itself; it needs the
		// run identity for the workflow engine, so pass the run as-is.
		s.transitionReapedTask(workspaceID, run)
		return fencedTransitionApplied
	}
	switch strings.ToLower(strings.TrimSpace(run.Status)) {
	case "succeeded":
		return s.fencedTaskTransition(workspaceID, project, agent, run.TaskID, run.ID, func(task *entity.Task) fenceDecision {
			if task.Status.IsTerminal() {
				return fenceDecisionSkip
			}
			prev := task.Status
			now := time.Now().UTC()
			task.Status = entity.TaskStatusDoneSuccess
			resetInfraFailureStreak(task)
			task.ArchivedAt = &now
			task.UpdatedAt = now
			entity.ApplyStatusTimestamps(task, prev, now)
			return fenceDecisionApply
		})
	case "failed":
		return s.fencedTaskTransition(workspaceID, project, agent, run.TaskID, run.ID, func(task *entity.Task) fenceDecision {
			if task.Status.IsTerminal() {
				return fenceDecisionSkip
			}
			if task.Status != entity.TaskStatusInProgress && task.Status != entity.TaskStatusPending {
				return fenceDecisionSkip
			}
			prev := task.Status
			now := time.Now().UTC()
			task.LastError = firstNonEmpty(strings.TrimSpace(run.ErrorMessage), strings.TrimSpace(run.ErrorCode), "runtime run failed")
			if isRuntimeInfraFailureCode(run.ErrorCode) {
				if !applyInfraFailureBackoffMutation(task, run.ErrorCode) {
					return fenceDecisionSkip
				}
				return fenceDecisionApply
			}
			task.Status = entity.TaskStatusDoneFailed
			task.ArchivedAt = &now
			task.UpdatedAt = now
			entity.ApplyStatusTimestamps(task, prev, now)
			return fenceDecisionApply
		})
	default:
		// cancelled or unknown terminal status — just release the fence.
		return s.fencedTaskTransition(workspaceID, run.ProjectID, run.AgentID, run.TaskID, run.ID, nil)
	}
}

// taskTokenOwnedByRun is the fence check behind every reaper-side task
// transition (GPT fix 3): the task's ActiveRuntimeRunID must still name THIS
// run, or the transition is not ours to make.
func (s *Server) taskTokenOwnedByRun(project, agent string, task *entity.Task, runID string) bool {
	if s == nil || task == nil {
		return false
	}
	return strings.TrimSpace(task.ActiveRuntimeRunID) == strings.TrimSpace(runID) && strings.TrimSpace(runID) != ""
}

// fenceDecision is the three-state result of the fenced transition's mutate:
// the mutation decides IN MEMORY only — it never writes the task store itself,
// so the helper stays the single persist owner.
type fenceDecision int

const (
	// fenceDecisionApply: the task was mutated in memory; the helper must
	// persist it (and archive it when the mutation set ArchivedAt), then
	// release the token.
	fenceDecisionApply fenceDecision = iota
	// fenceDecisionSkip: nothing to transition — the helper releases the
	// token without persisting task-state changes.
	fenceDecisionSkip
	// fenceDecisionRetry: the mutation's own side-state writes (workflow
	// engine, IM notification, …) failed or must be retried — the helper
	// keeps the token while the run is live, or releases it to the sweep
	// once the run is terminal.
	fenceDecisionRetry
)

// fencedTaskTransitionOutcome classifies the result of the unified task
// runtime transition critical section.
type fencedTaskTransitionOutcome int

const (
	// fencedTransitionNotOurs: the token no longer names this run — a newer
	// dispatch owns the task, its state must not be touched.
	fencedTransitionNotOurs fencedTaskTransitionOutcome = iota
	// fencedTransitionSkipped: task is terminal / not in a transitionable
	// state — nothing to do, token released when persist succeeded.
	fencedTransitionSkipped
	// fencedTransitionApplied: mutation persisted AND token released.
	fencedTransitionApplied
	// fencedTransitionPersistFailed: the task-store write failed — the token
	// is deliberately NOT released so the transition stays retryable (the
	// reaper sweep or the next finish converges).
	fencedTransitionPersistFailed
	// fencedTransitionTaskMissing: task not found in the store.
	fencedTransitionTaskMissing
)

// fencedTaskTransition is THE single critical section for "move the task that
// run R is executing" (GPT 收口 1+2): under one token-mutex hold it reads the
// task, verifies the fence (ActiveRuntimeRunID == runID), applies the caller's
// mutation, and persists it. Both the finish path (applyFencedFinishTransition)
// and the reaper (transitionReapedTask) go through this helper — neither may
// finalize unconditionally and clear afterwards, and neither may mutate the
// task outside the fence.
//
// The mutation is a pure in-memory three-state decision — the helper is the
// SINGLE owner of every task-store write (including the archive move, which
// rides the same write via ArchivedAt):
//
//	fenceDecisionApply  → mutate the task AND clear the token in ONE store
//	                      write; a follow-up ArchiveTask call moves FS-store
//	                      rows best-effort (the DB row is already archived)
//	fenceDecisionSkip   → nothing to do (terminal/non-transitionable); one
//	                      store write releases the token
//	fenceDecisionRetry  → the mutation's own side-state writes (workflow
//	                      engine, …) failed; NOTHING is persisted and the
//	                      token stays, so the pass-wide sweep replay — which
//	                      requires the fence — redrives the transition
//
// Contract (GPT 收口 4d): a failed task-store write persists NOTHING and
// releases NOTHING — the stored task stays byte-identical to what a replaying
// sweep re-reads, so the retry can never double-apply. The token is only ever
// released in the same write that lands the (skipping or applied) state.
func (s *Server) fencedTaskTransition(workspaceID, project, agent, taskID, runID string, mutate func(task *entity.Task) fenceDecision) fencedTaskTransitionOutcome {
	if s == nil || s.ts == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return fencedTransitionTaskMissing
	}
	s.runtimeTaskTokenMu.Lock()
	defer s.runtimeTaskTokenMu.Unlock()
	task, err := s.ts.GetTask(project, agent, taskID)
	if err != nil || task == nil {
		return fencedTransitionTaskMissing
	}
	if task.ActiveRuntimeRunID != runID {
		// Fence: a newer dispatch owns the task — do not touch its state
		// and do not release anything.
		return fencedTransitionNotOurs
	}
	decision := fenceDecisionApply
	if mutate != nil {
		decision = mutate(task)
	}
	switch decision {
	case fenceDecisionRetry:
		// Side-state writes (workflow engine) failed on this attempt. Keep
		// the token so the sweep replay can redrive the transition under the
		// same fence; nothing was persisted.
		slog.Warn("fenced transition: side-state write failed; token kept for replay", "run", runID, "task", task.ID)
		return fencedTransitionPersistFailed
	case fenceDecisionSkip:
		task.ActiveRuntimeRunID = ""
		task.UpdatedAt = time.Now().UTC()
		if err := s.ts.UpdateTask(project, agent, task); err != nil {
			slog.Warn("fenced transition: token release write failed; sweep will reconcile", "run", runID, "task", task.ID, "error", err)
			return fencedTransitionPersistFailed
		}
		return fencedTransitionSkipped
	}
	// Apply: mutation + token release land in ONE write under the fence, so a
	// persist failure leaves nothing behind for a replay to double-apply.
	task.ActiveRuntimeRunID = ""
	task.UpdatedAt = time.Now().UTC()
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		slog.Warn("fenced transition: persist failed; token kept for replay", "run", runID, "task", task.ID, "error", err)
		return fencedTransitionPersistFailed
	}
	if task.ArchivedAt != nil {
		// Archive move for stores that keep active and archived rows apart.
		// The DB write above already persisted the archived state; a failure
		// here cannot unfence anything (the token is released) and is
		// invisible to active listings, so it is best-effort.
		if err := s.ts.ArchiveTask(project, agent, task); err != nil {
			slog.Warn("fenced transition: archive move failed; state already persisted", "run", runID, "task", task.ID, "error", err)
		}
	}
	return fencedTransitionApplied
}

// transitionReapedTask drives the task forward after its run was force-failed
// with lease_expired (GPT fix 2 + 收口 2): it goes through the SAME unified
// fencedTaskTransition critical section as the finish path — no bare
// applyInfraFailureBackoff outside the fence. Non-workflow tasks land in the
// unified infra backoff/blocked logic; workflow tasks fail their active step
// explicitly through the workflow engine's own failure/rework mechanism
// (CompleteAndAdvance "failed"), so the step and run never stay in_progress.
func (s *Server) transitionReapedTask(workspaceID string, run controldb.RuntimeRun) {
	if s == nil || s.ts == nil || strings.TrimSpace(run.TaskID) == "" {
		return
	}
	outcome := s.fencedTaskTransition(workspaceID, run.ProjectID, run.AgentID, run.TaskID, run.ID, func(task *entity.Task) fenceDecision {
		if task.Status.IsTerminal() {
			return fenceDecisionSkip
		}
		if task.Status != entity.TaskStatusInProgress && task.Status != entity.TaskStatusPending {
			return fenceDecisionSkip
		}
		if s.runtimeTaskHasWorkflow(workspaceID, run.ProjectID, run.TaskID) {
			// Workflow-side failure is driven by transitionReapedWorkflowTask
			// (its own store); on success the task mutation below lands with
			// the fence release in the helper's single write. A workflow write
			// failure returns Retry so NOTHING is persisted and the token
			// stays for the next pass's replay.
			if !s.transitionReapedWorkflowTask(workspaceID, run, task) {
				return fenceDecisionRetry
			}
			return fenceDecisionApply
		}
		task.LastError = "lease expired; run reaped by control plane (lease_expired)"
		applyInfraFailureBackoffMutation(task, "lease_expired")
		return fenceDecisionApply
	})
	switch outcome {
	case fencedTransitionApplied:
		slog.Warn("reaped run's task transitioned through the fence", "run", run.ID, "task", run.TaskID)
	case fencedTransitionNotOurs:
		// A newer dispatch owns the task — nothing to do.
	case fencedTransitionPersistFailed:
		// Token kept; the reaper sweep or the next pass converges.
	}
}

// transitionReapedWorkflowTask drives a reaped WORKFLOW task into the
// workflow engine's failure/rework mechanism (GPT 收口 3 + 6-3): the active
// step is failed explicitly with CompleteAndAdvance("failed") so the step
// instance, the run, and the task all reach a defined terminal/rework state
// instead of lingering in_progress. Idempotent across replays: if the
// workflow run is ALREADY terminal (a previous pass persisted the failure but
// the task write failed), the engine is NOT re-driven — the task state is
// simply applied so the fenced helper converges. Called under the token
// fence; it mutates the passed task in memory and returns false only when the
// workflow-side writes failed this pass — the helper then keeps the token for
// retry. Persisting the task is the fenced helper's job (single persist
// owner).
func (s *Server) transitionReapedWorkflowTask(workspaceID string, run controldb.RuntimeRun, task *entity.Task) bool {
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	stepStatus := "failed"
	stepError := "lease expired; run reaped by control plane (lease_expired)"
	if task.Vars[workflowBranchIDVar] != "" {
		branchTransitioned, workflowAlreadyTerminal := s.failReapedWorkflowBranch(workspaceID, run, task, stepStatus, stepError)
		if !branchTransitioned {
			return false
		}
		if !workflowAlreadyTerminal {
			s.auditLog(auditLogInput{
				WorkspaceID:  workspaceID,
				Action:       "runtime_run.reaped",
				ResourceType: "task",
				ResourceID:   task.ID,
				Summary:      "Workflow branch task run reaped; branch failed through the workflow rework path",
				After:        map[string]any{"runId": run.ID, "taskId": task.ID, "stepStatus": stepStatus},
			})
		}
		s.applyReapedWorkflowTaskState(task, stepError)
		return true
	}
	wfRun, found, err := wfStore.RunForTask(run.ProjectID, task.ID)
	if err != nil {
		slog.Warn("reaped workflow task: reading workflow run failed", "run", run.ID, "task", task.ID, "error", err)
		return false
	}
	if !found {
		// Run record already cleaned up — nothing workflow-side to fail; the
		// caller converges the task to its non-workflow backoff state.
		return false
	}
	if workflowRunStatusIsTerminal(wfRun.Status) {
		// Idempotent replay (GPT 收口 6-3): the engine already recorded the
		// failure on a previous pass — re-driving CompleteAndAdvance would be
		// a silent no-op at best. Just converge the task state.
		s.applyReapedWorkflowTaskState(task, stepError)
		return true
	}
	output := strings.TrimSpace(task.Summary)
	if output == "" {
		output = stepError
	}
	if _, err := wfStore.CompleteAndAdvance(run.ProjectID, task.ID, task.Summary, output, nil, stepStatus); err != nil {
		slog.Warn("reaped workflow task: failing step failed", "run", run.ID, "task", task.ID, "error", err)
		return false
	}
	s.applyReapedWorkflowTaskState(task, stepError)
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "runtime_run.reaped",
		ResourceType: "task",
		ResourceID:   task.ID,
		Summary:      "Workflow task run reaped; active step failed through the workflow rework path",
		After:        map[string]any{"runId": run.ID, "taskId": task.ID, "stepStatus": stepStatus},
	})
	return true
}

// applyReapedWorkflowTaskState mutates the task in memory into the terminal
// state of a reaped workflow execution. The fenced helper persists it.
func (s *Server) applyReapedWorkflowTaskState(task *entity.Task, stepError string) {
	prev := task.Status
	now := time.Now().UTC()
	task.Status = entity.TaskStatusDoneFailed
	task.LastError = stepError
	task.ArchivedAt = &now
	task.UpdatedAt = now
	entity.ApplyStatusTimestamps(task, prev, now)
}

// workflowRunStatusIsTerminal reports whether a workflow run status is final.
func workflowRunStatusIsTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "completed", "cancelled", "canceled":
		return true
	}
	return false
}

// failReapedWorkflowBranch fails a reaped branch task's branch instance.
// Idempotent across replays: if the parent workflow run is already terminal
// (the failure landed on a previous pass), the branch store is not re-driven
// — returns (true, true). Returns (false, false) when the branch write failed
// this pass so the caller keeps the token for retry.
func (s *Server) failReapedWorkflowBranch(workspaceID string, run controldb.RuntimeRun, task *entity.Task, stepStatus, stepError string) (branchFailed, workflowAlreadyTerminal bool) {
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if wfRun, found, err := wfStore.RunForTask(run.ProjectID, task.ID); err == nil && found && workflowRunStatusIsTerminal(wfRun.Status) {
		// The branch failure already landed on a previous pass — replay only
		// converges the task state.
		return true, true
	}
	if _, err := s.completeRuntimeWorkflowBranch(workspaceID, run.ProjectID, task, nil, stepStatus); err != nil {
		slog.Warn("reaped workflow branch task: failing branch failed", "run", run.ID, "task", task.ID, "error", err)
		return false, false
	}
	return true, false
}

// ── Fork session slot class (D1: decided at enqueue, fail-closed) ────────────

// runtimeReadonlyForkCapabilities is the platform-fixed read-only capability
// category (GPT ruling: the allowlist is fixed by the platform, never derived
// from free-form prompts). A fork session is slot-exempt ONLY when its
// declared capability set names nothing outside this list.
var runtimeReadonlyForkCapabilities = map[string]struct{}{
	"inspect": {},
	"log":     {},
	"list":    {},
	"read":    {},
	"status":  {},
}

// forkSessionSlotClass inspects a fork session's persisted capability set and
// returns "readonly" when every declared capability is within the platform
// read-only category, "normal" otherwise. Fail-closed: unparseable JSON,
// non-string entries, an empty set, or any unknown/unrecognised mode map to
// "normal" (occupies the slot). "mode":"inherit" is a policy knob, not a
// capability, and never grants the exemption.
func forkSessionSlotClass(session controldb.AgentSession) string {
	raw := strings.TrimSpace(session.CapabilitiesJSON)
	if raw == "" {
		return controldb.SlotClassNormal
	}
	var caps map[string]any
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return controldb.SlotClassNormal
	}
	declared := 0
	for key, value := range caps {
		if strings.EqualFold(strings.TrimSpace(key), "mode") {
			continue
		}
		name := ""
		switch v := value.(type) {
		case string:
			name = strings.TrimSpace(v)
		case bool:
			if !v {
				continue
			}
			name = strings.TrimSpace(key)
		default:
			return controldb.SlotClassNormal
		}
		if name == "" {
			return controldb.SlotClassNormal
		}
		if _, ok := runtimeReadonlyForkCapabilities[strings.ToLower(name)]; !ok {
			return controldb.SlotClassNormal
		}
		declared++
	}
	if declared == 0 {
		return controldb.SlotClassNormal
	}
	return controldb.SlotClassReadonly
}

// ── Workspace reaper ─────────────────────────────────────────────────────────

// startRuntimeReaper launches the workspace-level reaper singleton. Idempotent
// per server: repeated calls (scheduler restarts, repeated Start) never spawn
// a second loop. The loop stops when ctx is cancelled; waitForRuntimeReaper
// must be awaited before the control DB closes on shutdown.
func (s *Server) startRuntimeReaper(ctx context.Context) {
	if s == nil || s.controlDB == nil {
		return
	}
	s.runtimeReaperOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		s.runtimeReaperCancel = cancel
		done := make(chan struct{})
		s.runtimeReaperDone = done
		go s.runtimeReaperLoop(loopCtx, done)
	})
}

// stopRuntimeReaper cancels the loop and waits for exit. Used on shutdown and
// in tests.
func (s *Server) stopRuntimeReaper() {
	if s == nil || s.runtimeReaperCancel == nil {
		return
	}
	s.runtimeReaperCancel()
	s.waitForRuntimeReaper()
}

// waitForRuntimeReaper blocks until the reaper loop has exited. Called during
// server shutdown BEFORE the control DB is closed (plan 2.1-3: reaper must
// stop first so it never queries a closed DB).
func (s *Server) waitForRuntimeReaper() {
	if s == nil || s.runtimeReaperDone == nil {
		return
	}
	<-s.runtimeReaperDone
}

func (s *Server) runtimeReaperLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(runtimeReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runtimeReaperPass()
		}
	}
}

// runtimeReaperPass force-fails every running run whose lease has been stale
// past the grace window. Sole criterion: status='running' AND
// lease_expires_at < now-grace (plan 2.2-1: no node-health join — lease
// renewal and heartbeat share one loop, so the lease IS the heartbeat). Every
// kill is a conditional UPDATE re-checking lease + generation, so a run that
// gets renewed, finished, or taken over mid-pass is left untouched.
func (s *Server) runtimeReaperPass() {
	if s == nil || s.controlDB == nil {
		return
	}
	now := time.Now().UTC()
	cutoff := controldb.LeaseExpiredCutoff(runtimeReaperLeaseGrace, now)
	workspaces, err := s.controlDB.ListWorkspaces()
	if err != nil {
		slog.Warn("runtime reaper pass skipped: listing workspaces failed", "error", err)
		return
	}
	for _, ws := range workspaces {
		if strings.TrimSpace(ws.ID) == "" {
			continue
		}
		s.runtimeReaperPassWorkspace(ws.ID, cutoff)
	}
}

func (s *Server) runtimeReaperPassWorkspace(workspaceID string, cutoff time.Time) {
	expired, err := s.controlDB.ListExpiredRunningRuns(workspaceID, cutoff, 100)
	if err != nil {
		slog.Warn("runtime reaper pass skipped: listing expired runs failed", "workspace", workspaceID, "error", err)
		return
	}
	for _, run := range expired {
		reaped, err := s.controlDB.ReapExpiredRuntimeRun(workspaceID, run.ID, run.LeaseGeneration, cutoff)
		if err != nil {
			slog.Warn("runtime reaper kill failed", "run", run.ID, "error", err)
			continue
		}
		if !reaped {
			// Lost the race against renew/finish — nothing to do.
			continue
		}
		slog.Warn("runtime run reaped: lease expired past grace", "run", run.ID, "node", run.RuntimeNodeID, "task", run.TaskID, "leaseExpiredAt", run.LeaseExpiresAt)
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			Action:       "runtime_run.reaped",
			ResourceType: "runtime_run",
			ResourceID:   run.ID,
			Summary:      "Runtime run force-failed: lease expired past reaper grace",
			After: map[string]any{
				"runtimeNodeId":   run.RuntimeNodeID,
				"taskId":          run.TaskID,
				"leaseExpiredAt":  run.LeaseExpiresAt,
				"graceSeconds":    int(runtimeReaperLeaseGrace.Seconds()),
				"errorCode":       "lease_expired",
				"leaseGeneration": run.LeaseGeneration + 1,
			},
		})
		// Fenced task transition (fix 2): move the task through the unified
		// infra backoff/blocked path while the token still names this run,
		// then release the token. Order matters — the transition check uses
		// the token, the clear releases it, and anything left over (write
		// failure mid-transition) is reconciled by the sweep below or the
		// next pass.
		s.transitionReapedTask(workspaceID, run)
		// Conditional token clear: only if the task still names THIS run.
		// clearStaleTaskRuntimeToken replays the reaped transition first, so
		// a fenced persist failure above is retried before the fence drops.
		if strings.TrimSpace(run.TaskID) != "" && s.ts != nil {
			if !s.clearStaleTaskRuntimeTokenByID(workspaceID, run) {
				// Field already changed or write failed — the pass-wide
				// clearStaleTaskRuntimeToken sweep reconciles any residue.
				s.reconcileStaleTaskToken(workspaceID, run)
			}
		}
	}
	s.sweepStaleTaskRuntimeTokens(workspaceID)
}

// sweepStaleTaskRuntimeTokens is the pass-wide orphan reconciliation (Claude
// fix-round finding 1): every cycle, ALL projects' tasks are scanned for
// execution tokens pointing at runs that are already terminal or missing —
// not just the tasks of runs reaped in this pass. This closes the window
// where a finish-side token clear failed and the orphan would otherwise
// persist forever.
func (s *Server) sweepStaleTaskRuntimeTokens(workspaceID string) {
	if s == nil || s.ts == nil || s.st == nil {
		return
	}
	projectRows, err := s.st.ListProjects()
	if err != nil {
		slog.Warn("stale-token sweep skipped: listing projects failed", "workspace", workspaceID, "error", err)
		return
	}
	for _, project := range projectRows {
		if project == nil || strings.TrimSpace(project.Name) == "" {
			continue
		}
		memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
			WorkspaceID: workspaceID,
			ProjectID:   project.Name,
			MemberType:  "agent_worker",
		})
		if err != nil {
			continue
		}
		for _, membership := range memberships {
			agent := strings.TrimSpace(membership.Title)
			if agent == "" {
				agent = strings.TrimSpace(membership.MemberID)
			}
			if agent == "" {
				continue
			}
			tasks, err := s.ts.ListTasks(project.Name, agent, entity.TaskStatusInProgress, entity.TaskStatusPending, entity.TaskStatusBlocked)
			if err != nil {
				continue
			}
			for _, task := range tasks {
				if task == nil || strings.TrimSpace(task.ActiveRuntimeRunID) == "" {
					continue
				}
				s.clearStaleTaskRuntimeToken(workspaceID, project.Name, agent, task)
			}
		}
	}
}

// clearStaleTaskRuntimeTokenByID is the run-referenced variant of
// clearStaleTaskRuntimeToken: it loads the task the reaped run was executing
// and runs the replay+clear reconciliation against it.
func (s *Server) clearStaleTaskRuntimeTokenByID(workspaceID string, run controldb.RuntimeRun) bool {
	if s == nil || s.ts == nil || strings.TrimSpace(run.TaskID) == "" {
		return false
	}
	task, err := s.ts.GetTask(run.ProjectID, run.AgentID, run.TaskID)
	if err != nil || task == nil {
		return false
	}
	return s.clearStaleTaskRuntimeToken(workspaceID, run.ProjectID, run.AgentID, task)
}

// reconcileStaleTaskToken sweeps the task referenced by a reaped run if the
// direct conditional clear did not apply (field mismatch or write failure),
// auditing an orphan when a stale token is actually removed.
func (s *Server) reconcileStaleTaskToken(workspaceID string, run controldb.RuntimeRun) {
	if s == nil || s.ts == nil || strings.TrimSpace(run.TaskID) == "" {
		return
	}
	task, err := s.ts.GetTask(run.ProjectID, run.AgentID, run.TaskID)
	if err != nil || task == nil {
		return
	}
	s.clearStaleTaskRuntimeToken(workspaceID, run.ProjectID, run.AgentID, task)
}
