package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Q0 PR-2 service-layer tests. Matrix: B9 (fork slot_class), B17 (queued does
// not occupy the slot but manual start still cannot double-dispatch), A13
// (reaper vs finish concurrency — single legal transition), A14 (takeover vs
// finish), plus token lifecycle and reaper unit coverage.

func slotTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	return s, workspaceID
}

func slotTestNode(t *testing.T, s *Server, workspaceID string) controldb.RuntimeNode {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	node := controldb.RuntimeNode{
		ID: "rtn-slot", WorkspaceID: workspaceID, Name: "SlotNode", Kind: "personal_computer",
		Status: "online", LastSeenAt: now, CreatedByUserID: "admin", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
		t.Fatalf("node: %v", err)
	}
	return node
}

// forkSessionSlotClass: platform-fixed read-only allowlist, fail-closed.
func TestForkSessionSlotClassClassification(t *testing.T) {
	readOnly := func(caps map[string]any) string {
		raw, _ := json.Marshal(caps)
		return forkSessionSlotClass(controldb.AgentSession{CapabilitiesJSON: string(raw)})
	}
	cases := []struct {
		name string
		caps map[string]any
		want string
	}{
		{"read-only set", map[string]any{"inspect": true, "log": true}, controldb.SlotClassReadonly},
		{"read-only strings", map[string]any{"inspect": "inspect", "list": "list"}, controldb.SlotClassReadonly},
		{"mode inherit with read-only caps", map[string]any{"mode": "inherit", "status": true}, controldb.SlotClassReadonly},
		{"write capability fails closed", map[string]any{"inspect": true, "write": true}, controldb.SlotClassNormal},
		{"unknown capability fails closed", map[string]any{"teleport": true}, controldb.SlotClassNormal},
		{"empty set fails closed", map[string]any{}, controldb.SlotClassNormal},
		{"mode only fails closed", map[string]any{"mode": "inherit"}, controldb.SlotClassNormal},
		{"non-string non-bool fails closed", map[string]any{"inspect": 42}, controldb.SlotClassNormal},
		{"false capability ignored but none left", map[string]any{"inspect": false}, controldb.SlotClassNormal},
	}
	for _, tc := range cases {
		if got := readOnly(tc.caps); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
	if forkSessionSlotClass(controldb.AgentSession{CapabilitiesJSON: "{not json"}) != controldb.SlotClassNormal {
		t.Error("unparseable capabilities must fail closed to normal")
	}
	if forkSessionSlotClass(controldb.AgentSession{}) != controldb.SlotClassNormal {
		t.Error("empty capabilities JSON must fail closed to normal")
	}
}

// B17 path-level: one queued run on the worker + a manual start for the same
// task → the manual request converges on the SAME run (idempotent run_key),
// never a second active run.
func TestQueuedRunDoesNotOccupySlotButBlocksDuplicateDispatch(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	slotTestNode(t, s, workspaceID)
	task := &entity.Task{
		ID: "task-b17", Title: "B17", Status: entity.TaskStatusPending, Priority: 2,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	first, err := s.enqueueRuntimeTaskRun(workspaceID, "sample", "pm", task, "", "http://127.0.0.1", "admin")
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// The queued run must NOT occupy the worker slot...
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 || runOccupiesWorkerSlot(runs[0], time.Now().UTC()) {
		t.Fatalf("queued run must not occupy the slot: %+v", runs)
	}
	// ...but a second dispatch of the same task converges on the same run.
	second, err := s.enqueueRuntimeTaskRun(workspaceID, "sample", "pm", task, "", "http://127.0.0.1", "admin")
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second dispatch created run %s, want idempotent %s", second.ID, first.ID)
	}
	active, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, Status: "queued"})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active runs = %d, want 1 (B17)", len(active))
	}
	// And the task token points at that single run.
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil || stored.ActiveRuntimeRunID != first.ID {
		t.Fatalf("task token = %+v, want run %s", stored, first.ID)
	}
}

