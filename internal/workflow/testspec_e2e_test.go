package workflow

import (
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// newGreenfieldRunStore opens a control DB + store and instantiates the
// greenfield template as a run for one task (requirement -> ... -> QA).
func newGreenfieldRunStore(t *testing.T) (*Store, string) {
	t.Helper()
	controlDB, err := controldb.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatalf("open control db: %v", err)
	}
	t.Cleanup(func() { controlDB.Close() })
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "workspace-gf", Name: "GF", Slug: "gf", Root: t.TempDir()}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	store := NewStore(controlDB, "workspace-gf")
	def, ok := DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "GF vNext")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := store.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		"pm-agent":        {Type: "agent", ID: "pm"},
		"product-owner":   {Type: "user", ID: "po"},
		"qa-agent":        {Type: "agent", ID: "qa"},
		"developer-agent": {Type: "agent", ID: "dev"},
		"reviewer-agent":  {Type: "agent", ID: "reviewer"},
		"owner-engineer":  {Type: "user", ID: "eng"},
		"qa-owner":        {Type: "user", ID: "qaowner"},
		"release-agent":   {Type: "agent", ID: "rel"},
		// Large-requirement module S1: scale gate + batched-path bindings.
		"workstream-agent-1": {Type: "agent", ID: "ws1"},
		"workstream-agent-2": {Type: "agent", ID: "ws2"},
	}
	if _, _, err := store.StartRun("project", "task-gf-e2e", def.ID, bindings); err != nil {
		t.Fatalf("start run: %v", err)
	}
	return store, "task-gf-e2e"
}

func gfStepInstance(t *testing.T, store *Store, taskID, stepID string) *entity.WorkflowStepInstance {
	t.Helper()
	run, ok, err := store.RunForTask("project", taskID)
	if err != nil || !ok {
		t.Fatalf("run for task: ok=%v err=%v", ok, err)
	}
	insts, err := store.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range insts {
		if insts[i].StepID == stepID {
			return &insts[i]
		}
	}
	t.Fatalf("step instance %q missing", stepID)
	return nil
}

const gfManifest = `[
 {"case_id":"AUTH-001","ac_id":"AC-1","risk_level":"high","automation_level":"api_integration","execution_type":"auto","expected_result":"expired token yields 401 unauthorized"},
 {"case_id":"UI-001","ac_id":"AC-2","risk_level":"low","automation_level":"manual","execution_type":"manual","expected_result":"operator sees the confirm banner"}
]`

