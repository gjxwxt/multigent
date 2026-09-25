package api

// Branch-join re-drive (S2 round 7, defects D-F/D-G found by the real
// fan-out run on 2026-09-24):
//
//	 D-F — the hourly worktree reaper reclaimed the delivery worktrees of
//	       terminal branch children WHILE their join was still parked. Both
//	       real branches (t-20260924-hps07k, t-20260924-o3obrx) had completed,
//	       pushed and been rejected by the (then credential-less) push-evidence
//	       gate; the platform's documented "a rejected branch keeps its
//	       worktree for retry" guarantee was silently defeated by the sweep,
//	       because `Task.Status.IsTerminal()` cannot distinguish "finished and
//	       done" from "finished but still awaiting its join".
//	 D-G — with the tree gone there was NO entry point left that could
//	       re-evaluate the pending join: the fan-out re-drive skips branches
//	       that already have instances, and the branch child's own report path
//	       needs the (terminal) agent of a (terminal) child run. The stage was
//	       permanently stuck with two delivered branches.
//
// The guard and the re-drive share one predicate — "is this branch child
// still parked on an unfinished join?" — so the reaper can never retire a
// tree the re-drive needs, and the re-drive can never resurrect a branch the
// workflow already resolved.

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// pendingBranchJoinForTask reports whether task is a fan-out branch child
// whose parent run is STILL PARKED on the branch's parallel stage, i.e. the
// join has not resolved yet. The returned instance carries the child run ID
// and the recorded branch status.
//
// The predicate is deliberately anchored on the PARENT RUN, not on the task
// status: `done_success` (the archived child) plus `LastError` (the rejected
// join) is exactly the retryable shape the platform documents, and it looks
// identical to "finished, nothing left to do" from the task alone. The
// returned instance carries its own status so callers can tell "still to be
// joined" from "already resolved as failed/skipped".
func (s *Server) pendingBranchJoinForTask(workspaceID, project string, task *entity.Task) (entity.WorkflowBranchInstance, bool, error) {
	var zero entity.WorkflowBranchInstance
	if s == nil || s.controlDB == nil || task == nil {
		return zero, false, nil
	}
	rootTaskID := strings.TrimSpace(task.Vars[workflowRootTaskIDVar])
	runID := strings.TrimSpace(task.Vars[workflowRunIDVar])
	stepID := strings.TrimSpace(task.Vars[workflowStepIDVar])
	branchID := strings.TrimSpace(task.Vars[workflowBranchIDVar])
	if rootTaskID == "" || runID == "" || stepID == "" || branchID == "" {
		return zero, false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, rootTaskID)
	if err != nil {
		return zero, false, err
	}
	if !found || strings.TrimSpace(run.ID) != runID {
		return zero, false, nil
	}
	// The stage is resolved the moment the run moves off it (or dies): after
	// that the branch delivery is part of the run's history and its worktree
	// is ordinary garbage again. The status guard also disposes of historical
	// residue: a terminal run can still carry step instances (the 2026-09-23
	// wfr-07249bes leftovers are exactly that shape) and must never keep a
	// worktree alive.
	if strings.TrimSpace(run.Status) != "active" {
		return zero, false, nil
	}
	if strings.TrimSpace(run.ActiveStepID) != stepID {
		return zero, false, nil
	}
	instances, err := wfStore.BranchInstancesForStep(runID, stepID)
	if err != nil {
		return zero, false, err
	}
	for _, inst := range instances {
		if strings.TrimSpace(inst.BranchID) != branchID {
			continue
		}
		if strings.TrimSpace(inst.ChildTaskID) != strings.TrimSpace(task.ID) {
			return zero, false, nil
		}
		return inst, true, nil
	}
	return zero, false, nil
}

