package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/preview"
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
	s.SetConsoleOrigin("http://127.0.0.1:27891")
	s.SetPreviewOrigin("http://127.0.0.1:27892")
	s.SetPreviewCopilotDrawerEnabled(true)
	s.SetPreviewTurnReceiptsEnabled(true)
	s.SetPreviewTurnReceiptsProjects("sample")

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
	s.SetPreviewTurnReceiptsProjects("sample")

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

func TestTurnPreviewStart_SnapshotIsolationAndFlagCheck(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	s.previewEngine = preview.NewEngine()
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-turn-preview-start", repoDir)

	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-turn-preview-start/preview/turns", "admin", nil)
	store := s.receiptStore(req)

	// 1. When flag is disabled -> 403 Forbidden
	s.SetPreviewTurnReceiptsEnabled(false)
	startReqDisabled := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-turn-preview-start/preview/turns/turn-1/preview/start", "admin", nil)
	startReqDisabled.SetPathValue("name", "sample")
	startReqDisabled.SetPathValue("taskId", task.ID)
	startReqDisabled.SetPathValue("turnId", "turn-1")
	wDisabled := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnPreviewStart(wDisabled, startReqDisabled)
	if wDisabled.Code != http.StatusForbidden {
		t.Fatalf("expected 403 forbidden when turn receipts disabled, got %d: %s", wDisabled.Code, wDisabled.Body.String())
	}

	s.SetPreviewTurnReceiptsEnabled(true)

	// 2. When turn does not exist -> 404 Not Found
	startReq404 := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-turn-preview-start/preview/turns/turn-nonexistent/preview/start", "admin", nil)
	startReq404.SetPathValue("name", "sample")
	startReq404.SetPathValue("taskId", task.ID)
	startReq404.SetPathValue("turnId", "turn-nonexistent")
	w404 := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnPreviewStart(w404, startReq404)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent turn, got %d: %s", w404.Code, w404.Body.String())
	}

	// 3. Create a receipt in PENDING state -> 409 Conflict (must be CAPTURED)
	created, err := store.Create(context.Background(), previewreceipt.CreateParams{
		Project:        "sample",
		TaskID:         task.ID,
		TurnID:         "turn-pending",
		BaselineTree:   "tree-1",
		BaselineCommit: "commit-1",
		Creator:        "admin",
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	startReqPending := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-turn-preview-start/preview/turns/"+created.ID+"/preview/start", "admin", nil)
	startReqPending.SetPathValue("name", "sample")
	startReqPending.SetPathValue("taskId", task.ID)
	startReqPending.SetPathValue("turnId", created.ID)
	wPending := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnPreviewStart(wPending, startReqPending)
	if wPending.Code != http.StatusConflict {
		t.Fatalf("expected 409 for non-captured turn, got %d: %s", wPending.Code, wPending.Body.String())
	}

	// 4. Create snapshot directory and transition to CAPTURED
	projectGitRoot := gitworktree.ProjectRootForWorktree(repoDir)
	snapshotDir := previewreceipt.TurnSnapshotDir(projectGitRoot, task.ID, created.TurnID)
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(snapshotDir, "index.html"), []byte("<h1>Snapshot</h1>"), 0644)

	// Invariant check: snapshotDir must NOT equal repoDir (worktreeDir)
	if snapshotDir == repoDir {
		t.Fatalf("snapshotDir must not equal worktreeDir: %s", snapshotDir)
	}

	r1, _ := store.Transition(context.Background(), "sample", task.ID, created.ID, created.Revision, previewreceipt.StatusExecuting, nil)
	r2, _ := store.Transition(context.Background(), "sample", task.ID, r1.ID, r1.Revision, previewreceipt.StatusCapturing, nil)
	captured, _ := store.Transition(context.Background(), "sample", task.ID, r2.ID, r2.Revision, previewreceipt.StatusCaptured, nil)

	// Test turn preview start with captured receipt
	startReqCaptured := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-turn-preview-start/preview/turns/"+captured.ID+"/preview/start", "admin", nil)
	startReqCaptured.SetPathValue("name", "sample")
	startReqCaptured.SetPathValue("taskId", task.ID)
	startReqCaptured.SetPathValue("turnId", captured.ID)
	wCaptured := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnPreviewStart(wCaptured, startReqCaptured)

	// Since previewEngine is initialized on s, it should succeed (CLI/Static project type returns 200)
	if wCaptured.Code != http.StatusOK {
		t.Fatalf("expected 200 OK starting turn preview, got %d: %s", wCaptured.Code, wCaptured.Body.String())
	}

	var res map[string]any
	_ = json.Unmarshal(wCaptured.Body.Bytes(), &res)
	if res["turnId"] != captured.ID {
		t.Fatalf("expected turnId %s, got %v", captured.ID, res["turnId"])
	}
}

