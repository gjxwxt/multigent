package workflow

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// G2 regression: ReactivateFailedRunForStep must capture the failed step's
// summary BEFORE the reset wipes it, so the reactivated step event preserves
// the failure cause (audit trail keeps WHAT failed, not only that a retry
// happened). Before the fix the event read inst.Summary after the reset and
// the previous-failure clause was dead code.
func TestReactivateFailedRunForStepPreservesFailureSummary(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-g2", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	store := NewStore(controlDB, "workspace-g2")
	def := &entity.WorkflowDefinition{
		ID:          "wf-g2",
		Name:        "G2",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "work",
		Steps: []entity.WorkflowStep{
			{ID: "work", Type: "agent_task", Title: "work", Position: entity.WorkflowPosition{X: 0, Y: 0}},
		},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := store.StartRun("project-g2", "task-g2", def.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndAdvance("project-g2", "task-g2", "console restarted mid-run", "", nil, "failed"); err != nil {
		t.Fatalf("fail the step: %v", err)
	}
	run, found, err := store.RunForTask("project-g2", "task-g2")
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if run.Status != "failed" {
		t.Fatalf("expected failed run, got %s", run.Status)
	}

	reactivated, err := store.ReactivateFailedRunForStep("project-g2", "task-g2", run.ID, "work", "operator retry via manual start")
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if reactivated.Status != "active" || reactivated.ActiveStepID != "work" {
		t.Fatalf("run must be reactivated onto work, got %s@%s", reactivated.Status, reactivated.ActiveStepID)
	}

	events, err := store.ListStepEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundEvent := false
	for _, ev := range events {
		if ev.Status != "reactivated" {
			continue
		}
		foundEvent = true
		if !strings.Contains(ev.Summary, "console restarted mid-run") {
			t.Fatalf("reactivated event must preserve the previous failure summary, got %q", ev.Summary)
		}
		if !strings.Contains(ev.Summary, "operator retry via manual start") {
			t.Fatalf("reactivated event must carry the retry reason, got %q", ev.Summary)
		}
	}
	if !foundEvent {
		t.Fatalf("no reactivated step event found for run %s step work", run.ID)
	}
}
