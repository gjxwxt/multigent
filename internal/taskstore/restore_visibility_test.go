package taskstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Regression (2026-09-29 ghost archive): a task archived at terminal state and
// then restored to pending via PUT /api/v1/tasks/update stayed invisible to
// ListTasks, the scheduler and the UI. The restore must clear ArchivedAt in
// the same write that moves the status out of terminal.
func TestRestorePendingTaskVisibleInListTasks(t *testing.T) {
	t.Run("dbstore", func(t *testing.T) { testRestoreVisible(t, newDBTestStore) })
	t.Run("fsstore", func(t *testing.T) { testRestoreVisible(t, newFSTestStore) })
}

func TestUpdateTaskRestorePendingTaskVisible(t *testing.T) {
	// DBStore only: its UpdateTask resolves the record across active and
	// archived storage (GetTask scans every alias), so the retry path can
	// legitimately flip an already-archived record back to pending. The FS
	// backend scopes UpdateTask to the active queue by contract — its archive
	// restore goes through PersistTask above.
	t.Run("dbstore", func(t *testing.T) { testRestoreVisibleUpdateTask(t, newDBTestStore) })
}

func TestRestoreKeepsArchivedAtOnTerminalPersist(t *testing.T) {
	t.Run("dbstore", func(t *testing.T) { testTerminalPersistKeepsArchive(t, newDBTestStore) })
	t.Run("fsstore", func(t *testing.T) { testTerminalPersistKeepsArchive(t, newFSTestStore) })
}

type storeFactory func(t *testing.T) Store

func newDBTestStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return NewDB(root, db)
}

func newFSTestStore(t *testing.T) Store {
	t.Helper()
	return New(t.TempDir())
}

func seedTerminalArchivedTask(t *testing.T, ts Store) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	task := &entity.Task{
		ID:        entity.NewTaskID(),
		Title:     "failed task",
		Status:    entity.TaskStatusDoneFailed,
		Prompt:    "do the thing",
		CreatedBy: "test",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := ts.AddTask("sample", "dev", task); err != nil {
		t.Fatal(err)
	}
	if err := ts.ArchiveTask("sample", "dev", task); err != nil {
		t.Fatal(err)
	}
	if task.ArchivedAt == nil {
		t.Fatal("fixture must archive the task")
	}
	// Sanity: the archived task is invisible while terminal (existing contract).
	active, err := ts.ListTasks("sample", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("terminal task must not be listed as active: %+v", active)
	}
	return task
}

func testRestoreVisible(t *testing.T, newStore storeFactory) {
	t.Helper()
	ts := newStore(t)
	task := seedTerminalArchivedTask(t, ts)

	// Reopen: the exact sequence the API update path performs — patch status
	// to pending, then PersistTask.
	task.Status = entity.TaskStatusPending
	if err := ts.PersistTask("sample", "dev", task); err != nil {
		t.Fatal(err)
	}
	if task.ArchivedAt != nil {
		t.Fatalf("ArchivedAt must be cleared on restore, got %v", task.ArchivedAt)
	}
	active, err := ts.ListTasks("sample", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != task.ID {
		t.Fatalf("restored task must be immediately visible, got %+v", active)
	}
}

func testRestoreVisibleUpdateTask(t *testing.T, newStore storeFactory) {
	t.Helper()
	ts := newStore(t)
	task := seedTerminalArchivedTask(t, ts)

	// The scheduler/manual retry path archives on failure, then flips the
	// in-memory task back to pending and persists via UpdateTask.
	task.Status = entity.TaskStatusPending
	task.StartedAt = nil
	task.FinishedAt = nil
	if err := ts.UpdateTask("sample", "dev", task); err != nil {
		t.Fatal(err)
	}
	if task.ArchivedAt != nil {
		t.Fatalf("ArchivedAt must be cleared on retry restore, got %v", task.ArchivedAt)
	}
	active, err := ts.ListTasks("sample", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != task.ID {
		t.Fatalf("retried task must be immediately visible, got %+v", active)
	}
}

func testTerminalPersistKeepsArchive(t *testing.T, newStore storeFactory) {
	t.Helper()
	ts := newStore(t)
	task := seedTerminalArchivedTask(t, ts)

	// A terminal persist must not resurrect the task into the active queue.
	if err := ts.PersistTask("sample", "dev", task); err != nil {
		t.Fatal(err)
	}
	if task.ArchivedAt == nil {
		t.Fatal("terminal persist must keep the archive stamp")
	}
	active, err := ts.ListTasks("sample", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("terminal task must stay archived, got %+v", active)
	}
}
