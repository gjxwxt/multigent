package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// TestPilotGreenfieldDeliveryPipelineFullLifecycle runs a complete end-to-end
// verification of the Greenfield Delivery Pipeline (v2) with Design Gate & Pre-merge QA,
// followed by ChatOps cascade cleanup upon project deletion.
func TestPilotGreenfieldDeliveryPipelineFullLifecycle(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	// 1. Instantiate greenfield-delivery-pipeline template into a workflow definition
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "zh-CN", "Pilot Greenfield Pipeline")
	if !ok {
		t.Fatal("greenfield-delivery-pipeline template not found")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}

	// 2. Start workflow run with initial inputs
	initialInputs := map[string]string{
		"request": "Build user auth and profile module with JWT",
		"context": "Greenfield delivery pipeline pilot task",
	}
	run, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, nil, initialInputs)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.ActiveStepID != "requirement_draft" {
		t.Fatalf("expected active step requirement_draft, got %s", run.ActiveStepID)
	}
	if run.Status != "active" {
		t.Fatalf("expected run status active, got %s", run.Status)
	}

	// 3. Step 1: requirement_draft (PM agent task) completes
	draftOutputs := map[string]string{
		"requirement_draft": "PRD v1: User Authentication & Profile with SQLite & JWT",
		"open_questions":    "None",
	}
	trans1, err := wfStore.CompleteAndAdvance("resproj", task.ID, "Requirement drafted", "", draftOutputs, "completed")
	if err != nil {
		t.Fatalf("complete requirement_draft: %v", err)
	}
	if trans1.Next == nil || trans1.Next.ID != "requirement_review" {
		t.Fatalf("expected next step requirement_review, got %v", trans1.Next)
	}

	// 4. Step 2: requirement_review (Human review) approves
	reqReviewBody := `{"decision":"approve","comments":"Requirements approved by PO","outputs":{"approved_requirement":"PRD v1: User Authentication & Profile with SQLite & JWT"}}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(reqReviewBody))
	req2 = req2.WithContext(context.WithValue(req2.Context(), ctxUserKey, "admin"))
	req2.SetPathValue("name", "resproj")
	req2.SetPathValue("taskId", task.ID)
	w2 := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("requirement_review approve status=%d body=%s", w2.Code, w2.Body.String())
	}

	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found || run.ActiveStepID != "design_review" {
		t.Fatalf("expected active step design_review, got found=%v step=%s err=%v", found, run.ActiveStepID, err)
	}

	// 5. Step 3: design_review (Design Gate Human Review)
	// 5a. Approval fails closed without approved design or explicit waiver
	designBypassBody := `{"decision":"approve","comments":"Trying to bypass design gate"}`
	reqBypass := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(designBypassBody))
	reqBypass = reqBypass.WithContext(context.WithValue(reqBypass.Context(), ctxUserKey, "admin"))
	reqBypass.SetPathValue("name", "resproj")
	reqBypass.SetPathValue("taskId", task.ID)
	wBypass := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wBypass, reqBypass)
	if wBypass.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on design bypass, got %d: %s", wBypass.Code, wBypass.Body.String())
	}
	if !strings.Contains(wBypass.Body.String(), "design verification requires either an approved design reference or an explicit design_waiver_reason") {
		t.Fatalf("unexpected design bypass error message: %s", wBypass.Body.String())
	}

	// 5a-2. Approval also fails closed when supplying forged snapshot path (server cleanses path, OD fails, no waiver)
	designForgedBody := `{"decision":"approve","comments":"Trying to bypass with forged path","outputs":{"approved_design_project_id":"od-forged-proj","approved_design_snapshot_path":"snapshots/fake/manifest.json"}}`
	reqForged := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(designForgedBody))
	reqForged = reqForged.WithContext(context.WithValue(reqForged.Context(), ctxUserKey, "admin"))
	reqForged.SetPathValue("name", "resproj")
	reqForged.SetPathValue("taskId", task.ID)
	wForged := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wForged, reqForged)
	if wForged.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on forged snapshot path, got %d: %s", wForged.Code, wForged.Body.String())
	}

	// 5b. Approval succeeds with explicit audited design waiver
	designWaiverBody := `{"decision":"approve","comments":"Approved with design waiver for headless module","outputs":{"design_waiver_reason":"Headless authentication backend module with standard REST contracts","approved_design_source":"waived"}}`
	reqDesignPass := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(designWaiverBody))
	reqDesignPass = reqDesignPass.WithContext(context.WithValue(reqDesignPass.Context(), ctxUserKey, "admin"))
	reqDesignPass.SetPathValue("name", "resproj")
	reqDesignPass.SetPathValue("taskId", task.ID)
	wDesignPass := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wDesignPass, reqDesignPass)
	if wDesignPass.Code != http.StatusOK {
		t.Fatalf("expected 200 on design approval with waiver, got %d: %s", wDesignPass.Code, wDesignPass.Body.String())
	}

	run, found, err = wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found || run.ActiveStepID != "implementation" {
		t.Fatalf("expected active step implementation, got found=%v step=%s err=%v", found, run.ActiveStepID, err)
	}

	// 6. Step 4: implementation (Owner engineer agent task) completes
	implOutputs := map[string]string{
		"pr":        "feat: auth and profile backend implementation with unit tests",
		"tests_run": "18 unit tests passed in 0.4s",
		"risks":     "Low risk; isolated database migrations",
	}
	trans4, err := wfStore.CompleteAndAdvance("resproj", task.ID, "Implementation completed", "", implOutputs, "completed")
	if err != nil {
		t.Fatalf("complete implementation: %v", err)
	}
	if trans4.Next == nil || trans4.Next.ID != "self_review" {
		t.Fatalf("expected next step self_review, got %v", trans4.Next)
	}

	// 7. Step 5: self_review (Reviewer agent task) passes
	selfReviewOutputs := map[string]string{
		"self_review_verdict": "pass",
		"review_comments":     "Implementation strictly matches approved requirements; zero SQL injection or secret leak vectors.",
		"design_conformance":  "Design waived for headless backend.",
	}
	trans5, err := wfStore.CompleteAndAdvance("resproj", task.ID, "Self review passed", "", selfReviewOutputs, "completed")
	if err != nil {
		t.Fatalf("complete self_review: %v", err)
	}
	if trans5.Next == nil || trans5.Next.ID != "code_review" {
		t.Fatalf("expected next step code_review, got %v", trans5.Next)
	}

	// 8. Step 6: code_review (Human review) approves
	codeReviewBody := `{"decision":"approve","comments":"LGTM, proceed to QA","outputs":{"approved_change":"feat: auth and profile backend implementation with unit tests"}}`
	reqCodeReview := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(codeReviewBody))
	reqCodeReview = reqCodeReview.WithContext(context.WithValue(reqCodeReview.Context(), ctxUserKey, "admin"))
	reqCodeReview.SetPathValue("name", "resproj")
	reqCodeReview.SetPathValue("taskId", task.ID)
	wCodeReview := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wCodeReview, reqCodeReview)
	if wCodeReview.Code != http.StatusOK {
		t.Fatalf("code_review status=%d body=%s", wCodeReview.Code, wCodeReview.Body.String())
	}

	run, found, err = wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found || run.ActiveStepID != "qa" {
		t.Fatalf("expected active step qa, got found=%v step=%s err=%v", found, run.ActiveStepID, err)
	}

	// 9. Step 7: qa (QA agent task) completes with risk coverage matrix
	matrixJSON := `[
		{"item_id":"AC-AUTH-1","risk_level":"high","status":"passed","evidence":"Login and JWT validation tests green"},
		{"item_id":"AC-AUTH-2","risk_level":"high","status":"blocked","uncovered_reason":"External SMS OTP provider endpoint timed out in sandbox"}
	]`
	qaOutputs := map[string]string{
		"risk_coverage_matrix": matrixJSON,
		"test_report":          "QA Suite executed: 1 passed, 1 blocked on external SMS gateway",
	}
	trans7, err := wfStore.CompleteAndAdvance("resproj", task.ID, "QA evaluation complete", "", qaOutputs, "completed")
	if err != nil {
		t.Fatalf("complete qa: %v", err)
	}
	if trans7.Next == nil || trans7.Next.ID != "qa_signoff" {
		t.Fatalf("expected next step qa_signoff, got %v", trans7.Next)
	}

	// 10. Step 8: qa_signoff (Human QA Review)
	// 10a. Fails closed when high-risk blocked item has no per-item waiver
	qaNoWaiverBody := `{"decision":"approve","comments":"Approving without waiver"}`
	reqQANoWaiver := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(qaNoWaiverBody))
	reqQANoWaiver = reqQANoWaiver.WithContext(context.WithValue(reqQANoWaiver.Context(), ctxUserKey, "admin"))
	reqQANoWaiver.SetPathValue("name", "resproj")
	reqQANoWaiver.SetPathValue("taskId", task.ID)
	wQANoWaiver := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wQANoWaiver, reqQANoWaiver)
	if wQANoWaiver.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on QA signoff without waiver, got %d: %s", wQANoWaiver.Code, wQANoWaiver.Body.String())
	}
	if !strings.Contains(wQANoWaiver.Body.String(), "requires explicit manual waiver") {
		t.Fatalf("unexpected QA signoff error message: %s", wQANoWaiver.Body.String())
	}

	// 10b. Passes with explicit manual waiver for AC-AUTH-2
	qaWaiverBody := `{"decision":"approve","comments":"QA sign-off granted with manual SMS mock verification","outputs":{"manual_waivers":"{\"AC-AUTH-2\":\"SMS mock was manually verified in test harness\"}"}}`
	reqQAWaiver := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(qaWaiverBody))
	reqQAWaiver = reqQAWaiver.WithContext(context.WithValue(reqQAWaiver.Context(), ctxUserKey, "admin"))
	reqQAWaiver.SetPathValue("name", "resproj")
	reqQAWaiver.SetPathValue("taskId", task.ID)
	wQAWaiver := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wQAWaiver, reqQAWaiver)
	if wQAWaiver.Code != http.StatusOK {
		t.Fatalf("expected 200 on QA signoff with waiver, got %d: %s", wQAWaiver.Code, wQAWaiver.Body.String())
	}

	run, found, err = wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found || run.ActiveStepID != "pr_open_and_merge" {
		t.Fatalf("expected active step pr_open_and_merge, got found=%v step=%s err=%v", found, run.ActiveStepID, err)
	}

	// 11. Step 9: pr_open_and_merge (Owner engineer agent task) completes
	mergeOutputs := map[string]string{
		"merged_sha": "d3f4a6b8c0e2",
		"pr_url":     "https://git.internal/resproj/merge_requests/101",
	}
	trans9, err := wfStore.CompleteAndAdvance("resproj", task.ID, "Merge completed", "", mergeOutputs, "completed")
	if err != nil {
		t.Fatalf("complete pr_open_and_merge: %v", err)
	}
	if trans9.Next == nil || trans9.Next.ID != "release" {
		t.Fatalf("expected next step release, got %v", trans9.Next)
	}

	// 12. Step 10: release (Owner engineer agent task) completes
	releaseOutputs := map[string]string{
		"tag":              "v0.1.0",
		"deployed_version": "0.1.0-pilot",
		"health_status":    "healthy 200 OK",
	}
	trans10, err := wfStore.CompleteAndAdvance("resproj", task.ID, "First release deployed", "", releaseOutputs, "completed")
	if err != nil {
		t.Fatalf("complete release: %v", err)
	}
	if trans10.Next == nil || trans10.Next.ID != "go_live_confirm" {
		t.Fatalf("expected next step go_live_confirm, got %v", trans10.Next)
	}

	// 13. Step 11: go_live_confirm (Product owner human review) completes delivery
	goLiveBody := `{"decision":"approve","comments":"Pilot go-live verified and accepted"}`
	reqGoLive := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/workflow/review", strings.NewReader(goLiveBody))
	reqGoLive = reqGoLive.WithContext(context.WithValue(reqGoLive.Context(), ctxUserKey, "admin"))
	reqGoLive.SetPathValue("name", "resproj")
	reqGoLive.SetPathValue("taskId", task.ID)
	wGoLive := httptest.NewRecorder()
	s.handlePostTaskWorkflowReview(wGoLive, reqGoLive)
	if wGoLive.Code != http.StatusOK {
		t.Fatalf("expected 200 on go_live_confirm, got %d: %s", wGoLive.Code, wGoLive.Body.String())
	}

	// 14. Verify final workflow run and task state
	run, found, err = wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatalf("run not found: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("expected run status completed, got %s", run.Status)
	}
	finalTask, _, err := s.findTaskInProject("resproj", task.ID)
	if err != nil {
		t.Fatalf("task lookup: %v", err)
	}
	if finalTask.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("expected task status done, got %s", finalTask.Status)
	}

	// 15. Verify ChatOps Cascade Cleanup on Project Deletion
	// Seed a connection, channel link, and agent channel binding on resproj
	conn := controldb.Connection{
		ID:             "conn-pilot-1",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "Pilot MM Connection",
		Status:         "active",
		CreatedBy:      "admin",
	}
	if err := s.controlDB.UpsertConnection(conn); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	if err := s.controlDB.UpsertProjectChannelLink(controldb.ProjectChannelLink{
		ID:          "link-pilot-1",
		WorkspaceID: workspaceID,
		ProjectID:   "resproj",
		Provider:    "mattermost",
		ChannelID:   "chan-pilot-1",
		ChannelName: "pilot-channel",
	}); err != nil {
		t.Fatalf("upsert project channel link: %v", err)
	}
	if err := s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "bind-pilot-1",
		WorkspaceID:  workspaceID,
		ProjectID:    "resproj",
		AgentID:      "pilot-agent",
		ConnectionID: "conn-pilot-1",
		Provider:     "mattermost",
		Status:       "connected",
	}); err != nil {
		t.Fatalf("upsert agent channel binding: %v", err)
	}

	// Delete project resproj as admin
	delRec := httptest.NewRecorder()
	delReq := providerTestRequest(http.MethodDelete, "/api/v1/projects/resproj", "admin", nil)
	delReq.SetPathValue("name", "resproj")
	s.handleDeleteProject(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on project delete, got %d: %s", delRec.Code, delRec.Body.String())
	}

	// Confirm project is deleted
	if _, err := s.st.Project("resproj"); err == nil {
		t.Fatalf("project resproj still exists after deletion")
	}
	// Confirm project memberships are deleted
	memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
		WorkspaceID: workspaceID,
		ProjectID:   "resproj",
	})
	if err != nil || len(memberships) != 0 {
		t.Fatalf("expected 0 memberships, got %d, err=%v", len(memberships), err)
	}
	// Confirm project channel links are deleted
	links, err := s.controlDB.ListProjectChannelLinks(workspaceID, "resproj")
	if err != nil || len(links) != 0 {
		t.Fatalf("expected 0 channel links, got %d, err=%v", len(links), err)
	}
	// Confirm agent channel bindings are deleted
	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   "resproj",
	})
	if err != nil || len(bindings) != 0 {
		t.Fatalf("expected 0 agent bindings, got %d, err=%v", len(bindings), err)
	}
}
