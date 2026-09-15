package api

import (
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Q1 stamp-race barrier tests (GPT review P0-1): the enqueue inserts a run as
// 'preparing' — invisible to ClaimRuntimeRun — and promotes it to 'queued'
// ONLY after the task token stamp lands. These tests pin both halves of that
// contract at the DB layer, where the interleaving is fully controllable.

// Barrier: a run sitting in the insert→stamp window (status=preparing) must
// NEVER be claimable, no matter how many claim rounds fire. This is the exact
// interleaving GPT flagged: node claims between insert and stamp would
// execute work whose finish the task fence then drops.
func TestPreparingRunIsNeverClaimable(t *testing.T) {
	store := newRuntimeRunsBarrierDB(t)
	run := seedingRun(controldb.RuntimeRun{
		ID: "rtrun-barrier", RunKey: "k-barrier", Status: "preparing",
	})
	if err := store.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("insert preparing run: %v", err)
	}

	nodeID := "rtn-barrier"
	for round := 0; round < 5; round++ {
		claimed, found, err := store.ClaimRuntimeRun("ws", nodeID, 90, nil)
		if err != nil {
			t.Fatalf("claim round %d: %v", round, err)
		}
		if found {
			t.Fatalf("node claimed a PREPARING run (%s) — the insert→stamp window leaked", claimed.ID)
		}
	}

	// The promote is the only door out of preparing: after it, claim succeeds.
	if _, promoted, err := store.PromotePreparingRuntimeRun("ws", "rtrun-barrier"); err != nil || !promoted {
		t.Fatalf("promote: promoted=%v err=%v", promoted, err)
	}
	claimed, found, err := store.ClaimRuntimeRun("ws", nodeID, 90, nil)
	if err != nil || !found {
		t.Fatalf("claim after promote: found=%v err=%v", found, err)
	}
	if claimed.ID != "rtrun-barrier" || claimed.Status != "running" {
		t.Fatalf("claimed %s status=%s, want the promoted run running", claimed.ID, claimed.Status)
	}
}

// Stamp-failure cleanup: FailQueuedRuntimeRun must retire a preparing run
// outright (it can never have been claimed — there is nothing to leave
// running) so its run_key frees up and no node ever sees it.
func TestFailQueuedRuntimeRunCoversPreparing(t *testing.T) {
	store := newRuntimeRunsBarrierDB(t)
	run := seedingRun(controldb.RuntimeRun{
		ID: "rtrun-stampf", RunKey: "k-stampf", Status: "preparing",
	})
	if err := store.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("insert: %v", err)
	}

	failed, failedIt, err := store.FailQueuedRuntimeRun("ws", "rtrun-stampf", "token_stamp_failed", "stamp failed")
	if err != nil || !failedIt {
		t.Fatalf("fail preparing run: failedIt=%v err=%v", failedIt, err)
	}
	if failed.Status != "failed" || failed.ErrorCode != "token_stamp_failed" {
		t.Fatalf("failed run = %+v", failed)
	}
	if _, found, err := store.ClaimRuntimeRun("ws", "rtn-x", 90, nil); err != nil || found {
		t.Fatalf("failed run must be unclaimable: found=%v err=%v", found, err)
	}
	// run_key freed: ActiveRuntimeRunByKey no longer converges on it.
	if _, found, _ := store.ActiveRuntimeRunByKey("ws", "k-stampf"); found {
		t.Fatal("failed run must not count as active for dedupe")
	}
}

