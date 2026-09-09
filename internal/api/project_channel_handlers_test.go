package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/imbridge"
)

func TestProvisionProjectChannel_Success(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	projectName := "order-center"

	// 1. Create project
	if err := s.st.SaveProject(projectName, &entity.Project{Name: projectName}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// 2. Setup mock Mattermost server
	invitedUsers := make(map[string]bool)
	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-bot-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{
				{ID: "team-mm-1", Name: "team-main", DisplayName: "Main Team"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			var ch imbridge.MattermostChannel
			_ = json.NewDecoder(r.Body).Decode(&ch)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{
				ID:          "chan-mm-provisioned-1",
				TeamID:      ch.TeamID,
				Name:        ch.Name,
				DisplayName: ch.DisplayName,
				Type:        ch.Type,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if u := body["user_id"]; u != "" {
				invitedUsers[u] = true
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockMM.Close()

	// 3. Setup Mattermost connection and secret
	connID := "conn-mm-provision-test"
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mattermost-bot-main",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"botId":"bot-user-123","baseUrl":"` + mockMM.URL + `"}`,
		IMInstanceID:   "inst-test-1",
	}); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}

	secret, err := sealConnectionSecret(map[string]string{
		"baseUrl":  mockMM.URL,
		"botToken": "test-bot-token",
		"appId":    "bot-user-123",
	})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("upsert secret: %v", err)
	}

	// 4. Setup bound user 'alex' in user_channel_identities
	_ = s.users.CreateUser("admin", "adminpass", RoleAdmin, "", "", "", "", "")
	_ = s.users.CreateUser("alex", "pass", RoleMember, "", "", "", "", "")

	dummyBinding := controldb.AgentChannelBinding{
		ID:           "chan-dummy-alex",
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		AgentID:      "pm",
		Provider:     "mattermost",
		ConnectionID: connID,
		Status:       "connected",
	}
	if err := s.controlDB.UpsertAgentChannelBinding(dummyBinding); err != nil {
		t.Fatalf("upsert dummy binding: %v", err)
	}

	if err := s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "uch-alex-mm",
		WorkspaceID:      workspaceID,
		UserID:           "alex",
		ChannelBindingID: "chan-dummy-alex",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-alex-id",
		ExternalChatID:   "dummy-chat",
	}); err != nil {
		t.Fatalf("upsert user channel identity: %v", err)
	}

	// 5. Call handleProvisionProjectChannel
	reqBody := projectChannelProvisionRequest{
		Provider:        "mattermost",
		ConnectionID:    connID,
		ChannelName:     "proj-order-center",
		DisplayName:     "订单中心协作频道",
		Visibility:      "private",
		WorkerIDs:       []string{"aw-mira", "aw-lina"},
		MemberUsernames: []string{"alex", "unbound-charlie"},
	}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+projectName+"/channels/provision", "admin", reqBody)
	req.SetPathValue("name", projectName)

	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp projectChannelProvisionResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !resp.OK {
		t.Fatalf("expected ok true, got false")
	}
	if resp.Status != "success" {
		t.Fatalf("expected status success, got %q", resp.Status)
	}
	if resp.ChannelID != "chan-mm-provisioned-1" {
		t.Fatalf("expected channel ID chan-mm-provisioned-1, got %q", resp.ChannelID)
	}
	if resp.ChannelName != "proj-order-center" {
		t.Fatalf("expected channelName proj-order-center, got %q", resp.ChannelName)
	}
	if resp.Visibility != "private" {
		t.Fatalf("expected visibility private, got %q", resp.Visibility)
	}
	if len(resp.InvitedMembers) != 1 || resp.InvitedMembers[0] != "alex" {
		t.Fatalf("expected invitedMembers [alex], got %v", resp.InvitedMembers)
	}
	if len(resp.UnboundMembers) != 1 || resp.UnboundMembers[0] != "unbound-charlie" {
		t.Fatalf("expected unboundMembers [unbound-charlie], got %v", resp.UnboundMembers)
	}

	// Verify Mattermost invites
	if !invitedUsers["bot-user-123"] {
		t.Fatalf("expected bot-user-123 to be invited")
	}
	if !invitedUsers["mm-user-alex-id"] {
		t.Fatalf("expected mm-user-alex-id to be invited")
	}

	// Verify ProjectChannelLink
	link, found, err := s.controlDB.GetProjectChannelLink(workspaceID, projectName, "mattermost", "inst-test-1")
	if err != nil || !found {
		t.Fatalf("expected ProjectChannelLink to be saved, found=%v, err=%v", found, err)
	}
	if link.ChannelID != "chan-mm-provisioned-1" {
		t.Fatalf("expected ProjectChannelLink channel ID chan-mm-provisioned-1, got %q", link.ChannelID)
	}

	// Verify DB bindings
	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectName,
		Provider:    "mattermost",
		Status:      "connected",
	})
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) == 0 {
		t.Fatalf("expected bindings for project, got none")
	}
	for _, b := range bindings {
		if b.ExternalChatID != "chan-mm-provisioned-1" {
			t.Errorf("binding %s has unexpected chat ID %q", b.AgentID, b.ExternalChatID)
		}
	}
}

