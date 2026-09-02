package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedDesignGateRun parks a greenfield run on the design_review step for the
// seeded task, mirroring the live flow the UI drives.
func seedDesignGateRun(t *testing.T, s *Server, workspaceID string, taskID string) {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "snapshot test")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRunWithInput("resproj", taskID, def.ID, nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	run, found, err := wfStore.RunForTask("resproj", taskID)
	if err != nil || !found {
		t.Fatalf("run not found: %v", err)
	}
	run.ActiveStepID = "design_review"
	run.Status = "active"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range instances {
		if instances[i].StepID == "design_review" {
			instances[i].Status = "open"
			instances[i].StartedAt = now
			if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestReviewFreezesDesignSnapshot asserts the frozen-contract behaviour: an
// approve on the design gate captures the OD artifacts at that moment and
// carries the HTML + bundle path in the step outputs, so downstream steps are
// decoupled from later OD edits.
func TestReviewFreezesDesignSnapshot(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	fake := newFakeODClient()
	fake.files = []ODProjectFile{
		{Path: "task-manager.html", Kind: "html", Size: 300},
		{Path: "styles.css", Kind: "css", Size: 60},
	}
	fake.fileContents = map[string][]byte{
		"task-manager.html": []byte("<html><body>approved design v1</body></html>"),
		"styles.css":        []byte("body{color:#111}"),
	}
	s.designClient = fake

	body := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_snapshot_test"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/t-res-1/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("review: %d %s", w.Code, w.Body.String())
	}

	// Outputs carry the frozen HTML inline.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatalf("run: %v", err)
	}
	if run.ActiveStepID != "implementation" {
		t.Fatalf("expected run to advance to implementation, at %s", run.ActiveStepID)
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var impl *entity.WorkflowStepInstance
	for i := range instances {
		if instances[i].StepID == "implementation" {
			impl = &instances[i]
		}
	}
	if impl == nil {
		t.Fatal("implementation instance missing")
	}
	if got := impl.InputValues["approved_design_html"]; !strings.Contains(got, "approved design v1") {
		t.Fatalf("frozen html not carried into implementation inputs: %q", got)
	}
	if got := impl.InputValues["approved_design_snapshot_path"]; !strings.Contains(got, task.ID+"/manifest.json") {
		t.Fatalf("snapshot path not carried: %q", got)
	}

	// The bundle exists on disk with manifest + artifacts.
	manifestPath := filepath.Join(s.st.Root(), designSnapshotDir, task.ID, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !strings.Contains(string(raw), `"entry": "task-manager.html"`) {
		t.Fatalf("manifest entry wrong: %s", raw)
	}
	htmlOnDisk, err := os.ReadFile(filepath.Join(s.st.Root(), designSnapshotDir, task.ID, "task-manager.html"))
	if err != nil || !strings.Contains(string(htmlOnDisk), "approved design v1") {
		t.Fatalf("snapshot artifact missing/broken: %v", err)
	}
}

// TestReviewDesignSnapshotFailureDoesNotBlock asserts the degrade path: when
// OD cannot serve the files the review still advances (project id remains the
// contract) and the failure leaves a comment trail.
func TestReviewDesignSnapshotFailureDoesNotBlock(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	fake := newFakeODClient()
	fake.files = nil // list returns empty → snapshot degrades
	s.designClient = fake

	body := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_snapshot_fail"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/t-res-1/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("review must not block on snapshot failure: %d %s", w.Code, w.Body.String())
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found || run.ActiveStepID != "implementation" {
		t.Fatalf("run should advance despite snapshot failure: found=%v active=%s err=%v", found, run.ActiveStepID, err)
	}
}
