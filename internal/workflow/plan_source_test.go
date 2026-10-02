package workflow

import (
	"fmt"
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

// The freeze path must carry the agent's dependsKind through to the frozen
// plan — both the map shape the template documents and the bare-string
// shorthand models actually emit. Losing the contract edge at build time is
// the freeze-path twin of the DependsKindMap decode defect (wfr-kwm2nckn).
func TestBuildDeliveryPlanCarriesDependsKind(t *testing.T) {
	anchors := `[{"id":"uc-1","text":"export endpoint","source":"requirements.md"}]`
	mapShape := `{"planId":"p1","workPackages":[
		{"id":"wp-1","title":"Frontend","acceptanceCriteria":["uc-1"],"agentBinding":"a"},
		{"id":"wp-2","title":"Backend","dependsOn":["wp-1"],"dependsKind":{"wp-1":"contract"},"acceptanceCriteria":["uc-1"],"agentBinding":"a"},
		{"id":"wp-3","title":"Tests","dependsOn":["wp-2"],"dependsKind":{"wp-2":"artifact"},"acceptanceCriteria":["uc-1"],"agentBinding":"a"}
	]}`
	built, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{mapShape}, []string{anchors})
	if err != nil {
		t.Fatalf("map-shape dependsKind must build: %v", err)
	}
	if built.WorkPackages[1].DependsKind["wp-1"] != PlanDepKindContract {
		t.Fatalf("contract edge lost at freeze: %+v", built.WorkPackages[1].DependsKind)
	}
	if built.WorkPackages[2].DependsKind["wp-2"] != PlanDepKindArtifact {
		t.Fatalf("artifact edge lost at freeze: %+v", built.WorkPackages[2].DependsKind)
	}

	// Shorthand: a bare "contract" applies to every edge of the package.
	shorthand := `{"planId":"p1","workPackages":[
		{"id":"wp-1","title":"Frontend","acceptanceCriteria":["uc-1"],"agentBinding":"a"},
		{"id":"wp-2","title":"Backend","dependsOn":["wp-1"],"dependsKind":"contract","acceptanceCriteria":["uc-1"],"agentBinding":"a"}
	]}`
	built, err = BuildDeliveryPlanFromRunOutputs("run-1", []string{shorthand}, []string{anchors})
	if err != nil {
		t.Fatalf("string-shorthand dependsKind must build: %v", err)
	}
	if built.WorkPackages[1].DependsKind["wp-1"] != PlanDepKindContract {
		t.Fatalf("shorthand contract edge lost at freeze: %+v", built.WorkPackages[1].DependsKind)
	}

	// An unknown kind must be refused at build time, not silently dropped.
	bad := `{"planId":"p1","workPackages":[
		{"id":"wp-1","title":"A","acceptanceCriteria":["uc-1"],"agentBinding":"a"},
		{"id":"wp-2","title":"B","dependsOn":["wp-1"],"dependsKind":"bond","acceptanceCriteria":["uc-1"],"agentBinding":"a"}
	]}`
	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{bad}, []string{anchors}); err == nil ||
		!strings.Contains(err.Error(), "dependsKind") {
		t.Fatalf("unknown dependsKind must fail closed, got %v", err)
	}
}

// sharedContract arrives in two shapes: the documented object array and the
// bare string array models reliably emit (contract ids already committed by
// contract_batch). Both must freeze; the string form carries id=artifact.
func TestBuildDeliveryPlanSharedContractShapes(t *testing.T) {
	anchors := `[{"id":"uc-1","text":"export endpoint","source":"requirements.md"}]`
	base := `{"planId":"p1","sharedContract":%s,"workPackages":[
		{"id":"wp-1","title":"Frontend","acceptanceCriteria":["uc-1"],"agentBinding":"a","contractRefs":["infra:app-skeleton"]}
	]}`

	objects := `[{"id":"infra:app-skeleton","artifact":"repo skeleton","path":"/"}]`
	built, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{fmt.Sprintf(base, objects)}, []string{anchors})
	if err != nil {
		t.Fatalf("object-array sharedContract must build: %v", err)
	}
	if len(built.SharedContract) != 1 || built.SharedContract[0].Artifact != "repo skeleton" {
		t.Fatalf("object shape lost: %+v", built.SharedContract)
	}

	stringsShape := `["infra:app-skeleton","contract:markdown-table-columns"]`
	built, err = BuildDeliveryPlanFromRunOutputs("run-1", []string{fmt.Sprintf(base, stringsShape)}, []string{anchors})
	if err != nil {
		t.Fatalf("string-array sharedContract must build: %v", err)
	}
	// canonicalization sorts by id; assert by content.
	if len(built.SharedContract) != 2 {
		t.Fatalf("string shape dropped entries: %+v", built.SharedContract)
	}
	byID := map[string]PlanContractItem{}
	for _, c := range built.SharedContract {
		byID[c.ID] = c
	}
	c := byID["infra:app-skeleton"]
	if c.ID != "infra:app-skeleton" || c.Artifact != "infra:app-skeleton" {
		t.Fatalf("string shape mistranslated: %+v", built.SharedContract)
	}

	if _, err := BuildDeliveryPlanFromRunOutputs("run-1", []string{fmt.Sprintf(base, `42`)}, []string{anchors}); err == nil {
		t.Fatal("a non-array sharedContract must fail closed")
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
