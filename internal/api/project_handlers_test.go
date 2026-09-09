package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
)

func TestHandleCreateProject_Validation(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	tests := []struct {
		name        string
		projectName string
		wantCode    int
		wantSubstr  string
	}{
		{
			name:        "chinese name rejected",
			projectName: "变更申请台账",
			wantCode:    http.StatusBadRequest,
			wantSubstr:  "may only contain letters, numbers",
		},
		{
			name:        "space in name rejected",
			projectName: "order center",
			wantCode:    http.StatusBadRequest,
			wantSubstr:  "may only contain letters, numbers",
		},
		{
			name:        "dot prefix rejected",
			projectName: ".hidden-project",
			wantCode:    http.StatusBadRequest,
			wantSubstr:  "cannot start with '.'",
		},
		{
			name:        "empty name rejected",
			projectName: "",
			wantCode:    http.StatusBadRequest,
			wantSubstr:  "project name is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := createProjectBody{
				Name:        tc.projectName,
				Description: "some description",
			}
			req := providerTestRequest(http.MethodPost, "/api/v1/projects", "admin", body)

			w := httptest.NewRecorder()
			s.handleCreateProject(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("expected status %d, got %d: %s", tc.wantCode, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantSubstr) {
				t.Fatalf("expected error body to contain %q, got %s", tc.wantSubstr, w.Body.String())
			}
		})
	}
}

func TestHandleCreateProject_UnifiedPayload(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	// Create test agent worker
	worker := controldb.AgentWorker{
		ID:          "aw_mira_test",
		WorkspaceID: workspaceID,
		Name:        "Mira",
		DisplayName: "Mira (Developer)",
		Role:        "developer",
		Status:      "active",
	}
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("upsert agent worker: %v", err)
	}

	// Create test workspace user
	if err := s.users.CreateUser("alex", "pass123", RoleMember, "", "", "", "", ""); err != nil {
		t.Fatalf("create user alex: %v", err)
	}

	projectName := "pilot-change-request"
	body := createProjectBody{
		Name:            projectName,
		Description:     "变更申请台账 MVP",
		WorkerIDs:       []string{worker.ID},
		MemberUsernames: []string{"alex"},
	}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects", "admin", body)

	w := httptest.NewRecorder()
	s.handleCreateProject(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	// 1. Verify project entity created in store
	p, err := s.st.Project(projectName)
	if err != nil {
		t.Fatalf("expected project %s in store: %v", projectName, err)
	}
	if p.Description != "变更申请台账 MVP" {
		t.Fatalf("expected description %q, got %q", "变更申请台账 MVP", p.Description)
	}

	// 2. Verify agent worker membership in controldb
	memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectName,
		MemberType:  "agent_worker",
	})
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if len(memberships) != 1 || memberships[0].MemberID != worker.ID {
		t.Fatalf("expected 1 membership for worker %s, got: %+v", worker.ID, memberships)
	}

	// 3. Verify user was granted project access
	alexUser := s.users.GetUser("alex")
	if alexUser == nil {
		t.Fatalf("alex not found")
	}
	foundAlexAccess := false
	for _, prj := range alexUser.Projects {
		if prj.Project == projectName {
			foundAlexAccess = true
			break
		}
	}
	if !foundAlexAccess {
		t.Fatalf("expected alex to have project access for %s, got: %+v", projectName, alexUser.Projects)
	}
}

func TestHandleCreateProject_WithChannelProvisionAndRollback(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	// Mock Mattermost server that always fails with conflict
	mockMM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{
				{ID: "team-mm-1", Name: "team-main", DisplayName: "Main Team"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "store.sql_channel.saved.create.app_error",
				"message": "A channel with that name already exists on that team",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockMM.Close()

	// Register Mattermost connection
	connID := "conn-mm-create-fail-test"
	conn := controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "Test MM",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"botId":"bot-fail-1","baseUrl":"` + mockMM.URL + `"}`,
		IMInstanceID:   "inst-test-1",
	}
	if err := s.controlDB.UpsertConnection(conn); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{
		"baseUrl":  mockMM.URL,
		"botToken": "test-bot-token",
		"appId":    "bot-fail-1",
	})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("upsert connection secret: %v", err)
	}

	projectName := "project-channel-fail"
	body := createProjectBody{
		Name:        projectName,
		Description: "Should rollback on channel conflict",
		Channel: &projectChannelProvisionRequest{
			Provider:    "mattermost",
			Mode:        "create",
			ChannelName: "existing-channel",
			Visibility:  "private",
		},
	}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects", "admin", body)

	w := httptest.NewRecorder()
	s.handleCreateProject(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", w.Code, w.Body.String())
	}

	// Verify project was ROLLED BACK and does NOT exist
	if _, err := s.st.Project(projectName); err == nil {
		t.Fatalf("expected project %s to be rolled back on channel failure, but it exists!", projectName)
	}
}
