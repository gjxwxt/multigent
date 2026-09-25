package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// D-N regression fixtures for the production worktree resolver: a non-git
// agent directory must not shadow later observable candidates, and a
// DECLARED worktree that went missing is returned as-is (the gate fails
// closed on it; worktree loss ≠ "no code worktree").

func observableGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")
	return dir
}

func seedResolverTask(t *testing.T, s *Server, project, agent, taskID, worktreeDir string) {
	t.Helper()
	if err := s.ts.AddTask(project, agent, &entity.Task{
		ID: taskID, Title: "resolver fixture", Status: entity.TaskStatusInProgress,
		WorktreeDir: worktreeDir,
	}); err != nil {
		t.Fatalf("persist task: %v", err)
	}
}

// The agent home dir (agents/<agent>) exists but is not a git repository:
// the resolver must skip it and land on the observable workspace instead of
// wedging the linear QA gate behind "requires an observable worktree".
func TestResolveTaskWorktreeSkipsNonGitAgentDir(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	// No task anywhere: findTaskInProject misses, agent fallback needs an
	// agent name — exercise the fallback chain via an existing task with an
	// empty WorktreeDir.
	agentHome := filepath.Join(s.st.ProjectDir("sample"), "agents", "Nora")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Make the workspace observable (git repo); agent home stays non-git.
	replaceGitDir := observableGitDir(t)
	if err := os.Rename(filepath.Join(replaceGitDir, ".git"), filepath.Join(wsDir, ".git")); err != nil {
		t.Fatal(err)
	}
	seedResolverTask(t, s, "sample", "Nora", "t-dn-root", "")

	res := s.resolveTaskWorktreeDir("sample", "t-dn-root")
	if res != wsDir {
		t.Fatalf("resolver must land on the observable workspace, got %q", res)
	}
}

// A declared WorktreeDir that no longer exists on disk must be returned
// as-is so the gate keeps failing closed on the missing worktree.
func TestResolveTaskWorktreeKeepsMissingDeclaredWorktreeFailClosed(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	missing := filepath.Join(s.st.ProjectDir("sample"), "worktrees", "gone")
	seedResolverTask(t, s, "sample", "Nora", "t-dn-lost", missing)

	res := s.resolveTaskWorktreeDir("sample", "t-dn-lost")
	if res != missing {
		t.Fatalf("declared-but-missing worktree must be returned as-is, got %q", res)
	}
}

// No observable candidate anywhere (no workspace, non-git agent dir, no
// declared worktree) → the resolver reports "no code worktree" honestly.
func TestResolveTaskWorktreeReportsEmptyWhenNothingObservable(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	agentHome := filepath.Join(s.st.ProjectDir("sample"), "agents", "Nora")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	seedResolverTask(t, s, "sample", "Nora", "t-dn-none", "")

	if res := s.resolveTaskWorktreeDir("sample", "t-dn-none"); res != "" {
		t.Fatalf("no observable candidate must resolve to empty, got %q", res)
	}
}
