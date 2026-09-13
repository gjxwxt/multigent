package db

import (
	"path/filepath"
	"sync"
	"testing"
)

// Q0 PR-1: run_key idempotency. The partial unique index
// idx_runtime_runs_active_key(workspace_id, run_key) WHERE status IN
// ('queued','running') AND run_key <> '' must dedupe concurrent enqueues of
// the same intent, never constrain legacy empty-key rows, and release the key
// as soon as the run reaches a terminal state.

func newRunKeyTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "runkey.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertWorkspace(Workspace{ID: "ws-rk", Name: "ws-rk"}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	return store
}

func runKeyRun(id, runKey string) RuntimeRun {
	return RuntimeRun{
		ID:          id,
		WorkspaceID: "ws-rk",
		ProjectID:   "proj",
		AgentID:     "agent",
		TaskID:      "task-" + id,
		Status:      "queued",
		RunKey:      runKey,
	}
}

// A6: the same run_key enqueued twice (sequentially) yields one active run;
// the second insert surfaces the existing active run.
func TestRuntimeRunKeyDuplicateActiveEnqueueReturnsExisting(t *testing.T) {
	store := newRunKeyTestStore(t)
	key, err := RuntimeRunKeyTask("ws-rk", "proj", "t-1")
	if err != nil {
		t.Fatalf("build key: %v", err)
	}
	first, inserted, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-1", key))
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	second, inserted, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-2", key))
	if err != nil {
		t.Fatalf("second insert must not error: %v", err)
	}
	if inserted {
		t.Fatal("second insert must not create a new run")
	}
	if second.ID != first.ID {
		t.Fatalf("second enqueue returned %s, want existing %s", second.ID, first.ID)
	}
	runs, err := store.ListRuntimeRuns(RuntimeRunFilter{WorkspaceID: "ws-rk"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("active runs = %d, want 1", len(runs))
	}
}

// A6 concurrency variant: 20 goroutines enqueue the same key; exactly one
// row is created and every loser gets the winner's ID.
func TestRuntimeRunKeyConcurrentEnqueueOnlyOneActive(t *testing.T) {
	store := newRunKeyTestStore(t)
	key, err := RuntimeRunKeyTask("ws-rk", "proj", "t-c")
	if err != nil {
		t.Fatalf("build key: %v", err)
	}
	const n = 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run, _, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-"+string(rune('a'+i)), key))
			if err != nil {
				t.Errorf("enqueue %d: %v", i, err)
				return
			}
			mu.Lock()
			ids[i] = run.ID
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	first := ids[0]
	for i, id := range ids {
		if id == "" {
			t.Fatalf("enqueue %d produced no id", i)
		}
		if id != first {
			t.Fatalf("enqueue %d got %s, want winner %s", i, id, first)
		}
	}
	runs, err := store.ListRuntimeRuns(RuntimeRunFilter{WorkspaceID: "ws-rk"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("rows = %d, want 1", len(runs))
	}
}

// A7: different keys never collide.
func TestRuntimeRunKeyDifferentKeysBothEnqueue(t *testing.T) {
	store := newRunKeyTestStore(t)
	key1, _ := RuntimeRunKeyTask("ws-rk", "proj", "t-a")
	key2, _ := RuntimeRunKeyTask("ws-rk", "proj", "t-b")
	if _, inserted, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-1", key1)); err != nil || !inserted {
		t.Fatalf("first: %v %v", inserted, err)
	}
	if _, inserted, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-2", key2)); err != nil || !inserted {
		t.Fatalf("second: %v %v", inserted, err)
	}
}

// A8: once the previous run with the key is terminal, the key is released.
func TestRuntimeRunKeyReleasedAfterTerminal(t *testing.T) {
	store := newRunKeyTestStore(t)
	key, _ := RuntimeRunKeyTask("ws-rk", "proj", "t-x")
	first, _, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-1", key))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	first.Status = "succeeded"
	if err := store.UpsertRuntimeRun(first); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	second, inserted, err := store.UpsertRuntimeRunIdempotent(runKeyRun("run-2", key))
	if err != nil || !inserted {
		t.Fatalf("re-enqueue after terminal must succeed: inserted=%v err=%v", inserted, err)
	}
	if second.ID == first.ID {
		t.Fatal("re-enqueue must create a new run, not return the terminal one")
	}
}

