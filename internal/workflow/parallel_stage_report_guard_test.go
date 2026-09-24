package workflow

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// TestParallelStageRefusesReportCompletionWithoutBranches (review round 5,
// D-C — gate integrity). A parallel stage must advance only through its
// branch join. Report-style completions (runtime step/complete, a review
// click on a parked parent) present ZERO branch instances; because
// normalizeWorkflowOutputValues deliberately skips field validation for
// parallel stages, such a report used to complete the stage with no branch
// work at all and route the run past the fan-out over the default exit edge.
//
// Real S2 run wfr-wlhdwalv (2026-09-24) sat in exactly that shape after its
// fan-out activation failed: the run was active on parallel_workstreams with
// no branch instances, so any stray parent run (startup auto-recovery wakeup,
// manual start) could have faked the stage and skipped both workstreams. The
// guard must refuse the report with no writes, and it must NOT block the
// branch join (CompleteBranchAndMaybeAdvance calls back into CompleteAndAdvance
// with a branch instance present — asserted at the end of this test).
func TestParallelStageRefusesReportCompletionWithoutBranches(t *testing.T) {
	controlDB, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "workspace-guard", Name: "Guard", Slug: "guard", Root: t.TempDir()}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	store := NewStore(controlDB, "workspace-guard")
	now := time.Now().UTC()
	def := entity.WorkflowDefinition{
		ID: "wf-guard", Name: "Guard", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID: "start", Type: "agent_task", Title: "Start", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}},
			},
			{
				ID: "parallel", Type: "parallel_stage", Title: "Parallel", JoinPolicy: "all",
				OutputFields: []entity.WorkflowField{{Name: "branch_reports"}},
				Branches: []entity.WorkflowBranch{
					{ID: "ws_a", Title: "WS-A", OutputFields: []entity.WorkflowField{{Name: "branch_summary"}}},
					{ID: "ws_b", Title: "WS-B", OutputFields: []entity.WorkflowField{{Name: "branch_summary"}}},
				},
			},
			{ID: "done", Type: "agent_task", Title: "Done", ActorRole: "pm-agent"},
		},
		Edges: []entity.WorkflowEdge{
			{From: "start", To: "parallel"},
			{From: "parallel", To: "done", IsDefault: true},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		"pm-agent": {Type: "agent", ID: "pm"},
		"ws_a":     {Type: "agent", ID: "pm"},
		"ws_b":     {Type: "agent", ID: "pm"},
	}
	run, _, err := store.StartRun("project", "task-guard", def.ID, bindings)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	tr, err := store.CompleteAndAdvance("project", "task-guard", "contract frozen", "", map[string]string{
		"branch_summary": "contract frozen",
	}, "completed")
	if err != nil {
		t.Fatalf("complete start step: %v", err)
	}
	if tr.Next == nil || tr.Next.ID != "parallel" {
		t.Fatalf("expected next=parallel, got %+v", tr.Next)
	}

	// The report-style completion of the parallel stage must be refused.
	_, err = store.CompleteAndAdvance("project", "task-guard", "stage done", "", map[string]string{
		"branch_reports": "trust me",
	}, "completed")
	if err == nil {
		t.Fatal("parallel stage with no branch instances must not complete through a step report")
	}
	if !strings.Contains(err.Error(), "it advances only through its branch join") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}
	parked, ok, err := store.RunForTask("project", "task-guard")
	if err != nil || !ok {
		t.Fatalf("run lookup: ok=%v err=%v", ok, err)
	}
	if parked.ActiveStepID != "parallel" {
		t.Fatalf("run must stay parked on the stage, got active=%q", parked.ActiveStepID)
	}
	instances, err := store.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range instances {
		if instances[i].StepID != "parallel" {
			continue
		}
		if strings.TrimSpace(instances[i].Status) == "completed" {
			t.Fatal("refused completion must leave the stage instance pending (no writes)")
		}
		if len(instances[i].OutputValues) != 0 {
			t.Fatalf("refused completion must not persist outputs: %v", instances[i].OutputValues)
		}
	}
	// Read-only preview parity: WillComplete must not advertise the transition
	// the store refuses.
	willComplete, err := store.WillComplete("project", "task-guard", map[string]string{"branch_reports": "trust me"}, "stage done", "", "completed")
	if err == nil {
		t.Fatal("WillComplete must reject the same shape CompleteAndAdvance refuses")
	}
	if willComplete {
		t.Fatal("WillComplete reported a completion that the store refuses")
	}

	// The branch join still completes the stage (regression: the guard keys on
	// the join's own precondition — every branch terminal, none failed).
	for _, branch := range tr.Next.Branches {
		values := buildBranchInputValues(*tr.NextInst, branch)
		if err := store.SaveBranchInstance(&entity.WorkflowBranchInstance{
			RunID: run.ID, StepID: "parallel", BranchID: branch.ID, Status: "running",
			ActorType: "agent", ActorID: "pm", ChildTaskID: entity.NewTaskID(),
			InputValues: values, StartedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("save branch instance %s: %v", branch.ID, err)
		}
	}
	first, err := store.CompleteBranchAndMaybeAdvance("project", "task-guard", run.ID, "parallel", "ws_a", "task-guard", "ws_a done", map[string]string{"branch_summary": "ws_a done"}, "completed")
	if err != nil {
		t.Fatalf("complete branch ws_a: %v", err)
	}
	if first.AllDone {
		t.Fatal("first branch completion must wait for the remaining branch")
	}

	// Review round 5 P1 (reviewer re-check): the same refusal must hold in the
	// narrower window where SOME branch instances exist. Before the tightened
	// condition a stray parent report could complete the stage as soon as one
	// instance existed — under joinPolicy=any the stage completes on the first
	// branch, so that window is not hypothetical.
	_, err = store.CompleteAndAdvance("project", "task-guard", "stage done", "", map[string]string{
		"branch_reports": "trust me",
	}, "completed")
	if err == nil {
		t.Fatal("a parallel stage with a running branch must not complete through a step report")
	}
	if !strings.Contains(err.Error(), "it advances only through its branch join") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}
	if willComplete, err := store.WillComplete("project", "task-guard", map[string]string{"branch_reports": "trust me"}, "stage done", "", "completed"); err == nil || willComplete {
		t.Fatalf("WillComplete must agree while a branch is still running (willComplete=%v err=%v)", willComplete, err)
	}
	second, err := store.CompleteBranchAndMaybeAdvance("project", "task-guard", run.ID, "parallel", "ws_b", "task-guard", "ws_b done", map[string]string{"branch_summary": "ws_b done"}, "completed")
	if err != nil {
		t.Fatalf("complete branch ws_b: %v", err)
	}
	if !second.AllDone {
		t.Fatal("second branch completion must converge the stage")
	}
	if second.Transition.Next == nil || second.Transition.Next.ID != "done" {
		t.Fatalf("the join must still route past the stage, got %+v", second.Transition.Next)
	}
}
