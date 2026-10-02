package workflow

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// TestDefaultEdgeFallbackAnnotation covers the annotate helper's four shapes:
// conditional-route match (silent), fallback without a verdict (generic
// degradation note), fallback WITH an unmatched verdict (named anomaly), and
// the plain linear hop (silent) — plus idempotency.
func TestDefaultEdgeFallbackAnnotation(t *testing.T) {
	mkEdges := func() []entity.WorkflowEdge {
		return []entity.WorkflowEdge{
			{ID: "e-batched", From: "scale_gate", To: "contract_batch", Condition: &entity.WorkflowEdgeCondition{Field: "scale_verdict", Operator: "eq", Value: "batched"}},
			{ID: "e-linear", From: "scale_gate", To: "acceptance_test_design", Condition: &entity.WorkflowEdgeCondition{Field: "scale_verdict", Operator: "eq", Value: "linear"}},
			{ID: "e-default", From: "scale_gate", To: "acceptance_test_design", IsDefault: true},
		}
	}

	// 1. A conditional route matched: no annotation.
	taken, _ := chooseNextEdge(mkEdges(), "scale_gate", map[string]string{"scale_verdict": "batched"}, "")
	summary := annotateDefaultEdgeFallback("done", mkEdges(), "scale_gate", taken, map[string]string{"scale_verdict": "batched"})
	if summary != "done" {
		t.Fatalf("a matched conditional route must not be annotated, got %q", summary)
	}

	// 2. No verdict: the default edge fires and the summary gains the
	//    degradation note.
	taken, _ = chooseNextEdge(mkEdges(), "scale_gate", map[string]string{}, "")
	if !taken.IsDefault {
		t.Fatalf("fixture: no verdict must take the default edge, got %+v", taken)
	}
	summary = annotateDefaultEdgeFallback("gate done", mkEdges(), "scale_gate", taken, map[string]string{})
	if !strings.Contains(summary, defaultEdgeFallbackAnnotation) {
		t.Fatalf("fallback without a verdict must be annotated, got %q", summary)
	}
	if !strings.Contains(summary, "降级") {
		t.Fatalf("annotation must name the degradation, got %q", summary)
	}

	// 3. A verdict present but unmatched: the note names the stray value.
	taken, _ = chooseNextEdge(mkEdges(), "scale_gate", map[string]string{"scale_verdict": "weird"}, "")
	summary = annotateDefaultEdgeFallback("gate done", mkEdges(), "scale_gate", taken, map[string]string{"scale_verdict": "weird"})
	if !strings.Contains(summary, "weird") {
		t.Fatalf("a present-but-unmatched verdict must be named in the annotation, got %q", summary)
	}

	// 4. Idempotent: annotating an already-annotated summary changes nothing.
	once := summary
	twice := annotateDefaultEdgeFallback(once, mkEdges(), "scale_gate", taken, map[string]string{"scale_verdict": "weird"})
	if twice != once {
		t.Fatalf("annotation must be idempotent:\nonce=%q\ntwice=%q", once, twice)
	}

	// 5. A plain linear hop (no conditional edges at all) is silent.
	linear := []entity.WorkflowEdge{{ID: "e-next", From: "work", To: "next", IsDefault: true}}
	taken, _ = chooseNextEdge(linear, "work", map[string]string{}, "")
	summary = annotateDefaultEdgeFallback("worked", linear, "work", taken, map[string]string{})
	if summary != "worked" {
		t.Fatalf("a plain linear hop must not be annotated, got %q", summary)
	}
}