// Test 1: Same Agent in Two Projects (Verify no overwrite)
func TestProvisionProjectChannel_SameAgentTwoProjects_NoOverwrite(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	projA := "project-alpha"
	projB := "project-beta"

	_ = s.st.SaveProject(projA, &entity.Project{Name: projA})
	_ = s.st.SaveProject(projB, &entity.Project{Name: projB})

	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "team-1", Name: "team-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			var ch imbridge.MattermostChannel
			_ = json.NewDecoder(r.Body).Decode(&ch)
			cid := "chan-" + ch.Name
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{
				ID:     cid,
				TeamID: ch.TeamID,
				Name:   ch.Name,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockMM.Close()

	connID := "conn-shared"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-shared",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `","botId":"bot-shared"}`,
	})
	sec, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-xyz"})
	sec.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(sec)

	_ = s.users.CreateUser("admin", "adminpass", RoleAdmin, "", "", "", "", "")

	// 1. Provision Project A with Lina
	reqA := providerTestRequest(http.MethodPost, "/api/v1/projects/"+projA+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		ChannelName:  "proj-alpha-chan",
		WorkerIDs:    []string{"Lina"},
	})
	reqA.SetPathValue("name", projA)
	rrA := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrA, reqA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("projA provision failed: %d %s", rrA.Code, rrA.Body.String())
	}

	// 2. Provision Project B with Lina
	reqB := providerTestRequest(http.MethodPost, "/api/v1/projects/"+projB+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		ChannelName:  "proj-beta-chan",
		WorkerIDs:    []string{"Lina"},
	})
	reqB.SetPathValue("name", projB)
	rrB := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrB, reqB)
	if rrB.Code != http.StatusOK {
		t.Fatalf("projB provision failed: %d %s", rrB.Code, rrB.Body.String())
	}

	// 3. Verify Project A's binding was NOT overwritten
	bindingsA, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projA,
		AgentID:     "Lina",
		Provider:    "mattermost",
	})
	if len(bindingsA) != 1 {
		t.Fatalf("expected 1 binding for ProjA/Lina, got %d", len(bindingsA))
	}
	if bindingsA[0].ExternalChatID != "chan-proj-alpha-chan" {
		t.Fatalf("CRITICAL: ProjA binding overwritten! expected chan-proj-alpha-chan, got %q", bindingsA[0].ExternalChatID)
	}

	// 4. Verify Project B's binding is separate
	bindingsB, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projB,
		AgentID:     "Lina",
		Provider:    "mattermost",
	})
	if len(bindingsB) != 1 {
		t.Fatalf("expected 1 binding for ProjB/Lina, got %d", len(bindingsB))
	}
	if bindingsB[0].ExternalChatID != "chan-proj-beta-chan" {
		t.Fatalf("expected chan-proj-beta-chan for ProjB, got %q", bindingsB[0].ExternalChatID)
	}

	if bindingsA[0].ID == bindingsB[0].ID {
		t.Fatalf("CRITICAL: ProjA and ProjB share the exact same binding ID %q", bindingsA[0].ID)
	}
}

// Test 2: Multi-Instance Isolation & Safety
func TestProvisionProjectChannel_MultiInstanceIsolation(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-multi-inst"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	var invitedUserIDs []string
	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "t-1", Name: "team-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{ID: "c-inst-1", TeamID: "t-1", Name: "chan-inst-1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if u := body["user_id"]; u != "" {
				invitedUserIDs = append(invitedUserIDs, u)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockMM.Close()

	// Setup 2 connections in 2 distinct instances
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-inst-1",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-inst-1",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		IMInstanceID:   "instance-alpha",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `","botId":"bot-alpha"}`,
	})
	sec1, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-1"})
	sec1.ConnectionID = "conn-inst-1"
	_ = s.controlDB.UpsertConnectionSecret(sec1)

	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-inst-2",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-inst-2",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		IMInstanceID:   "instance-beta",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `","botId":"bot-beta"}`,
	})
	sec2, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-2"})
	sec2.ConnectionID = "conn-inst-2"
	_ = s.controlDB.UpsertConnectionSecret(sec2)

	// Setup user-alpha in instance-alpha and user-beta in instance-beta
	_ = s.users.CreateUser("user-alpha", "pass", RoleMember, "", "", "", "", "")
	_ = s.users.CreateUser("user-beta", "pass", RoleMember, "", "", "", "", "")

	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "bind-alpha-ref",
		WorkspaceID:  workspaceID,
		ProjectID:    "other-proj",
		AgentID:      "agent-a",
		Provider:     "mattermost",
		ConnectionID: "conn-inst-1",
	})
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "ident-alpha",
		WorkspaceID:      workspaceID,
		UserID:           "user-alpha",
		ChannelBindingID: "bind-alpha-ref",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-alpha",
	})

	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "bind-beta-ref",
		WorkspaceID:  workspaceID,
		ProjectID:    "other-proj",
		AgentID:      "agent-b",
		Provider:     "mattermost",
		ConnectionID: "conn-inst-2",
	})
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "ident-beta",
		WorkspaceID:      workspaceID,
		UserID:           "user-beta",
		ChannelBindingID: "bind-beta-ref",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-beta",
	})

	// Case A: Missing instanceId when multiple exist -> should fail-closed with 400
	reqAmbiguous := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:    "mattermost",
		ChannelName: "test-chan",
	})
	reqAmbiguous.SetPathValue("name", proj)
	rrAmbiguous := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrAmbiguous, reqAmbiguous)
	if rrAmbiguous.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when ambiguous instanceId, got %d: %s", rrAmbiguous.Code, rrAmbiguous.Body.String())
	}

	// Case B: Explicit instanceId -> succeeds and scopes to instance-alpha (excluding instance-beta bots and members)
	reqExplicit := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:        "mattermost",
		InstanceID:      "instance-alpha",
		ChannelName:     "test-chan-alpha",
		MemberUsernames: []string{"user-alpha", "user-beta"},
	})
	reqExplicit.SetPathValue("name", proj)
	rrExplicit := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrExplicit, reqExplicit)
	if rrExplicit.Code != http.StatusOK {
		t.Fatalf("expected 200 with explicit instanceId, got %d: %s", rrExplicit.Code, rrExplicit.Body.String())
	}
	var resp projectChannelProvisionResponse
	_ = json.NewDecoder(rrExplicit.Body).Decode(&resp)
	if resp.InstanceID != "instance-alpha" {
		t.Fatalf("expected instanceId instance-alpha, got %q", resp.InstanceID)
	}

	// Verify user-alpha was invited but user-beta was reported as unbound on instance-alpha
	if len(resp.InvitedMembers) != 1 || resp.InvitedMembers[0] != "user-alpha" {
		t.Fatalf("expected invitedMembers [user-alpha], got %v (failed: %v, unbound: %v, warning: %q)", resp.InvitedMembers, resp.FailedMembers, resp.UnboundMembers, resp.Warning)
	}
	if len(resp.UnboundMembers) != 1 || resp.UnboundMembers[0] != "user-beta" {
		t.Fatalf("expected unboundMembers [user-beta], got %v", resp.UnboundMembers)
	}

	// Verify mock server invited bot-alpha and mm-user-alpha, but NOT bot-beta or mm-user-beta
	for _, u := range invitedUserIDs {
		if u == "bot-beta" || u == "mm-user-beta" {
			t.Fatalf("CRITICAL LEAK: instance-beta entity %q was invited to instance-alpha channel!", u)
		}
	}
}