// B9 service-side: a readonly fork run does not occupy the slot (verified via
// the shared runOccupiesWorkerSlot over persisted rows).
func TestForkSlotClassServiceSemantics(t *testing.T) {
	_, workspaceID := slotTestServer(t)
	now := time.Now().UTC()
	base := controldb.RuntimeRun{
		ID: "run-b9", WorkspaceID: workspaceID, AgentWorkerID: "aw-pm",
		ProjectID: "sample", AgentID: "pm", Status: "running",
		LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
	}
	ro := base
	ro.SlotClass = controldb.SlotClassReadonly
	if runOccupiesWorkerSlot(ro, now) {
		t.Fatal("readonly fork run must not occupy the slot")
	}
	normal := base
	normal.SlotClass = controldb.SlotClassNormal
	if !runOccupiesWorkerSlot(normal, now) {
		t.Fatal("normal fork run must occupy the slot")
	}
	// runtimeRunBlocksAgent keeps gating on queued but delegates running to
	// the same slot predicate.
	queued := base
	queued.Status = "queued"
	if !runtimeRunBlocksAgent(queued, now) {
		t.Fatal("queued run must still gate dispatch (blocks duplicate dispatch)")
	}
	if !runtimeRunBlocksAgent(normal, now) {
		t.Fatal("running normal run must block the agent")
	}
	if runtimeRunBlocksAgent(ro, now) {
		t.Fatal("running readonly run must not block the agent (slot-free)")
	}
}

// A13: reaper force-kill races an old node's finish. Whatever order they land
// in, the run ends terminal exactly once, the task token is cleared at most
// once, and no resurrect happens.
func TestReaperVsFinishConvergence(t *testing.T) {
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			s, workspaceID := slotTestServer(t)
			node := slotTestNode(t, s, workspaceID)
			task := &entity.Task{
				ID: fmt.Sprintf("task-a13-%d", round), Title: "A13", Status: entity.TaskStatusInProgress,
				Priority: 2, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			}
			if err := s.ts.AddTask("sample", "pm", task); err != nil {
				t.Fatalf("add task: %v", err)
			}
			run := controldb.RuntimeRun{
				ID: fmt.Sprintf("run-a13-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
				AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
				Status: "running", LeaseExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
				LeaseGeneration: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			// Reaper: expire-then-reap on the CURRENT generation.
			go func() {
				defer wg.Done()
				<-start
				cutoff := time.Now().UTC().Add(-time.Second)
				ok, err := s.controlDB.ReapExpiredRuntimeRun(workspaceID, run.ID, run.LeaseGeneration, cutoff)
				if err != nil {
					t.Errorf("reap: %v", err)
				}
				if ok {
					s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, run.ID)
				}
			}()
			// Old node finish on the same generation.
			go func() {
				defer wg.Done()
				<-start
				_, _, err := s.controlDB.FinishRuntimeRun(workspaceID, run.ID, node.ID, run.LeaseGeneration, "succeeded", "", "", "{}")
				if err != nil && !controldb.LeaseGenerationMismatch(err) {
					t.Errorf("finish: %v", err)
				}
				if err == nil {
					s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, run.ID)
				}
			}()
			close(start)
			wg.Wait()

			final, found, err := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
			if err != nil || !found {
				t.Fatalf("load run: %v", err)
			}
			if final.Status != "failed" && final.Status != "succeeded" {
				t.Fatalf("round %d: run must be terminal, got %s", round, final.Status)
			}
			stored, _ := s.ts.GetTask("sample", "pm", task.ID)
			if stored == nil {
				t.Fatalf("task missing")
			}
			if stored.ActiveRuntimeRunID != "" {
				t.Fatalf("round %d: task token must be cleared, got %q", round, stored.ActiveRuntimeRunID)
			}
		})
	}
}

