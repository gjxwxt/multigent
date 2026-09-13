package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
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
// if this task write then fails, the run stays authoritative (run_key dedup
// still prevents duplicate dispatch) and the field is compensated later —
// either by the next enqueue/claim writing it, or by clearStaleTaskRuntimeToken
// converging on the reaper cycle. Stamp and clear share one mutex so an old
// run's clear can never interleave between a new dispatch's read and write
// (GPT fix 3: the token is a real fence, not advisory).
func (s *Server) setTaskActiveRuntimeRun(project, agent, taskID, runID string) {
	if s == nil || s.ts == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	s.runtimeTaskTokenMu.Lock()
	defer s.runtimeTaskTokenMu.Unlock()
	task, err := s.ts.GetTask(project, agent, taskID)
	if err != nil || task == nil {
		return
	}
	if task.ActiveRuntimeRunID == runID {
		return
	}
	task.ActiveRuntimeRunID = runID
	task.UpdatedAt = time.Now().UTC()
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		slog.Warn("runtime task token write failed; run remains authoritative", "run", runID, "task", taskID, "error", err)
	}
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
// a run that is already terminal or no longer exists is stale — clear it and
// audit so the task can be dispatched again. run-side data is authoritative.
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
	if !s.clearTaskActiveRuntimeRunIfRun(project, agent, task.ID, runID) {
		return false
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "task.runtime_token_orphan",
		ResourceType: "task",
		ResourceID:   task.ID,
		Summary:      "Cleared stale runtime execution token (run already terminal or missing)",
		After: map[string]any{
			"runId":       runID,
			"runTerminal": found,
		},
	})
	return true
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

// transitionReapedTask drives the task forward after its run was force-failed
// with lease_expired (GPT fix 2). The transition is fenced on the execution
// token: only the task that still names THIS run is moved. Workflow tasks are
// left to the workflow engine's own rework path (same exclusion as
// finalizeRuntimeTaskRun); non-workflow tasks go through the unified infra
// backoff/blocked logic so the task never stays in_progress.
func (s *Server) transitionReapedTask(workspaceID string, run controldb.RuntimeRun) {
	if s == nil || s.ts == nil || strings.TrimSpace(run.TaskID) == "" {
		return
	}
	task, err := s.ts.GetTask(run.ProjectID, run.AgentID, run.TaskID)
	if err != nil || task == nil {
		return
	}
	// Fence (fix 3): if the token no longer names this run, a newer
	// dispatch already owns the task — do not touch its state.
	if !s.taskTokenOwnedByRun(run.ProjectID, run.AgentID, task, run.ID) {
		return
	}
	if task.Status.IsTerminal() {
		return
	}
	if s.runtimeTaskHasWorkflow(workspaceID, run.ProjectID, run.TaskID) {
		// Workflow runs fail their step through the workflow engine; the
		// reaper only records the kill. No in_progress residue: the workflow
		// step fails with the run.
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			Action:       "runtime_run.reaped",
			ResourceType: "task",
			ResourceID:   task.ID,
			Summary:      "Workflow task run reaped; step failure handled by the workflow engine",
			After:        map[string]any{"runId": run.ID, "taskId": task.ID},
		})
		return
	}
	if task.Status != entity.TaskStatusInProgress && task.Status != entity.TaskStatusPending {
		return
	}
	task.LastError = "lease expired; run reaped by control plane (lease_expired)"
	s.applyInfraFailureBackoff(workspaceID, run.ProjectID, run.AgentID, task, "lease_expired")
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
		if strings.TrimSpace(run.TaskID) != "" && s.ts != nil {
			if !s.clearTaskActiveRuntimeRunIfRun(run.ProjectID, run.AgentID, run.TaskID, run.ID) {
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
