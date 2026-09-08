package db

import (
	"path/filepath"
	"testing"
)

func TestTaskThreadProjections_LifecycleAndUniqueConstraint(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	ws := Workspace{
		ID:   "ws-test-1",
		Name: "Test Workspace",
		Slug: "test-workspace",
		Root: dir,
	}
	if err := store.UpsertWorkspace(ws); err != nil {
		t.Fatalf("UpsertWorkspace: %v", err)
	}

	// 1. Insert an active projection
	p1 := TaskThreadProjection{
		ID:          "ttp-1",
		WorkspaceID: ws.ID,
		ProjectID:   "proj-auth",
		TaskID:      "task-101",
		Provider:    "mattermost",
		ChannelID:   "chan-mm-1",
		RootPostID:  "post-root-1",
		Status:      "active",
	}
	if err := store.UpsertTaskThreadProjection(p1); err != nil {
		t.Fatalf("UpsertTaskThreadProjection p1: %v", err)
	}

	// 2. Query active projection
	got, found, err := store.ActiveTaskThreadProjection(ws.ID, "task-101", "mattermost")
	if err != nil || !found {
		t.Fatalf("ActiveTaskThreadProjection expected found, got found=%v err=%v", found, err)
	}
	if got.RootPostID != "post-root-1" || got.Status != "active" {
		t.Fatalf("unexpected projection: %+v", got)
	}

	// 3. Query by root post
	byRoot, foundRoot, err := store.TaskThreadProjectionByRoot("mattermost", "chan-mm-1", "post-root-1")
	if err != nil || !foundRoot {
		t.Fatalf("TaskThreadProjectionByRoot expected found, got found=%v err=%v", foundRoot, err)
	}
	if byRoot.ID != "ttp-1" {
		t.Fatalf("expected ID ttp-1, got %s", byRoot.ID)
	}

	// 4. Attempt to insert a second active projection for the same task and provider (should fail partial unique constraint)
	p2 := TaskThreadProjection{
		ID:          "ttp-2",
		WorkspaceID: ws.ID,
		ProjectID:   "proj-auth",
		TaskID:      "task-101",
		Provider:    "mattermost",
		ChannelID:   "chan-mm-1",
		RootPostID:  "post-root-2",
		Status:      "active",
	}
	err = store.UpsertTaskThreadProjection(p2)
	if err == nil {
		t.Fatalf("expected UNIQUE constraint violation when inserting second active projection for same task, got nil")
	}

	// 5. Close projection
	if err := store.CloseTaskThreadProjection(ws.ID, "task-101", "mattermost"); err != nil {
		t.Fatalf("CloseTaskThreadProjection: %v", err)
	}

	// Now ActiveTaskThreadProjection should return false
	_, foundAfterClose, err := store.ActiveTaskThreadProjection(ws.ID, "task-101", "mattermost")
	if err != nil || foundAfterClose {
		t.Fatalf("expected not found after close, got found=%v err=%v", foundAfterClose, err)
	}

	// 6. After close, inserting a new active projection (e.g. rebind / restart) should succeed!
	p3 := TaskThreadProjection{
		ID:          "ttp-3",
		WorkspaceID: ws.ID,
		ProjectID:   "proj-auth",
		TaskID:      "task-101",
		Provider:    "mattermost",
		ChannelID:   "chan-mm-1",
		RootPostID:  "post-root-3",
		Status:      "active",
	}
	if err := store.UpsertTaskThreadProjection(p3); err != nil {
		t.Fatalf("UpsertTaskThreadProjection p3 after closing p1: %v", err)
	}

	// Verify p3 is now active
	activeP3, foundP3, err := store.ActiveTaskThreadProjection(ws.ID, "task-101", "mattermost")
	if err != nil || !foundP3 || activeP3.ID != "ttp-3" {
		t.Fatalf("expected active projection ttp-3, got %+v (found=%v)", activeP3, foundP3)
	}

	// 7. List projections should return both (ttp-3 active, ttp-1 closed)
	all, err := store.ListTaskThreadProjections(TaskThreadProjectionFilter{
		WorkspaceID: ws.ID,
		TaskID:      "task-101",
	})
	if err != nil {
		t.Fatalf("ListTaskThreadProjections: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 projections in history, got %d", len(all))
	}
}
