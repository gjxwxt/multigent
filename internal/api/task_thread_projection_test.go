package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/imbridge"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func TestWorkflowTaskThreadProjection_Integration(t *testing.T) {
	var postCount atomic.Int32
	var receivedPosts []map[string]any

	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" && r.Method == http.MethodPost {
			count := postCount.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedPosts = append(receivedPosts, body)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "mock-post-" + string(rune('0'+count)),
				"create_at": 1725790000000,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockMM.Close()

	s, workspaceID := newConnectionGrantPolicyServer(t)
	s.threadProjections = imbridge.NewTaskThreadProjectionService(s.controlDB, mockMM.Client())

	// Configure Mattermost connection
	connID := "conn-mm-integration"
	err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
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
		"baseUrl":  mockMM.URL,
		"botToken": "token-xyz-123",
	})
	if err != nil {
		t.Fatalf("SealConnectionSecret: %v", err)
	}
	secret.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(secret)

	// Configure agent channel binding for project "sample"
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:             "chan-sample-mira",
		WorkspaceID:    workspaceID,
		ProjectID:      "sample",
		AgentID:        "Mira",
		Provider:       "mattermost",
		ConnectionID:   connID,
		ExternalBotID:  "bot-mira",
		ExternalChatID: "chan-task-chat-1",
		Status:         "connected",
	})

	now := time.Now().UTC()
	task := &entity.Task{
		ID:        "task-flow-1",
		Title:     "Feature: User Authentication",
		Summary:   "Add login endpoints",
		Assignee:  "sample/Mira",
		Status:    entity.TaskStatusInProgress,
		CreatedBy: "Josh",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.ts.AddTask("sample", "Mira", task)

	t.Setenv("CHATOPS_CALLBACK_BASE_URL", "http://192.168.139.231:27892")

	// 1. Trigger workflow start notification
	s.notifyTaskThreadStarted(workspaceID, "sample", task, "unified-delivery-pipeline")

	// Wait briefly for asynchronous goroutine
	time.Sleep(100 * time.Millisecond)

	if postCount.Load() != 1 {
		t.Fatalf("expected 1 post for root post, got %d", postCount.Load())
	}
	if receivedPosts[0]["channel_id"] != "chan-task-chat-1" {
		t.Fatalf("expected channel_id chan-task-chat-1, got %v", receivedPosts[0]["channel_id"])
	}
	msg, _ := receivedPosts[0]["message"].(string)
	expectedURL := "http://192.168.139.231:27892/projects/sample/tasks/task-flow-1"
	if !strings.Contains(msg, expectedURL) {
		t.Fatalf("expected root post message to contain %s, got: %s", expectedURL, msg)
	}

	// 2. Verify active projection saved in DB
	activeProj, found, err := s.controlDB.ActiveTaskThreadProjection(workspaceID, task.ID, "mattermost")
	if err != nil || !found {
		t.Fatalf("ActiveTaskThreadProjection: found=%v, err=%v", found, err)
	}
	if activeProj.RootPostID != "mock-post-1" {
		t.Fatalf("expected root post id mock-post-1, got %s", activeProj.RootPostID)
	}

	// 3. Trigger step transition
	transition := workflowstore.TransitionResult{
		Run: entity.WorkflowRun{
			ID:     "run-1",
			TaskID: task.ID,
		},
		Current: entity.WorkflowStepInstance{
			StepID:  "implement",
			Status:  "completed",
			Summary: "Implemented JWT auth with unit tests",
			ActorID: "Mira",
		},
		Next: &entity.WorkflowStep{
			ID:    "review",
			Title: "Agent Self Review",
		},
		Done: false,
	}

	s.notifyTaskThreadStepTransition(workspaceID, "sample", task, transition, map[string]string{
		"mr_url": "https://gitlab.example.com/mr/1",
	})

	time.Sleep(100 * time.Millisecond)

	// S0: Routine step "implement" updates Live Card via PATCH, does NOT create noisy thread post!
	if postCount.Load() != 1 {
		t.Fatalf("expected 1 root post (silent live card update), got %d posts", postCount.Load())
	}

	// 4. Trigger completion (S1: Completion posts delivery summary)
	doneTransition := workflowstore.TransitionResult{
		Run: entity.WorkflowRun{
			ID:     "run-1",
			TaskID: task.ID,
		},
		Current: entity.WorkflowStepInstance{
			StepID:  "release",
			Status:  "completed",
			Summary: "Tagged v1.0.0 and deployed",
		},
		Done: true,
	}

	s.notifyTaskThreadStepTransition(workspaceID, "sample", task, doneTransition, nil)

	time.Sleep(100 * time.Millisecond)

	// 1 root post + 1 close task thread delivery post
	if postCount.Load() != 2 {
		t.Fatalf("expected 2 posts total (1 root post + 1 delivery completion post), got %d", postCount.Load())
	}
	if receivedPosts[1]["root_id"] != "mock-post-1" {
		t.Fatalf("expected reply root_id mock-post-1, got %v", receivedPosts[1]["root_id"])
	}

	// Verify projection is now closed in DB
	_, foundAfterClose, err := s.controlDB.ActiveTaskThreadProjection(workspaceID, task.ID, "mattermost")
	if err != nil || foundAfterClose {
		t.Fatalf("expected active projection closed, got found=%v", foundAfterClose)
	}
}
