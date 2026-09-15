package api

import (
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/taskstore"
)

// GPT re-review P0-1 barrier test: the second enqueue of the same intent must
// NEVER insert its own row (preparing or otherwise) — the preparing-unique
// index rejects it at the initial INSERT, so no second token stamp can ever
// start. The exact interleaving: A promotes to queued, then B enqueues while
// a node claims A. Invariants asserted:
//
//  1. B's enqueue returns A's run; the DB holds exactly ONE row for the key
//     (B's rejected INSERT left no superseded/failed residue).
//  2. The task's execution token names A — B never wrote its own token
//     (it never got far enough to stamp).
//  3. A, once claimed by a node and finished through the normal lease path,
//     is NOT dropped by the task fence: the fence accepts A's finish because
//     the token still names A.
func TestEnqueueJoinNeverSecondStampWhileNodeClaimsA(t *testing.T) {
	s, ws := runKeyTestServer(t)
	seedSampleAgentsForTest(t, s, ws)

	now := time.Now().UTC()
	task := &entity.Task{ID: "t-join-barrier", Title: "join barrier", Status: entity.TaskStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// A: real enqueue through the production path — inserts preparing, stamps
	// the task token, promotes to queued.
	runA, err := s.enqueueRuntimeTaskRun(ws, "sample", "pm", task, "", "http://127.0.0.1:1", "admin")
	if err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if runA.RunKey == "" {
		t.Fatal("task run must carry a run_key")
	}

	// Node claims A concurrently with B's enqueue — the interleaving GPT
	// specified.
	nodeID := "rtn-join-barrier"
	claimedA, found, err := s.controlDB.ClaimRuntimeRun(ws, nodeID, 90, nil)
	if err != nil || !found {
		t.Fatalf("node claim A: found=%v err=%v", found, err)
	}
	if claimedA.ID != runA.ID {
		t.Fatalf("node claimed %s, want A (%s)", claimedA.ID, runA.ID)
	}

	// B: the duplicate dispatch lands while A is already claimed by the node.
	runB, err := s.enqueueRuntimeTaskRun(ws, "sample", "pm", task, "", "http://127.0.0.1:1", "admin")
	if err != nil {
		t.Fatalf("enqueue B must join A, not error: %v", err)
	}
	if runB.ID != runA.ID {
		t.Fatalf("B joined %s, want A %s — a second row got through the unique index", runB.ID, runA.ID)
	}

	// Invariant 1: exactly one row for the intent, and it is A's (running).
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: ws})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	intentRuns := 0
	for _, r := range runs {
		if r.RunKey == runA.RunKey {
			intentRuns++
			if r.ID != runA.ID {
				t.Fatalf("row for intent is %s, want A", r.ID)
			}
		}
	}
	if intentRuns != 1 {
		t.Fatalf("rows for the intent = %d, want exactly 1 (B's insert must not persist)", intentRuns)
	}

	// Invariant 2: the task token names A — B never stamped its own run.
	stored, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil || stored == nil {
		t.Fatalf("load task: %v", err)
	}
	if stored.ActiveRuntimeRunID != runA.ID {
		t.Fatalf("task token = %q, want A %s (B must never write its own token)", stored.ActiveRuntimeRunID, runA.ID)
	}

	// Invariant 3: A finishes through the normal lease path and the task
	// fence ACCEPTS the finish (token names the finishing run) — the
	// duplicate enqueue left A's ownership intact.
	finished, ok, err := s.controlDB.FinishRuntimeRun(ws, runA.ID, nodeID, claimedA.LeaseGeneration, "succeeded", "", "", "{}")
	if err != nil || !ok {
		t.Fatalf("finish A: ok=%v err=%v", ok, err)
	}
	if finished.Status != "succeeded" {
		t.Fatalf("finished status = %s, want succeeded", finished.Status)
	}
	if !s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, runA.ID) {
		t.Fatal("task fence must accept A's finish — the token still names A (a second stamp would have broken this)")
	}
	stored, _ = s.ts.GetTask("sample", "pm", task.ID)
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token after accepted finish = %q, want cleared", stored.ActiveRuntimeRunID)
	}
}

// The stamp-failure leg of the barrier: when A's token stamp fails, its run
// is failed while still preparing (unclaimable), and B can enqueue cleanly
// afterwards — the key is free again, the task never got a token from A.
func TestEnqueueStampFailureFreesKeyForRetry(t *testing.T) {
	s, ws := runKeyTestServer(t)
	seedSampleAgentsForTest(t, s, ws)

	now := time.Now().UTC()
	task := &entity.Task{ID: "t-stamp-fail", Title: "stamp fail", Status: entity.TaskStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// Break the task store so the stamp inside enqueue fails.
	s.ts = &missingTaskStore{Store: s.ts}
	if _, err := s.enqueueRuntimeTaskRun(ws, "sample", "pm", task, "", "http://127.0.0.1:1", "admin"); err == nil {
		t.Fatal("enqueue with a failing stamp must error")
	}
	s.ts = s.ts.(*missingTaskStore).Store

	// The failed run is terminal and unclaimable; the key is free.
	if _, found, _ := s.controlDB.ActiveRuntimeRunByKey(ws, "task:"+ws+":sample:t-stamp-fail"); found {
		t.Fatal("stamp-failed run must not stay active for its key")
	}

	// B retries: a clean enqueue succeeds and takes the token.
	task2 := &entity.Task{ID: "t-stamp-fail", Title: "stamp fail", Status: entity.TaskStatusPending, CreatedAt: now, UpdatedAt: now}
	runB, err := s.enqueueRuntimeTaskRun(ws, "sample", "pm", task2, "", "http://127.0.0.1:1", "admin")
	if err != nil {
		t.Fatalf("retry enqueue after stamp failure: %v", err)
	}
	stored, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil || stored == nil {
		t.Fatalf("load task: %v", err)
	}
	if stored.ActiveRuntimeRunID != runB.ID {
		t.Fatalf("task token = %q, want B %s (the retry owns the stamp)", stored.ActiveRuntimeRunID, runB.ID)
	}
}

// missingTaskStore fails GetTask so setTaskActiveRuntimeRun (the stamp) errors.
type missingTaskStore struct {
	taskstore.Store
}

func (m *missingTaskStore) GetTask(project, agent, id string) (*entity.Task, error) {
	return nil, errStampProbe
}

var errStampProbe = &stampProbeError{}

type stampProbeError struct{}

func (*stampProbeError) Error() string { return "stamp probe: task store unavailable" }