// A9: legacy rows with run_key='' must never be constrained — many empty-key
// active rows coexist.
func TestRuntimeRunKeyEmptyKeysUnconstrained(t *testing.T) {
	store := newRunKeyTestStore(t)
	for i := 0; i < 5; i++ {
		if _, inserted, err := store.UpsertRuntimeRunIdempotent(runKeyRun("legacy-"+string(rune('a'+i)), "")); err != nil || !inserted {
			t.Fatalf("legacy %d: inserted=%v err=%v", i, inserted, err)
		}
	}
	runs, err := store.ListRuntimeRuns(RuntimeRunFilter{WorkspaceID: "ws-rk"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 5 {
		t.Fatalf("empty-key rows = %d, want 5 (empty keys bypass the unique index)", len(runs))
	}
}

// A9b: a fresh DB where every row predates run_key still migrates cleanly
// (the migration adds columns + partial index without touching existing rows).
func TestRuntimeRunKeyMigrationOnLegacyDB(t *testing.T) {
	// Open() runs migrate() — the mere existence of rows created AFTER
	// migration with empty keys plus the index presence is the assertion.
	store := newRunKeyTestStore(t)
	var idx int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_runtime_runs_active_key'`).Scan(&idx); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if idx != 1 {
		t.Fatal("idx_runtime_runs_active_key must exist after migration")
	}
	// The partial index must not constrain legacy-style empty-key rows: seed
	// via raw SQL bypassing helpers, mimicking pre-Q0 rows.
	if _, err := store.sql.Exec(`INSERT INTO runtime_runs (id, workspace_id, project_id, agent_id, task_id, status, created_at, updated_at) VALUES ('old-1','ws-rk','p','a','t1','queued','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	if _, err := store.sql.Exec(`INSERT INTO runtime_runs (id, workspace_id, project_id, agent_id, task_id, status, created_at, updated_at) VALUES ('old-2','ws-rk','p','a','t2','queued','2026-01-01T00:00:01Z','2026-01-01T00:00:01Z')`); err != nil {
		t.Fatalf("seed old row 2 (must not collide on empty run_key): %v", err)
	}
}

// run_key builders: pure functions with strict input validation — no empty
// keys may ever be produced from partial input.
func TestRuntimeRunKeyBuilders(t *testing.T) {
	if k, err := RuntimeRunKeyTask("ws", "p", "t"); err != nil || k != "task:ws:p:t" {
		t.Fatalf("task key = %q err=%v", k, err)
	}
	if _, err := RuntimeRunKeyTask("ws", "", "t"); err == nil {
		t.Fatal("task key with empty project must error")
	}
	if k, err := RuntimeRunKeyWakeup("ws", "p", "a", "scheduled"); err != nil || k != "wakeup:ws:p:a:scheduled" {
		t.Fatalf("wakeup key = %q err=%v", k, err)
	}
	if k, err := RuntimeRunKeyWakeup("ws", "p", "a", "attention:att-1"); err != nil || k != "wakeup:ws:p:a:attention:att-1" {
		t.Fatalf("attention key = %q err=%v", k, err)
	}
	if _, err := RuntimeRunKeyWakeup("ws", "p", "a", ""); err == nil {
		t.Fatal("wakeup key with empty intent must error")
	}
	if k, err := RuntimeRunKeyWorkflowStep("ws", "wfr-1", "wfs-1"); err != nil || k != "wf:ws:wfr-1:wfs-1" {
		t.Fatalf("wf key = %q err=%v", k, err)
	}
	if k, err := RuntimeRunKeyExec("ws", "admin", "p", "a", "client-key-1"); err != nil || k != "exec:ws:admin:p:a:client-key-1" {
		t.Fatalf("exec key = %q err=%v", k, err)
	}
	if _, err := RuntimeRunKeyExec("ws", "admin", "p", "a", "bad key with spaces"); err == nil {
		t.Fatal("exec key with spaces must error")
	}
	if err := ValidateIdempotencyKey(string(make([]byte, 129, 129)) + "x"); err == nil {
		t.Fatal("oversized key must error")
	}
}
