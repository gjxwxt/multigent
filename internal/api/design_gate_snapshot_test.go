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

// TestReviewDesignSnapshotAtomicFailureRequiresWaiver asserts the fail-closed
// behaviour: when OD cannot serve the files, review is blocked with
// design_snapshot_failed unless an explicit audited waiver reason is provided.
func TestReviewDesignSnapshotAtomicFailureRequiresWaiver(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	fake := newFakeODClient()
	fake.files = nil // list returns empty → snapshot capture fails
	s.designClient = fake

	// 1. Attempt approval without waiver: must fail-closed with 400 design_snapshot_failed
	body := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_snapshot_fail"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/t-res-1/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("review without waiver must be blocked: expected 400, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "design_snapshot_failed") {
		t.Fatalf("expected design_snapshot_failed error code, got %s", w.Body.String())
	}

	// 2. Retry with explicit design_waiver_reason: must succeed and carry design_waived="true"
	bodyWithWaiver := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_snapshot_fail","design_waiver_reason":"OD unreachable, approved via static design doc"}}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/t-res-1/workflow/review", strings.NewReader(bodyWithWaiver))
	req2 = req2.WithContext(context.WithValue(req2.Context(), ctxUserKey, "admin"))
	req2.SetPathValue("name", "resproj")
	req2.SetPathValue("taskId", task.ID)
	w2 := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("review with waiver must succeed: %d %s", w2.Code, w2.Body.String())
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found || run.ActiveStepID != "implementation" {
		t.Fatalf("run should advance to implementation: found=%v active=%s err=%v", found, run.ActiveStepID, err)
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
	if got := impl.InputValues["design_waived"]; got != "true" {
		t.Fatalf("expected design_waived=true in implementation inputs, got %q", got)
	}
	if got := impl.InputValues["design_waiver_reason"]; !strings.Contains(got, "OD unreachable") {
		t.Fatalf("expected design_waiver_reason in implementation inputs, got %q", got)
	}
}

// TestDesignSnapshotArtifactsKeepsCompanionsByExtension locks the 2026-09-03
// 4test fix: companions are selected by file extension, because OpenDesign
// reports css/js with kind "code" (not "css"/"js"). The old code matched kind
// exactly and dropped every companion, freezing only the entry HTML shell.
func TestDesignSnapshotArtifactsKeepsCompanionsByExtension(t *testing.T) {
	files := []ODProjectFile{
		{Path: "index.html", Kind: "html", Size: 595},
		{Path: "styles.css", Kind: "code", Size: 15997},
		{Path: "js/app.js", Kind: "code", Size: 14195},
		{Path: "js/api.js", Kind: "code", Size: 42939},
		{Path: "README.md", Kind: "text", Size: 4559},
	}

	got := designSnapshotArtifacts(files)

	want := []string{"index.html", "js/api.js", "js/app.js", "styles.css"}
	if len(got) != len(want) {
		t.Fatalf("expected %d artifacts, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("position %d: want %s, got %s", i, w, got[i].Path)
		}
	}
	// Entry HTML must sort first so it becomes the inline/entry artifact.
	if got[0].Path != "index.html" {
		t.Errorf("entry HTML must be first, got %s", got[0].Path)
	}
	// README.md (kind "text", .md extension) must be excluded.
	for _, g := range got {
		if g.Path == "README.md" {
			t.Errorf("non html/css/js file should not be kept: %s", g.Path)
		}
	}
}

