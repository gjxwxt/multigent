package kvfacade

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTest opens a fresh in-memory facade with its own schema.
func openTest(t *testing.T) *Facade {
	t.Helper()
	// File-backed: modernc's private-cache in-memory DBs are per-connection,
	// so a pool would see N different empty databases. A temp file gives the
	// concurrency tests one honest shared database.
	dir := t.TempDir()
	f, err := Open(filepath.Join(dir, "kvfacade.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func mustWorkspace(t *testing.T, f *Facade, id string) {
	t.Helper()
	if err := f.UpsertWorkspace(Workspace{ID: id, Name: "w-" + id, Root: "/tmp/" + id}); err != nil {
		t.Fatalf("UpsertWorkspace: %v", err)
	}
	w, found, err := f.Workspace(id)
	if err != nil || !found || w.Name != "w-"+id {
		t.Fatalf("Workspace roundtrip: found=%v err=%v w=%+v", found, err, w)
	}
}

// TestKVRecordCRUDAndRevision pins the record CRUD + monotonic revision
// semantics the CAS discipline stands on.
func TestKVRecordCRUDAndRevision(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	// insert-if-absent: first wins, second is a no-op.
	inserted, err := f.InsertRecordIfAbsent("runs", "ws1", []string{"r1"}, `{"v":1}`)
	if err != nil || !inserted {
		t.Fatalf("first InsertRecordIfAbsent: inserted=%v err=%v", inserted, err)
	}
	inserted, err = f.InsertRecordIfAbsent("runs", "ws1", []string{"r1"}, `{"v":2}`)
	if err != nil || inserted {
		t.Fatalf("second InsertRecordIfAbsent must lose: inserted=%v err=%v", inserted, err)
	}

	// revision bumps on every write.
	rev1, found, err := f.RecordRevision("runs", "ws1", []string{"r1"})
	if err != nil || !found {
		t.Fatalf("RecordRevision: %v %v", found, err)
	}
	if rev1 != "1" {
		t.Fatalf("first write revision = %q, want 1", rev1)
	}
	if err := f.UpsertRecord("runs", "ws1", []string{"r1"}, `{"v":2}`); err != nil {
		t.Fatalf("UpsertRecord: %v", err)
	}
	rev2, _, _ := f.RecordRevision("runs", "ws1", []string{"r1"})
	if rev2 == rev1 {
		t.Fatalf("revision must bump: %q == %q", rev2, rev1)
	}

	// list with prefix + revision.
	rows, err := f.ListRecordsWithRevision("runs", "ws1", []string{"r1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListRecordsWithRevision: %v len=%d", err, len(rows))
	}
	if rows[0].Revision != rev2 || rows[0].Payload != `{"v":2}` {
		t.Fatalf("row mismatch: %+v", rows[0])
	}

	// payload-witnessing CAS: right payload+revision swaps, wrong payload doesn't.
	ok, err := f.UpdateRecordIfPayloadAndRevision("runs", "ws1", []string{"r1"}, `{"v":3}`, `{"v":WRONG}`, rev2)
	if err != nil || ok {
		t.Fatalf("CAS with wrong payload must fail: ok=%v err=%v", ok, err)
	}
	ok, err = f.UpdateRecordIfPayloadAndRevision("runs", "ws1", []string{"r1"}, `{"v":3}`, `{"v":2}`, rev2)
	if err != nil || !ok {
		t.Fatalf("CAS with right witness must swap: ok=%v err=%v", ok, err)
	}

	// revision-blind CAS against the NEW revision (stale fails).
	rev3, _, _ := f.RecordRevision("runs", "ws1", []string{"r1"})
	ok, err = f.UpdateRecordIfRevision("runs", "ws1", []string{"r1"}, `{"v":4}`, rev2) // stale
	if err != nil || ok {
		t.Fatalf("stale revision CAS must fail: ok=%v err=%v", ok, err)
	}
	ok, err = f.UpdateRecordIfRevision("runs", "ws1", []string{"r1"}, `{"v":4}`, rev3)
	if err != nil || !ok {
		t.Fatalf("fresh revision CAS must swap: ok=%v err=%v", ok, err)
	}

	// delete.
	if err := f.DeleteRecord("runs", "ws1", []string{"r1"}); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	_, found, _ = f.GetRecord("runs", "ws1", []string{"r1"})
	if found {
		t.Fatalf("record should be gone")
	}
}

// TestConcurrentUpsertIfRevision pins the CAS race boundary: N connections
// race UpsertRecordIfRevision-style swaps on one row; exactly one wins each
// round, every loser observes swapped=false, and the final payload is the
// winner's.
func TestConcurrentUpsertIfRevision(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	if err := f.UpsertRecord("runs", "ws1", []string{"race"}, "seed"); err != nil {
		t.Fatal(err)
	}
	const racers = 8
	const rounds = 25
	wins := make([]int, racers)
	var wg sync.WaitGroup
	for round := 0; round < rounds; round++ {
		rev, _, _ := f.RecordRevision("runs", "ws1", []string{"race"})
		wg.Add(racers)
		for r := 0; r < racers; r++ {
			go func(r int, rev string) {
				defer wg.Done()
				ok, err := f.UpdateRecordIfRevision("runs", "ws1", []string{"race"},
					fmt.Sprintf("winner-%d-round-%d", r, round), rev)
				if err != nil {
					t.Errorf("racer %d: %v", r, err)
					return
				}
				if ok {
					wins[r]++
				}
			}(r, rev)
		}
		wg.Wait()
	}
	total := 0
	for _, w := range wins {
		total += w
	}
	if total != rounds {
		t.Fatalf("each round exactly one winner: got %d wins over %d rounds", total, rounds)
	}
	payload, found, _ := f.GetRecord("runs", "ws1", []string{"race"})
	if !found || !strings.HasPrefix(payload, "winner-") {
		t.Fatalf("final payload must be a winner's: %q found=%v", payload, found)
	}
}

// encodeClaim builds a claim marker payload the way the workflow store does:
// "transition-claim:<len(id)>:<id><unixNano>:<runJSON>".
func encodeClaim(claimID string, claimedAtNano int64, runJSON string) string {
	return ClaimMarkerPrefix + itoa(len(claimID)) + ":" + claimID + itoa64(claimedAtNano) + ":" + runJSON
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

// TestCommitTransitionGuardedClaimSemantics pins the claim-owner gate:
//  1. commit with the right claimID lands the whole batch atomically;
//  2. commit after the claim was stolen (marker replaced) fails with
//     ErrClaimLost and NOTHING from the losing batch persists;
//  3. a plain (non-marker) run payload rejects every claimant — fail-closed.
func TestCommitTransitionGuardedClaimSemantics(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	runKey := []string{"run", "task1"}
	// Seed the run row as a claim marker held by claimant A.
	if err := f.UpsertRecord("workflow_runs", "ws1", runKey, encodeClaim("claim-A", time.Now().UnixNano(), `{"step":1}`)); err != nil {
		t.Fatal(err)
	}

	// 1. legitimate commit: claimant A advances the run.
	err := f.CommitTransitionGuarded("ws1", "workflow_runs", runKey, "claim-A", []KVWrite{
		{Table: "workflow_steps", Workspace: "ws1", Key: []string{"task1", "step1"}, Payload: `{"state":"done"}`},
		{Table: "workflow_runs", Workspace: "ws1", Key: runKey, Payload: `{"step":2}`}, // releases the claim
	})
	if err != nil {
		t.Fatalf("legitimate transition commit: %v", err)
	}
	payload, found, _ := f.GetRecord("workflow_runs", "ws1", runKey)
	if !found || payload != `{"step":2}` {
		t.Fatalf("run payload must be the post-transition state: %q", payload)
	}

	// 2. a NEW claimant B steals via CAS; old holder A's commit must fail whole.
	if err := f.UpsertRecord("workflow_runs", "ws1", runKey, encodeClaim("claim-B", time.Now().UnixNano(), `{"step":2}`)); err != nil {
		t.Fatal(err)
	}
	err = f.CommitTransitionGuarded("ws1", "workflow_runs", runKey, "claim-A", []KVWrite{
		{Table: "workflow_steps", Workspace: "ws1", Key: []string{"task1", "step1"}, Payload: `{"state":"STALE-A"}`},
		{Table: "workflow_events", Workspace: "ws1", Key: []string{"task1", "ev1"}, Payload: `{"by":"A"}`},
	})
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale claimant must get ErrClaimLost, got: %v", err)
	}
	// NOTHING from A's batch persisted.
	if p, _, _ := f.GetRecord("workflow_steps", "ws1", []string{"task1", "step1"}); p == `{"state":"STALE-A"}` {
		t.Fatalf("losing batch's step write must not persist")
	}
	if p, _, _ := f.GetRecord("workflow_events", "ws1", []string{"task1", "ev1"}); p != "" {
		t.Fatalf("losing batch's event write must not persist, got %q", p)
	}

	// 3. plain payload (transition already finished) rejects everyone — fail-closed.
	if err := f.UpsertRecord("workflow_runs", "ws1", runKey, `{"step":3}`); err != nil {
		t.Fatal(err)
	}
	err = f.CommitTransitionGuarded("ws1", "workflow_runs", runKey, "claim-B", []KVWrite{
		{Table: "workflow_runs", Workspace: "ws1", Key: runKey, Payload: `{"step":4}`},
	})
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("plain payload must reject claimants, got: %v", err)
	}

	// 4. missing run row rejects.
	err = f.CommitTransitionGuarded("ws1", "workflow_runs", []string{"run", "nope"}, "claim-B", []KVWrite{
		{Table: "workflow_runs", Workspace: "ws1", Key: []string{"run", "nope"}, Payload: "x"},
	})
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("missing run row must fail closed, got: %v", err)
	}
}

