package api

// F1 closeout: what the contract review approves must be what the freeze
// freezes. The e-contract-batch-review edge forwards scale_gate's plan to the
// review via contract_batch's $input side, while the freeze's recency chain
// consumed step OUTPUTS first — a reworked contract_batch that emits its own
// refined delivery_plan output would be frozen although the human approved
// the older snapshot. The fix prepends the freezing review step's own input
// values so freeze == approval.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// refinedPlanJSON is a contract_batch-refined plan: same shape, different
// planId and work packages than the scale_gate draft.
func refinedPlanJSON() string {
	return `{
	  "planId": "plan-refined",
	  "workPackages": [
	    {"id":"wp-refined-1","title":"Refined package","domain":"server",
	     "acceptanceCriteria":["uc-2"],"agentBinding":"pm"}
	  ],
	  "sharedContract": [{"id":"api-skeleton","artifact":"audit export endpoint","path":"server/audit"}]
	}`
}

func approvedPlanIDFromFrozen(t *testing.T, s *Server, workspaceID, project, taskID string) string {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	record, version, opened, err := wfStore.LoadFrozenPlanForRun(project, runForTask(t, s, workspaceID, taskID).ID)
	if err != nil || !opened {
		t.Fatalf("frozen plan lookup: opened=%v err=%v", opened, err)
	}
	_ = record
	return version.Plan.PlanID
}

