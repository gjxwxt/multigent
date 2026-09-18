package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/fixturesandbox"
	_ "modernc.org/sqlite"
)

func writeContractForAPITest(t *testing.T, dir string) {
	t.Helper()
	dotM := filepath.Join(dir, ".multigent")
	if err := os.MkdirAll(dotM, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{
  "version": 1,
  "engine": "sqlite",
  "storage": "server/data/app.db",
  "defaultFixtureVersion": "1.0.0",
  "schemaFingerprintPaths": ["server/data/schema.sql"],
  "generator": {
    "command": "seed-cmd",
    "timeoutSeconds": 30
  },
  "scenarios": {
    "edge_cases": {
      "description": "Boundary conditions scenario",
      "command": "seed-edge"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dotM, "fixtures.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	schemaDir := filepath.Join(dir, "server", "data")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "schema.sql"), []byte("CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT);"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setupFixtureSandboxTestEnv(t *testing.T) (*Server, string, string, string) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	grantProjectRoleForTest(t, s, workspaceID, "user-viewer", ProjectRoleViewer)
	grantProjectRoleForTest(t, s, workspaceID, "user-operator", ProjectRoleOperator)

	worktreeDir := t.TempDir()
	writeContractForAPITest(t, worktreeDir)

	task := &entity.Task{
		ID:          "t-fs-1",
		Title:       "Fixture Sandbox Task",
		Status:      entity.TaskStatusInProgress,
		WorktreeDir: worktreeDir,
		BranchName:  "task/fs-1",
	}
	if err := s.ts.AddTask("sample", "agent", task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	dataDir := t.TempDir()
	store := fixturesandbox.NewStore(s.controlDB, workspaceID, dataDir)
	gen := func(ctx context.Context, wtDir, argv string, timeout time.Duration) (string, error) {
		out := filepath.Join(wtDir, "server", "data", "app.db")
		_ = os.Remove(out)
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		db, err := sql.Open("sqlite", out)
		if err != nil {
			return "", err
		}
		defer db.Close()
		label := "default-data"
		if strings.Contains(argv, "edge") {
			label = "edge-data"
		}
		stmt := fmt.Sprintf("CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT); INSERT INTO items VALUES (1, '%s');", label)
		if _, err := db.Exec(stmt); err != nil {
			return "", err
		}
		return out, nil
	}
	s.fixtureSandbox = fixturesandbox.NewProvisioner(store, gen)

	return s, workspaceID, worktreeDir, task.ID
}

func TestFixtureSandbox_GetStatus_ContractAndContractLess(t *testing.T) {
	s, _, _, taskID := setupFixtureSandboxTestEnv(t)

	// 1. Unauthenticated request via server handler -> 401
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox", nil)
	unauthRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauth, got %d", unauthRec.Code)
	}

	// 2. Query contract-bearing task status -> 200 with contract details
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox", "user-viewer", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", taskID)
	rec := httptest.NewRecorder()
	s.handleGetTaskFixtureSandbox(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var status fixturesandbox.TaskSandboxStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.HasContract {
		t.Fatalf("expected hasContract=true")
	}
	if status.Engine != "sqlite" || status.Storage != "server/data/app.db" {
		t.Fatalf("unexpected engine/storage: %+v", status)
	}
	if status.ActiveScenario != "default" {
		t.Fatalf("expected activeScenario default, got %s", status.ActiveScenario)
	}
	if len(status.AvailableScenarios) < 2 {
		t.Fatalf("expected at least 2 scenarios, got %d", len(status.AvailableScenarios))
	}

	// 3. Query contract-less task -> 200 with hasContract=false
	emptyDir := t.TempDir()
	task2 := &entity.Task{
		ID:          "t-fs-nocontract",
		Title:       "No contract task",
		Status:      entity.TaskStatusInProgress,
		WorktreeDir: emptyDir,
	}
	_ = s.ts.AddTask("sample", "agent", task2)

	req2 := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-fs-nocontract/fixture-sandbox", "user-viewer", nil)
	req2.SetPathValue("name", "sample")
	req2.SetPathValue("taskId", "t-fs-nocontract")
	rec2 := httptest.NewRecorder()
	s.handleGetTaskFixtureSandbox(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var status2 fixturesandbox.TaskSandboxStatus
	if err := json.Unmarshal(rec2.Body.Bytes(), &status2); err != nil {
		t.Fatalf("decode status2: %v", err)
	}
	if status2.HasContract {
		t.Fatalf("expected hasContract=false for contract-less task")
	}
}

func TestFixtureSandbox_Reset_RBACAndRestoresBaseline(t *testing.T) {
	s, _, worktreeDir, taskID := setupFixtureSandboxTestEnv(t)

	// Provision preview initially
	_, err := s.fixtureSandbox.ProvisionForPreview(context.Background(), taskID, "sample", worktreeDir)
	if err != nil {
		t.Fatalf("ProvisionForPreview: %v", err)
	}

	// 1. Unauthenticated POST -> 401
	unauthReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox/reset", nil)
	unauthRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauth, got %d", unauthRec.Code)
	}

	// 2. Viewer role POST -> 403 Forbidden
	viewerReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox/reset", "user-viewer", nil)
	viewerReq.SetPathValue("name", "sample")
	viewerReq.SetPathValue("taskId", taskID)
	viewerRec := httptest.NewRecorder()
	s.handleResetTaskFixtureSandbox(viewerRec, viewerReq)
	if viewerRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer, got %d: %s", viewerRec.Code, viewerRec.Body.String())
	}

	// 3. Operator role POST -> 200 OK
	opReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox/reset", "user-operator", nil)
	opReq.SetPathValue("name", "sample")
	opReq.SetPathValue("taskId", taskID)
	opRec := httptest.NewRecorder()
	s.handleResetTaskFixtureSandbox(opRec, opReq)
	if opRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for operator, got %d: %s", opRec.Code, opRec.Body.String())
	}

	var resetResp map[string]any
	if err := json.Unmarshal(opRec.Body.Bytes(), &resetResp); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	if resetResp["success"] != true {
		t.Fatalf("expected success: true, got %+v", resetResp)
	}
	if resetResp["resetCount"].(float64) != 1 {
		t.Fatalf("expected resetCount: 1, got %v", resetResp["resetCount"])
	}
}

func TestFixtureSandbox_SwitchScenario_AndValidation(t *testing.T) {
	s, _, _, taskID := setupFixtureSandboxTestEnv(t)

	// 1. Viewer role -> 403 Forbidden
	reqPayload := map[string]string{"scenario": "edge_cases"}
	viewerReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox/scenario", "user-viewer", reqPayload)
	viewerReq.SetPathValue("name", "sample")
	viewerReq.SetPathValue("taskId", taskID)
	viewerRec := httptest.NewRecorder()
	s.handleSwitchTaskFixtureSandboxScenario(viewerRec, viewerReq)
	if viewerRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer, got %d: %s", viewerRec.Code, viewerRec.Body.String())
	}

	// 2. Operator switches to invalid scenario -> 400 Bad Request
	invPayload := map[string]string{"scenario": "non_existent"}
	invReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox/scenario", "user-operator", invPayload)
	invReq.SetPathValue("name", "sample")
	invReq.SetPathValue("taskId", taskID)
	invRec := httptest.NewRecorder()
	s.handleSwitchTaskFixtureSandboxScenario(invRec, invReq)
	if invRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for undeclared scenario, got %d: %s", invRec.Code, invRec.Body.String())
	}

	// 3. Operator switches to edge_cases -> 200 OK
	opReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox/scenario", "user-operator", reqPayload)
	opReq.SetPathValue("name", "sample")
	opReq.SetPathValue("taskId", taskID)
	opRec := httptest.NewRecorder()
	s.handleSwitchTaskFixtureSandboxScenario(opRec, opReq)
	if opRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for edge_cases, got %d: %s", opRec.Code, opRec.Body.String())
	}

	var swResp map[string]any
	if err := json.Unmarshal(opRec.Body.Bytes(), &swResp); err != nil {
		t.Fatalf("decode switch response: %v", err)
	}
	if swResp["scenario"] != "edge_cases" {
		t.Fatalf("expected scenario edge_cases, got %v", swResp["scenario"])
	}

	// 4. Query status verifies scenario is now edge_cases
	statReq := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/"+taskID+"/fixture-sandbox", "user-viewer", nil)
	statReq.SetPathValue("name", "sample")
	statReq.SetPathValue("taskId", taskID)
	statRec := httptest.NewRecorder()
	s.handleGetTaskFixtureSandbox(statRec, statReq)
	var status fixturesandbox.TaskSandboxStatus
	_ = json.Unmarshal(statRec.Body.Bytes(), &status)
	if status.ActiveScenario != "edge_cases" {
		t.Fatalf("expected active scenario edge_cases, got %s", status.ActiveScenario)
	}
}

func TestFixtureSandbox_TaskNotFound_Returns404(t *testing.T) {
	s, _, _, _ := setupFixtureSandboxTestEnv(t)

	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-non-existent/fixture-sandbox", "user-viewer", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", "t-non-existent")
	rec := httptest.NewRecorder()
	s.handleGetTaskFixtureSandbox(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing task, got %d: %s", rec.Code, rec.Body.String())
	}
}