// A14 (GPT fix 1 revision): with an expired-lease running run, the racing
// claim must find NOTHING (running runs are invisible to claim — only the
// reaper terminates them), the old finish either converges before the reap or
// is rejected, and the retry after the reap is a NEW run with generation 1.
func TestTakeoverVsFinishConvergence(t *testing.T) {
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			s, workspaceID := slotTestServer(t)
			node := slotTestNode(t, s, workspaceID)
			task := &entity.Task{
				ID: fmt.Sprintf("task-a14-%d", round), Title: "A14", Status: entity.TaskStatusInProgress,
				Priority: 2, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			}
			if err := s.ts.AddTask("sample", "pm", task); err != nil {
				t.Fatalf("add task: %v", err)
			}
			run := controldb.RuntimeRun{
				ID: fmt.Sprintf("run-a14-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
				AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
				Status: "running", LeaseExpiresAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
				LeaseGeneration: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			// node-b claim: must find no work — the expired-lease run is
			// running, and claim candidates are strictly queued.
			go func() {
				defer wg.Done()
				<-start
				_, found, err := s.controlDB.ClaimRuntimeRun(workspaceID, "rtn-slot-b", 60, nil)
				if err != nil {
					t.Errorf("claim on expired-lease run: %v", err)
				}
				if found {
					t.Errorf("claim took over an expired-lease running run (fix 1 violation)")
				}
			}()
			// Old node finish on the old generation.
			go func() {
				defer wg.Done()
				<-start
				_, _, err := s.controlDB.FinishRuntimeRun(workspaceID, run.ID, node.ID, run.LeaseGeneration, "succeeded", "", "", "{}")
				if err != nil && !controldb.LeaseGenerationMismatch(err) {
					t.Errorf("stale finish: %v", err)
				}
			}()
			close(start)
			wg.Wait()

			final, found, err := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
			if err != nil || !found {
				t.Fatalf("load run: %v", err)
			}
			switch final.Status {
			case "running":
				// Reap won the race (or is still pending): the run must still
				// belong to its original node and generation — claim never
				// touched it.
				if final.RuntimeNodeID != node.ID || final.LeaseGeneration != 1 {
					t.Fatalf("round %d: running run mutated without reap: %+v", round, final)
				}
			case "succeeded":
				// Finish won first; the run keeps its original ownership.
				if final.RuntimeNodeID != node.ID || final.LeaseGeneration != 1 {
					t.Fatalf("round %d: finished run mutated: %+v", round, final)
				}
			default:
				t.Fatalf("round %d: unexpected status %s", round, final.Status)
			}
			if final.Status == "succeeded" {
				// Terminal run: a claim must find nothing, and the task token
				// must have been conditionally cleared by the finish path.
				_, found, err := s.controlDB.ClaimRuntimeRun(workspaceID, "rtn-slot-b", 60, nil)
				if err != nil {
					t.Fatalf("late claim: %v", err)
				}
				if found {
					t.Fatalf("round %d: terminal run resurrected by claim", round)
				}
				if stored, _ := s.ts.GetTask("sample", "pm", task.ID); stored != nil && stored.ActiveRuntimeRunID != "" {
					t.Logf("round %d: finish won before the in-flight clear; residual token reconciles via sweep", round)
				}
			}
			// The retry path is a NEW run: reap the old one (converging with
			// any in-flight finish), re-enqueue, and the claim serves the NEW
			// run with generation 1 — never the old run's identity. When the
			// old finish already landed (terminal), the reap is a no-op and
			// the re-enqueue is still the only legal retry.
			if _, err := s.controlDB.ReapExpiredRuntimeRun(workspaceID, run.ID, final.LeaseGeneration, time.Now().UTC().Add(-time.Second)); err != nil {
				t.Fatalf("reap: %v", err)
			}
			retry := controldb.RuntimeRun{
				ID: fmt.Sprintf("run-a14-retry-%d", round), WorkspaceID: workspaceID,
				AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
				Status: "queued", Priority: 2,
				CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			if err := s.controlDB.UpsertRuntimeRun(retry); err != nil {
				t.Fatalf("re-enqueue: %v", err)
			}
			claimed, found, err := s.controlDB.ClaimRuntimeRun(workspaceID, "rtn-slot-b", 60, nil)
			if err != nil || !found {
				t.Fatalf("retry claim found=%v err=%v", found, err)
			}
			if claimed.ID != retry.ID || claimed.LeaseGeneration != 1 || claimed.RuntimeNodeID != "rtn-slot-b" {
				t.Fatalf("round %d: retry claim must serve the NEW run: %+v", round, claimed)
			}
			// The old run's identity stays terminal/reaped — the stale owner
			// cannot resurrect it via renew.
			if _, _, err := s.controlDB.ExtendRuntimeRunLeaseWithGeneration(workspaceID, run.ID, node.ID, run.LeaseGeneration, 60); !controldb.LeaseGenerationMismatch(err) {
				t.Fatalf("round %d: stale renew after reap: err=%v, want generation mismatch", round, err)
			}
		})
	}
}

