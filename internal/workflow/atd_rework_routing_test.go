package workflow

import "testing"

// S2 run5 (2026-09-26, wfr-g6shgofc): integration_review request_changes used
// to route DIRECTLY to implementation, bypassing acceptance_test_design — the
// batched path then reached qa with empty test_spec_* inputs (required) and
// the QA agent correctly refused twice. Regression: the rework edge must enter
// acceptance_test_design first, so the spec baseline exists (refreshed with
// the integration verdict) before any reworked implementation runs.
func TestIntegrationReworkRoutesThroughAcceptanceTestDesign(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	found := false
	for _, e := range tmpl.Edges {
		if e.From != "integration_review" {
			continue
		}
		if e.Condition == nil || e.Condition.Field != "decision" || e.Condition.Value != "request_changes" {
			continue
		}
		found = true
		if e.To != "acceptance_test_design" {
			t.Fatalf("integration_review request_changes must route to acceptance_test_design (spec baseline before reworked implementation), got %q", e.To)
		}
		// The spec author must see the human verdict and the aggregated branch
		// reports (what was delivered and what failed at the seams).
		if e.InputMapping["review_comments"] != "$output.comments" {
			t.Fatalf("e-integration-rework must forward the integration verdict as review_comments, got %q", e.InputMapping["review_comments"])
		}
		if e.InputMapping["branch_reports"] != "$input.branch_reports" {
			t.Fatalf("e-integration-rework must forward branch_reports as delivery context, got %q", e.InputMapping["branch_reports"])
		}
		// Later loops must not lose an existing baseline (pass-through).
		for _, key := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
			if e.InputMapping[key] != "$input."+key {
				t.Fatalf("e-integration-rework must pass through %s, got %q", key, e.InputMapping[key])
			}
		}
	}
	if !found {
		t.Fatal("e-integration-rework edge not found")
	}
	// No direct seam-to-code edge may remain.
	for _, e := range tmpl.Edges {
		if e.From == "integration_review" && e.To == "implementation" {
			t.Fatalf("edge %s: integration_review must never reach implementation directly on the batched path (QA starves without a spec baseline)", e.ID)
		}
	}
	// ATD declares the rework context as optional inputs so the agent prompt
	// actually carries them.
	atd := findStep(t, tmpl, "acceptance_test_design")
	inNames := map[string]bool{}
	for _, f := range atd.InputFields {
		inNames[f.Name] = true
	}
	for _, want := range []string{"review_comments", "branch_reports"} {
		if !inNames[want] {
			t.Fatalf("acceptance_test_design must declare optional input %s (rework context)", want)
		}
	}
	// e-atd-impl must carry the verdict context + a freshly designed spec into
	// the reworked implementation.
	for _, e := range tmpl.Edges {
		if e.ID != "e-atd-impl" {
			continue
		}
		if e.InputMapping["review_comments"] != "$input.review_comments" {
			t.Fatalf("e-atd-impl must forward review_comments into the reworked implementation, got %q", e.InputMapping["review_comments"])
		}
		for _, key := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
			if e.InputMapping[key] != "$output."+key {
				t.Fatalf("e-atd-impl must forward freshly designed %s, got %q", key, e.InputMapping[key])
			}
		}
	}
}

// TestGreenfieldImplementationNeedsSpecOnEveryEntry: on the batched path the
// spec baseline must exist BEFORE implementation (re)runs — either produced by
// the immediately preceding acceptance_test_design, or carried through a
// rework edge that itself received it from a spec-producing hop.
func TestGreenfieldImplementationNeedsSpecOnEveryEntry(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	for _, e := range tmpl.Edges {
		if e.To != "implementation" {
			continue
		}
		if e.From == "acceptance_test_design" {
			if e.InputMapping["test_spec_doc"] != "$output.test_spec_doc" {
				t.Fatalf("edge %s: ATD->implementation must deliver the freshly designed spec", e.ID)
			}
			continue
		}
		for _, key := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
			if e.InputMapping[key] != "$input."+key {
				t.Fatalf("edge %s (%s->implementation) must forward %s so the reworked implementation keeps its baseline, got %q", e.ID, e.From, key, e.InputMapping[key])
			}
		}
	}
}