// Idempotent join at the DB layer: under the preparing-unique index a second
// enqueue of the same intent is rejected at its own INSERT — it can never
// mint a second run, let alone reach a second token stamp. The winner lookup
// (OtherActiveRuntimeRunByKey) retains the SQL-level self-exclusion for the
// legacy promote-collision path (same-second created_at ties are broken by
// RANDOM id, so a caller-side loop can keep picking its own row forever).
func TestOtherActiveRuntimeRunByKeyExcludesSelf(t *testing.T) {
	store := newRuntimeRunsBarrierDB(t)
	winner := seedingRun(controldb.RuntimeRun{
		ID: "rtrun-winner", RunKey: "k-join", Status: "queued",
	})
	if err := store.UpsertRuntimeRun(winner); err != nil {
		t.Fatalf("insert winner: %v", err)
	}
	dup := seedingRun(controldb.RuntimeRun{
		ID: "rtrun-dup", RunKey: "k-join", Status: "preparing",
	})
	if _, inserted, err := store.UpsertRuntimeRunIdempotent(dup); err != nil {
		t.Fatalf("insert dup: %v", err)
	} else if inserted {
		t.Fatal("second enqueue of the same intent must be REJECTED by the preparing-unique index, not inserted")
	}

	// The rejected insert returns the winner — the join happens before any
	// stamp can fire.
	stored, found, _ := store.ActiveRuntimeRunByKey("ws", "k-join")
	if !found || stored.ID != "rtrun-winner" {
		t.Fatalf("key lookup = %s, want the winner", stored.ID)
	}
	runs, _ := store.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: "ws"})
	keyRuns := 0
	for _, r := range runs {
		if r.RunKey == "k-join" {
			keyRuns++
		}
	}
	if keyRuns != 1 {
		t.Fatalf("runs for key = %d, want exactly 1 (second insert must not create a row)", keyRuns)
	}
	got, found, err := store.OtherActiveRuntimeRunByKey("ws", "k-join", "rtrun-dup")
	if err != nil || !found {
		t.Fatalf("other lookup: found=%v err=%v", found, err)
	}
	if got.ID != "rtrun-winner" {
		t.Fatalf("winner = %s, want rtrun-winner (self-exclusion failed)", got.ID)
	}
	// Excluding the winner finds nothing — no second row exists to find.
	if _, found, _ := store.OtherActiveRuntimeRunByKey("ws", "k-join", "rtrun-winner"); found {
		t.Fatal("excluding the winner must find nothing — the index guarantees one row per key")
	}
}

// Promote conditionality: only a preparing row promotes; a row already
// claimed (running) must never be reset to queued by a late promote.
func TestPromoteOnlyAppliesToPreparing(t *testing.T) {
	store := newRuntimeRunsBarrierDB(t)
	run := seedingRun(controldb.RuntimeRun{
		ID: "rtrun-prom", RunKey: "k-prom", Status: "preparing",
	})
	if err := store.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, ok, err := store.ClaimRuntimeRun("ws", "rtn-prom", 90, nil); err != nil || ok {
		t.Fatalf("preparing run claimed: ok=%v err=%v", ok, err)
	}
	// The claim can't have happened; force the row to running the way the
	// finish/lease machinery would, then verify promote refuses it.
	if err := store.UpsertRuntimeRun(controldb.RuntimeRun{
		ID: "rtrun-prom", WorkspaceID: "ws", ProjectID: "p", AgentID: "a",
		TaskID: "t", Status: "running", RunKey: "k-prom",
		LeaseExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
		LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("force running: %v", err)
	}
	if _, promoted, err := store.PromotePreparingRuntimeRun("ws", "rtrun-prom"); err != nil || promoted {
		t.Fatalf("promote of running run must be refused: promoted=%v err=%v", promoted, err)
	}
}

// newRuntimeRunsBarrierDB opens an isolated store with the workspace seeded.
func newRuntimeRunsBarrierDB(t *testing.T) controldb.Store {
	t.Helper()
	s, err := controldb.Open(filepath.Join(t.TempDir(), "barrier.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "ws"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return s
}

// seedingRun fills the required identity columns around the caller's fields.
func seedingRun(base controldb.RuntimeRun) controldb.RuntimeRun {
	if base.WorkspaceID == "" {
		base.WorkspaceID = "ws"
	}
	if base.ProjectID == "" {
		base.ProjectID = "p"
	}
	if base.AgentID == "" {
		base.AgentID = "a"
	}
	if base.TaskID == "" {
		base.TaskID = "t"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	base.CreatedAt = now
	base.UpdatedAt = now
	return base
}

// The entity import guard: task status types are part of the run contract.
var _ = entity.TaskStatusPending