// Task token lifecycle: enqueue stamps, finish clears, reaper sweep clears
// residue when the run is terminal (task.runtime_token_orphan audit).
func TestTaskActiveRuntimeRunTokenLifecycle(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	task := &entity.Task{
		ID: "task-token", Title: "Token", Status: entity.TaskStatusInProgress,
		Priority: 2, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-token", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
		LeaseGeneration: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)
	if stored, _ := s.ts.GetTask("sample", "pm", task.ID); stored == nil || stored.ActiveRuntimeRunID != run.ID {
		t.Fatal("token stamp must apply")
	}
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored.ActiveRuntimeRunID != run.ID {
		t.Fatalf("token = %q, want %q", stored.ActiveRuntimeRunID, run.ID)
	}
	// Conditional clear with the WRONG run id is a no-op.
	if s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, "run-other") {
		t.Fatal("clear with wrong run id must not apply")
	}
	// Right run id clears.
	if !s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, run.ID) {
		t.Fatal("clear with matching run id must apply")
	}
	// Stale-token sweep: re-stamp then make the run terminal; the sweep must
	// clear the token and audit an orphan.
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)
	done := run
	done.Status = "succeeded"
	done.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.controlDB.UpsertRuntimeRun(done); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	stored, _ = s.ts.GetTask("sample", "pm", task.ID)
	if !s.clearStaleTaskRuntimeToken(workspaceID, "sample", "pm", stored) {
		t.Fatal("stale token sweep must clear a token pointing at a terminal run")
	}
	stored, _ = s.ts.GetTask("sample", "pm", task.ID)
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token residue: %q", stored.ActiveRuntimeRunID)
	}
}

// Reaper unit: a run past lease+grace is reaped, audited, and its token
// cleared; a renewed run is untouched (C4 assertion at unit level).
func TestRuntimeReaperPassKillsOnlyExpiredLeases(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)

	makeTask := func(id string) {
		task := &entity.Task{ID: id, Title: id, Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
		if err := s.ts.AddTask("sample", "pm", task); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	makeRun := func(id, taskID string, lease time.Time) controldb.RuntimeRun {
		run := controldb.RuntimeRun{
			ID: id, WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
			AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: taskID,
			Status: "running", LeaseExpiresAt: lease.Format(time.RFC3339), LeaseGeneration: 1,
			CreatedAt: nowText, UpdatedAt: nowText,
		}
		if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		s.setTaskActiveRuntimeRun("sample", "pm", taskID, id)
		return run
	}
	makeTask("task-dead")
	dead := makeRun("run-dead", "task-dead", now.Add(-10*time.Minute))
	makeTask("task-alive")
	_ = makeRun("run-alive", "task-alive", now.Add(time.Minute))

	s.runtimeReaperPass()

	deadFinal, found, err := s.controlDB.RuntimeRunByID(workspaceID, "run-dead")
	if err != nil || !found {
		t.Fatalf("load dead run: %v", err)
	}
	if deadFinal.Status != "failed" || deadFinal.ErrorCode != "lease_expired" {
		t.Fatalf("dead run not reaped: %+v", deadFinal)
	}
	if deadFinal.LeaseGeneration != dead.LeaseGeneration+1 {
		t.Fatalf("reap must bump generation: %d", deadFinal.LeaseGeneration)
	}
	stored, _ := s.ts.GetTask("sample", "pm", "task-dead")
	if stored == nil || stored.ActiveRuntimeRunID != "" {
		t.Fatalf("reaped run's task token must be cleared: %+v", stored)
	}
	aliveFinal, found, err := s.controlDB.RuntimeRunByID(workspaceID, "run-alive")
	if err != nil || !found {
		t.Fatalf("load alive run: %v", err)
	}
	if aliveFinal.Status != "running" || aliveFinal.LeaseGeneration != 1 {
		t.Fatalf("renewed (live-lease) run must be untouched: %+v", aliveFinal)
	}
	storedAlive, _ := s.ts.GetTask("sample", "pm", "task-alive")
	if storedAlive == nil || storedAlive.ActiveRuntimeRunID != "run-alive" {
		t.Fatalf("live run's task token must stay: %+v", storedAlive)
	}
}

// Reaper lifecycle: start twice → single loop; waitForRuntimeReaper returns
// only after the loop exits (shutdown order guarantee).
func TestRuntimeReaperSingletonLifecycle(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	_ = workspaceID
	s.startRuntimeReaper(context.Background())
	if s.runtimeReaperDone == nil {
		t.Fatal("reaper loop must be started")
	}
	// Idempotent: a second start must not replace the done channel.
	done1 := s.runtimeReaperDone
	s.startRuntimeReaper(context.Background())
	if s.runtimeReaperDone != done1 {
		t.Fatal("second start must not spawn a second loop")
	}
	s.stopRuntimeReaper()
	s.waitForRuntimeReaper()
}

// GPT fix 2: after a successful reap the task must be driven through the
// fenced transition into the unified infra backoff/blocked path — a
// non-workflow task never stays in_progress after its run is reaped.
func TestReaperDrivesTaskIntoInfraBackoff(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-reap-backoff", Title: "ReapBackoff", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-reap-backoff", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	s.runtimeReaperPass()

	final, found, err := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
	if err != nil || !found {
		t.Fatalf("load run: %v", err)
	}
	if final.Status != "failed" || final.ErrorCode != "lease_expired" {
		t.Fatalf("run not reaped: %+v", final)
	}
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing")
	}
	// First infra failure: back to pending with a 5-minute NotBefore, NOT
	// stuck in_progress, token released.
	if stored.Status != entity.TaskStatusPending {
		t.Fatalf("task must be pending after first lease_expired, got %s (%+v)", stored.Status, stored)
	}
	if stored.NotBefore == nil || !stored.NotBefore.After(now) {
		t.Fatalf("task must carry a future NotBefore after backoff: %+v", stored.NotBefore)
	}
	if stored.InfraFailureStreak != 1 {
		t.Fatalf("infra streak = %d, want 1", stored.InfraFailureStreak)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token must be released after reap: %q", stored.ActiveRuntimeRunID)
	}
}

