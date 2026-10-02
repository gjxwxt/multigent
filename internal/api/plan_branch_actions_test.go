package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func branchActionRequest(username, project, taskID, branchID, action string, body any) *http.Request {
	reader := bytes.NewReader(nil)
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/tasks/"+taskID+"/workflow/branch/"+branchID+"/"+action, reader)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", project)
	req.SetPathValue("taskId", taskID)
	req.SetPathValue("branchId", branchID)
	return req.WithContext(context.WithValue(req.Context(), ctxUserKey, username))
}

// seedFailedBranchFixture produces the production failure shape: a frozen
// plan-driven stage whose wp-a branch failed (instance failed, child run
// failed, child task done_failed).
func seedFailedBranchFixture(t *testing.T) (*Server, string, *entity.Task, workflowstore.Store, string, entity.WorkflowBranchInstance) {
	t.Helper()
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := *workflowstore.NewStore(s.controlDB, workspaceID)

	if rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}); rec.Code != http.StatusOK {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	inst, found := branchInstanceFor(t, &wfStore, runID, "parallel", "wp-a")
	if !found {
		t.Fatal("wp-a must be materialized in wave 0")
	}
	// Fail the child through the production state shape: child task
	// done_failed + child run failed on its start step.
	child, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	prev := child.Status
	now := time.Now().UTC()
	child.Status = entity.TaskStatusDoneFailed
	child.LastError = "module A could not be delivered"
	child.UpdatedAt = now
	entity.ApplyStatusTimestamps(child, prev, now)
	if err := s.ts.PersistTask("sample", "pm", child); err != nil {
		t.Fatal(err)
	}
	childRun, foundRun, err := wfStore.RunForTask("sample", child.ID)
	if err != nil || !foundRun {
		t.Fatalf("child run lookup: found=%v err=%v", foundRun, err)
	}
	childInstances, err := wfStore.ListStepInstances(childRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range childInstances {
		childInstances[i].Status = "failed"
		childInstances[i].Summary = "module A could not be delivered"
		childInstances[i].FinishedAt = now
		if err := wfStore.SaveStepInstance(&childInstances[i]); err != nil {
			t.Fatal(err)
		}
	}
	childRun.Status = "failed"
	childRun.UpdatedAt = now
	if err := wfStore.SaveRun(&childRun); err != nil {
		t.Fatal(err)
	}
	inst.Status = workflowstore.PlanWPStateFailed
	inst.Summary = "module A could not be delivered"
	if err := wfStore.SaveBranchInstance(&inst); err != nil {
		t.Fatal(err)
	}
	root, _, err := s.findTaskInProject("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	return s, workspaceID, root, wfStore, runID, inst
}

func TestBranchRetryRevivesFailedBranch(t *testing.T) {
	s, _, root, wfStore, runID, inst := seedFailedBranchFixture(t)

	req := branchActionRequest("admin", "sample", root.ID, "wp-a", "retry", map[string]string{"reason": "transient blip"})
	rec := httptest.NewRecorder()
	s.handlePostTaskBranchRetry(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp branchActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ChildTaskID != inst.ChildTaskID {
		t.Fatalf("retry must reuse the same child identity, got %+v", resp)
	}

	// Child task reopened, child run active again, instance running with the
	// budget bumped.
	gotChild, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.Status != entity.TaskStatusInProgress {
		t.Fatalf("child task must reopen, got %s", gotChild.Status)
	}
	childRun, _, err := wfStore.RunForTask("sample", inst.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.Status != "active" {
		t.Fatalf("child run must be reactivated, got %s", childRun.Status)
	}
	instAfter, _ := branchInstanceFor(t, &wfStore, runID, "parallel", "wp-a")
	if instAfter.Status != workflowstore.PlanWPStateRunning {
		t.Fatalf("instance must be running after retry, got %s", instAfter.Status)
	}
	if instAfter.InputValues[planWPRetryBudgetField] != "1" {
		t.Fatalf("retry budget must increment, got %v", instAfter.InputValues)
	}
}

func TestBranchRetryRefusesNonFailedAndCancelledRuns(t *testing.T) {
	s, _, root, wfStore, _, inst := seedFailedBranchFixture(t)

	// A completed (non-failed) branch refuses.
	req := branchActionRequest("admin", "sample", root.ID, "wp-b", "retry", nil)
	rec := httptest.NewRecorder()
	s.handlePostTaskBranchRetry(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retrying a running branch must 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// A cancelled child run refuses with an operator-readable message.
	child, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	childRun, _, err := wfStore.RunForTask("sample", child.ID)
	if err != nil {
		t.Fatal(err)
	}
	childRun.Status = "cancelled"
	if err := wfStore.SaveRun(&childRun); err != nil {
		t.Fatal(err)
	}
	req = branchActionRequest("admin", "sample", root.ID, "wp-a", "retry", nil)
	rec = httptest.NewRecorder()
	s.handlePostTaskBranchRetry(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retrying a cancelled child must 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("the refusal must name the state, got %s", rec.Body.String())
	}
}

func TestBranchDeferPreservesFailureAndUnblocksProgress(t *testing.T) {
	s, _, root, wfStore, runID, inst := seedFailedBranchFixture(t)
	_ = inst

	req := branchActionRequest("admin", "sample", root.ID, "wp-a", "defer", map[string]any{
		"reason":          "not this round",
		"failureCategory": "environment",
		"createFollowUp":  true,
	})
	rec := httptest.NewRecorder()
	s.handlePostTaskBranchDefer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("defer must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp branchActionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Status != workflowstore.PlanWPStateDeferred {
		t.Fatalf("defer must land deferred, got %+v", resp)
	}
	if resp.FollowUpID == "" {
		t.Fatal("createFollowUp must mint a follow-up task")
	}
	followUp, err := s.ts.GetTask("sample", "pm", resp.FollowUpID)
	if err != nil {
		t.Fatal(err)
	}
	if followUp.Status != entity.TaskStatusPending {
		t.Fatalf("follow-up task must be pending, got %s", followUp.Status)
	}

	instAfter, _ := branchInstanceFor(t, &wfStore, runID, "parallel", "wp-a")
	if instAfter.Status != workflowstore.PlanWPStateDeferred {
		t.Fatalf("instance must be deferred, got %s", instAfter.Status)
	}
	if !strings.Contains(instAfter.Summary, "original failure: module A could not be delivered") {
		t.Fatalf("deferred summary must preserve the failure, got %q", instAfter.Summary)
	}
	if instAfter.InputValues[failureCategoryField] != "environment" {
		t.Fatalf("category must be recorded, got %v", instAfter.InputValues)
	}

	// The deferred subtree must read as settled, not blocked.
	instances, err := wfStore.BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, one := range instances {
		states[one.BranchID] = one.Status
	}
	_, version, ok, err := wfStore.LoadFrozenPlanForRun("sample", runID)
	if err != nil || !ok {
		t.Fatalf("frozen plan must load: ok=%v err=%v", ok, err)
	}
	progress, err := workflowstore.EvaluatePlanProgress(version.Plan, states)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Blocked) != 0 {
		t.Fatalf("a deferral must clear the blocked set, got %+v", progress.Blocked)
	}
}

func TestBranchDeferValidatesCategory(t *testing.T) {
	s, _, root, _, _, inst := seedFailedBranchFixture(t)
	_ = inst

	req := branchActionRequest("admin", "sample", root.ID, "wp-a", "defer", map[string]string{"failureCategory": "made-up"})
	rec := httptest.NewRecorder()
	s.handlePostTaskBranchDefer(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid category must 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBranchDeferRedrivesParkedStage is the live-fire regression (batch-test1,
// 2026-10-02): a stage whose LAST failure is deferred has no completion event
// left to drive the join — every branch is already terminal and nothing is in
// flight. Before the fix the defer re-stamped the instance and returned,
// parking the run at the parallel stage forever (the reconciler skips
// blocked roots). The defer must now re-drive the stage itself: with every
// work package settled (completed / deferred / deferred-by-propagation) the
// join advances in the same request.
func TestBranchDeferRedrivesParkedStage(t *testing.T) {
	s, workspaceID, root, wfStore, runID, inst := seedFailedBranchFixture(t)

	// Fail wp-b too: the live-fire parked shape had NO successful branch —
	// every failure was ruled out by hand, so no completion event is left to
	// drive the join.
	failBranchChild(t, s, "sample", workspaceID, runID, "wp-b", "module B could not be delivered")
	run, _, err := wfStore.RunForTask("sample", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ActiveStepID != "parallel" {
		t.Fatalf("stage must still be parked on parallel before the defers, got %s", run.ActiveStepID)
	}

	// Defer wp-a first (stage must stay parked: wp-b is still failed).
	req := branchActionRequest("admin", "sample", root.ID, "wp-a", "defer", map[string]any{
		"reason":          "not this round",
		"failureCategory": "business_assertion",
	})
	rec := httptest.NewRecorder()
	s.handlePostTaskBranchDefer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("defer wp-a must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	run, _, _ = wfStore.RunForTask("sample", root.ID)
	if run.ActiveStepID != "parallel" {
		t.Fatalf("stage must stay parked while wp-b is still failed, got %s", run.ActiveStepID)
	}

	// Defer wp-b — the LAST failure. Every package is now settled
	// (deferred / deferred-by-propagation), nothing is in flight, and no
	// future completion event exists: this defer itself must advance the
	// join.
	req2 := branchActionRequest("admin", "sample", root.ID, "wp-b", "defer", map[string]any{
		"reason":          "last failure ruled out of this round",
		"failureCategory": "environment",
	})
	rec2 := httptest.NewRecorder()
	s.handlePostTaskBranchDefer(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("defer wp-b must be 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// The join must have advanced in the same request: nothing else will
	// ever drive it (no running branch, no pending completion).
	run, _, err = wfStore.RunForTask("sample", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ActiveStepID == "parallel" {
		t.Fatal("defer of the last failure must re-drive the parked join (run still parked at parallel)")
	}
	// The fixture's parallel stage is its last step, so advancing the join
	// completes the run — "no longer parked at parallel" is the assertion.
	if run.ActiveStepID == "parallel" && run.Status == "active" {
		t.Fatal("defer of the last failure must re-drive the parked join (run still parked at parallel)")
	}
	_ = inst
}

// failBranchChild drives one materialized branch child into the production
// failure shape (task done_failed, child run failed, instance failed).
func failBranchChild(t *testing.T, s *Server, project, workspaceID, runID, branchID, reason string) {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	inst, found := branchInstanceFor(t, wfStore, runID, "parallel", branchID)
	if !found {
		t.Fatalf("branch %s must be materialized", branchID)
	}
	child, err := s.ts.GetTask(project, "pm", inst.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	prev := child.Status
	now := time.Now().UTC()
	child.Status = entity.TaskStatusDoneFailed
	child.LastError = reason
	child.UpdatedAt = now
	entity.ApplyStatusTimestamps(child, prev, now)
	if err := s.ts.PersistTask(project, "pm", child); err != nil {
		t.Fatal(err)
	}
	childRun, foundRun, err := wfStore.RunForTask(project, child.ID)
	if err != nil || !foundRun {
		t.Fatalf("child run lookup for %s: found=%v err=%v", branchID, foundRun, err)
	}
	childInstances, err := wfStore.ListStepInstances(childRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range childInstances {
		childInstances[i].Status = "failed"
		childInstances[i].Summary = reason
		childInstances[i].FinishedAt = now
		if err := wfStore.SaveStepInstance(&childInstances[i]); err != nil {
			t.Fatal(err)
		}
	}
	childRun.Status = "failed"
	childRun.UpdatedAt = now
	if err := wfStore.SaveRun(&childRun); err != nil {
		t.Fatal(err)
	}
	inst.Status = workflowstore.PlanWPStateFailed
	inst.Summary = reason
	if err := wfStore.SaveBranchInstance(&inst); err != nil {
		t.Fatal(err)
	}
}
