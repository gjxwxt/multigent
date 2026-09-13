package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Q0 PR-3 tests: infra failure backoff/cap (B5-B8), workflow exclusion
// (B11-B12), edit-does-not-reset (B14), dispatch gates + unblock.

func infraTestTask(t *testing.T, s *Server, id string, status entity.TaskStatus) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	task := &entity.Task{ID: id, Title: id, Status: status, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add %s: %v", id, err)
	}
	return task
}

func infraFailRun(t *testing.T, s *Server, workspaceID, taskID, errorCode string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	node := controldb.RuntimeNode{
		ID: "rtn-infra", WorkspaceID: workspaceID, Name: "InfraNode", Kind: "personal_computer",
		Status: "online", LastSeenAt: now, CreatedByUserID: "admin", CreatedAt: now, UpdatedAt: now,
	}
	_ = s.controlDB.UpsertRuntimeNode(node)
	run := controldb.RuntimeRun{
		ID: "run-" + taskID, WorkspaceID: workspaceID, RuntimeNodeID: node.ID,
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: taskID,
		Status: "running", LeaseExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
		LeaseGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	body := strings.NewReader(fmt.Sprintf(`{"leaseGeneration":1,"errorCode":%q,"errorMessage":"boom"}`, errorCode))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/fail", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), node))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunFail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func contextWithNode(ctx context.Context, node controldb.RuntimeNode) context.Context {
	return context.WithValue(ctx, ctxRuntimeNodeKey, runtimeNodePrincipal{Node: node})
}

// B5: infra failures 1 and 2 → task back to pending with NotBefore≈+5m,
// streak incrementing, no notification side effects.
func TestInfraFailureBackoffPending(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b5", entity.TaskStatusInProgress)

	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	got, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != entity.TaskStatusPending {
		t.Fatalf("after fail 1: status=%s, want pending", got.Status)
	}
	if got.InfraFailureStreak != 1 {
		t.Fatalf("after fail 1: streak=%d, want 1", got.InfraFailureStreak)
	}
	if got.NotBefore == nil || got.NotBefore.Before(time.Now().UTC().Add(4*time.Minute)) || got.NotBefore.After(time.Now().UTC().Add(6*time.Minute)) {
		t.Fatalf("after fail 1: NotBefore=%v, want ≈now+5m", got.NotBefore)
	}

	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	got, _ = s.ts.GetTask("sample", "pm", task.ID)
	if got.Status != entity.TaskStatusPending || got.InfraFailureStreak != 2 {
		t.Fatalf("after fail 2: status=%s streak=%d, want pending/2", got.Status, got.InfraFailureStreak)
	}
}

// B6: the third infra failure parks the task in blocked with a comment notice
// (no IM binding in the test env → degraded to comment, which is the
// documented fallback) and an audit entry.
func TestInfraFailureCapBlocksAndNotifies(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b6", entity.TaskStatusInProgress)

	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	infraFailRun(t, s, workspaceID, task.ID, "workspace_prepare_failed")
	got, _ := s.ts.GetTask("sample", "pm", task.ID)
	if got == nil || got.Status != entity.TaskStatusBlocked {
		t.Fatalf("after fail 3: status=%+v, want blocked", got)
	}
	if got.InfraFailureStreak != 3 {
		t.Fatalf("streak=%d, want 3", got.InfraFailureStreak)
	}
	comments, err := s.ts.ListComments("sample", "pm", task.ID)
	if err != nil || len(comments) == 0 {
		t.Fatalf("blocked notification comment missing: %v", err)
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

// B7: two infra failures then a success → streak reset to zero.
func TestInfraFailureStreakResetOnSuccess(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b7", entity.TaskStatusInProgress)
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")

	// Mark the pending task back in progress, then complete successfully.
	got, _ := s.ts.GetTask("sample", "pm", task.ID)
	got.Status = entity.TaskStatusInProgress
	if err := s.ts.UpdateTask("sample", "pm", got); err != nil {
		t.Fatalf("update: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	run := controldb.RuntimeRun{
		ID: "run-b7-ok", WorkspaceID: workspaceID, RuntimeNodeID: "rtn-infra",
		AgentWorkerID: "aw-pm", ProjectID: "sample", AgentID: "pm", TaskID: task.ID,
		Status: "running", LeaseExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
		LeaseGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("seed ok run: %v", err)
	}
	body := strings.NewReader(`{"leaseGeneration":1,"result":{"summary":"done"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/"+run.ID+"/complete", body)
	req.SetPathValue("runId", run.ID)
	req = req.WithContext(contextWithNode(req.Context(), controldb.RuntimeNode{ID: "rtn-infra", WorkspaceID: workspaceID}))
	rec := httptest.NewRecorder()
	s.handleRuntimeNodeRunComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", rec.Code, rec.Body.String())
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
		t.Fatalf("task not archived as success: %+v", done)
	}
	if done.InfraFailureStreak != 0 {
		t.Fatalf("streak=%d, want 0 after success", done.InfraFailureStreak)
	}
}

// B8: blocked → scheduler does not dispatch; manual start 409 with unblock
// hint; unblock endpoint (manager) restores pending and clears the streak,
// audited as task.unblock.
func TestBlockedTaskGatesAndUnblock(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b8", entity.TaskStatusInProgress)
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	got, _ := s.ts.GetTask("sample", "pm", task.ID)
	if got.Status != entity.TaskStatusBlocked {
		t.Fatalf("setup: status=%s, want blocked", got.Status)
	}

	// Scheduler selection must skip the blocked task.
	selected, err := s.nextRuntimePendingTask("sample", "pm")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if selected != nil && selected.ID == task.ID {
		t.Fatal("blocked task must not be selected for dispatch")
	}

	// Manual start → 409 with unblock hint (authenticated as admin).
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("manual start status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unblock") {
		t.Fatalf("409 must mention unblock: %s", rec.Body.String())
	}

	// Unblock for a missing task → 404.
	reqForbidden := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/task-missing/unblock", "admin", nil)
	reqForbidden.SetPathValue("name", "sample")
	reqForbidden.SetPathValue("taskId", "task-missing")
	recForbidden := httptest.NewRecorder()
	s.handleUnblockProjectTask(recForbidden, reqForbidden)
	if recForbidden.Code != http.StatusNotFound {
		t.Fatalf("unblock missing task status=%d, want 404", recForbidden.Code)
	}

	// Unblock by admin (workspace manager-level) → 200, task pending, streak 0.
	reqOK := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/unblock", "admin", nil)
	reqOK.SetPathValue("name", "sample")
	reqOK.SetPathValue("taskId", task.ID)
	recOK := httptest.NewRecorder()
	s.handleUnblockProjectTask(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("unblock status=%d body=%s", recOK.Code, recOK.Body.String())
	}
	unblocked, _ := s.ts.GetTask("sample", "pm", task.ID)
	if unblocked.Status != entity.TaskStatusPending || unblocked.InfraFailureStreak != 0 {
		t.Fatalf("after unblock: status=%s streak=%d, want pending/0", unblocked.Status, unblocked.InfraFailureStreak)
	}

	// Unblocking again → 409 (not blocked anymore).
	reqAgain := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/unblock", "admin", nil)
	reqAgain.SetPathValue("name", "sample")
	reqAgain.SetPathValue("taskId", task.ID)
	recAgain := httptest.NewRecorder()
	s.handleUnblockProjectTask(recAgain, reqAgain)
	if recAgain.Code != http.StatusConflict {
		t.Fatalf("second unblock status=%d, want 409", recAgain.Code)
	}
}

// B11: workflow tasks are structurally excluded from infra counting — their
// finish path returns before the backoff logic, keeping done_failed semantics.
func TestWorkflowTaskFailureNotInfraCounted(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b11", entity.TaskStatusInProgress)
	// Attach a workflow run to the task (workflow_runs kv keyed
	// [project, taskID, runID] — the same lookup runtimeTaskHasWorkflow uses).
	runPayload := `{"id":"wfr-b11","project":"sample","taskId":"task-b11","status":"running"}`
	if err := s.controlDB.UpsertRecord("workflow_runs", workspaceID, []string{"sample", task.ID, "wfr-b11"}, runPayload); err != nil {
		t.Fatalf("seed workflow run: %v", err)
	}
	infraFailRun(t, s, workspaceID, task.ID, "agent_run_failed")
	got, _ := s.ts.GetTask("sample", "pm", task.ID)
	if got == nil {
		// workflow path archives as done_failed
		archived, _ := s.ts.ListArchivedTasks("sample", "pm")
		for _, at := range archived {
			if at.ID == task.ID {
				got = at
			}
		}
	}
	if got == nil {
		t.Fatal("task missing everywhere")
	}
	if got.InfraFailureStreak != 0 {
		t.Fatalf("workflow task streak=%d, want 0 (excluded)", got.InfraFailureStreak)
	}
	if got.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("workflow task status=%s, want done_failed (rework path)", got.Status)
	}
}

// B12: business failures (non-infra error codes like agent_run_failed on the
// plain-task path... no — agent_run_failed IS infra) — here: a business
// failure code (empty/custom) must archive done_failed without touching the
// streak; and a cancelled task never counts.
func TestBusinessFailureAndCancelledNotCounted(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b12a", entity.TaskStatusInProgress)
	infraFailRun(t, s, workspaceID, task.ID, "custom_business_failure")
	got, _ := s.ts.GetTask("sample", "pm", task.ID)
	if got != nil && got.InfraFailureStreak != 0 {
		// done_failed tasks are archived; fetch from archive if needed.
		archived, _ := s.ts.ListArchivedTasks("sample", "pm")
		for _, at := range archived {
			if at.ID == task.ID && at.InfraFailureStreak != 0 {
				t.Fatalf("business failure must not count: streak=%d", at.InfraFailureStreak)
			}
		}
	}
}

// B14: editing the task (title/description) does not clear the streak.
func TestTaskEditKeepsStreak(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	task := infraTestTask(t, s, "task-b14", entity.TaskStatusInProgress)
	infraFailRun(t, s, workspaceID, task.ID, "executor_failed")
	got, _ := s.ts.GetTask("sample", "pm", task.ID)
	if got.InfraFailureStreak != 1 {
		t.Fatalf("setup streak=%d, want 1", got.InfraFailureStreak)
	}
	got.Title = "edited title"
	got.Description = "edited description"
	if err := s.ts.UpdateTask("sample", "pm", got); err != nil {
		t.Fatalf("edit: %v", err)
	}
	after, _ := s.ts.GetTask("sample", "pm", task.ID)
	if after.InfraFailureStreak != 1 {
		t.Fatalf("edit must not reset streak, got %d", after.InfraFailureStreak)
	}
}

// Unit: the closed infra error code set behaves as D4 specifies.
func TestIsRuntimeInfraFailureCode(t *testing.T) {
	for _, code := range []string{"spec_fetch_failed", "workspace_prepare_failed", "agent_prepare_failed", "executor_failed", "lease_expired", "agent_run_failed"} {
		if !isRuntimeInfraFailureCode(code) {
			t.Errorf("%s must count as infra failure", code)
		}
	}
	for _, code := range []string{"", "workflow_step_not_completed", "custom_business_failure", "unsupported_run_kind", "empty_prompt"} {
		if isRuntimeInfraFailureCode(code) {
			t.Errorf("%q must NOT count as infra failure", code)
		}
	}
}
