package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/previewreceipt"
	"github.com/multigent/multigent/internal/secretbox"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func setupTestWorktreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, string(out))
		}
	}

	run("init")
	run("config", "user.name", "Tester")
	run("config", "user.email", "tester@example.com")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("initial code\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md", "code.txt")
	run("commit", "-m", "init")

	return dir
}

func setupHumanReviewTask(t *testing.T, s *Server, workspaceID, project, taskID, worktreeDir string) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	task := &entity.Task{
		ID:          taskID,
		Status:      entity.TaskStatusInProgress,
		WorktreeDir: worktreeDir,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.ts.AddTask(project, "pm", task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID:          "wf-" + taskID,
		Name:        "HR Definition",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "review",
		Steps: []entity.WorkflowStep{
			{ID: "review", Type: "human_review", Title: "Human Review Step"},
			{ID: "dev", Type: "agent_task", Title: "Dev Step"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	if _, _, err := wfStore.StartRun(project, task.ID, def.ID, nil); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return task
}

func TestGetTaskPreviewTurnsAndDiff_AuthAndPrivacy(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-turn-auth", repoDir)

	// Create a receipt directly in store
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-turn-auth/preview/turns", "admin", nil)
	store := s.receiptStore(req)
	created, err := store.Create(context.Background(), previewreceipt.CreateParams{
		Project:          "sample",
		TaskID:           task.ID,
		TurnID:           "turn-test-1",
		BaselineTree:     "dummy-tree",
		BaselineCommit:   "dummy-commit",
		BaselineRef:      "refs/mg-turns/turn-test-1",
		Creator:          "admin",
		DisplayDiff:      "--- a/code.txt\n+++ b/code.txt\n@@ -1 +1 @@\n-initial code\n+updated code\n",
		OperationalPatch: "SUPER_SECRET_OPERATIONAL_PATCH_CONTENT_THAT_MUST_NEVER_LEAK",
		LeaseDuration:    10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	r1, err := store.Transition(context.Background(), "sample", task.ID, created.ID, created.Revision, previewreceipt.StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition to executing: %v", err)
	}
	r2, err := store.Transition(context.Background(), "sample", task.ID, r1.ID, r1.Revision, previewreceipt.StatusCapturing, nil)
	if err != nil {
		t.Fatalf("transition to capturing: %v", err)
	}
	_, err = store.Transition(context.Background(), "sample", task.ID, r2.ID, r2.Revision, previewreceipt.StatusCaptured, func(r *previewreceipt.PreviewReceipt) error {
		r.TouchedPaths = []string{"code.txt"}
		return nil
	})
	if err != nil {
		t.Fatalf("transition to captured: %v", err)
	}

	// 1. Unauthorized request
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-turn-auth/preview/turns", nil)
	unauthReq.SetPathValue("name", "sample")
	unauthReq.SetPathValue("taskId", task.ID)
	unauthW := httptest.NewRecorder()
	s.withTokenAuth(http.HandlerFunc(s.handleGetTaskPreviewTurns)).ServeHTTP(unauthW, unauthReq)
	if unauthW.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized, got %d: %s", unauthW.Code, unauthW.Body.String())
	}

	// 2. Authorized request
	authReq := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-turn-auth/preview/turns", "admin", nil)
	authReq.SetPathValue("name", "sample")
	authReq.SetPathValue("taskId", task.ID)
	authW := httptest.NewRecorder()
	s.handleGetTaskPreviewTurns(authW, authReq)
	if authW.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", authW.Code, authW.Body.String())
	}

	rawBody := authW.Body.String()
	// CRITICAL INVARIANT: OperationalPatch must never leak in JSON output!
	if strings.Contains(rawBody, "SUPER_SECRET_OPERATIONAL_PATCH") {
		t.Fatalf("CRITICAL SECURITY LEAK: OperationalPatch was serialized in turns response: %s", rawBody)
	}
	if strings.Contains(rawBody, "operationalPatch") {
		t.Fatalf("operationalPatch field key present in response JSON: %s", rawBody)
	}

	var turnsRes struct {
		OK       bool                          `json:"ok"`
		Receipts []*previewreceipt.PreviewReceipt `json:"receipts"`
	}
	if err := json.Unmarshal([]byte(rawBody), &turnsRes); err != nil {
		t.Fatalf("unmarshal turns response: %v", err)
	}
	if len(turnsRes.Receipts) != 1 || turnsRes.Receipts[0].TurnID != "turn-test-1" {
		t.Fatalf("expected 1 receipt with TurnID turn-test-1, got: %+v", turnsRes.Receipts)
	}
	if turnsRes.Receipts[0].OperationalPatch != "" {
		t.Fatalf("expected empty OperationalPatch on receipt struct, got %s", turnsRes.Receipts[0].OperationalPatch)
	}

	// 3. Get diff endpoint
	diffReq := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-turn-auth/preview/turns/turn-test-1/diff", "admin", nil)
	diffReq.SetPathValue("name", "sample")
	diffReq.SetPathValue("taskId", task.ID)
	diffReq.SetPathValue("turnId", "turn-test-1")
	diffW := httptest.NewRecorder()
	s.handleGetTaskPreviewTurnDiff(diffW, diffReq)
	if diffW.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for diff, got %d: %s", diffW.Code, diffW.Body.String())
	}
	var diffRes struct {
		OK           bool     `json:"ok"`
		TurnID       string   `json:"turnId"`
		Status       string   `json:"status"`
		DisplayDiff  string   `json:"displayDiff"`
		TouchedPaths []string `json:"touchedPaths"`
	}
	if err := json.Unmarshal(diffW.Body.Bytes(), &diffRes); err != nil {
		t.Fatalf("unmarshal diff: %v", err)
	}
	if diffRes.TurnID != "turn-test-1" || !strings.Contains(diffRes.DisplayDiff, "updated code") {
		t.Fatalf("unexpected diff response: %+v", diffRes)
	}

	// 4. Nonexistent turn
	notfoundReq := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-turn-auth/preview/turns/turn-missing/diff", "admin", nil)
	notfoundReq.SetPathValue("name", "sample")
	notfoundReq.SetPathValue("taskId", task.ID)
	notfoundReq.SetPathValue("turnId", "turn-missing")
	notfoundW := httptest.NewRecorder()
	s.handleGetTaskPreviewTurnDiff(notfoundW, notfoundReq)
	if notfoundW.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing turn, got %d: %s", notfoundW.Code, notfoundW.Body.String())
	}
}

func TestPostTaskPreviewTurnRollback_Lifecycle(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-rollback", repoDir)

	// Execute turn via TurnEngine to have a legitimate CAPTURED turn with postimages
	engine := s.turnEngine(providerTestRequest(http.MethodPost, "/", "admin", nil))
	receipt, err := engine.ExecuteTurn(context.Background(), previewreceipt.ExecuteTurnParams{
		WorkspaceID:    workspaceID,
		Project:        "sample",
		ProjectGitRoot: repoDir,
		TaskID:         task.ID,
		WorktreeDir:    repoDir,
		Prompt:         "change code",
		Actor:          "admin",
		Runner: previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
			return os.WriteFile(filepath.Join(cloneDir, "code.txt"), []byte("modified by copilot\n"), 0644)
		}),
		LeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}
	if receipt.Status != previewreceipt.StatusCaptured {
		t.Fatalf("expected CAPTURED status, got %s", receipt.Status)
	}

	// Verify file was updated in worktree
	content, _ := os.ReadFile(filepath.Join(repoDir, "code.txt"))
	if string(content) != "modified by copilot\n" {
		t.Fatalf("expected modified code in worktree, got: %s", string(content))
	}

	// 1. Rollback when task is not at human_review step -> blocked (409)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	_, _ = wfStore.CompleteAndAdvance("sample", task.ID, "ok", "", map[string]string{"decision": "approve"}, "completed")
	// Now step is 'dev' (agent_task)

	rbReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-rollback/preview/turns/"+receipt.ID+"/rollback", "admin", nil)
	rbReq.SetPathValue("name", "sample")
	rbReq.SetPathValue("taskId", task.ID)
	rbReq.SetPathValue("turnId", receipt.ID)
	rbW := httptest.NewRecorder()

	s.handlePostTaskPreviewTurnRollback(rbW, rbReq)
	if rbW.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict when not at human_review step, got %d: %s", rbW.Code, rbW.Body.String())
	}

	// Reset workflow step to human_review
	_ = wfStore.SaveRun(&entity.WorkflowRun{
		ID:           "run-rollback",
		Project:      "sample",
		TaskID:       task.ID,
		DefinitionID: "wf-t-rollback",
		Status:       "active",
		ActiveStepID: "review",
		StartedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	})

	// 2. Perform surgical rollback -> success!
	rbWSuccess := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnRollback(rbWSuccess, rbReq)
	if rbWSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for rollback, got %d: %s", rbWSuccess.Code, rbWSuccess.Body.String())
	}

	var rbRes map[string]any
	if err := json.Unmarshal(rbWSuccess.Body.Bytes(), &rbRes); err != nil {
		t.Fatalf("unmarshal rollback response: %v", err)
	}
	if rbRes["status"] != previewreceipt.StatusRolledBack {
		t.Fatalf("expected status ROLLED_BACK, got: %v", rbRes["status"])
	}

	// Verify code was restored in worktree
	restoredContent, _ := os.ReadFile(filepath.Join(repoDir, "code.txt"))
	if string(restoredContent) != "initial code\n" {
		t.Fatalf("expected code.txt restored to initial, got: %s", string(restoredContent))
	}
}

