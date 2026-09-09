package api

import (
	"bytes"
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
	"github.com/multigent/multigent/internal/workflow"
)

func setupTestChatopsEnv(t *testing.T) (*Server, string, *httptest.Server, *atomic.Int32, *[]map[string]any, string) {
	t.Helper()
	t.Setenv("CHATOPS_CALLBACK_BASE_URL", "http://127.0.0.1:27892")
	var postCount atomic.Int32
	var receivedPosts []map[string]any

	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/users/") && r.Method == http.MethodGet {
			userID := strings.TrimPrefix(r.URL.Path, "/api/v4/users/")
			if userID == "mm-user-unknown" || userID == "mm-user-invalid" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"User not found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"` + userID + `","username":"` + userID + `"}`))
			return
		}
		if r.URL.Path == "/api/v4/actions/dialogs/open" && r.Method == http.MethodPost {
			postCount.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedPosts = append(receivedPosts, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"OK"}`))
			return
		}
		if (r.URL.Path == "/api/v4/posts" || r.Method == http.MethodPut) && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			postCount.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedPosts = append(receivedPosts, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"mock-post-id","create_at":1725790000000}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() { mockMM.Close() })

	s, workspaceID := newConnectionGrantPolicyServer(t)
	s.threadProjections = imbridge.NewTaskThreadProjectionService(s.controlDB, mockMM.Client())

	hmacSecret := "test-hmac-secret-123"
	connID := "conn-mm-chatops-test"
	err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mock-mm-chatops",
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	})
	if err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}
	secret, err := controldb.SealConnectionSecret(map[string]string{
		"baseUrl":          mockMM.URL,
		"botToken":         "token-bot-123",
		"bridgeHmacSecret": hmacSecret,
	})
	if err != nil {
		t.Fatalf("SealConnectionSecret: %v", err)
	}
	secret.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(secret)

	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:             "binding-chatops-1",
		WorkspaceID:    workspaceID,
		ProjectID:      "sample",
		AgentID:        "pm",
		Provider:       "mattermost",
		ConnectionID:   connID,
		ExternalBotID:  "bot-sample",
		ExternalChatID: "chan-chatops-1",
		Status:         "connected",
	})

	// Bind mattermost user "mm-user-admin" to platform user "admin"
	err = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "ucid-1",
		WorkspaceID:      workspaceID,
		UserID:           "admin",
		ChannelBindingID: "binding-chatops-1",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-admin",
	})
	if err != nil {
		t.Fatalf("UpsertUserChannelIdentity: %v", err)
	}

	return s, workspaceID, mockMM, &postCount, &receivedPosts, hmacSecret
}

func setupTestWorkflowTask(t *testing.T, s *Server, workspaceID string) (*entity.Task, workflow.ReviewResolutionPreview) {
	t.Helper()
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")

	now := time.Now().UTC()
	task := &entity.Task{
		ID:        "task-chatops-test-1",
		Title:     "Feature: User Identity Support",
		Summary:   "Initial requirement",
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		CreatedBy: "admin",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Create active thread projection
	_ = s.controlDB.UpsertTaskThreadProjection(controldb.TaskThreadProjection{
		ID:          "ttp-test-" + task.ID,
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
		TaskID:      task.ID,
		Provider:    "mattermost",
		ChannelID:   "chan-chatops-1",
		RootPostID:  "mock-root-post-1",
		Status:      "active",
	})

	wfStore := workflow.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "pipeline-test", Name: "Test Pipeline", Version: 1,
		Scope: "workspace", StartStepID: "requirement_review",
		Steps: []entity.WorkflowStep{
			{
				ID: "requirement_review", Type: "human_review", Title: "Requirement Review", ActorRole: "admin",
				InputFields: []entity.WorkflowField{{Name: "requirement_draft"}},
				OutputFields: []entity.WorkflowField{
					{Name: "decision"},
					{Name: "comments"},
					{Name: "approved_requirement"},
				},
			},
			{
				ID: "implement", Type: "agent_task", Title: "Implement", ActorRole: "pm",
				OutputFields: []entity.WorkflowField{{Name: "code"}},
			},
		},
		Edges: []entity.WorkflowEdge{
			{
				ID: "e1", From: "requirement_review", To: "implement", IsDefault: true,
				Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "approved"},
			},
			{
				ID: "e2", From: "requirement_review", To: "requirement_review",
				Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "rejected"},
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	// Start workflow
	_, _, err := wfStore.StartRunWithInput("sample", task.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"admin": {Type: "human", ID: "admin"},
		"pm":    {Type: "agent", ID: "pm"},
	}, map[string]string{
		"requirement_draft": "Support Mattermost interactive reviews",
	})
	if err != nil {
		t.Fatalf("StartRunWithInput: %v", err)
	}

	run, ok, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !ok {
		t.Fatalf("RunForTask: %v", err)
	}

	preview, err := wfStore.GetReviewResolutionPreview("sample", task, run.ActiveStepID)
	if err != nil {
		t.Fatalf("GetReviewResolutionPreview: %v", err)
	}

	return task, preview
}

func TestMattermostActionCallback_DirectApprove_Success(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "approve",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	t.Logf("DirectApprove resp text: %s", text)
	if text == "" {
		t.Fatalf("expected ephemeral confirmation, got %v", resp)
	}

	// Verify workflow advanced
	wfStore := workflow.NewStore(s.controlDB, workspaceID)
	run, ok, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !ok {
		t.Fatalf("RunForTask: %v", err)
	}
	if run.ActiveStepID == preview.StepID {
		t.Fatalf("expected workflow to advance past %s, but still on %s", preview.StepID, run.ActiveStepID)
	}
}

func TestMattermostActionCallback_DualCAS_StaleVersionConflict(t *testing.T) {
	s, workspaceID, _, postCount, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	// Intentionally provide a stale expected state version
	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion + 999, // Stale!
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "approve",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if text == "" || !bytes.Contains([]byte(text), []byte("409 Conflict")) {
		t.Fatalf("expected 409 Conflict error message, got %s", text)
	}
	if postCount.Load() != 1 {
		t.Fatalf("expected a fresh review card to be reissued after stale click, got %d Mattermost writes", postCount.Load())
	}

	// Verify workflow was NOT advanced
	wfStore := workflow.NewStore(s.controlDB, workspaceID)
	run, ok, _ := wfStore.RunForTask("sample", task.ID)
	if !ok || run.ActiveStepID != preview.StepID {
		t.Fatalf("workflow should not advance on stale conflict")
	}
}

func TestMattermostActionCallback_DualCAS_StaleHashConflict(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	// Intentionally provide an outdated snapshot hash
	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   "0000000000000000000000000000000000000000000000000000000000000000", // Tampered/Stale hash!
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "approve",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if text == "" || !bytes.Contains([]byte(text), []byte("409 Conflict")) {
		t.Fatalf("expected 409 Conflict error message for hash drift, got %s", text)
	}
}

func TestMattermostActionCallback_ExpiredDialogReissuesCurrentCard(t *testing.T) {
	s, workspaceID, _, postCount, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	nonce := imbridge.GenerateNonce()
	if err := s.controlDB.CreateChatopsActionSession(&controldb.ChatopsActionSession{
		ID:                   "cas-expired-dialog",
		WorkspaceID:          workspaceID,
		Project:              "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActionType:           "edit",
		ActorMMUserID:        "mm-user-admin",
		ActorPlatformUserID:  "admin",
		State:                "dialog_opened",
		ActionNonce:          nonce,
		TokenHash:            "expired-dialog-token",
		ExpiresAt:            time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateChatopsActionSession: %v", err)
	}

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "edit",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                nonce,
		ExpiresAt:            time.Now().UTC().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context:   mattermostActionContext{ActionToken: tok, Action: "edit"},
	})
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "补发当前版本审批卡片") {
		t.Fatalf("expected expired dialog recovery, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if postCount.Load() != 1 {
		t.Fatalf("expected one fresh review card post, got %d", postCount.Load())
	}
	session, found, err := s.controlDB.ChatopsActionSessionByNonce(workspaceID, nonce)
	if err != nil || !found || session.State != "expired" {
		t.Fatalf("expected old dialog session marked expired, found=%v state=%q err=%v", found, session.State, err)
	}
}

func TestMattermostActionCallback_FailedDialogReissuesCurrentCard(t *testing.T) {
	s, workspaceID, _, postCount, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	nonce := imbridge.GenerateNonce()
	if err := s.controlDB.CreateChatopsActionSession(&controldb.ChatopsActionSession{
		ID:                   "cas-failed-dialog",
		WorkspaceID:          workspaceID,
		Project:              "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActionType:           "edit",
		ActorMMUserID:        "mm-user-admin",
		ActorPlatformUserID:  "admin",
		State:                "failed",
		ActionNonce:          nonce,
		TokenHash:            "failed-dialog-token",
		ExpiresAt:            time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateChatopsActionSession: %v", err)
	}

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "edit",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                nonce,
		ExpiresAt:            time.Now().UTC().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context:   mattermostActionContext{ActionToken: tok, Action: "edit"},
	})
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "补发当前版本审批卡片") {
		t.Fatalf("expected failed dialog recovery, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if postCount.Load() != 1 {
		t.Fatalf("expected one fresh review card post, got %d", postCount.Load())
	}
	session, found, err := s.controlDB.ChatopsActionSessionByNonce(workspaceID, nonce)
	if err != nil || !found || session.State != "stale" {
		t.Fatalf("expected old failed dialog session marked stale, found=%v state=%q err=%v", found, session.State, err)
	}
}

func TestMattermostActionCallback_OpenDialog_Reject(t *testing.T) {
	s, workspaceID, _, postCount, receivedPosts, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	nonce := imbridge.GenerateNonce()
	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "reject",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                nonce,
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		TriggerID: "trig-12345",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "reject",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if postCount.Load() != 1 {
		t.Fatalf("expected 1 call to mock MM (dialog open), got %d", postCount.Load())
	}

	firstCall := (*receivedPosts)[0]
	if firstCall["trigger_id"] != "trig-12345" {
		t.Fatalf("expected trigger_id trig-12345, got %v", firstCall["trigger_id"])
	}

	// Verify action session created in database by nonce
	session, found, err := s.controlDB.ChatopsActionSessionByNonce(workspaceID, nonce)
	if err != nil || !found {
		t.Fatalf("expected action session created, err=%v, found=%v", err, found)
	}
	if session.State != "dialog_opened" {
		t.Fatalf("expected session state dialog_opened, got %s", session.State)
	}
}

func TestMattermostDialogSubmit_SuccessAndDoubleSubmitPrevention(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	sessionID := "cas-test-submit-1"
	nonce := imbridge.GenerateNonce()
	err := s.controlDB.CreateChatopsActionSession(&controldb.ChatopsActionSession{
		ID:                   sessionID,
		WorkspaceID:          workspaceID,
		Project:              "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActionType:           "approve",
		ActorMMUserID:        "mm-user-admin",
		ActorPlatformUserID:  "admin",
		State:                "dialog_opened",
		ActionNonce:          nonce,
		TokenHash:            "fake-tok-hash-1",
		ExpiresAt:            time.Now().UTC().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateChatopsActionSession: %v", err)
	}

	dialogTok, err := imbridge.SignDialogToken(hmacSecret, imbridge.DialogTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActorMMUserID:        "mm-user-admin",
		SessionID:            sessionID,
		PostID:               "mock-post-card-1",
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("SignDialogToken: %v", err)
	}

	// 1. Submit valid dialog
	submitBodyBytes, _ := json.Marshal(mattermostDialogPayload{
		Type:       "dialog_submission",
		CallbackID: "chatops-dialog-" + sessionID,
		State:      dialogTok,
		UserID:     "mm-user-admin",
		ChannelID:  "chan-chatops-1",
		Submission: map[string]string{
			"comments": "Approved via interactive dialog",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/dialog-submit", bytes.NewReader(submitBodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostDialogSubmit(rec, req)
	t.Logf("DialogSubmit response: %s", rec.Body.String())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on dialog submission, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify session marked completed
	sess, found, err := s.controlDB.ChatopsActionSessionByID(workspaceID, sessionID)
	if err != nil || !found {
		t.Fatalf("ChatopsActionSessionByID: found=%v, err=%v", found, err)
	}
	if sess.State != "completed" {
		t.Fatalf("expected session state completed, got %s", sess.State)
	}

	// 2. Resubmit the same dialog - atomic claim should prevent double execution
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/dialog-submit", bytes.NewReader(submitBodyBytes))
	s.handleMattermostDialogSubmit(rec2, req2)

	var errResp map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &errResp)
	errorsMap, _ := errResp["errors"].(map[string]any)
	if errorsMap == nil || errorsMap["comments"] == nil {
		t.Fatalf("expected atomic claim duplicate submission error, got %v", errResp)
	}
}

func TestMattermostActionCallback_ForgedUser_Rejected(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-unknown", // forged user unknown to MM
		UserName:  "attacker",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "approve",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "无法在 Mattermost 验证您的用户身份") {
		t.Fatalf("expected user verification failure message, got %q", text)
	}
}

func TestMattermostActionCallback_ChannelMismatch_Rejected(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-spoofed", // mismatched channel!
		PostID:    "mock-post-card-1",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "approve",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "安全拦截") {
		t.Fatalf("expected security interception for channel mismatch, got %q", text)
	}
}

func TestMattermostActionCallback_MissingCallbackBaseURL_Rejected(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	t.Setenv("CHATOPS_CALLBACK_BASE_URL", "") // missing!

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "reject",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		TriggerID: "trig-123",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "reject",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec := httptest.NewRecorder()

	s.handleMattermostActionCallback(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "CHATOPS_CALLBACK_BASE_URL") {
		t.Fatalf("expected fail-closed missing callback URL message, got %q", text)
	}
}

func TestMattermostActionCallback_DirectApprove_AntiReplayAndTracePersistence(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	nonce := imbridge.GenerateNonce()
	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                nonce,
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	bodyBytes, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-post-card-1",
		Context: mattermostActionContext{
			ActionToken: tok,
			Action:      "approve",
		},
	})

	// 1. First execution: succeeds and persists trace
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec1 := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first execution expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// Verify trace and outputs in action session
	session, found, err := s.controlDB.ChatopsActionSessionByNonce(workspaceID, nonce)
	if err != nil || !found {
		t.Fatalf("expected action session created by nonce, found=%v, err=%v", found, err)
	}
	if session.State != "completed" {
		t.Fatalf("expected session state completed, got %s", session.State)
	}
	if session.ResolutionTraceJSON == "" || session.ResolvedOutputsJSON == "" {
		t.Fatalf("expected non-empty trace and outputs in session, got trace=%q outputs=%q", session.ResolutionTraceJSON, session.ResolvedOutputsJSON)
	}
	var outputs map[string]string
	if err := json.Unmarshal([]byte(session.ResolvedOutputsJSON), &outputs); err != nil || (outputs["decision"] != "approved" && outputs["decision"] != "approve") {
		t.Fatalf("expected approved decision in persisted outputs, got %v", outputs)
	}

	// 2. Replay execution with identical token: must be rejected
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyBytes))
	rec2 := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec2, req2)

	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	text2, _ := resp2["text"].(string)
	if !strings.Contains(text2, "请勿重复操作") {
		t.Fatalf("expected duplicate operation rejection, got %q", text2)
	}
}

// Mattermost sends post-action callbacks as application/json with its routing
// fields at the top level and card-defined values nested under context. Keep
// this independent from the local struct helper so a drift in that wire shape
// is caught by the same parser the public endpoint uses.
func TestMattermostActionCallback_RealPostActionPayloads(t *testing.T) {
	for _, action := range []string{"approve", "edit", "reject"} {
		t.Run(action, func(t *testing.T) {
			s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
			task, preview := setupTestWorkflowTask(t, s, workspaceID)
			nonce := imbridge.GenerateNonce()
			token, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
				WorkspaceID:          workspaceID,
				ProjectID:            "sample",
				TaskID:               task.ID,
				StepID:               preview.StepID,
				Action:               action,
				ChannelID:            "chan-chatops-1",
				ConnectionID:         "conn-mm-chatops-test",
				ExpectedStateVersion: preview.ExpectedStateVersion,
				ReviewSnapshotHash:   preview.ReviewSnapshotHash,
				Nonce:                nonce,
				ExpiresAt:            time.Now().UTC().Add(time.Hour).Unix(),
			})
			if err != nil {
				t.Fatalf("SignActionToken: %v", err)
			}
			body, err := json.Marshal(map[string]any{
				"user_id":    "mm-user-admin",
				"channel_id": "chan-chatops-1",
				"post_id":    "mock-post-card-1",
				"team_id":    "mm-team-1",
				"type":       "button",
				"context": map[string]any{
					"action_token": token,
					"action":       action,
				},
			})
			if err != nil {
				t.Fatalf("marshal callback: %v", err)
			}
			if action != "approve" {
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("unmarshal callback: %v", err)
				}
				payload["trigger_id"] = "trigger-real-post-action"
				body, _ = json.Marshal(payload)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.handleMattermostActionCallback(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			session, found, err := s.controlDB.ChatopsActionSessionByNonce(workspaceID, nonce)
			if err != nil || !found {
				t.Fatalf("expected session from real payload, found=%v err=%v", found, err)
			}
			if action == "approve" {
				if session.State != "completed" {
					t.Fatalf("approve session state = %q, want completed", session.State)
				}
				var response map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &response)
				if response["ephemeral_text"] == "" || response["update"] == nil {
					t.Fatalf("approve must use Mattermost post-action update response, got %v", response)
				}
			} else if session.State != "dialog_opened" {
				t.Fatalf("%s session state = %q, want dialog_opened", action, session.State)
			}
		})
	}
}

func TestMattermostActionCallback_RejectsContextActionMismatchBeforeSession(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)
	nonce := imbridge.GenerateNonce()
	token, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID: workspaceID, ProjectID: "sample", TaskID: task.ID, StepID: preview.StepID,
		Action: "approve", ChannelID: "chan-chatops-1", ConnectionID: "conn-mm-chatops-test",
		ExpectedStateVersion: preview.ExpectedStateVersion, ReviewSnapshotHash: preview.ReviewSnapshotHash,
		Nonce: nonce, ExpiresAt: time.Now().UTC().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"user_id": "mm-user-admin", "channel_id": "chan-chatops-1", "post_id": "mock-post-card-1",
		"context": map[string]any{"action_token": token, "action": "reject"},
	})
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body)))
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if response["error"] == nil {
		t.Fatalf("expected Mattermost action error response, got %v", response)
	}
	if _, found, err := s.controlDB.ChatopsActionSessionByNonce(workspaceID, nonce); err != nil || found {
		t.Fatalf("mismatched action must not create a session, found=%v err=%v", found, err)
	}
}
