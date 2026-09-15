package db

import (
	"path/filepath"
	"testing"
	"time"
)

// GPT re-review P0-1: the partial unique index must cover preparing so a
// second enqueue of the same intent is rejected at its initial INSERT (never
// a second stamp), and databases upgraded from the old schema must have any
// duplicate active rows deduped BEFORE the index can be created.

// Fresh database: the index already includes preparing; a second insert of
// the same key in ANY active status fails, and the idempotent wrapper joins
// the first run.
func TestPreparingUniqueIndexRejectsSecondActiveInsert(t *testing.T) {
	store := openBarrierStore(t)
	first := barrierRun("rr-idx-a", "k-idx", "preparing")
	if err := store.UpsertRuntimeRun(first); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	for _, status := range []string{"preparing", "queued", "running"} {
		dup := barrierRun("rr-idx-dup-"+status, "k-idx", status)
		if err := store.UpsertRuntimeRun(dup); err == nil {
			t.Fatalf("second insert with status %s must violate the unique index", status)
		}
	}
	// Empty-key rows are never constrained (fork/exec/legacy callers).
	free := barrierRun("rr-idx-empty", "", "preparing")
	if err := store.UpsertRuntimeRun(free); err != nil {
		t.Fatalf("empty-key insert must bypass the index: %v", err)
	}
}

// Legacy database path: the old schema let a preparing row coexist with a
// queued one. Migration must dedupe (survivor = most advanced), retire the
// losers safely, and land the widened index.
func TestPreparingIndexMigrationDedupesLegacyDuplicates(t *testing.T) {
	store := openBarrierStore(t)
	// Drop the migrated index and reconstruct the LEGACY shape: queued+running
	// unique index only, plus a duplicate preparing row and a double-queued
	// collision that the legacy index would have rejected — so emulate a
	// database that predates run_key dedupe by inserting directly.
	if _, err := store.(*SQLiteStore).sql.Exec(`DROP INDEX IF EXISTS idx_runtime_runs_active_key`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	old := barrierRun("rr-legacy-running", "k-legacy", "running")
	old.LeaseExpiresAt = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	if err := store.UpsertRuntimeRun(old); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	dupPreparing := barrierRun("rr-legacy-prep", "k-legacy", "preparing")
	if err := store.UpsertRuntimeRun(dupPreparing); err != nil {
		t.Fatalf("seed duplicate preparing (legal under the legacy index): %v", err)
	}
	// A second group: two queued rows with the same key (possible only from
	// pre-dedupe data).
	q1 := barrierRun("rr-legacy-q1", "k-legacy2", "queued")
	q2 := barrierRun("rr-legacy-q2", "k-legacy2", "queued")
	q2.UpdatedAt = q1.UpdatedAt
	if err := store.UpsertRuntimeRun(q1); err != nil {
		t.Fatalf("seed q1: %v", err)
	}
	if err := store.UpsertRuntimeRun(q2); err != nil {
		t.Fatalf("seed q2: %v", err)
	}

	if err := store.(*SQLiteStore).migrateRuntimeRunPreparingUniqueIndex(); err != nil {
		t.Fatalf("migration: %v", err)
	}

	// Survivors: the running row for k-legacy; q1 (lowest id wins the tie)
	// for k-legacy2.
	keepRunning, found, _ := store.RuntimeRunByID("ws", "rr-legacy-running")
	if !found || keepRunning.Status != "running" {
		t.Fatalf("running survivor must stay running: %+v", keepRunning)
	}
	keepQ1, found, _ := store.RuntimeRunByID("ws", "rr-legacy-q1")
	if !found || keepQ1.Status != "queued" {
		t.Fatalf("q1 survivor must stay queued: %+v", keepQ1)
	}
	// The duplicate preparing row is DELETED (never claimable).
	if _, found, _ := store.RuntimeRunByID("ws", "rr-legacy-prep"); found {
		t.Fatal("superseded preparing row must be deleted, not failed")
	}
	// The losing queued row is FAILED with a superseded marker.
	lostQ2, found, _ := store.RuntimeRunByID("ws", "rr-legacy-q2")
	if !found {
		t.Fatal("losing queued row must exist as a failed audit record")
	}
	if lostQ2.Status != "failed" || lostQ2.ErrorCode != "superseded_migration" {
		t.Fatalf("losing queued row = %s/%s, want failed/superseded_migration", lostQ2.Status, lostQ2.ErrorCode)
	}

	// The widened index is in place: any second active insert for a key fails.
	if err := store.UpsertRuntimeRun(barrierRun("rr-legacy-dup", "k-legacy", "preparing")); err == nil {
		t.Fatal("post-migration second active insert must violate the widened index")
	}
	// Idempotency: re-running the migration is a no-op.
	if err := store.(*SQLiteStore).migrateRuntimeRunPreparingUniqueIndex(); err != nil {
		t.Fatalf("re-migration must be a no-op: %v", err)
	}
}

// Stuck-preparing rescue: rows past the created_at grace are listed; fresh
// rows and already-promoted rows are not.
func TestListStuckPreparingRunsByCreatedAtCutoff(t *testing.T) {
	store := openBarrierStore(t)
	stuck := barrierRun("rr-stuck", "k-stuck", "preparing")
	stuck.CreatedAt = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if err := store.UpsertRuntimeRun(stuck); err != nil {
		t.Fatalf("seed stuck: %v", err)
	}
	fresh := barrierRun("rr-fresh", "k-fresh", "preparing")
	if err := store.UpsertRuntimeRun(fresh); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	cutoff := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	stuckRuns, err := store.ListStuckPreparingRuns("ws", cutoff, 100)
	if err != nil {
		t.Fatalf("list stuck: %v", err)
	}
	if len(stuckRuns) != 1 || stuckRuns[0].ID != "rr-stuck" {
		t.Fatalf("stuck list = %+v, want exactly rr-stuck (fresh row excluded)", stuckRuns)
	}
}

func openBarrierStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "preparing-index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertWorkspace(Workspace{ID: "ws", Name: "ws"}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	return s
}

func barrierRun(id, runKey, status string) RuntimeRun {
	now := time.Now().UTC().Format(time.RFC3339)
	return RuntimeRun{
		ID: id, WorkspaceID: "ws", ProjectID: "p", AgentID: "a", TaskID: "t",
		Status: status, RunKey: runKey, CreatedAt: now, UpdatedAt: now,
	}
}
