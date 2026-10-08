package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// postWorkflowReviewForTest posts a workflow review decision to the review
// handler the way the console/REST callers do.
func postWorkflowReviewForTest(s *Server, project, taskID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/tasks/"+taskID+"/workflow/review", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", project)
	req.SetPathValue("taskId", taskID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w, req)
	return w
}

// The requirement-freeze gate (isRequirementReviewStep) matches by
// DECLARATION — any human_review step whose definition declares
// open_questions as an input participates in the clarify loop — so the
// preflight_review head step (greenfield v7, brownfield v2) gets the same
// fail-closed approve protection as the original greenfield
// requirement_review step. Ordinary review gates without the declaration
// must NOT trip the gate.
func TestClarifyGateMatchesPreflightReviewByDeclaration(t *testing.T) {
	preflightReview := entity.WorkflowStep{
		ID:          "preflight_review",
		Type:        "human_review",
		InputFields: []entity.WorkflowField{{Name: "open_questions"}, {Name: "preflight_report"}},
	}
	if !isRequirementReviewStep(preflightReview) {
		t.Fatal("preflight_review declaring open_questions must match the clarify gate")
	}
	requirementReview := entity.WorkflowStep{
		ID:          "requirement_review",
		Type:        "human_review",
		InputFields: []entity.WorkflowField{{Name: "open_questions"}, {Name: "requirement_draft"}},
	}
	if !isRequirementReviewStep(requirementReview) {
		t.Fatal("requirement_review declaring open_questions must still match the clarify gate")
	}
	// A human_review step without the declaration is a plain gate.
	if isRequirementReviewStep(entity.WorkflowStep{ID: "code_review", Type: "human_review", InputFields: []entity.WorkflowField{{Name: "pr"}}}) {
		t.Fatal("human_review without open_questions declaration must NOT match the clarify gate")
	}
	// An agent step declaring open_questions as an output is not a gate.
	if isRequirementReviewStep(entity.WorkflowStep{ID: "preflight", Type: "agent_task", OutputFields: []entity.WorkflowField{{Name: "open_questions"}}}) {
		t.Fatal("agent_task must NOT match the clarify gate even with an open_questions field")
	}
	// requirementReviewDeclaresOpenQuestions resolves against the run's own
	// definition (active step), keeping v5-and-older snapshots gate-exempt.
	def := entity.WorkflowDefinition{
		Steps: []entity.WorkflowStep{
			{ID: "preflight_review", Type: "human_review", InputFields: []entity.WorkflowField{{Name: "open_questions"}}},
			{ID: "code_review", Type: "human_review", InputFields: []entity.WorkflowField{{Name: "pr"}}},
		},
	}
	if !requirementReviewDeclaresOpenQuestions(def, "preflight_review") {
		t.Fatal("declaration lookup must match preflight_review")
	}
	if requirementReviewDeclaresOpenQuestions(def, "code_review") {
		t.Fatal("declaration lookup must NOT match a step without the declaration")
	}
	if requirementReviewDeclaresOpenQuestions(def, "missing_step") {
		t.Fatal("declaration lookup must fail closed for unknown steps")
	}
}

// End-to-end: a preflight_review approve with open_questions still populated
// must be rejected 409 fail-closed by the review handler — a raw REST/ChatOps
// approve cannot bypass the per-question clarify gate on the new preflight
// head.
func TestPreflightReviewApproveBlockedWhileQuestionsOpen(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := workflowstore.EnsureGreenfieldDefinition(wfStore, "en"); err != nil {
		t.Fatalf("ensure greenfield: %v", err)
	}
	def, ok, err := wfStore.Definition(workflowstore.GreenfieldDeliveryWorkflowID)
	if err != nil || !ok {
		t.Fatalf("greenfield definition missing: ok=%v err=%v", ok, err)
	}
	now := time.Now().UTC()
	task := &entity.Task{
		ID: "task-preflight-gate", Title: "Preflight gate", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "sample/pm", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		"pm-agent":      {Type: "agent", ID: "pm"},
		"product-owner": {Type: "human", ID: "admin"},
	}
	if _, _, err := wfStore.StartRunWithInput("sample", task.ID, def.ID, bindings, map[string]string{
		"request": "Build the thing",
		"context": "preflight freeze gate e2e",
	}); err != nil {
		t.Fatalf("start run: %v", err)
	}

	// The preflight agent reports 2 open questions (JSON array shape).
	if _, err := wfStore.CompleteAndAdvance("sample", task.ID, "preflighted", "", map[string]string{
		"preflight_report": `{"repo":"not initialized","env":"ready"}`,
		"open_questions":   `[{"id":"Q1","priority":"high","question":"项目尚未初始化，是否先执行项目初始化流程？"},{"id":"Q2","priority":"medium","question":"仓库缺少锁文件，允许 agent 生成并提交吗？"}]`,
	}, "completed"); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	run, found, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run for task: found=%v err=%v", found, err)
	}
	if run.ActiveStepID != "preflight_review" {
		t.Fatalf("expected active step preflight_review, got %s", run.ActiveStepID)
	}

	// Raw approve with questions still open: 409 fail-closed.
	body := `{"decision":"approve","comments":"trying to slip past the gate"}`
	w := postWorkflowReviewForTest(s, "sample", task.ID, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("approve with open questions must 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "requirement_freeze_blocked") {
		t.Fatalf("expected requirement_freeze_blocked error, got: %s", w.Body.String())
	}

	// The loop exit stays available: request_changes routes back to preflight.
	bodyRC := `{"decision":"request_changes","comments":"run the initialization first","outputs":{"clarifications":"[{\"id\":\"Q1\",\"status\":\"answered\",\"conclusion\":\"init first\"}]"}}`
	wRC := postWorkflowReviewForTest(s, "sample", task.ID, bodyRC)
	if wRC.Code != http.StatusOK {
		t.Fatalf("request_changes must pass, got %d: %s", wRC.Code, wRC.Body.String())
	}
	run, found, err = wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("re-fetch run: found=%v err=%v", found, err)
	}
	if run.ActiveStepID != "preflight" {
		t.Fatalf("expected rework loop back to preflight, got %s", run.ActiveStepID)
	}
}
