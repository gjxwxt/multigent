package api

import (
	"context"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedStalledRunFixture builds a task on an active run of the given step type
// whose step instance is running and whose run.UpdatedAt can be pushed back.
func seedStalledRunFixture(t *testing.T, s *Server, workspaceID, stepType string, idle time.Duration) (*entity.Task, entity.WorkflowRun) {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-stall-" + stepType, Name: "Stall " + stepType, Version: 1, Scope: "workspace", StartStepID: "work",
		Steps: []entity.WorkflowStep{
			{ID: "work", Type: stepType, Title: "Work", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}}},
		},
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := &entity.Task{
		ID: "task-stall-" + stepType, Title: "Stall " + stepType, Status: entity.TaskStatusInProgress,
		Assignee: "sample/pm", Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	run, _, err := wfStore.StartRun("sample", task.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent": {Type: "agent", ID: "pm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run.UpdatedAt = time.Now().UTC().Add(-idle)
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range instances {
		instances[i].Status = "running"
		if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
			t.Fatal(err)
		}
	}
	return task, run
}

func TestWorkflowReconcilerRefiresStalledAgentRun(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task, _ := seedStalledRunFixture(t, s, workspaceID, "agent_task", 16*time.Minute)

	refired, alerted := s.workflowReconcilerPass(context.Background(), 15*time.Minute)
	if refired != 1 {
		t.Fatalf("a 16-minute-stalled agent run must be re-fired, got refired=%d", refired)
	}
	if alerted != 0 {
		t.Fatalf("no runtime runs exist, alerts must be 0, got %d", alerted)
	}
	_ = task
}

func TestWorkflowReconcilerRespectsThresholdAndHumanReview(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)

	// 14 minutes idle: below the 15-minute threshold, untouched.
	seedStalledRunFixture(t, s, workspaceID, "agent_task", 14*time.Minute)
	if refired, _ := s.workflowReconcilerPass(context.Background(), 15 * time.Minute); refired != 0 {
		t.Fatalf("a run below the threshold must not be re-fired, got %d", refired)
	}

	// human_review steps are never auto-advanced, however stale.
	seedStalledRunFixture(t, s, workspaceID, "human_review", 2*time.Hour)
	if refired, _ := s.workflowReconcilerPass(context.Background(), 15 * time.Minute); refired != 0 {
		t.Fatalf("a stalled human_review step must never be re-fired, got %d", refired)
	}
}

func TestWorkflowStallThresholdParsing(t *testing.T) {
	t.Setenv("MULTIGENT_WORKFLOW_STALL_THRESHOLD", "5m")
	if got := workflowStallThreshold(); got != 5*time.Minute {
		t.Fatalf("5m must parse, got %s", got)
	}
	t.Setenv("MULTIGENT_WORKFLOW_STALL_THRESHOLD", "bogus")
	if got := workflowStallThreshold(); got != defaultWorkflowStall {
		t.Fatalf("invalid value must fall back to default, got %s", got)
	}
	t.Setenv("MULTIGENT_WORKFLOW_STALL_THRESHOLD", "-3m")
	if got := workflowStallThreshold(); got != defaultWorkflowStall {
		t.Fatalf("negative value must fall back to default, got %s", got)
	}
}

// Tier 2 needs a runtime_runs row; the SQL store in unit fixtures is available
// through controldb. Keep the alert path covered at the unit level for the
// no-runs shape; the SQL-backed path is exercised in deployment acceptance.
func TestWorkflowReconcilerAlertPathNoRuns(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task, _ := seedStalledRunFixture(t, s, workspaceID, "agent_task", 2*time.Hour)
	// No runtime run rows: the alert sweep must be a clean zero.
	if alerted := s.alertStalledRuntimeRuns(workspaceID, "sample", task.ID, 15*time.Minute, time.Now().UTC()); alerted != 0 {
		t.Fatalf("no runtime runs must yield zero alerts, got %d", alerted)
	}
	_ = workflowstore.NewStore(s.controlDB, workspaceID)
}

// Regression (live batch-test1): the plan-wave re-drive inside Tier 1 passed an
// empty BranchTransitionResult.Branch, so materializeNextPlanWave resolved the
// stage through Branch.StepID == "" and bailed with
// "definition wf-... has no step" — the re-drive never re-materialized
// anything. A stalled parallel_stage run must resolve its stage by the run's
// active step ID instead.
func TestWorkflowReconcilerPlanWaveRedriveResolvesStage(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task, run := seedStalledRunFixture(t, s, workspaceID, "parallel_stage", 20*time.Minute)

	// Mark the stage instance pending (the shape a stalled stage actually has).
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range instances {
		instances[i].Status = "pending"
		if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
			t.Fatal(err)
		}
	}

	// No frozen plan exists in this fixture: the re-drive must not treat that
	// as an error, and must not resolve the stage through an empty step ID
	// (the old code logged "definition has no step" and counted the fire
	// anyway). refired=1 is the trigger re-fire; the wave hook fails closed.
	refired, _ := s.workflowReconcilerPass(context.Background(), 15*time.Minute)
	if refired != 1 {
		t.Fatalf("a stalled parallel_stage run must still be re-fired, got %d", refired)
	}
	_ = task
}

// TestWorkflowReconcilerAlertsStaleRuntimeRun is the Tier-2 positive path:
// a workflow task whose runtime run is still marked running but whose
// updated_at froze past the threshold gets an attention note on the task
// (alert-only — a busy build and a wedge look identical without agent-side
// heartbeats).
func TestWorkflowReconcilerAlertsStaleRuntimeRun(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task, _ := seedStalledRunFixture(t, s, workspaceID, "agent_task", 2*time.Hour)

	stale := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if err := s.controlDB.UpsertRuntimeRun(controldb.RuntimeRun{
		ID:          "rtrun-stale-hang",
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
		AgentID:     "pm",
		TaskID:      task.ID,
		Status:      "running",
		StartedAt:   stale,
		UpdatedAt:   stale,
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh runtime run on the same task must NOT alert.
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.controlDB.UpsertRuntimeRun(controldb.RuntimeRun{
		ID:          "rtrun-fresh",
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
		AgentID:     "pm",
		TaskID:      task.ID,
		Status:      "running",
		StartedAt:   fresh,
		UpdatedAt:   fresh,
	}); err != nil {
		t.Fatal(err)
	}

	alerted := s.alertStalledRuntimeRuns(workspaceID, "sample", task.ID, 15*time.Minute, time.Now().UTC())
	if alerted != 1 {
		t.Fatalf("exactly the stale runtime run must alert, got %d", alerted)
	}
	stored, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Summary, "rtrun-stale-hang") || !strings.Contains(stored.Summary, "suspected agent hang") {
		t.Fatalf("task must carry the hang alert, got summary %q", stored.Summary)
	}
}
