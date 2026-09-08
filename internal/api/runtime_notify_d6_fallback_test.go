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

	// Alex can currently access the project served by the destination Bot.
	if err := store.UpsertUser(controldb.User{
		Username:     "alex",
		Role:         "member",
		ProjectsJSON: `[{"project":"1test","role":"viewer"}]`,
	}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if err := store.UpsertWorkspaceMember(wsID, "alex", WorkspaceRoleMember); err != nil {
		t.Fatalf("workspace member: %v", err)
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
		IMInstanceID:   "imi-engineering",
		ProfileJSON:    "{}",
	}); err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}
	if err := store.UpsertIMInstance(controldb.IMInstance{
		ID:          "imi-engineering",
		WorkspaceID: wsID,
		Provider:    "mattermost",
		DisplayName: "Engineering Mattermost",
		Attestation: imInstanceAttestationAdmin,
		CreatedBy:   "admin",
	}); err != nil {
		t.Fatalf("instance: %v", err)
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
		users:     newUserStore(store),
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

	// 3. A different Bot on the same administrator-attested instance may reuse
	// Alex's external user ID, but it must not reuse Mira's Bot-specific DM ID.
	connOther := "conn-mm-other-bot"
	_ = store.UpsertConnection(controldb.Connection{
		ID:             connOther,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "mm-conn-other",
		AuthType:       "bot_token",
		Status:         "active",
		IMInstanceID:   "imi-engineering",
		ProfileJSON:    "{}",
	})
	bindingC := controldb.AgentChannelBinding{
		ID:           "binding-kobe",
		WorkspaceID:  wsID,
		ProjectID:    "1test",
		AgentID:      "kobe",
		Provider:     "mattermost",
		ConnectionID: connOther,
		Status:       "connected",
	}
	_ = store.UpsertAgentChannelBinding(bindingC)

	principalKobe := runtimeAgentPrincipal{
		WorkspaceID: wsID,
		Project:     "1test",
		Agent:       "kobe",
	}
	targetOther, foundOther, errOther := server.runtimeNotifyTargetForRecipient(principalKobe, bindingC, "alex")
	if errOther != nil {
		t.Fatalf("runtimeNotifyTargetForRecipient other connection failed: %v", errOther)
	}
	if !foundOther || targetOther.ReceiveID != "mm-user-alex-123" || targetOther.ChatID != "" {
		t.Fatalf("expected cross-Bot fallback to open a fresh DM, got found=%v target=%#v", foundOther, targetOther)
	}

	// 4. An unassociated connection must remain isolated.
	connUnassociated := "conn-mm-unassociated"
	_ = store.UpsertConnection(controldb.Connection{
		ID:             connUnassociated,
		WorkspaceID:    wsID,
		Provider:       "mattermost",
		ConnectionName: "mm-unassociated",
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    "{}",
	})
	bindingD := controldb.AgentChannelBinding{
		ID:           "binding-unassociated",
		WorkspaceID:  wsID,
		ProjectID:    "1test",
		AgentID:      "unassociated",
		Provider:     "mattermost",
		ConnectionID: connUnassociated,
		Status:       "connected",
	}
	_ = store.UpsertAgentChannelBinding(bindingD)
	_, foundUnassociated, err := server.runtimeNotifyTargetForRecipient(runtimeAgentPrincipal{WorkspaceID: wsID, Project: "1test", Agent: "unassociated"}, bindingD, "alex")
	if err != nil || foundUnassociated {
		t.Fatalf("unassociated connection crossed the D6 boundary: found=%v err=%v", foundUnassociated, err)
	}

	// 5. A historical binding cannot bypass revoked access to the destination Agent.
	if err := store.UpsertUser(controldb.User{Username: "alex", Role: "member", ProjectsJSON: `[]`}); err != nil {
		t.Fatalf("revoke project access: %v", err)
	}
	_, foundAfterRevocation, err := server.runtimeNotifyTargetForRecipient(principalLina, bindingB, "alex")
	if err != nil || foundAfterRevocation {
		t.Fatalf("revoked recipient access must fail closed: found=%v err=%v", foundAfterRevocation, err)
	}
}
