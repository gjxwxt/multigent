package api

// A1 round-level residue (blind-review follow-up): the guard pins the card to
// (run, step), but a request_changes rework loop RE-ENTERS the same step —
// the engine resets the SAME step instance in place (store: Status=pending,
// same instance ID, fresh InputValues). A round-1 card therefore still
// matches run+step in round N+1 and its approval drives the round-2 decision
// point: the reviewer approves the reworked round WITHOUT the reworked input
// ever reaching them through that card.
//
// This test reproduces the real HTTP/state-machine scenario end to end: mint
// a card at round 1, drive a console request_changes + contract_batch rework
// back into contract_review, then submit the OLD card's approve through the
// trigger callback.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func wfStoreInstancesForRun(t *testing.T, s *Server, workspaceID, runID string) ([]entity.WorkflowStepInstance, error) {
	t.Helper()
	return workflowstore.NewStore(s.controlDB, workspaceID).ListStepInstances(runID)
}

// TestTriggerCallbackStaleRoundCardDrivesNewRound is a COUNTEREXAMPLE probe:
// it asserts the stale-round shape is REFUSED. If the guard below lands, this
// test passes; on the pre-fix code it fails with the run advanced (proving
// the old card drove the new round).
func TestTriggerCallbackStaleRoundCardDrivesNewRound(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-a1-round", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-a1-round", samplePlanJSON())
	run := runForTask(t, s, workspaceID, "task-a1-round")

	// Round 1: a card is minted for (run, contract_review). Mirrors the
	// production mint shape with this fixture's project ("sample").
	oldCard := workflowNotificationRecord{
		ID:                "wn-a1-round1",
		WorkspaceID:       workspaceID,
		Project:           "sample",
		TaskID:            "task-a1-round",
		TaskTitle:         "GF delivery task-a1-round",
		WorkflowRunID:     run.ID,
		WorkflowID:        run.DefinitionID,
		StepID:            "contract_review",
		StepTitle:         "Shared Contract Review",
		RecipientUserID:   "admin",
		Provider:          "feishu",
		Status:            "sent",
		CallbackTokenHash: hashWorkflowCallbackToken("a1-round-token"),
		// Production mint (createWorkflowNotificationRecord) pins the run's
		// decision-point generation; a rework round-trip moves run.UpdatedAt.
		WorkflowRunUpdatedAt: run.UpdatedAt,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	if err := s.saveWorkflowNotification(oldCard); err != nil {
		t.Fatal(err)
	}

	// Rework: the console reviewer requests changes; contract_batch reworks;
	// the run re-enters contract_review (round 2, same instance ID reset).
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-a1-round", workflowReviewBody{
		Decision: "request_changes", Comments: "refine the plan",
	}); err != nil {
		t.Fatalf("request_changes failed (%d): %v", status, err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, "task-a1-round", map[string]string{
		"contract_artifacts": "schema + error codes committed (rework)",
		"delivery_plan":      refinedPlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rework completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	afterRework := runForTask(t, s, workspaceID, "task-a1-round")
	if afterRework.ActiveStepID != "contract_review" || afterRework.Status != "active" {
		t.Fatalf("run must be re-parked on contract_review (round 2), got %s@%s", afterRework.Status, afterRework.ActiveStepID)
	}

	// The OLD round-1 card's approve arrives through the IM callback.
	_, err := s.submitWorkflowReviewFromTrigger(workspaceID, oldCard, workflowTriggerCallbackBody{
		Decision: "approve", Comments: "round-1 stale approve", Outputs: map[string]string{},
	}, httptest.NewRequest(http.MethodPost, "/", nil))
	if err == nil {
		t.Fatal("the round-1 card must NOT drive the round-2 decision point (stale-round card accepted)")
	}
	if !strings.Contains(err.Error(), "stale workflow review card") {
		t.Fatalf("rejection must name the stale-card cause, got: %v", err)
	}
	after := runForTask(t, s, workspaceID, "task-a1-round")
	if after.ActiveStepID != "contract_review" || after.Status != "active" {
		t.Fatalf("a rejected stale-round card must leave the run parked on contract_review, got %s@%s", after.Status, after.ActiveStepID)
	}
	// Zero write on the rejected approval: the review instance's outputs stay
	// empty (no decision recorded).
	instances, err := wfStoreInstancesForRun(t, s, workspaceID, after.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.StepID != "contract_review" {
			continue
		}
		if strings.TrimSpace(inst.OutputValues["decision"]) != "" {
			t.Fatalf("a rejected stale-round card must not record a decision: %+v", inst.OutputValues)
		}
	}
}

// TestTriggerCallbackCurrentRoundCardStillActs: a card minted at the CURRENT
// decision round (fresh generation pin, like the production mint) still
// reaches the freeze/gates — the round guard must not break the live path.
func TestTriggerCallbackCurrentRoundCardStillActs(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-a1-round-ok", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-a1-round-ok", "not-json")
	run := runForTask(t, s, workspaceID, "task-a1-round-ok")

	currentCard := workflowNotificationRecord{
		ID:                   "wn-a1-round-ok",
		WorkspaceID:          workspaceID,
		Project:              "sample",
		TaskID:               "task-a1-round-ok",
		TaskTitle:            "GF delivery task-a1-round-ok",
		WorkflowRunID:        run.ID,
		WorkflowID:           run.DefinitionID,
		StepID:               "contract_review",
		StepTitle:            "Shared Contract Review",
		RecipientUserID:      "admin",
		Provider:             "feishu",
		Status:               "sent",
		CallbackTokenHash:    hashWorkflowCallbackToken("a1-round-ok-token"),
		WorkflowRunUpdatedAt: run.UpdatedAt,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	if err := s.saveWorkflowNotification(currentCard); err != nil {
		t.Fatal(err)
	}

	// A malformed plan must refuse the approval INSIDE the freeze gate —
	// proving the callback got past the card guards (run, step, generation).
	_, err := s.submitWorkflowReviewFromTrigger(workspaceID, currentCard, workflowTriggerCallbackBody{
		Decision: "approve", Outputs: map[string]string{},
	}, httptest.NewRequest(http.MethodPost, "/", nil))
	if err == nil {
		t.Fatal("the malformed plan must refuse (the card itself is valid)")
	}
	if !strings.Contains(err.Error(), "delivery_plan_freeze_rejected") {
		t.Fatalf("a current-round card must fail on the freeze gate, not the card guard: %v", err)
	}
	after := runForTask(t, s, workspaceID, "task-a1-round-ok")
	if after.ActiveStepID != "contract_review" {
		t.Fatalf("a refused approval must leave the run parked, got %s@%s", after.Status, after.ActiveStepID)
	}
}

// A1 migration residue (re-verification round): production mint always pins
// WorkflowRunUpdatedAt, but records persisted by OLDER builds carry no such
// field — they unmarshal with a ZERO pin and, under the legacy exemption,
// skip the generation check entirely. Callback tokens have no expiry, so a
// pre-upgrade card is still a live bearer credential: after a rework
// round-trip back to the same step it would drive the NEW round.
//
// This counterexample walks the REAL HTTP callback entry (not the internal
// helper) with an unpinned card, reworks the run back into the same step,
// and asserts the legacy card is refused with an actionable re-fetch path.
func TestTriggerCallbackLegacyUnpinnedCardRefusedAfterRework(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-a1-legacy", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-a1-legacy", samplePlanJSON())
	run := runForTask(t, s, workspaceID, "task-a1-legacy")

	// A pre-upgrade card: (run, step) pinned, generation pin ABSENT (zero).
	legacyCard := workflowNotificationRecord{
		ID:                "wn-a1-legacy-unpinned",
		WorkspaceID:       workspaceID,
		Project:           "sample",
		TaskID:            "task-a1-legacy",
		TaskTitle:         "GF delivery task-a1-legacy",
		WorkflowRunID:     run.ID,
		WorkflowID:        run.DefinitionID,
		StepID:            "contract_review",
		StepTitle:         "Shared Contract Review",
		RecipientUserID:   "admin",
		Provider:          "feishu",
		Status:            "sent",
		CallbackTokenHash: hashWorkflowCallbackToken("a1-legacy-token"),
		// WorkflowRunUpdatedAt deliberately ZERO: the pre-upgrade shape.
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.saveWorkflowNotification(legacyCard); err != nil {
		t.Fatal(err)
	}

	// Rework round-trip: request_changes, contract_batch re-emits (F1
	// fail-closed shape), the run re-enters contract_review at round 2.
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-a1-legacy", workflowReviewBody{
		Decision: "request_changes", Comments: "refine the plan",
	}); err != nil {
		t.Fatalf("request_changes failed (%d): %v", status, err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, "task-a1-legacy", map[string]string{
		"contract_artifacts": "schema + error codes committed (rework)",
		"delivery_plan":      refinedPlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rework completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	afterRework := runForTask(t, s, workspaceID, "task-a1-legacy")
	if afterRework.ActiveStepID != "contract_review" || afterRework.Status != "active" {
		t.Fatalf("run must be re-parked on contract_review (round 2), got %s@%s", afterRework.Status, afterRework.ActiveStepID)
	}

	// The unpinned legacy card's approve arrives through the REAL HTTP
	// callback entry. It must be refused with an actionable message.
	cb := postC1TriggerCallback(t, s, workspaceID, legacyCard.ID, "a1-legacy-token",
		`{"decision":"approve","comments":"legacy round-1 approve"}`)
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("an unpinned legacy card must be refused after a rework round-trip, got %d: %s", cb.Code, cb.Body.String())
	}
	if !strings.Contains(cb.Body.String(), "stale workflow review card") {
		t.Fatalf("refusal must name the stale-card cause, got: %s", cb.Body.String())
	}
	if !strings.Contains(cb.Body.String(), "open the task in the console") {
		t.Fatalf("refusal must give an actionable re-fetch path, got: %s", cb.Body.String())
	}
	// The rejection leaves zero writes: the run stays parked on the current
	// decision point and the current round's instance carries no decision.
	after := runForTask(t, s, workspaceID, "task-a1-legacy")
	if after.ActiveStepID != "contract_review" || after.Status != "active" {
		t.Fatalf("a refused legacy card must leave the run parked on contract_review, got %s@%s", after.Status, after.ActiveStepID)
	}
	instances, err := wfStoreInstancesForRun(t, s, workspaceID, after.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.StepID != "contract_review" {
			continue
		}
		if strings.TrimSpace(inst.OutputValues["decision"]) != "" {
			t.Fatalf("a refused legacy card must not record a decision: %+v", inst.OutputValues)
		}
	}
}
