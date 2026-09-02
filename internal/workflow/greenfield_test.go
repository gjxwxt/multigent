package workflow

import (
	"testing"
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
