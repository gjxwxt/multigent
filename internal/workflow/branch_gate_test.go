package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Round-19 P0-A: the QA touched_paths checkpoint must hold on EVERY
// completion entry. A custom parallel_stage workflow can route qa work
// through a branch — CompleteBranchAndMaybeAdvance previously reached
// neither the resolver nor the gate, so a branch could declare arbitrary
// touched_paths with no real-change verification at all.

func newBranchGateStore(t *testing.T) (*Store, string, string) {
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
		ID:          "wf-branch-qa",
		Name:        "Branch QA Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID:       "start",
				Type:     "agent_task",
				Title:    "Start",
				Position: entity.WorkflowPosition{X: 0, Y: 0},
			},
			{
				ID:       "parallel",
				Type:     "parallel_stage",
				Title:    "Parallel Stage",
				Position: entity.WorkflowPosition{X: 240, Y: 0},
				Branches: []entity.WorkflowBranch{
					{
						ID:    "qa_branch",
						Title: "QA Branch",
						OutputFields: []entity.WorkflowField{
							{Name: "risk_coverage_matrix", Description: "matrix."},
							{Name: "touched_paths", Description: "files touched."},
							{Name: "test_report", Description: "report."},
						},
					},
					{
						ID:    "other_branch",
						Title: "Other Branch",
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
	for _, branch := range transition.Next.Branches {
		inst := &entity.WorkflowBranchInstance{
			RunID:       run.ID,
			StepID:      transition.Next.ID,
			BranchID:    branch.ID,
			Status:      "running",
			ActorType:   "agent",
			ActorID:     "qa-agent",
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

func seedBranchWorktree(t *testing.T, store *Store) string {
	t.Helper()
	wt := newGitWorktree(t)
	store.WorktreeResolver = func(project, taskID string) string { return wt }
	return wt
}

func TestBranchQAGateBlocksUndeclaredBusinessChange(t *testing.T) {
	store, runID, stepID := newBranchGateStore(t)
	wt := seedBranchWorktree(t, store)
	// QA branch edits a business file without declaring it.
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc Bad() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", runID, stepID, "qa_branch", "task-1", "qa done",
		map[string]string{"risk_coverage_matrix": "[{\"item_id\":\"AC-1\",\"risk_level\":\"low\",\"status\":\"passed\"}]",
			"touched_paths": "server_test.go", "test_report": "ok"}, "completed")
	if err == nil || !strings.Contains(err.Error(), "not declared") || !strings.Contains(err.Error(), "server.go") {
		t.Fatalf("branch qa completion must hit the real-change gate, got: %v", err)
	}
	// The branch must still be pending for a corrected completion.
	branches, err := store.BranchInstancesForStep(runID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b.BranchID == "qa_branch" && b.Status != "running" {
			t.Fatalf("rejected branch must stay pending, got %q", b.Status)
		}
	}
}

func TestBranchQAGateHappyPathCompletes(t *testing.T) {
	store, runID, stepID := newBranchGateStore(t)
	wt := seedBranchWorktree(t, store)
	// QA branch adds a test file and declares it honestly.
	if err := os.WriteFile(filepath.Join(wt, "qa_probe_test.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", runID, stepID, "qa_branch", "task-1", "qa done",
		map[string]string{"risk_coverage_matrix": "[{\"item_id\":\"AC-1\",\"risk_level\":\"low\",\"status\":\"passed\"}]",
			"touched_paths": "qa_probe_test.go", "test_report": "ok"}, "completed")
	if err != nil {
		t.Fatalf("honest branch qa completion must pass the gate: %v", err)
	}
	if res.AllDone {
		t.Fatal("other branch still pending")
	}
}

func TestBranchQAGateFailClosedWithoutWorktree(t *testing.T) {
	store, runID, stepID := newBranchGateStore(t)
	// Resolver wired but reports no worktree — fail-closed, not silent
	// declaration-only downgrade.
	store.WorktreeResolver = func(project, taskID string) string { return "" }
	_, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", runID, stepID, "qa_branch", "task-1", "qa done",
		map[string]string{"risk_coverage_matrix": "[{\"item_id\":\"AC-1\",\"risk_level\":\"low\",\"status\":\"passed\"}]",
			"touched_paths": "whatever_test.go", "test_report": "ok"}, "completed")
	if err == nil || !strings.Contains(err.Error(), "observable worktree") {
		t.Fatalf("unobservable worktree must fail closed, got: %v", err)
	}
}

func TestLinearQAGateFailClosedWithBrokenWorktree(t *testing.T) {
	// Drive the LINEAR path: replace the run's active step context by using
	// a definition whose active step is a qa step declaring touched_paths.
	// Simplest construction: a fresh store + linear definition.
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	linStore := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-linear-qa",
		Name:        "Linear QA Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "qa",
		Steps: []entity.WorkflowStep{
			{
				ID:    "qa",
				Type:  "agent_task",
				Title: "QA",
				OutputFields: []entity.WorkflowField{
					{Name: "touched_paths", Description: "files touched."},
				},
				Position: entity.WorkflowPosition{X: 0, Y: 0},
			},
		},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := linStore.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := linStore.StartRun("project", "task-1", def.ID, nil); err != nil {
		t.Fatalf("start run: %v", err)
	}
	// Resolver points at a directory that is not a git worktree: the
	// linear completion path must fail closed (round-19 P0-A).
	broken := t.TempDir()
	linStore.WorktreeResolver = func(project, taskID string) string { return broken }
	_, err = linStore.CompleteAndAdvance("project", "task-1", "qa done", "", map[string]string{
		"touched_paths": "whatever_test.go",
	}, "completed")
	if err == nil || !strings.Contains(err.Error(), "observable worktree") {
		t.Fatalf("broken worktree must fail closed, got: %v", err)
	}
}

// newLinearQAGateStore builds a one-step linear workflow whose qa step
// declares touched_paths (the D-N fixture shape).
func newLinearQAGateStore(t *testing.T) *Store {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	linStore := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-linear-qa-dn",
		Name:        "Linear QA D-N",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "qa",
		Steps: []entity.WorkflowStep{
			{
				ID:    "qa",
				Type:  "agent_task",
				Title: "QA",
				OutputFields: []entity.WorkflowField{
					{Name: "touched_paths", Description: "files touched."},
				},
				Position: entity.WorkflowPosition{X: 0, Y: 0},
			},
		},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := linStore.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := linStore.StartRun("project", "task-1", def.ID, nil); err != nil {
		t.Fatalf("start run: %v", err)
	}
	return linStore
}

// D-N regression 1: a hub-branch run keeps its deltas on branch tasks, so
// the root task has NO code worktree. When the resolver reports that
// honestly (empty string), the linear QA gate must downgrade to the
// declaration-only allowlist instead of wedging behind "requires an
// observable worktree".
func TestLinearQAGateDowngradesToDeclarationOnlyWhenNoCodeWorktree(t *testing.T) {
	linStore := newLinearQAGateStore(t)
	linStore.WorktreeResolver = func(project, taskID string) string { return "" }
	res, err := linStore.CompleteAndAdvance("project", "task-1", "qa done", "", map[string]string{
		"touched_paths": "qa_probe_test.go",
	}, "completed")
	if err != nil {
		t.Fatalf("declaration-only completion with no code worktree must pass: %v", err)
	}
	if res.Run.Status != "completed" {
		t.Fatalf("run must complete, got %s", res.Run.Status)
	}
}

// D-N regression 2: a resolver that names an unobservable directory still
// fails closed — the downgrade above is reserved for the explicit "no code
// worktree" answer, never for a missing/unreadable one.
func TestLinearQAGateStillFailsClosedOnUnobservablePath(t *testing.T) {
	linStore := newLinearQAGateStore(t)
	broken := t.TempDir()
	linStore.WorktreeResolver = func(project, taskID string) string { return broken }
	_, err := linStore.CompleteAndAdvance("project", "task-1", "qa done", "", map[string]string{
		"touched_paths": "qa_probe_test.go",
	}, "completed")
	if err == nil || !strings.Contains(err.Error(), "observable worktree") {
		t.Fatalf("unobservable path must fail closed, got: %v", err)
	}
}

// D-N regression 3: with a REAL observable worktree the cross-check stays
// in force — a declared path with no real change is still a phantom.
func TestLinearQAGateCrossCheckStaysOnRealWorktree(t *testing.T) {
	linStore := newLinearQAGateStore(t)
	wt := newGitWorktree(t)
	linStore.WorktreeResolver = func(project, taskID string) string { return wt }
	// qa_probe_test.go does not exist as an uncommitted change: declaring it
	// must hit Direction 2 (phantom path).
	_, err := linStore.CompleteAndAdvance("project", "task-1", "qa done", "", map[string]string{
		"touched_paths": "qa_probe_test.go",
	}, "completed")
	if err == nil || !strings.Contains(err.Error(), "does not match the worktree") {
		t.Fatalf("phantom declaration on a real worktree must fail, got: %v", err)
	}
}
