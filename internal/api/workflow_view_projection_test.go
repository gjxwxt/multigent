package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// GET task-workflow view projection (canvas subflow realism): plan-driven
// parallel stages materialize branches that never live in the definition
// snapshot, so the read path overlays the frozen-plan branch definitions onto
// the served definition. Persistence must stay untouched.
func TestTaskWorkflowViewProjectsPlanDerivedBranches(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/sample/tasks/task-plan-root/workflow", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", "task-plan-root")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	w := httptest.NewRecorder()
	s.handleGetTaskWorkflow(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET workflow must be 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp taskWorkflowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	var parallel *entity.WorkflowStep
	for i := range resp.Definition.Steps {
		if resp.Definition.Steps[i].ID == "parallel" {
			parallel = &resp.Definition.Steps[i]
		}
	}
	if parallel == nil {
		t.Fatal("parallel step missing from served definition")
	}
	if len(parallel.Branches) != 2 {
		t.Fatalf("served parallel step must project the 2 materialized work packages, got %d", len(parallel.Branches))
	}
	seen := map[string]string{}
	for _, b := range parallel.Branches {
		seen[b.ID] = b.Title
	}
	if seen["wp-a"] != "Work package A" || seen["wp-b"] != "Work package B" {
		t.Fatalf("projected branches must carry the plan titles, got %v", seen)
	}

	// The instance rows must line up with the projected definitions so the
	// canvas preview can join status by branchId.
	instances, err := wfStore.BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != len(parallel.Branches) {
		t.Fatalf("instance/definition count mismatch: %d vs %d", len(instances), len(parallel.Branches))
	}

	// The stored definition must remain untouched (read-path only).
	stored, found, err := wfStore.RunDefinition(entity.WorkflowRun{ID: runID, DefinitionID: "wf-plan-parent"})
	if err != nil || !found {
		t.Fatalf("stored definition lookup: found=%v err=%v", found, err)
	}
	for _, step := range stored.Steps {
		if step.ID == "parallel" && len(step.Branches) != 0 {
			t.Fatalf("persistence must stay untouched, stored parallel step grew %d branches", len(step.Branches))
		}
	}

	// wp-c (dependency not yet complete) must not appear in the projection.
	if _, ok := seen["wp-c"]; ok {
		t.Fatal("wp-c is not materialized yet and must not be projected")
	}
}

// A legacy parallel stage whose branches are all static (ids match the
// definition) must keep the definition as-is — the projection only overlays
// when instance branchIds fall outside the static list.
func TestTaskWorkflowViewKeepsLegacyStaticBranches(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/sample/tasks/task-plan-root/workflow", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", "task-plan-root")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	w := httptest.NewRecorder()
	s.handleGetTaskWorkflow(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET workflow must be 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp taskWorkflowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, step := range resp.Definition.Steps {
		if step.ID == "parallel" && len(step.Branches) != 0 {
			t.Fatal("stage with no materialized branch instances must keep the definition untouched")
		}
	}
}

// A plan-driven stage whose static placeholder ids collide with the
// materialized instance ids (operator scoped them on the canvas before the
// run reached the stage) must keep the static definition — the projection
// overlays only foreign ids.
func TestTaskWorkflowViewKeepsMatchingStaticBranches(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)

	rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The run serves from its DEFINITION SNAPSHOT: inject hand-scoped static
	// branches (matching the work-package ids) into the run's snapshot, the
	// way a canvas edit before the stage would have.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	snap, found, err := wfStore.RunDefinition(run)
	if err != nil || !found {
		t.Fatalf("run definition: found=%v err=%v", found, err)
	}
	for i := range snap.Steps {
		if snap.Steps[i].ID == "parallel" {
			snap.Steps[i].Branches = []entity.WorkflowBranch{
				{ID: "wp-a", Title: "Hand-scoped A", ActorRole: "pm"},
				{ID: "wp-b", Title: "Hand-scoped B", ActorRole: "pm"},
			}
		}
	}
	run.DefinitionSnapshot = &snap
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/sample/tasks/task-plan-root/workflow", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", "task-plan-root")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	w := httptest.NewRecorder()
	s.handleGetTaskWorkflow(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET workflow must be 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp taskWorkflowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, step := range resp.Definition.Steps {
		if step.ID != "parallel" {
			continue
		}
		if len(step.Branches) != 2 || step.Branches[0].Title != "Hand-scoped A" {
			t.Fatalf("static branches matching instance ids must stay hand-scoped, got %+v", step.Branches)
		}
	}
}
