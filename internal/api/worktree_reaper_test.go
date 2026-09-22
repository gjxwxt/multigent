package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
)

// TestWorktreeReaperSweep_TerminalOnly locks the sweep's safety envelope:
// terminal tasks get reclaimed, non-terminal tasks are untouched, and
// recordless directories are left for explicit orphan cleanup.
func TestWorktreeReaperSweep_TerminalOnly(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	_ = workspaceID
	s.worktreeMgr = gitworktree.NewManager()
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	gitRoot := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	if err := os.MkdirAll(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	seedWorkspaceGitRepo(t, gitRoot)

	mkTask := func(id string, status entity.TaskStatus) *entity.Task {
		return &entity.Task{ID: id, Title: "t " + id, Status: status}
	}
	terminal := mkTask("t-20260922-reap1", entity.TaskStatusDoneSuccess)
	active := mkTask("t-20260922-keep1", entity.TaskStatusInProgress)
	if err := s.ts.AddTask("sample", "worker", terminal); err != nil {
		t.Fatalf("add terminal task: %v", err)
	}
	if err := s.ts.AddTask("sample", "worker", active); err != nil {
		t.Fatalf("add active task: %v", err)
	}

	mkWorktree := func(t *testing.T, taskID string) string {
		t.Helper()
		dir, _, err := s.worktreeMgr.EnsureWorktree(gitRoot, taskID, "main", "feat/"+taskID)
		if err != nil {
			t.Fatalf("EnsureWorktree %s: %v", taskID, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	terminalDir := mkWorktree(t, terminal.ID)
	activeDir := mkWorktree(t, active.ID)
	recordless := filepath.Join(gitRoot, ".multigent", "worktrees", "t-20260922-norecord")
	if err := os.MkdirAll(recordless, 0o755); err != nil {
		t.Fatal(err)
	}

	s.sweepTerminalTaskWorktrees(t.Context())

	if _, err := os.Stat(terminalDir); !os.IsNotExist(err) {
		t.Fatalf("terminal task worktree should be reclaimed, stat err=%v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("active task worktree must be untouched: %v", err)
	}
	if _, err := os.Stat(recordless); err != nil {
		t.Fatalf("recordless dir must be left for explicit orphan cleanup: %v", err)
	}

	// Idempotent: a second sweep must not error or resurrect anything.
	s.sweepTerminalTaskWorktrees(t.Context())
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("active task worktree must survive second sweep: %v", err)
	}
}

// worktreeReaperStartupDelay is not exercised here — the loop timer is only
// wired in startWorktreeReaper; the sweep itself is what needs determinism.
func TestWorktreeReaperSweep_CheckpointsDirtyWorktree(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	_ = workspaceID
	s.worktreeMgr = gitworktree.NewManager()
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	gitRoot := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	if err := os.MkdirAll(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	seedWorkspaceGitRepo(t, gitRoot)

	task := &entity.Task{ID: "t-20260922-dirty1", Title: "dirty", Status: entity.TaskStatusDoneSuccess}
	if err := s.ts.AddTask("sample", "worker", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	dir, _, err := s.worktreeMgr.EnsureWorktree(gitRoot, task.ID, "main", "feat/"+task.ID)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.sweepTerminalTaskWorktrees(t.Context())

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dirty terminal worktree should still be reclaimed, stat err=%v", err)
	}
	// The uncommitted file must survive as a checkpoint commit on the branch.
	var out strings.Builder
	cmd := exec.Command("git", "show", "feat/"+task.ID+":uncommitted.txt")
	cmd.Dir = gitRoot
	out.Reset()
	if b, err := cmd.Output(); err != nil || strings.TrimSpace(string(b)) != "precious" {
		t.Fatalf("checkpoint commit must preserve uncommitted work on the branch (err=%v, out=%q)", err, string(b))
	}
}
