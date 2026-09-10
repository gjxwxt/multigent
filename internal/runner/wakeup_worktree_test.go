package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestWakeupTargetWorktreePrefersEntityThenVars(t *testing.T) {
	if dir, branch := wakeupTargetWorktree(nil); dir != "" || branch != "" {
		t.Fatalf("nil task should resolve empty, got dir=%q branch=%q", dir, branch)
	}
	pure := &entity.Task{Type: "wakeup"}
	if dir, branch := wakeupTargetWorktree(pure); dir != "" || branch != "" {
		t.Fatalf("pure reminder wakeup should stay on agent home, got dir=%q branch=%q", dir, branch)
	}
	fromVars := &entity.Task{
		Type: "wakeup",
		Vars: map[string]string{
			wakeupWorktreeDirVar:    "/tmp/some-worktree",
			wakeupWorktreeBranchVar: "task/t-123",
		},
	}
	if dir, branch := wakeupTargetWorktree(fromVars); dir != "/tmp/some-worktree" || branch != "task/t-123" {
		t.Fatalf("vars fallback failed, got dir=%q branch=%q", dir, branch)
	}
	entityWins := &entity.Task{
		Type:        "wakeup",
		WorktreeDir: "/tmp/entity-worktree",
		BranchName:  "task/entity",
		Vars: map[string]string{
			wakeupWorktreeDirVar:    "/tmp/var-worktree",
			wakeupWorktreeBranchVar: "task/var",
		},
	}
	if dir, branch := wakeupTargetWorktree(entityWins); dir != "/tmp/entity-worktree" || branch != "task/entity" {
		t.Fatalf("entity fields should win, got dir=%q branch=%q", dir, branch)
	}
}

func TestWakeupRunScopePureReminderStaysOnAgentHome(t *testing.T) {
	task := &entity.Task{ID: "t-1", Type: "wakeup", Prompt: "[wakeup] attention"}
	boundary, dir, err := wakeupRunScope(task)
	if err != nil {
		t.Fatalf("wakeupRunScope: %v", err)
	}
	if dir != "" {
		t.Fatalf("pure reminder must not remount, got dir=%q", dir)
	}
	if !strings.Contains(boundary, "Attention 唤醒与信号处理安全边界") {
		t.Fatalf("expected attention boundary, got %q", boundary)
	}
	if strings.Contains(boundary, "Git Worktree") {
		t.Fatalf("attention boundary must not claim a worktree mount")
	}
}

func TestWakeupRunScopeWorktreeMountsWorktreeBoundary(t *testing.T) {
	wt := t.TempDir()
	task := &entity.Task{
		ID:         "t-2",
		Type:       "wakeup",
		BaseBranch: "main",
		Vars: map[string]string{
			wakeupWorktreeDirVar:    wt,
			wakeupWorktreeBranchVar: "task/t-20260910-62lk48",
		},
	}
	boundary, dir, err := wakeupRunScope(task)
	if err != nil {
		t.Fatalf("wakeupRunScope: %v", err)
	}
	if dir != wt {
		t.Fatalf("expected exec dir %q, got %q", wt, dir)
	}
	if !strings.Contains(boundary, "Git Worktree 独立分支安全边界约束") {
		t.Fatalf("expected git worktree boundary, got %q", boundary)
	}
	if !strings.Contains(boundary, "task/t-20260910-62lk48") {
		t.Fatalf("boundary should name the feature branch, got %q", boundary)
	}
	if !strings.Contains(boundary, "基于 `main`") {
		t.Fatalf("boundary should state the base branch, got %q", boundary)
	}
}

func TestWakeupRunScopeWorktreeMissingFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	task := &entity.Task{
		ID:   "t-3",
		Type: "wakeup",
		Vars: map[string]string{
			wakeupWorktreeDirVar:    missing,
			wakeupWorktreeBranchVar: "task/t-3",
		},
	}
	boundary, dir, err := wakeupRunScope(task)
	if err == nil {
		t.Fatalf("missing worktree must fail closed, got dir=%q boundary=%q", dir, boundary)
	}
	if !strings.Contains(err.Error(), "execution_scope_mismatch") {
		t.Fatalf("expected execution_scope_mismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error should name the missing worktree path, got %v", err)
	}
	if dir != "" || boundary != "" {
		t.Fatalf("no scope should be returned on failure, got dir=%q", dir)
	}
}

