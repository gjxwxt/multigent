package db

import (
	"path/filepath"
	"testing"
)

func TestExternalIdentitiesAreWorkspaceAndProviderScoped(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	for _, ws := range []Workspace{
		{ID: "ws-one", Name: "One", Slug: "one", Root: "/tmp/one", CreatedAt: nowUTC()},
		{ID: "ws-two", Name: "Two", Slug: "two", Root: "/tmp/two", CreatedAt: nowUTC()},
	} {
		if err := db.UpsertWorkspace(ws); err != nil {
			t.Fatalf("workspace: %v", err)
		}
	}
	for _, u := range []User{
		{Username: "ella", CreatedAt: nowUTC()},
		{Username: "owner-a", CreatedAt: nowUTC()},
	} {
		if err := db.UpsertUser(u); err != nil {
			t.Fatalf("user: %v", err)
		}
	}

	if err := db.UpsertExternalIdentity(ExternalIdentity{
		ID:             "ext-one",
		WorkspaceID:    "ws-one",
		Provider:       "feishu",
		ExternalUserID: "ou_same",
		UserID:         "ella",
	}); err != nil {
		t.Fatalf("upsert one: %v", err)
	}
	if err := db.UpsertExternalIdentity(ExternalIdentity{
		ID:             "ext-two",
		WorkspaceID:    "ws-two",
		Provider:       "feishu",
		ExternalUserID: "ou_same",
		UserID:         "owner-a",
	}); err != nil {
		t.Fatalf("upsert two: %v", err)
	}
	if err := db.UpsertExternalIdentity(ExternalIdentity{
		ID:             "ext-three",
		WorkspaceID:    "ws-one",
		Provider:       "lark",
		ExternalUserID: "ou_same",
		UserID:         "owner-a",
	}); err != nil {
		t.Fatalf("upsert three: %v", err)
	}

	one, ok, err := db.ExternalIdentityByExternalID("ws-one", "feishu", "ou_same")
	if err != nil || !ok {
		t.Fatalf("lookup one ok=%v err=%v", ok, err)
	}
	two, ok, err := db.ExternalIdentityByExternalID("ws-two", "feishu", "ou_same")
	if err != nil || !ok {
		t.Fatalf("lookup two ok=%v err=%v", ok, err)
	}
	three, ok, err := db.ExternalIdentityByExternalID("ws-one", "lark", "ou_same")
	if err != nil || !ok {
		t.Fatalf("lookup three ok=%v err=%v", ok, err)
	}
	if one.UserID != "ella" || two.UserID != "owner-a" || three.UserID != "owner-a" {
		t.Fatalf("scope mismatch: one=%#v two=%#v three=%#v", one, two, three)
	}
}

