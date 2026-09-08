package db

import (
	"path/filepath"
	"testing"
)

func TestIMInstanceConnectionAssociation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	const workspaceID = "ws-im-instance"
	if err := store.UpsertWorkspace(Workspace{ID: workspaceID, Name: "IM", Slug: "im", Root: t.TempDir()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	for _, id := range []string{"conn-a", "conn-b"} {
		if err := store.UpsertConnection(Connection{
			ID:             id,
			WorkspaceID:    workspaceID,
			Provider:       "mattermost",
			ConnectionName: id,
			OwnerType:      "workspace",
			OwnerID:        workspaceID,
			AuthType:       "bot_token",
			Status:         "active",
			ProfileJSON:    `{}`,
		}); err != nil {
			t.Fatalf("connection %s: %v", id, err)
		}
	}

	instance := IMInstance{
		ID:          "imi-engineering",
		WorkspaceID: workspaceID,
		Provider:    "mattermost",
		DisplayName: "Engineering Mattermost",
		CreatedBy:   "admin",
	}
	if err := store.UpsertIMInstance(instance); err != nil {
		t.Fatalf("upsert instance: %v", err)
	}
	for _, connectionID := range []string{"conn-a", "conn-b"} {
		if err := store.SetConnectionIMInstance(workspaceID, instance.ID, connectionID); err != nil {
			t.Fatalf("attach %s: %v", connectionID, err)
		}
		connection, found, err := store.ConnectionByID(connectionID)
		if err != nil || !found || connection.IMInstanceID != instance.ID {
			t.Fatalf("connection %s instance=%q found=%v err=%v", connectionID, connection.IMInstanceID, found, err)
		}
	}

	if err := store.DeleteIMInstance(workspaceID, instance.ID); err == nil {
		t.Fatal("deleting a non-empty IM instance should fail")
	}
	if err := store.ClearConnectionIMInstance(workspaceID, instance.ID, "conn-a"); err != nil {
		t.Fatalf("detach conn-a: %v", err)
	}
	if err := store.ClearConnectionIMInstance(workspaceID, instance.ID, "conn-b"); err != nil {
		t.Fatalf("detach conn-b: %v", err)
	}
	if err := store.DeleteIMInstance(workspaceID, instance.ID); err != nil {
		t.Fatalf("delete empty instance: %v", err)
	}
}

func TestUpsertConnectionPreservesIMInstanceAssociation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	const workspaceID = "ws-im-instance-preserve"
	if err := store.UpsertWorkspace(Workspace{ID: workspaceID, Name: "IM", Slug: "im-preserve", Root: t.TempDir()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := store.UpsertConnection(Connection{
		ID:             "conn-a",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "agent-a",
		OwnerType:      "workspace",
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		IMInstanceID:   "imi-existing",
		ProfileJSON:    `{}`,
	}); err != nil {
		t.Fatalf("initial connection: %v", err)
	}
	if err := store.UpsertConnection(Connection{
		ID:             "conn-new-id-is-ignored-by-conflict",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "agent-a",
		OwnerType:      "workspace",
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"http://mm.example.test"}`,
	}); err != nil {
		t.Fatalf("refresh connection: %v", err)
	}
	connection, found, err := store.ConnectionByID("conn-a")
	if err != nil || !found || connection.IMInstanceID != "imi-existing" {
		t.Fatalf("association was not preserved: %#v found=%v err=%v", connection, found, err)
	}
}
