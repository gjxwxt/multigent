package api

// Branch-level human rulings on plan-driven parallel stages (P0-2): when a
// branch child fails, the stage stalls (failed work packages block their
// dependents and the join). These endpoints give the operator exactly two
// sanctioned ways out — retry the failed branch in place, or defer it out of
// this round — without ever touching the frozen plan bytes, the digest, or
// the plan version.
//
// Identity rules (both deliberately rigid):
//   - retry REUSES the existing branch instance and its child task. Re-running
//     planStageBranches is impossible by design (the existingIDs gate skips
//     any package that already has an instance), so the retry resets the child
//     run through ReactivateFailedRunForStep and re-queues the SAME task.
//   - defer re-stamps the failed instance as deferred; the deferred state
//     propagates deterministically through EvaluatePlanProgress, so no manual
//     cascade onto dependents exists or is needed.
//
// Both actions are audited and leave the original failure summary in the
// instance/event trail — a ruling must never erase what actually happened.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// planWPMaxRetries bounds operator retries per work package. A human keeps
// the key (every retry is a manual click), but the budget stops the
// retry-fail-retry loop from burning sandboxes forever.
const planWPMaxRetries = 2

// planWPRetryBudgetField counts completed retry cycles on the branch instance.
// Platform-owned like the review_rounds counter: reported values are never
// trusted to increment it.
const planWPRetryBudgetField = "wp_retry_budget"

// failureCategoryField carries the structured failure class the operator
// assigns when deferring. It is the seed dataset for any future automatic
// triage — without real categories the classifier can never be built.
const failureCategoryField = "failure_category"

// PlanFailureCategories is the closed enum of failure classes. "unknown"
// exists so the UI never forces a guess; it is a valid choice, not a default.
var PlanFailureCategories = []string{"compile_test", "business_assertion", "environment", "unknown"}

func validFailureCategory(v string) bool {
	for _, c := range PlanFailureCategories {
		if v == c {
			return true
		}
	}
	return false
}

type branchActionBody struct {
	Reason          string `json:"reason"`
	FailureCategory string `json:"failureCategory"`
	CreateFollowUp  bool   `json:"createFollowUp"`
}

type branchActionResponse struct {
	OK          bool   `json:"ok"`
	BranchID    string `json:"branchId"`
	Status      string `json:"status"`
	ChildTaskID string `json:"childTaskId,omitempty"`
	FollowUpID  string `json:"followUpTaskId,omitempty"`
}

func (s *Server) handlePostTaskBranchRetry(w http.ResponseWriter, r *http.Request) {
	s.handleTaskBranchAction(w, r, "retry")
}

func (s *Server) handlePostTaskBranchDefer(w http.ResponseWriter, r *http.Request) {
	s.handleTaskBranchAction(w, r, "defer")
}

