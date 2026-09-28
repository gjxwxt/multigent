package api

// C1 closeout: the qa_signoff risk-coverage matrix checkpoint must be
// effective at ALL completion entries - console review, the IM trigger
// callback, and the runtime step report. The shared choke point is
// enforceQASignoffMatrixGate. These tests prove the two previously unguarded
// entries reject a matrix-less approval BEFORE any write and advance with a
// valid matrix. The console entry rejection shape is pinned by
// TestC1ConsoleQASignoffMatrixGateStillEnforced.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func c1MatrixJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal([]map[string]string{
		{"item_id": "AC-1", "risk_level": "high", "status": "passed", "evidence": "TestThing green"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// seedQASignoffRun fast-forwards a greenfield pipeline run to the qa_signoff
// step, mirroring the pilot greenfield test flow through the store: every
// intermediate step is an agent step; the human qa_signoff step is the entry
// under test. The qa step matrix lands as the signoff step input via the
// e-qa edge mapping, so an approving entry with no matrix of its own still
// has the input-side fallback. Rejection tests strip that input matrix.
func c1SeedQASignoffRun(t *testing.T, withInputMatrix bool) (*Server, string, *entity.Task, *workflowstore.Store, entity.WorkflowRun) {
	t.Helper()
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	matrixJSON := ""
	if withInputMatrix {
		matrixJSON = c1MatrixJSON(t)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "c1 entry matrix test")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	run, found, err := wfStore.RunForTask("resproj", task.ID)
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
		if instances[i].StepID != "qa_signoff" {
			continue
		}
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
	return s, workspaceID, task, wfStore, run
}

func c1StepInstance(t *testing.T, wfStore *workflowstore.Store, runID, stepID string) (entity.WorkflowStepInstance, bool) {
	t.Helper()
	instances, err := wfStore.ListStepInstances(runID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range instances {
		if instances[i].StepID == stepID {
			return instances[i], true
		}
	}
	return entity.WorkflowStepInstance{}, false
}

func c1ApproveBody(t *testing.T, withMatrix bool) string {
	t.Helper()
	body := map[string]any{
		"decision": "approve",
		"comments": "entry gate check",
	}
	if withMatrix {
		body["outputs"] = map[string]string{"risk_coverage_matrix": c1MatrixJSON(t)}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// runParkedOnQASignoff asserts the zero-write rejection contract.
func runParkedOnQASignoff(t *testing.T, wfStore *workflowstore.Store, project, taskID string) {
	t.Helper()
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if run.Status != "active" || run.ActiveStepID != "qa_signoff" {
		t.Fatalf("rejected approval must leave the run parked on qa_signoff, got %s@%s", run.Status, run.ActiveStepID)
	}
}

func c1NotificationRecord(t *testing.T, s *Server, workspaceID, id, token string, task *entity.Task, runID, definitionID string) workflowNotificationRecord {
	t.Helper()
	record := workflowNotificationRecord{
		ID:                id,
		WorkspaceID:       workspaceID,
		Project:           "resproj",
		TaskID:            task.ID,
		TaskTitle:         task.Title,
		WorkflowRunID:     runID,
		WorkflowID:        definitionID,
		StepID:            "qa_signoff",
		StepTitle:         "QA Sign-off",
		RecipientUserID:   "admin",
		Provider:          "feishu",
		Status:            "sent",
		CallbackTokenHash: hashWorkflowCallbackToken(token),
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	if err := s.saveWorkflowNotification(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func postC1TriggerCallback(t *testing.T, s *Server, workspaceID, recordID, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/workflow/triggers/"+recordID+"/callback?token="+token, strings.NewReader(body))
	req.SetPathValue("workspaceId", workspaceID)
	req.SetPathValue("notificationId", recordID)
	rec := httptest.NewRecorder()
	s.handlePostWorkflowTriggerCallback(rec, req)
	return rec
}

// TestC1TriggerCallbackQASignoffMatrixGate: the IM trigger callback entry.
func TestC1TriggerCallbackQASignoffMatrixGate(t *testing.T) {
	t.Run("rejects matrixless approval before any write", func(t *testing.T) {
		s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, false)

		record := c1NotificationRecord(t, s, workspaceID, "wn-c1-reject", "c1-callback-token", task, run.ID, run.DefinitionID)
		rec := postC1TriggerCallback(t, s, workspaceID, record.ID, "c1-callback-token", c1ApproveBody(t, false))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("trigger callback must reject a matrixless qa_signoff approval with 400, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "risk_coverage_matrix") {
			t.Fatalf("rejection must name the missing matrix: %s", rec.Body.String())
		}
		runParkedOnQASignoff(t, wfStore, "resproj", task.ID)
		stored, ok, err := s.workflowNotification(workspaceID, record.ID)
		if err != nil || !ok {
			t.Fatalf("notification lookup: ok=%v err=%v", ok, err)
		}
		if stored.Status == "acted" {
			t.Fatal("rejected callback must not mark the notification acted")
		}
	})

	t.Run("advances with a valid matrix", func(t *testing.T) {
		s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, false)

		record := c1NotificationRecord(t, s, workspaceID, "wn-c1-approve", "c1-callback-token-ok", task, run.ID, run.DefinitionID)
		rec := postC1TriggerCallback(t, s, workspaceID, record.ID, "c1-callback-token-ok", c1ApproveBody(t, true))
		if rec.Code != http.StatusOK {
			t.Fatalf("trigger callback with a valid matrix must advance, got %d: %s", rec.Code, rec.Body.String())
		}
		after, found, err := wfStore.RunForTask("resproj", task.ID)
		if err != nil || !found {
			t.Fatalf("run lookup after approval: found=%v err=%v", found, err)
		}
		if after.ActiveStepID == "qa_signoff" {
			t.Fatalf("valid matrix must advance the run past qa_signoff, still at %s", after.ActiveStepID)
		}
	})
}

func postC1RuntimeStepComplete(t *testing.T, s *Server, workspaceID, project, taskID, agent string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+taskID+"/workflow/step/complete", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      project,
		Agent:        agent,
		Capabilities: []string{"task.use"},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeWorkflowStepComplete(rec, req)
	return rec
}

// TestC1RuntimeStepReportQASignoffMatrixGate: the runtime step report entry.
func TestC1RuntimeStepReportQASignoffMatrixGate(t *testing.T) {
	t.Run("rejects matrixless approval before any write", func(t *testing.T) {
		s, workspaceID, task, wfStore, _ := c1SeedQASignoffRun(t, false)

		rec := postC1RuntimeStepComplete(t, s, workspaceID, "resproj", task.ID, "agent", map[string]any{
			"summary": "approving without a matrix",
			"status":  "completed",
			"outputs": map[string]string{"decision": "approve", "comments": "trust me"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("runtime step report must reject a matrixless qa_signoff approval with 400, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "risk_coverage_matrix") {
			t.Fatalf("rejection must name the missing matrix: %s", rec.Body.String())
		}
		runParkedOnQASignoff(t, wfStore, "resproj", task.ID)
	})

	t.Run("advances with a valid matrix", func(t *testing.T) {
		s, workspaceID, task, wfStore, _ := c1SeedQASignoffRun(t, false)

		rec := postC1RuntimeStepComplete(t, s, workspaceID, "resproj", task.ID, "agent", map[string]any{
			"summary": "approving with full matrix",
			"status":  "completed",
			"outputs": map[string]string{
				"decision":             "approve",
				"comments":             "matrix covered",
				"risk_coverage_matrix": c1MatrixJSON(t),
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("runtime step report with a valid matrix must advance, got %d: %s", rec.Code, rec.Body.String())
		}
		after, found, err := wfStore.RunForTask("resproj", task.ID)
		if err != nil || !found {
			t.Fatalf("run lookup after approval: found=%v err=%v", found, err)
		}
		if after.ActiveStepID == "qa_signoff" {
			t.Fatalf("valid matrix must advance the run past qa_signoff, still at %s", after.ActiveStepID)
		}
	})
}

// TestC1ConsoleQASignoffMatrixGateStillEnforced pins the console entry to the
// shared choke point: a matrixless approval via the review endpoint still
// rejects with the same message shape.
func TestC1ConsoleQASignoffMatrixGateStillEnforced(t *testing.T) {
	s, _, task, wfStore, _ := c1SeedQASignoffRun(t, false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(c1ApproveBody(t, false)))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("console review must reject a matrixless qa_signoff approval with 400, got %d: %s", rec.Code, rec.Body.String())
	}
	runParkedOnQASignoff(t, wfStore, "resproj", task.ID)
}