func TestUnbindUserConnection(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	wsID := "ws-unbind-test"
	if err := db.UpsertWorkspace(Workspace{ID: wsID, Name: "TestWS", Slug: "testws", Root: "/tmp/ws", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := db.UpsertUser(User{Username: "alex", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("user: %v", err)
	}

	// Insert connections first to satisfy foreign key constraint
	c1 := Connection{ID: "conn-1", WorkspaceID: wsID, Provider: "mattermost", ConnectionName: "MM-1", OwnerType: "workspace", OwnerID: wsID, CreatedAt: nowUTC()}
	c2 := Connection{ID: "conn-2", WorkspaceID: wsID, Provider: "mattermost", ConnectionName: "MM-2", OwnerType: "workspace", OwnerID: wsID, CreatedAt: nowUTC()}
	if err := db.UpsertConnection(c1); err != nil {
		t.Fatalf("conn-1: %v", err)
	}
	if err := db.UpsertConnection(c2); err != nil {
		t.Fatalf("conn-2: %v", err)
	}

	// Two bindings under conn-1, one under conn-2
	b1 := AgentChannelBinding{ID: "chan-1", WorkspaceID: wsID, ProjectID: "p1", AgentID: "a1", ConnectionID: "conn-1", Provider: "mattermost", Status: "connected"}
	b2 := AgentChannelBinding{ID: "chan-2", WorkspaceID: wsID, ProjectID: "p2", AgentID: "a1", ConnectionID: "conn-1", Provider: "mattermost", Status: "connected"}
	b3 := AgentChannelBinding{ID: "chan-3", WorkspaceID: wsID, ProjectID: "p3", AgentID: "a1", ConnectionID: "conn-2", Provider: "mattermost", Status: "connected"}
	for _, b := range []AgentChannelBinding{b1, b2, b3} {
		if err := db.UpsertAgentChannelBinding(b); err != nil {
			t.Fatalf("binding %s: %v", b.ID, err)
		}
	}

	// User alex bound to chan-1, chan-2, and chan-3 with external user "mm-alex"
	if err := db.UpsertUserChannelIdentity(UserChannelIdentity{
		ID: "uch-1", WorkspaceID: wsID, UserID: "alex", ChannelBindingID: "chan-1", Provider: "mattermost", ExternalUserID: "mm-alex",
	}); err != nil {
		t.Fatalf("uch-1: %v", err)
	}
	if err := db.UpsertUserChannelIdentity(UserChannelIdentity{
		ID: "uch-2", WorkspaceID: wsID, UserID: "alex", ChannelBindingID: "chan-2", Provider: "mattermost", ExternalUserID: "mm-alex",
	}); err != nil {
		t.Fatalf("uch-2: %v", err)
	}
	if err := db.UpsertUserChannelIdentity(UserChannelIdentity{
		ID: "uch-3", WorkspaceID: wsID, UserID: "alex", ChannelBindingID: "chan-3", Provider: "mattermost", ExternalUserID: "mm-alex",
	}); err != nil {
		t.Fatalf("uch-3: %v", err)
	}
	if err := db.UpsertExternalIdentity(ExternalIdentity{
		ID: "ext-1", WorkspaceID: wsID, Provider: "mattermost", ExternalUserID: "mm-alex", UserID: "alex",
	}); err != nil {
		t.Fatalf("ext-1: %v", err)
	}

	// Create an unused bind code for chan-1
	if err := db.CreateAgentChannelBindCode(AgentChannelBindCode{
		Code: "MG-CODE1", WorkspaceID: wsID, ChannelBindingID: "chan-1", UserID: "alex", TargetType: "user", ExpiresAt: nowUTC(),
	}); err != nil {
		t.Fatalf("bind code: %v", err)
	}

	// Unbind conn-1
	if err := db.UnbindUserConnection(wsID, "alex", "conn-1"); err != nil {
		t.Fatalf("unbind conn-1: %v", err)
	}

	// 1. Check user_channel_identities: chan-1 and chan-2 should be gone, chan-3 remains
	list, err := db.ListUserChannelIdentities(UserChannelIdentityFilter{WorkspaceID: wsID, UserID: "alex"})
	if err != nil {
		t.Fatalf("list uci: %v", err)
	}
	if len(list) != 1 || list[0].ChannelBindingID != "chan-3" {
		t.Fatalf("expected only chan-3 remaining, got: %#v", list)
	}

	// 2. Check external_identity: since chan-3 (conn-2) still uses mm-alex, ext should still exist
	ext, found, err := db.ExternalIdentityByExternalID(wsID, "mattermost", "mm-alex")
	if err != nil || !found {
		t.Fatalf("expected ext to remain for conn-2, found=%v err=%v", found, err)
	}
	if ext.UserID != "alex" {
		t.Fatalf("unexpected ext user: %s", ext.UserID)
	}

	// 3. Check bind code: should be marked as used
	codeRow, codeFound, err := db.AgentChannelBindCodeByCode("MG-CODE1")
	if err != nil || !codeFound {
		t.Fatalf("code lookup: found=%v err=%v", codeFound, err)
	}
	if codeRow.UsedAt == "" {
		t.Fatalf("expected unused bind code to be invalidated with UsedAt set, got empty")
	}

	// 4. Now unbind conn-2
	if err := db.UnbindUserConnection(wsID, "alex", "conn-2"); err != nil {
		t.Fatalf("unbind conn-2: %v", err)
	}
	// External identity should now be deleted because no other channel identity remains
	_, foundAfter, err := db.ExternalIdentityByExternalID(wsID, "mattermost", "mm-alex")
	if err != nil {
		t.Fatalf("ext after unbind conn-2: %v", err)
	}
	if foundAfter {
		t.Fatalf("expected external identity to be deleted when all connection routes removed")
	}
}
