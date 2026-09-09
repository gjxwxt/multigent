package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
	"github.com/multigent/multigent/internal/workflow"
)

func nowUTCStr() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// 1. 不同独立实例、相同外部 user ID，分别绑定不同平台用户成功
func TestChatops_SlashBind_CrossInstance_SameExternalUser_Allowed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	_ = s.users.CreateUser("alice", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "alice", WorkspaceRoleMember)
	_ = s.users.CreateUser("bob", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "bob", WorkspaceRoleMember)

	// Two completely independent connections (no shared IMInstanceID)
	conn1 := "conn-indep-1"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             conn1,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "MM-Indep-1",
		Status:         "active",
	})
	sec1, _ := controldb.SealConnectionSecret(map[string]string{"commandToken": "tok-1"})
	sec1.ConnectionID = conn1
	_ = s.controlDB.UpsertConnectionSecret(sec1)
	binding1 := controldb.AgentChannelBinding{
		ID:           "bind-1",
		WorkspaceID:  workspaceID,
		ProjectID:    "p1",
		AgentID:      "a1",
		Provider:     "mattermost",
		ConnectionID: conn1,
		Status:       "connected",
	}
	_ = s.controlDB.UpsertAgentChannelBinding(binding1)

	conn2 := "conn-indep-2"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             conn2,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "MM-Indep-2",
		Status:         "active",
	})
	sec2, _ := controldb.SealConnectionSecret(map[string]string{"commandToken": "tok-2"})
	sec2.ConnectionID = conn2
	_ = s.controlDB.UpsertConnectionSecret(sec2)
	binding2 := controldb.AgentChannelBinding{
		ID:           "bind-2",
		WorkspaceID:  workspaceID,
		ProjectID:    "p2",
		AgentID:      "a2",
		Provider:     "mattermost",
		ConnectionID: conn2,
		Status:       "connected",
	}
	_ = s.controlDB.UpsertAgentChannelBinding(binding2)

	now := time.Now().UTC()
	// Alice binds mm-user-common on conn-indep-1
	codeA := "MG-ALICE-COMMON"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeA,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding1.ID,
		UserID:           "alice",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})
	formA := url.Values{
		"command":    {"/mg"},
		"text":       {"bind " + codeA},
		"token":      {"tok-1"},
		"user_id":    {"mm-user-common"},
		"channel_id": {"ch-alice"},
	}
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formA.Encode()))
	reqA.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recA := httptest.NewRecorder()
	s.handleMattermostSlashBind(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("Alice bind failed: %d %s", recA.Code, recA.Body.String())
	}

	// Bob binds the SAME mm-user-common on conn-indep-2 (separate instance scope)
	codeB := "MG-BOB-COMMON"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeB,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding2.ID,
		UserID:           "bob",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})
	formB := url.Values{
		"command":    {"/mg"},
		"text":       {"bind " + codeB},
		"token":      {"tok-2"},
		"user_id":    {"mm-user-common"},
		"channel_id": {"ch-bob"},
	}
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formB.Encode()))
	reqB.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recB := httptest.NewRecorder()
	s.handleMattermostSlashBind(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("Bob independent bind failed: %d %s", recB.Code, recB.Body.String())
	}

	// Verify both identities co-exist without conflict or collision
	id1, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding1.ID,
	})
	if err != nil || len(id1) != 1 || id1[0].UserID != "alice" {
		t.Fatalf("expected alice on bind1, got: %#v err=%v", id1, err)
	}

	id2, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding2.ID,
	})
	if err != nil || len(id2) != 1 || id2[0].UserID != "bob" {
		t.Fatalf("expected bob on bind2, got: %#v err=%v", id2, err)
	}
}

