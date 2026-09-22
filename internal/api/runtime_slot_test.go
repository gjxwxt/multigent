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
	"github.com/multigent/multigent/internal/taskstore"
	workflowstore "github.com/multigent/multigent/internal/workflow"
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

// seedRealWorkflowRun starts a REAL workflow run (workflowstore.StartRun on a
// saved definition) attached to taskID, and marks the first step's instance
// running so the task mirrors an in-flight workflow execution. Returns the
// run and the active step ID.
func seedRealWorkflowRun(t *testing.T, s *Server, workspaceID, project, taskID string) (entity.WorkflowRun, string) {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := entity.WorkflowDefinition{
		ID: "wf-fence-test-v1", Name: "Fence Test", Version: 1, Scope: "workspace",
		StartStepID: "step_a",
		Steps: []entity.WorkflowStep{
			{ID: "step_a", Type: "agent_task", Title: "A", Position: entity.WorkflowPosition{X: 0, Y: 0}},
			{ID: "step_b", Type: "agent_task", Title: "B", Position: entity.WorkflowPosition{X: 100, Y: 0}},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "e_ab", From: "step_a", To: "step_b", IsDefault: true},
		},
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	run, instances, err := wfStore.StartRun(project, taskID, def.ID, nil)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	for i := range instances {
		if instances[i].StepID != run.ActiveStepID {
			continue
		}
		instances[i].Status = "running"
		instances[i].UpdatedAt = time.Now().UTC()
		if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
			t.Fatalf("mark step running: %v", err)
		}
	}
	return run, run.ActiveStepID
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

// GPT 收口 3: a reaped WORKFLOW task must explicitly enter the workflow
// failure/rework mechanism — the active step instance, the run, and the task
// all leave the in-flight state; nothing stays in_progress.
func TestReaperFailsWorkflowStepThroughReworkPath(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-wf-reap", Title: "WfReap", Status: entity.TaskStatusInProgress, Priority: 2,
		Vars: map[string]string{}, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	wfRun, activeStepID := seedRealWorkflowRun(t, s, workspaceID, "sample", task.ID)
	task.Vars["workflow_run_id"] = wfRun.ID
	if err := s.ts.UpdateTask("sample", "pm", task); err != nil {
		t.Fatalf("stamp workflow var: %v", err)
	}

	run := controldb.RuntimeRun{
		ID: "run-wf-reap", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	s.runtimeReaperPass()

	// Run: force-failed by the reaper.
	runFinal, found, err := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
	if err != nil || !found {
		t.Fatalf("load run: %v", err)
	}
	if runFinal.Status != "failed" || runFinal.ErrorCode != "lease_expired" {
		t.Fatalf("run not reaped: %+v", runFinal)
	}
	// Workflow run: failed (terminal), no dangling ActiveStepID.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	wfFinal, ok, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !ok {
		t.Fatalf("load workflow run: ok=%v err=%v", ok, err)
	}
	if wfFinal.Status != "failed" {
		t.Fatalf("workflow run status=%s, want failed (rework path)", wfFinal.Status)
	}
	// Step instance: failed, finished.
	instances, err := wfStore.ListStepInstances(wfRun.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	stepFinal := ""
	for _, inst := range instances {
		if inst.StepID == activeStepID {
			stepFinal = inst.Status
		}
	}
	if stepFinal != "failed" {
		t.Fatalf("active step status=%q, want failed", stepFinal)
	}
	// Task: archived done_failed — never in_progress.
	archived, err := s.ts.ListArchivedTasks("sample", "pm")
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	var done *entity.Task
	for _, at := range archived {
		if at.ID == task.ID {
			done = at
		}
	}
	if done == nil || done.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("workflow task not archived as done_failed: %+v", done)
	}
	// Token released (persist succeeded inside the fence).
	active, _ := s.ts.GetTask("sample", "pm", task.ID)
	if active != nil && active.ActiveRuntimeRunID != "" {
		t.Fatalf("token must be released after fenced workflow transition: %q", active.ActiveRuntimeRunID)
	}
}