func TestPostTaskPreviewChat_ExecutionAndCapture(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-chat-exec", repoDir)

	chatPayload := previewChatBody{
		Message: "please update code.txt to say agent was here",
	}
	// 1. Disabled feature flag returns 409 feature_disabled
	s.SetPreviewTurnReceiptsEnabled(false)
	reqDisabled := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-chat-exec/preview/chat", "admin", chatPayload)
	reqDisabled.SetPathValue("name", "sample")
	reqDisabled.SetPathValue("taskId", task.ID)
	wDisabled := httptest.NewRecorder()

	s.handlePostTaskPreviewChat(wDisabled, reqDisabled)
	if wDisabled.Code != http.StatusConflict {
		t.Fatalf("expected 409 for disabled feature, got %d: %s", wDisabled.Code, wDisabled.Body.String())
	}
	if !strings.Contains(wDisabled.Body.String(), "feature_disabled") {
		t.Fatalf("expected feature_disabled error code, got: %s", wDisabled.Body.String())
	}

	// 2. Enable turn receipts and mock the runner
	s.SetConsoleOrigin("http://127.0.0.1:27891")
	s.SetPreviewOrigin("http://127.0.0.1:27892")
	s.SetPreviewCopilotDrawerEnabled(true)
	s.SetPreviewTurnReceiptsEnabled(true)

	s.previewAgentRunnerFunc = func(workspaceID, project, agentName, runtimeURL string) previewreceipt.AgentRunner {
		return previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
			// Modify code.txt in clone
			return os.WriteFile(filepath.Join(cloneDir, "code.txt"), []byte("agent was here\n"), 0644)
		})
	}

	// Execute chat
	reqEnabled := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-chat-exec/preview/chat", "admin", chatPayload)
	reqEnabled.SetPathValue("name", "sample")
	reqEnabled.SetPathValue("taskId", task.ID)
	wEnabled := httptest.NewRecorder()

	s.handlePostTaskPreviewChat(wEnabled, reqEnabled)
	if wEnabled.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from chat turn execution, got %d: %s", wEnabled.Code, wEnabled.Body.String())
	}

	rawChatBody := wEnabled.Body.String()
	// Assert OperationalPatch is never leaked
	if strings.Contains(rawChatBody, "operationalPatch") || strings.Contains(rawChatBody, "StoredPatch") {
		t.Fatalf("OperationalPatch field leaked in chat response: %s", rawChatBody)
	}

	var chatRes struct {
		OK           bool     `json:"ok"`
		TurnID       string   `json:"turnId"`
		Status       string   `json:"status"`
		DisplayDiff  string   `json:"displayDiff"`
		TouchedPaths []string `json:"touchedPaths"`
	}
	if err := json.Unmarshal([]byte(rawChatBody), &chatRes); err != nil {
		t.Fatalf("unmarshal chat response: %v", err)
	}
	if !chatRes.OK || chatRes.Status != previewreceipt.StatusCaptured {
		t.Fatalf("unexpected chat turn status: %+v", chatRes)
	}
	if !strings.Contains(chatRes.DisplayDiff, "agent was here") {
		t.Fatalf("expected diff to contain agent change, got: %s", chatRes.DisplayDiff)
	}

	// Verify main worktree was actually updated
	worktreeContent, err := os.ReadFile(filepath.Join(repoDir, "code.txt"))
	if err != nil {
		t.Fatalf("read worktree file: %v", err)
	}
	if string(worktreeContent) != "agent was here\n" {
		t.Fatalf("expected worktree file updated to 'agent was here\n', got: %s", string(worktreeContent))
	}

	// Verify comment was added to task
	comments, err := s.ts.ListComments("sample", "pm", task.ID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Body, "[Preview Copilot Turn") {
		t.Fatalf("expected audit comment added to task, got comments: %+v", comments)
	}
}
