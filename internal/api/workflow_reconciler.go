package api

// Workflow reconciler (P1-4): the periodic safety net behind the event-driven
// wave engine. Branch completions are what advance a plan-driven stage, so a
// lost event (a restart between child completion and parent advance, a claim
// whose claimant died mid-materialization) leaves the run parked forever —
// the startup recovery scan only helps after a restart. This loop re-drives
// BOTH failure shapes on a ticker, using the same two levers the production
// paths use:
//
//   - Tier 1 (automatic): an ACTIVE workflow run whose UpdatedAt has been
//     frozen past the stall threshold, sitting on a non-human step with NO
//     active runtime run, gets its trigger re-fired (deduped by
//     hasActiveRuntimeRun inside fireTaskTriggerOrQueueRuntime anyway).
//     Plan-driven runs additionally re-run materializeNextPlanWave, which
//     picks up held-back (concurrency-gated) or claim-expired packages.
//   - Tier 2 (alert only): a RUNNING runtime run whose updated_at has been
//     frozen past the threshold while the node lease is still being renewed
//     looks like a wedged agent (container alive, no progress). Long builds
//     and wedges are indistinguishable from here and there is no real agent
//     heartbeat, so this ONLY posts an attention note on the task — killing
//     work automatically is out of scope until runtime-side heartbeats land.
//
// human_review steps are NEVER auto-advanced (the startup-recovery invariant).

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

const (
	workflowReconcilerInterval = 60 * time.Second
	workflowStallThresholdEnv  = "MULTIGENT_WORKFLOW_STALL_THRESHOLD"
	defaultWorkflowStall       = 15 * time.Minute
)

// workflowStallThreshold parses MULTIGENT_WORKFLOW_STALL_THRESHOLD (Go duration
// syntax); an unparseable value falls back to the default.
func workflowStallThreshold() time.Duration {
	raw := strings.TrimSpace(os.Getenv(workflowStallThresholdEnv))
	if raw == "" {
		return defaultWorkflowStall
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("[workflow-reconciler] ignoring invalid %s=%q, using %s", workflowStallThresholdEnv, raw, defaultWorkflowStall)
		return defaultWorkflowStall
	}
	return d
}

func (s *Server) startWorkflowReconciler(ctx context.Context) {
	if s == nil || s.controlDB == nil {
		return
	}
	s.workflowReconcilerOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		s.workflowReconcilerCancel = cancel
		done := make(chan struct{})
		s.workflowReconcilerDone = done
		go s.workflowReconcilerLoop(loopCtx, done)
	})
}

func (s *Server) stopWorkflowReconciler() {
	if s == nil || s.workflowReconcilerCancel == nil {
		return
	}
	s.workflowReconcilerCancel()
	if s.workflowReconcilerDone != nil {
		<-s.workflowReconcilerDone
	}
}

func (s *Server) workflowReconcilerLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(workflowReconcilerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.workflowReconcilerPass(ctx, workflowStallThreshold())
		}
	}
}