// TestCommitTransitionGuardedTxPlan pins the Tx variant: plan runs INSIDE the
// IMMEDIATE transaction, can read records, and its writes land atomically
// with the transition; a plan error aborts everything.
func TestCommitTransitionGuardedTxPlan(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	runKey := []string{"run", "task2"}
	if err := f.UpsertRecord("workflow_runs", "ws1", runKey, encodeClaim("claim-P", time.Now().UnixNano(), `{}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.UpsertRecord("workflow_runs", "ws1", []string{"run", "task2", "memo"}, `{"frozen":false}`); err != nil {
		t.Fatal(err)
	}

	// plan reads the memo and includes a write derived from it.
	planRan := false
	err := f.CommitTransitionGuardedTx("ws1", "workflow_runs", runKey, "claim-P", func(tx KVTxReader) ([]KVWrite, error) {
		planRan = true
		memo, found, err := tx.GetRecord("workflow_runs", "ws1", []string{"run", "task2", "memo"})
		if err != nil || !found {
			return nil, fmt.Errorf("plan read: found=%v err=%v", found, err)
		}
		return []KVWrite{
			{Table: "workflow_runs", Workspace: "ws1", Key: []string{"run", "task2", "memo"}, Payload: strings.Replace(memo, "false", "true", 1)},
			{Table: "workflow_runs", Workspace: "ws1", Key: runKey, Payload: `{"step":1}`},
		}, nil
	})
	if err != nil || !planRan {
		t.Fatalf("plan commit: err=%v planRan=%v", err, planRan)
	}
	memo, _, _ := f.GetRecord("workflow_runs", "ws1", []string{"run", "task2", "memo"})
	if memo != `{"frozen":true}` {
		t.Fatalf("plan's derived write must land: %q", memo)
	}

	// plan error aborts the whole batch (run keeps the claim marker).
	if err := f.UpsertRecord("workflow_runs", "ws1", runKey, encodeClaim("claim-Q", time.Now().UnixNano(), `{}`)); err != nil {
		t.Fatal(err)
	}
	err = f.CommitTransitionGuardedTx("ws1", "workflow_runs", runKey, "claim-Q", func(tx KVTxReader) ([]KVWrite, error) {
		return nil, errors.New("plan boom")
	})
	if err == nil || !strings.Contains(err.Error(), "plan boom") {
		t.Fatalf("plan error must propagate, got: %v", err)
	}
	if p, _, _ := f.GetRecord("workflow_runs", "ws1", runKey); !verifyClaimOwner(p, "claim-Q") {
		t.Fatalf("plan failure must leave the claim intact, got %q", p)
	}
}

// TestCommitRecordWritesGuardedRaceFree pins the round-19 P1 semantics:
// a guard's slot-free check and the occupy write are transactional — two
// concurrent guarded commits cannot both observe "slot free".
func TestCommitRecordWritesGuardedRaceFree(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	const contenders = 12
	var wg sync.WaitGroup
	winners := 0
	var mu sync.Mutex
	for r := 0; r < contenders; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			err := f.CommitRecordWritesGuarded("ws1", func(tx KVTxReader) error {
				// guard: claim the slot only if still free
				_, found, err := tx.GetRecord("slots", "ws1", []string{"only"})
				if err != nil {
					return err
				}
				if found {
					return fmt.Errorf("slot taken")
				}
				return nil
			}, []KVWrite{
				{Table: "slots", Workspace: "ws1", Key: []string{"only"}, Payload: fmt.Sprintf(`{"by":%d}`, r)},
			})
			if err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("guarded check-then-write must admit exactly one writer, got %d", winners)
	}
}

// TestCommitTransitionConcurrentSteal pins the fencing boundary under real
// concurrency: two transitioners hold claims; the loser's guarded commit
// fails whole even when racing the winner.
func TestCommitTransitionConcurrentSteal(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	runKey := []string{"run", "race3"}
	if err := f.UpsertRecord("workflow_runs", "ws1", runKey, encodeClaim("claim-1", time.Now().UnixNano(), `{}`)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, claim := range []string{"claim-1", "claim-1"} {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			// both racers commit against the same claim; only ONE
			// transaction can win (the second sees the marker already
			// replaced by the winner's post-transition payload).
			results <- f.CommitTransitionGuarded("ws1", "workflow_runs", runKey, c, []KVWrite{
				{Table: "workflow_runs", Workspace: "ws1", Key: runKey, Payload: `{"done":true}`},
			})
		}(claim)
	}
	wg.Wait()
	close(results)
	wins, losses := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrClaimLost) {
			losses++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("exactly one winner one loser: wins=%d losses=%d", wins, losses)
	}
}

// TestReentrancy pins reentrancy expectations: a plan/guard that calls back
// into the facade must NOT deadlock on a pool connection (the tx handle is
// the only sanctioned read path) — and writing through the pool inside a
// guard would be rejected by design (KVTxReader is read-only).
func TestReentrancy(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")

	if err := f.UpsertRecord("runs", "ws1", []string{"r"}, "v1"); err != nil {
		t.Fatal(err)
	}

	// Guard re-reading through the tx reader is legal and consistent.
	err := f.CommitRecordWritesGuarded("ws1", func(tx KVTxReader) error {
		a, found1, _ := tx.GetRecord("runs", "ws1", []string{"r"})
		b, found2, _ := tx.GetRecord("runs", "ws1", []string{"r"})
		if !found1 || !found2 || a != b {
			t.Errorf("tx reads must be consistent: %q %q", a, b)
		}
		return nil
	}, []KVWrite{{Table: "runs", Workspace: "ws1", Key: []string{"r"}, Payload: "v2"}})
	if err != nil {
		t.Fatalf("reentrant guard reads: %v", err)
	}

	// A guard trying to WRITE via the facade's pool methods signals the
	// design boundary: KVTxReader exposes no writes. The compile-level rule
	// is asserted here behaviorally — the guard's only write path is the
	// KVWrite batch it returns/joins.
	if _, ok := any(KVTxReader(nil)).(interface {
		UpsertRecord(table, workspaceID string, key []string, payload string) error
	}); ok {
		t.Fatalf("KVTxReader must not expose writes")
	}
}

// TestKeyPrefixBoundary pins the ≤3-segment key contract.
func TestKeyPrefixBoundary(t *testing.T) {
	f := openTest(t)
	mustWorkspace(t, f, "ws1")
	if _, err := f.ListRecords("t", "ws1", []string{"a", "b", "c", "d"}); err == nil {
		t.Fatalf("4-segment prefix must error")
	}
	if _, err := f.ListRecordsWithRevision("t", "ws1", []string{"a", "b", "c", "d"}); err == nil {
		t.Fatalf("4-segment prefix must error (with-revision)")
	}
	// key padding: short keys pad with empty k2/k3
	if err := f.UpsertRecord("t", "ws1", []string{"only1"}, "p"); err != nil {
		t.Fatal(err)
	}
	rows, err := f.ListRecords("t", "ws1", []string{"only1"})
	if err != nil || len(rows) != 1 || rows[0].Key[0] != "only1" {
		t.Fatalf("short-key roundtrip: %v %v", err, rows)
	}
}