// Test 3: Link Mode Non-Existent Channel (Strict 404, never secretly creates)
func TestProvisionProjectChannel_LinkMode_NotFound(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-link-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	channelCreated := false
	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "t-1", Name: "team-1"}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/teams/t-1/channels/name/"):
			// Simulate channel not found
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"store.sql_channel.get.existing.app_error","message":"Channel not found"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			channelCreated = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockMM.Close()

	connID := "conn-link"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-link",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `"}`,
	})
	sec, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-xyz"})
	sec.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(sec)

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		Mode:         "link",
		ChannelName:  "non-existent-channel",
	})
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent link, got %d: %s", rr.Code, rr.Body.String())
	}
	if channelCreated {
		t.Fatalf("CRITICAL: CreateChannel was invoked during link mode!")
	}
}

// Test 4: Non-Manager RBAC Denied (403 Forbidden)
func TestProvisionProjectChannel_NonManager_Forbidden(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	proj := "proj-rbac-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})

	// User 'charlie' is a regular member, NOT a project manager or admin
	_ = s.users.CreateUser("charlie", "pass", RoleMember, "", "", "", "", "")

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "charlie", projectChannelProvisionRequest{
		Provider:    "mattermost",
		ChannelName: "test-chan",
	})
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-manager, got %d: %s", rr.Code, rr.Body.String())
	}
}

// Test 5: Partial Invite Failure Reporting
func TestProvisionProjectChannel_PartialFailureReporting(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-partial-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")
	_ = s.users.CreateUser("good-user", "pass", RoleMember, "", "", "", "", "")
	_ = s.users.CreateUser("bad-user", "pass", RoleMember, "", "", "", "", "")

	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "t-1", Name: "team-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{ID: "c-part", TeamID: "t-1", Name: "chan-partial"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] == "mm-bad-user" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"permission denied to invite user"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockMM.Close()

	connID := "conn-part"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-part",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `"}`,
	})
	sec, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-xyz"})
	sec.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(sec)

	_ = s.controlDB.UpsertExternalIdentity(controldb.ExternalIdentity{
		ID:             "ext-good",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ExternalUserID: "mm-good-user",
		UserID:         "good-user",
	})
	_ = s.controlDB.UpsertExternalIdentity(controldb.ExternalIdentity{
		ID:             "ext-bad",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ExternalUserID: "mm-bad-user",
		UserID:         "bad-user",
	})

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:        "mattermost",
		ConnectionID:    connID,
		ChannelName:     "chan-partial",
		MemberUsernames: []string{"good-user", "bad-user"},
	})
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp projectChannelProvisionResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	if !resp.OK {
		t.Fatalf("expected OK true")
	}
	if resp.Status != "partial" {
		t.Fatalf("expected status partial, got %q", resp.Status)
	}
	if len(resp.InvitedMembers) != 1 || resp.InvitedMembers[0] != "good-user" {
		t.Fatalf("expected invitedMembers [good-user], got %v", resp.InvitedMembers)
	}
	if len(resp.FailedMembers) != 1 || resp.FailedMembers[0] != "bad-user" {
		t.Fatalf("expected failedMembers [bad-user], got %v", resp.FailedMembers)
	}
	if !strings.Contains(resp.Warning, "bad-user") {
		t.Fatalf("expected warning to mention bad-user, got %q", resp.Warning)
	}
}

