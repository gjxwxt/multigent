package workflow

// Regression suite for fix round S2-1 (Fix A + Fix B), encoding the review
// gate's acceptance list: gate rejections must leave zero terminal state
// behind, corrected re-reports succeed and duplicates cannot double-advance,
// multi-step branches keep intermediate completions intra-branch, the
// baseline delta catches committed changes and same-path re-modification,
// and the scaffold-noise false positive from the S2 dogfood is gone.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/gitworktree"
)

// gitCommand builds a git command with the same env isolation the shared
// worktree fixture uses (the fixture's run helper is file-local there).
func gitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	return cmd
}

// TestQABaselineDeltaIgnoresPlatformScaffolding is the direct S2 dogfood
// repro: scaffold noise (.cursor/, .mcp.json, stray docs) that predates the
// task must NOT make the gate demand its declaration, while the task's own
// real change must still be required.
func TestQABaselineDeltaIgnoresPlatformScaffolding(t *testing.T) {
	wt := newGitWorktree(t)
	// Platform materialization noise, present BEFORE baseline capture.
	for _, p := range []string{".cursor/settings.json", ".mcp.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, p)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, p), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatalf("capture baseline: %v", err)
	}
	// The task's only change: a test file.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt); err != nil {
		t.Fatalf("scaffold noise must not fail an honest completion: %v", err)
	}
	// The task's change is still mandatory to declare.
	err := verifyQATouchedPathsAgainstWorktree("none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("undeclared real change must still fail, got: %v", err)
	}
}

// TestQABaselineDeltaCatchesCommittedChange covers GPT's B rejection:
// committed changes empty the git status, so a path-set-only comparison
// would miss them. The fingerprinted baseline still reports the file as
// changed (baseline fingerprint != committed content fingerprint).
func TestQABaselineDeltaCatchesCommittedChange(t *testing.T) {
	wt := newGitWorktree(t)
	if err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := gitCommand(t, wt, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	// The task commits its work: status is now clean.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestCommitted() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "task work")
	if out, err := gitCommand(t, wt, "status", "--porcelain").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("test setup: worktree should be clean after commit, got %q (%v)", out, err)
	}
	// Declaring "none" must STILL fail: the committed delta is a delivery.
	err := verifyQATouchedPathsAgainstWorktree("none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("committed change must not escape the gate, got: %v", err)
	}
	// Declaring it honestly passes the cross-check (whitelist then rules).
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt); err != nil {
		t.Fatalf("honest committed-change declaration must pass: %v", err)
	}
}

// TestQABaselineDeltaCatchesSamePathReModification covers the second half of
// GPT's B rejection: a file modified before the task (baseline captured the
// modified content) and modified AGAIN by the task must be reported as
// changed even though its path was already in the baseline.
func TestQABaselineDeltaCatchesSamePathReModification(t *testing.T) {
	wt := newGitWorktree(t)
	// Dirty BEFORE baseline: server_test.go already modified vs HEAD.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc PreExisting() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	// The task modifies the SAME file again.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TaskEdit() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyQATouchedPathsAgainstWorktree("none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("same-path re-modification must be reported, got: %v", err)
	}
	if err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt); err != nil {
		t.Fatalf("declaring the re-modification must pass: %v", err)
	}
}

// TestQABaselineDeltaCatchesDeletion: a baseline file deleted by the task
// surfaces as a delta path under its original name.
func TestQABaselineDeltaCatchesDeletion(t *testing.T) {
	wt := newGitWorktree(t)
	if err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(wt, "server_test.go")); err != nil {
		t.Fatal(err)
	}
	err := verifyQATouchedPathsAgainstWorktree("none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("deletion must be reported, got: %v", err)
	}
}

// TestQAGateCorruptBaselineFailsClosed: a corrupt baseline document must
// NOT silently downgrade to absolute measurement (that downgrade would be
// exploitable); the gate fails closed instead.
func TestQAGateCorruptBaselineFailsClosed(t *testing.T) {
	wt := newGitWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, ".multigent", "qa_baseline.json"), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), "qa baseline") {
		t.Fatalf("corrupt baseline must fail closed, got: %v", err)
	}
}

// TestQAGateNoBaselineFallsBackAbsolute: worktrees created before the fix
// have no baseline; the gate falls back to the previous absolute status
// measurement (still fail-closed, still strict).
func TestQAGateNoBaselineFallsBackAbsolute(t *testing.T) {
	wt := newGitWorktree(t)
	if err := os.Remove(filepath.Join(wt, ".multigent", "qa_baseline.json")); err != nil {
		t.Fatal(err)
	}
	// Scaffold noise now FAILS again (absolute surface) — documented
	// degradation for legacy worktrees, never a silent pass.
	if err := os.WriteFile(filepath.Join(wt, ".mcp.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyQATouchedPathsAgainstWorktree("server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("legacy worktree without baseline must use absolute status, got: %v", err)
	}
}