func (s *Server) handleTaskBranchAction(w http.ResponseWriter, r *http.Request, action string) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	branchID := r.PathValue("branchId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	var body branchActionBody
	_ = s.readJSON(w, r, &body) // body is optional for both actions

	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	root, rootAgent, err := s.findTaskInProject(project, taskID)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if !s.canOperateAgent(r, project, rootAgent) {
		s.jsonError(w, http.StatusForbidden, "agent operator access required")
		return
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, root.ID)
	if err != nil || !found {
		s.jsonError(w, http.StatusNotFound, "workflow run not found for task")
		return
	}
	inst, stepID, err := s.branchInstanceForAction(wfStore, run.ID, branchID)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, err.Error())
		return
	}

	switch action {
	case "retry":
		resp, status, err := s.retryPlanBranch(workspaceID, project, rootAgent, root, run, inst, stepID, strings.TrimSpace(body.Reason))
		if err != nil {
			if status != http.StatusInternalServerError {
				s.jsonError(w, status, err.Error())
				return
			}
			s.serverError(w, err)
			return
		}
		s.auditLog(auditLogInput{
			Action:       "workflow.branch.retry",
			ResourceType: "task",
			ResourceID:   project + "/" + root.ID,
			Summary:      fmt.Sprintf("branch %s retried (%s)", branchID, resp.Status),
			After:        map[string]any{"branchId": branchID, "childTaskId": resp.ChildTaskID, "reason": body.Reason},
			Request:      r,
		})
		_ = json.NewEncoder(w).Encode(resp)
	case "defer":
		resp, status, err := s.deferPlanBranch(workspaceID, project, rootAgent, root, run, inst, strings.TrimSpace(body.Reason), strings.TrimSpace(body.FailureCategory), body.CreateFollowUp, r)
		if err != nil {
			if status != http.StatusInternalServerError {
				s.jsonError(w, status, err.Error())
				return
			}
			s.serverError(w, err)
			return
		}
		s.auditLog(auditLogInput{
			Action:       "workflow.branch.defer",
			ResourceType: "task",
			ResourceID:   project + "/" + root.ID,
			Summary:      fmt.Sprintf("branch %s deferred (%s)", branchID, body.FailureCategory),
			After:        map[string]any{"branchId": branchID, "category": body.FailureCategory, "followUpTaskId": resp.FollowUpID, "reason": body.Reason},
			Request:      r,
		})
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// branchInstanceForAction resolves the branch instance anywhere on the run
// (any step) so a ruling can land on a wave that already scrolled past the
// stage's current bookkeeping.
func (s *Server) branchInstanceForAction(wfStore *workflowstore.Store, runID, branchID string) (entity.WorkflowBranchInstance, string, error) {
	instances, err := wfStore.ListBranchInstances(runID)
	if err != nil {
		return entity.WorkflowBranchInstance{}, "", err
	}
	for _, inst := range instances {
		if inst.BranchID == branchID {
			return inst, inst.StepID, nil
		}
	}
	return entity.WorkflowBranchInstance{}, "", fmt.Errorf("branch %q not found on run", branchID)
}

// retryPlanBranch revives a failed branch through its existing identities:
// child run reactivated (resetting only the failed step), child task reopened,
// instance back to running, runtime re-queued. Preconditions are explicit and
// operator-readable: the child run must be failed (a cancelled child says so),
// the budget must have room.
func (s *Server) retryPlanBranch(workspaceID, project, rootAgent string, root *entity.Task, run entity.WorkflowRun, inst entity.WorkflowBranchInstance, stepID, reason string) (branchActionResponse, int, error) {
	resp := branchActionResponse{BranchID: inst.BranchID}
	if strings.TrimSpace(inst.Status) != workflowstore.PlanWPStateFailed {
		return resp, http.StatusConflict, fmt.Errorf("branch %q is %q, not failed; only a failed branch can be retried", inst.BranchID, inst.Status)
	}
	budget := 0
	if inst.InputValues != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(inst.InputValues[planWPRetryBudgetField])); err == nil && n > 0 {
			budget = n
		}
	}
	if budget >= planWPMaxRetries {
		return resp, http.StatusConflict, fmt.Errorf("branch %q exhausted its retry budget (%d of %d); defer it or re-plan instead", inst.BranchID, budget, planWPMaxRetries)
	}

	childID := strings.TrimSpace(inst.ChildTaskID)
	if childID == "" {
		return resp, http.StatusConflict, fmt.Errorf("branch %q has no child task to retry", inst.BranchID)
	}
	child, childAgent, err := s.findTaskInProject(project, childID)
	if err != nil {
		return resp, http.StatusInternalServerError, err
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	childRun, found, err := wfStore.RunForTask(project, childID)
	if err != nil || !found {
		return resp, http.StatusNotFound, fmt.Errorf("child run not found for branch %q", inst.BranchID)
	}
	// Operator-readable precondition: ReactivateFailedRunForStep only accepts
	// a failed run; a cancelled child needs the root cancelled instead.
	if status := strings.TrimSpace(childRun.Status); status != "failed" {
		if status == "cancelled" {
			return resp, http.StatusConflict, fmt.Errorf("branch %q child run is cancelled; retry is not available — cancel the root task instead", inst.BranchID)
		}
		return resp, http.StatusConflict, fmt.Errorf("branch %q child run is %s, not failed; nothing to reactivate", inst.BranchID, status)
	}
	// Which step did the child run fail on? ReactivateFailedRunForStep wants
	// exactly that step.
	failedStepID, err := s.failedStepOfRun(wfStore, childRun)
	if err != nil {
		return resp, http.StatusInternalServerError, err
	}
	reactivated, err := wfStore.ReactivateFailedRunForStep(project, childID, childRun.ID, failedStepID, retryReason(reason, inst))
	if err != nil {
		return resp, http.StatusInternalServerError, err
	}
	// Reopen the child task itself (done_failed is terminal in the task store).
	prev := child.Status
	now := time.Now().UTC()
	child.Status = entity.TaskStatusInProgress
	child.LastError = ""
	child.UpdatedAt = now
	entity.ApplyStatusTimestamps(child, prev, now)
	if err := s.ts.PersistTask(project, childAgent, child); err != nil {
		return resp, http.StatusInternalServerError, err
	}
	// Instance back to running with the incremented retry budget.
	inst.Status = workflowstore.PlanWPStateRunning
	inst.Summary = ""
	inst.UpdatedAt = now
	if inst.InputValues == nil {
		inst.InputValues = map[string]string{}
	}
	inst.InputValues[planWPRetryBudgetField] = strconv.Itoa(budget + 1)
	if err := wfStore.SaveBranchInstance(&inst); err != nil {
		return resp, http.StatusInternalServerError, err
	}
	s.fireTaskTriggerOrQueueRuntime(workspaceID, project, childAgent, child, nil, retryReason(reason, inst))
	s.appendRootTaskComment(project, rootAgent, root, fmt.Sprintf("branch %s retried (attempt %d of %d)%s", inst.BranchID, budget+1, planWPMaxRetries, retryReasonSuffix(reason)))
	resp = branchActionResponse{OK: true, BranchID: inst.BranchID, Status: reactivated.Status, ChildTaskID: childID}
	return resp, http.StatusOK, nil
}

