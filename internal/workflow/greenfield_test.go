package workflow

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestExportGreenfieldJSON(t *testing.T) {
	exportPath := os.Getenv("WORKFLOW_EXPORT")
	if exportPath == "" {
		return
	}
	def, ok := DefinitionFromTemplate("greenfield-delivery-pipeline", "zh-CN", "新项目交付流水线（设计确认闸门）")
	if !ok {
		t.Fatal("definition not found")
	}
	def.ID = "wf-x97mgd2q"
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportPath, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// Requirement clarification loop (v6): submitting a round is the ONLY exit of
// requirement_review — every non-freeze submit maps back to requirement_draft
// with the full clarification contract, and the freeze edge keys on approve
// with the server enforcing the open_questions-cleared gate. A direct
// approve→design edge that bypasses the clarification loop must not exist.
func TestGreenfieldRequirementClarificationLoop(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	var reqDraft, reqReview *entity.WorkflowStep
	for i := range tmpl.Steps {
		switch tmpl.Steps[i].ID {
		case "requirement_draft":
			reqDraft = &tmpl.Steps[i]
		case "requirement_review":
			reqReview = &tmpl.Steps[i]
		}
	}
	if reqDraft == nil || reqReview == nil {
		t.Fatal("requirement_draft/requirement_review step missing")
	}
	// The review step declares open_questions as an input (per-question cards
	// have a declared source) and clarifications as a declared output.
	inputs := map[string]bool{}
	for _, f := range reqReview.InputFields {
		inputs[f.Name] = true
	}
	if !inputs["open_questions"] || !inputs["requirement_draft"] {
		t.Fatalf("requirement_review inputs = %v, want open_questions + requirement_draft", inputs)
	}
	outputs := map[string]bool{}
	for _, f := range reqReview.OutputFields {
		outputs[f.Name] = true
	}
	if !outputs["clarifications"] {
		t.Fatalf("requirement_review outputs = %v, want clarifications", outputs)
	}
	// The draft step receives the clarification contract on rework entry.
	draftInputs := map[string]bool{}
	for _, f := range reqDraft.InputFields {
		draftInputs[f.Name] = true
	}
	for _, want := range []string{"clarifications", "previous_draft", "open_questions", "review_rounds"} {
		if !draftInputs[want] {
			t.Fatalf("requirement_draft inputs missing %q (clarification contract)", want)
		}
	}
	// Edge topology: exactly one forward edge (freeze, approve-conditioned)
	// and one rework edge back to requirement_draft (request_changes).
	forward, rework := 0, 0
	for _, e := range tmpl.Edges {
		if e.From != "requirement_review" {
			continue
		}
		switch {
		case e.To == "design_review":
			forward++
			if e.Condition == nil || e.Condition.Field != "decision" || e.Condition.Value != "approve" {
				t.Fatalf("freeze edge condition = %+v, want decision eq approve", e.Condition)
			}
			if e.InputMapping["clarifications"] != "$output.clarifications" ||
				e.InputMapping["approved_requirement"] != "$input.requirement_draft" {
				t.Fatalf("freeze edge must carry the woven draft + clarification log, mapping=%v", e.InputMapping)
			}
		case e.To == "requirement_draft":
			rework++
			if e.Condition == nil || e.Condition.Value != "request_changes" {
				t.Fatalf("rework edge condition = %+v, want decision eq request_changes", e.Condition)
			}
			for _, want := range []string{"clarifications", "previous_draft", "open_questions", "review_rounds"} {
				if e.InputMapping[want] == "" {
					t.Fatalf("rework edge missing mapping for %s: %v", want, e.InputMapping)
				}
			}
		default:
			t.Fatalf("unexpected edge from requirement_review to %q — the clarification loop has exactly two exits", e.To)
		}
	}
	if forward != 1 || rework != 1 {
		t.Fatalf("requirement_review edges: forward=%d rework=%d, want 1/1", forward, rework)
	}
	// The rework edge id carries "rework" so the platform increments
	// review_rounds (isReworkEdge matches on the id substring).
	for _, e := range tmpl.Edges {
		if e.From == "requirement_review" && e.To == "requirement_draft" && !strings.Contains(strings.ToLower(e.ID), "rework") {
			t.Fatalf("rework edge id %q must contain 'rework' for platform round accounting", e.ID)
		}
	}
}

func TestGreenfieldTemplateShape(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	if len(tmpl.Steps) != 17 {
		t.Fatalf("expected 17 steps (12 original + scale_gate/contract_batch/contract_review/parallel_workstreams/integration_review), got %d", len(tmpl.Steps))
	}
	if tmpl.StartStepID != "requirement_draft" {
		t.Fatalf("start step = %q", tmpl.StartStepID)
	}
	var design *struct{}
	_ = design
	foundDesign, foundQA, foundQASignoff := false, false, false
	for _, s := range tmpl.Steps {
		if s.ID == "design_review" {
			foundDesign = true
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
			for _, want := range []string{"decision", "comments", "approved_design_source", "approved_design_project_id", "approved_design_preview_url", "design_waiver_reason", "design_waived"} {
				if !outputs[want] {
					t.Fatalf("design_review missing output %q", want)
				}
			}
			for _, f := range s.OutputFields {
				if f.Name == "approved_design_source" && !f.Optional {
					t.Fatalf("approved_design_source must be optional")
				}
			}
			inputs := map[string]bool{}
			for _, f := range s.InputFields {
				inputs[f.Name] = true
			}
			if !inputs["approved_requirement"] {
				t.Fatalf("design_review missing input approved_requirement")
			}
		}
		if s.ID == "qa" {
			foundQA = true
			if s.Type != "agent_task" || s.ActorRole != "qa-agent" {
				t.Fatalf("qa step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
			hasMatrix := false
			for _, f := range s.OutputFields {
				if f.Name == "risk_coverage_matrix" {
					hasMatrix = true
					if f.Optional {
						t.Fatalf("risk_coverage_matrix MUST be mandatory (not optional)")
					}
				}
			}
			if !hasMatrix {
				t.Fatalf("qa step missing risk_coverage_matrix output")
			}
		}
		if s.ID == "qa_signoff" {
			foundQASignoff = true
			if s.Type != "human_review" || s.ActorRole != "qa-owner" {
				t.Fatalf("qa_signoff step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
		if s.ID == "implementation" {
			if s.Type != "agent_task" || s.ActorRole != "developer-agent" {
				t.Fatalf("implementation step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
		if s.ID == "self_review" {
			if s.Type != "agent_task" || s.ActorRole != "reviewer-agent" {
				t.Fatalf("self_review step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
		if s.ID == "code_review" {
			if s.Type != "human_review" || s.ActorRole != "owner-engineer" {
				t.Fatalf("code_review step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
		if s.ID == "pr_open_and_merge" {
			if s.Type != "agent_task" || s.ActorRole != "developer-agent" {
				t.Fatalf("pr_open_and_merge step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
		if s.ID == "release" {
			if s.Type != "agent_task" || s.ActorRole != "release-agent" {
				t.Fatalf("release step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
		if s.ID == "go_live_confirm" {
			if s.Type != "human_review" || s.ActorRole != "product-owner" {
				t.Fatalf("go_live_confirm step mismatch: type=%s role=%s", s.Type, s.ActorRole)
			}
		}
	}
	if !foundDesign {
		t.Fatal("design_review step missing")
	}
	if !foundQA {
		t.Fatal("qa step missing")
	}
	if !foundQASignoff {
		t.Fatal("qa_signoff step missing")
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

func TestGreenfieldDesignContractAndQAPreMergeFlow(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}

	// 1. Verify all 3 rework edges preserve the design contract and requirement
	reworkTargets := map[string]string{
		"self_review": "e-self-review-rework",
		"code_review": "e-code-review-rework",
		"qa_signoff":  "e-qa-rework",
	}

	for fromStep, edgeID := range reworkTargets {
		var found bool
		for _, e := range tmpl.Edges {
			if e.From == fromStep && e.To == "implementation" {
				found = true
				if e.ID != edgeID {
					t.Fatalf("unexpected edge ID %q for %s->implementation, want %q", e.ID, fromStep, edgeID)
				}
				mapping := e.InputMapping
				for _, requiredKey := range []string{
					"approved_requirement",
					"approved_design_source",
					"approved_design_project_id",
					"approved_design_preview_url",
					"approved_design_html",
					"approved_design_snapshot_path",
					// vNext (acceptance-test-design plan §5.2): rework must never
					// lose the acceptance baseline.
					"test_spec_doc",
					"test_spec_manifest",
					"test_spec_summary",
				} {
					if mapping[requiredKey] != "$input."+requiredKey {
						t.Fatalf("rework edge %s missing mapping for %s: got %v", edgeID, requiredKey, mapping)
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing rework edge from %s to implementation", fromStep)
		}
	}

	// 2. Verify Pre-Merge QA flow: code_review -> qa -> qa_signoff -> pr_open_and_merge
	var crToQA, qaToSignoff, signoffToMerge bool
	for _, e := range tmpl.Edges {
		if e.From == "code_review" && e.To == "qa" {
			crToQA = true
			if e.Condition == nil || e.Condition.Field != "decision" || e.Condition.Value != "approve" {
				t.Fatalf("code_review->qa condition mismatch: %+v", e.Condition)
			}
			mapping := e.InputMapping
			if mapping["approved_design_snapshot_path"] != "$input.approved_design_snapshot_path" ||
				mapping["approved_requirement"] != "$input.approved_requirement" {
				t.Fatalf("code_review->qa missing design inputs: %v", mapping)
			}
		}
		if e.From == "qa" && e.To == "qa_signoff" {
			qaToSignoff = true
			mapping := e.InputMapping
			if mapping["risk_coverage_matrix"] != "$output.risk_coverage_matrix" ||
				mapping["test_report"] != "$output.test_report" {
				t.Fatalf("qa->qa_signoff missing matrix/report mapping: %v", mapping)
			}
		}
		if e.From == "qa_signoff" && e.To == "pr_open_and_merge" {
			signoffToMerge = true
			if e.Condition == nil || e.Condition.Field != "decision" || e.Condition.Value != "approve" {
				t.Fatalf("qa_signoff->pr_open_and_merge condition mismatch: %+v", e.Condition)
			}
		}
	}

	if !crToQA {
		t.Fatal("missing code_review -> qa edge")
	}
	if !qaToSignoff {
		t.Fatal("missing qa -> qa_signoff edge")
	}
	if !signoffToMerge {
		t.Fatal("missing qa_signoff -> pr_open_and_merge edge (QA MUST strictly precede merge to protect main!)")
	}
}

// Regression (2026-10-03, round L): the template's scattered x positions put
// several steps within one node width (198px) of each other, and every step
// shares y=180 — the read-only React Flow board (ProjectTaskFollowPage) renders
// stored positions verbatim, so the 17 nodes visibly stacked on top of each
// other. Positions must follow the main spine at the same 280px pitch as
// software-delivery-v1, and the template version must advance so
// EnsureGreenfieldDefinition upgrades already-seeded copies in place.
func TestGreenfieldTemplateLayoutNoOverlap(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	if tmpl.Version < 2 {
		t.Fatalf("template version = %d; must be >= 2 so EnsureGreenfieldDefinition upgrades stored v1 copies (canvas layout fix)", tmpl.Version)
	}
	const nodeWidth = 198 // WORKFLOW_NODE_WIDTH in web/src/components/workflow/WorkflowBoard.tsx
	byX := map[int]string{}
	for _, s := range tmpl.Steps {
		if prev, clash := byX[s.Position.X]; clash {
			t.Fatalf("steps %q and %q share x=%d (same-column overlap is only acceptable with distinct y; all steps use y=180)", prev, s.ID, s.Position.X)
		}
		byX[s.Position.X] = s.ID
	}
	xs := make([]int, 0, len(byX))
	for x := range byX {
		xs = append(xs, x)
	}
	sort.Ints(xs)
	for i := 1; i < len(xs); i++ {
		if gap := xs[i] - xs[i-1]; gap < nodeWidth {
			t.Fatalf("gap between x=%d (%s) and x=%d (%s) is %d < node width %d — nodes will overlap on the canvas", xs[i-1], byX[xs[i-1]], xs[i], byX[xs[i]], gap, nodeWidth)
		}
	}
}
