package db

import (
	"path/filepath"
	"testing"
)

func testConnection(fixture string) Connection {
	return Connection{
		ID:             "conn-" + fixture,
		WorkspaceID:    "ws-test",
		Provider:       "gitlab",
		ConnectionName: fixture,
		OwnerType:      "workspace",
		OwnerID:        "ws-test",
		AuthType:       "api_key",
		Status:         "active",
		ProfileJSON:    "{}",
	}
}

func seedConnection(t *testing.T, db *SQLiteStore, id string) {
	t.Helper()
	c := testConnection(id)
	if err := db.UpsertConnection(c); err != nil {
		t.Fatalf("upsert connection %s: %v", id, err)
	}
}

func TestSetDefaultConnectionSingleDefaultPerProvider(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.UpsertWorkspace(Workspace{ID: "ws-test", Name: "ws", Slug: "ws", Root: "/tmp/ws"}); err != nil {
		t.Fatalf("ensure workspace: %v", err)
	}
	seedConnection(t, db, "a")
	seedConnection(t, db, "b")

	if err := db.SetDefaultConnection("ws-test", "conn-a"); err != nil {
		t.Fatalf("set default a: %v", err)
	}
	a, ok, err := db.ConnectionByID("conn-a")
	if err != nil || !ok {
		t.Fatalf("get a: ok=%v err=%v", ok, err)
	}
	if !a.IsDefault {
		t.Fatalf("expected conn-a to be default")
	}

	// Marking b must clear a: one default per (workspace, provider).
	if err := db.SetDefaultConnection("ws-test", "conn-b"); err != nil {
		t.Fatalf("set default b: %v", err)
	}
	a, _, _ = db.ConnectionByID("conn-a")
	b, _, err := db.ConnectionByID("conn-b")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if a.IsDefault || !b.IsDefault {
		t.Fatalf("expected default to move a->b, got a=%v b=%v", a.IsDefault, b.IsDefault)
	}

	// A different provider is unaffected by the gitlab default.
	c := testConnection("od")
	c.Provider = "opendesign"
	c.ID = "conn-od"
	if err := db.UpsertConnection(c); err != nil {
		t.Fatalf("upsert od connection: %v", err)
	}
	if err := db.SetDefaultConnection("ws-test", "conn-od"); err != nil {
		t.Fatalf("set default od: %v", err)
	}
	if b, _, _ = db.ConnectionByID("conn-b"); !b.IsDefault {
		t.Fatalf("gitlab default must survive an opendesign default being set")
	}

	// Cross-workspace and missing connections are rejected.
	if err := db.SetDefaultConnection("ws-other", "conn-a"); err == nil {
		t.Fatalf("expected cross-workspace set default to fail")
	}
	if err := db.SetDefaultConnection("ws-test", "conn-missing"); err == nil {
		t.Fatalf("expected missing connection set default to fail")
	}
}

func TestConnectionMigrationBackfillsIsDefaultColumn(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.UpsertWorkspace(Workspace{ID: "ws-test", Name: "ws", Slug: "ws", Root: "/tmp/ws"}); err != nil {
		t.Fatalf("ensure workspace: %v", err)
	}
	seedConnection(t, db, "plain")
	c, ok, err := db.ConnectionByID("conn-plain")
	if err != nil || !ok {
		t.Fatalf("get plain: ok=%v err=%v", ok, err)
	}
	if c.IsDefault {
		t.Fatalf("fresh connection must not be default until marked")
	}
}
