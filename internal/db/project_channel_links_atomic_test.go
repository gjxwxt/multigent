package db

import (
	"path/filepath"
	"testing"
	"time"
)

// The verified remote binding delete must live inside
// DeleteProjectControlPlaneScope's single transaction (post-P0.6 review):
// a recreated project must never inherit the old binding's trust, and a
// failure mid-scope must never leave a half-deleted control plane. The
// failure is injected at the LAST statement of the transaction (the binding
// delete) — if the four deletes were not atomic, memberships, channel links,
// and agent bindings would already be gone when the binding delete fails.
func TestDeleteProjectControlPlaneScopeBindingFailureIsAtomic(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339)
	if err := store.UpsertWorkspace(Workspace{ID: "ws-x", Name: "ws-x"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := store.UpsertConnection(Connection{ID: "conn-1", WorkspaceID: "ws-x", Provider: "gitlab", ConnectionName: "conn-1"}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	seed := func() {
		if err := store.UpsertProjectMembership(ProjectMembership{
			ID: "pm-1", WorkspaceID: "ws-x", ProjectID: "proj-x", MemberType: "user", MemberID: "u1",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
		if err := store.UpsertProjectChannelLink(ProjectChannelLink{
			ID: "pcl-1", WorkspaceID: "ws-x", ProjectID: "proj-x", Provider: "feishu",
		}); err != nil {
			t.Fatalf("seed channel link: %v", err)
		}
		if err := store.UpsertAgentChannelBinding(AgentChannelBinding{
			ID: "acb-1", WorkspaceID: "ws-x", ProjectID: "proj-x", AgentWorkerID: "aw-1",
			AgentID: "a-1", Provider: "feishu", ConnectionID: "conn-1",
		}); err != nil {
			t.Fatalf("seed agent binding: %v", err)
		}
		if err := store.UpsertVerifiedRemoteBinding(VerifiedRemoteBinding{
			WorkspaceID: "ws-x", ProjectID: "proj-x", Provider: "gitlab",
			ConnectionID: "conn-1", RemoteProjectID: "58",
			PathWithNamespace: "g/proj-x", VerifiedAt: now,
			Source: BindingSourcePlatformCreate,
		}); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
	}
	seed()

	if _, err := store.sql.Exec(`CREATE TRIGGER fail_binding_delete BEFORE DELETE ON verified_remote_bindings FOR EACH ROW BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := store.DeleteProjectControlPlaneScope("ws-x", "proj-x"); err == nil {
		t.Fatal("DeleteProjectControlPlaneScope must fail when the binding delete fails")
	}
	if _, err := store.sql.Exec(`DROP TRIGGER fail_binding_delete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	count := func(table string) int {
		t.Helper()
		var n int
		if err := store.sql.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE project_id = 'proj-x'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	for _, table := range []string{"project_memberships", "project_channel_links", "agent_channel_bindings", "verified_remote_bindings"} {
		if got := count(table); got != 1 {
			t.Fatalf("%s rows = %d after failed delete, want 1 (transaction must roll back — no partial delete)", table, got)
		}
	}

	// Without the injected failure the same call removes everything.
	if err := store.DeleteProjectControlPlaneScope("ws-x", "proj-x"); err != nil {
		t.Fatalf("clean delete: %v", err)
	}
	for _, table := range []string{"project_memberships", "project_channel_links", "agent_channel_bindings", "verified_remote_bindings"} {
		if got := count(table); got != 0 {
			t.Fatalf("%s rows = %d after successful delete, want 0", table, got)
		}
	}
}
