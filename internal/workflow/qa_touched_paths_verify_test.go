package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/gitworktree"
)

func newGitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "server.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server_test.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")
	// S2-1 Fix B: production worktrees get a baseline captured at the
	// protected materialization point (before any agent runs); tests must
	// mirror that so the gate measures the delivery delta, not absolute
	// status. Capture BEFORE the test dirties the tree.
	if _, err := gitworktree.CaptureQABaseline(dir); err != nil {
		t.Fatalf("capture qa baseline: %v", err)
	}
	return dir
}

func TestVerifyTouchedPathsUndeclaredRealChangeFails(t *testing.T) {
	wt := newGitWorktree(t)
	// QA edited a BUSINESS file without declaring it.
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc Bad() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	declared := "server_test.go"
	err := verifyQATouchedPathsAgainstWorktree(declared, wt)
	if err == nil || !strings.Contains(err.Error(), "not declared") || !strings.Contains(err.Error(), "server.go") {
		t.Fatalf("undeclared business change must fail, got: %v", err)
	}
	// Declaring "none" with a dirty tree is the same lie.
	err = verifyQATouchedPathsAgainstWorktree("none", wt)
	if err == nil || !strings.Contains(err.Error(), "server.go") {
		t.Fatalf("none-with-dirty-tree must fail, got: %v", err)
	}
}

func TestVerifyTouchedPathsPhantomDeclarationFails(t *testing.T) {
	wt := newGitWorktree(t)
	// QA declares a test-file edit that never happened.
	err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), "no worktree change") {
		t.Fatalf("phantom declaration must fail, got: %v", err)
	}
}

func TestVerifyTouchedPathsBusinessChangeLaunderingFails(t *testing.T) {
	wt := newGitWorktree(t)
	// QA edits a business file AND declares it honestly — the whitelist
	// must still reject it (declaration cannot launder a path).
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc X() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyQATouchedPathsAgainstWorktree("server.go", wt)
	if err == nil || !strings.Contains(err.Error(), "test-artifact rule") {
		t.Fatalf("honestly-declared business change must still fail the whitelist, got: %v", err)
	}
}

func TestVerifyTouchedPathsHappyPathPasses(t *testing.T) {
	wt := newGitWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX(t *testing.T) {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt); err != nil {
		t.Fatalf("matching declared+real test edit must pass: %v", err)
	}
	// Clean tree + none also passes.
	wt2 := newGitWorktree(t)
	if err := verifyQATouchedPathsAgainstWorktree("none", wt2); err != nil {
		t.Fatalf("clean tree with none must pass: %v", err)
	}
}

func TestVerifyTouchedPathsUntrackedAndRename(t *testing.T) {
	wt := newGitWorktree(t)
	// New untracked test file must be declared.
	if err := os.WriteFile(filepath.Join(wt, "extra_test.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt); err == nil {
		t.Fatal("untracked file must count as a real change")
	}
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go\nextra_test.go", wt); err == nil {
		t.Fatal("phantom server_test.go still fails even with untracked declared")
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestY() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go\nextra_test.go", wt); err != nil {
		t.Fatalf("declared+real (modified+untracked) must pass: %v", err)
	}
}
