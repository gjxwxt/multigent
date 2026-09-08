package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
)

type userIMConnectionResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Provider         string `json:"provider"`
	ProviderLabel    string `json:"providerLabel"`
	Status           string `json:"status"`
	BaseURL          string `json:"baseUrl,omitempty"`
	HasAccess        bool   `json:"hasAccess"`
	Bound            bool   `json:"bound"`
	ExternalUserID   string `json:"externalUserId,omitempty"`
	ExternalUsername string `json:"externalUsername,omitempty"`
	BoundAt          string `json:"boundAt,omitempty"`
}

type userIMIdentitiesListResponse struct {
	Connections []userIMConnectionResponse `json:"connections"`
}

type userIMBindCodeResponse struct {
	Code           string `json:"code"`
	Command        string `json:"command"`
	ExpiresAt      string `json:"expiresAt"`
	Provider       string `json:"provider"`
	ConnectionID   string `json:"connectionId"`
	ConnectionName string `json:"connectionName"`
}

func (s *Server) handleUserIMIdentities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	u := s.currentUser(r)
	if u == nil || strings.TrimSpace(u.Username) == "" {
		s.jsonError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}

	conns, err := s.controlDB.ListConnections(controldb.ConnectionFilter{
		WorkspaceID: workspaceID,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}

	// Filter connections to IM providers
	var imConns []controldb.Connection
	for _, conn := range conns {
		if _, isIM := imbridge.LookupProvider(conn.Provider); isIM {
			imConns = append(imConns, conn)
		}
	}

	// Fetch all connected bindings in this workspace
	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		Status:      "connected",
	})
	if err != nil {
		s.serverError(w, err)
		return
	}

	bindingsByConn := make(map[string][]controldb.AgentChannelBinding)
	for _, b := range bindings {
		bindingsByConn[b.ConnectionID] = append(bindingsByConn[b.ConnectionID], b)
	}

	// Fetch user's channel identities
	userChannelIdentities, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID: workspaceID,
		UserID:      u.Username,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}

	boundByBindingID := make(map[string]controldb.UserChannelIdentity)
	for _, uci := range userChannelIdentities {
		boundByBindingID[uci.ChannelBindingID] = uci
	}

	isWorkspaceAdmin := u.Role == RoleAdmin || s.canAdminCurrentWorkspace(r)

	out := make([]userIMConnectionResponse, 0, len(imConns))
	for _, conn := range imConns {
		providerInfo, _ := imbridge.LookupProvider(conn.Provider)
		label := conn.Provider
		if providerInfo != nil {
			label = providerInfo.Info().Label
		}

		connBindings := bindingsByConn[conn.ID]

		// Check if user has access to at least one project/agent worker using this connection
		hasAccess := isWorkspaceAdmin
		if !hasAccess {
			for _, b := range connBindings {
				if b.ProjectID != "" && s.canAccessProject(r, b.ProjectID) {
					hasAccess = true
					break
				}
				if b.AgentWorkerID != "" {
					worker, found, _ := s.controlDB.AgentWorkerByID(workspaceID, b.AgentWorkerID)
					if found {
						if allowed, _ := s.canAccessAgentWorkerForRequest(r, workspaceID, worker); allowed {
							hasAccess = true
							break
						}
					}
				}
			}
		}

		// Check if user is bound to this connection
		bound := false
		var boundIdentity controldb.UserChannelIdentity
		for _, b := range connBindings {
			if uci, ok := boundByBindingID[b.ID]; ok {
				bound = true
				boundIdentity = uci
				break
			}
		}

		var extUsername string
		if bound && boundIdentity.MetadataJSON != "" {
			var meta map[string]any
			if err := json.Unmarshal([]byte(boundIdentity.MetadataJSON), &meta); err == nil {
				if uname, ok := meta["externalUsername"].(string); ok && uname != "" {
					extUsername = uname
				}
			}
		}

		var baseURL string
		if conn.ProfileJSON != "" {
			var profile map[string]any
			if err := json.Unmarshal([]byte(conn.ProfileJSON), &profile); err == nil {
				if bu, ok := profile["baseUrl"].(string); ok {
					baseURL = bu
				}
			}
		}

		displayName := strings.TrimSpace(conn.ConnectionName)
		if displayName == "" || displayName == "default" {
			displayName = label
		}

		out = append(out, userIMConnectionResponse{
			ID:               conn.ID,
			Name:             displayName,
			Provider:         conn.Provider,
			ProviderLabel:    label,
			Status:           conn.Status,
			BaseURL:          baseURL,
			HasAccess:        hasAccess,
			Bound:            bound,
			ExternalUserID:   boundIdentity.ExternalUserID,
			ExternalUsername: extUsername,
			BoundAt:          boundIdentity.CreatedAt,
		})
	}

	_ = json.NewEncoder(w).Encode(userIMIdentitiesListResponse{Connections: out})
}

func (s *Server) handleUserIMConnectionBindCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	u := s.currentUser(r)
	if u == nil || strings.TrimSpace(u.Username) == "" {
		s.jsonError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}

	connectionID := strings.TrimSpace(r.PathValue("connectionId"))
	if connectionID == "" {
		s.jsonError(w, http.StatusBadRequest, "connectionId is required")
		return
	}

	conn, found, err := s.controlDB.ConnectionByID(connectionID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || conn.WorkspaceID != workspaceID {
		s.jsonError(w, http.StatusNotFound, "connection not found")
		return
	}

	if _, isIM := imbridge.LookupProvider(conn.Provider); !isIM {
		s.jsonError(w, http.StatusBadRequest, "connection is not an IM provider")
		return
	}

	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID:  workspaceID,
		ConnectionID: connectionID,
		Status:       "connected",
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(bindings) == 0 {
		s.jsonError(w, http.StatusBadRequest, "no active agent channels configured for this connection")
		return
	}

	isWorkspaceAdmin := u.Role == RoleAdmin || s.canAdminCurrentWorkspace(r)
	var selectedBinding *controldb.AgentChannelBinding
	for i := range bindings {
		b := &bindings[i]
		if isWorkspaceAdmin {
			selectedBinding = b
			break
		}
		if b.ProjectID != "" && s.canAccessProject(r, b.ProjectID) {
			selectedBinding = b
			break
		}
		if b.AgentWorkerID != "" {
			worker, found, _ := s.controlDB.AgentWorkerByID(workspaceID, b.AgentWorkerID)
			if found {
				if allowed, _ := s.canAccessAgentWorkerForRequest(r, workspaceID, worker); allowed {
					selectedBinding = b
					break
				}
			}
		}
	}

	if selectedBinding == nil {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "you do not have access to any projects or agents using this connection")
		return
	}

	code, err := newHumanBindCode()
	if err != nil {
		s.serverError(w, err)
		return
	}

	now := time.Now().UTC()
	expiresAt := now.Add(10 * time.Minute).Format(time.RFC3339)
	if err := s.controlDB.CreateAgentChannelBindCode(controldb.AgentChannelBindCode{
		Code:             code,
		WorkspaceID:      workspaceID,
		ChannelBindingID: selectedBinding.ID,
		UserID:           u.Username,
		TargetType:       "user",
		ExpiresAt:        expiresAt,
		CreatedAt:        now.Format(time.RFC3339),
	}); err != nil {
		s.serverError(w, err)
		return
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		ActorType:    "user",
		ActorID:      u.Username,
		Action:       "user.im_bind_code_created",
		ResourceType: "connection",
		ResourceID:   connectionID,
		Summary:      fmt.Sprintf("User %s generated IM bind code for connection %s (%s)", u.Username, conn.ConnectionName, conn.Provider),
		Request:      r,
	})

	displayName := strings.TrimSpace(conn.ConnectionName)
	if displayName == "" || displayName == "default" {
		displayName = conn.Provider
	}

	_ = json.NewEncoder(w).Encode(userIMBindCodeResponse{
		Code:           code,
		Command:        fmt.Sprintf("/mg bind %s", code),
		ExpiresAt:      expiresAt,
		Provider:       conn.Provider,
		ConnectionID:   conn.ID,
		ConnectionName: displayName,
	})
}

func (s *Server) handleUserIMConnectionUnbind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	u := s.currentUser(r)
	if u == nil || strings.TrimSpace(u.Username) == "" {
		s.jsonError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}

	connectionID := strings.TrimSpace(r.PathValue("connectionId"))
	if connectionID == "" {
		s.jsonError(w, http.StatusBadRequest, "connectionId is required")
		return
	}

	conn, found, err := s.controlDB.ConnectionByID(connectionID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || conn.WorkspaceID != workspaceID {
		s.jsonError(w, http.StatusNotFound, "connection not found")
		return
	}

	if err := s.controlDB.UnbindUserConnection(workspaceID, u.Username, connectionID); err != nil {
		s.serverError(w, err)
		return
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		ActorType:    "user",
		ActorID:      u.Username,
		Action:       "user.im_connection_unbound",
		ResourceType: "connection",
		ResourceID:   connectionID,
		Summary:      fmt.Sprintf("User %s unbound IM connection %s (%s)", u.Username, conn.ConnectionName, conn.Provider),
		Request:      r,
	})

	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