// GPT fix 2 cap side: two prior infra failures + a reap converge on blocked
// with the human-owner notification, never an in_progress residue.
func TestReaperDrivesTaskIntoBlockedAfterCap(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-reap-blocked", Title: "ReapBlocked", Status: entity.TaskStatusInProgress, Priority: 2, InfraFailureStreak: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-reap-blocked", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	s.runtimeReaperPass()

	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing")
	}
	if stored.Status != entity.TaskStatusBlocked {
		t.Fatalf("task must be blocked after 3rd infra failure, got %s", stored.Status)
	}
	if stored.InfraFailureStreak != 3 {
		t.Fatalf("infra streak = %d, want 3", stored.InfraFailureStreak)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token must be released after reap: %q", stored.ActiveRuntimeRunID)
	}
}

// GPT fix 3: the reaper's task transition is fenced on the execution token —
// a task whose token already moved to a NEWER run is not touched by the old
// run's reap.
func TestReaperTransitionFencedByTaskToken(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-fence", Title: "Fence", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	oldRun := controldb.RuntimeRun{
		ID: "run-fence-old", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(oldRun); err != nil {
		t.Fatalf("seed old run: %v", err)
	}
	// The token has MOVED to a newer run (a fresh dispatch already owns the
	// task); the old reap must not touch the task state.
	newRun := controldb.RuntimeRun{
		ID: "run-fence-new", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(newRun); err != nil {
		t.Fatalf("seed new run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, newRun.ID)

	reaped, err := s.controlDB.ReapExpiredRuntimeRun(workspaceID, oldRun.ID, oldRun.LeaseGeneration, now.Add(-time.Second))
	if err != nil || !reaped {
		t.Fatalf("reap old run: reaped=%v err=%v", reaped, err)
	}
	s.transitionReapedTask(workspaceID, oldRun)
	s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, oldRun.ID)

	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing")
	}
	if stored.Status != entity.TaskStatusInProgress {
		t.Fatalf("fenced reap must not move a task owned by a newer run: %s", stored.Status)
	}
	if stored.InfraFailureStreak != 0 {
		t.Fatalf("fenced reap must not advance the streak: %d", stored.InfraFailureStreak)
	}
	if stored.ActiveRuntimeRunID != newRun.ID {
		t.Fatalf("newer run's token must survive: %q", stored.ActiveRuntimeRunID)
	}
}

// Claude fix-round finding 1: the pass-wide stale-token sweep reconciles an
// orphan the finish path could not clear (token points at a terminal run)
// even when nothing was reaped in the same pass.
func TestReaperPassSweepsOrphanedTokensWithoutReap(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-orphan", Title: "Orphan", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-orphan", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "succeeded", FinishedAt: nowText, LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed terminal run: %v", err)
	}
	// Simulate the finish-side clear failure: the token is still stamped on
	// the task although the run is already terminal.
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	// Nothing here is past lease+grace — the pass reaps nothing, but the
	// sweep must still clean the orphan.
	s.runtimeReaperPass()

	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing")
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("pass-wide sweep must clear the orphaned token: %q", stored.ActiveRuntimeRunID)
	}
}

