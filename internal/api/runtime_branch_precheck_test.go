package api

// Regression tests for fix round S2-1 Fix A: the branch-join QA precheck in
// handleRuntimeWorkflowStepComplete must reject a failing branch completion
// BEFORE any persistence, so the child run / task / branch instance all stay
// retryable (the S2 dogfood left a done_success task and a completed child
// run behind a rejected branch output, wedging the parent join forever).
//
// precheckBranchJoinGate is exercised as a unit over (store, task, outputs);
// the handler wiring around it is a thin if-err-return.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/workflow"
)

func newPrecheckServer(t *testing.T) (*Server, *workflow.Store) {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "ws", Name: "WS", Slug: "ws", Root: t.TempDir()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	s := &Server{controlDB: controlDB}
	return s, workflow.NewStore(controlDB, "ws")
}

// newBranchTestWorktree builds a committed git repo used as the test
// worktree surface.
func newBranchTestWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	for p, c := range map[string]string{
		"server.go":      "package main\n",
		"server_test.go": "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "base")
	return dir
}

func writeTestFile(t *testing.T, dir, rel, content string) error {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0644)
}

// branchTaskWithVars builds a task whose Vars mark it as a parallel-branch
// child (branchID empty string = no branch vars).
func branchTaskWithVars(t *testing.T, branchID string) *entity.Task {
	t.Helper()
	task := &entity.Task{ID: "task-precheck-" + branchID, Title: "branch child", Status: entity.TaskStatusInProgress}
	if branchID != "" {
		task.Vars = map[string]string{
			workflowRunIDVar:    "wfr-test",
			workflowStepIDVar:   "parallel_workstreams",
			workflowBranchIDVar: branchID,
		}
	}
	return task
}

// TestPrecheckBranchJoinGateRejectsUndeclaredBusinessChange: a branch task
// whose single-step child workflow is terminal-now runs the join gate with
// the branch's output contract.
func TestPrecheckBranchJoinGateRejectsUndeclaredBusinessChange(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	wt := newBranchTestWorktree(t)
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-workstream_2", wt)
	if _, err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	// The task's real delta includes an undeclared business edit.
	if err := writeTestFile(t, wt, "server.go", "package main\n\nfunc Bad() {}\n"); err != nil {
		t.Fatal(err)
	}
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }

	// The child run: single-step branch definition, active step "start"
	// (terminal: no outgoing edges), with the branch's output contract.
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-branch-single",
		Name:        "Single-step branch",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID:   "start",
			Type: "agent_task",
			Title: "Branch work",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary", Description: "summary"},
				{Name: "touched_paths", Description: "paths"},
			},
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	if _, _, err := wfStore.StartRun("proj", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
	})
	if err == nil || !strings.Contains(err.Error(), "server.go") {
		t.Fatalf("precheck must reject undeclared business change, got: %v", err)
	}
}

// TestPrecheckBranchJoinGatePassesHonestDeclaration: the S2 dogfood's false
// positive is fixed — baseline-captured scaffolding noise does not fail an
// honest declaration.
func TestPrecheckBranchJoinGatePassesHonestDeclaration(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	wt := newBranchTestWorktree(t)
	// Scaffold noise exists at materialization time → inside the baseline.
	if err := writeTestFile(t, wt, ".mcp.json", "{}\n"); err != nil {
		t.Fatal(err)
	}
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-workstream_2", wt)
	if _, err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, wt, "server_test.go", "package main\n\nfunc TestX() {}\n"); err != nil {
		t.Fatal(err)
	}
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }

	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-branch-single-2",
		Name:        "Single-step branch 2",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID:   "start",
			Type: "agent_task",
			Title: "Branch work",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary", Description: "summary"},
				{Name: "touched_paths", Description: "paths"},
			},
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	if _, _, err := wfStore.StartRun("proj", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
	}); err != nil {
		t.Fatalf("honest declaration must pass precheck: %v", err)
	}
}

// TestPrecheckBranchJoinGateSkipsIntermediateStep: a multi-step branch
// workflow completing its FIRST step must not run the join gate — the
// completion advances within the branch and never reaches the parent join
// (GPT gate condition: multi-step branch support must be preserved).
func TestPrecheckBranchJoinGateSkipsIntermediateStep(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-branch-multi",
		Name:        "Multi-step branch",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "first",
		Steps: []entity.WorkflowStep{
			{ID: "first", Type: "agent_task", Title: "First", Position: entity.WorkflowPosition{X: 0, Y: 0}},
			{ID: "final", Type: "agent_task", Title: "Final",
				OutputFields: []entity.WorkflowField{{Name: "touched_paths", Description: "paths"}},
				Position:     entity.WorkflowPosition{X: 240, Y: 0}},
		},
		Edges:     []entity.WorkflowEdge{{ID: "e1", From: "first", To: "final"}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	if _, _, err := wfStore.StartRun("proj", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	// If the (wrong) join gate ran here it would fail closed: no worktree
	// is wired. Intermediate steps must skip it entirely.
	s.worktreeResolveOverride = func(project, taskID string) string { return "" }
	if err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{}); err != nil {
		t.Fatalf("intermediate branch step must skip the join gate, got: %v", err)
	}
}

// TestPrecheckBranchJoinGateSkipsFinalStepOfMultiStep: same multi-step
// branch, completing the FINAL step now — the gate must run (and reject
// here, since no worktree is observable).
func TestPrecheckBranchJoinGateRunsOnFinalStepOfMultiStep(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-branch-multi-2",
		Name:        "Multi-step branch 2",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "first",
		Steps: []entity.WorkflowStep{
			{ID: "first", Type: "agent_task", Title: "First", Position: entity.WorkflowPosition{X: 0, Y: 0}},
			{ID: "final", Type: "agent_task", Title: "Final",
				OutputFields: []entity.WorkflowField{{Name: "touched_paths", Description: "paths"}},
				Position:     entity.WorkflowPosition{X: 240, Y: 0}},
		},
		Edges:     []entity.WorkflowEdge{{ID: "e1", From: "first", To: "final"}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	if _, _, err := wfStore.StartRun("proj", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Advance to the final step.
	if _, err := wfStore.CompleteAndAdvance("proj", task.ID, "first done", "", map[string]string{}, "completed"); err != nil {
		t.Fatal(err)
	}
	// Final step + no observable worktree + gate runs → must fail closed.
	s.worktreeResolveOverride = func(project, taskID string) string { return "" }
	err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{"touched_paths": "x_test.go"})
	if err == nil {
		t.Fatal("final step of multi-step branch must run the join gate")
	}
}

// TestPrecheckBranchJoinGateSkipsNonBranchTask: plain workflow tasks (no
// branch vars) never touch the precheck — zero regression on the linear
// path.
func TestPrecheckBranchJoinGateSkipsNonBranchTask(t *testing.T) {
	s, _ := newPrecheckServer(t)
	task := branchTaskWithVars(t, "")
	if err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{}); err != nil {
		t.Fatalf("non-branch task must skip precheck, got: %v", err)
	}
}

// singleStepBranchDef builds the generated-style single-step branch
// definition (start step carries the branch output contract, no edges).
func singleStepBranchDef(id string) *entity.WorkflowDefinition {
	now := time.Now().UTC()
	return &entity.WorkflowDefinition{
		ID:          id,
		Name:        "Single-step branch",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID:   "start",
			Type: "agent_task",
			Title: "Branch work",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary", Description: "summary"},
				{Name: "touched_paths", Description: "paths"},
			},
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}
