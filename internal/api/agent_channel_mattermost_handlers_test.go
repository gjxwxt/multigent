package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

func setupMattermostTestBinding(t *testing.T, s *Server, workspaceID, project, agent, commandToken string) (controldb.AgentChannelBinding, string) {
	t.Helper()
	connID := "conn-mm-" + project + "-" + agent
	chanID := "chan-mm-" + project + "-" + agent
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "agent-" + project + "-" + agent,
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	}); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{
		"baseUrl":      "http://127.0.0.1:8065",
		"botToken":     "test-bot-token",
		"commandToken": commandToken,
	})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}
	binding := controldb.AgentChannelBinding{
		ID:           chanID,
		WorkspaceID:  workspaceID,
		ProjectID:    project,
		AgentID:      agent,
		Provider:     "mattermost",
		ConnectionID: connID,
		Status:       "connected",
		MetadataJSON: `{"appId":"bot-user-id"}`,
	}
	if err := s.controlDB.UpsertAgentChannelBinding(binding); err != nil {
		t.Fatalf("binding: %v", err)
	}
	return binding, connID
}

func TestMattermostSlashBind_MethodNotAllowed(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/im/mattermost/commands/bind", nil)
	rr := httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestMattermostSlashBind_InvalidOrExpiredCode(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	binding, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "pm", "valid-cmd-token")

	// 1. Non-existent code
	form := url.Values{
		"command": {"/bind"},
		"text":    {"MG-NONEXISTENT"},
		"token":   {"valid-cmd-token"},
		"user_id": {"mm-usr-1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 ephemeral, got %d", rr.Code)
	}
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["response_type"] != "ephemeral" || !strings.Contains(resp["text"].(string), "无效") {
		t.Fatalf("unexpected response: %#v", resp)
	}

	// 2. Expired code
	now := time.Now().UTC()
	expiredAt := now.Add(-10 * time.Minute).Format(time.RFC3339)
	if err := s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             "MG-EXPIRED",
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "owner",
		TargetType:       "user",
		ExpiresAt:        expiredAt,
		CreatedAt:        now.Add(-20 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("create bind code err: %v", err)
	}

	form.Set("text", "MG-EXPIRED")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if !strings.Contains(resp["text"].(string), "过期") {
		t.Fatalf("expected expired error, got %#v", resp)
	}

	// 3. Already used code
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             "MG-USED",
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "owner",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		UsedAt:           now.Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})
	form.Set("text", "MG-USED")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if !strings.Contains(resp["text"].(string), "已使用") {
		t.Fatalf("expected already used error, got %#v", resp)
	}
}

func TestMattermostSlashBind_CommandTokenUnauthorized(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	binding, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "pm", "secret-token-123")

	now := time.Now().UTC()
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             "MG-TOKEN-TEST",
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "owner",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})

	form := url.Values{
		"command": {"/bind"},
		"text":    {"MG-TOKEN-TEST"},
		"token":   {"wrong-token"},
		"user_id": {"mm-usr-1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	// Security invariant: token mismatch MUST be HTTP 401
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rr.Code)
	}
}

func TestMattermostSlashBind_TargetMismatch(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	binding, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "pm", "tok-123")

	now := time.Now().UTC()
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             "MG-CHAT-CODE",
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "owner",
		TargetType:       "chat",
		TargetName:       "dev-room",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})

	form := url.Values{
		"command": {"/bind"},
		"text":    {"MG-CHAT-CODE"},
		"token":   {"tok-123"},
		"user_id": {"mm-usr-1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if !strings.Contains(resp["text"].(string), "群聊绑定码") {
		t.Fatalf("expected target mismatch message, got %#v", resp)
	}
}

