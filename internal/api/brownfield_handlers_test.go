package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/projecttemplate"
	"github.com/multigent/multigent/internal/runtimeauth"
	"github.com/multigent/multigent/internal/sandbox"
)

func TestBrownfieldScan_RBAC_DistinguishesViewerOperatorAndUnauthenticated(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	grantProjectRoleForTest(t, s, workspaceID, "user-viewer", ProjectRoleViewer)
	grantProjectRoleForTest(t, s, workspaceID, "user-operator", ProjectRoleOperator)
	grantProjectRoleForTest(t, s, workspaceID, "user-manager", ProjectRoleManager)

	// Set up project repo directory
	tempDir := t.TempDir()
	p, _ := s.st.Project("sample")
	p.Repo = tempDir
	_ = s.st.SaveProject("sample", p)

	// Create sample package.json + lockfile in repo
	_ = os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"name":"test","scripts":{"dev":"vite"}}`), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "package-lock.json"), []byte(`{}`), 0644)

	// 1. Unauthenticated request via server handler -> 401 Unauthorized
	unauthReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/sample/brownfield/scan", nil)
	unauthRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d: %s", unauthRec.Code, unauthRec.Body.String())
	}

	// 2. Viewer role request -> 403 Forbidden with project_operator_required
	viewerReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/brownfield/scan", "user-viewer", nil)
	viewerReq.SetPathValue("name", "sample")
	viewerRec := httptest.NewRecorder()
	s.handleBrownfieldScan(viewerRec, viewerReq)
	if viewerRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer, got %d: %s", viewerRec.Code, viewerRec.Body.String())
	}
	if !bytes.Contains(viewerRec.Body.Bytes(), []byte(ErrCodeProjectOperatorRequired)) {
		t.Fatalf("expected ErrCodeProjectOperatorRequired, got: %s", viewerRec.Body.String())
	}

	// 3. Operator role request -> 200 OK
	operatorReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/brownfield/scan", "user-operator", nil)
	operatorReq.SetPathValue("name", "sample")
	operatorRec := httptest.NewRecorder()
	s.handleBrownfieldScan(operatorRec, operatorReq)
	if operatorRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for operator, got %d: %s", operatorRec.Code, operatorRec.Body.String())
	}

	var resp brownfieldScanResponse
	if err := json.Unmarshal(operatorRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Evaluation.Status != "ready" {
		t.Fatalf("expected evaluation ready, got: %s", resp.Evaluation.Status)
	}

	// 4. Manager role request -> 200 OK
	managerReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/brownfield/scan", "user-manager", nil)
	managerReq.SetPathValue("name", "sample")
	managerRec := httptest.NewRecorder()
	s.handleBrownfieldScan(managerRec, managerReq)
	if managerRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for manager, got %d: %s", managerRec.Code, managerRec.Body.String())
	}
}

func TestBrownfieldScan_DetectsFullstackProjectAndSynthesizesSpec(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	grantProjectRoleForTest(t, s, workspaceID, "user-operator", ProjectRoleOperator)

	tempDir := t.TempDir()
	if _, err := projecttemplate.Materialize(tempDir, projecttemplate.ReactGoFullstackID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	p, _ := s.st.Project("sample")
	p.Repo = tempDir
	_ = s.st.SaveProject("sample", p)

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/brownfield/scan", "user-operator", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleBrownfieldScan(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp brownfieldScanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal scan response: %v", err)
	}

	if resp.Evaluation.Status != "ready" {
		t.Fatalf("expected evaluation ready, got %s, issues: %+v", resp.Evaluation.Status, resp.Evaluation.Issues)
	}
	if resp.Evaluation.RecommendedProfile != sandbox.ProfileBase {
		t.Fatalf("expected recommended profile base, got %s", resp.Evaluation.RecommendedProfile)
	}
	if resp.Evaluation.SynthesizedRuntime == nil {
		t.Fatalf("expected non-nil synthesized runtime spec")
	}
	if resp.Evaluation.SynthesizedRuntime.Frontend == nil || resp.Evaluation.SynthesizedRuntime.Backend == nil {
		t.Fatalf("expected both frontend and backend in synthesized spec")
	}
}

func TestBrownfieldEvaluate_RuntimeEndpoint(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	tempDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"name":"test","scripts":{"dev":"vite"}}`), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "package-lock.json"), []byte(`{}`), 0644)

	p := &entity.Project{
		Name: "runtime-bf",
		Repo: tempDir,
	}
	_ = s.st.SaveProject("runtime-bf", p)

	// 1. Authorized runtime request with task.use capability
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/brownfield/evaluate", nil)
	principal := runtimeauth.Principal{
		WorkspaceID:  workspaceID,
		Project:      "runtime-bf",
		Agent:        "runner",
		Capabilities: []string{"task.use"},
	}
	reqWithAuth := req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, principal))
	rec := httptest.NewRecorder()
	s.handleRuntimeBrownfieldEvaluate(rec, reqWithAuth)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp brownfieldScanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Evaluation.Status != "ready" {
		t.Fatalf("expected ready, got: %s", resp.Evaluation.Status)
	}

	// 2. Unauthorized runtime request (missing task.use capability) -> 403 Forbidden
	reqNoCap := req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeauth.Principal{
		WorkspaceID:  workspaceID,
		Project:      "runtime-bf",
		Capabilities: []string{"other.cap"},
	}))
	recNoCap := httptest.NewRecorder()
	s.handleRuntimeBrownfieldEvaluate(recNoCap, reqNoCap)
	if recNoCap.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing capability, got %d", recNoCap.Code)
	}
}

// Regression (2026-09-22 VM dogfood): a project created with a forge path in
// the repo field (e.g. "owner/repo", the brownfield bind-remote flow) made the
// scan default to a non-existent relative directory and report a misleading
// empty "not_ready". The workspace materialization must win whenever it
// exists; a forge-style repo value must never be treated as a scan directory.
func TestBrownfieldScan_WorkspacePreferredOverForgeRepoPath(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	grantProjectRoleForTest(t, s, workspaceID, "user-operator", ProjectRoleOperator)

	tempDir := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := projecttemplate.Materialize(tempDir, projecttemplate.ReactGoFullstackID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	p, _ := s.st.Project("sample")
	p.Repo = "root/api-key-hub" // forge path recorded at creation, not a directory
	_ = s.st.SaveProject("sample", p)

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/brownfield/scan", "user-operator", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleBrownfieldScan(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp brownfieldScanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal scan response: %v", err)
	}
	// The scan must have run against the workspace materialization (tempDir),
	// not the forge path.
	if resp.Report.Repo != filepath.Clean(tempDir) {
		t.Fatalf("expected scan rooted at workspace %s, got %s", filepath.Clean(tempDir), resp.Report.Repo)
	}
	if resp.Evaluation.Status != "ready" {
		t.Fatalf("expected evaluation ready from workspace materialization, got %s, issues: %+v", resp.Evaluation.Status, resp.Evaluation.Issues)
	}
}
