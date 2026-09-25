package workflow

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// planAtomicityFixture builds a one-approval-run whose active step is a human
// review, so CompleteAndAdvanceWithExtras can be exercised directly.
func planAtomicityFixture(t *testing.T, store *Store) entity.WorkflowRun {
	t.Helper()
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID: "wf-plan-atomic", Name: "Plan atomicity", Version: 1, Scope: "workspace", StartStepID: "draft",
		Steps: []entity.WorkflowStep{
			{ID: "draft", Type: "agent_task", Title: "Draft", ActorRole: "pm", OutputFields: []entity.WorkflowField{{Name: "delivery_plan"}}},
			{ID: "review", Type: "human_review", Title: "Contract review", ActorRole: "owner",
				OutputFields: []entity.WorkflowField{{Name: "decision"}, {Name: "comments"}}},
			{ID: "after", Type: "agent_task", Title: "After", ActorRole: "pm", OutputFields: []entity.WorkflowField{{Name: "result"}}},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "a", From: "draft", To: "review", IsDefault: true},
			{ID: "b", From: "review", To: "after", IsDefault: true},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	run, _, err := store.StartRun("sample", "task-atomic", def.ID, map[string]entity.WorkflowActorBinding{
		"pm": {Type: "agent", ID: "pm"}, "owner": {Type: "human", ID: "admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndAdvance("sample", "task-atomic", "draft done", "", map[string]string{"delivery_plan": "{}"}, "completed"); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.RunByID("sample", run.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if loaded.ActiveStepID != "review" {
		t.Fatalf("fixture must park at the review, got %s", loaded.ActiveStepID)
	}
	return loaded
}

func atomicityFixturePlan() DeliveryPlan {
	plan := fixturePlan()
	plan.RunID = "wfr-atomicity"
	return plan
}

// TestCompleteAndAdvanceWithExtrasCommitsFreezeWithTheTransition proves the
// slice-4 atomicity claim at the store level:
//
//   - an extras failure aborts the WHOLE transition (no advance, no record),
//   - a successful extras write lands in the SAME commit as the advance
//     (the advance and the frozen plan are observable together or not at all),
//   - a replayed transition cannot write the extras twice.
func TestCompleteAndAdvanceWithExtrasCommitsFreezeWithTheTransition(t *testing.T) {
	controlDB, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "ws-atomic", Name: "Atomic", Slug: "ws-atomic", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	store := NewStore(controlDB, "ws-atomic")
	project := "sample"
	run := planAtomicityFixture(t, store)
	plan := atomicityFixturePlan()
	plan.RunID = run.ID

	// 1) Extras failure (plan rejected mid-flight): nothing may be written.
	bomb := func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		return nil, errors.New("plan validation failed mid-transition")
	}
	if _, err := store.CompleteAndAdvanceWithExtras(project, "task-atomic", "approve", "", map[string]string{"decision": "approve", "comments": "approved"}, "completed", bomb); err == nil {
		t.Fatal("a failing extras write must abort the transition")
	}
	if _, _, ok, err := store.LoadFrozenPlanForRun(project, run.ID); err != nil || ok {
		t.Fatalf("the aborted transition must not leave a frozen plan (ok=%v err=%v)", ok, err)
	}
	if loaded, _, _ := store.RunByID(project, run.ID); loaded.ActiveStepID != "review" {
		t.Fatalf("the aborted transition must not advance the run, got %s", loaded.ActiveStepID)
	}

	// 2) Successful extras: both effects land together.
	extras, err := store.PreparePlanFreezeExtras(project, run.ID, plan, "admin", PlanApprovalProvenance{StepID: "review"})
	if err != nil {
		t.Fatal(err)
	}
	transition, err := store.CompleteAndAdvanceWithExtras(project, "task-atomic", "approve", "", map[string]string{"decision": "approve", "comments": "approved"}, "completed", extras)
	if err != nil {
		t.Fatal(err)
	}
	if transition.Run.ActiveStepID != "after" {
		t.Fatalf("the transition must advance to after, got %s", transition.Run.ActiveStepID)
	}
	record, version, ok, err := store.LoadFrozenPlanForRun(project, run.ID)
	if err != nil || !ok {
		t.Fatalf("the committed transition must carry the frozen plan: ok=%v err=%v", ok, err)
	}
	if version.Digest == "" || record.CurrentVersion != 1 || version.ApprovedBy != "admin" {
		t.Fatalf("frozen record incomplete: %+v %+v", record, version)
	}

	// 3) A replayed review (the step instance is already consumed) may not mint
	// a second version even if extras are supplied again.
	replay, err := store.PreparePlanFreezeExtras(project, run.ID, plan, "admin", PlanApprovalProvenance{StepID: "review"})
	if err != nil {
		t.Fatalf("preparing a replay freeze must not fail on its own: %v", err)
	}
	if _, err := store.CompleteAndAdvanceWithExtras(project, "task-atomic", "approve again", "", map[string]string{"decision": "approve", "comments": "approved"}, "completed", replay); err == nil {
		t.Fatal("a consumed review step must refuse the replay")
	}
	record, _, _, err = store.LoadFrozenPlanForRun(project, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Versions) != 1 {
		t.Fatalf("the replay must not add a version, got %d", len(record.Versions))
	}
}
