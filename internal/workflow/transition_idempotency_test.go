package workflow

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func dbOpenForTransitionTests(t *testing.T) (db.Store, error) {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-term", Name: "WS", Slug: "workspace-term", Root: t.TempDir()}); err != nil {
		return nil, err
	}
	return controlDB, nil
}

// Q2 (GPT review): CompleteAndAdvance is a multi-write transition over
// independent kv_records rows with no enclosing DB transaction. A duplicate
// completion of the same step (double webhook, ChatOps click racing the
// runtime finish, retry after timeout) must NOT persist a second completion
// event / next-step reset / review_rounds increment. The claim gate makes the
// transition exclusive; the exact invariants GPT demanded are asserted here:
// one winner, every duplicate stale, one completion event, one active next
// step, review_rounds incremented exactly once.
//
// Deterministic overlap: a fresh claim marker (racer A mid-transition) is
// planted BEFORE racer B runs — that is exactly the interleaving a double
// dispatch produces, without depending on goroutine timing.
func TestCompleteAndAdvanceConcurrentDuplicateIsIdempotent(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)

	runBefore, ok, err := store.RunForTask("project", "task-self-review")
	if err != nil || !ok {
		t.Fatalf("load run: %v", err)
	}
	stepBefore := runBefore.ActiveStepID
	eventsBefore := countStepEvents(t, store, runBefore.ID, stepBefore)

	// Racer A is mid-transition (claim held, nothing persisted yet).
	if err := store.writeTransitionClaimMarker(&runBefore); err != nil {
		t.Fatalf("plant claim: %v", err)
	}

	// Racer B attempts the same step completion while A holds the claim —
	// must go stale with ZERO writes (no event, no instance change, no round
	// increment).
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed by racer B", "", map[string]string{
		"self_review":         "race report from racer B",
		"self_review_verdict": "pass",
		"review_rounds":       "1",
	}, "completed"); !errors.Is(err, ErrStaleWorkflowTransition) {
		t.Fatalf("duplicate completion must return ErrStaleWorkflowTransition, got %v", err)
	}
	if got := countStepEvents(t, store, runBefore.ID, stepBefore); got != eventsBefore {
		t.Fatalf("stale completion wrote events: %d → %d, want no change", eventsBefore, got)
	}

	// Racer A finishes: in production its CompleteAndAdvance call holds the
	// claim from its own gate pass; the test mirrors the release-then-finish
	// half deterministically by letting A's transition proceed through a
	// normal completion (the gate is re-enterable once the claim is gone).
	store.releaseWorkflowTransitionClaim(&runBefore)
	transition, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed by racer A", "", map[string]string{
		"self_review":         "race report from racer A",
		"self_review_verdict": "pass",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("winning completion must succeed, got %v", err)
	}
	if transition.Next == nil || transition.Next.ID != "ci_ready_gate" {
		t.Fatalf("winner routed to %v, want ci_ready_gate", transition.Next)
	}

	run, ok, err := store.RunForTask("project", "task-self-review")
	if err != nil || !ok {
		t.Fatalf("reload run: %v", err)
	}
	if run.Status != "active" || run.ActiveStepID != "ci_ready_gate" {
		t.Fatalf("run must have advanced exactly once: status=%s activeStep=%s", run.Status, run.ActiveStepID)
	}

	// Invariant 1: exactly ONE completion event for the completed step
	// (racer B's stale attempt added none).
	if got := countStepEvents(t, store, run.ID, stepBefore); got != eventsBefore+1 {
		t.Fatalf("completion events for step %s: %d, want exactly %d+1", stepBefore, got, eventsBefore)
	}

	// Invariant 2: the next step exists ONCE as pending (no duplicate reset).
	instances, err := store.ListStepInstances(run.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	pendingNext := 0
	for _, inst := range instances {
		if inst.StepID == run.ActiveStepID && inst.Status == "pending" {
			pendingNext++
		}
	}
	if pendingNext != 1 {
		t.Fatalf("pending next-step instances: %d, want 1 (duplicate reset would add more)", pendingNext)
	}
	// Invariant 3 (review_rounds increments exactly once — GPT: 「round 只增加
	// 一次」): the platform increment rides ONLY the rework edge (pass edge
	// carries no review_rounds InputMapping by design), so exercise it: after
	// the winner's pass to ci_ready_gate, complete ci_ready_gate with a reject
	// → run bounces to code_review... — instead drive the rework route
	// directly: reload from a fresh store where the verdict is issues_fixed,
	// then assert the reworked implement instance carries rounds+1 (1 → 2),
	// exactly one increment, never two.
	//
	// The winner above took the pass route, so verify the increment on a
	// separate deterministic rework transition here.
	reworkStore := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, reworkStore)
	reworkRun, ok, err := reworkStore.RunForTask("project", "task-self-review")
	if err != nil || !ok {
		t.Fatalf("load rework run: %v", err)
	}
	if err := reworkStore.writeTransitionClaimMarker(&reworkRun); err != nil {
		t.Fatalf("plant rework claim: %v", err)
	}
	if _, err := reworkStore.CompleteAndAdvance("project", "task-self-review", "reviewed by racer B", "", map[string]string{
		"self_review":         "race report from racer B",
		"self_review_verdict": "issues_fixed",
		"review_rounds":       "1",
	}, "completed"); !errors.Is(err, ErrStaleWorkflowTransition) {
		t.Fatalf("duplicate rework completion must go stale, got %v", err)
	}
	reworkStore.releaseWorkflowTransitionClaim(&reworkRun)
	reworkTransition, err := reworkStore.CompleteAndAdvance("project", "task-self-review", "reviewed by racer A", "", map[string]string{
		"self_review":         "race report from racer A",
		"self_review_verdict": "issues_fixed",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("winning rework completion must succeed, got %v", err)
	}
	if reworkTransition.Next == nil || reworkTransition.Next.ID != "implement" {
		t.Fatalf("rework winner routed to %v, want implement", reworkTransition.Next)
	}
	if reworkTransition.NextInst == nil {
		t.Fatal("rework must produce a next implement instance")
	}
	if gotRounds := reworkTransition.NextInst.InputValues["review_rounds"]; gotRounds != "2" {
		t.Fatalf("reworked implement review_rounds = %q, want \"2\" (model said 1, exactly one platform increment)", gotRounds)
	}
}

