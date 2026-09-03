package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/agentdir"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/rbac"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
)

func newConnectionGrantPolicyServer(t *testing.T) (*Server, string) {
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
	if err := s.users.CreateUser("owner", "pass123", RoleMember, "", "", "", "", ""); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := s.controlDB.UpsertWorkspaceMember(workspaceID, "admin", WorkspaceRoleAdmin); err != nil {
		t.Fatalf("admin member: %v", err)
	}
	if err := s.controlDB.UpsertWorkspaceMember(workspaceID, "owner", WorkspaceRoleMember); err != nil {
		t.Fatalf("owner member: %v", err)
	}
	if err := s.users.UpdateUser("owner", nil, nil, nil, nil, nil, nil, nil, nil, []agentAccess{{Project: "sample", Agent: "pm", Role: string(rbac.AgentRoleOwner)}}, nil); err != nil {
		t.Fatalf("link owner agent: %v", err)
	}
	if err := st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	return s, workspaceID
}

func seedSampleAgentsForTest(t *testing.T, s *Server, workspaceID string) {
	t.Helper()
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "pm", "aw-pm", "pm-sample-pm")
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "backend", "aw-backend", "pm-sample-backend")
}

func TestUserOwnedConnectionGrantTargetsAreLimitedToOwnerAndLinkedAgents(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	connection := controldb.Connection{
		ID:             "conn-owner",
		WorkspaceID:    workspaceID,
		Provider:       "github",
		ConnectionName: "default",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
	}
	req := httptest.NewRequest("POST", "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "owner"))

	allowed := []struct {
		targetType string
		targetID   string
	}{
		{ConnectionTargetUser, "owner"},
		{ConnectionTargetAgent, "agent_worker:aw-pm"},
	}
	for _, tc := range allowed {
		if err := s.validateConnectionGrantTarget(req, connection, tc.targetType, tc.targetID); err != nil {
			t.Fatalf("expected %s/%s to be allowed: %v", tc.targetType, tc.targetID, err)
		}
	}

	blocked := []struct {
		targetType string
		targetID   string
	}{
		{ConnectionTargetWorkspace, workspaceID},
		{ConnectionTargetProject, "sample"},
		{ConnectionTargetUser, "admin"},
		{ConnectionTargetAgent, "agent_worker:aw-backend"},
	}
	for _, tc := range blocked {
		if err := s.validateConnectionGrantTarget(req, connection, tc.targetType, tc.targetID); err == nil {
			t.Fatalf("expected %s/%s to be blocked", tc.targetType, tc.targetID)
		}
	}
}

func TestUserOwnedConnectionGrantMustBeCreatedByOwner(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	connection := controldb.Connection{
		ID:             "conn-owner",
		WorkspaceID:    workspaceID,
		Provider:       "github",
		ConnectionName: "default",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "owner",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}

	adminRec := httptest.NewRecorder()
	adminReq := providerTestRequest(http.MethodPost, "/api/v1/connections/conn-owner/grants", "admin", createConnectionGrantRequest{
		TargetType: ConnectionTargetAgent,
		TargetID:   "agent_worker:aw-pm",
	})
	adminReq.SetPathValue("id", "conn-owner")
	s.handleCreateConnectionGrant(adminRec, adminReq)
	if adminRec.Code != http.StatusForbidden {
		t.Fatalf("admin grant status=%d body=%s", adminRec.Code, adminRec.Body.String())
	}

	ownerRec := httptest.NewRecorder()
	ownerReq := providerTestRequest(http.MethodPost, "/api/v1/connections/conn-owner/grants", "owner", createConnectionGrantRequest{
		TargetType: ConnectionTargetAgent,
		TargetID:   "agent_worker:aw-pm",
	})
	ownerReq.SetPathValue("id", "conn-owner")
	s.handleCreateConnectionGrant(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusCreated {
		t.Fatalf("owner grant status=%d body=%s", ownerRec.Code, ownerRec.Body.String())
	}
}

func TestUserOwnedConnectionCanGrantToOperatedProjectAgents(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	grantProjectRoleForTest(t, s, workspaceID, "operator", ProjectRoleOperator)
	grantProjectRoleForTest(t, s, workspaceID, "viewer", ProjectRoleViewer)
	for _, username := range []string{"operator", "viewer"} {
		connection := controldb.Connection{
			ID:             "conn-" + username,
			WorkspaceID:    workspaceID,
			Provider:       "github",
			ConnectionName: "default",
			OwnerType:      ConnectionOwnerUser,
			OwnerID:        username,
			AuthType:       ConnectionAuthAPIKey,
			Status:         "active",
			ProfileJSON:    `{}`,
			CreatedBy:      username,
		}
		if err := s.controlDB.UpsertConnection(connection); err != nil {
			t.Fatalf("connection %s: %v", username, err)
		}
	}

	operatorRec := httptest.NewRecorder()
	operatorReq := providerTestRequest(http.MethodPost, "/api/v1/connections/conn-operator/grants", "operator", createConnectionGrantRequest{
		TargetType: ConnectionTargetAgent,
		TargetID:   "agent_worker:aw-backend",
	})
	operatorReq.SetPathValue("id", "conn-operator")
	s.handleCreateConnectionGrant(operatorRec, operatorReq)
	if operatorRec.Code != http.StatusCreated {
		t.Fatalf("operator grant status=%d body=%s", operatorRec.Code, operatorRec.Body.String())
	}

	viewerRec := httptest.NewRecorder()
	viewerReq := providerTestRequest(http.MethodPost, "/api/v1/connections/conn-viewer/grants", "viewer", createConnectionGrantRequest{
		TargetType: ConnectionTargetAgent,
		TargetID:   "agent_worker:aw-backend",
	})
	viewerReq.SetPathValue("id", "conn-viewer")
	s.handleCreateConnectionGrant(viewerRec, viewerReq)
	if viewerRec.Code != http.StatusBadRequest {
		t.Fatalf("viewer grant status=%d body=%s", viewerRec.Code, viewerRec.Body.String())
	}
}

// A project manager can install a workspace connection into their project
// (handleInstallProjectToolBindings only requires project-manager access), so
// deleting the same project-scoped grant must stay symmetric. Without this a
// project manager could connect a tool but never uninstall it — and the
// settings-page uninstall would half-fail: bindings deleted, grant stuck.
func TestProjectManagerCanDeleteProjectGrantOnWorkspaceConnection(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	connection := controldb.Connection{
		ID:             "conn-gl",
		WorkspaceID:    workspaceID,
		Provider:       "gitlab",
		ConnectionName: "default",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "admin",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	seedSeq := 0
	seedProjectGrant := func(t *testing.T, connectionID, project string) string {
		t.Helper()
		seedSeq++
		grant := controldb.ConnectionGrant{
			ID:           fmt.Sprintf("grant-%s-%d", project, seedSeq),
			WorkspaceID:  workspaceID,
			ConnectionID: connectionID,
			TargetType:   ConnectionTargetProject,
			TargetID:     project,
			CreatedBy:    "admin",
			CreatedAt:    "2026-08-31T00:00:00Z",
		}
		if err := s.controlDB.CreateConnectionGrant(grant); err != nil {
			t.Fatalf("create grant: %v", err)
		}
		return grant.ID
	}

	// pm manages "sample" only; sample2 exists but is not managed by them.
	if err := st_SaveProjectForTest(s, "sample"); err != nil {
		t.Fatalf("save sample: %v", err)
	}
	if err := st_SaveProjectForTest(s, "sample2"); err != nil {
		t.Fatalf("save sample2: %v", err)
	}
	grantProjectRoleForTest(t, s, workspaceID, "pm", ProjectRoleManager)
	grantProjectRoleForTest(t, s, workspaceID, "operator", ProjectRoleOperator)

	deleteGrant := func(username, grantID, connectionID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodDelete, "/api/v1/connections/"+connectionID+"/grants/"+grantID, username, nil)
		req.SetPathValue("id", connectionID)
		req.SetPathValue("grantId", grantID)
		s.handleDeleteConnectionGrant(rec, req)
		return rec
	}

	// Project manager may revoke the project-scoped grant on their project.
	sampleGrant := seedProjectGrant(t, "conn-gl", "sample")
	if rec := deleteGrant("pm", sampleGrant, "conn-gl"); rec.Code != http.StatusOK {
		t.Fatalf("pm delete project grant status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Same role, different project: must stay forbidden.
	sample2Grant := seedProjectGrant(t, "conn-gl", "sample2")
	if rec := deleteGrant("pm", sample2Grant, "conn-gl"); rec.Code != http.StatusForbidden {
		t.Fatalf("pm delete other-project grant status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Operator is below manager level: forbidden.
	sampleGrant2 := seedProjectGrant(t, "conn-gl", "sample")
	if rec := deleteGrant("operator", sampleGrant2, "conn-gl"); rec.Code != http.StatusForbidden {
		t.Fatalf("operator delete grant status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Workspace-targeted grants are infrastructure-level: project manager must
	// not be able to revoke them even for a managed project.
	wsGrant := controldb.ConnectionGrant{
		ID:           "grant-ws",
		WorkspaceID:  workspaceID,
		ConnectionID: "conn-gl",
		TargetType:   ConnectionTargetWorkspace,
		TargetID:     workspaceID,
		CreatedBy:    "admin",
		CreatedAt:    "2026-08-31T00:00:00Z",
	}
	if err := s.controlDB.CreateConnectionGrant(wsGrant); err != nil {
		t.Fatalf("create ws grant: %v", err)
	}
	if rec := deleteGrant("pm", "grant-ws", "conn-gl"); rec.Code != http.StatusForbidden {
		t.Fatalf("pm delete workspace grant status=%d body=%s", rec.Code, rec.Body.String())
	}

	// User-owned connections keep the strict rule entirely.
	userConn := connection
	userConn.ID = "conn-user"
	userConn.OwnerType = ConnectionOwnerUser
	userConn.OwnerID = "owner"
	if err := s.controlDB.UpsertConnection(userConn); err != nil {
		t.Fatalf("user conn: %v", err)
	}
	userGrant := seedProjectGrant(t, "conn-user", "sample")
	if rec := deleteGrant("pm", userGrant, "conn-user"); rec.Code != http.StatusForbidden {
		t.Fatalf("pm delete user-connection grant status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func st_SaveProjectForTest(s *Server, name string) error {
	return s.st.SaveProject(name, &entity.Project{Name: name})
}
