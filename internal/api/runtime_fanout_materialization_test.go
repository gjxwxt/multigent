package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// buildFanoutGitWorkspace turns the sample project workspace into a real git
// repository so activateParallelWorkflowStep can materialize branch worktrees
// (the S2 v2 seam under test).
func buildFanoutGitWorkspace(t *testing.T, s *Server) (gitRoot string, baseCommit string) {
	t.Helper()
	gitRoot = filepath.Join(s.root, "projects", "sample", "workspace")
	if err := os.MkdirAll(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = gitRoot
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+gitRoot)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	for p, c := range map[string]string{
		"server.go":      "package main\n",
		"server_test.go": "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(gitRoot, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "base")
	out := run("rev-parse", "HEAD")
	return gitRoot, trimGitOut(out)
}

func trimGitOut(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// seedFanoutParentRun seeds a parent task + run at a start agent step whose
// next step is a parallel_stage with two branches (ws_a, ws_b). The parent
// task carries a frozen BaseCommit like an HTTP-created brownfield task.
func seedFanoutParentRun(t *testing.T, s *Server, workspaceID, baseCommit string) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-fanout-parent", Name: "Fanout parent", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID: "start", Type: "agent_task", Title: "Contract", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
			},
			{
				ID: "parallel", Type: "parallel_stage", Title: "Parallel", JoinPolicy: "all",
				Branches: []entity.WorkflowBranch{
					{
						ID: "ws_a", Title: "WS-1",
						OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
					},
					{
						ID: "ws_b", Title: "WS-2",
						OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
					},
				},
			},
		},
		Edges:     []entity.WorkflowEdge{{From: "start", To: "parallel"}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	parentTask := &entity.Task{
		ID: "task-fanout-root", Title: "Fanout root", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main",
		Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", parentTask); err != nil {
		t.Fatal(err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		// ActorRole keys: the start step + both branch start roles.
		"pm-agent": {Type: "agent", ID: "pm"},
		"ws_a":     {Type: "agent", ID: "pm"},
		"ws_b":     {Type: "agent", ID: "pm"},
		"parallel": {Type: "agent", ID: "pm"},
	}
	if _, _, err := wfStore.StartRun("sample", parentTask.ID, def.ID, bindings); err != nil {
		t.Fatal(err)
	}
	return parentTask
}

// TestFanoutMaterializesBranchWorktreesAndBaselines drives the REAL fan-out
// path (parent start step completes over HTTP → activateParallelWorkflowStep)
// and asserts the S2 v2 materialization seam: each branch task gets its own
// platform worktree at the frozen baseCommit, and a capture-time QA baseline
// is persisted to the control plane before the child run exists.
func TestFanoutMaterializesBranchWorktreesAndBaselines(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutParentRun(t, s, workspaceID, baseCommit)

	// The parent's start step completes over the production HTTP chain.
	// This must fan out and materialize both branch tasks.
	rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("parent start completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.BranchInstancesForStep(runIDForParent(t, s, workspaceID), "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("fan-out must create 2 branch instances, got %d", len(instances))
	}
	childIDs := map[string]string{}
	for _, inst := range instances {
		if inst.Status != "running" {
			t.Fatalf("branch %s must be running, got %q", inst.BranchID, inst.Status)
		}
		if inst.ChildTaskID == "" {
			t.Fatalf("branch %s instance missing ChildTaskID", inst.BranchID)
		}
		childIDs[inst.BranchID] = inst.ChildTaskID
	}
	if len(childIDs) != 2 {
		t.Fatalf("expected two distinct branches, got %v", childIDs)
	}

	seenWorktrees := map[string]bool{}
	for branchID, childID := range childIDs {
		task, err := s.ts.GetTask("sample", "pm", childID)
		if err != nil {
			t.Fatalf("branch child task %s: %v", childID, err)
		}
		if task.BaseCommit != baseCommit {
			t.Fatalf("branch %s BaseCommit = %q, want frozen %q", branchID, task.BaseCommit, baseCommit)
		}
		if task.WorktreeDir == "" {
			t.Fatalf("branch %s must have a materialized WorktreeDir", branchID)
		}
		if _, err := os.Stat(filepath.Join(task.WorktreeDir, ".git")); err != nil {
			t.Fatalf("branch %s worktree missing .git anchor: %v", branchID, err)
		}
		if seenWorktrees[task.WorktreeDir] {
			t.Fatalf("branches must get DISTINCT worktrees, %s shared", task.WorktreeDir)
		}
		seenWorktrees[task.WorktreeDir] = true
		// Capture-time baseline must exist in the control plane (the only
		// copy the join gate trusts).
		if _, err := os.Stat(filepath.Join(task.WorktreeDir, ".multigent", "qa_baseline_manifest.json")); err != nil {
			t.Fatalf("branch %s worktree missing qa baseline manifest: %v", branchID, err)
		}
		if _, ok, err := wfStore.LoadQABaselinePayload("sample", childID); err != nil || !ok {
			t.Fatalf("branch %s control-plane baseline missing: ok=%v err=%v", childID, ok, err)
		}
	}

	// Each branch agent edits ITS OWN worktree (business file, honestly
	// declared) and completes over HTTP; the parent must join.
	for branchID, childID := range childIDs {
		task, err := s.ts.GetTask("sample", "pm", childID)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(task.WorktreeDir, fmt.Sprintf("module_%s.go", branchID))
		if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if resolved := s.resolveTaskWorktreeDir("sample", childID); resolved != task.WorktreeDir {
			t.Fatalf("branch %s resolver drift: resolved=%q task=%q", branchID, resolved, task.WorktreeDir)
		}
		rec := postBranchStepComplete(t, s, workspaceID, childID, map[string]string{
			"branch_summary": "did things",
			"touched_paths":  fmt.Sprintf("module_%s.go", branchID),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("branch %s honest completion must be 200, got %d: %s", branchID, rec.Code, rec.Body.String())
		}
	}

	parentRun, found, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil || !found {
		t.Fatalf("parent run lookup: found=%v err=%v", found, err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("parent run must complete after both branches join, got %q@%q", parentRun.Status, parentRun.ActiveStepID)
	}
}

// TestFanoutMaterializationIsIdempotentOnReDrive: re-completing the parent
// start step (crash-window re-drive) must not duplicate branch instances or
// dirty the frozen baselines.
func TestFanoutMaterializationIsIdempotentOnReDrive(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutParentRun(t, s, workspaceID, baseCommit)

	for i := 0; i < 2; i++ {
		rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-root", map[string]string{
			"branch_summary": "contract frozen",
			"touched_paths":  "contract.md",
		})
		if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
			t.Fatalf("drive %d: unexpected status %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, _, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("re-drive must not duplicate branch instances, got %d", len(instances))
	}
}

// gitworktreeManagerForTest mirrors production wiring (server.go).
func gitworktreeManagerForTest() *gitworktree.Manager {
	return gitworktree.NewManager()
}

// runIDForParent resolves the parent run ID seeded by seedFanoutParentRun.
func runIDForParent(t *testing.T, s *Server, workspaceID string) string {
	t.Helper()
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", "task-fanout-root")
	if err != nil || !found {
		t.Fatalf("parent run lookup: found=%v err=%v", found, err)
	}
	return run.ID
}

// TestFanoutJoinRejectionKeepsWorktreeRetryable: a branch that reports an
// honest-but-undelivered delta (declaration/reality mismatch) must be
// rejected with the worktree STILL ON DISK so the agent can correct and
// re-report — the cleanup deferral (S2 v2 seam fix) is what makes the
// branch join retryable at all.
func TestFanoutJoinRejectionKeepsWorktreeRetryable(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutParentRun(t, s, workspaceID, baseCommit)

	rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("parent start: %d %s", rec.Code, rec.Body.String())
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, _, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel")
	if err != nil || len(instances) != 2 {
		t.Fatalf("instances: %v %d", err, len(instances))
	}
	childID := instances[0].ChildTaskID
	task, err := s.ts.GetTask("sample", "pm", childID)
	if err != nil {
		t.Fatal(err)
	}

	// Edit a business file but declare a DIFFERENT path — phantom rejection.
	if err := os.WriteFile(filepath.Join(task.WorktreeDir, "module_x.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, childID, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "module_wrong.go",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("phantom declaration must be rejected with 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// The worktree must survive the rejection (cleanup deferral).
	if _, err := os.Stat(filepath.Join(task.WorktreeDir, "module_x.go")); err != nil {
		t.Fatalf("worktree must survive a join rejection for retry: %v", err)
	}
	fresh, err := s.ts.GetTask("sample", "pm", childID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status.IsTerminal() {
		t.Fatalf("rejected branch task must stay non-terminal, got %q", fresh.Status)
	}

	// Corrected re-report: declare honestly.
	rec = postBranchStepComplete(t, s, workspaceID, childID, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "module_x.go",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("corrected completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// After the join is accepted, the worktree is retired.
	if _, err := os.Stat(task.WorktreeDir); !os.IsNotExist(err) {
		t.Fatalf("accepted branch worktree must be cleaned up, stat err=%v", err)
	}
}
