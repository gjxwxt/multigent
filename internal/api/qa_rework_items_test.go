package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Batch B-a: on a qa_signoff rejection, the platform aggregates every
// failed/blocked/unexecuted matrix item into the structured qa_rework_items
// output, enriched with the acceptance baseline's expected_result from the
// acceptance_test_design manifest. The e-qa-rework edge forwards it to the
// implementation step as machine-readable fix targets.
func TestQASignoffRejectionAggregatesReworkItems(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "rework items test")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, nil, map[string]string{
		"request": "build the thing",
		"context": "rework traceability",
	}); err != nil {
		t.Fatal(err)
	}
	// Fast-forward the run to qa_signoff (the aggregation lives in the
	// signoff rejection enrichment; the full pipeline is covered by the
	// workflow package E2E).
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatal("run not found")
	}
	run.ActiveStepID = "qa_signoff"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}

	// qa outputs land as the signoff step's inputs (e-qa-to-signoff mapping);
	// the acceptance baseline lives in the acceptance_test_design outputs.
	matrix := `[
	 {"item_id":"AUTH-001","acceptance_criteria":"expired token yields 401","risk_level":"high","status":"failed","evidence":"returned 200"},
	 {"item_id":"UI-003","risk_level":"low","status":"blocked","uncovered_reason":"staging env down"}
	]`
	manifest := `[{"case_id":"AUTH-001","ac_id":"AC-1","risk_level":"high","automation_level":"api_integration","execution_type":"auto","expected_result":"expired token yields 401 unauthorized"}]`
	qaInst := workflowStepInstanceByStepIDMust(t, wfStore, run.ID, "qa_signoff")
	qaInst.InputValues = map[string]string{
		"risk_coverage_matrix": matrix,
	}
	if err := wfStore.SaveStepInstance(qaInst); err != nil {
		t.Fatal(err)
	}
	atdInst := workflowStepInstanceByStepIDMust(t, wfStore, run.ID, "acceptance_test_design")
	atdInst.OutputValues = map[string]string{"test_spec_manifest": manifest}
	if err := wfStore.SaveStepInstance(atdInst); err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{"decision": "request_changes"}
	enrichQARejectionComments(outputs, entity.WorkflowStep{ID: "qa_signoff", Type: "human_review"}, run, wfStore)

	raw := strings.TrimSpace(outputs["qa_rework_items"])
	if raw == "" {
		t.Fatal("rejection must produce structured qa_rework_items")
	}
	var items []struct {
		ItemID          string `json:"item_id"`
		RiskLevel       string `json:"risk_level"`
		Status          string `json:"status"`
		ExpectedResult  string `json:"expected_result"`
		UncoveredReason string `json:"uncovered_reason"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("qa_rework_items must be valid JSON: %v (%s)", err, raw)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 rework items, got %d (%s)", len(items), raw)
	}
	if items[0].ItemID != "AUTH-001" || items[0].Status != "failed" || items[0].RiskLevel != "high" {
		t.Fatalf("failed item not aggregated correctly: %+v", items[0])
	}
	if items[0].ExpectedResult != "expired token yields 401 unauthorized" {
		t.Fatalf("expected_result from the manifest must enrich the rework item: %+v", items[0])
	}
	if items[1].ItemID != "UI-003" || items[1].Status != "blocked" || items[1].UncoveredReason == "" {
		t.Fatalf("blocked item not aggregated: %+v", items[1])
	}
	if !strings.Contains(outputs["comments"], "QA 准出未通过") {
		t.Fatalf("human-readable comment block must still be present: %q", outputs["comments"])
	}
}

func workflowStepInstanceByStepIDMust(t *testing.T, wfStore *workflowstore.Store, runID, stepID string) *entity.WorkflowStepInstance {
	t.Helper()
	instances, err := wfStore.ListStepInstances(runID)
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := workflowStepInstanceByStepID(instances, stepID)
	if !ok {
		t.Fatalf("step instance %q missing", stepID)
	}
	return &inst
}
