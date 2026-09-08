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

const imInstanceAttestationAdmin = "admin_attested"

type imInstanceResponse struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	DisplayName string `json:"displayName"`
	Attestation string `json:"attestation"`
	CreatedBy   string `json:"createdBy,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

type createIMInstanceRequest struct {
	Provider     string `json:"provider"`
	DisplayName  string `json:"displayName"`
	ConnectionID string `json:"connectionId"`
}

func (s *Server) handleListIMInstances(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.imInstanceWorkspace(w, r)
	if !ok {
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	instances, err := s.controlDB.ListIMInstances(controldb.IMInstanceFilter{
		WorkspaceID: workspaceID,
		Provider:    provider,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]imInstanceResponse, 0, len(instances))
	for _, instance := range instances {
		out = append(out, imInstanceToResponse(instance))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"instances": out})
}

func (s *Server) handleCreateIMInstance(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.imInstanceWorkspace(w, r)
	if !ok {
		return
	}
	var body createIMInstanceRequest
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	body.Provider = strings.TrimSpace(body.Provider)
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	body.ConnectionID = strings.TrimSpace(body.ConnectionID)
	if _, found := imbridge.LookupProvider(body.Provider); !found {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeUnsupportedProvider, "unsupported IM provider")
		return
	}
	if body.DisplayName == "" || len([]rune(body.DisplayName)) > 120 {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "displayName is required and must be at most 120 characters")
		return
	}
	existing, err := s.controlDB.ListIMInstances(controldb.IMInstanceFilter{WorkspaceID: workspaceID, Provider: body.Provider})
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, instance := range existing {
		if strings.EqualFold(instance.DisplayName, body.DisplayName) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, "an IM instance with this name already exists")
			return
		}
	}
	if err := s.validateIMInstanceConnection(workspaceID, body.Provider, body.ConnectionID); err != nil {
		s.writeIMInstanceConnectionError(w, err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	instance := controldb.IMInstance{
		ID:          newChannelID("imi"),
		WorkspaceID: workspaceID,
		Provider:    body.Provider,
		DisplayName: body.DisplayName,
		Attestation: imInstanceAttestationAdmin,
		CreatedBy:   requestUsername(r),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.controlDB.UpsertIMInstance(instance); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.controlDB.SetConnectionIMInstance(workspaceID, instance.ID, body.ConnectionID); err != nil {
		_ = s.controlDB.DeleteIMInstance(workspaceID, instance.ID)
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "im_instance.created",
		ResourceType: "im_instance",
		ResourceID:   instance.ID,
		Summary:      fmt.Sprintf("Created administrator-attested %s instance", instance.Provider),
		After: map[string]any{
			"provider":     instance.Provider,
			"displayName":  instance.DisplayName,
			"attestation":  instance.Attestation,
			"connectionId": body.ConnectionID,
		},
		Request: r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(imInstanceToResponse(instance))
}

func (s *Server) handleAttachIMInstanceConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID, instance, ok := s.imInstanceForRequest(w, r)
	if !ok {
		return
	}
	connectionID := strings.TrimSpace(r.PathValue("connectionId"))
	if err := s.validateIMInstanceConnection(workspaceID, instance.Provider, connectionID); err != nil {
		s.writeIMInstanceConnectionError(w, err)
		return
	}
	if err := s.controlDB.SetConnectionIMInstance(workspaceID, instance.ID, connectionID); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "im_instance.connection_attached",
		ResourceType: "im_instance",
		ResourceID:   instance.ID,
		Summary:      fmt.Sprintf("Attached IM connection to %s instance", instance.Provider),
		After:        map[string]any{"connectionId": connectionID},
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleDetachIMInstanceConnection(w http.ResponseWriter, r *http.Request) {
	workspaceID, instance, ok := s.imInstanceForRequest(w, r)
	if !ok {
		return
	}
	connectionID := strings.TrimSpace(r.PathValue("connectionId"))
	if err := s.controlDB.ClearConnectionIMInstance(workspaceID, instance.ID, connectionID); err != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, err.Error())
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "im_instance.connection_detached",
		ResourceType: "im_instance",
		ResourceID:   instance.ID,
		Summary:      fmt.Sprintf("Detached IM connection from %s instance", instance.Provider),
		After:        map[string]any{"connectionId": connectionID},
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleDeleteIMInstance(w http.ResponseWriter, r *http.Request) {
	workspaceID, instance, ok := s.imInstanceForRequest(w, r)
	if !ok {
		return
	}
	if err := s.controlDB.DeleteIMInstance(workspaceID, instance.ID); err != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, err.Error())
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "im_instance.deleted",
		ResourceType: "im_instance",
		ResourceID:   instance.ID,
		Summary:      fmt.Sprintf("Deleted empty %s instance", instance.Provider),
		Before:       imInstanceToResponse(instance),
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) imInstanceWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return "", false
	}
	if !s.canAdminWorkspace(r, workspaceID) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeWorkspaceAdminRequired, "workspace admin access required")
		return "", false
	}
	return workspaceID, true
}

func (s *Server) imInstanceForRequest(w http.ResponseWriter, r *http.Request) (string, controldb.IMInstance, bool) {
	workspaceID, ok := s.imInstanceWorkspace(w, r)
	if !ok {
		return "", controldb.IMInstance{}, false
	}
	instance, found, err := s.controlDB.IMInstanceByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.serverError(w, err)
		return "", controldb.IMInstance{}, false
	}
	if !found || instance.WorkspaceID != workspaceID {
		s.jsonError(w, http.StatusNotFound, "IM instance not found")
		return "", controldb.IMInstance{}, false
	}
	return workspaceID, instance, true
}

func (s *Server) validateIMInstanceConnection(workspaceID, provider, connectionID string) error {
	connection, found, err := s.controlDB.ConnectionByID(connectionID)
	if err != nil {
		return err
	}
	if !found || connection.WorkspaceID != workspaceID {
		return errIMInstanceConnectionNotFound
	}
	if connection.Provider != provider {
		return fmt.Errorf("connection provider must match the IM instance provider")
	}
	if connection.Status != "active" || !isAgentChannelConnection(connection) {
		return fmt.Errorf("connection must be an active agent IM channel")
	}
	return nil
}

var errIMInstanceConnectionNotFound = fmt.Errorf("connection not found")

func (s *Server) writeIMInstanceConnectionError(w http.ResponseWriter, err error) {
	if err == errIMInstanceConnectionNotFound {
		s.jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
}

func imInstanceToResponse(instance controldb.IMInstance) imInstanceResponse {
	return imInstanceResponse{
		ID:          instance.ID,
		Provider:    instance.Provider,
		DisplayName: instance.DisplayName,
		Attestation: instance.Attestation,
		CreatedBy:   instance.CreatedBy,
		CreatedAt:   instance.CreatedAt,
		UpdatedAt:   instance.UpdatedAt,
	}
}