// TestFreezeConsumesApprovedPlanNotRefinedOutput: scale_gate emits plan A,
// the reworked contract_batch emits refined plan B as its OUTPUT. Before the
// F1 fix the review edge forwarded the $input side, so the reviewer saw A
// while the freeze consumed the newer B — the approval and the frozen plan
// diverged. The fix follows the S2 P1-1 precedent (output-side forwarding for
// every freeze-consumed field): the reviewer now SEES the refined B and the
// freeze records the SAME B. Display == freeze.
func TestFreezeConsumesApprovedPlanNotRefinedOutput(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-f1-approved", baseCommit)

	// Walk to contract_review with scale_gate's plan A. The reviewer requests
	// changes; the reworked contract_batch re-completes with its OWN refined
	// plan B as OUTPUT. The review input still carries A ($input passthrough
	// on e-contract-batch-review).
	driveToContractReview(t, s, workspaceID, "task-f1-approved", samplePlanJSON())
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-f1-approved", workflowReviewBody{
		Decision: "request_changes", Comments: "refine the plan",
	}); err != nil {
		t.Fatalf("contract_review request_changes failed (%d): %v", status, err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, "task-f1-approved", map[string]string{
		"contract_artifacts": "schema + error codes committed (rework)",
		"delivery_plan":      refinedPlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reworked contract_batch completion with refined output must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The reviewer is shown the edge-carried input: plan A.
	run := runForTask(t, s, workspaceID, "task-f1-approved")
	instances, err := workflowstore.NewStore(s.controlDB, workspaceID).ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reviewInput string
	for _, inst := range instances {
		if inst.StepID == "contract_review" {
			reviewInput = inst.InputValues["delivery_plan"]
		}
	}
	if !strings.Contains(reviewInput, "plan-refined") {
		t.Fatalf("reviewer must be shown the reworked plan B (the freeze will consume it), got: %s", reviewInput)
	}
	if strings.Contains(reviewInput, "plan-slice4") {
		t.Fatalf("the stale scale_gate draft must not shadow the reworked output, got: %s", reviewInput)
	}

	if status, err := approveContractReview(t, s, workspaceID, "task-f1-approved"); err != nil {
		t.Fatalf("approve failed (%d): %v", status, err)
	}

	// Display == freeze: the reviewer approved the refined plan B they were
	// shown, and exactly B is frozen — never the unseen older draft.
	if got := approvedPlanIDFromFrozen(t, s, workspaceID, "sample", "task-f1-approved"); got != "plan-refined" {
		t.Fatalf("freeze must record the shown-and-approved plan B (plan-refined), got %q", got)
	}
}

// driveToContractReviewNoPlan walks to contract_review with contract_batch
// deliberately emitting NO plan output (the blind-approval residue shape).
func driveToContractReviewNoPlan(t *testing.T, s *Server, workspaceID, taskID string) {
	t.Helper()
	rec := postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"requirement_draft": "The console must clear local session state on expiry and export audit logs.",
		"open_questions":    "none",
		"requirement_items": requirementItemsJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("requirement_draft completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "requirement approved",
	}); err != nil {
		t.Fatalf("requirement_review approve failed (%d): %v", status, err)
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "design approved with waiver",
		Outputs: map[string]string{"design_waiver_reason": "acceptance run: UI prototype not required"},
	}); err != nil {
		t.Fatalf("design_review approve failed (%d): %v", status, err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"scale_verdict": "batched",
		"delivery_plan": samplePlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("scale_gate completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"contract_artifacts": "schema + error codes committed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("contract_batch completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestFreezeRefusesApprovalWhenNoPlanReachesReview: the normal first pass —
// scale_gate emits plan A, contract_batch emits NO plan output (the agent
// ignored the re-emit mandate). The review edge forwards the $output side, so
// the review input's plan fields arrive EMPTY while the freeze would fall
// back to the recency chain and freeze a plan the human was never shown.
// Fail-closed (F1 follow-up): the approve is REFUSED — the producing step
// must re-run and deliver the plan to the review before it can be frozen.
func TestFreezeRefusesApprovalWhenNoPlanReachesReview(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-f1-display", baseCommit)
	// Walk the chain but complete contract_batch WITHOUT a plan output (the
	// residue shape: the agent ignored the re-emit mandate).
	driveToContractReviewNoPlan(t, s, workspaceID, "task-f1-display")

	run := runForTask(t, s, workspaceID, "task-f1-display")
	instances, err := workflowstore.NewStore(s.controlDB, workspaceID).ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var shownPlan string
	for _, inst := range instances {
		if inst.StepID == "contract_review" {
			shownPlan = inst.InputValues["delivery_plan"]
		}
	}
	if shownPlan != "" {
		t.Fatalf("precondition: no plan must reach the review in this shape, got %q", shownPlan)
	}

	// Approving without a plan visible must refuse BEFORE any write.
	if status, err := approveContractReview(t, s, workspaceID, "task-f1-display"); err == nil {
		t.Fatal("an approval with no plan at the review must be refused (blind approval of an unseen plan)")
	} else if status != http.StatusBadRequest {
		t.Fatalf("refusal must be 400, got %d: %v", status, err)
	} else if !strings.Contains(err.Error(), "no delivery plan reached the review step") {
		t.Fatalf("refusal must name the missing plan, got: %v", err)
	}
	after := runForTask(t, s, workspaceID, "task-f1-display")
	if after.ActiveStepID != "contract_review" {
		t.Fatalf("a refused approval must leave the run parked at contract_review, got %s@%s", after.Status, after.ActiveStepID)
	}
	if _, _, ok, err := workflowstore.NewStore(s.controlDB, workspaceID).LoadFrozenPlanForRun("sample", run.ID); err != nil || ok {
		t.Fatalf("a refused blind approval must leave no frozen record (ok=%v err=%v)", ok, err)
	}

	// The rework loop repairs it: contract_batch re-emits the plan output →
	// the reviewer now SEES the plan → the same plan is frozen.
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-f1-display", workflowReviewBody{
		Decision: "request_changes", Comments: "plan never reached the review; re-emit it",
	}); err != nil {
		t.Fatalf("request_changes failed (%d): %v", status, err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, "task-f1-display", map[string]string{
		"contract_artifacts": "schema + error codes committed (rework)",
		"delivery_plan":      refinedPlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rework completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if status, err := approveContractReview(t, s, workspaceID, "task-f1-display"); err != nil {
		t.Fatalf("approve after plan delivery failed (%d): %v", status, err)
	}
	if got := approvedPlanIDFromFrozen(t, s, workspaceID, "sample", "task-f1-display"); got != "plan-refined" {
		t.Fatalf("the frozen plan must be the one the reviewer SAW (plan-refined), got %q", got)
	}
}

// TestFreezeStillConsumesRecencyChainWithoutEdgePlan: when the review input
// carries no plan (a shape without the edge mapping), the freeze falls back
// to the existing recency-first output chain — behavior preserved.
func TestFreezeStillConsumesRecencyChainWithoutEdgePlan(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-f1-legacy", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-f1-legacy", samplePlanJSON())
	// Normal contract_batch completion (no plan output): the review input
	// carries scale_gate's plan via $input passthrough — the fallback path is
	// the same recency chain.
	if status, err := approveContractReview(t, s, workspaceID, "task-f1-legacy"); err != nil {
		t.Fatalf("approve failed (%d): %v", status, err)
	}
	if got := approvedPlanIDFromFrozen(t, s, workspaceID, "sample", "task-f1-legacy"); got != "plan-slice4" {
		t.Fatalf("legacy shape must still freeze scale_gate's plan, got %q", got)
	}
}
