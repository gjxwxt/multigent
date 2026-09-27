package workflow

// Delivery SHA hand-off: template-structure guarantees for the greenfield
// pipeline. The QA step must declare the machine-verified anchor inputs, the
// edge chain implementation→self_review→code_review→qa must thread the
// FRESH evidence from the delivery gate, and the rework edges must NOT carry
// a stale anchor (re-entering implementation re-proves the SHA at
// completion — a carried pre-rework SHA would hand QA a stale commit).

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func gfStep(t *testing.T, tmpl entity.WorkflowTemplate, id string) entity.WorkflowStep {
	t.Helper()
	for _, s := range tmpl.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("step %q not found in template", id)
	return entity.WorkflowStep{}
}

func gfEdge(t *testing.T, tmpl entity.WorkflowTemplate, id string) entity.WorkflowEdge {
	t.Helper()
	for _, e := range tmpl.Edges {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("edge %q not found in template", id)
	return entity.WorkflowEdge{}
}

func gfHasInput(t *testing.T, step entity.WorkflowStep, name string, optional bool) {
	t.Helper()
	for _, f := range step.InputFields {
		if f.Name == name {
			if f.Optional != optional {
				t.Fatalf("input %q on step %q: optional=%v, want %v", name, step.ID, f.Optional, optional)
			}
			return
		}
	}
	t.Fatalf("step %q must declare input %q", step.ID, name)
}

// TestGreenfieldQADeclaresMachineAnchorInputs: the QA step's declared input
// contract must carry the machine anchor (required delivery_sha, optional
// delivery_branch) — prompt-visible so the agent knows the ONLY sanctioned
// code locator.
func TestGreenfieldQADeclaresMachineAnchorInputs(t *testing.T) {
	for _, locale := range []string{"en", "zh-CN"} {
		tmpl := greenfieldDeliveryTemplate(locale)
		qa := gfStep(t, tmpl, "qa")
		gfHasInput(t, qa, "delivery_sha", false)
		gfHasInput(t, qa, "delivery_branch", true)
		// The qaDesc must instruct the clone+checkout+rev-parse protocol and
		// forbid the free-text fallback: breaking any of these phrases means
		// the agent could regress to anchoring on pr/approved_change free
		// text (the run6 failure mode).
		for _, phrase := range []string{"delivery_sha", "git rev-parse HEAD"} {
			if !strings.Contains(qa.Description, phrase) {
				t.Fatalf("qa description (locale %s) must instruct the verification protocol: missing %q", locale, phrase)
			}
		}
		if locale == "en" {
			for _, phrase := range []string{"never fall back", "free-text PR summary"} {
				if !strings.Contains(qa.Description, phrase) {
					t.Fatalf("qa description must forbid the free-text fallback: missing %q", phrase)
				}
			}
		}
		if locale == "zh-CN" {
			for _, phrase := range []string{"严禁回退", "自由文本", "如实"} {
				if !strings.Contains(qa.Description, phrase) {
					t.Fatalf("qa description must forbid the free-text fallback: missing %q", phrase)
				}
			}
		}
	}
}

// TestGreenfieldDeliverySHAThreadedFromImplToQA: the evidence chain must use
// $output.delivery_sha at the implementation boundary (fresh gate evidence)
// and $input.delivery_sha downstream (threaded pass-through).
func TestGreenfieldDeliverySHAThreadedFromImplToQA(t *testing.T) {
	tmpl := greenfieldDeliveryTemplate("en")
	// Fresh evidence off the implementation completion.
	implSR := gfEdge(t, tmpl, "e-impl-self-review")
	if got := implSR.InputMapping["delivery_sha"]; got != "$output.delivery_sha" {
		t.Fatalf("e-impl-self-review must map delivery_sha from the implementation OUTPUT (fresh gate evidence), got %q", got)
	}
	// Threaded pass-through downstream.
	for _, edgeID := range []string{"e-self-review-pass", "e-code-review-qa"} {
		e := gfEdge(t, tmpl, edgeID)
		if got := e.InputMapping["delivery_sha"]; got != "$input.delivery_sha" {
			t.Fatalf("%s must thread delivery_sha from the step INPUT, got %q", edgeID, got)
		}
		if got := e.InputMapping["delivery_branch"]; got != "$input.delivery_branch" {
			t.Fatalf("%s must thread delivery_branch from the step INPUT, got %q", edgeID, got)
		}
	}
}

// TestGreenfieldReworkEdgesCarryNoStaleAnchor: edges re-entering
// implementation must NOT map delivery_sha — the pre-rework evidence would
// be stale for the code the rework is about to write; the fresh anchor is
// re-proven at the rework completion.
func TestGreenfieldReworkEdgesCarryNoStaleAnchor(t *testing.T) {
	tmpl := greenfieldDeliveryTemplate("en")
	for _, edgeID := range []string{"e-self-review-rework", "e-code-review-rework", "e-qa-rework", "e-atd-impl"} {
		e := gfEdge(t, tmpl, edgeID)
		if v, ok := e.InputMapping["delivery_sha"]; ok {
			t.Fatalf("rework edge %s must NOT carry a stale delivery_sha, got %q", edgeID, v)
		}
	}
}

// TestGreenfieldParallelEvidenceContextOnly: the parallel-stage aggregate
// must NOT surface a single scalar delivery_sha into integration_review —
// with multiple workstreams the unprefixed aggregate key is whichever
// branch wrote first, and one arbitrary workstream SHA masquerading as
// "the" candidate would mislead the reviewer. QA's authoritative anchor
// comes only from the integrated implementation's own completion gate.
func TestGreenfieldParallelEvidenceContextOnly(t *testing.T) {
	tmpl := greenfieldDeliveryTemplate("en")
	e := gfEdge(t, tmpl, "e-parallel-integration")
	if v, ok := e.InputMapping["delivery_sha"]; ok {
		t.Fatalf("e-parallel-integration must NOT map a scalar delivery_sha (multi-branch first-writer masquerade), got %q", v)
	}
	if v, ok := e.InputMapping["delivery_branch"]; ok {
		t.Fatalf("e-parallel-integration must NOT map a scalar delivery_branch, got %q", v)
	}
	// qa_signoff records the anchor for the audited merge step.
	signoff := gfStep(t, tmpl, "qa_signoff")
	gfHasInput(t, signoff, "delivery_sha", true)
	mergeEdge := gfEdge(t, tmpl, "e-qa-signoff-approve")
	if got := mergeEdge.InputMapping["delivery_sha"]; got != "$input.delivery_sha" {
		t.Fatalf("e-qa-signoff-approve must carry the proven SHA to the merge step, got %q", got)
	}
	// The middle hand-off steps declare the threaded anchor so the input
	// contract is visible where the values land.
	gfHasInput(t, gfStep(t, tmpl, "self_review"), "delivery_sha", true)
	gfHasInput(t, gfStep(t, tmpl, "code_review"), "delivery_sha", true)
}

// TestGreenfieldImplDeclaresOptionalEvidenceOutputs: the implementation step
// (and the parallel branches) declare the machine keys as OPTIONAL outputs —
// the gate writes them; the agent cannot satisfy anything by writing them.
func TestGreenfieldImplDeclaresOptionalEvidenceOutputs(t *testing.T) {
	tmpl := greenfieldDeliveryTemplate("en")
	impl := gfStep(t, tmpl, "implementation")
	for _, name := range []string{"delivery_sha", "delivery_branch"} {
		found := false
		for _, f := range impl.OutputFields {
			if f.Name == name {
				if !f.Optional {
					t.Fatalf("output %q on implementation must be optional (the platform writes it, not the agent)", name)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("implementation step must declare optional output %q", name)
		}
	}
}