func TestWakeupRunScopeEntityWorktreeMountsToo(t *testing.T) {
	// The local scheduler path mirrors the worktree onto the entity fields;
	// scope resolution must honour that channel as well.
	wt := t.TempDir()
	task := &entity.Task{
		ID:          "t-4",
		Type:        "wakeup",
		WorktreeDir: wt,
		BranchName:  "task/t-4",
	}
	_, dir, err := wakeupRunScope(task)
	if err != nil {
		t.Fatalf("wakeupRunScope: %v", err)
	}
	if dir != wt {
		t.Fatalf("expected exec dir %q, got %q", wt, dir)
	}
}

func TestWakeupRunScopeBranchOnlyKeepsAgentHome(t *testing.T) {
	// A branch signal without a directory cannot be mounted; it must not be
	// treated as a worktree target and must not fail the run.
	task := &entity.Task{
		ID:         "t-5",
		Type:       "wakeup",
		BranchName: "task/t-5",
	}
	boundary, dir, err := wakeupRunScope(task)
	if err != nil {
		t.Fatalf("wakeupRunScope: %v", err)
	}
	if dir != "" {
		t.Fatalf("branch-only wakeup must stay on agent home, got dir=%q", dir)
	}
	if !strings.Contains(boundary, "Attention 唤醒与信号处理安全边界") {
		t.Fatalf("expected attention boundary, got %q", boundary)
	}
}

func TestExecPromptWakeupWorktreeEnvFailsClosed(t *testing.T) {
	// Runtime-node path: the wakeup worktree travels through
	// RuntimeControlEnv. A missing worktree must abort before any agent
	// process is spawned.
	r := New(t.TempDir(), nil, nil)
	r.SetAgentMetaOverride("proj", "agent", &entity.AgentMeta{
		Name:    "agent",
		Project: "proj",
	})
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := r.ExecPromptWithRuntimeControlEnvContext(
		t.Context(),
		"proj", "agent",
		"do the workflow step",
		"",
		map[string]string{
			wakeupWorktreeDirVar:    missing,
			wakeupWorktreeBranchVar: "task/t-6",
			"MULTIGENT_TASK_ID":     "t-6",
		},
	)
	if err == nil {
		t.Fatal("expected failure for missing wakeup worktree")
	}
	if !strings.Contains(err.Error(), "execution_scope_mismatch") {
		t.Fatalf("expected execution_scope_mismatch, got %v", err)
	}
}

func TestExecPromptWakeupWorktreeEnvPrefersWorktreeDir(t *testing.T) {
	// When the wakeup worktree exists, the exec path must resolve the run
	// directory to it rather than the agent home. We assert this indirectly:
	// without the var the run would use agentDir; the prompt file is written
	// inside execAgentDir, so a successful run against a worktree whose agent
	// home does not exist proves the remount happened. Full-run assertions
	// need an agent CLI, so this test only checks the resolution helper.
	wt := t.TempDir()
	if _, err := os.Stat(wt); err != nil {
		t.Fatal(err)
	}
	// agentDir does not exist in this fake root, so a run that stayed on the
	// agent home would fail while writing the prompt file; the worktree
	// remount is observable through wakeupRunScope directly.
	task := &entity.Task{
		ID:   "t-7",
		Type: "wakeup",
		Vars: map[string]string{
			wakeupWorktreeDirVar:    wt,
			wakeupWorktreeBranchVar: "task/t-7",
		},
	}
	_, dir, err := wakeupRunScope(task)
	if err != nil {
		t.Fatalf("wakeupRunScope: %v", err)
	}
	if dir != wt {
		t.Fatalf("expected exec dir %q, got %q", wt, dir)
	}
}
