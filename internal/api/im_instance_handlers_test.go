package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func TestIMInstanceAdministrationRequiresAdminAndAttachesOnlyAgentChannels(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
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
			ProfileJSON:    `{"usage":"agent_im_channel","purpose":"agent_channel"}`,
			CreatedBy:      "admin",
		}); err != nil {
			t.Fatalf("connection %s: %v", connectionID, err)
		}
	}
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-regular",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "regular",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "admin",
	}); err != nil {
		t.Fatalf("regular connection: %v", err)
	}

	memberRec := httptest.NewRecorder()
	s.handleCreateIMInstance(memberRec, providerTestRequest(http.MethodPost, "/api/v1/im/instances", "owner", createIMInstanceRequest{
		Provider: "mattermost", DisplayName: "Engineering Mattermost", ConnectionID: "conn-mira",
	}))
	if memberRec.Code != http.StatusForbidden {
		t.Fatalf("member status=%d body=%s", memberRec.Code, memberRec.Body.String())
	}

	badRec := httptest.NewRecorder()
	s.handleCreateIMInstance(badRec, providerTestRequest(http.MethodPost, "/api/v1/im/instances", "admin", createIMInstanceRequest{
		Provider: "mattermost", DisplayName: "Not an agent channel", ConnectionID: "conn-regular",
	}))
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("regular connection status=%d body=%s", badRec.Code, badRec.Body.String())
	}

	createRec := httptest.NewRecorder()
	s.handleCreateIMInstance(createRec, providerTestRequest(http.MethodPost, "/api/v1/im/instances", "admin", createIMInstanceRequest{
		Provider: "mattermost", DisplayName: "Engineering Mattermost", ConnectionID: "conn-mira",
	}))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var instance imInstanceResponse
	if err := json.NewDecoder(createRec.Body).Decode(&instance); err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	if instance.ID == "" || instance.Attestation != imInstanceAttestationAdmin {
		t.Fatalf("instance=%#v", instance)
	}
	connection, found, err := s.controlDB.ConnectionByID("conn-mira")
	if err != nil || !found || connection.IMInstanceID != instance.ID {
		t.Fatalf("mira association=%q found=%v err=%v", connection.IMInstanceID, found, err)
	}

	attachRec := httptest.NewRecorder()
	attachReq := providerTestRequest(http.MethodPut, "/api/v1/im/instances/"+instance.ID+"/connections/conn-lina", "admin", nil)
	attachReq.SetPathValue("id", instance.ID)
	attachReq.SetPathValue("connectionId", "conn-lina")
	s.handleAttachIMInstanceConnection(attachRec, attachReq)
	if attachRec.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", attachRec.Code, attachRec.Body.String())
	}
	connection, found, err = s.controlDB.ConnectionByID("conn-lina")
	if err != nil || !found || connection.IMInstanceID != instance.ID {
		t.Fatalf("lina association=%q found=%v err=%v", connection.IMInstanceID, found, err)
	}

	detachRec := httptest.NewRecorder()
	detachReq := providerTestRequest(http.MethodDelete, "/api/v1/im/instances/"+instance.ID+"/connections/conn-lina", "admin", nil)
	detachReq.SetPathValue("id", instance.ID)
	detachReq.SetPathValue("connectionId", "conn-lina")
	s.handleDetachIMInstanceConnection(detachRec, detachReq)
	if detachRec.Code != http.StatusOK {
		t.Fatalf("detach status=%d body=%s", detachRec.Code, detachRec.Body.String())
	}
}