// 2. 同一已确认实例跨 Bot，绑定抢占被拒绝
func TestChatops_SlashBind_SameAttestedInstance_CrossBot_ConflictRejected(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	_ = s.users.CreateUser("alice", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "alice", WorkspaceRoleMember)
	_ = s.users.CreateUser("bob", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "bob", WorkspaceRoleMember)

	instanceID := "imi-attested-test"
	_ = s.controlDB.UpsertIMInstance(controldb.IMInstance{
		ID:          instanceID,
		WorkspaceID: workspaceID,
		Provider:    "mattermost",
		DisplayName: "Mattermost Test",
		Attestation: imInstanceAttestationAdmin,
		CreatedAt:   nowUTCStr(),
	})

	conn1 := "conn-inst-1"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             conn1,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "MM-Inst-1",
		Status:         "active",
		IMInstanceID:   instanceID,
	})
	sec1, _ := controldb.SealConnectionSecret(map[string]string{"commandToken": "tok-1"})
	sec1.ConnectionID = conn1
	_ = s.controlDB.UpsertConnectionSecret(sec1)
	binding1 := controldb.AgentChannelBinding{
		ID:           "bind-inst-1",
		WorkspaceID:  workspaceID,
		ProjectID:    "p1",
		AgentID:      "a1",
		Provider:     "mattermost",
		ConnectionID: conn1,
		Status:       "connected",
	}
	_ = s.controlDB.UpsertAgentChannelBinding(binding1)

	conn2 := "conn-inst-2"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             conn2,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "MM-Inst-2",
		Status:         "active",
		IMInstanceID:   instanceID,
	})
	sec2, _ := controldb.SealConnectionSecret(map[string]string{"commandToken": "tok-2"})
	sec2.ConnectionID = conn2
	_ = s.controlDB.UpsertConnectionSecret(sec2)
	binding2 := controldb.AgentChannelBinding{
		ID:           "bind-inst-2",
		WorkspaceID:  workspaceID,
		ProjectID:    "p2",
		AgentID:      "a2",
		Provider:     "mattermost",
		ConnectionID: conn2,
		Status:       "connected",
	}
	_ = s.controlDB.UpsertAgentChannelBinding(binding2)

	now := time.Now().UTC()
	// Alice binds mm-alice on conn-inst-1
	codeA := "MG-ALICE-1"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeA,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding1.ID,
		UserID:           "alice",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})
	formA := url.Values{
		"command":    {"/mg"},
		"text":       {"bind " + codeA},
		"token":      {"tok-1"},
		"user_id":    {"mm-alice"},
		"channel_id": {"ch-alice"},
	}
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formA.Encode()))
	reqA.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recA := httptest.NewRecorder()
	s.handleMattermostSlashBind(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("Alice bind failed: %d", recA.Code)
	}

	// Bob attempts to claim the SAME mm-alice on conn-inst-2 (same admin_attested instance) -> Must be rejected!
	codeB := "MG-BOB-2"
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             codeB,
		WorkspaceID:      workspaceID,
		ChannelBindingID: binding2.ID,
		UserID:           "bob",
		TargetType:       "user",
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
		CreatedAt:        now.Format(time.RFC3339),
	})
	formB := url.Values{
		"command":    {"/mg"},
		"text":       {"bind " + codeB},
		"token":      {"tok-2"},
		"user_id":    {"mm-alice"},
		"channel_id": {"ch-bob"},
	}
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/commands/bind", strings.NewReader(formB.Encode()))
	reqB.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recB := httptest.NewRecorder()
	s.handleMattermostSlashBind(recB, reqB)

	var respB map[string]any
	_ = json.Unmarshal(recB.Body.Bytes(), &respB)
	textB, _ := respB["text"].(string)
	if !strings.Contains(textB, "绑定失败") || !strings.Contains(textB, "@alice") {
		t.Fatalf("expected conflict failure mentioning @alice, got: %s", textB)
	}
}

// 3. 空/非法/篡改 ConnectionID 卡片 fail-closed
func TestChatops_Approval_EmptyOrTamperedConnectionID_FailClosed(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	// Case A: Missing ConnectionID
	tokEmptyConn, _ := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "", // EMPTY!
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	bodyEmpty, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		ChannelID: "chan-chatops-1",
		Context:   mattermostActionContext{ActionToken: tokEmptyConn, Action: "approve"},
	})
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyEmpty))
	recA := httptest.NewRecorder()
	s.handleMattermostActionCallback(recA, reqA)
	var respA map[string]any
	_ = json.Unmarshal(recA.Body.Bytes(), &respA)
	if !strings.Contains(respA["text"].(string), "安全拦截") {
		t.Fatalf("expected safety intercept on empty connectionID, got: %v", respA)
	}

	// Case B: Non-existent ConnectionID
	tokGhostConn, _ := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         "conn-ghost-nonexistent",
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	bodyGhost, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		ChannelID: "chan-chatops-1",
		Context:   mattermostActionContext{ActionToken: tokGhostConn, Action: "approve"},
	})
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyGhost))
	recB := httptest.NewRecorder()
	s.handleMattermostActionCallback(recB, reqB)
	var respB map[string]any
	_ = json.Unmarshal(recB.Body.Bytes(), &respB)
	if !strings.Contains(respB["text"].(string), "安全拦截") && !strings.Contains(respB["text"].(string), "无法获取") {
		t.Fatalf("expected safety intercept on ghost connectionID, got: %v", respB)
	}
}

// 4. 同一可信范围出现多身份歧义时，审批 fail-closed 并记录审计
func TestChatops_Approval_AmbiguousIdentityInTrustedScope_FailClosed(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	// Create user charlie
	_ = s.users.CreateUser("charlie", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "charlie", WorkspaceRoleMember)

	// Inject a second conflicting identity on another binding of the same connection
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "binding-chatops-conflicting",
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		AgentID:      "pm-shadow",
		Provider:     "mattermost",
		ConnectionID: "conn-mm-chatops-test",
		Status:       "connected",
	})
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "ucid-conflicting",
		WorkspaceID:      workspaceID,
		UserID:           "charlie", // Charlie claims the same mm-user-admin!
		ChannelBindingID: "binding-chatops-conflicting",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-admin",
	})

	tok, _ := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
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
	body, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		ChannelID: "chan-chatops-1",
		Context:   mattermostActionContext{ActionToken: tok, Action: "approve"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	text, _ := resp["text"].(string)
	if !strings.Contains(text, "身份验证异常") && !strings.Contains(text, "歧义") {
		t.Fatalf("expected ambiguity failure response, got: %s", text)
	}

	// Verify workflow step did NOT advance
	wfStore := workflow.NewStore(s.controlDB, workspaceID)
	run, ok, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !ok {
		t.Fatalf("RunForTask: %v", err)
	}
	if run.ActiveStepID != preview.StepID {
		t.Fatalf("workflow should remain on %s on ambiguity rejection, but was on %s", preview.StepID, run.ActiveStepID)
	}
}