// §8 e2e: requirement review -> design review -> acceptance_test_design ->
// implementation -> self_review -> code_review -> qa -> qa_signoff rework,
// asserting the manifest gate, spec forwarding to implementation and QA, and
// that the QA rework carries the failed items plus the original spec.
func TestGreenfieldE2EWithAcceptanceTestDesign(t *testing.T) {
	store, taskID := newGreenfieldRunStore(t)
	const project = "project"

	// 1. requirement_draft completes.
	if _, err := store.CompleteAndAdvance(project, taskID, "drafted", "", map[string]string{
		"requirement_draft": "Structured requirement with AC-1/AC-2",
		"open_questions":    "none",
	}, "completed"); err != nil {
		t.Fatalf("req draft: %v", err)
	}
	// 2. requirement_review approves.
	if _, err := store.CompleteAndAdvance(project, taskID, "approved", "", map[string]string{
		"decision":             "approve",
		"comments":             "looks right",
		"approved_requirement": "Approved requirement text",
	}, "completed"); err != nil {
		t.Fatalf("req review: %v", err)
	}
	// 3. design_review approves.
	if _, err := store.CompleteAndAdvance(project, taskID, "design ok", "", map[string]string{
		"decision": "approve",
		"comments": "ship it",
	}, "completed"); err != nil {
		t.Fatalf("design review: %v", err)
	}
	// 3b. scale_gate verdict: linear (large-requirement module S1) — routes
	// straight to acceptance_test_design on the historical single-track path.
	if _, err := store.CompleteAndAdvance(project, taskID, "small requirement, linear", "", map[string]string{
		"scale_verdict": "linear",
	}, "completed"); err != nil {
		t.Fatalf("scale gate linear: %v", err)
	}
	// 4. acceptance_test_design with an INVALID manifest must abort (no
	// routing) and keep the step pending.
	_, err := store.CompleteAndAdvance(project, taskID, "spec", "", map[string]string{
		"test_spec_doc":      "doc",
		"test_spec_manifest": `[{"case_id":"","risk_level":"nope","expected_result":"待观察"}]`,
		"test_spec_summary":  "1 case",
	}, "completed")
	if err == nil {
		t.Fatal("invalid manifest must block completion")
	}
	inst := gfStepInstance(t, store, taskID, "acceptance_test_design")
	if inst.Status != "pending" {
		t.Fatalf("step must stay pending after manifest rejection, got %s", inst.Status)
	}
	if len(inst.OutputValues) != 0 {
		t.Fatalf("rejected output must not persist, got %v", inst.OutputValues)
	}
	// 5. Valid manifest completes and lands on implementation with the spec.
	tr, err := store.CompleteAndAdvance(project, taskID, "spec done", "", map[string]string{
		"test_spec_doc":      "full spec",
		"test_spec_manifest": gfManifest,
		"test_spec_summary":  "2 cases: 1 high 1 low",
	}, "completed")
	if err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	if tr.Next == nil || tr.Next.ID != "implementation" {
		t.Fatalf("expected next=implementation, got %+v", tr.Next)
	}
	inst = gfStepInstance(t, store, taskID, "implementation")
	if inst.InputValues["test_spec_manifest"] != gfManifest {
		t.Fatalf("implementation lost the original manifest: %q", inst.InputValues["test_spec_manifest"])
	}
	if inst.InputValues["test_spec_doc"] != "full spec" {
		t.Fatalf("implementation lost the spec doc: %q", inst.InputValues["test_spec_doc"])
	}
	// 6. implementation completes with the evidence mapping.
	if _, err := store.CompleteAndAdvance(project, taskID, "impl", "", map[string]string{
		"pr":                           "diff summary",
		"tests_run":                    "go test ./...",
		"risks":                        "none",
		"test_implementation_evidence": `{"AUTH-001":{"test":"TestExpiredToken","file":"auth_test.go","result":"pass"}}`,
	}, "completed"); err != nil {
		t.Fatalf("implementation: %v", err)
	}
	// 7. self_review passes.
	if _, err := store.CompleteAndAdvance(project, taskID, "self review", "", map[string]string{
		"self_review_verdict": "pass",
		"review_comments":     "clean",
	}, "completed"); err != nil {
		t.Fatalf("self review: %v", err)
	}
	// self_review must have received the spec + developer evidence.
	inst = gfStepInstance(t, store, taskID, "self_review")
	if inst.OutputValues["self_review_verdict"] != "pass" {
		t.Fatalf("self review verdict not persisted: %v", inst.OutputValues)
	}
	if inst.InputValues["test_spec_manifest"] != gfManifest {
		t.Fatalf("self_review did not receive the spec: %q", inst.InputValues["test_spec_manifest"])
	}
	if !strings.Contains(inst.InputValues["test_implementation_evidence"], "TestExpiredToken") {
		t.Fatalf("self_review did not receive developer evidence: %q", inst.InputValues["test_implementation_evidence"])
	}
	// 8. code_review approves -> qa.
	if _, err := store.CompleteAndAdvance(project, taskID, "cr ok", "", map[string]string{
		"decision":        "approve",
		"comments":        "lgtm",
		"approved_change": "approved diff",
	}, "completed"); err != nil {
		t.Fatalf("code review: %v", err)
	}
	// QA receives the spec AND the developer evidence (reconciliation §5.3).
	inst = gfStepInstance(t, store, taskID, "qa")
	if inst.InputValues["test_spec_manifest"] != gfManifest {
		t.Fatalf("qa must receive the original manifest, got %q", inst.InputValues["test_spec_manifest"])
	}
	if !strings.Contains(inst.InputValues["test_implementation_evidence"], "TestExpiredToken") {
		t.Fatalf("qa did not receive developer evidence: %q", inst.InputValues["test_implementation_evidence"])
	}
	// 9. qa produces the matrix and declares its touched paths (QA checkpoint
	// gate, Batch B-b: only test artifacts).
	matrix := `[{"item_id":"AUTH-001","acceptance_criteria":"AC-1","risk_level":"high","status":"failed","evidence":"expired token returned 200"}]`
	if _, err := store.CompleteAndAdvance(project, taskID, "qa done", "", map[string]string{
		"risk_coverage_matrix": matrix,
		"test_report":          "AUTH-001 failed",
		"touched_paths":        "server/store/store_test.go\nweb/e2e/auth.spec.ts",
	}, "completed"); err != nil {
		t.Fatalf("qa: %v", err)
	}
	// 10. qa_signoff requests rework — implementation must receive BOTH the
	// failed-item context and the original spec reference.
	signoffOutputs := map[string]string{
		"decision": "request_changes",
		"comments": "AUTH-001 failed: expired token must yield 401",
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "rework", "", signoffOutputs, "completed"); err != nil {
		t.Fatalf("qa signoff rework: %v", err)
	}
	inst = gfStepInstance(t, store, taskID, "implementation")
	if inst.InputValues["test_spec_manifest"] != gfManifest {
		t.Fatalf("reworked implementation lost the original manifest: %q", inst.InputValues["test_spec_manifest"])
	}
	if inst.InputValues["test_spec_doc"] != "full spec" {
		t.Fatalf("reworked implementation lost the spec doc: %q", inst.InputValues["test_spec_doc"])
	}
	if inst.InputValues["test_spec_summary"] != "2 cases: 1 high 1 low" {
		t.Fatalf("reworked implementation lost the spec summary: %q", inst.InputValues["test_spec_summary"])
	}
	if !strings.Contains(inst.InputValues["review_comments"], "AUTH-001 failed") {
		t.Fatalf("reworked implementation lost the failed-item context: %q", inst.InputValues["review_comments"])
	}
	_ = signoffOutputs
}
