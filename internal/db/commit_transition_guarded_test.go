package db

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// GPT re-review round 3 P0: the claim gate must also guard the SUCCESS path.
// A transitioner whose claim expired and was stolen must not be able to walk
// the success path and write its stale step instance / event / run state on
// top of the winner's. CommitTransitionGuarded verifies the claim owner
// inside one BEGIN IMMEDIATE transaction and commits the whole batch or
// nothing.

func guardedTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "guarded.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertWorkspace(Workspace{ID: "ws", Name: "ws"}); err != nil {
		t.Fatalf("ws: %v", err)
	}
	return s
}

func guardedClaimMarker(claimID string) string {
	// Same framing as the workflow package: <prefix><len>:<claimID><nano>:<runJSON>.
	return transitionClaimMarkerPrefix + strconv.Itoa(len(claimID)) + ":" + claimID + "1700000000000000000:" + `{"id":"run-1"}`
}

func TestCommitTransitionGuardedCommitOwnerSucceeds(t *testing.T) {
	s := guardedTestStore(t)
	runKey := []string{"p", "t", "run-1"}
	if err := s.UpsertRecord("workflow_runs", "ws", runKey, guardedClaimMarker("claim-A")); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	writes := []KVWrite{
		{Table: "workflow_step_instances", Workspace: "ws", Key: []string{"run-1", "s1", "wfsi-1"}, Payload: `{"status":"completed"}`},
		{Table: "workflow_step_events", Workspace: "ws", Key: []string{"run-1", "wfse-1"}, Payload: `{"status":"completed"}`},
		{Table: "workflow_runs", Workspace: "ws", Key: runKey, Payload: `{"id":"run-1","status":"active","activeStepId":"s2"}`},
	}
	if err := s.CommitTransitionGuarded("ws", runKey, "claim-A", writes); err != nil {
		t.Fatalf("owner commit must succeed: %v", err)
	}
	if payload, found, _ := s.GetRecord("workflow_runs", "ws", runKey); !found || strings.Contains(payload, transitionClaimMarkerPrefix) {
		t.Fatalf("run payload after commit = %q, want the plain run payload (marker replaced)", payload)
	}
	if _, found, _ := s.GetRecord("workflow_step_instances", "ws", []string{"run-1", "s1", "wfsi-1"}); !found {
		t.Fatal("instance write missing")
	}
	if _, found, _ := s.GetRecord("workflow_step_events", "ws", []string{"run-1", "wfse-1"}); !found {
		t.Fatal("event write missing")
	}
}

func TestCommitTransitionGuardedRejectsStolenClaimAtomically(t *testing.T) {
	s := guardedTestStore(t)
	runKey := []string{"p", "t", "run-1"}
	// B stole the claim: the row now holds B's marker.
	if err := s.UpsertRecord("workflow_runs", "ws", runKey, guardedClaimMarker("claim-B")); err != nil {
		t.Fatalf("seed B claim: %v", err)
	}
	// A (stale owner) tries to commit its full success batch.
	writes := []KVWrite{
		{Table: "workflow_step_instances", Workspace: "ws", Key: []string{"run-1", "s1", "wfsi-1"}, Payload: `{"status":"completed"}`},
		{Table: "workflow_step_events", Workspace: "ws", Key: []string{"run-1", "wfse-A"}, Payload: `{"status":"completed"}`},
		{Table: "workflow_runs", Workspace: "ws", Key: runKey, Payload: `{"id":"run-1","status":"active","activeStepId":"sA"}`},
	}
	err := s.CommitTransitionGuarded("ws", runKey, "claim-A", writes)
	if !errors.Is(err, ErrTransitionClaimLost) {
		t.Fatalf("stale owner commit must fail with ErrTransitionClaimLost, got %v", err)
	}
	// ATOMICITY: none of A's writes landed — not the event, not the instance,
	// and the run row still holds B's marker untouched.
	if payload, found, _ := s.GetRecord("workflow_runs", "ws", runKey); !found || !strings.Contains(payload, "claim-B") {
		t.Fatalf("run row must still hold B's marker, got %q (found=%v)", payload, found)
	}
	if _, found, _ := s.GetRecord("workflow_step_instances", "ws", []string{"run-1", "s1", "wfsi-1"}); found {
		t.Fatal("stale owner's instance write must be rolled back")
	}
	if _, found, _ := s.GetRecord("workflow_step_events", "ws", []string{"run-1", "wfse-A"}); found {
		t.Fatal("stale owner's event write must be rolled back")
	}
}

// A plain (non-marker) run payload also refuses: the transition already
// finished (the winner's final write replaced the marker), so the stale
// owner's commit is a double-dispatch.
func TestCommitTransitionGuardedRejectsPlainPayload(t *testing.T) {
	s := guardedTestStore(t)
	runKey := []string{"p", "t", "run-1"}
	if err := s.UpsertRecord("workflow_runs", "ws", runKey, `{"id":"run-1","status":"active","activeStepId":"s2"}`); err != nil {
		t.Fatalf("seed plain payload: %v", err)
	}
	err := s.CommitTransitionGuarded("ws", runKey, "claim-A", []KVWrite{
		{Table: "workflow_runs", Workspace: "ws", Key: runKey, Payload: `{"id":"run-1"}`},
	})
	if !errors.Is(err, ErrTransitionClaimLost) {
		t.Fatalf("commit over a plain payload must be refused, got %v", err)
	}
}

// The batch is atomic even against unrelated write failures: a batch whose
// second write is invalid (over-long key part is fine — use a nil workspace
// to force a constraint path is fragile; instead verify all-or-nothing via a
// batch where the last write errors by closing the store mid-flight is also
// fragile — the honest atomicity proof is the stolen-claim test above plus
// the transaction wrapper itself; this test pins the empty-batch guard).
func TestCommitTransitionGuardedEmptyBatchRefused(t *testing.T) {
	s := guardedTestStore(t)
	if err := s.CommitTransitionGuarded("ws", []string{"p", "t", "r"}, "claim-A", nil); err == nil {
		t.Fatal("empty batch must be refused")
	}
}