// 5. 同实例跨 Bot 审批成功；解除实例关联后立刻失败
func TestChatops_Approval_SameInstance_CrossBot_ApprovedAndRevoked(t *testing.T) {
	s, workspaceID, mockMM, _, _, _ := setupTestChatopsEnv(t)

	instanceID := "imi-shared-approval"
	_ = s.controlDB.UpsertIMInstance(controldb.IMInstance{
		ID:          instanceID,
		WorkspaceID: workspaceID,
		Provider:    "mattermost",
		DisplayName: "Shared Approval Instance",
		Attestation: imInstanceAttestationAdmin,
		CreatedAt:   nowUTCStr(),
	})

	// Bot 1 (Mira): where user Alex bound
	connMira := "conn-mira"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connMira,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "Mira-Bot",
		Status:         "active",
		IMInstanceID:   instanceID,
	})
	bindMira := controldb.AgentChannelBinding{
		ID:           "bind-mira",
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		AgentID:      "mira",
		Provider:     "mattermost",
		ConnectionID: connMira,
		Status:       "connected",
	}
	_ = s.controlDB.UpsertAgentChannelBinding(bindMira)

	// User Alex binds on Mira
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "ucid-alex-mira",
		WorkspaceID:      workspaceID,
		UserID:           "admin", // Multigent admin user
		ChannelBindingID: "bind-mira",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-alex",
	})

	// Bot 2 (Lina): sends the approval card
	connLina := "conn-lina"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connLina,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "Lina-Bot",
		Status:         "active",
		IMInstanceID:   instanceID,
	})
	secLina, _ := controldb.SealConnectionSecret(map[string]string{
		"baseUrl":          mockMM.URL,
		"botToken":         "token-lina-123",
		"bridgeHmacSecret": "lina-secret-123",
	})
	secLina.ConnectionID = connLina
	_ = s.controlDB.UpsertConnectionSecret(secLina)
	bindLina := controldb.AgentChannelBinding{
		ID:           "bind-lina",
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		AgentID:      "lina",
		Provider:     "mattermost",
		ConnectionID: connLina,
		Status:       "connected",
	}
	_ = s.controlDB.UpsertAgentChannelBinding(bindLina)

	task, preview := setupTestWorkflowTask(t, s, workspaceID)

	// Step A: Alex approves card sent by Lina (Cross-bot via same attested instance) -> SUCCEEDS!
	tokLina, _ := imbridge.SignActionToken("lina-secret-123", imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               preview.StepID,
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         connLina, // Sent by Lina!
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	bodyLina, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-alex", // Alex clicking!
		ChannelID: "chan-chatops-1",
		Context:   mattermostActionContext{ActionToken: tokLina, Action: "approve"},
	})
	reqA := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyLina))
	recA := httptest.NewRecorder()
	s.handleMattermostActionCallback(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %d", recA.Code)
	}
	var respA map[string]any
	_ = json.Unmarshal(recA.Body.Bytes(), &respA)
	if !strings.Contains(respA["text"].(string), "审批已成功提交") {
		t.Fatalf("expected success message, got: %v", respA)
	}

	// Step B: Now detach Lina from the instance
	_ = s.controlDB.ClearConnectionIMInstance(workspaceID, instanceID, connLina)

	// Re-try approval on a new step / card -> MUST FAIL CLOSED!
	tokLina2, _ := imbridge.SignActionToken("lina-secret-123", imbridge.ActionTokenPayload{
		WorkspaceID:          workspaceID,
		ProjectID:            "sample",
		TaskID:               task.ID,
		StepID:               "next-step",
		Action:               "approve",
		ChannelID:            "chan-chatops-1",
		ConnectionID:         connLina,
		ExpectedStateVersion: 2,
		ReviewSnapshotHash:   "hash-2",
		Nonce:                imbridge.GenerateNonce(),
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	})
	bodyLina2, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-alex",
		ChannelID: "chan-chatops-1",
		Context:   mattermostActionContext{ActionToken: tokLina2, Action: "approve"},
	})
	reqB := httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(bodyLina2))
	recB := httptest.NewRecorder()
	s.handleMattermostActionCallback(recB, reqB)
	var respB map[string]any
	_ = json.Unmarshal(recB.Body.Bytes(), &respB)
	textB, _ := respB["text"].(string)
	if !strings.Contains(textB, "未与 Multigent 平台关联") {
		t.Fatalf("expected unassociated rejection after instance detachment, got: %s", textB)
	}
}
