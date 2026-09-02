package workflow

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestGreenfieldTemplateShape(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	if len(tmpl.Steps) != 9 {
		t.Fatalf("expected 9 steps, got %d", len(tmpl.Steps))
	}
	if tmpl.StartStepID != "requirement_draft" {
		t.Fatalf("start step = %q", tmpl.StartStepID)
	}
	var design *struct{}
	_ = design
	found := false
	for _, s := range tmpl.Steps {
		if s.ID == "design_review" {
			found = true
			if s.Type != "human_review" {
				t.Fatalf("design_review type = %q", s.Type)
			}
			if s.Config["designGate"] != "true" {
				t.Fatalf("designGate config = %v (Config is map[string]string, value must be exactly \"true\")", s.Config)
			}
			outputs := map[string]bool{}
			for _, f := range s.OutputFields {
				outputs[f.Name] = true
			}
			for _, want := range []string{"decision", "comments", "approved_design_source", "approved_design_project_id", "approved_design_preview_url"} {
				if !outputs[want] {
					t.Fatalf("design_review missing output %q", want)
				}
			}
			for _, f := range s.OutputFields {
				if f.Name == "approved_design_source" && !f.Optional {
					t.Fatalf("approved_design_source must be optional")
				}
			}
		}
	}
	if !found {
		t.Fatal("design_review step missing")
	}
	// rework edge design_review -> requirement_draft
	rework := false
	for _, e := range tmpl.Edges {
		if e.From == "design_review" && e.To == "requirement_draft" && e.Condition != nil &&
			e.Condition.Field == "decision" && e.Condition.Value == "request_changes" {
			rework = true
		}
		if e.From == "design_review" && e.To == "implementation" {
			mapping := e.InputMapping
			if mapping["approved_design_project_id"] != "$output.approved_design_project_id" {
				t.Fatalf("design approve edge mapping = %v", mapping)
			}
		}
	}
	if !rework {
		t.Fatal("design rework edge missing")
	}
}

// The self_review step claims to judge the implementation "against the
// requirement and design", so the frozen design contract fields must reach
// it: without them the reviewer agent can only guess the design from the PR
// diff (the reviewer cannot see what the human actually approved).
func TestGreenfieldSelfReviewCarriesDesignContract(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	var selfReview *entity.WorkflowStep
	for i := range tmpl.Steps {
		if tmpl.Steps[i].ID == "self_review" {
			selfReview = &tmpl.Steps[i]
		}
	}
	if selfReview == nil {
		t.Fatal("self_review step missing")
	}
	inputs := map[string]bool{}
	for _, f := range selfReview.InputFields {
		inputs[f.Name] = true
	}
	for _, want := range []string{"pr", "approved_requirement", "approved_design_html", "approved_design_snapshot_path", "approved_design_project_id"} {
		if !inputs[want] {
			t.Fatalf("self_review missing input %q", want)
		}
	}
	var carryEdge bool
	for _, e := range tmpl.Edges {
		if e.From == "implementation" && e.To == "self_review" {
			mapping := e.InputMapping
			if mapping["approved_design_html"] != "$input.approved_design_html" ||
				mapping["approved_design_snapshot_path"] != "$input.approved_design_snapshot_path" ||
				mapping["approved_requirement"] != "$input.approved_requirement" {
				t.Fatalf("implementation->self_review edge must carry the frozen design contract, mapping=%v", mapping)
			}
			carryEdge = true
		}
	}
	if !carryEdge {
		t.Fatal("implementation->self_review edge missing")
	}
}

func TestGreenfieldTemplateEnAndZh(t *testing.T) {
	for _, locale := range []string{"en", "zh-CN"} {
		tmpl, ok := Template("greenfield-delivery-pipeline", locale)
		if !ok {
			t.Fatalf("template missing for locale %s", locale)
		}
		if tmpl.Name == "" || tmpl.Description == "" {
			t.Fatalf("empty name/description for %s", locale)
		}
		for _, s := range tmpl.Steps {
			if s.Title == "" || s.Description == "" {
				t.Fatalf("step %s has empty title/description for %s", s.ID, locale)
			}
		}
	}
}

// The self_review rework edge must key on the explicit self_review_verdict
// output: the review report itself is always non-empty, so a neq-"" condition
// on review_comments bounces every self-review back to implementation forever
// (od-e2e incident). A missing verdict must fall through to the default pass
// edge so the human code review gate stays in the loop.
func TestGreenfieldSelfReviewRouting(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	var verdictField *entity.WorkflowField
	for _, s := range tmpl.Steps {
		if s.ID != "self_review" {
			continue
		}
		for i := range s.OutputFields {
			if s.OutputFields[i].Name == "self_review_verdict" {
				verdictField = &s.OutputFields[i]
			}
		}
	}
	if verdictField == nil {
		t.Fatal("self_review missing self_review_verdict output field")
	}
	reworkFound, defaultFound := false, false
	for _, e := range tmpl.Edges {
		if e.From != "self_review" {
			continue
		}
		if e.To == "implementation" {
			if e.Condition == nil || e.Condition.Field != "self_review_verdict" || e.Condition.Operator != "eq" || e.Condition.Value != "issues_fixed" {
				t.Fatalf("self_review rework edge condition = %+v, want self_review_verdict eq issues_fixed", e.Condition)
			}
			reworkFound = true
		}
		if e.To == "code_review" && e.IsDefault {
			defaultFound = true
		}
	}
	if !reworkFound || !defaultFound {
		t.Fatalf("self_review edges: rework=%v default=%v", reworkFound, defaultFound)
	}
	// Simulate the incident: report present, verdict absent -> default pass.
	edge, ok := chooseNextEdge(tmpl.Edges, "self_review", map[string]string{"review_comments": "评审报告：全部通过"}, "")
	if !ok || edge.To != "code_review" {
		t.Fatalf("missing verdict should default to code_review, got %q ok=%v", edge.To, ok)
	}
	edge, ok = chooseNextEdge(tmpl.Edges, "self_review", map[string]string{"self_review_verdict": "issues_fixed", "review_comments": "需修复"}, "")
	if !ok || edge.To != "implementation" {
		t.Fatalf("explicit issues_fixed should rework, got %q ok=%v", edge.To, ok)
	}
}