// Test 6: Create Mode Conflict (Strict 409 when channel already exists and unlinked)
func TestProvisionProjectChannel_CreateMode_Conflict409(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-conflict-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "t-1", Name: "team-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			// Return 400 conflict as Mattermost does when channel already exists
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"id":"store.sql_channel.saved.create.found.app_error","message":"A channel with that name already exists on the same team"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockMM.Close()

	connID := "conn-conflict"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-conflict",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `"}`,
	})
	sec, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-xyz"})
	sec.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(sec)

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		Mode:         "create",
		ChannelName:  "already-exists-chan",
	})
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "channel_already_exists") {
		t.Fatalf("expected error code channel_already_exists, got %s", rr.Body.String())
	}
}

// Test 7: Create Mode Idempotent Retry (200 OK when channel exists and matches project's existing link)
func TestProvisionProjectChannel_CreateMode_IdempotentRetry(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-idempotent-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "t-1", Name: "team-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			// Conflict on create
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"id":"store.sql_channel.saved.create.found.app_error","message":"A channel with that name already exists on the same team"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/teams/t-1/channels/name/"):
			// Return existing channel matching the link
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{
				ID:          "chan-matching-link",
				TeamID:      "t-1",
				Name:        "existing-proj-chan",
				DisplayName: "Existing Proj Chan",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockMM.Close()

	connID := "conn-idempotent"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-idempotent",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		IMInstanceID:   "inst-idem",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `","botId":"bot-idem"}`,
	})
	sec, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-xyz"})
	sec.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(sec)

	// Pre-seed ProjectChannelLink for THIS project
	_ = s.controlDB.UpsertProjectChannelLink(controldb.ProjectChannelLink{
		ID:           "pcl-existing",
		WorkspaceID:  workspaceID,
		ProjectID:    proj,
		Provider:     "mattermost",
		IMInstanceID: "inst-idem",
		TeamID:       "t-1",
		ChannelID:    "chan-matching-link",
		ChannelName:  "existing-proj-chan",
		Status:       "active",
	})

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		Mode:         "create",
		ChannelName:  "existing-proj-chan",
	})
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on idempotent retry, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp projectChannelProvisionResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.ChannelID != "chan-matching-link" {
		t.Fatalf("expected channelId chan-matching-link, got %q", resp.ChannelID)
	}
}