func TestMattermostSlashBind_Success_UserAndB4Routing(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	// In Mattermost, both agents share the same /bind command with the same token!
	sharedCommandToken := "shared-mattermost-slash-token"
	bindingA, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "lina", sharedCommandToken)
	bindingB, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "mira", sharedCommandToken)

	now := time.Now().UTC()
	codeA := "MG-LINA-1111"
	codeB := "MG-MIRA-2222"

	_ = s.users.CreateUser("user-a", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "user-a", WorkspaceRoleMember)
	_ = s.users.CreateUser("user-b", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "user-b", WorkspaceRoleMember)

	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeA,
		WorkspaceID:      workspaceID,
		ChannelBindingID: bindingA.ID,
		UserID:           "user-a",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeB,
		WorkspaceID:      workspaceID,
		ChannelBindingID: bindingB.ID,
		UserID:           "user-b",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})

	// 1. Bind to Lina using codeA
	formA := url.Values{
		"command":      {"/bind"},
		"text":         {codeA},
		"token":        {sharedCommandToken},
		"user_id":      {"mm-user-alpha"},
		"user_name":    {"alpha"},
		"channel_id":   {"ch-direct-lina"},
		"channel_name": {"alpha__lina_bot"},
	}
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formA.Encode()))
	reqA.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rrA := httptest.NewRecorder()
	s.handleMattermostSlashBind(rrA, reqA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rrA.Code)
	}
	var respA map[string]any
	_ = json.NewDecoder(rrA.Body).Decode(&respA)
	if respA["response_type"] != "ephemeral" || !strings.Contains(respA["text"].(string), "绑定成功") {
		t.Fatalf("expected ephemeral success, got %#v", respA)
	}

	// Verify DB state for Agent A
	identitiesA, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: bindingA.ID,
		UserID:           "user-a",
	})
	if err != nil || len(identitiesA) != 1 {
		t.Fatalf("expected 1 user channel identity for Lina, got %v (err=%v)", len(identitiesA), err)
	}
	if identitiesA[0].ExternalUserID != "mm-user-alpha" || identitiesA[0].ExternalChatID != "ch-direct-lina" {
		t.Fatalf("unexpected user channel identity: %#v", identitiesA[0])
	}
	// Verify external identities
	extA, found, err := s.controlDB.ExternalIdentityByExternalID(workspaceID, "mattermost", "mm-user-alpha")
	if err != nil || !found || extA.UserID != "user-a" {
		t.Fatalf("expected external identity for mm-user-alpha -> user-a, got found=%v %#v", found, extA)
	}

	// 2. Bind to Mira using codeB (B4 verification: routes to Mira, NOT Lina!)
	formB := url.Values{
		"command":      {"/bind"},
		"text":         {codeB},
		"token":        {sharedCommandToken},
		"user_id":      {"mm-user-beta"},
		"user_name":    {"beta"},
		"channel_id":   {"ch-direct-mira"},
		"channel_name": {"beta__mira_bot"},
	}
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formB.Encode()))
	reqB.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rrB := httptest.NewRecorder()
	s.handleMattermostSlashBind(rrB, reqB)
	if rrB.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rrB.Code)
	}

	// Verify DB state for Agent B
	identitiesB, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: bindingB.ID,
		UserID:           "user-b",
	})
	if err != nil || len(identitiesB) != 1 {
		t.Fatalf("expected 1 user channel identity for Mira, got %v (err=%v)", len(identitiesB), err)
	}
	if identitiesB[0].ExternalUserID != "mm-user-beta" || identitiesB[0].ExternalChatID != "ch-direct-mira" {
		t.Fatalf("unexpected user channel identity: %#v", identitiesB[0])
	}

	// Verify code marked used
	codeRowA, _, _ := s.controlDB.AgentChannelBindCodeByCode(codeA)
	if strings.TrimSpace(codeRowA.UsedAt) == "" {
		t.Fatalf("expected codeA to be marked used")
	}
}

func TestMattermostSlashBind_Success_Chat(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	binding, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "pm", "tok-chat")

	now := time.Now().UTC()
	codeChat := "MG-CHAT-9999"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeChat,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "owner",
		TargetType:       "chat",
		TargetName:       "Release War Room",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})

	form := url.Values{
		"command":      {"/bind-chat"},
		"text":         {codeChat},
		"token":        {"tok-chat"},
		"user_id":      {"mm-usr-admin"},
		"channel_id":   {"ch-group-war-room"},
		"channel_name": {"release-war-room"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.handleMattermostSlashBind(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if !strings.Contains(resp["text"].(string), "群聊绑定成功") {
		t.Fatalf("expected chat bound success, got %#v", resp)
	}

	// Verify target was created
	targets, err := s.controlDB.ListAgentChannelTargets(controldb.AgentChannelTargetFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
	})
	if err != nil || len(targets) != 1 {
		t.Fatalf("expected 1 target, got %v (err=%v)", len(targets), err)
	}
	if targets[0].DisplayName != "Release War Room" || targets[0].ExternalChatID != "ch-group-war-room" {
		t.Fatalf("unexpected target: %#v", targets[0])
	}
}
