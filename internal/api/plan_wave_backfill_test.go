package api

import (
	"context"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedBackfillFixture parks a run on a plan-driven parallel stage with a
// frozen two-wave plan; wave 0 materializes via a normal fan-out, and the
// parent's run.UpdatedAt is pushed back so a masking tick would be visible.
func seedBackfillFixture(t *testing.T) (*Server, string, *entity.Task, workflowstore.Store, string) {
	t.Helper()
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	root, _, err := s.findTaskInProject("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	return s, workspaceID, root, *wfStore, runID
}

// TestPlanWaveBackfillDoesNotBumpRunUpdatedAt is the spec-mandated masking
// regression: a backfill sweep over a stalled run must be a pure no-op on the
// run row — if the tick bumped UpdatedAt, Tier 1's stall judgment would never
// fire and a genuinely wedged run would become invisible to it forever.
func TestPlanWaveBackfillDoesNotBumpRunUpdatedAt(t *testing.T) {
	s, _, _, wfStore, _ := seedBackfillFixture(t)

	run, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-2 * time.Hour)
	run.UpdatedAt = stale
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}

	before, _, _ := wfStore.RunForTask("sample", "task-plan-root")
	if !before.UpdatedAt.Equal(stale) {
		t.Fatalf("fixture must persist the stale UpdatedAt, got %s", before.UpdatedAt)
	}

	s.planWaveBackfillPass(context.Background())

	after, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	// Wave 0 is already materialized by the fixture fan-out and nothing new
	// is ready (wp-a/wp-b unmaterialized-but-ready WILL materialize — see the
	// companion test), so here we assert only the invariant: whatever the
	// sweep did, the run row's UpdatedAt must not move.
	if !after.UpdatedAt.Equal(stale) {
		t.Fatalf("the backfill tick must not bump run.UpdatedAt (it would mask Tier 1's stall judgment): %s -> %s", stale, after.UpdatedAt)
	}
}

// TestPlanWaveBackfillMaterializesHeldBackWave: the backfill sweep re-runs the
// wave hook for an active plan-driven run and materializes ready packages
// without any branch-completion event — the lost-event safety net.
func TestPlanWaveBackfillMaterializesHeldBackWave(t *testing.T) {
	s, workspaceID, _, wfStore, runID := seedBackfillFixture(t)

	// Drive the fixture into the live post-fan-out shape: the start step
	// completes (wave 0 materializes), then wp-a completes through the
	// production endpoint. wp-c (depends on wp-a) becomes Ready — its
	// materialization rides the same completion event normally; to simulate
	// the LOST event we skip nothing but re-order: complete wp-a via the
	// runtime endpoint and let the sweep (not the completion path) unlock
	// wp-c. The completion handler DOES re-drive waves, so to isolate the
	// sweep we make wp-a's completion a direct instance-state write first
	// and hand the unlock to the backfill pass.
	if rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}); rec.Code != 200 {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	instA, found := branchInstanceFor(t, &wfStore, runID, "parallel", "wp-a")
	if !found {
		t.Fatal("wp-a must be materialized in wave 0")
	}
	// Direct instance-state completion (the lost-event shape: the completion
	// report never reached the parent driver).
	instA.Status = workflowstore.PlanWPStateCompleted
	if err := wfStore.SaveBranchInstance(&instA); err != nil {
		t.Fatal(err)
	}

	if n := s.planWaveBackfillPass(context.Background()); n != 1 {
		t.Fatalf("the backfill pass must materialize the newly-ready wave, got %d", n)
	}
	if _, found := branchInstanceFor(t, &wfStore, runID, "parallel", "wp-c"); !found {
		t.Fatal("wp-c must materialize from the backfill pass after wp-a completed")
	}

	// A second pass is a clean zero (existingIDs gate — 1:1:1:1 invariant).
	if n := s.planWaveBackfillPass(context.Background()); n != 0 {
		t.Fatalf("a second pass must be a no-op, got %d", n)
	}
}
