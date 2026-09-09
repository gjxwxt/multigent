package imbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func TestHumanReviewAssigneeLineMentionsOnlyVerifiedSameInstanceUsername(t *testing.T) {
	store, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	const workspaceID = "ws-reviewer"
	const instanceID = "imi-reviewer"
	_ = store.UpsertWorkspace(controldb.Workspace{ID: workspaceID, Name: "Review", Slug: "review"})
	_ = store.UpsertUser(controldb.User{Username: "reviewer", Role: "member"})
	_ = store.UpsertIMInstance(controldb.IMInstance{ID: instanceID, WorkspaceID: workspaceID, Provider: "mattermost", DisplayName: "Review MM"})
	_ = store.UpsertConnection(controldb.Connection{ID: "conn-target", WorkspaceID: workspaceID, Provider: "mattermost", ConnectionName: "target", Status: "active", IMInstanceID: instanceID})
	_ = store.UpsertConnection(controldb.Connection{ID: "conn-source", WorkspaceID: workspaceID, Provider: "mattermost", ConnectionName: "source", Status: "active", IMInstanceID: instanceID})
	_ = store.UpsertAgentChannelBinding(controldb.AgentChannelBinding{ID: "binding-reviewer", WorkspaceID: workspaceID, ProjectID: "project", AgentID: "agent", Provider: "mattermost", ConnectionID: "conn-source", Status: "connected"})
	if err := store.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID: "identity-reviewer", WorkspaceID: workspaceID, UserID: "reviewer", ChannelBindingID: "binding-reviewer", Provider: "mattermost", ExternalUserID: "mm-reviewer",
		MetadataJSON: `{"externalUsername":"reviewer.mm"}`,
	}); err != nil {
		t.Fatalf("UpsertUserChannelIdentity: %v", err)
	}

	svc := NewTaskThreadProjectionService(store, nil)
	if got := svc.humanReviewAssigneeLine(workspaceID, "conn-target", "reviewer"); got != "指定审批人：@reviewer.mm" {
		t.Fatalf("verified same-instance identity should be mentioned, got %q", got)
	}
	if got := svc.humanReviewAssigneeLine(workspaceID, "conn-target", "unbound"); strings.Contains(got, "@unbound") {
		t.Fatalf("unbound user must not receive a fabricated Mattermost mention, got %q", got)
	}
}

func TestTaskThreadProjectionService_E2E(t *testing.T) {
	var postCount atomic.Int32
	var lastReceivedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" && r.Method == http.MethodPost {
			count := postCount.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			lastReceivedBody = body

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "mm-post-id-" + string(rune('0'+count)),
				"create_at": 1725790000000,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	// Setup SQLite store
	dir := t.TempDir()
	store, err := controldb.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer store.Close()

	wsID := "ws-test-proj"
	_ = store.UpsertWorkspace(controldb.Workspace{ID: wsID, Name: "Test WS", Slug: "test-ws", Root: dir})

	// Setup connection with baseUrl pointing to mock server
	connID := "conn-mm-mock"
	err = store.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "mock-mm",
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	})
	if err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}
	secret, err := controldb.SealConnectionSecret(map[string]string{
		"baseUrl":  server.URL,
		"botToken": "fake-token-123",
	})
	if err != nil {
		t.Fatalf("SealConnectionSecret: %v", err)
	}
	secret.ConnectionID = connID
	if err := store.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("UpsertConnectionSecret: %v", err)
	}

	// Setup binding with default channel
	_ = store.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:             "bind-1",
		WorkspaceID:    wsID,
		ProjectID:      "1test",
		AgentID:        "Mira",
		Provider:       "mattermost",
		ConnectionID:   connID,
		ExternalBotID:  "bot-mira",
		ExternalChatID: "chan-general-1",
		Status:         "connected",
	})

	svc := NewTaskThreadProjectionService(store, server.Client())

	ctx := context.Background()

	// 1. EnsureTaskRootPost creates Root Post
	rootID, err := svc.EnsureTaskRootPost(ctx, TaskRootPostRequest{
		WorkspaceID: wsID,
		ProjectID:   "1test",
		TaskID:      "task-xyz",
		TaskTitle:   "Implement User Auth",
		TaskSummary: "Refactor JWT login",
		PipelineID:  "unified-delivery-pipeline",
		CreatedBy:   "Josh",
	})
	if err != nil {
		t.Fatalf("EnsureTaskRootPost failed: %v", err)
	}
	if rootID != "mm-post-id-1" {
		t.Fatalf("expected rootID mm-post-id-1, got %s", rootID)
	}
	if postCount.Load() != 1 {
		t.Fatalf("expected 1 post to mock server, got %d", postCount.Load())
	}
	if lastReceivedBody["channel_id"] != "chan-general-1" {
		t.Fatalf("expected channel_id chan-general-1, got %v", lastReceivedBody["channel_id"])
	}

	// 2. Second call is idempotent and reuses existing root post without HTTP call
	rootID2, err := svc.EnsureTaskRootPost(ctx, TaskRootPostRequest{
		WorkspaceID: wsID,
		ProjectID:   "1test",
		TaskID:      "task-xyz",
	})
	if err != nil || rootID2 != rootID {
		t.Fatalf("EnsureTaskRootPost second call mismatch: id=%s err=%v", rootID2, err)
	}
	if postCount.Load() != 1 {
		t.Fatalf("expected postCount to remain 1, got %d", postCount.Load())
	}

	// 3. PostStepTransition posts a reply into the thread
	err = svc.PostStepTransition(ctx, StepTransitionPostRequest{
		WorkspaceID:  wsID,
		ProjectID:    "1test",
		TaskID:       "task-xyz",
		StepID:       "implement",
		StepTitle:    "Implement and Unit Test",
		StepStatus:   "completed",
		Assignee:     "Lina",
		NextAssignee: "Mira",
		Summary:      "Added JWT login endpoint with 12 unit tests",
		Outputs: map[string]string{
			"mr_url": "https://gitlab.example.com/merge_requests/1",
		},
	})
	if err != nil {
		t.Fatalf("PostStepTransition failed: %v", err)
	}
	if postCount.Load() != 2 {
		t.Fatalf("expected postCount 2, got %d", postCount.Load())
	}
	if lastReceivedBody["root_id"] != rootID {
		t.Fatalf("expected root_id %s, got %v", rootID, lastReceivedBody["root_id"])
	}

	// 4. CloseTaskThread posts completion and marks projection closed
	err = svc.CloseTaskThread(ctx, wsID, "1test", "task-xyz", "All tests passed, v1.0.0 tagged", 10, "")
	if err != nil {
		t.Fatalf("CloseTaskThread failed: %v", err)
	}
	if postCount.Load() != 3 {
		t.Fatalf("expected postCount 3, got %d", postCount.Load())
	}

	// Active projection should now be false
	_, found, err := store.ActiveTaskThreadProjection(wsID, "task-xyz", "mattermost")
	if err != nil || found {
		t.Fatalf("expected active projection closed, found=%v err=%v", found, err)
	}
}
