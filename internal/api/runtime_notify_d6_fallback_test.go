package api

import (
	"path/filepath"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func TestD6OutboundIdentityFallback(t *testing.T) {
	dir := t.TempDir()
	store, err := controldb.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer store.Close()

	wsID := "ws-d6-test"
	if err := store.UpsertWorkspace(controldb.Workspace{ID: wsID, Name: "Test WS", Slug: "test-ws", Root: dir}); err != nil {
		t.Fatalf("UpsertWorkspace: %v", err)
	}

	// Insert user alex
	if err := store.UpsertUser(controldb.User{Username: "alex", Role: "member"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	// Connection
	connID := "conn-mm"
	if err := store.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "mm-conn",
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	}); err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}

	// Agent A binding (mira)
	bindingA := controldb.AgentChannelBinding{
		ID:           "binding-mira",
		WorkspaceID:  wsID,
		ProjectID:    "1test",
		AgentID:      "mira",
		Provider:     "mattermost",
		ConnectionID: connID,
		Status:       "connected",
	}
	if err := store.UpsertAgentChannelBinding(bindingA); err != nil {
		t.Fatalf("UpsertAgentChannelBinding A: %v", err)
	}

	// Agent B binding (lina)
	bindingB := controldb.AgentChannelBinding{
		ID:           "binding-lina",
		WorkspaceID:  wsID,
		ProjectID:    "1test",
		AgentID:      "lina",
		Provider:     "mattermost",
		ConnectionID: connID,
		Status:       "connected",
	}
	if err := store.UpsertAgentChannelBinding(bindingB); err != nil {
		t.Fatalf("UpsertAgentChannelBinding B: %v", err)
	}

	// User Alex bound ONLY on Agent A (Mira)
	alexIdentity := controldb.UserChannelIdentity{
		ID:               "alex-identity-mira",
		WorkspaceID:      wsID,
		UserID:           "alex",
		ChannelBindingID: bindingA.ID,
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-alex-123",
		ExternalChatID:   "mm-dm-alex-mira",
	}
	if err := store.UpsertUserChannelIdentity(alexIdentity); err != nil {
		t.Fatalf("UpsertUserChannelIdentity: %v", err)
	}

	server := &Server{
		controlDB: store,
	}

	principalLina := runtimeAgentPrincipal{
		WorkspaceID: wsID,
		Project:     "1test",
		Agent:       "lina",
	}

	// When Lina tries to notify Alex:
	// 1. Check runtimeNotifyTargetForRecipient for bindingB
	target, found, err := server.runtimeNotifyTargetForRecipient(principalLina, bindingB, "alex")
	if err != nil {
		t.Fatalf("runtimeNotifyTargetForRecipient failed: %v", err)
	}
	if !found {
		t.Fatalf("expected D6 fallback to find Alex's bound identity, but found=false")
	}
	if target.ReceiveID != "mm-user-alex-123" {
		t.Errorf("expected target.ReceiveID to be 'mm-user-alex-123', got '%s'", target.ReceiveID)
	}

	// 2. Check runtimeChannelToRow for bindingB
	row, err := server.runtimeChannelToRow(principalLina, bindingB)
	if err != nil {
		t.Fatalf("runtimeChannelToRow failed: %v", err)
	}
	if !row.CanNotify {
		t.Errorf("expected CanNotify=true for Lina via D6 workspace fallback")
	}
}