func TestPreviewAgentRunner_RejectsHostFallbackAndEnforcesContainer(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	runner := s.newPreviewAgentRunner(workspaceID, "sample", "pm", "http://127.0.0.1")

	// 1. Without scheduler -> fails with scheduler unavailable
	err := runner.RunAgent(context.Background(), t.TempDir(), "test prompt")
	if err == nil {
		t.Fatal("expected runner.RunAgent to fail when scheduler is nil")
	}
	if !strings.Contains(err.Error(), "agent runner scheduler unavailable") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// 2. With scheduler, but agent 'pm' has no container sandbox (SandboxNone/nil) -> fails closed!
	s.sched = &SchedulerManager{binPath: "/bin/echo"}
	err = runner.RunAgent(context.Background(), t.TempDir(), "test prompt")
	if err == nil {
		t.Fatal("expected runner.RunAgent to reject host execution / missing sandbox, but it succeeded")
	}
	if !strings.Contains(err.Error(), "requires an isolated container sandbox") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPostTaskPreviewChat_SensitiveDataRedactedInCommentAndError(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-chat-redact", repoDir)

	s.SetConsoleOrigin("http://127.0.0.1:27891")
	s.SetPreviewOrigin("http://127.0.0.1:27892")
	s.SetPreviewCopilotDrawerEnabled(true)
	s.SetPreviewTurnReceiptsEnabled(true)
	s.SetPreviewTurnReceiptsProjects("sample")

	secretToken := "sk-ant-api03-abcdef1234567890abcdef1234567890"

	// 1. Successful run with secret in prompt
	s.previewAgentRunnerFunc = func(workspaceID, project, agentName, runtimeURL string) previewreceipt.AgentRunner {
		return previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
			return os.WriteFile(filepath.Join(cloneDir, "code.txt"), []byte("fixed\n"), 0644)
		})
	}

	chatReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-chat-redact/preview/chat", "admin", previewChatBody{
		Message: "Fix bug using " + secretToken,
	})
	chatReq.SetPathValue("name", "sample")
	chatReq.SetPathValue("taskId", task.ID)
	wChat := httptest.NewRecorder()

	s.handlePostTaskPreviewChat(wChat, chatReq)
	if wChat.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", wChat.Code, wChat.Body.String())
	}

	// Verify task comment does NOT leak the secret
	comments, err := s.ts.ListComments("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	latestComment := comments[len(comments)-1].Body
	if strings.Contains(latestComment, "sk-ant-") {
		t.Fatalf("task comment leaked secret token: %s", latestComment)
	}
	if !strings.Contains(latestComment, "[REDACTED_SECRET]") {
		t.Fatalf("expected [REDACTED_SECRET] in comment, got: %s", latestComment)
	}

	// 2. Failing run with secret in error output
	s.previewAgentRunnerFunc = func(workspaceID, project, agentName, runtimeURL string) previewreceipt.AgentRunner {
		return previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
			return errors.New("fatal: auth failed with token " + secretToken)
		})
	}

	failReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-chat-redact/preview/chat", "admin", previewChatBody{
		Message: "Trigger failure",
	})
	failReq.SetPathValue("name", "sample")
	failReq.SetPathValue("taskId", task.ID)
	wFail := httptest.NewRecorder()

	s.handlePostTaskPreviewChat(wFail, failReq)
	if wFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 internal server error, got %d", wFail.Code)
	}
	failBody := wFail.Body.String()
	if strings.Contains(failBody, "sk-ant-") {
		t.Fatalf("HTTP error response leaked secret token: %s", failBody)
	}
}

