package db

import (
	"context"
	"database/sql"
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

// guardedTestStoreAt opens a store at an explicit path so a second handle can
// open the same file (the cross-connection lock test needs two connections to
// one database).
func guardedTestStoreAt(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
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

// GPT re-review round 4 P1: the guarded commit's comment promised BEGIN
// IMMEDIATE, but db.sql.Begin() issues a plain deferred BEGIN (the driver's
// _txlock only applies to its own Tx wrapper) — no RESERVED lock until the
// first statement, so "the claim check and every write are serialized against
// any other transition commit" was not actually guaranteed across processes.
// The fix runs the transaction on a dedicated connection with an explicit
// "BEGIN IMMEDIATE" (runImmediateTx). No URI-level _txlock default: that
// would be a global behavior change across every other Begin() call site
// (GPT round 4.5).
//
// The lock-acquisition proof must be DISCRIMINATING (GPT round 4.5 rejected
// the first attempt: probing after the transaction body had already executed
// an INSERT proves nothing — a deferred transaction also holds the write lock
// from its first statement onward). Both tests below probe at the only
// instant that separates the two modes: BEGIN has returned, ZERO statements
// of the body have run. At that instant IMMEDIATE must block the second
// handle (Busy) and deferred must let it through — so the first test fails
// on any regression to deferred, and the second documents the control.

// runDeferredTxForTest is the deferred control twin of runImmediateTxNotify:
// plain BEGIN, same onBegun seam. Test-only — production code has exactly one
// transaction wrapper and it is immediate.
func runDeferredTxForTest(conn *sql.Conn, onBegun func(), fn func(tx *immediateTx) error) error {
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	if onBegun != nil {
		onBegun()
	}
	if err := fn(&immediateTx{conn: conn}); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	_, err := conn.ExecContext(ctx, "COMMIT")
	return err
}

func TestImmediateTxHoldsWriteLockBeforeFirstStatement(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guarded-shared.db")
	s := guardedTestStoreAt(t, dbPath)
	runKey := []string{"p", "t", "run-1"}
	if err := s.UpsertRecord("workflow_runs", "ws", runKey, `{"id":"run-1"}`); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// Second independent handle with a SHORT busy timeout: the probe must
	// fail fast on the held lock, not queue behind the full 5s default.
	probe, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(100)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open probe handle: %v", err)
	}
	defer probe.Close()

	conn, err := s.sql.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	// beginGate closes the instant BEGIN IMMEDIATE returned and BEFORE the
	// body executes any SQL; releaseGate unblocks the body.
	beginGate := make(chan struct{})
	releaseGate := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- runImmediateTxNotify(conn, func() {
			close(beginGate)
			<-releaseGate
		}, func(tx *immediateTx) error {
			_, err := tx.Exec(`UPDATE kv_records SET payload = '{"touched":true}' WHERE table_name = 'workflow_runs' AND workspace_id = 'ws' AND k1 = 'p' AND k2 = 't' AND k3 = 'run-1'`)
			return err
		})
	}()

	<-beginGate
	// PROBE at the BEGIN-done / zero-SQL instant: the RESERVED lock must
	// already be held, so the probe write must fail Busy. If runImmediateTx
	// ever regresses to a deferred BEGIN, this write SUCCEEDS and the test
	// fails — that is the discriminating property GPT required.
	_, err = probe.Exec(`UPDATE kv_records SET payload = '{"probe":1}' WHERE table_name = 'workflow_runs' AND workspace_id = 'ws' AND k1 = 'p' AND k2 = 't' AND k3 = 'run-1'`)
	if err == nil {
		t.Fatal("second handle wrote while an IMMEDIATE transaction was open with ZERO statements executed — BEGIN IMMEDIATE is not acquiring the write lock up front (deferred regression)")
	}
	if !isSQLiteBusyErr(err) {
		t.Fatalf("probe failed with an unexpected error (want busy): %v", err)
	}
	close(releaseGate)
	if err := <-errCh; err != nil {
		t.Fatalf("IMMEDIATE transaction must commit after the probe: %v", err)
	}
	// After the transaction committed, the probe handle writes normally.
	if _, err := probe.Exec(`UPDATE kv_records SET payload = '{"probe":2}' WHERE table_name = 'workflow_runs' AND workspace_id = 'ws' AND k1 = 'p' AND k2 = 't' AND k3 = 'run-1'`); err != nil {
		t.Fatalf("probe write after COMMIT must succeed: %v", err)
	}
}

// Deferred control group (GPT round 4.5 item 3): at the SAME instant — BEGIN
// returned, zero statements executed — a plain deferred transaction holds NO
// write lock, so the second handle's write goes through. This is what makes
// TestImmediateTxHoldsWriteLockBeforeFirstStatement meaningful: the two tests
// differ ONLY in BEGIN vs BEGIN IMMEDIATE, and their outcomes must differ.
func TestDeferredTxAllowsConcurrentWriteBeforeFirstStatement(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guarded-shared.db")
	s := guardedTestStoreAt(t, dbPath)
	runKey := []string{"p", "t", "run-1"}
	if err := s.UpsertRecord("workflow_runs", "ws", runKey, `{"id":"run-1"}`); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	probe, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(100)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open probe handle: %v", err)
	}
	defer probe.Close()

	conn, err := s.sql.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	beginGate := make(chan struct{})
	releaseGate := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- runDeferredTxForTest(conn, func() {
			close(beginGate)
			<-releaseGate
		}, func(tx *immediateTx) error {
			return nil // zero statements: the deferred tx holds no lock yet
		})
	}()

	<-beginGate
	// Deferred + no statements: the probe write must SUCCEED.
	if _, err := probe.Exec(`UPDATE kv_records SET payload = '{"probe":1}' WHERE table_name = 'workflow_runs' AND workspace_id = 'ws' AND k1 = 'p' AND k2 = 't' AND k3 = 'run-1'`); err != nil {
		t.Fatalf("probe write against an open DEFERRED transaction with zero statements must succeed, got: %v", err)
	}
	close(releaseGate)
	if err := <-errCh; err != nil {
		t.Fatalf("deferred control transaction must commit: %v", err)
	}
}

// isSQLiteBusyErr reports whether err is SQLite's database-locked/busy error
// (modernc wraps SQLITE_BUSY as "database is locked" (5) / "database table is
// locked" (6)).
func isSQLiteBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "database table is locked") || strings.Contains(msg, "SQLITE_BUSY")
}
