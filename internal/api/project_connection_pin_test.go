package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/agentdir"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
)

// newConnectionPinServer builds a minimal server harness for the
// RemoteConnection backfill tests.
func newConnectionPinServer(t *testing.T) (*Server, string) {
	t.Helper()
	db, err := controldb.Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(t.TempDir(), "workspace")
	st := store.NewDB(root, db)
	ts := taskstore.NewDB(root, db)
	s := &Server{root: root, controlDB: db, st: st, ts: ts, users: newUserStore(db), agentDirectory: agentdir.New(db)}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		t.Fatalf("workspace id: %v", err)
	}
	if err := s.controlDB.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Test Workspace",
		Slug: "test-workspace",
		Root: root,
	}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	return s, workspaceID
}

func pinTestConnection(t *testing.T, s *Server, workspaceID, id, provider string) {
	t.Helper()
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             id,
		WorkspaceID:    workspaceID,
		Provider:       provider,
		ConnectionName: id,
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"http://gitlab.test","apiKey":"tok"}`,
		CreatedBy:      "test",
	}); err != nil {
		t.Fatalf("upsert connection %s: %v", id, err)
	}
}

func TestBackfillRemoteConnectionPinsFirstResolve(t *testing.T) {
	s, workspaceID := newConnectionPinServer(t)
	pinTestConnection(t, s, workspaceID, "conn-gl", "gitlab")

	project := "pinproj"
	p := &entity.Project{Name: project, Repo: filepath.Join(s.root, project, "workspace"), RemoteProvider: "gitlab"}
	if err := s.st.SaveProject(project, p); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// resolveCodeHost with an empty pin falls back to newest-updated; the
	// backfill must then pin what was actually used.
	host, connID, err := s.pinnedCodeHost(context.Background(), project, p, "gitlab")
	if err != nil {
		t.Fatalf("pinned code host: %v", err)
	}
	if host == nil || connID == "" {
		t.Fatalf("expected resolved host and connection id")
	}
	stored, err := s.st.Project(project)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if stored.RemoteConnection != connID {
		t.Fatalf("expected RemoteConnection backfilled to %q, got %q", connID, stored.RemoteConnection)
	}
}

func TestBackfillRemoteConnectionNeverOverwritesExplicitPin(t *testing.T) {
	s, workspaceID := newConnectionPinServer(t)
	pinTestConnection(t, s, workspaceID, "conn-gl-a", "gitlab")
	pinTestConnection(t, s, workspaceID, "conn-gl-b", "gitlab")

	project := "pinproj2"
	p := &entity.Project{Name: project, Repo: filepath.Join(s.root, project, "workspace"), RemoteProvider: "gitlab", RemoteConnection: "conn-gl-a"}
	if err := s.st.SaveProject(project, p); err != nil {
		t.Fatalf("save project: %v", err)
	}

	if _, _, err := s.pinnedCodeHost(context.Background(), project, p, "gitlab"); err != nil {
		t.Fatalf("pinned code host: %v", err)
	}
	stored, err := s.st.Project(project)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if stored.RemoteConnection != "conn-gl-a" {
		t.Fatalf("explicit pin must survive resolution, got %q", stored.RemoteConnection)
	}
}

func TestDesignConnectionAuditFieldsExposeHost(t *testing.T) {
	cfg := &designConnectionConfig{BaseURL: "http://127.0.0.1:7456", ConnID: "conn-od"}
	fields := designConnectionAuditFields(cfg)
	if fields["odConnectionId"] != "conn-od" {
		t.Fatalf("odConnectionId=%v", fields["odConnectionId"])
	}
	if fields["odHost"] != "127.0.0.1:7456" {
		t.Fatalf("odHost=%v", fields["odHost"])
	}
	if designConnectionAuditFields(nil) != nil {
		t.Fatalf("nil config must yield nil audit fields")
	}
}
