package workflow

// Regression tests for the od-e2e P0 routing incident: the agent_self_review
// step's outgoing edges must route on the dedicated self_review_verdict field
// with mutually exclusive eq conditions. The original unified template
// declared a neq-escalate pass edge BEFORE the rework edge, and
// first-match-wins routing let issues_fixed satisfy the pass route — a rework
// verdict sailed through to the CI gate.
//
// These tests exercise the real Store.CompleteAndAdvance transition (not the
// raw chooseNextEdge helper) because the route also runs through structured
// output normalization: a verdict not declared on the step's output fields
// would be rejected before routing ever sees it.

import (
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func startUnifiedSelfReviewRun(t *testing.T) *Store {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	store := NewStore(controlDB, "workspace-1")
	def, ok := DefinitionFromTemplate("unified-delivery-pipeline", "en", "self-review-routing-test")
	if !ok {
		t.Fatal("unified-delivery-pipeline template not registered")
	}
	if err := store.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	// Key bindings by step ID (workflowActorBindingForStep fallback order).
	bindings := map[string]entity.WorkflowActorBinding{}
	for _, step := range def.Steps {
		if step.Type == "human_review" {
			bindings[step.ID] = entity.WorkflowActorBinding{Type: "human", ID: "owner"}
			continue
		}
		bindings[step.ID] = entity.WorkflowActorBinding{Type: "agent", ID: "agent-" + step.ID}
	}
	if _, _, err := store.StartRun("project", "task-self-review", def.ID, bindings); err != nil {
		t.Fatalf("start run: %v", err)
	}
	return store
}

// driveToSelfReview walks the run through clarify → clarify_review approve →
// implement using CompleteAndAdvance itself, so the default edges on the way
// are covered too.
func driveToSelfReview(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "clarified", "", map[string]string{
		"clarified": "scope: fix bug; acceptance: bug gone; non-goals: none",
	}, "completed"); err != nil {
		t.Fatalf("complete clarify: %v", err)
	}
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "approved", "", map[string]string{
		"decision":       "approve",
		"comments":       "lgtm",
		"approved_scope": "scope: fix bug; acceptance: bug gone; non-goals: none",
	}, "completed"); err != nil {
		t.Fatalf("approve clarify_review: %v", err)
	}
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "implemented", "", map[string]string{
		"implementation": "branch task/x, commit abc123, tests green",
		"review_rounds":  "1",
	}, "completed"); err != nil {
		t.Fatalf("complete implement: %v", err)
	}
}

func TestUnifiedSelfReviewRoutingRealTransitions(t *testing.T) {
	cases := []struct {
		name       string
		outputVals map[string]string
		wantNext   string
	}{
		{
			name:       "pass routes to ci_ready_gate",
			outputVals: map[string]string{"self_review": "all criteria met, evidence attached", "self_review_verdict": "pass", "review_rounds": "1"},
			wantNext:   "ci_ready_gate",
		},
		{
			// THE incident: a rework verdict must bounce back to implement,
			// never ride the pass route.
			name:       "issues_fixed routes back to implement",
			outputVals: map[string]string{"self_review": "violates contract at file.go:42; minimal fix attached", "self_review_verdict": "issues_fixed", "review_rounds": "2"},
			wantNext:   "implement",
		},
		{
			name:       "escalate routes to code_review",
			outputVals: map[string]string{"self_review": "round cap reached with outstanding P0", "self_review_verdict": "escalate", "escalation_case": "issue 1: contract x, file.go:42", "review_rounds": "3"},
			wantNext:   "code_review",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := startUnifiedSelfReviewRun(t)
			driveToSelfReview(t, store)
			transition, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed", "", tc.outputVals, "completed")
			if err != nil {
				t.Fatalf("complete agent_self_review: %v", err)
			}
			if transition.Next == nil {
				t.Fatalf("expected next step %q, run done instead (status=%q)", tc.wantNext, transition.Run.Status)
			}
			if transition.Next.ID != tc.wantNext {
				t.Fatalf("verdict routing broken: got next step %q, want %q (outputs=%v)", transition.Next.ID, tc.wantNext, tc.outputVals)
			}
		})
	}
}

// A missing OR non-vocabulary verdict must not ride a wrong route. There is
// no default edge on agent_self_review: no matching condition means the run
// fails closed (route-mismatch error), keeping the step active so the
// reviewer can be re-prompted. The missing case is caught by the required
// field check before routing; a non-empty invalid token like "passs" passes
// output validation (the field only requires non-empty) and must then be
// caught by the route partition — review round 2 of the od-e2e incident.
func TestUnifiedSelfReviewMissingVerdictFailsClosed(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)
	// Report present, verdict absent — the od-e2e shape.
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed", "", map[string]string{
		"self_review":   "full report without a routing token",
		"review_rounds": "1",
	}, "completed"); err == nil {
		t.Fatal("missing self_review_verdict must fail closed, got a successful transition")
	}
}

