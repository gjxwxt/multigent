package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// finishBodyFor builds the minimal runtime-run finish request.
func finishBodyFor(t *testing.T, runID string, leaseGeneration int64, summary string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"leaseGeneration": leaseGeneration,
		"result":          map[string]any{"summary": summary},
	})
	if err != nil {
		t.Fatalf("marshal finish body: %v", err)
	}
	return string(body)
}

// seedWorkflowTaskOnNode seeds sample/pm on the runtime node with a two-step
// workflow (implement → recheck, both agent steps) and an active run on the
// first step. Returns the pieces the tests drive forward.
func seedWorkflowTaskOnNode(t *testing.T, s *Server, workspaceID, taskID string) (controldb.RuntimeRun, *workflowstore.Store) {
	t.Helper()
	node := slotTestNode(t, s, workspaceID)
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !ok {
		t.Fatalf("load aw-pm: %v %v", ok, err)
	}
	worker.DefaultRuntimeNodeID = node.ID
	if worker.DefaultModelAccountID == "" {
		worker.DefaultModelAccountID = "acct-test"
	}
	// The dispatch path queues a runtime run only when the worker's schedule
	// carries the task trigger (fireTaskTriggerOrQueueRuntime hb gate).
	schedule, err := json.Marshal(entity.HeartbeatConfig{Enabled: true, Triggers: []entity.TriggerType{entity.TriggerOnTask}})
	if err != nil {
		t.Fatalf("marshal schedule: %v", err)
	}
	worker.ScheduleJSON = string(schedule)
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("bind node: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{ID: taskID, Title: "Followup " + taskID, Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-followup-" + taskID, Name: "Followup Pipeline", Version: 1,
		Scope: "workspace", StartStepID: "implement",
		Steps: []entity.WorkflowStep{
			{ID: "implement", Type: "agent_task", Title: "Implement", ActorRole: "pm", OutputFields: []entity.WorkflowField{{Name: "code"}}},
			{ID: "recheck", Type: "agent_task", Title: "Recheck", ActorRole: "pm", OutputFields: []entity.WorkflowField{{Name: "verdict"}}},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "e1", From: "implement", To: "recheck", IsDefault: true},
			{ID: "e2", From: "recheck", To: "recheck", IsDefault: true},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	_, _, err = wfStore.StartRunWithInput("sample", taskID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm": {Type: "agent", ID: "pm"},
	}, nil)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	nowText := now.Format(time.RFC3339)
	run := controldb.RuntimeRun{
		ID: "run-followup-" + taskID, WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: taskID,
		Status: "running", LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339), LeaseGeneration: 1,
		CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := s.setTaskActiveRuntimeRun("sample", "pm", taskID, run.ID); err != nil {
		t.Fatalf("stamp token: %v", err)
	}
	return run, wfStore
}

// advanceStepMidRun simulates `mga task step done` during the run: the agent
// completes the CURRENT active step, the workflow advances to the next agent
// step, and the task is dispatched to that agent pending — exactly what
// handleRuntimeWorkflowStepComplete does (store transition + task move).
func advanceStepMidRun(t *testing.T, s *Server, workspaceID string, task *entity.Task, runID string) workflowstore.TransitionResult {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	transition, err := wfStore.CompleteAndAdvance("sample", task.ID, "mid-run summary", "mid-run output", map[string]string{"code": "done"}, "completed")
	if err != nil {
		t.Fatalf("CompleteAndAdvance: %v", err)
	}
	// activateNextWorkflowStep's agent branch: move the task pending onto the
	// next agent (same agent here) with the run token carried along.
	task.Status = entity.TaskStatusPending
	task.UpdatedAt = time.Now().UTC()
	task.ArchivedAt = nil
	if err := s.ts.PersistTask("sample", "pm", task); err != nil {
		t.Fatalf("persist moved task: %v", err)
	}
	if err := s.setTaskActiveRuntimeRun("sample", "pm", task.ID, runID); err != nil {
		t.Fatalf("re-stamp token: %v", err)
	}
	return transition
}