func TestPreviewAgentRunner_RejectsHTTPAgent(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	worker, found, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !found {
		t.Fatalf("get worker: %v, found=%v", err, found)
	}
	worker.Model = string(entity.ModelHTTPAgent)
	worker.RuntimeConfigJSON = `{"sandbox":{"provider":"docker"}}`
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatal(err)
	}

	s.sched = &SchedulerManager{binPath: "/bin/echo"}
	runner := s.newPreviewAgentRunner(workspaceID, "sample", "pm", "http://127.0.0.1")

	err = runner.RunAgent(context.Background(), t.TempDir(), "test prompt")
	if err == nil {
		t.Fatal("expected runner.RunAgent to reject HTTP agent, but it succeeded")
	}
	if !strings.Contains(err.Error(), "does not support HTTP agents") {
		t.Fatalf("expected error mentioning HTTP agent rejection, got: %v", err)
	}
}

func TestPreviewTurn_CommitReceiptsOnReviewApproval(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-review-commit", repoDir)

	store := s.receiptStoreForWorkspace(workspaceID)
	if store == nil {
		t.Fatal("receipt store nil")
	}

	// 1. Create a CAPTURED receipt
	rec, err := store.Create(context.Background(), previewreceipt.CreateParams{
		Project:        "sample",
		TaskID:         task.ID,
		TurnID:         "turn-commit-1",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	rec, err = store.Transition(context.Background(), "sample", task.ID, rec.ID, rec.Revision, previewreceipt.StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition to executing: %v", err)
	}
	rec, err = store.Transition(context.Background(), "sample", task.ID, rec.ID, rec.Revision, previewreceipt.StatusCapturing, nil)
	if err != nil {
		t.Fatalf("transition to capturing: %v", err)
	}
	rec, err = store.Transition(context.Background(), "sample", task.ID, rec.ID, rec.Revision, previewreceipt.StatusCaptured, nil)
	if err != nil {
		t.Fatalf("transition to captured: %v", err)
	}

	// Create snapshot dir
	snapDir := previewreceipt.TurnSnapshotDir(repoDir, task.ID, rec.TurnID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(snapDir, "snapshot.txt"), []byte("snap"), 0644)

	// Make worktree dirty so commit has something to commit
	if err := os.WriteFile(filepath.Join(repoDir, "code.txt"), []byte("modified by copilot\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Run commitAndPushReviewChanges
	if _, _, err := s.commitAndPushReviewChanges("sample", "pm", task); err != nil {
		t.Fatalf("commitAndPushReviewChanges failed: %v", err)
	}

	// 3. Verify receipt is now COMMITTED
	updated, err := store.Get(context.Background(), "sample", task.ID, rec.ID)
	if err != nil {
		t.Fatalf("get updated receipt: %v", err)
	}
	if updated.Status != previewreceipt.StatusCommitted {
		t.Fatalf("expected receipt status COMMITTED, got %s", updated.Status)
	}
	if updated.CommittedSHA == "" {
		t.Fatal("expected CommittedSHA to be set")
	}

	// 4. Verify snapshot dir is removed
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot dir to be cleaned up, got: %v", err)
	}
}

func TestPreviewTurn_ApprovalRejectedWhenTurnIsMutating(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-mutating-reject", repoDir)

	store := s.receiptStoreForWorkspace(workspaceID)
	if store == nil {
		t.Fatal("receipt store nil")
	}

	// Create an EXECUTING receipt (holding active slot)
	rec, err := store.Create(context.Background(), previewreceipt.CreateParams{
		Project:        "sample",
		TaskID:         task.ID,
		TurnID:         "turn-active-mutating",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	_, err = store.Transition(context.Background(), "sample", task.ID, rec.ID, rec.Revision, previewreceipt.StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition to executing: %v", err)
	}

	// Setup review approve request
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-mutating-reject/workflow/review", "admin", map[string]any{
		"decision": "approved",
		"summary":  "LGTM",
	})
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()

	s.handlePostTaskWorkflowReview(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when approving during active turn, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "actively EXECUTING") {
		t.Fatalf("expected error mentioning active turn, got: %s", w.Body.String())
	}
}

func TestPreviewTurn_StopPreviewCancelsActiveTurn(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-stop-cancel", repoDir)

	store := s.receiptStoreForWorkspace(workspaceID)
	if store == nil {
		t.Fatal("receipt store nil")
	}

	// Create an EXECUTING receipt
	rec, err := store.Create(context.Background(), previewreceipt.CreateParams{
		Project:        "sample",
		TaskID:         task.ID,
		TurnID:         "turn-to-stop",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	_, err = store.Transition(context.Background(), "sample", task.ID, rec.ID, rec.Revision, previewreceipt.StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition to executing: %v", err)
	}

	// Call stop endpoint
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-stop-cancel/preview/stop", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()

	s.handlePostTaskPreviewStop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from stop, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Verify receipt transitioned to FAILED with "stopped by operator"
	updated, err := store.Get(context.Background(), "sample", task.ID, rec.ID)
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if updated.Status != previewreceipt.StatusFailed {
		t.Fatalf("expected receipt to be FAILED, got %s", updated.Status)
	}
	if !strings.Contains(updated.FailureReason, "stopped by operator") {
		t.Fatalf("expected failure reason 'stopped by operator', got: %s", updated.FailureReason)
	}

	// Slot must be released
	slot, holder, err := store.GetActiveSlot(context.Background(), "sample", task.ID)
	if err != nil {
		t.Fatalf("GetActiveSlot: %v", err)
	}
	if holder != nil || (slot != nil && slot.ReceiptID != "") {
		t.Fatalf("expected slot to be released, got slot=%+v, holder=%+v", slot, holder)
	}
}

func TestPreviewTurn_StaleRecoverySweepsOrphanedTurns(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-stale-restart", repoDir)

	store := s.receiptStoreForWorkspace(workspaceID)
	if store == nil {
		t.Fatal("receipt store nil")
	}

	// Create an EXECUTING receipt with snapshot
	rec, err := store.Create(context.Background(), previewreceipt.CreateParams{
		Project:        "sample",
		TaskID:         task.ID,
		TurnID:         "turn-stale-crash",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	_, err = store.Transition(context.Background(), "sample", task.ID, rec.ID, rec.Revision, previewreceipt.StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}

	snapDir := previewreceipt.TurnSnapshotDir(repoDir, task.ID, rec.TurnID)
	_ = os.MkdirAll(snapDir, 0755)

	// Run recovery
	s.recoverActiveWorkflowRunsWithDelay(0)

	// Verify receipt is FAILED
	updated, err := store.Get(context.Background(), "sample", task.ID, rec.ID)
	if err != nil {
		t.Fatalf("get updated: %v", err)
	}
	if updated.Status != previewreceipt.StatusFailed {
		t.Fatalf("expected receipt FAILED, got %s", updated.Status)
	}
	if !strings.Contains(updated.FailureReason, "server restarted") {
		t.Fatalf("expected failure reason mentioning server restart, got: %s", updated.FailureReason)
	}

	// Snapshot dir should be removed
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir should be cleaned up on recovery: %v", err)
	}
}

func TestPreviewTurn_AllowlistEnforcement(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-allowlist-test", repoDir)

	s.SetConsoleOrigin("http://127.0.0.1:27891")
	s.SetPreviewOrigin("http://127.0.0.1:27892")
	s.SetPreviewCopilotDrawerEnabled(true)
	s.SetPreviewTurnReceiptsEnabled(true)

	runnerInvoked := false
	s.previewAgentRunnerFunc = func(workspaceID, project, agentName, runtimeURL string) previewreceipt.AgentRunner {
		return previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
			runnerInvoked = true
			return os.WriteFile(filepath.Join(cloneDir, "code.txt"), []byte("allowlist ok\n"), 0644)
		})
	}

	chatPayload := previewChatBody{
		Message: "test allowlist enforcement",
	}

	// 1. When project "sample" is NOT in allowlist -> 409 feature_disabled
	s.SetPreviewTurnReceiptsProjects("other-project,unrelated")
	reqChat := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-allowlist-test/preview/chat", "admin", chatPayload)
	reqChat.SetPathValue("name", "sample")
	reqChat.SetPathValue("taskId", task.ID)
	wChat := httptest.NewRecorder()

	s.handlePostTaskPreviewChat(wChat, reqChat)
	if wChat.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict when project not in allowlist, got %d: %s", wChat.Code, wChat.Body.String())
	}
	if !strings.Contains(wChat.Body.String(), "feature_disabled") || !strings.Contains(wChat.Body.String(), "allowlist") {
		t.Fatalf("expected feature_disabled and allowlist reason, got: %s", wChat.Body.String())
	}
	if runnerInvoked {
		t.Fatal("agent runner must NOT be invoked when project is not in allowlist")
	}

	// 2. Rollback also rejected with 409 feature_disabled
	reqRollback := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-allowlist-test/preview/turns/fake-turn/rollback", "admin", nil)
	reqRollback.SetPathValue("name", "sample")
	reqRollback.SetPathValue("taskId", task.ID)
	reqRollback.SetPathValue("turnId", "fake-turn")
	wRollback := httptest.NewRecorder()

	s.handlePostTaskPreviewTurnRollback(wRollback, reqRollback)
	if wRollback.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict for rollback when project not in allowlist, got %d: %s", wRollback.Code, wRollback.Body.String())
	}
	if !strings.Contains(wRollback.Body.String(), "feature_disabled") || !strings.Contains(wRollback.Body.String(), "allowlist") {
		t.Fatalf("expected feature_disabled and allowlist reason, got: %s", wRollback.Body.String())
	}

	// 3. Status response reports turnReceiptsEnabled == false for unlisted project
	reqStatus := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/tasks/t-allowlist-test/preview/status", "admin", nil)
	reqStatus.SetPathValue("name", "sample")
	reqStatus.SetPathValue("taskId", task.ID)
	wStatus := httptest.NewRecorder()
	s.handleGetTaskPreviewStatus(wStatus, reqStatus)
	var statusData map[string]any
	if err := json.Unmarshal(wStatus.Body.Bytes(), &statusData); err != nil {
		t.Fatal(err)
	}
	if statusData["turnReceiptsEnabled"] != false {
		t.Fatalf("expected turnReceiptsEnabled=false for unlisted project, got: %v", statusData["turnReceiptsEnabled"])
	}

	// 4. Once project "sample" is added to allowlist -> chat execution proceeds to 200 OK!
	s.SetPreviewTurnReceiptsProjects("other-project, sample, unrelated")
	wChatOK := httptest.NewRecorder()
	s.handlePostTaskPreviewChat(wChatOK, reqChat)
	if wChatOK.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when project is in allowlist, got %d: %s", wChatOK.Code, wChatOK.Body.String())
	}
	if !runnerInvoked {
		t.Fatal("agent runner should be invoked when project is in allowlist")
	}
}

func TestPreviewTurn_AuditEventLogging(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	s.SetConsoleOrigin("http://127.0.0.1:27891")
	s.SetPreviewOrigin("http://127.0.0.1:27892")
	s.SetPreviewCopilotDrawerEnabled(true)
	s.SetPreviewTurnReceiptsEnabled(true)
	s.SetPreviewTurnReceiptsProjects("sample")

	repoDir := setupTestWorktreeRepo(t)
	task := setupHumanReviewTask(t, s, workspaceID, "sample", "t-audit-test", repoDir)

	secretPrompt := "SUPER_CONFIDENTIAL_USER_PROMPT_12345"
	s.previewAgentRunnerFunc = func(ws, proj, ag, localAPI string) previewreceipt.AgentRunner {
		return previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
			return os.WriteFile(filepath.Join(cloneDir, "code.txt"), []byte("audited change\n"), 0644)
		})
	}

	// 1. Execute Turn -> Should emit preview_turn.executed
	chatBody := previewChatBody{Message: secretPrompt}
	reqChat := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-audit-test/preview/chat", "admin", chatBody)
	reqChat.SetPathValue("name", "sample")
	reqChat.SetPathValue("taskId", task.ID)
	wChat := httptest.NewRecorder()
	s.handlePostTaskPreviewChat(wChat, reqChat)
	if wChat.Code != http.StatusOK {
		t.Fatalf("chat failed: %d (%s)", wChat.Code, wChat.Body.String())
	}

	var chatResp map[string]any
	_ = json.Unmarshal(wChat.Body.Bytes(), &chatResp)
	turnID, _ := chatResp["turnId"].(string)
	if turnID == "" {
		t.Fatal("expected turnId in chat response")
	}

	// 2. Rollback Turn -> Should emit preview_turn.rolled_back
	reqRollback := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-audit-test/preview/turns/"+turnID+"/rollback", "admin", nil)
	reqRollback.SetPathValue("name", "sample")
	reqRollback.SetPathValue("taskId", task.ID)
	reqRollback.SetPathValue("turnId", turnID)
	wRollback := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnRollback(wRollback, reqRollback)
	if wRollback.Code != http.StatusOK {
		t.Fatalf("rollback failed: %d (%s)", wRollback.Code, wRollback.Body.String())
	}

	// 3. Rollback again -> Should fail with conflict/not found -> emit preview_turn.rollback_rejected
	wRollback2 := httptest.NewRecorder()
	s.handlePostTaskPreviewTurnRollback(wRollback2, reqRollback)
	if wRollback2.Code == http.StatusOK {
		t.Fatal("expected 2nd rollback to fail")
	}

	// 4. Cancel active turn / stop preview -> Should emit preview_turn.cancelled
	reqStop := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/t-audit-test/preview/stop", "admin", nil)
	reqStop.SetPathValue("name", "sample")
	reqStop.SetPathValue("taskId", task.ID)
	wStop := httptest.NewRecorder()
	s.handlePostTaskPreviewStop(wStop, reqStop)
	if wStop.Code != http.StatusOK {
		t.Fatalf("stop failed: %d (%s)", wStop.Code, wStop.Body.String())
	}

	// 5. Query controlDB for audit events
	events, err := s.controlDB.ListAuditEvents(db.AuditEventFilter{WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}

	actionsFound := make(map[string]bool)
	for _, ev := range events {
		if strings.HasPrefix(ev.Action, "preview_turn.") {
			actionsFound[ev.Action] = true

			// Strict privacy invariant assertion:
			if strings.Contains(ev.Summary, secretPrompt) || strings.Contains(ev.AfterJSON, secretPrompt) {
				t.Fatalf("audit event %s leaked confidential prompt: summary=%s, after=%s", ev.Action, ev.Summary, ev.AfterJSON)
			}
			if strings.Contains(ev.AfterJSON, "SUPER_SECRET") || strings.Contains(ev.AfterJSON, "diff --git") {
				t.Fatalf("audit event %s leaked diff or patch content: %s", ev.Action, ev.AfterJSON)
			}
		}
	}

	for _, expectedAction := range []string{
		"preview_turn.executed",
		"preview_turn.rolled_back",
		"preview_turn.rollback_rejected",
		"preview_turn.cancelled",
	} {
		if !actionsFound[expectedAction] {
			t.Errorf("expected audit event %s was not emitted", expectedAction)
		}
	}
}