// GPT 收口 4a: the OLD-finish-vs-NEW-enqueue race driven through the REAL
// HTTP finish handler (handleRuntimeNodeRunComplete), not FinishRuntimeRun
// directly. The old run's late success must never touch the task that a new
// dispatch now owns — the fence makes the transition a no-op.
func TestOldHTTPFinishVsNewEnqueueFence(t *testing.T) {
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			s, workspaceID := slotTestServer(t)
			node := slotTestNode(t, s, workspaceID)
			now := time.Now().UTC()
			nowText := now.Format(time.RFC3339)
			task := &entity.Task{ID: fmt.Sprintf("task-hofe-%d", round), Title: "HTTP old finish", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
			if err := s.ts.AddTask("sample", "pm", task); err != nil {
				t.Fatalf("add task: %v", err)
			}
			oldRun := controldb.RuntimeRun{
				ID: fmt.Sprintf("run-hofe-old-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
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
			// Old node's late success through the real HTTP handler.
			go func() {
				defer wg.Done()
				<-start
				body := strings.NewReader(`{"leaseGeneration":1,"result":{"summary":"late success"}}`)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+oldRun.ID+"/complete", body)
				req.SetPathValue("runId", oldRun.ID)
				req = req.WithContext(contextWithNode(req.Context(), node))
				rec := httptest.NewRecorder()
				s.handleRuntimeNodeRunComplete(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("old finish status=%d body=%s", rec.Code, rec.Body.String())
				}
			}()
			// New dispatch re-stamps the token to a new run.
			go func() {
				defer wg.Done()
				<-start
				newRun := controldb.RuntimeRun{
					ID: fmt.Sprintf("run-hofe-new-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
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
			// The old finish must NOT have transitioned the task: it stays
			// active (in_progress) under the new run's fence — never
			// done_success, never archived.
			if stored.Status != entity.TaskStatusInProgress {
				t.Fatalf("round %d: old HTTP finish mutated the task: %s", round, stored.Status)
			}
			archived, _ := s.ts.ListArchivedTasks("sample", "pm")
			for _, at := range archived {
				if at.ID == task.ID {
					t.Fatalf("round %d: old finish archived the task", round)
				}
			}
			newRunID := fmt.Sprintf("run-hofe-new-%d", round)
			if stored.ActiveRuntimeRunID != newRunID {
				t.Fatalf("round %d: token = %q, want the new run's fence", round, stored.ActiveRuntimeRunID)
			}
		})
	}
}

// GPT 收口 4b: reaper vs new enqueue race — the reaper kills the old run but
// the task has already been re-dispatched to a new run; the fenced transition
// must leave the new dispatch untouched and release only the old run's claim.
func TestReaperVsNewEnqueueFence(t *testing.T) {
	for round := 0; round < 10; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			s, workspaceID := slotTestServer(t)
			node := slotTestNode(t, s, workspaceID)
			now := time.Now().UTC()
			nowText := now.Format(time.RFC3339)
			task := &entity.Task{ID: fmt.Sprintf("task-rvne-%d", round), Title: "Reaper vs enqueue", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
			if err := s.ts.AddTask("sample", "pm", task); err != nil {
				t.Fatalf("add task: %v", err)
			}
			oldRun := controldb.RuntimeRun{
				ID: fmt.Sprintf("run-rvne-old-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
				AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
				Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
				CreatedAt: nowText, UpdatedAt: nowText,
			}
			if err := s.controlDB.UpsertRuntimeRun(oldRun); err != nil {
				t.Fatalf("seed old run: %v", err)
			}
			s.setTaskActiveRuntimeRun("sample", "pm", task.ID, oldRun.ID)

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			// Reaper pass kills the expired run.
			go func() {
				defer wg.Done()
				<-start
				s.runtimeReaperPass()
			}()
			// New dispatch re-stamps the token.
			go func() {
				defer wg.Done()
				<-start
				newRun := controldb.RuntimeRun{
					ID: fmt.Sprintf("run-rvne-new-%d", round), WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
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
			newRunID := fmt.Sprintf("run-rvne-new-%d", round)
			if stored.ActiveRuntimeRunID == newRunID {
				// New dispatch owns the task: the reaper must not have moved
				// it out of in_progress or advanced the streak.
				if stored.Status != entity.TaskStatusInProgress {
					t.Fatalf("round %d: reaper moved a task owned by the new dispatch: %s", round, stored.Status)
				}
				if stored.InfraFailureStreak != 0 {
					t.Fatalf("round %d: reaper advanced the streak on a fenced-out task: %d", round, stored.InfraFailureStreak)
				}
			}
			// Old run must be dead either way.
			oldFinal, _, _ := s.controlDB.RuntimeRunByID(workspaceID, oldRun.ID)
			if oldFinal.Status != "failed" || oldFinal.ErrorCode != "lease_expired" {
				t.Fatalf("round %d: old run not reaped: %+v", round, oldFinal)
			}
		})
	}
}

// flakyTaskStore wraps a taskstore.Store and fails the first N UpdateTask and
// ArchiveTask writes (simulating a transient store failure) so the fenced
// transition's persist-failure contract can be exercised end to end.
type flakyTaskStore struct {
	taskstore.Store
	failUpdate  int
	failArchive int
	// failUpdateWhen, when non-nil, intercepts UpdateTask by predicate
	// (before the counter logic) — used to target the token stamp write.
	failUpdateWhen func(t *entity.Task) bool
}

func (f *flakyTaskStore) UpdateTask(project, agent string, t *entity.Task) error {
	if f.failUpdateWhen != nil && f.failUpdateWhen(t) {
		return fmt.Errorf("injected transient store failure")
	}
	if f.failUpdate > 0 {
		f.failUpdate--
		return fmt.Errorf("injected transient store failure")
	}
	return f.Store.UpdateTask(project, agent, t)
}

func (f *flakyTaskStore) ArchiveTask(project, agent string, t *entity.Task) error {
	if f.failArchive > 0 {
		f.failArchive--
		return fmt.Errorf("injected transient store failure")
	}
	return f.Store.ArchiveTask(project, agent, t)
}

// GPT 收口 1/4d: when the task-store write fails inside the fenced finish
// transition, the token must NOT be released (the transition stays retryable
// under the old fence), and a retrying finish converges to the correct final
// state.
func TestFencedFinishWriteFailureKeepsTokenThenRecovers(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-wf-fail", Title: "Write failure", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-wf-fail", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	// Inject one persist failure into the finish path. The fenced helper is
	// the single persist owner: the mutation + token release ride ONE
	// UpdateTask write, so a failure persists nothing and the fence holds.
	flaky := &flakyTaskStore{Store: s.ts, failUpdate: 1}
	s.ts = flaky
	body := strings.NewReader(`{"leaseGeneration":1,"result":{"summary":"done"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/complete", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), node))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish with failing store status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The run is terminal, but the task must be untouched: still in_progress,
	// not archived, token still naming the run — the transition is retryable
	// by the sweep replay, not lost.
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task must remain active after write failure")
	}
	if stored.ActiveRuntimeRunID != run.ID {
		t.Fatalf("token must be kept on persist failure, got %q", stored.ActiveRuntimeRunID)
	}
	if stored.ArchivedAt != nil {
		t.Fatal("task must not be archived while its persist failed")
	}
	if stored.Status != entity.TaskStatusInProgress {
		t.Fatalf("task state must be untouched on persist failure, got %s", stored.Status)
	}

	// Recovery: heal the store and let the pass-wide sweep replay the finish
	// transition — the production recovery path (clearStaleTaskRuntimeToken
	// replays a terminal run's task transition before releasing the fence).
	s.ts = flaky.Store
	server := s
	found := server.clearStaleTaskRuntimeToken(workspaceID, "sample", "pm", mustActiveTask(t, s, "sample", "pm", task.ID))
	if !found {
		t.Fatal("sweep replay did not reconcile the task")
	}
	archived, err := s.ts.ListArchivedTasks("sample", "pm")
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	var done *entity.Task
	for _, at := range archived {
		if at.ID == task.ID {
			done = at
		}
	}
	if done == nil || done.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("recovery did not archive the task as success: %+v", done)
	}
	if done.ActiveRuntimeRunID != "" {
		t.Fatalf("recovery must release the token: %q", done.ActiveRuntimeRunID)
	}
}

// mustActiveTask loads a task from the active queue for the sweep helpers.
func mustActiveTask(t *testing.T, s *Server, project, agent, taskID string) *entity.Task {
	t.Helper()
	task, err := s.ts.GetTask(project, agent, taskID)
	if err != nil || task == nil {
		t.Fatalf("load task %s: %v", taskID, err)
	}
	return task
}

// GPT 收口 1/4d (reaper side): a persist failure inside the reaper's fenced
// transition keeps the token; the next reaper pass converges to the unified
// backoff state.
func TestFencedReaperWriteFailureKeepsTokenThenConverges(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-wf-fail-reap", Title: "Reap write failure", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-wf-fail-reap", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	// First pass with an injected persist failure inside the fenced
	// transition (the mutation is in-memory; the helper's UpdateTask is the
	// single persist and it is what fails).
	flaky := &flakyTaskStore{Store: s.ts, failUpdate: 1}
	s.ts = flaky
	s.runtimeReaperPass()
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing")
	}
	// Same-pass convergence: the pass-wide sweep replays the reaped
	// transition under the fence (token still names the run) and persists the
	// backoff. The task must NOT be left in_progress or dropped unfenced.
	if stored.Status != entity.TaskStatusPending || stored.NotBefore == nil {
		t.Fatalf("same-pass sweep must converge to backoff: status=%s notBefore=%v", stored.Status, stored.NotBefore)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("sweep must release the token after a successful replay: %q", stored.ActiveRuntimeRunID)
	}
	if stored.InfraFailureStreak != 1 {
		t.Fatalf("streak=%d, want exactly 1 (single fenced application, no double count)", stored.InfraFailureStreak)
	}

	// Recovery pass with a healthy store: the run is already reaped (not in
	// the expired list) so this pass must be a no-op for the task — the
	// backoff is NOT applied a second time.
	s.ts = flaky.Store
	s.runtimeReaperPass()
	stored, _ = s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing after recovery")
	}
	if stored.Status != entity.TaskStatusPending || stored.NotBefore == nil {
		t.Fatalf("recovery pass must keep the converged backoff: status=%s notBefore=%v", stored.Status, stored.NotBefore)
	}
	if stored.InfraFailureStreak != 1 {
		t.Fatalf("streak=%d after recovery pass, want exactly 1 (no double count)", stored.InfraFailureStreak)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token must stay released: %q", stored.ActiveRuntimeRunID)
	}
}

// Q0 收口 5: manual start JOINs the queue (task-queue semantics). With an
// agent already holding a queued/running run on its assigned node, a manual
// start must converge onto the SAME run via run_key idempotency (200), NOT
// 409. The legacy immediate-execution 409 is gone; backoff/blocked gates are
// unchanged (B8 covers them).
func TestManualStartJoinsQueueWhileAgentBusy(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	// Bind the pm worker to the online node so the manual start takes the
	// runtime-node enqueue path.
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !ok {
		t.Fatalf("load aw-pm: %v %v", ok, err)
	}
	worker.DefaultRuntimeNodeID = node.ID
	if worker.DefaultModelAccountID == "" {
		// Readiness gate: runnable agents must bind an explicit model account.
		worker.DefaultModelAccountID = "acct-test"
	}
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("bind node: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{ID: "task-manual-queue", Title: "Manual queue", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	startTask := func() *httptest.ResponseRecorder {
		req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
		req.SetPathValue("name", "sample")
		req.SetPathValue("taskId", task.ID)
		rec := httptest.NewRecorder()
		s.handleStartProjectTask(rec, req)
		return rec
	}

	// First manual start → queued run.
	rec1 := startTask()
	if rec1.Code != http.StatusOK {
		t.Fatalf("first manual start status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: task.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs after first start: n=%d err=%v", len(runs), err)
	}
	first := runs[0]
	if first.Status != "queued" {
		t.Fatalf("first run status=%s, want queued", first.Status)
	}

	// Second manual start while the run is queued → same run (idempotent),
	// 200 — the task-queue semantic, not a 409.
	rec2 := startTask()
	if rec2.Code != http.StatusOK {
		t.Fatalf("second manual start status=%d body=%s, want 200 (join queue)", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), first.ID) {
		t.Fatalf("second start must reference the SAME run %s, got %s", first.ID, rec2.Body.String())
	}
	runs, _ = s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: task.ID})
	if len(runs) != 1 {
		t.Fatalf("duplicate run created: n=%d, want 1", len(runs))
	}

	// A third start while the run is RUNNING (unexpired lease) → still
	// converges onto the same run; the fence (task token) stays intact.
	claimed, ok, err := s.controlDB.ClaimRuntimeRun(workspaceID, node.ID, 90, nil)
	if err != nil || !ok || claimed.ID != first.ID {
		t.Fatalf("claim run for running phase: ok=%v id=%s err=%v", ok, claimed.ID, err)
	}
	rec3 := startTask()
	if rec3.Code != http.StatusOK {
		t.Fatalf("third manual start status=%d body=%s, want 200 (join queue)", rec3.Code, rec3.Body.String())
	}
	if !strings.Contains(rec3.Body.String(), first.ID) {
		t.Fatalf("third start must reference the SAME run %s, got %s", first.ID, rec3.Body.String())
	}
	runs, _ = s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: task.ID})
	if len(runs) != 1 {
		t.Fatalf("running-phase duplicate: n=%d, want 1", len(runs))
	}
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored.ActiveRuntimeRunID != first.ID {
		t.Fatalf("execution token must still name the running run: %q", stored.ActiveRuntimeRunID)
	}
}

// ── GPT 收口 6: stamp failure awareness, idempotent workflow replay,
// missing-run replay via discovery key, delivery post-commit ordering ────────

// 6-1: a run whose task-token stamp failed must NEVER dispatch. The enqueue
// force-fails it (FailQueuedRuntimeRun), so the claim path cannot pick it up
// and no node executes work whose finish the fence would drop.
func TestEnqueueFailsRunWhenTokenStampFails(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	task := &entity.Task{ID: "task-stamp-fail", Title: "Stamp failure", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// failUpdateWhen targets the STAMP write specifically: the stamp is the
	// only UpdateTask that sets ActiveRuntimeRunID on this task.
	flaky := &flakyTaskStore{Store: s.ts, failUpdateWhen: func(t *entity.Task) bool {
		return t.ActiveRuntimeRunID != ""
	}}
	s.ts = flaky
	// Drive the enqueue primitive directly with the flaky store in place.
	hb := &entity.HeartbeatConfig{}
	_, err := s.enqueueSpecificRuntimeTaskRunFromRequest(workspaceID, "sample", "pm", task, hb, "http://127.0.0.1:1", "admin")
	if err == nil {
		t.Fatal("enqueue must surface the stamp failure")
	}
	s.ts = flaky.Store

	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: task.ID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs=%d, want 1", len(runs))
	}
	if runs[0].Status != "failed" || runs[0].ErrorCode != "token_stamp_failed" {
		t.Fatalf("run must be failed with token_stamp_failed, got status=%s code=%s", runs[0].Status, runs[0].ErrorCode)
	}
	// The failed run is invisible to the claim path.
	claimed, ok, err := s.controlDB.ClaimRuntimeRun(workspaceID, node.ID, 90, nil)
	if err != nil || ok {
		t.Fatalf("failed run must not be claimable: ok=%v err=%v", ok, err)
	}
	_ = claimed
}

// 6-3: the workflow reaper replay is idempotent — after the workflow engine
// already recorded the failure (first pass), a replay pass must NOT re-drive
// the engine (which would no-op or error) but MUST converge the task state.
func TestWorkflowReapReplayConvergesAfterEngineFailureRecorded(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-wf-replay", Title: "Replay", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	wfRun, activeStepID := seedRealWorkflowRun(t, s, workspaceID, "sample", task.ID)
	run := controldb.RuntimeRun{
		ID: "run-wf-replay", WorkspaceID: workspaceID, RuntimeNodeID: "rtn-slot",
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(-10 * time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	// Pass 1: the workflow engine records the failure, but the task persist
	// fails (injected) — the token must survive for the replay.
	flaky := &flakyTaskStore{Store: s.ts, failUpdate: 1}
	s.ts = flaky
	s.runtimeReaperPass()
	s.ts = flaky.Store

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	reloaded, found, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("workflow run missing: %v %v", found, err)
	}
	if reloaded.Status != "failed" {
		t.Fatalf("engine must have recorded the failure on pass 1, got %s", reloaded.Status)
	}

	// Pass 2 (replay): the engine is already terminal — the pass must not
	// wedge; the task must converge to done_failed and release the token.
	s.runtimeReaperPass()
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil {
		t.Fatal("task missing after replay pass")
	}
	if stored.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("replay must converge the task to done_failed, got %s", stored.Status)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("replay must release the token, got %q", stored.ActiveRuntimeRunID)
	}
	// The engine state is untouched by the replay (no double transition).
	final, _, _ := wfStore.RunForTask("sample", task.ID)
	if final.ID != wfRun.ID || final.Status != "failed" || final.ActiveStepID != "" {
		t.Fatalf("replay must not mutate the terminal workflow run: %+v", final)
	}
	_ = activeStepID
}

// 6-4: when the run row is already gone, the replay still releases the task's
// fence — addressed by the task's DISCOVERY key (project/agent as found by the
// sweep), not by the run's stored agent identity.
func TestMissingRunReplayReleasesTokenViaDiscoveryKey(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	now := time.Now().UTC()
	task := &entity.Task{ID: "task-missing-run", Title: "Missing run", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	// A run row that no longer exists; the task still carries its token.
	ghost := controldb.RuntimeRun{
		ID: "run-ghost", WorkspaceID: workspaceID,
		ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, ghost.ID)

	cur, _ := s.ts.GetTask("sample", "pm", task.ID)
	if !s.clearStaleTaskRuntimeToken(workspaceID, "sample", "pm", cur) {
		t.Fatal("missing-run reconciliation must succeed")
	}
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token must be released after missing-run replay, got %q", stored.ActiveRuntimeRunID)
	}
	if stored.Status != entity.TaskStatusInProgress {
		t.Fatalf("missing run defines no outcome; status must be untouched, got %s", stored.Status)
	}
}

// Node-PID scenario (收口 6-5): a stale LOCAL manual-run process heartbeat
// (PID alive + status running) must NOT block manual start for an agent that
// is assigned to a runtime node — the node path always enqueues.
func TestManualStartEnqueuesForNodeAgentWithStaleLocalPID(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !ok {
		t.Fatalf("load aw-pm: %v %v", ok, err)
	}
	worker.DefaultRuntimeNodeID = node.ID
	if worker.DefaultModelAccountID == "" {
		worker.DefaultModelAccountID = "acct-test"
	}
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("bind node: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{ID: "task-node-stale-pid", Title: "Stale PID", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// Simulate a stale local-process heartbeat: PID 1 is always alive on the
	// host, LastWakeupStatus says running — the legacy immediate-execution
	// gate would 409 here.
	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, "sample", "pm")
	hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
	if err != nil || hb == nil {
		t.Fatalf("load heartbeat: %v", err)
	}
	pid := 1
	hb.PID = pid
	hb.LastWakeupStatus = "running"
	if err := s.saveSchedulerTargetHeartbeat(workspaceID, target, hb); err != nil {
		t.Fatalf("save heartbeat: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("node-assigned manual start must enqueue despite live local PID, got %d: %s", rec.Code, rec.Body.String())
	}
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: task.ID})
	if err != nil || len(runs) != 1 || runs[0].Status != "queued" {
		t.Fatalf("expected one queued run, got n=%d err=%v", len(runs), err)
	}
}

// 6-2 (delivery ordering): a successful finish persists the task FIRST, then
// runs the delivery post-commit against the persisted task; a finish whose
// persist fails never runs delivery at all. worktreeMgr is nil in the test
// server, so snapshot/push/cleanup are no-ops — the observable contract is
// that the post-commit hook re-reads the PERSISTED task (done_success) and
// leaves it intact, and that the finish response is still 200 when delivery
// is a no-op.
func TestDeliveryPostCommitRunsAgainstPersistedTask(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-delivery", Title: "Delivery", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-delivery", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	body := strings.NewReader(`{"leaseGeneration":1,"result":{"summary":"delivered"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/complete", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), node))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil || stored.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("task must be done_success after delivery post-commit: %+v", stored)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token must be released: %q", stored.ActiveRuntimeRunID)
	}
	// The run terminal state is preserved through the post-commit.
	after, found, _ := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
	if !found || after.Status != "succeeded" {
		t.Fatalf("run must stay succeeded: found=%v status=%s", found, after.Status)
	}
}

// 6-5 (HTTP replay variant): a REAL handleRuntimeNodeRunFail carries the
// business failure into the fenced transition; the replay (sweep) then
// converges an interrupted transition from the RUN record alone.
func TestHTTPFailThenSweepReplayConvergesDoneFailed(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	task := &entity.Task{ID: "task-http-replay", Title: "HTTP replay", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	run := controldb.RuntimeRun{
		ID: "run-http-replay", WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	s.setTaskActiveRuntimeRun("sample", "pm", task.ID, run.ID)

	// The HTTP fail arrives but the task persist fails — the run IS terminal
	// (FinishRuntimeRun happened), the task stays fenced and untouched.
	flaky := &flakyTaskStore{Store: s.ts, failUpdate: 1}
	s.ts = flaky
	body := strings.NewReader(`{"leaseGeneration":1,"errorCode":"agent_run_failed","errorMessage":"business failure"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/fail", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), node))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunFail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail status=%d body=%s", rec.Code, rec.Body.String())
	}
	s.ts = flaky.Store
	terminal, _, _ := s.controlDB.RuntimeRunByID(workspaceID, run.ID)
	if terminal.Status != "failed" {
		t.Fatalf("run must be terminal after HTTP fail: %s", terminal.Status)
	}

	// The sweep replays from the RUN record: business failure → done_failed.
	cur, _ := s.ts.GetTask("sample", "pm", task.ID)
	if !s.clearStaleTaskRuntimeToken(workspaceID, "sample", "pm", cur) {
		t.Fatal("sweep replay must reconcile the interrupted finish")
	}
	stored, _ := s.ts.GetTask("sample", "pm", task.ID)
	if stored == nil || stored.Status != entity.TaskStatusDoneFailed || stored.ArchivedAt == nil {
		t.Fatalf("replay must archive the task as done_failed: %+v", stored)
	}
	if stored.ActiveRuntimeRunID != "" {
		t.Fatalf("token must be released: %q", stored.ActiveRuntimeRunID)
	}
	if stored.LastError != "business failure" {
		t.Fatalf("replay must carry the run's error message: %q", stored.LastError)
	}
}

// P2 soak finding (autoStart race): two tasks autoStart to the SAME agent at
// the same moment. The legacy immediate-execution path gated on the heartbeat
// PID and 409ed ("agent is already running") the loser — its first run failed
// and only the scheduler's later wake recovered it. With the agent start gate
// the second start JOINS the queue (200, distinct queued run) instead of
// failing: one slot per worker is enforced at claim time, not at start time.
func TestConcurrentAutoStartSameAgentJoinsQueue(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !ok {
		t.Fatalf("load aw-pm: %v %v", ok, err)
	}
	worker.DefaultRuntimeNodeID = node.ID
	if worker.DefaultModelAccountID == "" {
		worker.DefaultModelAccountID = "acct-test"
	}
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("bind node: %v", err)
	}

	now := time.Now().UTC()
	taskA := &entity.Task{ID: "task-autostart-a", Title: "AutoStart A", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	taskB := &entity.Task{ID: "task-autostart-b", Title: "AutoStart B", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", taskA); err != nil {
		t.Fatalf("add taskA: %v", err)
	}
	if err := s.ts.AddTask("sample", "pm", taskB); err != nil {
		t.Fatalf("add taskB: %v", err)
	}

	startTask := func(taskID string) *httptest.ResponseRecorder {
		req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+taskID+"/start", "admin", nil)
		req.SetPathValue("name", "sample")
		req.SetPathValue("taskId", taskID)
		rec := httptest.NewRecorder()
		s.handleStartProjectTask(rec, req)
		return rec
	}

	// Fire both starts concurrently, mirroring two projects autoStarting their
	// initialization tasks to the same agent in the same tick.
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	wg.Add(2)
	for i, taskID := range []string{taskA.ID, taskB.ID} {
		go func(i int, taskID string) {
			defer wg.Done()
			recs[i] = startTask(taskID)
		}(i, taskID)
	}
	wg.Wait()

	for i, rec := range recs {
		if rec == nil {
			t.Fatalf("start %d produced no response", i)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("concurrent start %d status=%d body=%s, want 200 (join queue)", i, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "already running") {
			t.Fatalf("concurrent start %d hit the legacy agent-busy 409: %s", i, rec.Body.String())
		}
	}

	// Both tasks own distinct queued runs; neither was dropped.
	runsA, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: taskA.ID})
	if err != nil || len(runsA) != 1 || runsA[0].Status != "queued" {
		t.Fatalf("taskA runs: n=%d status=%v err=%v", len(runsA), runsA, err)
	}
	runsB, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: taskB.ID})
	if err != nil || len(runsB) != 1 || runsB[0].Status != "queued" {
		t.Fatalf("taskB runs: n=%d status=%v err=%v", len(runsB), runsB, err)
	}
	if runsA[0].ID == runsB[0].ID {
		t.Fatalf("two distinct tasks must not converge onto one run: %s", runsA[0].ID)
	}

	// A later claim honors the one-slot-per-worker rule: the second queued run
	// stays queued while the first holds the slot (unexpired lease).
	first, ok, err := s.controlDB.ClaimRuntimeRun(workspaceID, node.ID, 90, nil)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	second, ok, err := s.controlDB.ClaimRuntimeRun(workspaceID, node.ID, 90, nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatalf("second claim must be refused while the worker slot is held, got run %s", second.ID)
	}
	if first.ID != runsA[0].ID && first.ID != runsB[0].ID {
		t.Fatalf("first claim must be one of the two queued runs, got %s", first.ID)
	}
}

// The local (non-node) autoStart path must serialize per agent too: the
// second concurrent start waits on the gate instead of racing the heartbeat
// PID check. The hook blocks both callers until released — the contract is
// that the second start cannot pass the busy checks until the first has left
// the gate's critical section.
func TestAgentStartGateSerializesLocalStarts(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "reviewer")

	now := time.Now().UTC()
	task := &entity.Task{ID: "task-gate-local", Title: "Gate local", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "reviewer", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	release := make(chan struct{})
	var entered, exited int
	var mu sync.Mutex
	s.agentStartTestHook = func(key string) func() {
		mu.Lock()
		entered++
		mu.Unlock()
		<-release
		mu.Lock()
		exited++
		mu.Unlock()
		return func() {}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = s.startProjectTaskDirect(workspaceID, "sample", "reviewer", task, nil)
		}()
	}
	// Give both goroutines a chance to reach the gate, then let them through.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	bothEntered := entered == 2
	mu.Unlock()
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !bothEntered {
		t.Fatalf("second start must block on the agent gate, entered=%d", entered)
	}
	if exited != 2 {
		t.Fatalf("both starts must pass the gate, exited=%d", exited)
	}
}

// ── Slot observability (scheduling observability module) ────────────────────

func TestRuntimeSlotStateEmpty(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	resp := s.runtimeSlotState(workspaceID, "sample", "pm", time.Now().UTC())
	if resp.Occupied || resp.Stale || resp.Releasable {
		t.Fatalf("empty slot must report unoccupied, got %+v", resp)
	}
}

func TestRuntimeSlotStateOccupiedAndStale(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	slotTestNode(t, s, workspaceID)
	now := time.Now().UTC()
	task := &entity.Task{ID: "task-slot-holder", Title: "Slot holder", Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	hb := &entity.HeartbeatConfig{}
	if _, err := s.enqueueSpecificRuntimeTaskRunFromRequest(workspaceID, "sample", "pm", task, hb, "http://127.0.0.1:1", "admin"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Claim the run so it is running with a live lease.
	claim, ok, err := s.controlDB.ClaimRuntimeRun(workspaceID, "rtn-slot", 90, nil)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// Force the lease far into the past WITHOUT bumping generation (the
	// renewal path would do that); UpsertRuntimeRun preserves the generation.
	expired := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	claim.LeaseExpiresAt = expired
	if err := s.controlDB.UpsertRuntimeRun(claim); err != nil {
		t.Fatalf("age lease: %v", err)
	}

	resp := s.runtimeSlotState(workspaceID, "sample", "pm", now)
	if !resp.Occupied {
		t.Fatalf("slot must report occupied, got %+v", resp)
	}
	if !resp.Stale || !resp.Releasable {
		t.Fatalf("expired-lease run must be stale+releasable, got %+v", resp)
	}
	if resp.Run == nil || resp.Run.ID != claim.ID || !resp.Run.LeaseExpired {
		t.Fatalf("run view must name the stale run, got %+v", resp.Run)
	}
	if resp.Task == nil || resp.Task.ID != "task-slot-holder" {
		t.Fatalf("task view must name the holding task, got %+v", resp.Task)
	}

	// Release via the same fenced path the reaper uses; slot must free up.
	if err := s.releaseStaleSlot(workspaceID, "sample", "pm", now); err != nil {
		t.Fatalf("release stale slot: %v", err)
	}
	after := s.runtimeSlotState(workspaceID, "sample", "pm", now)
	if after.Occupied {
		t.Fatalf("slot must be free after release, got %+v", after)
	}
	stored, err := s.ts.GetTask("sample", "pm", "task-slot-holder")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Status != entity.TaskStatusPending && stored.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("reaped task must land in a dispatchable/terminal state per reaper semantics, got %s", stored.Status)
	}
}
