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
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedQASignoffRun parks a greenfield run on the qa_signoff step.
func seedQASignoffRun(t *testing.T, s *Server, workspaceID, taskID string, matrixJSON string) {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "qa signoff test")
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
	run.ActiveStepID = "qa_signoff"
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
		if instances[i].StepID == "qa_signoff" {
			instances[i].Status = "open"
			instances[i].StartedAt = now
			if matrixJSON != "" {
				if instances[i].InputValues == nil {
					instances[i].InputValues = map[string]string{}
				}
				instances[i].InputValues["risk_coverage_matrix"] = matrixJSON
			}
			if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestDesignSnapshotPreviewEndpoint(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	root := strings.TrimSpace(s.st.Root())
	if root == "" {
		root = s.root
	}

	snapDir := filepath.Join(root, designSnapshotDir, task.ID)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := designSnapshotManifest{
		Version:   1,
		TaskID:    task.ID,
		ProjectID: "p-snap-prev",
		TakenAt:   time.Now().UTC(),
		Entry:     "index.html",
		Files: []designSnapshotFile{
			{Path: "index.html", Kind: "html", Size: 40},
			{Path: "app.css", Kind: "css", Size: 20},
		},
	}
	rawM, _ := json.Marshal(manifest)
	_ = os.WriteFile(filepath.Join(snapDir, "manifest.json"), rawM, 0o644)
	_ = os.WriteFile(filepath.Join(snapDir, "index.html"), []byte("<html><body>Design Preview</body></html>"), 0o644)
	_ = os.WriteFile(filepath.Join(snapDir, "app.css"), []byte("body { margin: 0; }"), 0o644)

	// 1. Fetch directory (should resolve entry index.html)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/design/snapshot/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	req.SetPathValue("path", "")
	w := httptest.NewRecorder()
	s.handleGetDesignSnapshotFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Design Preview") {
		t.Fatalf("unexpected content: %s", w.Body.String())
	}
	if w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("expected SAMEORIGIN, got %s", w.Header().Get("X-Frame-Options"))
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox allow-scripts allow-forms") || !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("expected strict CSP sandbox with frame-ancestors, got %q", csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got %q", w.Header().Get("X-Content-Type-Options"))
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("expected Referrer-Policy: no-referrer, got %q", w.Header().Get("Referrer-Policy"))
	}

	// 2. Fetch specific file app.css
	reqCss := httptest.NewRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/design/snapshot/app.css", nil)
	reqCss = reqCss.WithContext(context.WithValue(reqCss.Context(), ctxUserKey, "admin"))
	reqCss.SetPathValue("name", "resproj")
	reqCss.SetPathValue("taskId", task.ID)
	reqCss.SetPathValue("path", "app.css")
	wCss := httptest.NewRecorder()
	s.handleGetDesignSnapshotFile(wCss, reqCss)
	if wCss.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.css, got %d: %s", wCss.Code, wCss.Body.String())
	}
	if !strings.Contains(wCss.Body.String(), "margin: 0") {
		t.Fatalf("unexpected css content: %s", wCss.Body.String())
	}

	// 3. Path traversal attempt must be blocked
	reqTrav := httptest.NewRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/design/snapshot/../../agency.yaml", nil)
	reqTrav = reqTrav.WithContext(context.WithValue(reqTrav.Context(), ctxUserKey, "admin"))
	reqTrav.SetPathValue("name", "resproj")
	reqTrav.SetPathValue("taskId", task.ID)
	reqTrav.SetPathValue("path", "../../agency.yaml")
	wTrav := httptest.NewRecorder()
	s.handleGetDesignSnapshotFile(wTrav, reqTrav)
	if wTrav.Code != http.StatusBadRequest {
		t.Fatalf("traversal must return 400, got %d", wTrav.Code)
	}

	// 4. Cross-project task ID access must be blocked (P0 security fix)
	// User has access to otherproj, but requests a task belonging to resproj
	_ = os.MkdirAll(s.st.ProjectDir("otherproj"), 0o755)
	reqCross := httptest.NewRequest(http.MethodGet, "/api/v1/projects/otherproj/tasks/"+task.ID+"/design/snapshot/", nil)
	reqCross = reqCross.WithContext(context.WithValue(reqCross.Context(), ctxUserKey, "admin"))
	reqCross.SetPathValue("name", "otherproj")
	reqCross.SetPathValue("taskId", task.ID)
	reqCross.SetPathValue("path", "")
	wCross := httptest.NewRecorder()
	s.handleGetDesignSnapshotFile(wCross, reqCross)
	if wCross.Code != http.StatusNotFound {
		t.Fatalf("cross-project task access must return 404, got %d: %s", wCross.Code, wCross.Body.String())
	}
	if !strings.Contains(wCross.Body.String(), "task not found in this project") {
		t.Fatalf("expected task not found in this project message, got: %s", wCross.Body.String())
	}
}

