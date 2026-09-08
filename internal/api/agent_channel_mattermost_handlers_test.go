package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
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

func TestMattermostSlashBind_MultiTokenSupport(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	// Multiple comma/space separated tokens configured
	binding, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "pm", "tok-bind, tok-multigent, tok-mg")

	now := time.Now().UTC()
	for i, validTok := range []string{"tok-bind", "tok-multigent", "tok-mg"} {
		code := fmt.Sprintf("MG-MULTI-%d", i)
		_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
			Code:             code,
			WorkspaceID:      workspaceID,
			ChannelBindingID: binding.ID,
			UserID:           "owner",
			TargetType:       "user",
			ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
			CreatedAt:        now.Format(time.RFC3339),
		})

		form := url.Values{
			"command": {"/multigent"},
			"text":    {"bind " + code},
			"token":   {validTok},
			"user_id": {fmt.Sprintf("mm-usr-%d", i)},
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		s.handleMattermostSlashBind(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("token %q expected 200, got %d", validTok, rr.Code)
		}
		var resp map[string]any
		_ = json.NewDecoder(rr.Body).Decode(&resp)
		if resp["response_type"] != "ephemeral" || !strings.Contains(resp["text"].(string), "绑定成功") {
			t.Fatalf("token %q expected success, got %#v", validTok, resp)
		}
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

func TestMattermostSlashBind_ConflictRejectsTakeover(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	binding, _ := setupMattermostTestBinding(t, s, workspaceID, "sample", "lina", "tok-sample")

	_ = s.users.CreateUser("alice", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "alice", WorkspaceRoleMember)
	_ = s.users.CreateUser("bob", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "bob", WorkspaceRoleMember)

	now := time.Now().UTC()
	// 1. User A binds external user "mm-alice"
	codeA := "MG-ALICE1"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeA,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "alice",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})

	formA := url.Values{
		"command":    {"/mg"},
		"text":       {"bind " + codeA},
		"token":      {"tok-sample"},
		"user_id":    {"mm-alice"},
		"channel_id": {"ch-direct-alice"},
	}
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formA.Encode()))
	reqA.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rrA := httptest.NewRecorder()
	s.handleMattermostSlashBind(rrA, reqA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for Alice, got %d", rrA.Code)
	}

	// 2. User Bob tries to bind the same external user "mm-alice"
	codeB := "MG-BOB123"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeB,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding.ID,
		UserID:           "bob",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})

	formB := url.Values{
		"command":    {"/mg"},
		"text":       {"bind " + codeB},
		"token":      {"tok-sample"},
		"user_id":    {"mm-alice"}, // SAME external user!
		"channel_id": {"ch-direct-alice"},
	}
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formB.Encode()))
	reqB.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rrB := httptest.NewRecorder()
	s.handleMattermostSlashBind(rrB, reqB)
	if rrB.Code != http.StatusOK {
		t.Fatalf("expected 200 OK (ephemeral response), got %d", rrB.Code)
	}

	var respB map[string]any
	_ = json.NewDecoder(rrB.Body).Decode(&respB)
	respText, _ := respB["text"].(string)
	if !strings.Contains(respText, "绑定失败") || !strings.Contains(respText, "@alice") {
		t.Fatalf("expected conflict failure mentioning @alice, got: %s", respText)
	}

	// Verify Bob's bind code was NOT marked used
	codeRowB, _, _ := s.controlDB.AgentChannelBindCodeByCode(codeB)
	if strings.TrimSpace(codeRowB.UsedAt) != "" {
		t.Fatalf("expected codeB to remain unused on conflict rejection")
	}

	// Verify external identity is STILL alice
	ext, ok, err := s.controlDB.ExternalIdentityByExternalID(workspaceID, "mattermost", "mm-alice")
	if err != nil || !ok {
		t.Fatalf("external identity lookup: ok=%v err=%v", ok, err)
	}
	if ext.UserID != "alice" {
		t.Fatalf("expected external identity to remain 'alice', but got: %s", ext.UserID)
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

func TestSaveManualAgentIMChannel_RejectsDuplicateBotID(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	// Create connection for binding1
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-mira",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "agent-1test-Mira",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	}); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}

	// Agent 1 ("1test/Mira") binds bot-shared
	binding1 := controldb.AgentChannelBinding{
		ID:            "chan-mira",
		WorkspaceID:   workspaceID,
		ProjectID:     "1test",
		AgentID:       "Mira",
		Provider:      "mattermost",
		ConnectionID:  "conn-mira",
		ExternalBotID: "bot-shared",
		Status:        "connected",
	}
	if err := s.controlDB.UpsertAgentChannelBinding(binding1); err != nil {
		t.Fatalf("upsert binding1: %v", err)
	}

	// Agent 2 ("flow-check/Lina") attempts to bind the same bot-shared
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/channels", nil)
	result := imbridge.ManualSetupResult{
		Provider:      "mattermost",
		AppID:         "bot-shared",
		ExternalBotID: "bot-shared",
	}
	_, err := s.saveManualAgentIMChannel(req, workspaceID, "flow-check", "Lina", "", result)
	if err == nil {
		t.Fatalf("expected error when binding duplicate bot-shared to a different agent, got nil")
	}
	if !strings.Contains(err.Error(), "already bound to 1test/Mira") {
		t.Fatalf("expected already bound error, got: %v", err)
	}
}

func TestMattermostSlash_StatusAndHelp(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	setupMattermostTestBinding(t, s, workspaceID, "1test", "Mira", "tok-123")

	// 0. Test unauthorized access without valid token
	formNoToken := url.Values{
		"command": {"/multigent"},
		"text":    {"help"},
		"token":   {"invalid-tok"},
	}
	reqNoTok := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formNoToken.Encode()))
	reqNoTok.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recNoTok := httptest.NewRecorder()
	s.handleMattermostSlashBind(recNoTok, reqNoTok)
	if recNoTok.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthorized help call, got %d", recNoTok.Code)
	}

	// 1. Test /multigent help with valid token
	form := url.Values{
		"command": {"/multigent"},
		"text":    {"help"},
		"token":   {"tok-123"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleMattermostSlashBind(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("help response status: %d", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "Multigent ChatOps 协同指令指南") {
		t.Errorf("expected help text, got: %s", text)
	}

	// 2. Test /multigent status (unbound) with valid token
	formStatusUnbound := url.Values{
		"command":   {"/multigent"},
		"text":      {"status"},
		"token":     {"tok-123"},
		"user_id":   {"mm-unbound-user"},
		"user_name": {"stranger"},
	}
	reqUnbound := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formStatusUnbound.Encode()))
	reqUnbound.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recUnbound := httptest.NewRecorder()

	s.handleMattermostSlashBind(recUnbound, reqUnbound)
	var respUnbound map[string]any
	_ = json.Unmarshal(recUnbound.Body.Bytes(), &respUnbound)
	textUnbound, _ := respUnbound["text"].(string)
	if !strings.Contains(textUnbound, "未绑定") {
		t.Errorf("expected unbound status, got: %s", textUnbound)
	}

	// 3. Test /multigent status (bound) with valid token
	_ = s.controlDB.UpsertUser(controldb.User{
		Username: "alex",
		Role:     "manager",
	})
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "alex-identity",
		WorkspaceID:      workspaceID,
		UserID:           "alex",
		ChannelBindingID: "chan-mm-1test-Mira",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-alex",
	})

	formStatusBound := url.Values{
		"command":   {"/mg"},
		"text":      {"status"},
		"token":     {"tok-123"},
		"user_id":   {"mm-user-alex"},
		"user_name": {"alex"},
	}
	reqBound := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formStatusBound.Encode()))
	reqBound.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recBound := httptest.NewRecorder()

	s.handleMattermostSlashBind(recBound, reqBound)
	var respBound map[string]any
	_ = json.Unmarshal(recBound.Body.Bytes(), &respBound)
	textBound, _ := respBound["text"].(string)
	if !strings.Contains(textBound, "@alex") || !strings.Contains(textBound, "已就绪") {
		t.Errorf("expected bound status with @alex and 已就绪, got: %s", textBound)
	}
}