// Test 8: Bot Failure Tracking (Partial status, failedAgents, failedBots, no target created)
func TestProvisionProjectChannel_BotFailureTracking(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-bot-fail-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	allowBot := false
	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "t-1", Name: "team-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{
				ID:          "chan-bot-test",
				TeamID:      "t-1",
				Name:        "chan-bot-fail",
				DisplayName: "Bot Fail Chan",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] == "bot-will-fail" && !allowBot {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"id":"api.channel.add_member.user.app_error","message":"User cannot be added to channel"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockMM.Close()

	connID := "conn-failing-bot"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-failing-bot-conn",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"` + mockMM.URL + `","botId":"bot-will-fail"}`,
	})
	sec, _ := sealConnectionSecret(map[string]string{"baseUrl": mockMM.URL, "botToken": "token-xyz"})
	sec.ConnectionID = connID
	_ = s.controlDB.UpsertConnectionSecret(sec)

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		ChannelName:  "chan-bot-fail",
		WorkerIDs:    []string{"Mira"},
	})
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp projectChannelProvisionResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	if resp.Status != "partial" {
		t.Fatalf("expected status partial, got %q", resp.Status)
	}
	if len(resp.FailedBots) != 1 || resp.FailedBots[0] != "bot-will-fail" {
		t.Fatalf("expected failedBots [bot-will-fail], got %v", resp.FailedBots)
	}
	if len(resp.FailedAgents) == 0 {
		t.Fatalf("expected failedAgents to contain agents whose bot failed, got %v", resp.FailedAgents)
	}
	if len(resp.BoundAgents) != 0 {
		t.Fatalf("expected 0 boundAgents when bot failed, got %v", resp.BoundAgents)
	}

	// Verify binding status in DB is "error"
	bindings, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   proj,
		Provider:    "mattermost",
	})
	if len(bindings) == 0 || bindings[0].Status != "error" {
		t.Fatalf("expected binding status error, got %#v", bindings)
	}

	// Verify NO active AgentChannelTarget was created
	targets, _ := s.controlDB.ListAgentChannelTargets(controldb.AgentChannelTargetFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: bindings[0].ID,
	})
	if len(targets) != 0 {
		t.Fatalf("expected 0 AgentChannelTargets for failed bot, got %d", len(targets))
	}

	// Verify retry recovery: allow bot invite now, re-provision, and verify status recovered to "connected"
	allowBot = true
	reqRetry := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: connID,
		ChannelName:  "chan-bot-fail",
		WorkerIDs:    []string{"Mira"},
	})
	reqRetry.SetPathValue("name", proj)
	rrRetry := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrRetry, reqRetry)

	if rrRetry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retry, got %d: %s", rrRetry.Code, rrRetry.Body.String())
	}
	var respRetry projectChannelProvisionResponse
	_ = json.NewDecoder(rrRetry.Body).Decode(&respRetry)
	if respRetry.Status != "success" {
		t.Fatalf("expected status success on retry, got %q (warning: %q)", respRetry.Status, respRetry.Warning)
	}
	if len(respRetry.BoundAgents) == 0 || respRetry.BoundAgents[0] != "Mira" {
		t.Fatalf("expected Mira to be bound on retry, got %v", respRetry.BoundAgents)
	}

	// Verify binding status in DB was updated to "connected" via upsert (NOT stuck in "error")
	recoveredBindings, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   proj,
		Provider:    "mattermost",
	})
	if len(recoveredBindings) == 0 || recoveredBindings[0].Status != "connected" {
		t.Fatalf("expected binding status updated to connected on retry, got %#v", recoveredBindings)
	}

	// Verify active AgentChannelTarget was successfully created on retry recovery
	recoveredTargets, _ := s.controlDB.ListAgentChannelTargets(controldb.AgentChannelTargetFilter{
		WorkspaceID:      workspaceID,
		ChannelBindingID: recoveredBindings[0].ID,
	})
	if len(recoveredTargets) == 0 {
		t.Fatalf("expected AgentChannelTarget to be created after retry recovery")
	}
}