func TestQASignoffGateValidation(t *testing.T) {
	// Case 1: Missing risk_coverage_matrix blocks approval
	t.Run("MissingMatrixBlocksApproval", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		seedQASignoffRun(t, s, workspaceID, task.ID, "")

		body := `{"decision":"approve","comments":"looks fine"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing matrix, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "missing required risk_coverage_matrix") {
			t.Fatalf("unexpected error message: %s", w.Body.String())
		}
	})

	// Case 2: High risk item failed hard-blocks approval even if waiver submitted
	t.Run("HighRiskFailedHardBlocks", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixJSON := `[
			{"item_id":"AC-1","risk_level":"high","status":"failed","evidence":"Assertion error on token refresh"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixJSON)

		body := `{"decision":"approve","comments":"trying to pass","outputs":{"manual_waivers":"{\"AC-1\":\"waive it anyway\"}"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for high-risk failed, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "high-risk test item") || !strings.Contains(w.Body.String(), "AC-1") || !strings.Contains(w.Body.String(), "failed") {
			t.Fatalf("unexpected error: %s", w.Body.String())
		}
	})

	// Case 3: High risk item blocked/waived without explicit per-item waiver blocks
	t.Run("HighRiskBlockedWithoutWaiverBlocks", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixJSON := `[
			{"item_id":"AC-PAYMENT","risk_level":"high","status":"blocked","uncovered_reason":"Sandbox payment gateway down"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixJSON)

		body := `{"decision":"approve","comments":"approve without waiver"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for unpassed high-risk without waiver, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "requires explicit manual waiver") {
			t.Fatalf("unexpected error: %s", w.Body.String())
		}
	})

	// Case 4: High risk item blocked with explicit manual waiver passes
	t.Run("HighRiskBlockedWithValidWaiverPasses", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixJSON := `[
			{"item_id":"AC-PAYMENT","risk_level":"high","status":"blocked","uncovered_reason":"Sandbox payment gateway down"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixJSON)

		body := `{"decision":"approve","comments":"approved with manual signoff","outputs":{"manual_waivers":"{\"AC-PAYMENT\":\"Verified manually in sandbox environment with mock token\"}"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 with valid manual waiver, got %d: %s", w.Code, w.Body.String())
		}

		wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
		run, found, err := wfStore.RunForTask("resproj", task.ID)
		if err != nil || !found || run.ActiveStepID != "pr_open_and_merge" {
			t.Fatalf("run should advance to pr_open_and_merge: found=%v active=%s err=%v", found, run.ActiveStepID, err)
		}
	})

	// Case 5: All high risk items passed passes without waiver
	t.Run("AllPassedSucceeds", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixJSON := `[
			{"item_id":"AC-1","risk_level":"high","status":"passed","evidence":"All tests green"},
			{"item_id":"AC-2","risk_level":"medium","status":"passed","evidence":"Unit tests ok"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixJSON)

		body := `{"decision":"approve","comments":"qa looks great"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 when all high risk items passed, got %d: %s", w.Code, w.Body.String())
		}

		wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
		run, found, err := wfStore.RunForTask("resproj", task.ID)
		if err != nil || !found || run.ActiveStepID != "pr_open_and_merge" {
			t.Fatalf("run should advance to pr_open_and_merge: found=%v active=%s err=%v", found, run.ActiveStepID, err)
		}
	})

	// Case 6: Invalid manual_waivers JSON returns 400
	t.Run("InvalidManualWaiversJSONBlocks", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixJSON := `[
			{"item_id":"AC-1","risk_level":"high","status":"passed","evidence":"Unit tests ok"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixJSON)

		body := `{"decision":"approve","comments":"qa test","outputs":{"manual_waivers":"{invalid-json"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid manual_waivers JSON, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid manual_waivers JSON") {
			t.Fatalf("unexpected error: %s", w.Body.String())
		}
	})

	// Case 7: Missing or duplicate item_id returns 400
	t.Run("MissingOrDuplicateItemIDBlocks", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		// Empty item_id
		matrixEmptyID := `[
			{"item_id":"","risk_level":"high","status":"passed","evidence":"all green"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixEmptyID)

		body := `{"decision":"approve","comments":"empty id test"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing required item_id") {
			t.Fatalf("expected 400 missing required item_id, got %d: %s", w.Code, w.Body.String())
		}

		// Duplicate item_id
		matrixDuplicateID := `[
			{"item_id":"AC-DUP","risk_level":"high","status":"passed","evidence":"first"},
			{"item_id":"AC-DUP","risk_level":"low","status":"passed","evidence":"second"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixDuplicateID)
		reqDup := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		reqDup = reqDup.WithContext(context.WithValue(reqDup.Context(), ctxUserKey, "admin"))
		reqDup.SetPathValue("name", "resproj")
		reqDup.SetPathValue("taskId", task.ID)
		wDup := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(wDup, reqDup)
		if wDup.Code != http.StatusBadRequest || !strings.Contains(wDup.Body.String(), "duplicate item_id") {
			t.Fatalf("expected 400 duplicate item_id, got %d: %s", wDup.Code, wDup.Body.String())
		}
	})

	// Case 8: Invalid risk_level or status enum returns 400
	t.Run("InvalidEnumsBlock", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixInvalidRisk := `[
			{"item_id":"AC-1","risk_level":"extreme","status":"passed","evidence":"evidence"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixInvalidRisk)
		body := `{"decision":"approve","comments":"bad risk enum"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid risk_level") {
			t.Fatalf("expected 400 invalid risk_level, got %d: %s", w.Code, w.Body.String())
		}

		matrixInvalidStatus := `[
			{"item_id":"AC-2","risk_level":"high","status":"flying","evidence":"evidence"}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixInvalidStatus)
		reqStatus := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		reqStatus = reqStatus.WithContext(context.WithValue(reqStatus.Context(), ctxUserKey, "admin"))
		reqStatus.SetPathValue("name", "resproj")
		reqStatus.SetPathValue("taskId", task.ID)
		wStatus := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(wStatus, reqStatus)
		if wStatus.Code != http.StatusBadRequest || !strings.Contains(wStatus.Body.String(), "invalid status") {
			t.Fatalf("expected 400 invalid status, got %d: %s", wStatus.Code, wStatus.Body.String())
		}
	})

	// Case 9: High-risk passed item with empty evidence returns 400
	t.Run("HighRiskPassedWithoutEvidenceBlocks", func(t *testing.T) {
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
		matrixNoEvidence := `[
			{"item_id":"AC-HIGH","risk_level":"high","status":"passed","evidence":""}
		]`
		seedQASignoffRun(t, s, workspaceID, task.ID, matrixNoEvidence)
		body := `{"decision":"approve","comments":"missing evidence"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
		req.SetPathValue("name", "resproj")
		req.SetPathValue("taskId", task.ID)
		w := httptest.NewRecorder()
		s.handlePostTaskWorkflowReview(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "requires non-empty evidence") {
			t.Fatalf("expected 400 requires non-empty evidence, got %d: %s", w.Code, w.Body.String())
		}
	})
}
