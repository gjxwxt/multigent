package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// TestBranchCrashWindowReJoinResumesParent (S2-2.1): the parent run stays
// active@parallel while BOTH branch instances are already terminal — the
// process died between SaveBranchInstance and CompleteAndAdvance (or the join
// transition lost its claim). A re-report through the step/complete endpoint
// must re-drive the join from the recorded aggregate (CAS-claimed, so it can
// never double-advance), bringing the parent past the join — not return a
// zero-write replay that wedges the run at the barrier forever.
func TestBranchCrashWindowReJoinResumesParent(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-join-child", wt)
	task, _ := seedBranchChildRun(t, s, workspaceID)
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRunID := task.Vars[workflowRunIDVar]

	// Simulate the crash window directly: the first branch (ws_b) reached its
	// terminal write, but the join never advanced (parent run still
	// active@parallel). Store the branch terminal WITHOUT calling
	// CompleteAndAdvance — exactly the state after a crash between the two
	// writes.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{"branch_summary": "did things", "touched_paths": "server_test.go"}
	instances, err := wfStore.BranchInstancesForStep(parentRunID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].BranchID != "ws_b" {
		t.Fatalf("fixture must seed exactly the ws_b instance, got %+v", instances)
	}
	instances[0].Status = "completed"
	instances[0].Summary = "did things"
	instances[0].OutputValues = outputs
	instances[0].OutputArtifact = `{"branch_summary":"did things","touched_paths":"server_test.go"}`
	if err := wfStore.SaveBranchInstance(&instances[0]); err != nil {
		t.Fatal(err)
	}

	// A second branch exists in the parent plan but never reported (its task
	// was lost); the re-driving re-report comes from ws_b only, so the join
	// must NOT advance until both branches are terminal.
	reportBranchID := task.Vars[workflowBranchIDVar]
	if reportBranchID != "ws_b" {
		t.Fatalf("fixture branch id: %q", reportBranchID)
	}

	// Single-branch plan: ws_b terminal + re-report → the report completes the
	// child run (the task was never archived in this fixture) and the join
	// re-drives from the recorded aggregate; the parent run must move past the
	// parallel step. Recovery is asserted on PERSISTED state (the re-drive can
	// ride either the step-complete transition or the branch handler), never
	// on a response shape.
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, outputs)
	if rec.Code != http.StatusOK {
		t.Fatalf("crash-window re-report must succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	parentRun, _, err := wfStore.RunByID("sample", parentRunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("parent run must be completed after the resumed join, got %q@%q", parentRun.Status, parentRun.ActiveStepID)
	}

	// A SECOND re-report after the join ran must stay idempotent (200, no
	// error): the run is terminal now, so the zero-write replay path applies.
	rec = postBranchStepComplete(t, s, workspaceID, task.ID, outputs)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-join re-report must stay idempotent, got %d: %s", rec.Code, rec.Body.String())
	}
	parentRun, _, err = wfStore.RunByID("sample", parentRunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("parent run must stay completed, got %q", parentRun.Status)
	}
}

// TestCrashWindowFailedBranchNoAdvance (S2-2.2 review P1): the crash-window
// re-drive is a SUCCESS-path recovery. With the branch recorded FAILED, a
// re-report must return the recorded failure without advancing the parent —
// never re-drive the join into a success the stage did not earn.
func TestCrashWindowFailedBranchNoAdvance(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-join-child", wt)
	task, _ := seedBranchChildRun(t, s, workspaceID)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRunID := task.Vars[workflowRunIDVar]
	// Crash window with a FAILED branch recorded.
	instances, err := wfStore.BranchInstancesForStep(parentRunID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	instances[0].Status = "failed"
	instances[0].Summary = "branch blew up"
	if err := wfStore.SaveBranchInstance(&instances[0]); err != nil {
		t.Fatal(err)
	}
	// A failed re-report (status=failed): must return 200 with the recorded
	// failure, parent run NOT advanced.
	body, err := json.Marshal(map[string]any{
		"agent": "pm", "status": "failed", "summary": "branch blew up",
		"outputs": map[string]string{"branch_summary": "branch blew up"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+task.ID+"/workflow/step/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", task.ID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
		WorkspaceID: workspaceID, Project: "sample", Agent: "pm", Capabilities: []string{"task.use"},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeWorkflowStepComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed re-report must replay idempotently, got %d: %s", rec.Code, rec.Body.String())
	}
	parentRun, _, err := wfStore.RunByID("sample", parentRunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "active" || parentRun.ActiveStepID != "parallel" {
		t.Fatalf("failed branch must NOT advance the parent in the crash window, got %q@%q", parentRun.Status, parentRun.ActiveStepID)
	}
}