// TestIsHTML covers the extension helper used for html-first ordering.
func TestIsHTML(t *testing.T) {
	cases := map[string]bool{
		"index.html": true, "INDEX.HTML": true, "a/b/c.html": true,
		"styles.css": false, "app.js": false, "readme.md": false,
	}
	for path, want := range cases {
		if got := isHTML(path); got != want {
			t.Errorf("isHTML(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestDesignReviewApprovalFailsClosedWithoutReferenceOrWaiver proves P0 security fix:
// Approval cannot bypass design verification when neither design reference nor waiver is provided.
func TestDesignReviewApprovalFailsClosedWithoutReferenceOrWaiver(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	// Attempt approval with empty outputs (no project ID, no snapshot, no waiver)
	body := `{"decision":"approve","comments":"trying to bypass without design"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/t-res-1/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "design verification requires either an approved design reference or an explicit design_waiver_reason") {
		t.Fatalf("expected fail-closed error message, got: %s", w.Body.String())
	}
}

// TestDesignReviewApprovalFailsClosedWithForgedSnapshotPath proves that an attacker
// cannot bypass design verification by supplying a forged approved_design_snapshot_path.
// The server cleanses client-supplied snapshot paths and attempts to capture from OD;
// since the OD project ID is fake or capture fails, without a waiver it must fail-closed with 400.
func TestDesignReviewApprovalFailsClosedWithForgedSnapshotPath(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	// Case A: Forged project ID + forged snapshot path (OD capture fails, no waiver -> 400 design_snapshot_failed)
	body := `{
		"decision": "approve",
		"comments": "trying to bypass with forged snapshot path",
		"outputs": {
			"approved_design_project_id": "forged-od-proj-999",
			"approved_design_snapshot_path": "snapshots/forged-task-id/manifest.json",
			"approved_design_html": "<html><body>forged</body></html>"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on forged project ID + snapshot path, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "design_snapshot_failed") {
		t.Fatalf("expected design_snapshot_failed error message, got: %s", w.Body.String())
	}

	// Case B: Empty project ID + forged snapshot path (snapshot path cleansed, no waiver -> 400 missing reference/waiver)
	bodyB := `{
		"decision": "approve",
		"comments": "trying to bypass with only forged snapshot path",
		"outputs": {
			"approved_design_snapshot_path": "snapshots/forged-task-id/manifest.json",
			"approved_design_html": "<html><body>forged</body></html>"
		}
	}`
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(bodyB))
	reqB = reqB.WithContext(context.WithValue(reqB.Context(), ctxUserKey, "admin"))
	reqB.SetPathValue("name", "resproj")
	reqB.SetPathValue("taskId", task.ID)
	wB := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wB, reqB)

	if wB.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on forged snapshot path with empty project ID, got %d: %s", wB.Code, wB.Body.String())
	}
	if !strings.Contains(wB.Body.String(), "design verification requires either an approved design reference or an explicit design_waiver_reason") {
		t.Fatalf("expected reference/waiver required error message, got: %s", wB.Body.String())
	}
}

// TestCaptureDesignGateSnapshotRejectsPathTraversal verifies that any attempt by an upstream
// OD artifact path to traverse directories (e.g. "../../etc/passwd") is rejected before writing.
func TestCaptureDesignGateSnapshotRejectsPathTraversal(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	fake := newFakeODClient()
	fake.files = []ODProjectFile{
		{Path: "../../escaping.html", Kind: "html", Size: 100},
	}
	fake.fileContents = map[string][]byte{
		"../../escaping.html": []byte("<html>escaping</html>"),
	}
	s.designClient = fake

	body := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_traversal"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on path traversal in artifact, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "path traversal") {
		t.Fatalf("expected path traversal rejection error, got: %s", w.Body.String())
	}
}

// TestCaptureDesignGateSnapshotRejectsAbsolutePaths verifies that absolute paths like "/etc/passwd"
// are rejected before writing.
func TestCaptureDesignGateSnapshotRejectsAbsolutePaths(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	fake := newFakeODClient()
	fake.files = []ODProjectFile{
		{Path: "/etc/passwd.html", Kind: "html", Size: 100},
	}
	fake.fileContents = map[string][]byte{
		"/etc/passwd.html": []byte("<html>evil</html>"),
	}
	s.designClient = fake

	body := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_abs"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on absolute path artifact, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "path traversal") {
		t.Fatalf("expected path traversal rejection error, got: %s", w.Body.String())
	}
}