// A terminal run refuses re-entry outright: double-completing an already
// completed workflow is always a duplicate dispatch, never a legitimate
// retry. Uses a minimal two-step definition so the first completion lands
// the run in "completed" deterministically.
func TestCompleteAndAdvanceTerminalRunRefusesReentry(t *testing.T) {
	store := startMinimalTwoStepRun(t)
	if _, err := store.CompleteAndAdvance("project", "task-term", "step a done", "", map[string]string{
		"decision": "approve",
		"comments": "ok",
	}, "completed"); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	run, ok, err := store.RunForTask("project", "task-term")
	if err != nil || !ok {
		t.Fatalf("reload run: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("run status after terminal completion = %s, want completed", run.Status)
	}
	_, err = store.CompleteAndAdvance("project", "task-term", "step a done again", "", map[string]string{
		"decision": "approve",
		"comments": "ok",
	}, "completed")
	if !errors.Is(err, ErrStaleWorkflowTransition) {
		t.Fatalf("second completion of a completed run must return ErrStaleWorkflowTransition, got %v", err)
	}
}

// A failed route-mismatch abort releases the claim: the fail-closed retry
// contract (route validated BEFORE persistence, step stays re-runnable) must
// survive the new gate — a corrected re-run claims again immediately, not
// after a TTL.
func TestCompleteAndAdvanceRouteMismatchReleasesClaim(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed", "", map[string]string{
		"self_review":   "report without a verdict token",
		"review_rounds": "1",
	}, "completed"); err == nil {
		t.Fatal("missing verdict must fail the route partition")
	}
	// Immediate corrected re-run: must NOT hit ErrStaleWorkflowTransition.
	transition, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed with verdict", "", map[string]string{
		"self_review":         "report",
		"self_review_verdict": "pass",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("corrected re-run after route-mismatch abort must succeed immediately, got %v", err)
	}
	if transition.Next == nil || transition.Next.ID != "ci_ready_gate" {
		t.Fatalf("re-run routed to %v, want ci_ready_gate", transition.Next)
	}
}

// The claim TTL steals a marker left by a crashed transitioner, so a
// mid-transition crash cannot wedge the workflow forever.
func TestCompleteAndAdvanceStealsExpiredClaim(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)
	run, ok, err := store.RunForTask("project", "task-self-review")
	if err != nil || !ok {
		t.Fatalf("load run: %v", err)
	}

	// Simulate a crashed transitioner: claim marker with an ancient stamp.
	claimed := run
	claimed.UpdatedAt = time.Now().UTC().Add(-transitionClaimTTL - time.Minute)
	if err := store.writeTransitionClaimMarker(&claimed); err != nil {
		t.Fatalf("write claim marker: %v", err)
	}

	transition, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed after crash", "", map[string]string{
		"self_review":         "report",
		"self_review_verdict": "pass",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("expired claim must be stolen, got %v", err)
	}
	if transition.Next == nil || transition.Next.ID != "ci_ready_gate" {
		t.Fatalf("post-steal transition routed to %v", transition.Next)
	}
}

// startMinimalTwoStepRun seeds a tiny two-step definition: gate (human,
// decision+comments outputs) → terminal. start has no incoming edges; the
// single default edge from gate ends the run on completion.
func startMinimalTwoStepRun(t *testing.T) *Store {
	t.Helper()
	controlDB, err := dbOpenForTransitionTests(t)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	store := NewStore(controlDB, "workspace-term")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID: "wf-term-test", Name: "Terminal Test", Version: 1, Scope: "workspace",
		StartStepID: "gate",
		Steps: []entity.WorkflowStep{
			{ID: "gate", Type: "human_review", Title: "Gate", Position: entity.WorkflowPosition{X: 0, Y: 0},
				OutputFields: []entity.WorkflowField{{Name: "decision"}, {Name: "comments"}}},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "e-gate-approve", From: "gate", To: "gate_done", IsDefault: false,
				Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "approve"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	// A single conditional edge with no match fails closed; give the run a
	// default edge to NOWHERE? No — simplest terminal shape: no outgoing
	// edges at all. Completing the step completes the run.
	def.Edges = nil
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := store.StartRun("project", "task-term", def.ID, map[string]entity.WorkflowActorBinding{
		"gate": {Type: "human", ID: "owner"},
	}); err != nil {
		t.Fatalf("start run: %v", err)
	}
	return store
}

func countStepEvents(t *testing.T, store *Store, runID, stepID string) int {
	t.Helper()
	events, err := store.ListStepEvents(runID)
	if err != nil {
		t.Fatalf("list step events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.StepID == stepID {
			n++
		}
	}
	return n
}

// writeTransitionClaimMarker plants a claim marker directly (test-only seam).
func (s *Store) writeTransitionClaimMarker(run *entity.WorkflowRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return s.db.UpsertRecord("workflow_runs", s.workspaceID, []string{run.Project, run.TaskID, run.ID}, transitionClaimPrefix+string(raw))
}