// Test 9: Connection Validation (Inactive connection or instance mismatch)
func TestProvisionProjectChannel_ConnectionValidation(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-conn-val"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	// 1. Inactive connection
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-inactive",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-inactive",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "disabled",
		IMInstanceID:   "inst-a",
	})

	reqInactive := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: "conn-inactive",
		ChannelName:  "test-chan",
	})
	reqInactive.SetPathValue("name", proj)
	rrInactive := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrInactive, reqInactive)
	if rrInactive.Code != http.StatusBadRequest || !strings.Contains(rrInactive.Body.String(), "not active") {
		t.Fatalf("expected 400 not active, got %d: %s", rrInactive.Code, rrInactive.Body.String())
	}

	// 2. Active connection with instance mismatch
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-active-inst-a",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-inst-a",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		IMInstanceID:   "inst-a",
	})

	reqMismatch := providerTestRequest(http.MethodPost, "/api/v1/projects/"+proj+"/channels/provision", "admin", projectChannelProvisionRequest{
		Provider:     "mattermost",
		ConnectionID: "conn-active-inst-a",
		InstanceID:   "inst-b", // Mismatched!
		ChannelName:  "test-chan",
	})
	reqMismatch.SetPathValue("name", proj)
	rrMismatch := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rrMismatch, reqMismatch)
	if rrMismatch.Code != http.StatusBadRequest || !strings.Contains(rrMismatch.Body.String(), "does not match") {
		t.Fatalf("expected 400 does not match, got %d: %s", rrMismatch.Code, rrMismatch.Body.String())
	}
}

// Test 10: List Project Channels and Agent Bindings
func TestListProjectChannels(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	proj := "proj-list-test"
	_ = s.st.SaveProject(proj, &entity.Project{Name: proj})
	_ = s.users.CreateUser("admin", "pass", RoleAdmin, "", "", "", "", "")

	// Insert a ProjectChannelLink
	linkID := "pcl-test-list-1"
	chanID := "mm-chan-list-1"
	if err := s.controlDB.UpsertProjectChannelLink(controldb.ProjectChannelLink{
		ID:           linkID,
		WorkspaceID:  workspaceID,
		ProjectID:    proj,
		Provider:     "mattermost",
		IMInstanceID: "inst-test",
		TeamID:       "team-1",
		ChannelID:    chanID,
		ChannelName:  "dev-chan",
		DisplayName:  "#dev-chan",
		Visibility:   "private",
		Status:       "active",
	}); err != nil {
		t.Fatalf("upsert link: %v", err)
	}

	// Insert a connection for foreign key constraint
	connID := "conn-list-test"
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "mm-conn",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
	}); err != nil {
		t.Fatalf("upsert conn: %v", err)
	}

	// Insert one connected binding and one error binding
	if err := s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:             "binding-1",
		WorkspaceID:    workspaceID,
		ProjectID:      proj,
		AgentID:        "mira",
		Provider:       "mattermost",
		ConnectionID:   connID,
		ExternalChatID: chanID,
		Status:         "connected",
	}); err != nil {
		t.Fatalf("upsert binding 1: %v", err)
	}
	if err := s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:             "binding-2",
		WorkspaceID:    workspaceID,
		ProjectID:      proj,
		AgentID:        "lina",
		Provider:       "mattermost",
		ConnectionID:   connID,
		ExternalChatID: chanID,
		Status:         "error",
	}); err != nil {
		t.Fatalf("upsert binding 2: %v", err)
	}

	req := providerTestRequest(http.MethodGet, "/api/v1/projects/"+proj+"/channels", "admin", nil)
	req.SetPathValue("name", proj)
	rr := httptest.NewRecorder()
	s.handleListProjectChannels(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp listProjectChannelsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}

	if !resp.OK || len(resp.Channels) != 1 {
		t.Fatalf("expected 1 channel, got %d: %#v", len(resp.Channels), resp)
	}

	ch := resp.Channels[0]
	if ch.Link.ChannelID != chanID {
		t.Fatalf("expected channelID %s, got %s", chanID, ch.Link.ChannelID)
	}
	if !ch.HasError {
		t.Fatalf("expected ch.HasError to be true due to error binding")
	}
	if len(ch.Bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(ch.Bindings))
	}
}

