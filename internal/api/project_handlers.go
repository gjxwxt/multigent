package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/scaffold"
)

type createProjectBody struct {
	Name            string                          `json:"name"`
	Description     string                          `json:"description"`
	Repo            string                          `json:"repo"`
	Owners          []string                        `json:"owners"`
	WorkerIDs       []string                        `json:"workerIds,omitempty"`
	MemberUsernames []string                        `json:"memberUsernames,omitempty"`
	Channel         *projectChannelProvisionRequest `json:"channel,omitempty"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	var body createProjectBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if err := validateWorkspaceObjectName("project", name); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}
	if _, err := s.st.Project(name); err == nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, fmt.Sprintf("project %q already exists", name))
		return
	} else if !isNotFoundErr(err) {
		s.serverError(w, err)
		return
	}

	workspaceID, _ := s.currentWorkspaceForRequest(w, r)

	p := &entity.Project{
		Name:        name,
		Description: strings.TrimSpace(body.Description),
		Repo:        strings.TrimSpace(body.Repo),
		Owners:      body.Owners,
	}
	if err := scaffold.New(s.st).CreateProject(name, p); err != nil {
		s.serverError(w, err)
		return
	}
	cfg := &entity.ProjectConfig{
		Name:        name,
		Description: p.Description,
		Repo:        p.Repo,
		Owners:      p.Owners,
		Agents:      []entity.AgentSpec{},
	}
	if err := s.ts.SaveProjectConfig(name, cfg); err != nil {
		_ = s.st.DeleteProject(name)
		s.serverError(w, err)
		return
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)

	// 1. Assign agent worker memberships if specified
	if s.controlDB != nil && workspaceID != "" && len(body.WorkerIDs) > 0 {
		for _, workerRef := range body.WorkerIDs {
			workerRef = strings.TrimSpace(workerRef)
			if workerRef == "" {
				continue
			}
			worker, ok, err := s.agentDirectory.Worker(workspaceID, workerRef)
			if err != nil || !ok {
				continue
			}
			title := worker.DisplayName
			if title == "" {
				title = worker.Name
			}
			role := "member"
			lower := strings.ToLower(title)
			if strings.Contains(lower, "mira") || strings.Contains(lower, "coder") || strings.Contains(lower, "dev") {
				role = "developer"
			} else if strings.Contains(lower, "lina") || strings.Contains(lower, "review") {
				role = "reviewer"
			}

			membership := controldb.ProjectMembership{
				ID:               "pm_" + randomHex(12),
				WorkspaceID:      workspaceID,
				ProjectID:        name,
				MemberType:       "agent_worker",
				MemberID:         worker.ID,
				Role:             role,
				Title:            title,
				AutoPickTasks:    true,
				AttentionEnabled: true,
				PriorityWeight:   1,
				CreatedAt:        nowStr,
				UpdatedAt:        nowStr,
			}
			_ = s.controlDB.UpsertProjectMembership(membership)
		}
	}

	// 2. Grant project access to selected workspace users
	if s.users != nil && len(body.MemberUsernames) > 0 {
		for _, username := range body.MemberUsernames {
			username = strings.TrimSpace(username)
			if username == "" {
				continue
			}
			targetUser := s.users.GetUser(username)
			if targetUser == nil {
				continue
			}
			hasAccess := false
			for _, prj := range targetUser.Projects {
				if prj.Project == name {
					hasAccess = true
					break
				}
			}
			if !hasAccess {
				newProjects := append(targetUser.Projects, projectAccess{Project: name, Role: "member"})
				_ = s.users.UpdateUser(username, nil, nil, nil, nil, nil, nil, nil, newProjects, nil, nil, nil)
			}
		}
	}

	// 3. Provision ChatOps Channel if requested
	var channelResp *projectChannelProvisionResponse
	if body.Channel != nil && s.controlDB != nil && workspaceID != "" {
		cResp, pErr := s.provisionProjectChannelCore(r.Context(), workspaceID, name, *body.Channel, requestUsername(r))
		if pErr != nil {
			// Rollback newly created project on channel error so caller can correct input and retry cleanly
			_ = s.st.DeleteProject(name)
			_ = s.controlDB.DeleteProjectMembershipsByProject(workspaceID, name)
			_ = s.controlDB.DeleteProjectChannelLinks(workspaceID, name)
			_ = s.controlDB.DeleteAgentChannelBindingsByProject(workspaceID, name)
			if pErr.Code != "" {
				s.jsonErrorCode(w, pErr.StatusCode, pErr.Code, pErr.Message)
			} else {
				s.jsonError(w, pErr.StatusCode, pErr.Message)
			}
			return
		}
		channelResp = cResp
	}

	s.auditLog(auditLogInput{
		Action:       "project.create",
		ResourceType: "project",
		ResourceID:   name,
		Summary:      "Project created",
		After: map[string]any{
			"name":        name,
			"description": p.Description,
			"repo":        p.Repo,
		},
		Request: r,
	})
	w.WriteHeader(http.StatusCreated)
	respMap := map[string]any{
		"ok":      true,
		"project": name,
	}
	if channelResp != nil {
		respMap["channel"] = channelResp
	}
	_ = json.NewEncoder(w).Encode(respMap)
}

func validateWorkspaceObjectName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if len(name) > 80 {
		return fmt.Errorf("%s name must be 80 characters or fewer", kind)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("%s name may only contain letters, numbers, '.', '_' and '-'", kind)
	}
	if strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		return fmt.Errorf("%s name cannot start with '.' or contain '..'", kind)
	}
	return nil
}
