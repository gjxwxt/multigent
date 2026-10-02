package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedEntryProbeRun seeds a parent run parked at a parallel stage with the
// given step definition (no frozen plan is ever written), so a single fan-out
// call exercises the planStageBranches entry decision.
func seedEntryProbeRun(t *testing.T, s *Server, workspaceID, baseCommit string, stage entity.WorkflowStep) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-entry-probe", Name: "Entry probe", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID: "start", Type: "agent_task", Title: "Contract", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
			},
			stage,
		},
		Edges:     []entity.WorkflowEdge{{From: "start", To: stage.ID}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	parentTask := &entity.Task{
		ID: "task-entry-probe", Title: "Entry probe root", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "sample/pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main", Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", parentTask); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", parentTask.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent":     {Type: "agent", ID: "pm"},
		stage.ID:       {Type: "agent", ID: "pm"},
		"pm":           {Type: "agent", ID: "pm"},
		"workstream_1": {Type: "agent", ID: "pm"},
		"workstream_2": {Type: "agent", ID: "pm"},
	}); err != nil {
		t.Fatal(err)
	}
	return parentTask
}

// fanOutEntryProbe completes the start step through the production endpoint,
// driving the real fan-out path for the parked parallel stage.
func fanOutEntryProbe(t *testing.T, s *Server, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	return postBranchStepComplete(t, s, workspaceID, "task-entry-probe", map[string]string{
		"branch_summary": "contract landed",
		"touched_paths":  "contract.md",
	})
}

// containsPlanRefusal reports whether the response body carries the frozen
// plan refusal reason.
func containsPlanRefusal(body string) bool {
	return strings.Contains(body, "frozen delivery plan") ||
		strings.Contains(body, "no frozen delivery plan") ||
		strings.Contains(body, "declares plan inputs") ||
		strings.Contains(body, "plan-driven")
}

// --- The legacy-regression gate MUST be green BEFORE the conditional entry
// --- decision changes; it is the only guard against T2 (unconditionally
// --- deleting the static fallback would brick every legacy parallel stage).

// TestLegacyStaticParallelStageStillFallsBack is the T2 regression: a stage
// with static branches, no planMaterialization config, and NO plan input
// declaration keeps materializing its static branches when no frozen plan
// exists. Deleting the fallback unconditionally would fail this.
func TestLegacyStaticParallelStageStillFallsBack(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	stage := entity.WorkflowStep{
		ID: "parallel", Type: "parallel_stage", Title: "Legacy static stage", JoinPolicy: "all",
		Branches: []entity.WorkflowBranch{
			{ID: "workstream_1", Title: "Workstream 1", ActorRole: "pm",
				InputFields:  []entity.WorkflowField{{Name: "contract_artifacts"}},
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}}},
			{ID: "workstream_2", Title: "Workstream 2", ActorRole: "pm",
				InputFields:  []entity.WorkflowField{{Name: "contract_artifacts"}},
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}}},
		},
	}
	seedEntryProbeRun(t, s, workspaceID, baseCommit, stage)
	rec := fanOutEntryProbe(t, s, workspaceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy static stage must fall back to its static branches, got %d: %s", rec.Code, rec.Body.String())
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	runID := planRunIDFor(t, s, workspaceID, "task-entry-probe")
	if _, found := branchInstanceFor(t, wfStore, runID, "parallel", "workstream_1"); !found {
		t.Fatal("static branch workstream_1 must materialize on a legacy stage")
	}
	if _, found := branchInstanceFor(t, wfStore, runID, "parallel", "workstream_2"); !found {
		t.Fatal("static branch workstream_2 must materialize on a legacy stage")
	}
}

// TestStageDeclaringPlanInputsWithoutFrozenPlanRefuses: a stage that DECLARES
// plan inputs (delivery_plan / batch_plan) on its InputFields but lost the
// planMaterialization config (canvas edit, pre-25445bcf instantiation) must
// refuse rather than silently materialize unapproved static branches — the
// declaration itself promises plan-driven behavior.
func TestStageDeclaringPlanInputsWithoutFrozenPlanRefuses(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	stage := entity.WorkflowStep{
		ID: "parallel", Type: "parallel_stage", Title: "Declared but unconfigured stage", JoinPolicy: "all",
		InputFields: []entity.WorkflowField{
			{Name: "delivery_plan"},
			{Name: "batch_plan"},
		},
		Branches: []entity.WorkflowBranch{
			{ID: "workstream_1", Title: "Workstream 1", ActorRole: "pm",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}}},
		},
	}
	seedEntryProbeRun(t, s, workspaceID, baseCommit, stage)
	rec := fanOutEntryProbe(t, s, workspaceID)
	if rec.Code == http.StatusOK {
		t.Fatalf("a stage declaring plan inputs without a frozen plan must refuse, got 200: %s", rec.Body.String())
	}
	if !containsPlanRefusal(rec.Body.String()) {
		t.Fatalf("refusal must carry the plan-refusal reason, got: %s", rec.Body.String())
	}
}

// TestConfigDrivenStageWithoutFrozenPlanStillRefuses keeps the pre-existing
// plan-driven refusal (config path) intact after the conditional rework.
func TestConfigDrivenStageWithoutFrozenPlanStillRefuses(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	stage := entity.WorkflowStep{
		ID: "parallel", Type: "parallel_stage", Title: "Config-driven stage", JoinPolicy: "all",
		Config: map[string]string{planMaterializationConfigKey: "frozen"},
	}
	seedEntryProbeRun(t, s, workspaceID, baseCommit, stage)
	rec := fanOutEntryProbe(t, s, workspaceID)
	if rec.Code == http.StatusOK {
		t.Fatalf("config-driven stage without a frozen plan must refuse, got 200: %s", rec.Body.String())
	}
}
