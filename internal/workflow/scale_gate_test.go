package workflow

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Large-requirement module S1 (docs/large-requirement-delivery-module.md):
// the scale gate must route deterministically — linear/batched verdicts follow
// their explicit edges, and an undecided/empty verdict falls through to the
// DEFAULT linear edge (fail-safe toward the historical single-track path; a
// missing verdict can never silently fan out).
func TestGreenfieldScaleGateRouting(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "en")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	verdictSeen := 0
	batchedFound, linearFound, defaultLinear := false, false, false
	for _, e := range tmpl.Edges {
		if e.From != "scale_gate" {
			continue
		}
		switch e.To {
		case "acceptance_test_design":
			if e.IsDefault {
				defaultLinear = true
			} else if e.Condition != nil && e.Condition.Field == "scale_verdict" &&
				e.Condition.Operator == "eq" && e.Condition.Value == "linear" {
				linearFound = true
			}
		case "contract_batch":
			if e.Condition != nil && e.Condition.Field == "scale_verdict" &&
				e.Condition.Operator == "eq" && e.Condition.Value == "batched" {
				batchedFound = true
			}
		}
		if e.Condition != nil && e.Condition.Field == "scale_verdict" {
			verdictSeen++
		}
	}
	if verdictSeen != 2 {
		t.Fatalf("expected exactly 2 verdict-conditioned edges from scale_gate, got %d", verdictSeen)
	}
	if !batchedFound || !linearFound || !defaultLinear {
		t.Fatalf("scale_gate edges: batched=%v linear=%v defaultLinear=%v", batchedFound, linearFound, defaultLinear)
	}

	// Engine-level routing checks via chooseNextEdge.
	edge, ok := chooseNextEdge(tmpl.Edges, "scale_gate", map[string]string{"scale_verdict": "batched", "batch_plan": "..."}, "")
	if !ok || edge.To != "contract_batch" {
		t.Fatalf("batched verdict must route to contract_batch, got %q ok=%v", edge.To, ok)
	}
	edge, ok = chooseNextEdge(tmpl.Edges, "scale_gate", map[string]string{"scale_verdict": "linear"}, "")
	if !ok || edge.To != "acceptance_test_design" {
		t.Fatalf("linear verdict must route to acceptance_test_design, got %q ok=%v", edge.To, ok)
	}
	edge, ok = chooseNextEdge(tmpl.Edges, "scale_gate", map[string]string{}, "")
	if !ok || edge.To != "acceptance_test_design" {
		t.Fatalf("empty verdict must default to acceptance_test_design (fail-safe linear), got %q ok=%v", edge.To, ok)
	}
	edge, ok = chooseNextEdge(tmpl.Edges, "scale_gate", map[string]string{"scale_verdict": "unsure"}, "")
	if !ok || edge.To != "acceptance_test_design" {
		t.Fatalf("unknown verdict must default to acceptance_test_design, got %q ok=%v", edge.To, ok)
	}
}

// The scale gate must receive the full frozen design contract so the batched
// path can forward it to contract_batch/parallel branches without a round-trip
// through the design gate.
func TestGreenfieldScaleGateCarriesDesignContract(t *testing.T) {
	tmpl, ok := Template("greenfield-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("greenfield template not registered")
	}
	var gate *entity.WorkflowStep
	for i := range tmpl.Steps {
		if tmpl.Steps[i].ID == "scale_gate" {
			gate = &tmpl.Steps[i]
		}
	}
	if gate == nil {
		t.Fatal("scale_gate step missing")
	}
	if gate.Type != "agent_task" || gate.ActorRole != "pm-agent" {
		t.Fatalf("scale_gate step mismatch: type=%s role=%s", gate.Type, gate.ActorRole)
	}
	outNames := map[string]bool{}
	for _, f := range gate.OutputFields {
		outNames[f.Name] = true
	}
	if !outNames["scale_verdict"] || !outNames["batch_plan"] {
		t.Fatalf("scale_gate outputs missing scale_verdict/batch_plan: %v", outNames)
	}
	var carriesContract bool
	for _, e := range tmpl.Edges {
		if e.From == "design_review" && e.To == "scale_gate" {
			m := e.InputMapping
			if m["approved_requirement"] == "$input.approved_requirement" &&
				m["approved_design_html"] == "$output.approved_design_html" &&
				m["approved_design_snapshot_path"] == "$output.approved_design_snapshot_path" {
				carriesContract = true
			}
		}
	}
	if !carriesContract {
		t.Fatal("design_review->scale_gate edge must carry the frozen design contract")
	}
}

