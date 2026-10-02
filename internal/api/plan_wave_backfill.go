package api

// Plan-wave backfill tick (spec P1 顺手项): the wave unlock is event-driven
// (a branch completion re-runs materializeNextPlanWave), so a lost completion
// event leaves held-back (concurrency-gated) or claim-expired packages
// unmatrialized until either the next branch completes or Tier 1 fires after
// the full stall threshold. This 30-second sweep is the middle layer: it
// re-runs the materialization hook for ACTIVE plan-driven runs regardless of
// staleness. It deliberately does NOT touch Tier 1's stall judgment — the
// two answer different questions (backfill: "is there ready work?";
// Tier 1: "is the run wedged?").
//
// UpdatedAt discipline (spec-mandated regression): a materialization that
// dispatches work does not bump the WORKFLOW RUN's UpdatedAt — child tasks
// and runtime runs are separate records — so this tick cannot mask a real
// stall from Tier 1. TestPlanWaveBackfillDoesNotBumpRunUpdatedAt pins that.

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

const planWaveBackfillInterval = 30 * time.Second

func (s *Server) startPlanWaveBackfillLoop(ctx context.Context) {
	if s == nil || s.controlDB == nil {
		return
	}
	s.planWaveBackfillOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		s.planWaveBackfillCancel = cancel
		done := make(chan struct{})
		s.planWaveBackfillDone = done
		go s.planWaveBackfillLoop(loopCtx, done)
	})
}

func (s *Server) stopPlanWaveBackfillLoop() {
	if s == nil || s.planWaveBackfillCancel == nil {
		return
	}
	s.planWaveBackfillCancel()
	if s.planWaveBackfillDone != nil {
		<-s.planWaveBackfillDone
	}
}

func (s *Server) planWaveBackfillLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(planWaveBackfillInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.planWaveBackfillPass(ctx); n > 0 {
				logBackfill(n)
			}
		}
	}
}

func logBackfill(n int) {
	log.Printf("[plan-backfill] pass materialized waves for %d run(s)", n)
}

// planWaveBackfillPass is one 30s sweep over active runs. Split from the loop
// for tests. Returns the number of runs whose wave hook actually
// materialized something.
func (s *Server) planWaveBackfillPass(ctx context.Context) int {
	if s == nil || s.controlDB == nil || s.ts == nil {
		return 0
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || strings.TrimSpace(workspaceID) == "" {
		return 0
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	records, err := s.ts.ListAllTaskRecords("")
	if err != nil {
		return 0
	}
	materialized := 0
	for _, rec := range records {
		if ctx.Err() != nil {
			return materialized
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
		if err != nil || !found || run.Status != "active" || strings.TrimSpace(run.ActiveStepID) == "" {
			continue
		}
		def, defFound, err := wfStore.RunDefinition(run)
		if err != nil || !defFound {
			continue
		}
		var activeStep *entity.WorkflowStep
		for i := range def.Steps {
			if def.Steps[i].ID == run.ActiveStepID {
				activeStep = &def.Steps[i]
				break
			}
		}
		// Only plan-driven parallel stages are backfill territory; a legacy
		// static stage has nothing held back to re-drive.
		if activeStep == nil || activeStep.Type != "parallel_stage" || !planDriven(*activeStep) {
			continue
		}
		if _, _, _, planErr := wfStore.LoadFrozenPlanForRun(project, run.ID); planErr != nil {
			continue
		}
		instances, err := wfStore.ListStepInstances(run.ID)
		if err != nil {
			continue
		}
		var activeInst *entity.WorkflowStepInstance
		for i := range instances {
			if instances[i].StepID == run.ActiveStepID {
				activeInst = &instances[i]
				break
			}
		}
		if activeInst == nil {
			continue
		}
		did, waveErr := s.materializeNextPlanWave(workspaceID, project, agent, task, workflowstore.BranchTransitionResult{
			Branch: entity.WorkflowBranchInstance{
				RunID:    run.ID,
				StepID:   run.ActiveStepID,
				BranchID: run.ActiveStepID,
			},
			Transition: workflowstore.TransitionResult{Run: run, Current: *activeInst},
		}, nil)
		if waveErr != nil {
			// A blocked plan or an in-flight claim refuses loudly elsewhere;
			// the sweep just waits for the next tick.
			continue
		}
		if did {
			materialized++
		}
	}
	return materialized
}
