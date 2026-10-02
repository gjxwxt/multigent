package workflow

import (
	"strings"
	"testing"
)

// rulingPlan builds an N-package, 2-wave plan where wave 0 packages are
// completed and wave 1 packages deferred as demanded by the caller.
func rulingPlan(t *testing.T, n int, defers []string) (DeliveryPlan, PlanProgress) {
	t.Helper()
	reqs := make([]PlanRequirementItem, 0, n)
	wps := make([]PlanWorkPackage, 0, n)
	for i := 0; i < n; i++ {
		id := "wp-" + string(rune('a'+i))
		reqID := "uc-" + string(rune('a'+i))
		reqs = append(reqs, PlanRequirementItem{ID: reqID, Text: "deliver " + id})
		wp := PlanWorkPackage{
			ID:                 id,
			Title:              "Package " + id,
			AcceptanceCriteria: []string{reqID},
			AgentBinding:       "Nora",
		}
		if i >= 5 { // wave 1: depends on wp-a
			wp.DependsOn = []string{"wp-a"}
		}
		wps = append(wps, wp)
	}
	plan := DeliveryPlan{
		SchemaVersion:    DeliveryPlanSchemaVersion,
		PlanID:           "plan-ruling",
		Version:          1,
		RunID:            "wfr-ruling",
		RequirementItems: reqs,
		WorkPackages:     wps,
	}
	if err := ValidateDeliveryPlan(plan); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, id := range defers {
		states[id] = PlanWPStateDeferred
	}
	for _, wp := range wps {
		if states[wp.ID] == "" {
			if wp.DependsOn == nil { // wave 0
				states[wp.ID] = PlanWPStateCompleted
			}
		} else {
			continue
		}
		if wp.DependsOn != nil && states[wp.DependsOn[0]] == PlanWPStateCompleted {
			// leave unmaterialized → Ready; not needed for the ruling paths
			delete(states, wp.ID)
		}
	}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	return plan, progress
}

func TestDeferredJoinRulingAtHalfJoins(t *testing.T) {
	// 10 packages, 5 deferred: exactly 50% — NOT over the threshold — and
	// every deferred package is the SOLE referencer of its own item, so the
	// requirement-surface gate fires per-item too. To isolate the ratio
	// boundary the deferred set must reference items that OTHER (completed)
	// packages also reference — the shared-item shape from the surface test.
	reqs := []PlanRequirementItem{{ID: "uc-shared", Text: "shared"}}
	wps := []PlanWorkPackage{}
	for i := 0; i < 10; i++ {
		id := "wp-" + string(rune('a'+i))
		wps = append(wps, PlanWorkPackage{
			ID: id, Title: "Package " + id,
			AcceptanceCriteria: []string{"uc-shared"},
			AgentBinding:       "Nora",
		})
	}
	plan := DeliveryPlan{
		SchemaVersion: DeliveryPlanSchemaVersion, PlanID: "plan-half", Version: 1, RunID: "wfr-half",
		RequirementItems: reqs, WorkPackages: wps,
	}
	if err := ValidateDeliveryPlan(plan); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for i := 0; i < 10; i++ {
		id := "wp-" + string(rune('a'+i))
		if i < 5 {
			states[id] = PlanWPStateDeferred
		} else {
			states[id] = PlanWPStateCompleted
		}
	}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Deferred) != 5 {
		t.Fatalf("fixture must defer 5, got %v", progress.Deferred)
	}
	ruling := EvaluateDeferredJoinRuling(plan, progress)
	if ruling.Blocked {
		t.Fatalf("exactly 50%% deferred must still join: %s", ruling.Reason)
	}
}

func TestDeferredJoinRulingOverHalfBlocks(t *testing.T) {
	plan, progress := rulingPlan(t, 10, []string{"wp-b", "wp-f", "wp-g", "wp-h", "wp-i", "wp-j"})
	ruling := EvaluateDeferredJoinRuling(plan, progress)
	if !ruling.Blocked {
		t.Fatal("6 of 10 deferred must block the join")
	}
	for _, want := range []string{"6 of 10", "retry the deferred work", "abandon this round"} {
		if !strings.Contains(ruling.Reason, want) {
			t.Fatalf("refusal text must be actionable: missing %q in %q", want, ruling.Reason)
		}
	}
}

