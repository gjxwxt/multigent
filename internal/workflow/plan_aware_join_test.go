package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// S2 run5 premature-join defect (2026-09-26): for a plan-driven parallel
// stage, CompleteBranchAndMaybeAdvance computed AllDone over the ALREADY
// MATERIALIZED branch instances only. When a wave's last materialized branch
// completed while downstream work packages were not yet materialized (their
// branch instances do not exist until materializeNextPlanWave runs in the
// API layer AFTER the store call returns), the store advanced the stage with
// a partial aggregate. The run left parallel_workstreams, later branches
// could never join ("no longer active"), and the remaining waves never
// materialized.
//
// Fix contract under test:
//   - With a frozen plan: the stage may only advance when EVERY work package
//     of the plan is materialized (a branch instance exists) AND completed.
//     Un-materialized downstream work keeps the stage active (AllDone=false,
//     no transition) so the API layer can materialize the next wave and a
//     later completion re-drives the join.
//   - Without a frozen plan (legacy static branches): the original semantics
//     are unchanged — every materialized branch terminal means advance.
//   - A failed branch never advances (plan present or not).

func newPlanAwareJoinStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-plan-join",
		Name:        "Plan Aware Join",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{ID: "start", Type: "agent_task", Title: "Start", Position: entity.WorkflowPosition{X: 0, Y: 0}},
			{
				ID:       "parallel",
				Type:     "parallel_stage",
				Title:    "Parallel Stage",
				Position: entity.WorkflowPosition{X: 240, Y: 0},
				Branches: []entity.WorkflowBranch{
					{
						ID:    "wp-a",
						Title: "WP A",
						OutputFields: []entity.WorkflowField{
							{Name: "notes", Description: "notes."},
						},
					},
					{
						ID:    "wp-b",
						Title: "WP B",
						OutputFields: []entity.WorkflowField{
							{Name: "notes", Description: "notes."},
						},
					},
				},
			},
			{
				ID:        "review",
				Type:      "human_review",
				Title:     "Review",
				ActorRole: "reviewer",
				Position:  entity.WorkflowPosition{X: 480, Y: 0},
			},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "e1", From: "start", To: "parallel"},
			{ID: "e2", From: "parallel", To: "review"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	run, _, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"reviewer": {Type: "human", ID: "owner"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	transition, err := store.CompleteAndAdvance("project", "task-1", "start done", "", map[string]string{}, "completed")
	if err != nil {
		t.Fatalf("complete start: %v", err)
	}
	parent := transition.NextInst
	// Only wp-a is materialized: the plan-driven materializer creates wp-b's
	// instance only after wp-a completes (wave unlock).
	for _, branch := range transition.Next.Branches {
		if branch.ID != "wp-a" {
			continue
		}
		inst := &entity.WorkflowBranchInstance{
			RunID:       run.ID,
			StepID:      transition.Next.ID,
			BranchID:    branch.ID,
			Status:      "running",
			ActorType:   "agent",
			ActorID:     "dev-a",
			ChildTaskID: entity.NewTaskID(),
			StartedAt:   now,
			UpdatedAt:   now,
			InputValues: buildBranchInputValues(*parent, branch),
		}
		if err := store.SaveBranchInstance(inst); err != nil {
			t.Fatalf("save branch instance: %v", err)
		}
	}
	return store, run.ID, "parallel"
}

func freezePlanAwareJoinPlan(t *testing.T, store *Store, runID string) {
	t.Helper()
	plan := DeliveryPlan{
		SchemaVersion: DeliveryPlanSchemaVersion,
		PlanID:        "plan-join-race",
		Version:       1,
		RunID:         runID,
		RequirementItems: []PlanRequirementItem{
			{ID: "uc-1", Text: "catalog crud", Source: "brief"},
			{ID: "uc-2", Text: "report aggregation", Source: "brief"},
		},
		WorkPackages: []PlanWorkPackage{
			{ID: "wp-a", Title: "Catalog", AcceptanceCriteria: []string{"uc-1"}, AgentBinding: "dev-a"},
			{ID: "wp-b", Title: "Report", AcceptanceCriteria: []string{"uc-2"}, AgentBinding: "dev-b", DependsOn: []string{"wp-a"}},
		},
	}
	if _, _, err := store.FreezeDeliveryPlan("project", plan, "admin"); err != nil {
		t.Fatalf("freeze plan: %v", err)
	}
}

func completePlanAwareBranch(t *testing.T, store *Store, runID, stepID, branchID string, status string) (BranchTransitionResult, error) {
	t.Helper()
	wt := newGitWorktree(t)
	store.WorktreeResolver = func(project, taskID string) string { return wt }
	if err := os.WriteFile(filepath.Join(wt, branchID+"_probe.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return store.CompleteBranchAndMaybeAdvance("project", "task-1", runID, stepID, branchID, "task-1", branchID+" done",
		map[string]string{"notes": "ok"}, status)
}

// materializePlanAwareWP creates the named branch instance the way the
// API-layer wave materializer does after a dependency completes.
func materializePlanAwareWP(t *testing.T, store *Store, runID, stepID, branchID string) {
	t.Helper()
	now := time.Now().UTC()
	def, ok, err := store.Definition("wf-plan-join")
	if err != nil || !ok {
		t.Fatalf("load definition: %v", err)
	}
	step, found := stepByID(def.Steps, stepID)
	if !found {
		t.Fatalf("step %q not found", stepID)
	}
	insts, err := store.ListStepInstances(runID)
	if err != nil {
		t.Fatalf("list step instances: %v", err)
	}
	var parent *entity.WorkflowStepInstance
	for i := range insts {
		if insts[i].StepID == stepID {
			parent = &insts[i]
		}
	}
	if parent == nil {
		t.Fatalf("stage instance %q not found", stepID)
	}
	var branch entity.WorkflowBranch
	for _, b := range step.Branches {
		if b.ID == branchID {
			branch = b
		}
	}
	if err := store.SaveBranchInstance(&entity.WorkflowBranchInstance{
		RunID:       runID,
		StepID:      stepID,
		BranchID:    branchID,
		Status:      "running",
		ActorType:   "agent",
		ActorID:     "dev-b",
		ChildTaskID: entity.NewTaskID(),
		StartedAt:   now,
		UpdatedAt:   now,
		InputValues: buildBranchInputValues(*parent, branch),
	}); err != nil {
		t.Fatalf("materialize %s: %v", branchID, err)
	}
}

// THE defect: with a frozen plan whose wp-b is not yet materialized, wp-a's
// completion must NOT advance the stage (previously it did — premature join).
func TestPlanAwareJoinHoldsStageUntilAllWorkPackagesMaterialized(t *testing.T) {
	store, runID, stepID := newPlanAwareJoinStore(t)
	freezePlanAwareJoinPlan(t, store, runID)

	res, err := completePlanAwareBranch(t, store, runID, stepID, "wp-a", "completed")
	if err != nil {
		t.Fatalf("wp-a completion must pass the QA gate and record: %v", err)
	}
	if res.AllDone {
		t.Fatal("stage must not be all-done while plan work package wp-b is un-materialized")
	}
	if res.Transition.Next != nil {
		t.Fatal("stage must not advance while downstream work packages are un-materialized")
	}
	run, _, err := store.RunForTask("project", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.ActiveStepID != stepID || run.Status != "active" {
		t.Fatalf("run must stay active at %q, got active=%q status=%q", stepID, run.ActiveStepID, run.Status)
	}
	branches, err := store.BranchInstancesForStep(runID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, b := range branches {
		if b.BranchID == "wp-a" && b.Status == "completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("wp-a branch must be recorded completed, got %+v", branches)
	}
}

// Idempotent re-report of the already-completed branch while the plan is not
// fully materialized must be a zero-write replay (no advance, no error).
func TestPlanAwareJoinReplayWithoutFullMaterializationDoesNotAdvance(t *testing.T) {
	store, runID, stepID := newPlanAwareJoinStore(t)
	freezePlanAwareJoinPlan(t, store, runID)

	if _, err := completePlanAwareBranch(t, store, runID, stepID, "wp-a", "completed"); err != nil {
		t.Fatalf("first wp-a completion: %v", err)
	}
	res, err := completePlanAwareBranch(t, store, runID, stepID, "wp-a", "completed")
	if err != nil {
		t.Fatalf("re-report of a completed branch must replay idempotently: %v", err)
	}
	if res.AllDone || res.Transition.Next != nil {
		t.Fatal("idempotent replay must not advance while wp-b is un-materialized")
	}
	run, _, err := store.RunForTask("project", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.ActiveStepID != stepID {
		t.Fatalf("run must stay parked at the join, got active=%q", run.ActiveStepID)
	}
}

// Full materialization: once wp-b is materialized AND completed, the stage
// joins and advances (the wave-2 completion path).
func TestPlanAwareJoinAdvancesWhenAllWorkPackagesCompleted(t *testing.T) {
	store, runID, stepID := newPlanAwareJoinStore(t)
	freezePlanAwareJoinPlan(t, store, runID)

	if _, err := completePlanAwareBranch(t, store, runID, stepID, "wp-a", "completed"); err != nil {
		t.Fatalf("wp-a completion: %v", err)
	}
	// The API layer materializes wp-b after wp-a completes; mirror it here.
	materializePlanAwareWP(t, store, runID, stepID, "wp-b")

	res, err := completePlanAwareBranch(t, store, runID, stepID, "wp-b", "completed")
	if err != nil {
		t.Fatalf("wp-b completion must join the stage: %v", err)
	}
	if !res.AllDone {
		t.Fatal("all plan work packages materialized and completed: stage must be all-done")
	}
	if res.Transition.Next == nil || res.Transition.Next.ID != "review" {
		t.Fatalf("stage must advance to review, got next=%+v", res.Transition.Next)
	}
}

// Legacy static-branch semantics unchanged: no frozen plan → completing all
// materialized (static) branches still joins immediately.
func TestLegacyStaticBranchJoinUnchangedWithoutPlan(t *testing.T) {
	store, runID, stepID := newPlanAwareJoinStore(t)
	// No frozen plan on this run. Static branches are materialized up-front;
	// mirror that for wp-b.
	materializePlanAwareWP(t, store, runID, stepID, "wp-b")

	if _, err := completePlanAwareBranch(t, store, runID, stepID, "wp-a", "completed"); err != nil {
		t.Fatalf("wp-a completion: %v", err)
	}
	res, err := completePlanAwareBranch(t, store, runID, stepID, "wp-b", "completed")
	if err != nil {
		t.Fatalf("wp-b completion must join: %v", err)
	}
	if !res.AllDone || res.Transition.Next == nil || res.Transition.Next.ID != "review" {
		t.Fatalf("legacy join semantics must hold: AllDone=%v next=%+v", res.AllDone, res.Transition.Next)
	}
}

// A failed branch never advances — with a plan present either.
func TestPlanAwareJoinFailedBranchNeverAdvances(t *testing.T) {
	store, runID, stepID := newPlanAwareJoinStore(t)
	freezePlanAwareJoinPlan(t, store, runID)

	if _, err := completePlanAwareBranch(t, store, runID, stepID, "wp-a", "completed"); err != nil {
		t.Fatalf("wp-a completion: %v", err)
	}
	materializePlanAwareWP(t, store, runID, stepID, "wp-b")

	// A failed branch skips the QA gate (only completions are gated); it must
	// be recorded as failed and the stage must stay active.
	wt := newGitWorktree(t)
	store.WorktreeResolver = func(project, taskID string) string { return wt }
	res, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", runID, stepID, "wp-b", "task-1", "wp-b failed",
		map[string]string{"notes": "boom"}, "failed")
	if err != nil {
		t.Fatalf("failed branch completion must be recorded: %v", err)
	}
	if res.AllDone || res.Transition.Next != nil {
		t.Fatal("a failed branch must never advance the stage")
	}
	branches, err := store.BranchInstancesForStep(runID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b.BranchID == "wp-b" && b.Status != "failed" {
			t.Fatalf("wp-b must be recorded failed, got %q", b.Status)
		}
	}
}