// TestCaptureDesignGateSnapshotFailureLeavesNoHalfBakedArtifacts verifies that if capturing
// artifacts fails mid-way, no readable snapshot directory exists and no .tmp-* directories remain.
func TestCaptureDesignGateSnapshotFailureLeavesNoHalfBakedArtifacts(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	seedDesignGateRun(t, s, workspaceID, task.ID)

	fake := newFakeODClient()
	fake.files = []ODProjectFile{
		{Path: "page1.html", Kind: "html", Size: 100},
		{Path: "page2.html", Kind: "html", Size: 100},
	}
	// Only provide page1.html, page2.html fetch will fail
	fake.fileContents = map[string][]byte{
		"page1.html": []byte("<html>page1</html>"),
	}
	s.designClient = fake

	body := `{"decision":"approve","comments":"ok","outputs":{"approved_design_source":"existing","approved_design_project_id":"proj_mid_fail"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on mid-capture failure, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "design_snapshot_failed") {
		t.Fatalf("expected design_snapshot_failed error, got: %s", w.Body.String())
	}

	// 1. Destination directory must not exist
	destDir := filepath.Join(s.st.Root(), designSnapshotDir, task.ID)
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		t.Fatalf("destination directory should not exist after failure, got err: %v", err)
	}

	// 2. No temporary directory (.tmp-*) must remain
	parentDir := filepath.Join(s.st.Root(), designSnapshotDir)
	entries, _ := os.ReadDir(parentDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("found leftover temporary directory: %s", entry.Name())
		}
	}

	// 3. GET /design/snapshot must return 404
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/design/snapshot/page1.html", nil)
	getReq = getReq.WithContext(context.WithValue(getReq.Context(), ctxUserKey, "admin"))
	getReq.SetPathValue("name", "resproj")
	getReq.SetPathValue("taskId", task.ID)
	getReq.SetPathValue("path", "page1.html")
	getW := httptest.NewRecorder()
	s.handleGetDesignSnapshotFile(getW, getReq)
	if getW.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for failed snapshot file, got %d: %s", getW.Code, getW.Body.String())
	}
}

// TestGetDesignSnapshotRejectsSymlinkEscape verifies that a symlink pointing outside the snapshot
// directory is blocked with 400 Bad Request and not served.
func TestGetDesignSnapshotRejectsSymlinkEscape(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusInProgress)

	// Create valid snapshot directory
	taskSnapshotDir := filepath.Join(s.st.Root(), designSnapshotDir, task.ID)
	if err := os.MkdirAll(taskSnapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create an external secret file outside the snapshot directory
	secretFile := filepath.Join(s.st.Root(), "sensitive_secret.txt")
	if err := os.WriteFile(secretFile, []byte("TOP_SECRET_PASSWORD"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink pointing to the external file
	symlinkPath := filepath.Join(taskSnapshotDir, "link_to_secret.txt")
	if err := os.Symlink(secretFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/design/snapshot/link_to_secret.txt", nil)
	getReq = getReq.WithContext(context.WithValue(getReq.Context(), ctxUserKey, "admin"))
	getReq.SetPathValue("name", "resproj")
	getReq.SetPathValue("taskId", task.ID)
	getReq.SetPathValue("path", "link_to_secret.txt")
	getW := httptest.NewRecorder()
	s.handleGetDesignSnapshotFile(getW, getReq)

	if getW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on symlink escape, got %d: %s", getW.Code, getW.Body.String())
	}
	if !strings.Contains(getW.Body.String(), "escaping symlink") {
		t.Fatalf("expected escaping symlink error message, got: %s", getW.Body.String())
	}
	if strings.Contains(getW.Body.String(), "TOP_SECRET_PASSWORD") {
		t.Fatal("secret leaked through symlink!")
	}
}
