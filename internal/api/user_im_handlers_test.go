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