// deferPlanBranch re-stamps a failed branch as deferred. The original failure
// summary is preserved verbatim inside the new summary, the category is
// recorded on the instance, and an optional follow-up task keeps the waived
// work from silently disappearing at the end of the round.
func (s *Server) deferPlanBranch(workspaceID, project, rootAgent string, root *entity.Task, run entity.WorkflowRun, inst entity.WorkflowBranchInstance, reason, category string, createFollowUp bool, r *http.Request) (branchActionResponse, int, error) {
	resp := branchActionResponse{BranchID: inst.BranchID}
	status := strings.TrimSpace(inst.Status)
	if status != workflowstore.PlanWPStateFailed && status != workflowstore.PlanWPStateDeferred {
		return resp, http.StatusConflict, fmt.Errorf("branch %q is %q; only a failed (or already deferred) branch can be deferred", inst.BranchID, status)
	}
	if category == "" {
		category = "unknown"
	}
	if !validFailureCategory(category) {
		return resp, http.StatusBadRequest, fmt.Errorf("failureCategory must be one of: %s", strings.Join(PlanFailureCategories, ", "))
	}
	original := strings.TrimSpace(inst.Summary)
	deferredSummary := "deferred"
	if original != "" {
		deferredSummary += "; original failure: " + original
	}
	if reason != "" {
		deferredSummary += "; operator note: " + reason
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	now := time.Now().UTC()
	inst.Status = workflowstore.PlanWPStateDeferred
	inst.Summary = deferredSummary
	inst.UpdatedAt = now
	if inst.InputValues == nil {
		inst.InputValues = map[string]string{}
	}
	inst.InputValues[failureCategoryField] = category
	if err := wfStore.SaveBranchInstance(&inst); err != nil {
		return resp, http.StatusInternalServerError, err
	}
	_ = wfStore.SaveStepEvent(&entity.WorkflowStepEvent{
		ID:        "evt_" + run.ID + "_defer_" + strconv.FormatInt(now.UnixNano(), 36),
		RunID:     run.ID,
		StepID:    inst.StepID,
		Status:    "deferred",
		Summary:   deferredSummary,
		CreatedAt: now,
	})
	if createFollowUp {
		followUp, err := s.createDeferredFollowUpTask(project, root, inst, category, deferredSummary)
		if err != nil {
			log.Printf("[plan-branch-actions] deferred follow-up task for branch %s failed: %v", inst.BranchID, err)
		} else {
			resp.FollowUpID = followUp.ID
		}
	}
	s.appendRootTaskComment(project, rootAgent, root, fmt.Sprintf("branch %s deferred (category %s)%s: %s", inst.BranchID, category, retryReasonSuffix(reason), deferredSummary))
	resp.OK = true
	resp.Status = workflowstore.PlanWPStateDeferred
	resp.ChildTaskID = strings.TrimSpace(inst.ChildTaskID)

	// A defer can be the ruling that un-parks the stage: the failure that
	// blocked the join is now settled. Re-drive the stage through the same
	// branch-transition machinery a completion uses — the store's idempotent
	// replay path advances the join when every package is settled, and the
	// API layer below materializes held-back waves otherwise. Without this,
	// a stage whose last failure was deferred (or whose in-flight count
	// dropped to zero) sits parked forever: nothing will complete, so the
	// completion-driven re-drive never fires (live fire, batch-test1
	// 2026-10-02).
	drive, err := wfStore.CompleteBranchAndMaybeAdvance(project, root.ID, run.ID, inst.StepID, inst.BranchID, inst.ChildTaskID, deferredSummary, nil, workflowstore.PlanWPStateDeferred)
	if err != nil {
		// A concurrent winner (another ruling, a child completion, a cancel)
		// may have moved the run past this stage first — the stage state is
		// already correct, so a stale re-drive is not an operator error.
		log.Printf("[plan-branch-actions] defer re-drive for branch %s on run %s: %v", inst.BranchID, run.ID, err)
		return resp, http.StatusOK, nil
	}
	if drive.Transition.Run.ID != "" {
		if err := s.advanceParentAfterBranchCompletion(workspaceID, project, drive, r); err != nil {
			log.Printf("[plan-branch-actions] defer join advance for run %s: %v", run.ID, err)
		}
		return resp, http.StatusOK, nil
	}
	materialized, waveErr := s.materializeNextPlanWave(workspaceID, project, rootAgent, root, workflowstore.BranchTransitionResult{
		Branch:     inst,
		Transition: workflowstore.TransitionResult{Run: run},
	}, r)
	if waveErr != nil {
		log.Printf("[plan-branch-actions] defer wave re-drive for run %s: %v", run.ID, waveErr)
		return resp, http.StatusOK, nil
	}
	if materialized {
		// The ruling freed capacity and a new wave is running: the root is
		// no longer blocked on the deferred failure.
		prev := root.Status
		root.Status = entity.TaskStatusInProgress
		root.LastError = ""
		root.UpdatedAt = time.Now().UTC()
		entity.ApplyStatusTimestamps(root, prev, time.Now().UTC())
		if err := s.ts.PersistTask(project, rootAgent, root); err != nil {
			log.Printf("[plan-branch-actions] root restore after defer wave: %v", err)
		}
	}
	return resp, http.StatusOK, nil
}

// createDeferredFollowUpTask converts the waived work package into a normal
// backlog task so the deferral has a life after this round (the review
// findings and deploy-AI findings flows use the same conversion).
func (s *Server) createDeferredFollowUpTask(project string, root *entity.Task, inst entity.WorkflowBranchInstance, category, summary string) (*entity.Task, error) {
	now := time.Now().UTC()
	followUp := &entity.Task{
		ID:          entity.NewTaskID(),
		Title:       "Deferred: " + strings.TrimSpace(inst.Summary) + " (" + inst.BranchID + ")",
		Type:        entity.TaskTypeChore,
		Priority:    2,
		Assignee:    root.Assignee,
		CreatedBy:   root.CreatedBy,
		Status:      entity.TaskStatusPending,
		Description: summary + "\n\nDeferred from delivery plan branch " + inst.BranchID + " (failure category: " + category + "). Pick this up in a later round.",
		Labels:      []string{"deferred"},
		CreatedAt:   now,
		UpdatedAt:   now,
		Vars: map[string]string{
			workflowRootTaskIDVar: root.ID,
			workflowPlanIDVar:     root.Vars[workflowPlanIDVar],
			workflowPlanWPIDVar:   inst.BranchID,
		},
	}
	if err := s.ts.AddTask(project, agentOfAssignee(root.Assignee), followUp); err != nil {
		return nil, err
	}
	return followUp, nil
}

func agentOfAssignee(assignee string) string {
	if i := strings.Index(assignee, "/"); i >= 0 {
		return assignee[i+1:]
	}
	return assignee
}

func retryReason(reason string, inst entity.WorkflowBranchInstance) string {
	if strings.TrimSpace(reason) != "" {
		return "operator retry: " + strings.TrimSpace(reason)
	}
	return "operator retry of branch " + inst.BranchID
}

func retryReasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return " — " + strings.TrimSpace(reason)
}

// failedStepOfRun finds the run's failed step: the active step holding a
// failed instance, which is the shape every branch-child failure produces.
func (s *Server) failedStepOfRun(wfStore *workflowstore.Store, run entity.WorkflowRun) (string, error) {
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return "", err
	}
	latest := -1
	for i := range instances {
		if strings.TrimSpace(instances[i].Status) == "failed" {
			if latest < 0 || instances[i].FinishedAt.After(instances[latest].FinishedAt) {
				latest = i
			}
		}
	}
	if latest < 0 {
		return "", fmt.Errorf("run %s has no failed step instance", run.ID)
	}
	return instances[latest].StepID, nil
}

// appendRootTaskComment posts the ruling to the root task's summary trail so
// integration review sees defer/retry decisions next to the branch reports.
func (s *Server) appendRootTaskComment(project, rootAgent string, root *entity.Task, note string) {
	if strings.TrimSpace(note) == "" {
		return
	}
	stamp := time.Now().UTC().Format("2006-01-02 15:04")
	root.Summary = strings.TrimSpace(strings.TrimSpace(root.Summary) + "\n[" + stamp + "] " + note)
	root.UpdatedAt = time.Now().UTC()
	_ = s.ts.PersistTask(project, rootAgent, root)
}