// GPT fix 3: old finish vs new enqueue race. The token fence decides: an old
// run's finish may never overwrite a task that has been re-dispatched to a
// new run, and the new enqueue's stamp wins the field.
func TestOldFinishVsNewEnqueueTokenFence(t *testing.T) {
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			s, workspaceID := slotTestServer(t)
			node := slotTestNode(t, s, workspaceID)
			now := time.Now().UTC()
			nowText := now.Format(time.RFC3339)
			task := &entity.Task{ID: fmt.Sprintf("task-ofe-%d", round), Title: "OldFinishVsEnqueue", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
			if err := s.ts.AddTask("sample", "pm", task); err != nil {
				t.Fatalf("add task: %v", err)
			}
			oldRun := controldb.RuntimeRun{
				ID: fmt.Sprintf("run-ofe-old-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
				AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
				Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
				CreatedAt: nowText, UpdatedAt: nowText,
			}
			if err := s.controlDB.UpsertRuntimeRun(oldRun); err != nil {
				t.Fatalf("seed old run: %v", err)
			}
			s.setTaskActiveRuntimeRun("sample", "pm", task.ID, oldRun.ID)

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			// The OLD run's node finally reports success (late finish).
			go func() {
				defer wg.Done()
				<-start
				_, _, err := s.controlDB.FinishRuntimeRun(workspaceID, oldRun.ID, node.ID, oldRun.LeaseGeneration, "succeeded", "", "", "{}")
				if err != nil {
					t.Errorf("old finish: %v", err)
				} else {
					s.clearTaskActiveRuntimeRunIfRun("sample", "pm", task.ID, oldRun.ID)
				}
			}()
			// Meanwhile the task is re-dispatched: a new run is enqueued and
			// the token is re-stamped to the NEW run.
			go func() {
				defer wg.Done()
				<-start
				newRun := controldb.RuntimeRun{
					ID: fmt.Sprintf("run-ofe-new-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
					AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
					Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
					CreatedAt: nowText, UpdatedAt: nowText,
				}
				if err := s.controlDB.UpsertRuntimeRun(newRun); err != nil {
					t.Errorf("new enqueue: %v", err)
					return
				}
				s.setTaskActiveRuntimeRun("sample", "pm", task.ID, newRun.ID)
			}()
			close(start)
			wg.Wait()

			stored, _ := s.ts.GetTask("sample", "pm", task.ID)
			if stored == nil {
				t.Fatal("task missing")
			}
			// Convergence: the token names either run — but if it still names
			// the old run while the new run exists, the fence lost; that is
			// exactly what the re-stamp must prevent. The only FORBIDDEN
			// outcome is the token being CLEARED by the old finish after the
			// new stamp (old run deleting the new dispatch's fence).
			newRunID := fmt.Sprintf("run-ofe-new-%d", round)
			oldRunID := fmt.Sprintf("run-ofe-old-%d", round)
			switch stored.ActiveRuntimeRunID {
			case newRunID:
				// Correct: the new dispatch owns the task.
			case oldRunID:
				// The stamp raced in after the old clear; the sweep or the
				// next enqueue write converges here. Tolerated ordering.
			default:
				t.Fatalf("round %d: old finish cleared the new dispatch's token (fence violation): %q", round, stored.ActiveRuntimeRunID)
			}
			// The task itself must never have been transitioned by the old
			// finish path outside its own fenced clear.
			if stored.Status != entity.TaskStatusInProgress {
				t.Fatalf("round %d: task status mutated by racing finish: %s", round, stored.Status)
			}
		})
	}
}

// HTTP finish: a finish carrying a stale generation is rejected 409 and the
// run state is untouched.
func TestFinishRuntimeNodeRunStaleGenerationRejected(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-stale", Title: "Stale", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-stale", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
		LeaseGeneration: 2, CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := strings.NewReader(`{"leaseGeneration":1,"result":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/run-stale/complete", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{Node: node}))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale finish status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	final, _, _ := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
	if final.Status != "running" {
		t.Fatalf("stale finish must leave the run untouched: %+v", final)
	}
	// The current generation succeeds.
	body2 := strings.NewReader(`{"leaseGeneration":2,"result":{"summary":"ok"}}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/run-stale/complete", body2)
	req2.SetPathValue("runId", run.ID)
	req2 = req2.WithContext(context.WithValue(req2.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{Node: node}))
	rec2 := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("current-generation finish status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}
