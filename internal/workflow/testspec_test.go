package workflow

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// --- §8 template structure tests ---

func findStep(t *testing.T, tmpl entity.WorkflowTemplate, id string) entity.WorkflowStep {
	t.Helper()
	for _, s := range tmpl.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("step %q not found in template %s", id, tmpl.ID)
	return entity.WorkflowStep{}
}

func TestGreenfieldAcceptanceTestDesignStructure(t *testing.T) {
	for _, locale := range []string{"en", "zh-CN"} {
		tmpl, ok := Template("greenfield-delivery-pipeline", locale)
		if !ok {
			t.Fatalf("greenfield template not registered (%s)", locale)
		}
		atd := findStep(t, tmpl, "acceptance_test_design")
		if atd.Type != "agent_task" {
			t.Fatalf("acceptance_test_design must be an agent_task, got %s", atd.Type)
		}
		if atd.ActorRole != "qa-agent" {
			t.Fatalf("acceptance_test_design must be qa-agent-owned, got %s", atd.ActorRole)
		}
		outNames := map[string]bool{}
		for _, f := range atd.OutputFields {
			outNames[f.Name] = true
		}
		for _, want := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
			if !outNames[want] {
				t.Fatalf("%s: acceptance_test_design must output %s", locale, want)
			}
		}
		// Position: between design_review and implementation on the canvas.
		design := findStep(t, tmpl, "design_review")
		impl := findStep(t, tmpl, "implementation")
		if !(design.Position.X < atd.Position.X && atd.Position.X < impl.Position.X) {
			t.Fatalf("%s: acceptance_test_design must sit between design_review (%d) and implementation (%d), got %d",
				locale, design.Position.X, impl.Position.X, atd.Position.X)
		}
		// Edges: design approve -> ATD; ATD -> implementation (default).
		var designToATD, atdToImpl bool
		for _, e := range tmpl.Edges {
			if e.From == "design_review" && e.To == "acceptance_test_design" && e.Condition != nil &&
				e.Condition.Field == "decision" && e.Condition.Value == "approve" {
				designToATD = true
			}
			if e.From == "acceptance_test_design" && e.To == "implementation" && e.IsDefault {
				atdToImpl = true
				for _, key := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
					if e.InputMapping[key] != "$output."+key {
						t.Fatalf("%s: e-atd-impl must forward %s to implementation", locale, key)
					}
				}
			}
		}
		if !designToATD {
			t.Fatalf("%s: design approve edge must route to acceptance_test_design", locale)
		}
		if !atdToImpl {
			t.Fatalf("%s: acceptance_test_design must flow into implementation by default", locale)
		}
		// No legacy direct edge design_review -> implementation may survive.
		for _, e := range tmpl.Edges {
			if e.From == "design_review" && e.To == "implementation" {
				t.Fatalf("%s: legacy design_review->implementation edge must be removed", locale)
			}
		}
	}
}

func TestGreenfieldReworkPreservesTestSpec(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	// Every rework edge into implementation must forward the spec fields so
	// the developer never loses the acceptance baseline (plan §5.2).
	for _, e := range tmpl.Edges {
		if e.To != "implementation" || e.From == "acceptance_test_design" {
			continue
		}
		for _, key := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
			if got := e.InputMapping[key]; got != "$input."+key {
				t.Fatalf("rework edge %s (%s->implementation) must forward %s from its input, got %q", e.ID, e.From, key, got)
			}
		}
	}
}

func TestGreenfieldQAConsumesSpec(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	qa := findStep(t, tmpl, "qa")
	inNames := map[string]bool{}
	for _, f := range qa.InputFields {
		inNames[f.Name] = true
	}
	for _, want := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary", "test_implementation_evidence"} {
		if !inNames[want] {
			t.Fatalf("qa step must consume %s", want)
		}
	}
	// The e-code-review-qa edge must forward the spec AND the developer
	// evidence mapping — QA reconciles spec vs diff vs evidence (plan §5.3).
	for _, e := range tmpl.Edges {
		if e.ID == "e-code-review-qa" {
			if e.InputMapping["test_spec_doc"] != "$input.test_spec_doc" || e.InputMapping["test_spec_manifest"] != "$input.test_spec_manifest" {
				t.Fatal("code_review -> qa must forward the test spec")
			}
			if e.InputMapping["test_implementation_evidence"] != "$input.test_implementation_evidence" {
				t.Fatal("code_review -> qa must forward the developer evidence mapping")
			}
		}
		// Batch B-a: the QA rework edge must carry the platform-aggregated
		// structured rework list into implementation.
		if e.ID == "e-qa-rework" {
			if e.InputMapping["qa_rework_items"] != "$output.qa_rework_items" {
				t.Fatal("qa_signoff rework must forward qa_rework_items to implementation")
			}
		}
	}
	desc := qa.Description
	if !strings.Contains(desc, "test_implementation_evidence") {
		t.Fatal("qa description must reference reconciling the developer mapping")
	}
}

