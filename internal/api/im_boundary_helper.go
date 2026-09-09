package api

import (
	"strings"

	controldb "github.com/multigent/multigent/internal/db"
)

// connectionsShareDeliveryBoundary reports whether two connections can safely share
// delivery and identity mappings. Returns (usable, crossBot).
// - usable: true if both connections are active and either identical or belong to the same admin_attested IM instance.
// - crossBot: true only when usable is true and the two connection IDs differ (requiring destination Bot DM recreation).
func (s *Server) connectionsShareDeliveryBoundary(destination, source controldb.Connection) (bool, bool) {
	if s == nil || s.controlDB == nil || destination.WorkspaceID == "" ||
		destination.WorkspaceID != source.WorkspaceID || destination.Provider != source.Provider ||
		destination.Status != "active" || source.Status != "active" {
		return false, false
	}
	if destination.ID != "" && destination.ID == source.ID {
		return true, false
	}
	if strings.TrimSpace(destination.IMInstanceID) == "" || destination.IMInstanceID != source.IMInstanceID {
		return false, false
	}
	instance, found, err := s.controlDB.IMInstanceByID(destination.IMInstanceID)
	if err != nil || !found || instance.WorkspaceID != destination.WorkspaceID ||
		instance.Provider != destination.Provider || instance.Attestation != imInstanceAttestationAdmin {
		return false, false
	}
	return true, true
}

// bindingsShareDeliveryBoundary reports whether two agent channel bindings share
// a delivery and identity boundary.
func (s *Server) bindingsShareDeliveryBoundary(destination, source controldb.AgentChannelBinding) (bool, bool) {
	if s == nil || s.controlDB == nil || destination.WorkspaceID == "" ||
		destination.WorkspaceID != source.WorkspaceID || destination.Provider != source.Provider ||
		destination.Status != "connected" || source.Status != "connected" {
		return false, false
	}
	if strings.TrimSpace(destination.ConnectionID) != "" && destination.ConnectionID == source.ConnectionID {
		return true, false
	}
	destConn, destFound, destErr := s.controlDB.ConnectionByID(destination.ConnectionID)
	sourceConn, sourceFound, sourceErr := s.controlDB.ConnectionByID(source.ConnectionID)
	if destErr != nil || sourceErr != nil || !destFound || !sourceFound {
		return false, false
	}
	return s.connectionsShareDeliveryBoundary(destConn, sourceConn)
}

// getTrustedConnectionIDsForConnection returns all active connection IDs that share
// the same delivery/identity boundary with the specified connection ID.
func (s *Server) getTrustedConnectionIDsForConnection(workspaceID, connectionID string) ([]string, error) {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return nil, nil
	}
	conn, found, err := s.controlDB.ConnectionByID(connectionID)
	if err != nil || !found || conn.WorkspaceID != workspaceID || conn.Status != "active" {
		return []string{connectionID}, nil
	}
	instanceID := strings.TrimSpace(conn.IMInstanceID)
	if instanceID == "" {
		return []string{connectionID}, nil
	}
	instance, found, err := s.controlDB.IMInstanceByID(instanceID)
	if err != nil || !found || instance.WorkspaceID != workspaceID || instance.Attestation != imInstanceAttestationAdmin {
		return []string{connectionID}, nil
	}
	allConns, err := s.controlDB.ListConnections(controldb.ConnectionFilter{
		WorkspaceID: workspaceID,
		Provider:    conn.Provider,
		Status:      "active",
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range allConns {
		if c.IMInstanceID == instanceID {
			out = append(out, c.ID)
		}
	}
	if len(out) == 0 {
		out = append(out, connectionID)
	}
	return out, nil
}