func TestUnifiedSelfReviewUnknownVerdictFailsClosed(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)
	// Non-empty invalid token: survives required-field validation, must be
	// stopped by the eq-partition (no default edge) — never reach the CI gate.
	transition, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed", "", map[string]string{
		"self_review":         "full report",
		"self_review_verdict": "passs",
		"review_rounds":       "1",
	}, "completed")
	if err == nil {
		t.Fatalf("unknown verdict %q must fail closed, got transition to %v", "passs", transition.Next)
	}
	// Fail-closed means the step stays re-runnable: routing is validated
	// BEFORE persistence, so the instance must still be pending with no
	// output recorded and no completion event written.
	run, ok, err := store.RunForTask("project", "task-self-review")
	if err != nil || !ok {
		t.Fatalf("run must stay addressable: ok=%v err=%v", ok, err)
	}
	if run.Status != "active" || run.ActiveStepID != "agent_self_review" {
		t.Fatalf("run must stay on agent_self_review, got status=%q active=%q", run.Status, run.ActiveStepID)
	}
	instances, err := store.ListStepInstances(run.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, inst := range instances {
		if inst.StepID != "agent_self_review" {
			continue
		}
		if inst.Status != "pending" {
			t.Fatalf("instance must stay pending after a rejected verdict, got %q", inst.Status)
		}
		if inst.OutputArtifact != "" || len(inst.OutputValues) > 0 {
			t.Fatalf("rejected completion must not persist outputs: artifact=%q values=%v", inst.OutputArtifact, inst.OutputValues)
		}
	}
	events, err := store.ListStepEvents(run.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.StepID == "agent_self_review" {
			t.Fatalf("rejected completion must not write a step event, got status %q", ev.Status)
		}
	}
	// The re-run contract: the same call with a legal verdict must now drive
	// the pipeline forward to the CI gate.
	retry, err := store.CompleteAndAdvance("project", "task-self-review", "reviewed", "", map[string]string{
		"self_review":         "full report",
		"self_review_verdict": "pass",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("legal re-run after rejected verdict: %v", err)
	}
	if retry.Next == nil || retry.Next.ID != "ci_ready_gate" {
		t.Fatalf("legal re-run must advance to ci_ready_gate, got %v", retry.Next)
	}
}

// An escalate verdict promises a structured case file for the human gate;
// an empty escalation_case must be rejected so code_review never receives
// an empty folder.
func TestUnifiedSelfReviewEscalateRequiresCaseFile(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)
	if _, err := store.CompleteAndAdvance("project", "task-self-review", "escalating", "", map[string]string{
		"self_review":         "round cap reached with outstanding P0",
		"self_review_verdict": "escalate",
		"review_rounds":       "3",
		// escalation_case deliberately omitted
	}, "completed"); err == nil {
		t.Fatal("escalate without escalation_case must be rejected")
	}
}

// The rework edge must carry the round counter and the report back into
// implement so the round cap actually accumulates.
func TestUnifiedSelfReviewReworkCarriesRoundsAndReport(t *testing.T) {
	store := startUnifiedSelfReviewRun(t)
	driveToSelfReview(t, store)
	transition, err := store.CompleteAndAdvance("project", "task-self-review", "needs rework", "", map[string]string{
		"self_review":         "structured findings: contract violated at file.go:42",
		"self_review_verdict": "issues_fixed",
		"review_rounds":       "2",
	}, "completed")
	if err != nil {
		t.Fatalf("complete agent_self_review: %v", err)
	}
	if transition.NextInst == nil {
		t.Fatal("expected reworked implement instance")
	}
	if gotRounds := transition.NextInst.InputValues["review_rounds"]; gotRounds != "2" {
		t.Fatalf("rework must carry review_rounds into implement, got %q", gotRounds)
	}
	if gotComments := transition.NextInst.InputValues["review_comments"]; gotComments == "" {
		t.Fatal("rework must carry the self-review report as review_comments")
	}
}

// Greenfield already routed on self_review_verdict with a default pass edge
// (its verdict vocabulary is two-valued: pass / issues_fixed, no escalate);
// assert the partition stays intact so the unified fix and the greenfield
// pattern can't drift apart silently.
func TestGreenfieldSelfReviewVerdictPartition(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Skip("greenfield template not registered")
	}
	var hasDefaultPass bool
	conditions := map[string]bool{}
	for _, e := range tmpl.Edges {
		if e.From != "self_review" {
			continue
		}
		if e.Condition == nil || e.IsDefault {
			hasDefaultPass = hasDefaultPass || e.To == "code_review"
			continue
		}
		if e.Condition.Field != "self_review_verdict" {
			t.Fatalf("self_review edge %s conditions on %q — routing must key on the verdict, not the report", e.ID, e.Condition.Field)
		}
		if e.Condition.Operator != "eq" {
			t.Fatalf("self_review_verdict edge %s must use eq, got %q", e.ID, e.Condition.Operator)
		}
		conditions[e.Condition.Value] = true
	}
	if !hasDefaultPass {
		t.Fatal("greenfield self_review must keep a default edge to code_review")
	}
	if !conditions["issues_fixed"] {
		t.Fatal("greenfield missing eq edge for verdict issues_fixed")
	}
}