func TestDeferredJoinRulingRequirementSurfaceCovered(t *testing.T) {
	// The clean requirement-surface shape: 4 packages, 2 deferred (ratio
	// exactly 50% — under the strict threshold), and the two deferred ones
	// are the ONLY referencers of uc-shared while everything else completed.
	// The surface gate fires even though the ratio gate does not.
	reqs := []PlanRequirementItem{
		{ID: "uc-shared", Text: "shared capability"},
		{ID: "uc-other", Text: "other capability"},
	}
	wps := []PlanWorkPackage{
		{ID: "wp-p1", Title: "P1", AcceptanceCriteria: []string{"uc-shared"}, AgentBinding: "Nora"},
		{ID: "wp-p2", Title: "P2", AcceptanceCriteria: []string{"uc-shared"}, AgentBinding: "Nora"},
		{ID: "wp-p3", Title: "P3", AcceptanceCriteria: []string{"uc-other"}, AgentBinding: "Nora"},
		{ID: "wp-p4", Title: "P4", AcceptanceCriteria: []string{"uc-other"}, AgentBinding: "Nora"},
	}
	plan := DeliveryPlan{
		SchemaVersion: DeliveryPlanSchemaVersion, PlanID: "plan-req-surface", Version: 1, RunID: "wfr-req-surface",
		RequirementItems: reqs, WorkPackages: wps,
	}
	if err := ValidateDeliveryPlan(plan); err != nil {
		t.Fatal(err)
	}
	progress, err := EvaluatePlanProgress(plan, map[string]string{
		"wp-p1": PlanWPStateDeferred, "wp-p2": PlanWPStateDeferred,
		"wp-p3": PlanWPStateCompleted, "wp-p4": PlanWPStateCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Deferred) != 2 {
		t.Fatalf("fixture must defer exactly 2 of 4, got %v", progress.Deferred)
	}
	ruling := EvaluateDeferredJoinRuling(plan, progress)
	if !ruling.Blocked {
		t.Fatal("a requirement item whose every referencing package deferred must block the join even under the ratio")
	}
	if !strings.Contains(ruling.Reason, "uc-shared") {
		t.Fatalf("refusal must name the starved requirement item: %q", ruling.Reason)
	}
}

func TestDeferredJoinRulingZeroReferenceItemIsNotVacuous(t *testing.T) {
	// An orphan requirement item nobody references must NOT block the join
	// (vacuous truth guard): 2 packages, 1 deferred, orphan item present.
	plan := DeliveryPlan{
		SchemaVersion: DeliveryPlanSchemaVersion, PlanID: "plan-orphan-req", Version: 1, RunID: "wfr-orphan",
		RequirementItems: []PlanRequirementItem{
			{ID: "uc-orphan", Text: "nobody references me"},
			{ID: "uc-a", Text: "real"},
		},
		WorkPackages: []PlanWorkPackage{
			{ID: "wp-a", Title: "A", AcceptanceCriteria: []string{"uc-a"}, AgentBinding: "Nora"},
			{ID: "wp-b", Title: "B", AcceptanceCriteria: []string{"uc-a"}, AgentBinding: "Nora"},
		},
	}
	progress, err := EvaluatePlanProgress(plan, map[string]string{"wp-b": PlanWPStateDeferred})
	if err != nil {
		t.Fatal(err)
	}
	if ruling := EvaluateDeferredJoinRuling(plan, progress); ruling.Blocked {
		t.Fatalf("a zero-reference requirement item must not starve: %s", ruling.Reason)
	}
}

func TestDeferredJoinRulingNoDeferralsIsClean(t *testing.T) {
	plan, progress := rulingPlan(t, 3, nil)
	if ruling := EvaluateDeferredJoinRuling(plan, progress); ruling.Blocked {
		t.Fatalf("zero deferrals must never block: %s", ruling.Reason)
	}
}
