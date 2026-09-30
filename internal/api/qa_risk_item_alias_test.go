package api

// C1 (2026-09 VM acceptance): QA agents habitually emit the risk-coverage
// matrix rows with case_id/result field names (borrowed from the
// test_spec_manifest vocabulary) instead of item_id/status — all 51 rows of
// the pilot run were rejected by the qa_signoff gate on that formatting
// drift alone. These tests pin the alias fallbacks in qaRiskItem
// UnmarshalJSON: case_id and result are accepted as fallbacks, the canonical
// item_id/status names win when both are present, the result→status verb
// mapping covers the observed spellings, and unknown result values stay
// rejected by the gate's strict status check (the aliasing widens field
// NAME acceptance, never the set of statuses the gate lets through).

import (
	"encoding/json"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// currentQASignoffStep builds the qa_signoff step entity the gate expects
// (legacy ID matching resolves it to the QA sign-off gate kind).
func currentQASignoffStep() entity.WorkflowStep {
	return entity.WorkflowStep{ID: "qa_signoff", Title: "QA Sign-off", Type: "human_review"}
}

func unmarshalQARiskItem(t *testing.T, raw string) qaRiskItem {
	t.Helper()
	var item qaRiskItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal qaRiskItem: %v", err)
	}
	return item
}

func TestQARiskItemCaseIDAndResultAliases(t *testing.T) {
	item := unmarshalQARiskItem(t, `{"case_id":"AUTH-001","title":"login","risk_level":"high","result":"pass","evidence":"TestLogin green"}`)
	if item.ItemID != "AUTH-001" {
		t.Errorf("case_id must fall back to ItemID, got %q", item.ItemID)
	}
	if item.Status != "passed" {
		t.Errorf("result \"pass\" must map to Status passed, got %q", item.Status)
	}
	if item.Evidence != "TestLogin green" {
		t.Errorf("evidence must survive alias parsing, got %q", item.Evidence)
	}
}

func TestQAResultStatusMapping(t *testing.T) {
	cases := map[string]string{
		"pass":                  "passed",
		"pass_with_discrepancy": "passed",
		"fail":                  "failed",
		"failed":                "failed",
		"blocked":               "blocked",
		"block":                 "blocked",
		"waived":                "waived",
		"waiver":                "waived",
		"unexecuted":            "unexecuted",
		"not_verified":          "unexecuted",
		"not_run":               "unexecuted",
		"skipped":               "skipped",
		"skip":                  "skipped",
	}
	for raw, want := range cases {
		item := unmarshalQARiskItem(t, `{"item_id":"A1","result":"`+raw+`"}`)
		if item.Status != want {
			t.Errorf("result %q: want status %q, got %q", raw, want, item.Status)
		}
	}
}

func TestQAResultUnknownValueStaysRejected(t *testing.T) {
	// An unknown result verb must pass through unchanged so the gate's
	// strict status switch rejects it — aliasing must not widen the status
	// vocabulary.
	item := unmarshalQARiskItem(t, `{"item_id":"A1","result":"sort_of_ok"}`)
	if item.Status != "sort_of_ok" {
		t.Errorf("unknown result must pass through, got %q", item.Status)
	}
}

func TestQARiskItemCanonicalFieldsWinOverAliases(t *testing.T) {
	item := unmarshalQARiskItem(t, `{"item_id":"A1","case_id":"AUTH-001","status":"failed","result":"pass"}`)
	if item.ItemID != "A1" {
		t.Errorf("item_id must win over case_id, got %q", item.ItemID)
	}
	if item.Status != "failed" {
		t.Errorf("status must win over result, got %q", item.Status)
	}
}

func TestQARiskItemCaseIDWinsOverLegacyID(t *testing.T) {
	// Priority order is historical: item_id > id > case_id.
	item := unmarshalQARiskItem(t, `{"id":"legacy-id","case_id":"AUTH-001"}`)
	if item.ItemID != "legacy-id" {
		t.Errorf("id must win over case_id, got %q", item.ItemID)
	}
}

func TestQARiskItemResultLosesToCoverageStatusWhenBothAliased(t *testing.T) {
	// coverage_status is the older alias; result sits between status and
	// coverage_status in the fallback chain.
	item := unmarshalQARiskItem(t, `{"item_id":"A1","result":"pass","coverage_status":"failed"}`)
	if item.Status != "passed" {
		t.Errorf("result must be consulted before coverage_status, got %q", item.Status)
	}
}

// End-to-end through the gate: a case_id/result matrix parses and PASSES the
// qa_signoff validation, and an unknown result verb still rejects.
func TestValidateQASignoffGateAcceptsCaseIDResultMatrix(t *testing.T) {
	s, _, task, wfStore, run := c1SeedQASignoffRun(t, false)
	_ = s
	_ = task

	outputs := map[string]string{
		"decision": "approve",
		"risk_coverage_matrix": `[
			{"case_id":"AUTH-001","risk_level":"high","result":"pass","evidence":"TestLogin green"},
			{"case_id":"AUTH-002","risk_level":"low","result":"fail"}
		]`,
	}
	if err := validateQASignoffGate(outputs, currentQASignoffStep(), run, wfStore); err != nil {
		t.Fatalf("case_id/result matrix must pass the qa_signoff gate: %v", err)
	}
}

func TestValidateQASignoffGateStillRejectsUnknownResult(t *testing.T) {
	s, _, task, wfStore, run := c1SeedQASignoffRun(t, false)
	_ = s
	_ = task

	outputs := map[string]string{
		"decision": "approve",
		"risk_coverage_matrix": `[
			{"case_id":"AUTH-001","risk_level":"high","result":"sort_of_ok","evidence":"trust me"}
		]`,
	}
	err := validateQASignoffGate(outputs, currentQASignoffStep(), run, wfStore)
	if err == nil {
		t.Fatal("unknown result verb must still be rejected by the gate")
	}
}
