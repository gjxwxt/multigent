package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/agentdir"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
)

func setupUserIMTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	db, err := controldb.Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(t.TempDir(), "workspace")
	st := store.NewDB(root, db)
	ts := taskstore.NewDB(root, db)
	s := &Server{root: root, controlDB: db, st: st, ts: ts, users: newUserStore(db), agentDirectory: agentdir.New(db)}
	s.triggers = newTriggerManager(root, "", ts, s.controlDB)
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		t.Fatalf("workspace id: %v", err)
	}
	if err := s.controlDB.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Test Workspace",
		Slug: "test-workspace",
		Root: root,
	}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	_ = s.users.CreateUser("admin", "adminpass", RoleAdmin, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "admin", WorkspaceRoleAdmin)
	return s, workspaceID
}

func grantProjectAccessForTest(s *Server, username, project string) {
	_ = s.users.UpdateUser(username, nil, nil, nil, nil, nil, nil, nil, []projectAccess{{Project: project, Role: "member"}}, nil, nil)
}

func grantWorkerAccessForTest(s *Server, username, workerID string) {
	_ = s.users.UpdateUser(username, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []workerAccess{{WorkerID: workerID, Role: "operator"}})
}

func TestUserIMIdentities_ListAndBoundStatus(t *testing.T) {
	s, wsID := setupUserIMTestServer(t)

	// Create user alex (regular member)
	_ = s.users.CreateUser("alex", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "alex", WorkspaceRoleMember)

	// Grant alex access to project "p-shared"
	if err := s.st.SaveProject("p-shared", &entity.Project{Name: "p-shared"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	grantProjectAccessForTest(s, "alex", "p-shared")

	// Create IM Connection (Mattermost)
	connID := "conn-mm-main"
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "研发内网 Mattermost",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        wsID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"http://mm.internal:8065"}`,
	}); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}

	// Create Agent Channel Binding under p-shared
	chanID := "chan-mm-pshared"
	if err := s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           chanID,
		WorkspaceID:  wsID,
		ProjectID:    "p-shared",
		AgentID:      "lina",
		Provider:     "mattermost",
		ConnectionID: connID,
		Status:       "connected",
	}); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}

	token := s.users.IssueToken("alex", 24*time.Hour)

	// 1. Alex checks identities when UNBOUND
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	var resp userIMIdentitiesListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Connections) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp.Connections))
	}
	item := resp.Connections[0]
	if item.ID != connID || item.Name != "研发内网 Mattermost" || item.BaseURL != "http://mm.internal:8065" {
		t.Fatalf("unexpected connection info: %#v", item)
	}
	if !item.HasAccess {
		t.Fatalf("expected hasAccess=true for member of p-shared")
	}
	if item.Bound {
		t.Fatalf("expected bound=false initially")
	}

	// 2. Alex binds to this connection
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if err := s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "uch-alex",
		WorkspaceID:      wsID,
		UserID:           "alex",
		ChannelBindingID: chanID,
		Provider:         "mattermost",
		ExternalUserID:   "mm-usr-alex",
		MetadataJSON:     `{"externalUsername":"alex_in_mm"}`,
		CreatedAt:        nowStr,
	}); err != nil {
		t.Fatalf("upsert uci: %v", err)
	}

	// 3. Alex checks identities again when BOUND
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w2.Code)
	}
	var resp2 userIMIdentitiesListResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp2)
	item2 := resp2.Connections[0]
	if !item2.Bound || item2.ExternalUserID != "mm-usr-alex" || item2.ExternalUsername != "alex_in_mm" {
		t.Fatalf("expected bound=true with alex_in_mm, got: %#v", item2)
	}
}

func TestUserIMIdentities_ShowsAdminAttestedSharedBinding(t *testing.T) {
	s, workspaceID := setupUserIMTestServer(t)
	_ = s.users.CreateUser("alex", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(workspaceID, "alex", WorkspaceRoleMember)
	_ = s.st.SaveProject("p-shared", &entity.Project{Name: "p-shared"})
	grantProjectAccessForTest(s, "alex", "p-shared")

	instance := controldb.IMInstance{
		ID:          "imi-engineering",
		WorkspaceID: workspaceID,
		Provider:    "mattermost",
		DisplayName: "Engineering Mattermost",
		Attestation: imInstanceAttestationAdmin,
		CreatedBy:   "admin",
	}
	if err := s.controlDB.UpsertIMInstance(instance); err != nil {
		t.Fatalf("instance: %v", err)
	}
	for _, connectionID := range []string{"conn-mira", "conn-lina"} {
		if err := s.controlDB.UpsertConnection(controldb.Connection{
			ID:             connectionID,
			WorkspaceID:    workspaceID,
			Provider:       "mattermost",
			ConnectionName: connectionID,
			OwnerType:      ConnectionOwnerWorkspace,
			OwnerID:        workspaceID,
			AuthType:       "bot_token",
			Status:         "active",
			IMInstanceID:   instance.ID,
			ProfileJSON:    `{"baseUrl":"http://mm.internal:8065"}`,
		}); err != nil {
			t.Fatalf("connection %s: %v", connectionID, err)
		}
	}
	for _, binding := range []controldb.AgentChannelBinding{
		{ID: "binding-mira", WorkspaceID: workspaceID, ProjectID: "p-shared", AgentID: "mira", Provider: "mattermost", ConnectionID: "conn-mira", Status: "connected"},
		{ID: "binding-lina", WorkspaceID: workspaceID, ProjectID: "p-shared", AgentID: "lina", Provider: "mattermost", ConnectionID: "conn-lina", Status: "connected"},
	} {
		if err := s.controlDB.UpsertAgentChannelBinding(binding); err != nil {
			t.Fatalf("binding %s: %v", binding.ID, err)
		}
	}
	if err := s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "uch-alex-mira",
		WorkspaceID:      workspaceID,
		UserID:           "alex",
		ChannelBindingID: "binding-mira",
		Provider:         "mattermost",
		ExternalUserID:   "mm-alex",
		MetadataJSON:     `{"externalUsername":"alex"}`,
	}); err != nil {
		t.Fatalf("identity: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req.Header.Set("Authorization", "Bearer "+s.users.IssueToken("alex", time.Hour))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response userIMIdentitiesListResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]userIMConnectionResponse{}
	for _, connection := range response.Connections {
		byID[connection.ID] = connection
	}
	if !byID["conn-mira"].Bound || byID["conn-mira"].SharedBinding {
		t.Fatalf("source connection=%#v", byID["conn-mira"])
	}
	shared := byID["conn-lina"]
	if shared.Bound || !shared.SharedBinding || shared.SharedFrom != "conn-mira" || shared.ExternalUserID != "mm-alex" {
		t.Fatalf("shared connection=%#v", shared)
	}
}

func TestUserIMConnectionBindCode_AccessControl(t *testing.T) {
	s, wsID := setupUserIMTestServer(t)

	_ = s.users.CreateUser("bob", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "bob", WorkspaceRoleMember)

	// Bob only has access to "p-bob", NOT "p-secret"
	_ = s.st.SaveProject("p-secret", &entity.Project{Name: "p-secret"})
	_ = s.st.SaveProject("p-bob", &entity.Project{Name: "p-bob"})
	grantProjectAccessForTest(s, "bob", "p-bob")

	// Connection A: attached only to secret project
	connSecret := "conn-secret"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connSecret,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "Secret Mattermost",
		Status:         "active",
	})
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "chan-secret",
		WorkspaceID:  wsID,
		ProjectID:    "p-secret",
		AgentID:      "vault-bot",
		Provider:     "mattermost",
		ConnectionID: connSecret,
		Status:       "connected",
	})

	// Connection B: attached to p-bob
	connBob := "conn-bob"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connBob,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "Bob's Team Mattermost",
		Status:         "active",
	})
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "chan-bob",
		WorkspaceID:  wsID,
		ProjectID:    "p-bob",
		AgentID:      "helper-bot",
		Provider:     "mattermost",
		ConnectionID: connBob,
		Status:       "connected",
	})

	tokenBob := s.users.IssueToken("bob", 24*time.Hour)

	// 1. Bob attempts to generate bind code for secret connection -> 403 Forbidden!
	reqSecret := httptest.NewRequest(http.MethodPost, "/api/v1/user/im-identities/connections/"+connSecret+"/bind-code", nil)
	reqSecret.Header.Set("Authorization", "Bearer "+tokenBob)
	wSecret := httptest.NewRecorder()
	s.Handler().ServeHTTP(wSecret, reqSecret)

	if wSecret.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for unauthorized project connection, got %d: %s", wSecret.Code, wSecret.Body.String())
	}

	// 2. Bob generates bind code for accessible connection -> 200 OK!
	reqBob := httptest.NewRequest(http.MethodPost, "/api/v1/user/im-identities/connections/"+connBob+"/bind-code", nil)
	reqBob.Header.Set("Authorization", "Bearer "+tokenBob)
	wBob := httptest.NewRecorder()
	s.Handler().ServeHTTP(wBob, reqBob)

	if wBob.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for authorized connection, got %d: %s", wBob.Code, wBob.Body.String())
	}
	var respBob userIMBindCodeResponse
	_ = json.NewDecoder(wBob.Body).Decode(&respBob)
	if !strings.HasPrefix(respBob.Code, "MG-") || !strings.HasPrefix(respBob.Command, "/mg bind MG-") {
		t.Fatalf("unexpected bind code response: %#v", respBob)
	}

	// Verify bind code was created in DB anchored to chan-bob
	codeRow, found, err := s.controlDB.AgentChannelBindCodeByCode(respBob.Code)
	if err != nil || !found {
		t.Fatalf("bind code not found in DB: %v", err)
	}
	if codeRow.ChannelBindingID != "chan-bob" || codeRow.UserID != "bob" {
		t.Fatalf("unexpected binding anchor: %#v", codeRow)
	}
}

func TestUserIMConnectionUnbind_Transactional(t *testing.T) {
	s, wsID := setupUserIMTestServer(t)

	_ = s.users.CreateUser("charlie", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "charlie", WorkspaceRoleMember)
	_ = s.st.SaveProject("p-charlie", &entity.Project{Name: "p-charlie"})
	grantProjectAccessForTest(s, "charlie", "p-charlie")

	connID := "conn-charlie"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "Charlie MM",
		Status:         "active",
	})
	chanID := "chan-charlie"
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           chanID,
		WorkspaceID:  wsID,
		ProjectID:    "p-charlie",
		AgentID:      "bot-c",
		Provider:     "mattermost",
		ConnectionID: connID,
		Status:       "connected",
	})

	// Charlie is bound
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "uch-c",
		WorkspaceID:      wsID,
		UserID:           "charlie",
		ChannelBindingID: chanID,
		Provider:         "mattermost",
		ExternalUserID:   "mm-charlie",
	})
	_ = s.controlDB.UpsertExternalIdentity(controldb.ExternalIdentity{
		ID:             "ext-c",
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ExternalUserID: "mm-charlie",
		UserID:         "charlie",
	})

	// Charlie has an unused bind code
	_ = s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             "MG-OLDCODE",
		WorkspaceID:      wsID,
		ChannelBindingID: chanID,
		UserID:           "charlie",
		TargetType:       "user",
		ExpiresAt:        time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})

	tokenCharlie := s.users.IssueToken("charlie", 24*time.Hour)

	// Call DELETE /api/v1/user/im-identities/connections/{connectionId}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/user/im-identities/connections/"+connID, nil)
	req.Header.Set("Authorization", "Bearer "+tokenCharlie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	// Verify user_channel_identities deleted
	list, _ := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID: wsID,
		UserID:      "charlie",
	})
	if len(list) != 0 {
		t.Fatalf("expected 0 user channel identities, got %d", len(list))
	}

	// Verify external_identity deleted
	_, extFound, _ := s.controlDB.ExternalIdentityByExternalID(wsID, "mattermost", "mm-charlie")
	if extFound {
		t.Fatalf("expected external identity to be deleted")
	}

	// Verify old bind code invalidated
	codeRow, _, _ := s.controlDB.AgentChannelBindCodeByCode("MG-OLDCODE")
	if codeRow.UsedAt == "" {
		t.Fatalf("expected old bind code to be invalidated with UsedAt set")
	}
}

func TestUserIMIdentities_VisibilityRules_ABC(t *testing.T) {
	s, wsID := setupUserIMTestServer(t)

	// Create users
	_ = s.users.CreateUser("user-proj", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "user-proj", WorkspaceRoleMember)

	_ = s.users.CreateUser("user-worker", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "user-worker", WorkspaceRoleMember)

	_ = s.users.CreateUser("user-orphan", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "user-orphan", WorkspaceRoleMember)

	_ = s.users.CreateUser("user-outsider", "pass", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "user-outsider", WorkspaceRoleMember)

	// Projects and workers
	_ = s.st.SaveProject("proj-alpha", &entity.Project{Name: "proj-alpha"})
	grantProjectAccessForTest(s, "user-proj", "proj-alpha")

	workerID := "worker-lead"
	_ = s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:          workerID,
		WorkspaceID: wsID,
		Name:        "Lead Agent Worker",
	})
	grantWorkerAccessForTest(s, "user-worker", workerID)

	// Connection 1: attached to proj-alpha
	conn1 := "conn-alpha"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             conn1,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "Alpha Mattermost",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"http://mm.alpha:8065"}`,
	})
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "binding-alpha",
		WorkspaceID:  wsID,
		ProjectID:    "proj-alpha",
		AgentID:      "mira",
		Provider:     "mattermost",
		ConnectionID: conn1,
		Status:       "connected",
	})

	// Connection 2: attached to worker-lead
	conn2 := "conn-beta"
	_ = s.controlDB.UpsertConnection(controldb.Connection{
		ID:             conn2,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "Beta Mattermost",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"http://mm.beta:8065"}`,
	})
	_ = s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:            "binding-beta",
		WorkspaceID:   wsID,
		AgentWorkerID: workerID,
		Provider:      "mattermost",
		ConnectionID:  conn2,
		Status:        "connected",
	})

	// User orphan bound to conn1 in the past, but has no project access
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "uch-orphan",
		WorkspaceID:      wsID,
		UserID:           "user-orphan",
		ChannelBindingID: "binding-alpha",
		Provider:         "mattermost",
		ExternalUserID:   "mm-orphan",
		MetadataJSON:     `{"externalUsername":"orphan_mm"}`,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	})

	// 1. user-proj (Rule A: Project access -> sees conn1 with visible routes, excludes conn2)
	tokenProj := s.users.IssueToken("user-proj", time.Hour)
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req1.Header.Set("Authorization", "Bearer "+tokenProj)
	w1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("user-proj: expected 200, got %d", w1.Code)
	}
	var resp1 userIMIdentitiesListResponse
	_ = json.NewDecoder(w1.Body).Decode(&resp1)
	if len(resp1.Connections) != 1 {
		t.Fatalf("user-proj: expected exactly 1 connection, got %d", len(resp1.Connections))
	}
	if resp1.Connections[0].ID != conn1 {
		t.Errorf("user-proj: expected %s, got %s", conn1, resp1.Connections[0].ID)
	}
	if !resp1.Connections[0].HasAccess || len(resp1.Connections[0].Routes) != 1 {
		t.Fatalf("user-proj: expected hasAccess=true with 1 route, got %#v", resp1.Connections[0])
	}
	if resp1.Connections[0].Routes[0].AgentID != "mira" || resp1.Connections[0].Routes[0].ProjectID != "proj-alpha" {
		t.Errorf("user-proj: unexpected route: %#v", resp1.Connections[0].Routes[0])
	}

	// 2. user-worker (Rule A: Dual-path AgentWorker access -> sees conn2, excludes conn1)
	tokenWorker := s.users.IssueToken("user-worker", time.Hour)
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req2.Header.Set("Authorization", "Bearer "+tokenWorker)
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("user-worker: expected 200, got %d", w2.Code)
	}
	var resp2 userIMIdentitiesListResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp2)
	if len(resp2.Connections) != 1 {
		t.Fatalf("user-worker: expected exactly 1 connection, got %d", len(resp2.Connections))
	}
	if resp2.Connections[0].ID != conn2 {
		t.Errorf("user-worker: expected %s, got %s", conn2, resp2.Connections[0].ID)
	}
	if !resp2.Connections[0].HasAccess || len(resp2.Connections[0].Routes) != 1 {
		t.Fatalf("user-worker: expected hasAccess=true with 1 route, got %#v", resp2.Connections[0])
	}
	if resp2.Connections[0].Routes[0].AgentWorkerID != workerID {
		t.Errorf("user-worker: unexpected route: %#v", resp2.Connections[0].Routes[0])
	}

	// 3. user-orphan (Rule B: Orphan protection -> sees conn1 with bound=true, hasAccess=false, routes=empty)
	tokenOrphan := s.users.IssueToken("user-orphan", time.Hour)
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req3.Header.Set("Authorization", "Bearer "+tokenOrphan)
	w3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("user-orphan: expected 200, got %d", w3.Code)
	}
	var resp3 userIMIdentitiesListResponse
	_ = json.NewDecoder(w3.Body).Decode(&resp3)
	if len(resp3.Connections) != 1 {
		t.Fatalf("user-orphan: expected 1 connection (orphan entry), got %d", len(resp3.Connections))
	}
	if resp3.Connections[0].ID != conn1 {
		t.Errorf("user-orphan: expected %s, got %s", conn1, resp3.Connections[0].ID)
	}
	if !resp3.Connections[0].Bound || resp3.Connections[0].HasAccess || len(resp3.Connections[0].Routes) != 0 {
		t.Fatalf("user-orphan: expected bound=true, hasAccess=false, routes=0; got %#v", resp3.Connections[0])
	}

	// 4. user-outsider (Rule C: Unbound and no visible routes -> completely excluded)
	tokenOutsider := s.users.IssueToken("user-outsider", time.Hour)
	req4 := httptest.NewRequest(http.MethodGet, "/api/v1/user/im-identities", nil)
	req4.Header.Set("Authorization", "Bearer "+tokenOutsider)
	w4 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("user-outsider: expected 200, got %d", w4.Code)
	}
	var resp4 userIMIdentitiesListResponse
	_ = json.NewDecoder(w4.Body).Decode(&resp4)
	if len(resp4.Connections) != 0 {
		t.Fatalf("user-outsider: expected 0 connections (Rule C anti-leakage), got %d", len(resp4.Connections))
	}
}
