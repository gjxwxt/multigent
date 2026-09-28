package api

// A1 closeout: the IM trigger / ChatOps callback validates the card's bearer
// token but, before the fix, acted on the run's CURRENT decision point — a
// card minted for an earlier review step (or an earlier round of the same
// step, re-minted after a rework) could approve or drive a different active
// step. The fix pins the callback to the run and step the card was minted
// for. These tests reproduce the stale-card shape against the live guard.

import (
	"net/http"
	"strings"
	"testing"
)

// TestTriggerCallbackRejectsStaleCardForOtherStep: a card minted for a step
// that is no longer active (here: the contract_review step while the run sits
// on qa_signoff) must be refused before any transition write.
func TestTriggerCallbackRejectsStaleCardForOtherStep(t *testing.T) {
	s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, true)

	record := c1NotificationRecord(t, s, workspaceID, "wn-a1-stale-step", "a1-callback-token", task, run.ID, run.DefinitionID)
	// Forge the stale shape: the card claims it was issued for contract_review,
	// a step the run has already left.
	record.StepID = "contract_review"
	if err := s.saveWorkflowNotification(record); err != nil {
		t.Fatal(err)
	}

	rec := postC1TriggerCallback(t, s, workspaceID, record.ID, "a1-callback-token", c1ApproveBody(t, true))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stale card for a past step must be rejected with 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stale workflow review card") {
		t.Fatalf("rejection must name the stale-card cause: %s", rec.Body.String())
	}
	runParkedOnQASignoff(t, wfStore, "resproj", task.ID)
	stored, ok, err := s.workflowNotification(workspaceID, record.ID)
	if err != nil || !ok {
		t.Fatalf("notification lookup: ok=%v err=%v", ok, err)
	}
	if stored.Status == "acted" {
		t.Fatal("a rejected stale card must not be marked acted")
	}
}

// TestTriggerCallbackRejectsStaleCardForOtherRun: a card minted for a
// previous run of the same task must not act on the new run.
func TestTriggerCallbackRejectsStaleCardForOtherRun(t *testing.T) {
	s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, true)

	record := c1NotificationRecord(t, s, workspaceID, "wn-a1-stale-run", "a1-callback-token-2", task, "wfr-old-run", run.DefinitionID)
	rec := postC1TriggerCallback(t, s, workspaceID, record.ID, "a1-callback-token-2", c1ApproveBody(t, true))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stale card from another run must be rejected with 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stale workflow review card") {
		t.Fatalf("rejection must name the stale-card cause: %s", rec.Body.String())
	}
	runParkedOnQASignoff(t, wfStore, "resproj", task.ID)
}

// TestTriggerCallbackAcceptsCurrentCard: the guard must not break the live
// path — a card whose run ID and step ID both match the active run/step still
// advances (here it reaches the C1 matrix gate and is rejected for the matrix
// for a matrix-less body, proving the callback got PAST the stale-card guard
// into the existing gate logic).
func TestTriggerCallbackAcceptsCurrentCard(t *testing.T) {
	s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, false)

	record := c1NotificationRecord(t, s, workspaceID, "wn-a1-current", "a1-callback-token-3", task, run.ID, run.DefinitionID)
	rec := postC1TriggerCallback(t, s, workspaceID, record.ID, "a1-callback-token-3", c1ApproveBody(t, false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("current card must reach the matrix gate, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "risk_coverage_matrix") {
		t.Fatalf("a current card must fail on the matrix gate, not the card guard: %s", rec.Body.String())
	}
	runParkedOnQASignoff(t, wfStore, "resproj", task.ID)
}
