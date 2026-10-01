package api

// B3 regression (dockerd-wedged incident): a runtime node run whose EXECUTION
// infrastructure failed — the runner's Docker probe inside
// ExecPromptWithRuntimeControlEnvContext / RunTaskWithContext returned
// "docker sandbox: Docker daemon did not respond within 3s", the node reported
// executor_failed — used to archive a WORKFLOW task as a terminal done_failed,
// even though the step never ran and no agent judgment produced a failure.
// The workflow branch of applyFencedFinishTransition now routes server-
// controlled infra codes through the SAME backoff channel as plain tasks
// (failures 1-2 → pending + 5m NotBefore, 3 → blocked + human notify), while
// business failures (agent_run_failed) keep the done_failed rework contract
// (B11) and a completed-but-abandoned step still fails closed.
//
// The finish path is exercised through the real HTTP endpoint
// (handleRuntimeNodeRunFail) exactly like the production node does, so the
// test covers FinishRuntimeRun + the fenced transition, not a helper.

import (
	"context"
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

// infraWorkflowTaskOnNode seeds a WORKFLOW task bound to a runtime node with a
// live run: the same shape TestRunFinishWithoutStepCompletionStillFailsTask
// uses, so the fenced finish's workflow branch (runtimeTaskHasWorkflow) is
// exercised for real.
func infraWorkflowTaskOnNode(t *testing.T, s *Server, workspaceID, taskID string) controldb.RuntimeRun {
	t.Helper()
	node := slotTestNode(t, s, workspaceID)
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !ok {
		t.Fatalf("load aw-pm: %v %v", ok, err)
	}
	worker.DefaultModelAccountID = "acct-test"
	schedule, err := json.Marshal(entity.HeartbeatConfig{Enabled: true, Triggers: []entity.TriggerType{entity.TriggerOnTask}})
	if err != nil {
		t.Fatalf("marshal schedule: %v", err)
	}
	worker.ScheduleJSON = string(schedule)
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("bind node: %v", err)
	}
	now := time.Now().UTC()
	task := &entity.Task{ID: taskID, Title: "B3 " + taskID, Status: entity.TaskStatusInProgress, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-b3-" + taskID, Name: "B3 Pipeline", Version: 1,
		Scope: "workspace", StartStepID: "implement",
		Steps: []entity.WorkflowStep{
			{ID: "implement", Type: "agent_task", Title: "Implement", ActorRole: "pm"},
		},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := wfStore.StartRunWithInput("sample", taskID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm": {Type: "agent", ID: "pm"},
	}, nil); err != nil {
		t.Fatalf("start run: %v", err)
	}
	nowText := now.Format(time.RFC3339)
	run := controldb.RuntimeRun{
		ID: "run-b3-" + taskID, WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
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
	return run
}

// infraFailWorkflowRun reports a run failure through the real HTTP endpoint.
func infraFailWorkflowRun(t *testing.T, s *Server, workspaceID string, run controldb.RuntimeRun, errorCode, errorMessage string) {
	t.Helper()
	body := strings.NewReader(`{"leaseGeneration":1,"errorCode":"` + errorCode + `","errorMessage":"` + errorMessage + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/fail", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{
		Node: controldb.RuntimeNode{ID: run.RuntimeNodeID, WorkspaceID: workspaceID},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunFail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowTaskInfraProbeFailureBacksOffInsteadOfDoneFailed: the FIRST
// docker-probe failure (executor_failed) on a workflow task returns the task
// to pending with a NotBefore backoff — NOT the terminal done_failed the old
// code archived. The workflow run stays parked on its step, so the next
// dispatch re-runs the SAME step.
func TestWorkflowTaskInfraProbeFailureBacksOffInsteadOfDoneFailed(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run := infraWorkflowTaskOnNode(t, s, workspaceID, "task-b3-backoff")

	infraFailWorkflowRun(t, s, workspaceID, run, "executor_failed", "docker sandbox: Docker daemon did not respond within 3s — Docker may be starting, stuck, or unhealthy")

	got, err := s.ts.GetTask("sample", "pm", "task-b3-backoff")
	if err != nil || got == nil {
		t.Fatalf("load task: %v", err)
	}
	if got.Status == entity.TaskStatusDoneFailed {
		t.Fatal("infra probe failure must NOT archive the workflow task as done_failed")
	}
	if got.Status != entity.TaskStatusPending {
		t.Fatalf("after infra fail 1: status=%s, want pending (re-drivable)", got.Status)
	}
	if got.InfraFailureStreak != 1 {
		t.Fatalf("streak=%d, want 1", got.InfraFailureStreak)
	}
	if got.NotBefore == nil || got.NotBefore.Before(time.Now().UTC().Add(4*time.Minute)) || got.NotBefore.After(time.Now().UTC().Add(6*time.Minute)) {
		t.Fatalf("NotBefore=%v, want ≈now+5m", got.NotBefore)
	}
	if got.LastError == "" || !strings.Contains(got.LastError, "Docker daemon did not respond") {
		t.Fatalf("LastError must carry the probe failure, got %q", got.LastError)
	}
	// The workflow run stays parked on the step: the re-dispatch re-runs it.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	wfRun, ok, err := wfStore.RunForTask("sample", "task-b3-backoff")
	if err != nil || !ok {
		t.Fatalf("workflow run lookup: ok=%v err=%v", ok, err)
	}
	if wfRun.Status != "active" || wfRun.ActiveStepID != "implement" {
		t.Fatalf("workflow must stay active on the failed step, got status=%s active=%s", wfRun.Status, wfRun.ActiveStepID)
	}
	instances, err := wfStore.ListStepInstances(wfRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.StepID == "implement" && inst.Status != "pending" && inst.Status != "running" {
			t.Fatalf("failed step must stay re-drivable, got status %s", inst.Status)
		}
	}
}

// TestWorkflowTaskInfraProbeFailureCapBlocks: the THIRD consecutive infra
// failure parks the workflow task in blocked (never done_failed) and leaves a
// human-visible notification comment — the documented Q0 blocked contract.
func TestWorkflowTaskInfraProbeFailureCapBlocks(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run := infraWorkflowTaskOnNode(t, s, workspaceID, "task-b3-blocked")

	for i := 0; i < 2; i++ {
		infraFailWorkflowRun(t, s, workspaceID, run, "executor_failed", "docker sandbox: Docker daemon did not respond within 3s")
		// Reset the run rows to re-fail: the backoff mutation only touches
		// the task, but FinishRuntimeRun closed the run; re-open it the way
		// a re-dispatch would.
		now := time.Now().UTC().Format(time.RFC3339)
		reopened := run
		reopened.Status = "running"
		reopened.LeaseExpiresAt = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
		reopened.ErrorCode = ""
		reopened.ErrorMessage = ""
		reopened.CreatedAt = now
		reopened.UpdatedAt = now
		if err := s.controlDB.UpsertRuntimeRun(reopened); err != nil {
			t.Fatalf("reopen run: %v", err)
		}
		if err := s.setTaskActiveRuntimeRun("sample", "pm", "task-b3-blocked", run.ID); err != nil {
			t.Fatalf("restamp token: %v", err)
		}
	}
	infraFailWorkflowRun(t, s, workspaceID, run, "executor_failed", "docker sandbox: Docker daemon did not respond within 3s")

	got, err := s.ts.GetTask("sample", "pm", "task-b3-blocked")
	if err != nil || got == nil {
		// blocked is non-terminal but the task may have moved storage keys;
		// fall through to the assert below with whatever GetTask returned.
		t.Fatalf("load task: %v", err)
	}
	if got.Status != entity.TaskStatusBlocked {
		t.Fatalf("after 3 infra failures: status=%s, want blocked (not done_failed %s)", got.Status, entity.TaskStatusDoneFailed)
	}
	if got.InfraFailureStreak != 3 {
		t.Fatalf("streak=%d, want 3", got.InfraFailureStreak)
	}
	comments, cerr := s.ts.ListComments("sample", "pm", "task-b3-blocked")
	if cerr != nil || len(comments) == 0 {
		t.Fatalf("blocked notification comment missing: %v", cerr)
	}
	found := false
	for _, c := range comments {
		if strings.Contains(c.Body, "blocked") || strings.Contains(c.Body, "解除封锁") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no comment mentions the block: %+v", comments)
	}
}

// TestWorkflowTaskBusinessFailureStillDoneFailed: an agent-reported business
// failure (agent_run_failed — NOT in the infra code set) on a workflow task
// keeps the B11 contract: done_failed + rework path, streak untouched.
func TestWorkflowTaskBusinessFailureStillDoneFailed(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run := infraWorkflowTaskOnNode(t, s, workspaceID, "task-b3-business")

	infraFailWorkflowRun(t, s, workspaceID, run, "agent_run_failed", "agent decided the work cannot proceed")

	archived, err := s.ts.ListArchivedTasks("sample", "pm")
	if err != nil {
		t.Fatal(err)
	}
	var got *entity.Task
	for _, at := range archived {
		if at.ID == "task-b3-business" {
			got = at
		}
	}
	if got == nil {
		t.Fatal("business failure must archive the task (done_failed)")
	}
	if got.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("business failure status=%s, want done_failed", got.Status)
	}
	if got.InfraFailureStreak != 0 {
		t.Fatalf("business failure must not touch the streak, got %d", got.InfraFailureStreak)
	}
}

// TestWorkflowTaskAbandonedStepStillDoneFailed: a run that FINISHED
// successfully without the agent completing its step keeps the fail-closed
// workflow_step_not_completed contract — the infra channel must not launder an
// abandoned step into a retryable pending.
func TestWorkflowTaskAbandonedStepStillDoneFailed(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	run := infraWorkflowTaskOnNode(t, s, workspaceID, "task-b3-abandoned")

	body := strings.NewReader(`{"leaseGeneration":1,"result":{"summary":"gave up"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/complete", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{
		Node: controldb.RuntimeNode{ID: run.RuntimeNodeID, WorkspaceID: workspaceID},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", rec.Code, rec.Body.String())
	}

	archived, err := s.ts.ListArchivedTasks("sample", "pm")
	if err != nil {
		t.Fatal(err)
	}
	var got *entity.Task
	for _, at := range archived {
		if at.ID == "task-b3-abandoned" {
			got = at
		}
	}
	if got == nil || got.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("abandoned step must archive done_failed, got %+v", got)
	}
	if !strings.Contains(got.LastError, "workflow step was not completed") {
		t.Fatalf("expected workflow_step_not_completed guidance, got %q", got.LastError)
	}
	if got.InfraFailureStreak != 0 {
		t.Fatalf("abandoned step must not touch the streak, got %d", got.InfraFailureStreak)
	}
}