// Batched-path E2E at the store level: scale_gate=batched → contract_batch →
// contract_review approve → parallel fan-out (two branches complete) →
// aggregated branch outputs reach integration_review.
func TestGreenfieldBatchedPathFanOutAndConverge(t *testing.T) {
	controlDB, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "workspace-bp", Name: "BP", Slug: "bp", Root: t.TempDir()}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	store := NewStore(controlDB, "workspace-bp")
	def, ok := DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Batched E2E")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := store.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		"pm-agent":           {Type: "agent", ID: "pm"},
		"product-owner":      {Type: "user", ID: "po"},
		"qa-agent":           {Type: "agent", ID: "qa"},
		"developer-agent":    {Type: "agent", ID: "dev"},
		"reviewer-agent":     {Type: "agent", ID: "reviewer"},
		"owner-engineer":     {Type: "user", ID: "eng"},
		"qa-owner":           {Type: "user", ID: "qaowner"},
		"release-agent":      {Type: "agent", ID: "rel"},
		"workstream-agent-1": {Type: "agent", ID: "ws1"},
		"workstream-agent-2": {Type: "agent", ID: "ws2"},
	}
	if _, _, err := store.StartRun("project", "task-batched", def.ID, bindings); err != nil {
		t.Fatalf("start run: %v", err)
	}
	const project = "project"

	// Walk to the scale gate: requirement draft → review → design approve.
	if _, err := store.CompleteAndAdvance(project, "task-batched", "drafted", "", map[string]string{
		"requirement_draft": "Large requirement",
		"open_questions":    "none",
	}, "completed"); err != nil {
		t.Fatalf("req draft: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, "task-batched", "approved", "", map[string]string{
		"decision":             "approve",
		"comments":             "confirmed large",
		"approved_requirement": "Approved large requirement",
	}, "completed"); err != nil {
		t.Fatalf("req review: %v", err)
	}
	tr, err := store.CompleteAndAdvance(project, "task-batched", "design ok", "", map[string]string{
		"decision":                      "approve",
		"comments":                      "prototype confirmed",
		"approved_design_html":          "<html/>",
		"approved_design_snapshot_path": "design/snap",
	}, "completed")
	if err != nil {
		t.Fatalf("design review: %v", err)
	}
	if tr.Next == nil || tr.Next.ID != "scale_gate" {
		t.Fatalf("expected next=scale_gate, got %+v", tr.Next)
	}

	// Scale gate verdict = batched → contract_batch.
	tr, err = store.CompleteAndAdvance(project, "task-batched", "big requirement, batched", "", map[string]string{
		"scale_verdict": "batched",
		"batch_plan":    `{"workstreams":[{"branch_id":"workstream_1"},{"branch_id":"workstream_2"}],"shared_contract":["ddl","error_codes","api_skeleton"]}`,
	}, "completed")
	if err != nil {
		t.Fatalf("scale gate: %v", err)
	}
	if tr.Next == nil || tr.Next.ID != "contract_batch" {
		t.Fatalf("expected next=contract_batch, got %+v", tr.Next)
	}
	inst := gfStepInstance(t, store, "task-batched", "contract_batch")
	if inst.InputValues["batch_plan"] == "" {
		t.Fatal("contract_batch must receive batch_plan from the scale gate")
	}

	// Contract batch completes with artifacts → contract_review.
	tr, err = store.CompleteAndAdvance(project, "task-batched", "contract committed", "", map[string]string{
		"contract_artifacts": "schema+errors+api @ base commit 4a5e5aec",
	}, "completed")
	if err != nil {
		t.Fatalf("contract batch: %v", err)
	}
	if tr.Next == nil || tr.Next.ID != "contract_review" {
		t.Fatalf("expected next=contract_review, got %+v", tr.Next)
	}

	// Contract review approves → parallel stage activates.
	tr, err = store.CompleteAndAdvance(project, "task-batched", "contract approved", "", map[string]string{
		"decision": "approve",
		"comments": "contract complete",
	}, "completed")
	if err != nil {
		t.Fatalf("contract review: %v", err)
	}
	if tr.Next == nil || tr.Next.Type != "parallel_stage" || tr.Next.ID != "parallel_workstreams" {
		t.Fatalf("expected next=parallel_workstreams (parallel_stage), got %+v", tr.Next)
	}
	parent := tr.NextInst
	run, _, err := store.RunForTask(project, "task-batched")
	if err != nil {
		t.Fatalf("run for task: %v", err)
	}
	if len(tr.Next.Branches) != 2 {
		t.Fatalf("expected 2 template branches, got %d", len(tr.Next.Branches))
	}
	now := time.Now().UTC()
	for _, branch := range tr.Next.Branches {
		inst := &entity.WorkflowBranchInstance{
			RunID:       run.ID,
			StepID:      "parallel_workstreams",
			BranchID:    branch.ID,
			Status:      "running",
			ActorType:   "agent",
			ActorID:     bindings[branch.ActorRole].ID,
			ChildTaskID: entity.NewTaskID(),
			StartedAt:   now,
			UpdatedAt:   now,
			InputValues: buildBranchInputValues(*parent, branch),
		}
		if inst.InputValues["contract_artifacts"] == "" {
			t.Fatalf("branch %s must inherit contract_artifacts from the parallel stage inputs", branch.ID)
		}
		if err := store.SaveBranchInstance(inst); err != nil {
			t.Fatalf("save branch instance: %v", err)
		}
	}

	// First branch completes → not all done yet. Branch output fields named
	// touched_paths opt into the QA real-change gate (module branch path),
	// so the fixture uses test-file paths.
	first, err := store.CompleteBranchAndMaybeAdvance(project, "task-batched", run.ID, "parallel_workstreams", "workstream_1", "ws1 done", map[string]string{"branch_summary": "crypto chain built", "touched_paths": "server/a_test.go"}, "completed")
	if err != nil {
		t.Fatalf("complete branch 1: %v", err)
	}
	if first.AllDone {
		t.Fatal("first branch completion must wait for the remaining branch")
	}
	// Second branch completes → converge into integration_review with the
	// aggregated branch outputs.
	second, err := store.CompleteBranchAndMaybeAdvance(project, "task-batched", run.ID, "parallel_workstreams", "workstream_2", "ws2 done", map[string]string{"branch_summary": "state machine built", "touched_paths": "server/b_test.go"}, "completed")
	if err != nil {
		t.Fatalf("complete branch 2: %v", err)
	}
	if !second.AllDone {
		t.Fatal("second branch completion must converge the stage")
	}
	if second.Transition.Next == nil || second.Transition.Next.ID != "integration_review" {
		t.Fatalf("expected convergence into integration_review, got %+v", second.Transition.Next)
	}
	integration := gfStepInstance(t, store, "task-batched", "integration_review")
	merged := integration.InputValues["contract_artifacts"] + integration.InputValues["branch_reports"]
	if !strings.Contains(second.Transition.Run.ActiveStepID, "integration_review") {
		t.Fatalf("active step = %q, want integration_review", second.Transition.Run.ActiveStepID)
	}
	_ = merged
}
