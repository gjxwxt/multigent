package workflow

import (
	"strings"
	"testing"
)

func TestBuildDeliveryPlanFromRunOutputs(t *testing.T) {
	anchors := `[{"id":"uc-1","text":"clear local state","source":"requirements.md"}]`
	plan := `{"planId":"p1","workPackages":[{"id":"wp-a","title":"A","acceptanceCriteria":["uc-1"],"agentBinding":"pm"}]}`

	built, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{plan}, []string{anchors})
	if err != nil {
		t.Fatalf("valid plan + anchors must build: %v", err)
	}
	if built.PlanID != "p1" || built.RunID != "run-1" || len(built.RequirementItems) != 1 {
		t.Fatalf("built plan incomplete: %+v", built)
	}

	// The newest non-empty carrier is the plan under review: if it is broken
	// the freeze is refused rather than silently falling back to an older draft
	// the reviewer never saw.
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{"garbage", plan}, []string{anchors}); err == nil ||
		!strings.Contains(err.Error(), "not parseable") {
		t.Fatalf("a broken newest candidate must refuse the freeze, got %v", err)
	}
	// Empty carriers are skipped: a later populated carrier still wins.
	built, err = BuildDeliveryPlanFromRunOutputs("run-1", []string{"", "   ", plan}, []string{anchors})
	if err != nil || built.PlanID != "p1" {
		t.Fatalf("empty carriers must be skipped: %+v err=%v", built, err)
	}
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{"garbage"}, []string{anchors}); err == nil {
		t.Fatal("an unparseable plan source must fail closed")
	}
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", nil, []string{anchors}); err == nil {
		t.Fatal("a missing plan source must fail closed")
	}
}

func TestBuildDeliveryPlanRequirementAnchorRules(t *testing.T) {
	anchors := `[{"id":"uc-1","text":"clear local state","source":"requirements.md"}]`
	nonInfra := `{"workPackages":[{"id":"wp-a","title":"A","acceptanceCriteria":["uc-1"],"agentBinding":"pm"}]}`
	infraOnly := `{"workPackages":[{"id":"wp-infra","title":"Scaffolding","acceptanceCriteria":["infra:repo-bootstrap"],"agentBinding":"pm"}]}`

	// Missing anchor snapshot: fail-closed for requirement-linked plans...
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{nonInfra}, nil); err == nil {
		t.Fatal("a plan tracing to requirements must carry the frozen requirement snapshot")
	}
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{nonInfra}, []string{"[]"}); err == nil {
		t.Fatal("an empty requirement snapshot must be refused for requirement-linked plans")
	}
	// ...but an infrastructure-only plan may omit it (infra packages carry no
	// requirement anchors; 1:1:1:1 still holds for them).
	built, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{infraOnly}, nil)
	if err != nil {
		t.Fatalf("infra-only plans must not require a requirement snapshot: %v", err)
	}
	if len(built.RequirementItems) != 0 || len(built.WorkPackages) != 1 {
		t.Fatalf("infra-only plan shape changed: %+v", built)
	}

	// The wrapper accepts both a bare array and the {"items":[...]} envelope.
	for _, raw := range []string{anchors, `{"items":[{"id":"uc-1","text":"clear local state"}]}`} {
		if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{nonInfra}, []string{raw}); err != nil {
			t.Fatalf("anchor payload %q must parse: %v", raw, err)
		}
	}
	// Blank refs must not make an infra-only package look requirement-linked.
	blankInfra := `{"workPackages":[{"id":"wp-infra","title":"Scaffolding","acceptanceCriteria":["infra:repo-bootstrap","","  "],"agentBinding":"pm"}]}`
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{blankInfra}, nil); err == nil {
		t.Fatal("blank acceptance refs must not bypass the infra-only exemption")
	}
	// A reference to an item that is not in the frozen snapshot fails closed.
	dangling := `{"workPackages":[{"id":"wp-a","title":"A","acceptanceCriteria":["uc-404"],"agentBinding":"pm"}]}`
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{dangling}, []string{anchors}); err == nil ||
		!strings.Contains(err.Error(), "unknown requirement item") {
		t.Fatalf("dangling requirement references must be refused, got %v", err)
	}
}