// workflowReconcilerPass is one sweep. Split from the loop for tests.
func (s *Server) workflowReconcilerPass(ctx context.Context, stall time.Duration) (refired int, alerted int) {
	if s == nil || s.controlDB == nil || s.ts == nil {
		return 0, 0
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || strings.TrimSpace(workspaceID) == "" {
		return 0, 0
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	records, err := s.ts.ListAllTaskRecords("")
	if err != nil {
		return 0, 0
	}
	now := time.Now().UTC()
	for _, rec := range records {
		if ctx.Err() != nil {
			return refired, alerted
		}
		task := rec.Task
		project := strings.TrimSpace(rec.Project)
		agent := strings.TrimSpace(rec.Agent)
		if task == nil || project == "" || agent == "" {
			continue
		}
		if task.Status != entity.TaskStatusInProgress && task.Status != entity.TaskStatusPending {
			continue
		}
		run, found, err := wfStore.RunForTask(project, task.ID)
		if err != nil || !found {
			continue
		}
		// Tier 1: active run frozen past the threshold.
		if run.Status == "active" && strings.TrimSpace(run.ActiveStepID) != "" && !run.UpdatedAt.IsZero() && now.Sub(run.UpdatedAt) > stall {
			did, err := s.reconcileStalledWorkflowRun(workspaceID, project, agent, wfStore, run, task)
			if err != nil {
				log.Printf("[workflow-reconciler] run %s (task %s) reconcile failed: %v", run.ID, task.ID, err)
				continue
			}
			if did {
				refired++
			}
		}
		// Tier 2: running runtime run frozen past the threshold — alert only.
		alerted += s.alertStalledRuntimeRuns(workspaceID, project, task.ID, stall, now)
	}
	if refired > 0 || alerted > 0 {
		log.Printf("[workflow-reconciler] pass complete: refired=%d alerted=%d", refired, alerted)
	}
	return refired, alerted
}

// reconcileStalledWorkflowRun re-drives one stalled active run. Returns true
// when something was actually re-fired.
func (s *Server) reconcileStalledWorkflowRun(workspaceID, project, agent string, wfStore *workflowstore.Store, run entity.WorkflowRun, task *entity.Task) (bool, error) {
	def, defFound, err := wfStore.RunDefinition(run)
	if err != nil || !defFound {
		return false, nil
	}
	var activeStep *entity.WorkflowStep
	for i := range def.Steps {
		if def.Steps[i].ID == run.ActiveStepID {
			activeStep = &def.Steps[i]
			break
		}
	}
	// human_review steps are never auto-advanced; unknown steps fail closed.
	if activeStep == nil || activeStep.Type == "human_review" {
		return false, nil
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return false, err
	}
	var activeInst *entity.WorkflowStepInstance
	for i := range instances {
		if instances[i].StepID == run.ActiveStepID {
			activeInst = &instances[i]
			break
		}
	}
	if activeInst == nil || (activeInst.Status != "pending" && activeInst.Status != "running") {
		return false, nil
	}
	// An active runtime run means work is dispatched (or a wedge — Tier 2
	// territory, alerted separately); do not double-fire on top of it.
	if s.hasActiveRuntimeRun(workspaceID, project, agent, task.ID) {
		return false, nil
	}
	log.Printf("[workflow-reconciler] re-firing stalled run %s (task %s, step %s, idle %s)", run.ID, task.ID, run.ActiveStepID, time.Since(run.UpdatedAt).Round(time.Second))
	s.fireTaskTriggerOrQueueRuntime(workspaceID, project, agent, task, nil, "workflow reconciler re-fire after "+time.Since(run.UpdatedAt).Round(time.Second).String()+" idle")
	// Plan-driven stage: the wave unlock is event-driven, so a lost completion
	// event parks held-back or claim-expired packages; re-run the hook.
	if activeStep.Type == "parallel_stage" {
		if _, _, _, planErr := wfStore.LoadFrozenPlanForRun(project, run.ID); planErr == nil {
			// materializeNextPlanWave resolves the stage through
			// result.Branch.StepID — an empty Branch made the re-drive a
			// silent no-op ("definition has no step \"\"") on the first live
			// fire. The active step IS the parallel stage here.
			if _, waveErr := s.materializeNextPlanWave(workspaceID, project, agent, task, workflowstore.BranchTransitionResult{
				Branch: entity.WorkflowBranchInstance{
					RunID:    run.ID,
					StepID:   run.ActiveStepID,
					BranchID: run.ActiveStepID,
				},
				Transition: workflowstore.TransitionResult{Run: run, Current: *activeInst},
			}, nil); waveErr != nil {
				log.Printf("[workflow-reconciler] plan wave re-drive for run %s: %v", run.ID, waveErr)
			}
		}
	}
	return true, nil
}

// alertStalledRuntimeRuns posts an attention note for running runtime runs
// whose updated_at froze past the threshold. Alert-only: without a real
// agent-side heartbeat a busy build and a wedge look identical.
func (s *Server) alertStalledRuntimeRuns(workspaceID, project, taskID string, stall time.Duration, now time.Time) int {
	if s.controlDB == nil || strings.TrimSpace(taskID) == "" {
		return 0
	}
	runs, err := s.controlDB.ListRuntimeRuns(db.RuntimeRunFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
		TaskID:      taskID,
		Status:      "running",
	})
	if err != nil {
		return 0
	}
	alerted := 0
	for _, rr := range runs {
		updated, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(rr.UpdatedAt))
		if err != nil || updated.IsZero() {
			continue
		}
		if now.Sub(updated) <= stall {
			continue
		}
		alerted++
		agent := strings.TrimSpace(rr.AgentID)
		if agent == "" {
			continue
		}
		t, _, err := s.findTaskInProject(project, taskID)
		if err != nil || t == nil {
			continue
		}
		idle := now.Sub(updated).Round(time.Second)
		note := "[workflow-reconciler] runtime run " + rr.ID + " idle for " + idle.String() + " while still marked running — suspected agent hang (no auto-kill; investigate or cancel manually)"
		if !strings.Contains(t.Summary, note) {
			t.Summary = strings.TrimSpace(strings.TrimSpace(t.Summary) + "\n" + note)
			t.UpdatedAt = time.Now().UTC()
			_ = s.ts.PersistTask(project, agent, t)
			log.Printf("[workflow-reconciler] alert: task %s runtime run %s idle %s", taskID, rr.ID, idle)
		}
	}
	return alerted
}