// The happy path must keep the spec intact end to end: implementation ->
// self_review -> code_review -> qa. Dropping a hop silently starves QA of
// its required inputs on first pass (regression found by the E2E test).
func TestGreenfieldHappyPathForwardsTestSpec(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	expect := map[string][]string{
		"e-impl-self-review": {"test_spec_doc", "test_spec_manifest", "test_spec_summary", "test_implementation_evidence"},
		"e-self-review-pass": {"test_spec_doc", "test_spec_manifest", "test_spec_summary", "test_implementation_evidence"},
		"e-code-review-qa":   {"test_spec_doc", "test_spec_manifest", "test_spec_summary", "test_implementation_evidence"},
		"e-qa-to-signoff":    {"test_spec_doc", "test_spec_manifest", "test_spec_summary"},
	}
	for _, e := range tmpl.Edges {
		want, ok := expect[e.ID]
		if !ok {
			continue
		}
		for _, key := range want {
			if got := e.InputMapping[key]; got != "$input."+key && got != "$output."+key {
				t.Fatalf("edge %s must forward %s, got %q", e.ID, key, got)
			}
		}
	}
}

func TestGreenfieldImplementationConsumesSpec(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	impl := findStep(t, tmpl, "implementation")
	inNames := map[string]bool{}
	for _, f := range impl.InputFields {
		inNames[f.Name] = true
	}
	for _, want := range []string{"test_spec_doc", "test_spec_manifest", "test_spec_summary"} {
		if !inNames[want] {
			t.Fatalf("implementation step must consume %s", want)
		}
	}
	outNames := map[string]bool{}
	for _, f := range impl.OutputFields {
		outNames[f.Name] = true
	}
	if !outNames["test_implementation_evidence"] {
		t.Fatal("implementation must produce test_implementation_evidence")
	}
}

// --- §8 manifest validation tests ---

func validManifestJSON() string {
	return `[
	 {"case_id":"AUTH-001","ac_id":"AC-1","risk_level":"high","automation_level":"api_integration","execution_type":"auto","expected_result":"401 with code unauthorized when the token is expired"},
	 {"case_id":"AUTH-002","ac_id":"AC-1","risk_level":"medium","automation_level":"manual","execution_type":"manual","expected_result":"operator sees the session-expired banner and is redirected to login"}
	]`
}

func TestValidateTestSpecManifestAccepts(t *testing.T) {
	cases, err := ValidateTestSpecManifest(validManifestJSON())
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if len(cases) != 2 || cases[0].CaseID != "AUTH-001" {
		t.Fatalf("unexpected parse: %+v", cases)
	}
}

func TestValidateTestSpecManifestRejects(t *testing.T) {
	base := map[string]string{
		"case_id": "AUTH-001", "ac_id": "AC-1", "risk_level": "high",
		"automation_level": "unit", "execution_type": "auto", "expected_result": "returns 200 with the created id",
	}
	build := func(mutate func(m map[string]string)) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		mutate(m)
		return `[{"case_id":"` + m["case_id"] + `","ac_id":"` + m["ac_id"] + `","risk_level":"` + m["risk_level"] + `","automation_level":"` + m["automation_level"] + `","execution_type":"` + m["execution_type"] + `","expected_result":"` + m["expected_result"] + `"}]`
	}
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty payload", "", "empty"},
		{"not json", `not json`, "not a valid JSON array"},
		{"empty array", `[]`, "no cases"},
		{"missing case_id", build(func(m map[string]string) { m["case_id"] = "" }), "case_id is required"},
		{"missing ac_id", build(func(m map[string]string) { m["ac_id"] = "" }), "ac_id is required"},
		{"bad risk", build(func(m map[string]string) { m["risk_level"] = "critical" }), "risk_level"},
		{"bad automation", build(func(m map[string]string) { m["automation_level"] = "e2e" }), "automation_level"},
		{"bad execution", build(func(m map[string]string) { m["execution_type"] = "sometimes" }), "execution_type"},
		{"empty expected", build(func(m map[string]string) { m["expected_result"] = "" }), "expected_result is required"},
		{"placeholder expected (en)", build(func(m map[string]string) { m["expected_result"] = "To be observed at runtime" }), "placeholder"},
		{"placeholder expected (zh)", build(func(m map[string]string) { m["expected_result"] = "视情况而定" }), "placeholder"},
		{"duplicate ids", `[{"case_id":"A","ac_id":"x","risk_level":"low","automation_level":"unit","execution_type":"auto","expected_result":"ok"},{"case_id":"A","ac_id":"y","risk_level":"low","automation_level":"unit","execution_type":"auto","expected_result":"ok"}]`, "duplicate case_id"},
	}
	for _, tc := range cases {
		_, err := ValidateTestSpecManifest(tc.raw)
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q must mention %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateTestSpecManifestCaseLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 501; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"case_id":"C-` + strings.Repeat("x", 1) + `","ac_id":"a","risk_level":"low","automation_level":"unit","execution_type":"auto","expected_result":"ok"}`)
	}
	sb.WriteString("]")
	if _, err := ValidateTestSpecManifest(sb.String()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500-case ceiling rejection, got %v", err)
	}
}
