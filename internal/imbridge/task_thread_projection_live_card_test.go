package imbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

func TestLiveCard_FormattingAndProgressBar(t *testing.T) {
	req := LiveCardUpdateRequest{
		WorkspaceID:    "ws-1",
		ProjectID:      "proj-test",
		TaskID:         "task-888",
		TaskTitle:      "实现双因素认证模块",
		PipelineID:     "unified-pipeline",
		Initiator:      "alex",
		CurrentStepID:  "code-review",
		CurrentStep:    "代码初审与静态检查",
		StepStatus:     "running",
		StepIndex:      4,
		TotalSteps:     8,
		Assignee:       "Lina",
		ElapsedSeconds: 154,
		QualitySummary: "✓ 单测 142/142 全部通过 | ✓ 静态代码分析 0 风险",
	}

	content := FormatLiveCardContent(req)
	if !strings.Contains(content, "### 🚀 [Task] 实现双因素认证模块") {
		t.Errorf("expected task title in live card, got: %s", content)
	}
	if !strings.Contains(content, "█████░░░░░ 50%") {
		t.Errorf("expected 50%% progress bar, got: %s", content)
	}
	if !strings.Contains(content, "代码初审与静态检查") {
		t.Errorf("expected current step, got: %s", content)
	}
	if !strings.Contains(content, "2m 34s") {
		t.Errorf("expected duration 2m 34s, got: %s", content)
	}
	if !strings.Contains(content, "单测 142/142 全部通过") {
		t.Errorf("expected quality summary, got: %s", content)
	}
}

func TestLiveCardDebouncer_ThrottlingAndForceImmediate(t *testing.T) {
	var callCount int32
	var lastStep string

	updateFn := func(ctx context.Context, req LiveCardUpdateRequest) error {
		atomic.AddInt32(&callCount, 1)
		lastStep = req.CurrentStepID
		return nil
	}

	debouncer := NewLiveCardDebouncer(50*time.Millisecond, updateFn)
	ctx := context.Background()

	// 1. Rapid sequence of non-immediate updates
	for i := 1; i <= 5; i++ {
		_ = debouncer.Schedule(ctx, LiveCardUpdateRequest{
			WorkspaceID:   "ws-1",
			TaskID:        "task-1",
			CurrentStepID: fmt.Sprintf("step-%d", i),
		})
	}

	// Should not have called updateFn immediately
	if atomic.LoadInt32(&callCount) != 0 {
		t.Errorf("expected 0 calls before debounce delay, got %d", atomic.LoadInt32(&callCount))
	}

	// Wait for debounce timer to fire
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("expected 1 debounced call, got %d", atomic.LoadInt32(&callCount))
	}
	if lastStep != "step-5" {
		t.Errorf("expected last step 'step-5', got '%s'", lastStep)
	}

	// 2. ForceImmediate should execute synchronously
	_ = debouncer.Schedule(ctx, LiveCardUpdateRequest{
		WorkspaceID:    "ws-1",
		TaskID:         "task-1",
		CurrentStepID:  "step-immediate",
		ForceImmediate: true,
	})

	if atomic.LoadInt32(&callCount) != 2 {
		t.Errorf("expected 2 calls after immediate update, got %d", atomic.LoadInt32(&callCount))
	}
	if lastStep != "step-immediate" {
		t.Errorf("expected 'step-immediate', got '%s'", lastStep)
	}
}

func TestTaskThreadProjectionService_LiveCardPatchE2E(t *testing.T) {
	var patchCalls int32
	var lastPatchedMessage string

	mmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/posts/") && strings.HasSuffix(r.URL.Path, "/patch") {
			atomic.AddInt32(&patchCalls, 1)
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if msg, ok := payload["message"].(string); ok {
				lastPatchedMessage = msg
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "post-root-1"})
			return
		}
		if r.URL.Path == "/api/v4/posts" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "post-root-1"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mmServer.Close()

	dir := t.TempDir()
	store, err := controldb.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer store.Close()

	wsID := "ws-test-live"
	projID := "proj-live"
	_ = store.UpsertWorkspace(controldb.Workspace{ID: wsID, Name: "Test WS", Slug: "test-ws", Root: dir})

	// Insert test connection and secret
	connID := "conn-mm-mock"
	_ = store.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "mock-mm",
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	})
	secret, _ := controldb.SealConnectionSecret(map[string]string{
		"baseUrl":          mmServer.URL,
		"botToken":         "test-bot-token",
		"bridgeHmacSecret": "test-secret",
	})
	secret.ConnectionID = connID
	_ = store.UpsertConnectionSecret(secret)

	_ = store.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:             "chan-bind-1",
		WorkspaceID:    wsID,
		ProjectID:      projID,
		AgentID:        "mira",
		Provider:       "mattermost",
		ConnectionID:   connID,
		ExternalChatID: "chan-mm-1",
		Status:         "connected",
	})

	svc := NewTaskThreadProjectionService(store, mmServer.Client())
	ctx := context.Background()

	// Ensure Root Post
	postID, err := svc.EnsureTaskRootPost(ctx, TaskRootPostRequest{
		WorkspaceID: wsID,
		ProjectID:   projID,
		TaskID:      "task-live-1",
		TaskTitle:   "测试实时看板",
	})
	if err != nil || postID != "post-root-1" {
		t.Fatalf("EnsureTaskRootPost failed: %v", err)
	}

	// Direct patch live card with ForceImmediate
	err = svc.patchLiveCardDirect(ctx, LiveCardUpdateRequest{
		WorkspaceID:    wsID,
		ProjectID:      projID,
		TaskID:         "task-live-1",
		TaskTitle:      "测试实时看板",
		CurrentStepID:  "step-exec",
		CurrentStep:    "正在编译代码",
		StepIndex:      3,
		TotalSteps:     6,
		ElapsedSeconds: 45,
	})
	if err != nil {
		t.Fatalf("patchLiveCardDirect failed: %v", err)
	}

	if atomic.LoadInt32(&patchCalls) != 1 {
		t.Errorf("expected 1 patch call, got %d", atomic.LoadInt32(&patchCalls))
	}
	if !strings.Contains(lastPatchedMessage, "█████░░░░░ 50%") {
		t.Errorf("expected patched message to contain progress bar, got: %s", lastPatchedMessage)
	}

	// Close task thread and verify final green stamp patch
	err = svc.CloseTaskThread(ctx, wsID, projID, "task-live-1", "交付完成", 10, "")
	if err != nil {
		t.Fatalf("CloseTaskThread failed: %v", err)
	}
	if atomic.LoadInt32(&patchCalls) != 2 {
		t.Errorf("expected 2 patch calls after close, got %d", atomic.LoadInt32(&patchCalls))
	}
	if !strings.Contains(lastPatchedMessage, "已完成 (Completed)") {
		t.Errorf("expected final message to show completed badge, got: %s", lastPatchedMessage)
	}
}
