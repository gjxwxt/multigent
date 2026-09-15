package db

import (
	"path/filepath"
	"strconv"
	"testing"
)

// GPT re-review Q2: updated_at has SECOND precision, so two CAS writes within
// the same wall-clock second used to produce the same revision and a stale
// claimant's CAS could win against a fresher one. The revision is now a
// monotonic counter bumped on EVERY kv_records write; these tests pin that.
func TestRecordRevisionIsMonotonicAcrossSameSecondWrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "revision.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertWorkspace(Workspace{ID: "ws", Name: "ws"}); err != nil {
		t.Fatalf("ws: %v", err)
	}
	key := []string{"p", "t", "r"}
	if err := s.UpsertRecord("workflow_runs", "ws", key, "v1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rev1, found, err := s.RecordRevision("workflow_runs", "ws", key)
	if err != nil || !found {
		t.Fatalf("revision 1: found=%v err=%v", found, err)
	}
	// Write again immediately — same wall-clock second in almost all runs.
	if err := s.UpsertRecord("workflow_runs", "ws", key, "v2"); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	rev2, found, _ := s.RecordRevision("workflow_runs", "ws", key)
	if !found {
		t.Fatal("revision 2 missing")
	}
	if rev1 == rev2 {
		t.Fatalf("revision must change on every write even within one second: %s == %s (second-precision updated_at regression)", rev1, rev2)
	}
	n1, err1 := strconv.Atoi(rev1)
	n2, err2 := strconv.Atoi(rev2)
	if err1 != nil || err2 != nil || n2 <= n1 {
		t.Fatalf("revision must be strictly monotonic integers: %s -> %s (errs %v/%v)", rev1, rev2, err1, err2)
	}

	// A CAS conditioned on the OLD revision must now fail (the row moved).
	swapped, err := s.UpdateRecordIfRevision("workflow_runs", "ws", key, "stale", rev1)
	if err != nil {
		t.Fatalf("stale CAS: %v", err)
	}
	if swapped {
		t.Fatal("CAS with a stale revision must not swap")
	}
	payload, found, _ := s.GetRecord("workflow_runs", "ws", key)
	if !found || payload != "v2" {
		t.Fatalf("failed CAS must leave the row untouched, got %q (found=%v)", payload, found)
	}

	// A CAS with the CURRENT revision succeeds and bumps the counter again.
	swapped, err = s.UpdateRecordIfRevision("workflow_runs", "ws", key, "v3", rev2)
	if err != nil || !swapped {
		t.Fatalf("current-revision CAS: swapped=%v err=%v", swapped, err)
	}
	rev3, _, _ := s.RecordRevision("workflow_runs", "ws", key)
	if rev3 == rev2 {
		t.Fatalf("successful CAS must bump the revision: %s == %s", rev3, rev2)
	}

	// The payload-aware variant obeys both witnesses: right revision, wrong
	// payload → no swap.
	swapped, err = s.UpdateRecordIfPayloadAndRevision("workflow_runs", "ws", key, "wrong-witness", "not-the-payload", rev3)
	if err != nil || swapped {
		t.Fatalf("payload-aware CAS with wrong payload witness: swapped=%v err=%v", swapped, err)
	}
	swapped, err = s.UpdateRecordIfPayloadAndRevision("workflow_runs", "ws", key, "v4", "v3", rev3)
	if err != nil || !swapped {
		t.Fatalf("payload-aware CAS with both witnesses: swapped=%v err=%v", swapped, err)
	}
}