// A step completed DURING the run advances the workflow to the next agent step
// and lands the task pending there; the run's finish must NOT clobber that
// state with workflow_step_not_completed, and the followup dispatch must
// enqueue the next step's run (previously the pipeline stalled until a poller
// tick, which would run the task locally off the node).
func TestRunFinishAfterMidRunAdvanceKeepsTaskAndDispatchesFollowup(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run, _ := seedWorkflowTaskOnNode(t, s, workspaceID, "task-followup-1")

	task, err := s.ts.GetTask("sample", "pm", "task-followup-1")
	if err != nil || task == nil {
		t.Fatalf("load task: %v", err)
	}
	advanceStepMidRun(t, s, workspaceID, task, run.ID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/complete",
		strings.NewReader(finishBodyFor(t, run.ID, 1, "finished after mid-run advance")))
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), controldb.RuntimeNode{ID: run.RuntimeNodeID, WorkspaceID: workspaceID}))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The task must keep the state the mid-run dispatch placed (live, not
	// archived, no workflow_step_not_completed error). After the followup
	// dispatch the status is in_progress (queue semantics); without dispatch
	// it stays pending — both are healthy handoff states.
	stored, err := s.ts.GetTask("sample", "pm", "task-followup-1")
	if err != nil || stored == nil {
		t.Fatalf("reload task: %v", err)
	}
	if stored.Status != entity.TaskStatusPending && stored.Status != entity.TaskStatusInProgress {
		t.Fatalf("finish must not clobber the advanced task, got status %s (err=%s)", stored.Status, stored.LastError)
	}
	if stored.ArchivedAt != nil {
		t.Fatal("advanced task must not be archived by the run finish")
	}
	if strings.Contains(stored.LastError, "workflow step was not completed") {
		t.Fatalf("finish must not record step-not-completed on a healthy handoff: %q", stored.LastError)
	}

	// The followup dispatch must have enqueued a NEW run for the next step.
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: "task-followup-1"})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var queued int
	for _, r := range runs {
		if r.ID != run.ID && (r.Status == "queued" || r.Status == "running") {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("expected exactly one followup run enqueued after finish, got %d (runs=%+v)", queued, runs)
	}

	// The workflow must still be active on the next step.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	wfRun, ok, err := wfStore.RunForTask("sample", "task-followup-1")
	if err != nil || !ok || wfRun.Status != "active" || wfRun.ActiveStepID != "recheck" {
		t.Fatalf("workflow must stay active on recheck: ok=%v status=%s active=%s err=%v", ok, wfRun.Status, wfRun.ActiveStepID, err)
	}
}

// Without a mid-run advance, a workflow run finishing with its step NOT
// completed must still fail closed (the pre-existing contract).
func TestRunFinishWithoutStepCompletionStillFailsTask(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run, _ := seedWorkflowTaskOnNode(t, s, workspaceID, "task-stalled-1")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/complete",
		strings.NewReader(finishBodyFor(t, run.ID, 1, "gave up")))
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), controldb.RuntimeNode{ID: run.RuntimeNodeID, WorkspaceID: workspaceID}))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish status=%d body=%s", rec.Code, rec.Body.String())
	}

	stored, err := s.ts.GetTask("sample", "pm", "task-stalled-1")
	if err != nil || stored == nil {
		t.Fatalf("reload task: %v", err)
	}
	if stored.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("abandoned workflow step must fail the task, got %s", stored.Status)
	}
	if !strings.Contains(stored.LastError, "workflow step was not completed") {
		t.Fatalf("expected workflow_step_not_completed guidance, got %q", stored.LastError)
	}
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: "task-stalled-1"})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, r := range runs {
		if r.ID != run.ID && (r.Status == "queued" || r.Status == "running") {
			t.Fatalf("no followup run may be dispatched for an abandoned step: %+v", r)
		}
	}
}

// The followup probe itself: mid-run advance → probe true; untouched run →
// probe false (fail-closed).
func TestWorkflowAdvancedDuringRunProbe(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run, _ := seedWorkflowTaskOnNode(t, s, workspaceID, "task-probe-1")
	if s.workflowAdvancedDuringRun(&run) {
		t.Fatal("untouched run must not count as advanced")
	}
	task, err := s.ts.GetTask("sample", "pm", "task-probe-1")
	if err != nil || task == nil {
		t.Fatalf("load task: %v", err)
	}
	advanceStepMidRun(t, s, workspaceID, task, run.ID)
	if !s.workflowAdvancedDuringRun(&run) {
		t.Fatal("mid-run advance must be detected as advanced")
	}
}
