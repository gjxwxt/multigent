package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/imbridge"
)

// TestSaveManualAgentChannelAutoAttachesSingleAttestedInstance pins FIX 4:
// manual setup through the raw API path auto-attaches the connection to the
// workspace's single admin-attested IM instance for the provider, so
// instance-scoped provisioning sees it without a manual UI step.
func TestSaveManualAgentChannelAutoAttachesSingleAttestedInstance(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	if err := s.controlDB.UpsertIMInstance(controldb.IMInstance{
		ID: "imi-auto-1", WorkspaceID: workspaceID, Provider: "mattermost",
		DisplayName: "Only Instance", Attestation: "admin_attested", CreatedBy: "admin",
		CreatedAt: "2026-09-29T00:00:00Z", UpdatedAt: "2026-09-29T00:00:00Z",
	}); err != nil {
		t.Fatalf("upsert instance: %v", err)
	}

	if err := s.st.SaveProject("autoattach", &entity.Project{Name: "autoattach"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	binding, err := s.saveManualAgentIMChannel(req, workspaceID, "autoattach", "FP Dev A", "aw-auto-a", imbridge.ManualSetupResult{
		Provider:      "mattermost",
		BaseURL:       "http://127.0.0.1:8065",
		AuthType:      "bot_token",
		AppID:         "bot-auto-1",
		ExternalBotID: "bot-auto-1",
		SecretValues: map[string]string{
			"baseUrl":          "http://127.0.0.1:8065",
			"botToken":         "tok",
			"bridgeHmacSecret": "hmac-val",
			"appId":            "bot-auto-1",
		},
	})
	if err != nil {
		t.Fatalf("save channel: %v", err)
	}
	conn, found, err := s.controlDB.ConnectionByID(binding.ConnectionID)
	if err != nil || !found {
		t.Fatalf("connection lookup: found=%v err=%v", found, err)
	}
	if conn.IMInstanceID != "imi-auto-1" {
		t.Fatalf("expected auto-attach to imi-auto-1, got %q", conn.IMInstanceID)
	}
}

// TestSaveManualAgentChannelMultiInstanceNoAutoAttach: with two attested
// instances the server must NOT guess — connection stays unattached.
func TestSaveManualAgentChannelMultiInstanceNoAutoAttach(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	for _, id := range []struct{ id, name string }{{"imi-multi-1", "Alpha"}, {"imi-multi-2", "Beta"}} {
		if err := s.controlDB.UpsertIMInstance(controldb.IMInstance{
			ID: id.id, WorkspaceID: workspaceID, Provider: "mattermost",
			DisplayName: id.name, Attestation: "admin_attested", CreatedBy: "admin",
			CreatedAt: "2026-09-29T00:00:00Z", UpdatedAt: "2026-09-29T00:00:00Z",
		}); err != nil {
			t.Fatalf("upsert instance %s: %v", id.id, err)
		}
	}
	if err := s.st.SaveProject("autoattach2", &entity.Project{Name: "autoattach2"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	binding, err := s.saveManualAgentIMChannel(req, workspaceID, "autoattach2", "FP Dev B", "aw-auto-b", imbridge.ManualSetupResult{
		Provider:      "mattermost",
		BaseURL:       "http://127.0.0.1:8065",
		AuthType:      "bot_token",
		AppID:         "bot-auto-2",
		ExternalBotID: "bot-auto-2",
		SecretValues: map[string]string{
			"baseUrl": "http://127.0.0.1:8065", "botToken": "tok",
			"bridgeHmacSecret": "h", "appId": "bot-auto-2",
		},
	})
	if err != nil {
		t.Fatalf("save channel: %v", err)
	}
	conn, found, err := s.controlDB.ConnectionByID(binding.ConnectionID)
	if err != nil || !found {
		t.Fatalf("connection lookup: found=%v err=%v", found, err)
	}
	if conn.IMInstanceID != "" {
		t.Fatalf("multi-instance must not auto-attach, got %q", conn.IMInstanceID)
	}
}