// restoreBranchDeliveryWorktree re-materializes the evidence tree of a branch
// child whose worktree was retired after its delivery was recorded, and
// restores the worktree-side recovery copy of the ORIGINAL capture-time
// baseline.
//
// It deliberately never captures a NEW baseline: the tree now holds the
// DELIVERED content, so a fresh fingerprint would move the measurement origin
// onto the delivery itself and launder the real-change gate. The control-plane
// record persists from the fan-out materialization and stays authoritative;
// the fresh capture that materialization writes is overwritten by the record
// verbatim (digest-checked by the gate) or the restore fails closed.
func (s *Server) restoreBranchDeliveryWorktree(workspaceID, project, agent string, task *entity.Task) (string, bool, error) {
	if s == nil || s.worktreeMgr == nil || task == nil {
		return "", false, nil
	}
	gitRoot := s.resolveProjectGitRoot(project)
	if _, err := os.Stat(filepath.Join(gitRoot, ".git")); err != nil {
		return "", false, nil
	}
	// NEVER ask the general resolver here: its fallback chain happily returns
	// the project git root or an agent directory (both carry .git), and
	// treating that as "the worktree is already there" skips the restore and
	// silently measures the base checkout instead of the delivery. The task's
	// recorded worktree and the canonical platform path are the only two
	// surfaces that belong to THIS task.
	canonical := gitworktree.WorktreeDir(gitRoot, task.ID)
	for _, candidate := range []string{strings.TrimSpace(task.WorktreeDir), canonical} {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err == nil {
			return candidate, false, nil
		}
	}
	// A residual directory that is no longer a usable worktree (partial
	// retirement, interrupted cleanup) goes through the platform's own retire
	// sequence — which checkpoints uncommitted work onto the task branch
	// first — before it can be materialized again.
	for _, residual := range []string{strings.TrimSpace(task.WorktreeDir), canonical} {
		if residual == "" {
			continue
		}
		if _, err := os.Stat(residual); err != nil {
			continue
		}
		log.Printf("[branch-join-redrive] task %s: residual worktree %s is not a git worktree; retiring it through the platform cleanup path", task.ID, residual)
		if err := s.worktreeMgr.CleanupWorktree(gitRoot, task.ID); err != nil {
			return "", false, fmt.Errorf("retire residual worktree for task %s: %w", task.ID, err)
		}
		break
	}
	if strings.TrimSpace(task.BranchName) == "" || strings.TrimSpace(task.BaseCommit) == "" {
		return "", false, fmt.Errorf("branch task %s has no recorded branch/base commit; the delivered branch cannot be re-materialized", task.ID)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	payload, found, err := wfStore.LoadQABaselinePayload(project, task.ID)
	if err != nil {
		return "", false, fmt.Errorf("load control-plane qa baseline for %s: %w", task.ID, err)
	}
	if !found {
		return "", false, fmt.Errorf("branch task %s has no control-plane qa baseline; refusing to re-measure a restored worktree (restore the baseline record or retire the branch explicitly)", task.ID)
	}
	wtDir, branchName, materializeErr, _ := s.worktreeMgr.EnsureWorktreeAt(gitRoot, task.ID, task.BaseCommit, task.BranchName)
	if materializeErr != nil {
		return "", false, fmt.Errorf("re-materialize delivered branch %s: %w", task.BranchName, materializeErr)
	}
	if err := gitworktree.RestoreQABaselineMirror(wtDir, payload); err != nil {
		return "", false, fmt.Errorf("restore qa baseline mirror for %s: %w", task.ID, err)
	}
	if strings.TrimSpace(branchName) != "" {
		task.BranchName = branchName
	}
	task.WorktreeDir = wtDir
	if agent != "" {
		if err := s.ts.PersistTask(project, agent, task); err != nil {
			return "", false, fmt.Errorf("persist restored worktree for %s: %w", task.ID, err)
		}
	}
	log.Printf("[branch-join-redrive] task %s: restored delivery worktree %s at branch %s (baseline mirror re-adopted from the control plane)", task.ID, wtDir, task.BranchName)
	return wtDir, true, nil
}

// resumePendingBranchJoinForTask re-drives the join for a parked branch child
// via the manual start path — the platform's existing operator lever — in the
// same shape as the round-5 parked-stage resume. The agent's recorded
// declaration is replayed verbatim and the SAME join gate re-evaluates it
// against live git evidence, so a re-drive can never assert a completion the
// gate would not have accepted.
//
// Returns handled=false for tasks that are not parked branch children.
func (s *Server) resumePendingBranchJoinForTask(workspaceID, project, agent string, task *entity.Task, r *http.Request) (bool, string, error) {
	inst, pending, err := s.pendingBranchJoinForTask(workspaceID, project, task)
	if err != nil {
		return false, "", err
	}
	if !pending {
		return false, "", nil
	}
	// The stage can only resolve if EVERY branch lands, so a parked stage
	// always has a delivery to re-measure — even when the branch's own report
	// was recorded as a failure (a failed branch is terminal for the stage).
	if status := strings.TrimSpace(inst.Status); status == "failed" || status == "skipped" {
		return true, "branch_join_resolved", fmt.Errorf("branch %s is already %s for run %s; the parked stage is waiting on its other branches", inst.BranchID, status, inst.RunID)
	}
	if _, _, err := s.restoreBranchDeliveryWorktree(workspaceID, project, agent, task); err != nil {
		return true, "branch_join_restore_failed", err
	}
	outputs, recorded := s.recordedBranchDeclaration(workspaceID, inst)
	if !recorded {
		// No recorded declaration means the platform cannot reproduce what the
		// agent claimed to have delivered. Falling through with an empty
		// declaration would let the delta gate decide over a missing record —
		// the agent's own step output is the evidence of intent, and a branch
		// worth re-driving always has one.
		return true, "branch_join_declaration_missing", fmt.Errorf("branch child %s has no recorded step declaration (child run %s, step outputs were never persisted); refusing to re-drive a join without the agent's own declaration", task.ID, inst.ChildRunID)
	}
	result, joinErr := s.completeRuntimeWorkflowBranch(workspaceID, project, task, outputs, "completed")
	if joinErr != nil {
		// Mirror the agent-facing rejection semantics: the task record keeps
		// the visible reason, the branch instance stays parked, nothing is
		// fabricated. The operator sees the same gate message the agent did.
		task.LastError = joinErr.Error()
		task.UpdatedAt = time.Now().UTC()
		if agent != "" {
			if persistErr := s.ts.PersistTask(project, agent, task); persistErr != nil {
				log.Printf("[branch-join-redrive] task %s: record rejection failed: %v", task.ID, persistErr)
			}
		}
		log.Printf("[branch-join-redrive] task %s (branch %s, run %s): join gate rejected the re-drive: %v", task.ID, inst.BranchID, inst.RunID, joinErr)
		return true, "branch_join_rejected", joinErr
	}
	if err := s.advanceParentAfterBranchCompletion(workspaceID, project, result, r); err != nil {
		return true, "", err
	}
	// The rejection reason recorded on the task was the retry signal; now that
	// the gate accepted the SAME delivery, leaving it behind would misreport a
	// resolved branch as broken.
	if strings.TrimSpace(task.LastError) != "" {
		task.LastError = ""
		task.UpdatedAt = time.Now().UTC()
		if agent != "" {
			if persistErr := s.ts.PersistTask(project, agent, task); persistErr != nil {
				log.Printf("[branch-join-redrive] task %s: clear stale rejection failed: %v", task.ID, persistErr)
			}
		}
	}
	log.Printf("[branch-join-redrive] task %s: join re-driven for run %s branch %s (branch status %s, stage %s)",
		task.ID, inst.RunID, inst.BranchID, result.Branch.Status, stageResolution(result))
	return true, "branch_join_resumed", nil
}

// recordedBranchDeclaration replays the AGENT's own recorded step outputs for
// a branch child (branch_summary/touched_paths and friends) — never a value
// the operator typed. ok=false means no completed step output was recorded:
// the caller must refuse rather than let the gate judge an empty declaration.
func (s *Server) recordedBranchDeclaration(workspaceID string, inst entity.WorkflowBranchInstance) (map[string]string, bool) {
	childRunID := strings.TrimSpace(inst.ChildRunID)
	if childRunID == "" || s == nil || s.controlDB == nil {
		return nil, false
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.ListStepInstances(childRunID)
	if err != nil {
		log.Printf("[branch-join-redrive] child run %s: read step instances: %v", childRunID, err)
		return nil, false
	}
	var latest *entity.WorkflowStepInstance
	for i := range instances {
		if strings.TrimSpace(instances[i].Status) != "completed" {
			continue
		}
		if len(instances[i].OutputValues) == 0 {
			continue
		}
		if latest == nil || instances[i].UpdatedAt.After(latest.UpdatedAt) {
			latest = &instances[i]
		}
	}
	if latest == nil {
		return nil, false
	}
	outputs := make(map[string]string, len(latest.OutputValues))
	for k, v := range latest.OutputValues {
		outputs[k] = v
	}
	return outputs, true
}

func stageResolution(result workflowstore.BranchTransitionResult) string {
	if result.AllDone {
		return "all branches done"
	}
	return "awaiting other branches"
}

// failedRunStepForTask returns the step a FAILED workflow run died on — the
// step instance the failure path marked failed — for the task's current run.
// ok=false means the run is not in the failed shape this lever answers.
func (s *Server) failedRunStepForTask(workspaceID, project string, task *entity.Task) (entity.WorkflowRun, entity.WorkflowStep, entity.WorkflowStepInstance, bool, error) {
	var zeroRun entity.WorkflowRun
	var zeroStep entity.WorkflowStep
	var zeroInst entity.WorkflowStepInstance
	if s == nil || s.controlDB == nil || task == nil {
		return zeroRun, zeroStep, zeroInst, false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, task.ID)
	if err != nil {
		return zeroRun, zeroStep, zeroInst, false, err
	}
	if !found || strings.TrimSpace(run.Status) != "failed" {
		return zeroRun, zeroStep, zeroInst, false, nil
	}
	def, ok, err := wfStore.RunDefinition(run)
	if err != nil {
		return zeroRun, zeroStep, zeroInst, false, err
	}
	if !ok {
		return zeroRun, zeroStep, zeroInst, false, nil
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return zeroRun, zeroStep, zeroInst, false, err
	}
	// Newest failed instance wins (review round 9, P1-1): a retried-then-failed
	// run can carry several failed instances and store order is not a timeline.
	best := -1
	for i := range instances {
		if strings.TrimSpace(instances[i].Status) != "failed" {
			continue
		}
		if best < 0 || instances[i].FinishedAt.After(instances[best].FinishedAt) {
			best = i
		}
	}
	if best < 0 {
		return zeroRun, zeroStep, zeroInst, false, nil
	}
	for _, candidate := range def.Steps {
		if strings.TrimSpace(candidate.ID) != strings.TrimSpace(instances[best].StepID) {
			continue
		}
		return run, candidate, instances[best], true, nil
	}
	return zeroRun, zeroStep, zeroInst, false, nil
}

// reactivateFailedRunForManualStart is the run-level half of the operator
// retry lever (S2 round 9, D-J): a console restart mid-run marks the in-flight
// agent task and its workflow run failed, and the run then refuses re-entry
// with no platform recovery. Manual start resets ONLY that failed step and
// puts the run back on it, so the normal dispatch below can pick the work up.
//
// Returns handled=true when the run was reactivated (the caller must then
// continue into the ordinary agent dispatch, NOT return early: unlike the
// round-5/7 resumes, this lever's whole point is to run the agent).
func (s *Server) reactivateFailedRunForManualStart(workspaceID, project, agent string, task *entity.Task) (bool, string, error) {
	run, step, inst, ok, err := s.failedRunStepForTask(workspaceID, project, task)
	if err != nil || !ok {
		return false, "", err
	}
	if stepType := strings.TrimSpace(step.Type); stepType != "agent_task" {
		return false, "", fmt.Errorf("workflow run %s failed on a %s step; reactivating it is a workflow (human) action, not a task start", run.ID, stepType)
	}
	// The retry runs the step again, so it must be dispatched to the agent the
	// run binds to that step — starting a DIFFERENT agent on it would burn a
	// run on work it does not own.
	if binding, found := workflowActorBindingForStep(run.ActorBindings, step); found && strings.TrimSpace(binding.Type) == "agent" {
		// A bound step may only be re-run by ITS agent (review round 9, P2-1):
		// an empty/unknown requester is a refusal too, not a bypass.
		if bound := strings.TrimSpace(binding.ID); bound != "" && !strings.EqualFold(bound, agent) {
			return false, "", fmt.Errorf("workflow run %s is bound to agent %s for step %s; start it with that agent", run.ID, bound, step.ID)
		}
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if _, err := wfStore.ReactivateFailedRunForStep(project, task.ID, run.ID, step.ID,
		fmt.Sprintf("operator manual start re-activated the run after a failed %s step (failed instance %s)", step.ID, inst.ID)); err != nil {
		return false, "", err
	}
	log.Printf("[workflow-run-reactivate] task %s: re-activated failed run %s on step %s (previous failure: %s)", task.ID, run.ID, step.ID, strings.TrimSpace(inst.Summary))
	return true, "workflow_run_reactivated", nil
}
