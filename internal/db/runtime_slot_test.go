package db

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Q0 PR-2: Worker-slot occupancy + lease generation. Matrix A1-A5, A10-A12.
// (A6-A9 run_key tests live in run_key_test.go; A13/A14 api-level concurrency
// tests live in internal/api.)

func newSlotTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "slot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertWorkspace(Workspace{ID: "ws-slot", Name: "ws-slot"}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	return store
}

func slotRun(id, workerID, status string) RuntimeRun {
	return RuntimeRun{
		ID:            id,
		WorkspaceID:   "ws-slot",
		AgentWorkerID: workerID,
		ProjectID:     "proj",
		AgentID:       "agent-" + workerID,
		TaskID:        "task-" + id,
		Status:        status,
		Priority:      2,
	}
}

func slotNode(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	if err := store.UpsertRuntimeNode(RuntimeNode{ID: id, WorkspaceID: "ws-slot", Name: id, Kind: "personal_computer", Status: "online", LastSeenAt: nowUTC(), CreatedByUserID: "admin"}); err != nil {
		t.Fatalf("node %s: %v", id, err)
	}
}

// runOccupiesWorkerSlot unit semantics (D1): running + unexpired lease +
// slot_class != readonly. Queued never occupies; empty/unparseable lease never
// occupies (reaper/claim handle those); unknown slot classes fail closed to
// occupying (normal).
func TestRunOccupiesWorkerSlotSemantics(t *testing.T) {
	now := time.Now().UTC()
	live := now.Add(time.Minute).Format(time.RFC3339)
	stale := now.Add(-time.Minute).Format(time.RFC3339)

	cases := []struct {
		name string
		run  RuntimeRun
		want bool
	}{
		{"running live normal", RuntimeRun{Status: "running", LeaseExpiresAt: live, SlotClass: "normal"}, true},
		{"running live default slot class", RuntimeRun{Status: "running", LeaseExpiresAt: live}, true},
		{"queued never occupies", RuntimeRun{Status: "queued", LeaseExpiresAt: live}, false},
		{"terminal never occupies", RuntimeRun{Status: "succeeded", LeaseExpiresAt: live}, false},
		{"expired lease does not occupy", RuntimeRun{Status: "running", LeaseExpiresAt: stale}, false},
		{"empty lease does not occupy", RuntimeRun{Status: "running"}, false},
		{"bad lease does not occupy", RuntimeRun{Status: "running", LeaseExpiresAt: "not-a-time"}, false},
		{"readonly exempt even when live", RuntimeRun{Status: "running", LeaseExpiresAt: live, SlotClass: "readonly"}, false},
		{"READONLY case-insensitive exempt", RuntimeRun{Status: "running", LeaseExpiresAt: live, SlotClass: "READONLY"}, false},
		{"unknown slot class fails closed to occupying", RuntimeRun{Status: "running", LeaseExpiresAt: live, SlotClass: "weird"}, true},
	}
	for _, tc := range cases {
		if got := RunOccupiesWorkerSlot(tc.run, now); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// A1: 50 goroutines race to claim one queued run — exactly one wins.
func TestClaimRuntimeRunConcurrentSingleWinner(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	if err := store.UpsertRuntimeRun(slotRun("run-a1", "aw-1", "queued")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const n = 50
	winners := make([]int, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if found {
				mu.Lock()
				winners[i] = 1
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, w := range winners {
		total += w
	}
	if total != 1 {
		t.Fatalf("winners = %d, want exactly 1", total)
	}
	run, found, err := store.RuntimeRunByID("ws-slot", "run-a1")
	if err != nil || !found {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != "running" || run.LeaseGeneration != 1 {
		t.Fatalf("winner state: status=%s generation=%d", run.Status, run.LeaseGeneration)
	}
}

// A2: two queued runs of the SAME worker race — only one executes; the other
// stays queued because the first claim occupies the worker slot.
func TestClaimRuntimeRunSameWorkerOneSlot(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	for _, id := range []string{"run-1", "run-2"} {
		if err := store.UpsertRuntimeRun(slotRun(id, "aw-1", "queued")); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	first, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil || !found {
		t.Fatalf("first claim: %v", err)
	}
	second, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if found {
		t.Fatalf("second claim of same worker must not run while the first holds the slot, got %+v", second)
	}
	remaining := []string{"run-1", "run-2"}
	for _, id := range remaining {
		run, found, _ := store.RuntimeRunByID("ws-slot", id)
		if !found {
			continue
		}
		if run.ID == first.ID && run.Status != "running" {
			t.Fatalf("claimed run %s must be running", run.ID)
		}
		if run.ID != first.ID && run.Status != "queued" {
			t.Fatalf("unclaimed run %s must stay queued, got %s", run.ID, run.Status)
		}
	}
}

// A3: different workers claim in parallel — both succeed.
func TestClaimRuntimeRunDifferentWorkersParallel(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	if err := store.UpsertRuntimeRun(slotRun("run-w1", "aw-1", "queued")); err != nil {
		t.Fatalf("seed w1: %v", err)
	}
	if err := store.UpsertRuntimeRun(slotRun("run-w2", "aw-2", "queued")); err != nil {
		t.Fatalf("seed w2: %v", err)
	}
	var (
		mu       sync.Mutex
		got1     bool
		got2     bool
		wg       sync.WaitGroup
		_        = &mu
		sawState = map[string]bool{}
	)
	claim := func(expect string) {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			run, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if !found {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			mu.Lock()
			sawState[run.ID] = true
			if run.ID == "run-w1" {
				got1 = true
			}
			if run.ID == "run-w2" {
				got2 = true
			}
			mu.Unlock()
			return
		}
		_ = expect
	}
	wg.Add(2)
	go claim("run-w1")
	go claim("run-w2")
	wg.Wait()
	if !got1 || !got2 {
		t.Fatalf("both workers must claim their own run: w1=%v w2=%v state=%v", got1, got2, sawState)
	}
}

// A4: an expired-lease run is taken over by another node with generation+1;
// the old owner's stale renew and finish (old generation) take no effect.
func TestClaimRuntimeRunTakeoverBumpsGeneration(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	slotNode(t, store, "node-b")
	if err := store.UpsertRuntimeRun(slotRun("run-take", "aw-1", "queued")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil || !found {
		t.Fatalf("first claim: %v", err)
	}
	// Expire the lease (simulate node-a dying).
	expired := first
	expired.LeaseExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := store.UpsertRuntimeRun(expired); err != nil {
		t.Fatalf("expire: %v", err)
	}
	taken, found, err := store.ClaimRuntimeRun("ws-slot", "node-b", 60, nil)
	if err != nil || !found {
		t.Fatalf("takeover claim: %v", err)
	}
	if taken.RuntimeNodeID != "node-b" {
		t.Fatalf("takeover node = %s", taken.RuntimeNodeID)
	}
	if taken.LeaseGeneration != first.LeaseGeneration+1 {
		t.Fatalf("takeover generation = %d, want %d", taken.LeaseGeneration, first.LeaseGeneration+1)
	}
	// Old owner's renewal: rejected.
	if _, _, err := store.ExtendRuntimeRunLeaseWithGeneration("ws-slot", "run-take", "node-a", first.LeaseGeneration, 60); !LeaseGenerationMismatch(err) {
		t.Fatalf("stale renew: err=%v, want mismatch", err)
	}
	// Old owner's finish: rejected and the run stays running under node-b.
	if _, _, err := store.FinishRuntimeRun("ws-slot", "run-take", "node-a", first.LeaseGeneration, "succeeded", "", "", "{}"); !LeaseGenerationMismatch(err) {
		t.Fatalf("stale finish: err=%v, want mismatch", err)
	}
	check, _, _ := store.RuntimeRunByID("ws-slot", "run-take")
	if check.Status != "running" || check.RuntimeNodeID != "node-b" {
		t.Fatalf("run resurrected by stale finish: %+v", check)
	}
}

// A5: an unexpired lease is never taken over by another node.
func TestClaimRuntimeRunDoesNotTakeLiveLease(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	slotNode(t, store, "node-b")
	if err := store.UpsertRuntimeRun(slotRun("run-live", "aw-1", "queued")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil); err != nil || !found {
		t.Fatalf("first claim: %v", err)
	}
	if _, found, err := store.ClaimRuntimeRun("ws-slot", "node-b", 60, nil); err != nil {
		t.Fatalf("second claim: %v", err)
	} else if found {
		t.Fatal("live lease must not be taken over")
	}
}

// A10: same-node stale loop (old generation) renew/finish take no effect.
func TestStaleSameNodeGenerationRejected(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	if err := store.UpsertRuntimeRun(slotRun("run-gen", "aw-1", "queued")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil || !found {
		t.Fatalf("claim: %v", err)
	}
	// Simulate re-takeover by the SAME node (lease expired, re-claimed).
	expired := first
	expired.LeaseExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := store.UpsertRuntimeRun(expired); err != nil {
		t.Fatalf("expire: %v", err)
	}
	second, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil || !found {
		t.Fatalf("re-claim: %v", err)
	}
	if second.LeaseGeneration != first.LeaseGeneration+1 {
		t.Fatalf("re-claim generation = %d, want %d", second.LeaseGeneration, first.LeaseGeneration+1)
	}
	// Old generation on the same node: renew and finish both rejected.
	if _, _, err := store.ExtendRuntimeRunLeaseWithGeneration("ws-slot", "run-gen", "node-a", first.LeaseGeneration, 60); !LeaseGenerationMismatch(err) {
		t.Fatalf("stale renew same node: err=%v, want mismatch", err)
	}
	if _, _, err := store.FinishRuntimeRun("ws-slot", "run-gen", "node-a", first.LeaseGeneration, "succeeded", "", "", "{}"); !LeaseGenerationMismatch(err) {
		t.Fatalf("stale finish same node: err=%v, want mismatch", err)
	}
	// Generation 0 (pre-Q0 node): rejected fail-closed.
	if _, _, err := store.ExtendRuntimeRunLeaseWithGeneration("ws-slot", "run-gen", "node-a", 0, 60); !LeaseGenerationMismatch(err) {
		t.Fatalf("generation 0 renew: err=%v, want mismatch", err)
	}
	if _, _, err := store.FinishRuntimeRun("ws-slot", "run-gen", "node-a", 0, "succeeded", "", "", "{}"); !LeaseGenerationMismatch(err) {
		t.Fatalf("generation 0 finish: err=%v, want mismatch", err)
	}
	if _, _, err := store.ExtendRuntimeRunLease("ws-slot", "run-gen", "node-a", 60); err != nil {
		t.Fatalf("legacy ExtendRuntimeRunLease must stay available for the interface, got %v", err)
	}
}

// A11: a slot_class=readonly fork run does not block the next run of the same
// worker.
func TestClaimRuntimeRunReadonlyForkExempt(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	holder := slotRun("run-fork-ro", "aw-1", "running")
	holder.SlotClass = "readonly"
	holder.LeaseExpiresAt = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	if err := store.UpsertRuntimeRun(holder); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	if err := store.UpsertRuntimeRun(slotRun("run-next", "aw-1", "queued")); err != nil {
		t.Fatalf("seed next: %v", err)
	}
	claimed, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil || !found {
		t.Fatalf("claim next run: %v", err)
	}
	if claimed.ID != "run-next" {
		t.Fatalf("claimed %s, want run-next (readonly fork must exempt the slot)", claimed.ID)
	}
}

// A12: a slot_class=normal fork run blocks the same worker — including the
// fail-closed default (unknown classes normalize to occupying).
func TestClaimRuntimeRunNormalForkOccupies(t *testing.T) {
	for _, slotClass := range []string{"normal", "", "weird"} {
		store := newSlotTestStore(t)
		slotNode(t, store, "node-a")
		holder := slotRun("run-fork-n", "aw-1", "running")
		holder.SlotClass = slotClass
		holder.LeaseExpiresAt = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
		if err := store.UpsertRuntimeRun(holder); err != nil {
			t.Fatalf("seed holder: %v", err)
		}
		if err := store.UpsertRuntimeRun(slotRun("run-next", "aw-1", "queued")); err != nil {
			t.Fatalf("seed next: %v", err)
		}
		_, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if found {
			t.Fatalf("slot_class=%q: same-worker claim must be blocked while a normal-class run holds the slot", slotClass)
		}
		// The queued candidate must still be queued, not consumed.
		next, _, _ := store.RuntimeRunByID("ws-slot", "run-next")
		if next.Status != "queued" {
			t.Fatalf("slot_class=%q: queued run must remain queued, got %s", slotClass, next.Status)
		}
	}
}

// Reaper primitives: ListExpiredRunningRuns honors the sole lease criterion
// (no node-health involvement) and ReapExpiredRuntimeRun is conditional on
// lease + generation.
func TestReapExpiredRuntimeRunConditional(t *testing.T) {
	store := newSlotTestStore(t)
	slotNode(t, store, "node-a")
	if err := store.UpsertRuntimeRun(slotRun("run-reap", "aw-1", "queued")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claimed, found, err := store.ClaimRuntimeRun("ws-slot", "node-a", 60, nil)
	if err != nil || !found {
		t.Fatalf("claim: %v", err)
	}
	cutoff := time.Now().UTC()
	// Lease still live: nothing listed, nothing reaped.
	live, err := store.ListExpiredRunningRuns("ws-slot", cutoff, 100)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live lease must not be listed as expired: %+v", live)
	}
	// Expire the lease.
	expiredAt := time.Now().UTC().Add(-5 * time.Minute)
	expired := claimed
	expired.LeaseExpiresAt = expiredAt.Format(time.RFC3339)
	if err := store.UpsertRuntimeRun(expired); err != nil {
		t.Fatalf("expire: %v", err)
	}
	listed, err := store.ListExpiredRunningRuns("ws-slot", cutoff, 100)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "run-reap" {
		t.Fatalf("expired list = %+v, want run-reap", listed)
	}
	// Reap with a stale generation loses; with the current one wins.
	ok, err := store.ReapExpiredRuntimeRun("ws-slot", "run-reap", claimed.LeaseGeneration-1, cutoff)
	if err != nil || ok {
		t.Fatalf("stale-generation reap must not apply: ok=%v err=%v", ok, err)
	}
	if _, err := store.ReapExpiredRuntimeRun("ws-slot", "run-reap", claimed.LeaseGeneration, cutoff); err != nil {
		t.Fatalf("reap: %v", err)
	}
	final, _, _ := store.RuntimeRunByID("ws-slot", "run-reap")
	if final.Status != "failed" || final.ErrorCode != "lease_expired" {
		t.Fatalf("reaped run state: %+v", final)
	}
	if final.LeaseGeneration != claimed.LeaseGeneration+1 {
		t.Fatalf("reap must bump generation: %d", final.LeaseGeneration)
	}
	if final.FinishedAt == "" {
		t.Fatalf("finishedAt unset on reaped run")
	}
}

// LeaseExpiredCutoff: grace counts from lease expiry (now - grace).
func TestLeaseExpiredCutoff(t *testing.T) {
	now := time.Now().UTC()
	cutoff := LeaseExpiredCutoff(180*time.Second, now)
	if d := now.Sub(cutoff); d < 179*time.Second || d > 181*time.Second {
		t.Fatalf("cutoff offset = %v, want ~180s", d)
	}
}
