package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	gitworktree "github.com/multigent/multigent/internal/gitworktree"
)

// newReviewCommitRepo builds a minimal git repo (initial commit on main) for
// review-commit path tests.
func newReviewCommitRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test Runner")
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial commit")
	return tmpDir
}

// Batch 3 / round-14: the review-commit path commits a working tree an agent
// could have written. If the repo config carries core.hooksPath pointing at
// agent-controlled scripts (or fsmonitor/external-diff drivers), a bare
// `git add`/`git commit` executes them on the host. The sanitized invocation
// must neutralize that config: the probe script must NOT run, and the commit
// must still succeed.
func TestCommitAndPushReviewChangesDoesNotExecuteRepoConfig(t *testing.T) {
	repo := newReviewCommitRepo(t)

	probe := filepath.Join(repo, ".probe-ran")
	hooks := filepath.Join(repo, "evil-hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(hooks, "pre-commit")
	shell := "#!/bin/sh\ntouch " + probe + "\n"
	if err := os.WriteFile(script, []byte(shell), 0755); err != nil {
		t.Fatal(err)
	}
	setCfg := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %v: %v (%s)", args, err, string(out))
		}
	}
	setCfg("config", "core.hooksPath", hooks)
	setCfg("config", "core.fsmonitor", "/bin/touch") // fsmonitor.<something> style abuse guard: any external driver must not run

	s, _ := newConnectionGrantPolicyServer(t)
	task := &entity.Task{ID: "t-poisoned", BranchName: "main", WorktreeDir: repo}
	if err := os.WriteFile(filepath.Join(repo, "copilot_fix.txt"), []byte("reviewed fix"), 0644); err != nil {
		t.Fatal(err)
	}

	s.commitAndPushReviewChanges("sample", "pm", task)

	if _, err := os.Stat(probe); err == nil {
		t.Fatal("repo-controlled hook executed on the host during review commit")
	}
	logCmd := exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = repo
	out, _ := logCmd.Output()
	if !strings.Contains(string(out), "user in-context preview feedback fixes") {
		t.Fatalf("expected the review commit despite neutralized config, got %s", string(out))
	}
}

// Batch 3: the review commit holds the cross-process project Git lock for its
// whole window — a concurrent AcquireProjectLock holder must exclude it.
func TestCommitAndPushReviewChangesRespectsProjectLock(t *testing.T) {
	repo := newReviewCommitRepo(t)
	s, _ := newConnectionGrantPolicyServer(t)
	task := &entity.Task{ID: "t-lock", BranchName: "main", WorktreeDir: repo}
	if err := os.WriteFile(filepath.Join(repo, "copilot_fix.txt"), []byte("reviewed fix"), 0644); err != nil {
		t.Fatal(err)
	}

	unlock, err := gitworktree.AcquireProjectLock(repo)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.commitAndPushReviewChanges("sample", "pm", task)
	}()

	select {
	case <-done:
		t.Fatal("review commit ran while the project lock was held elsewhere")
	case <-time.After(700 * time.Millisecond):
		// Still blocked — correct. Release and let it proceed.
	}

	// Confirm it has NOT committed while the lock was held.
	logCmd := exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = repo
	out, _ := logCmd.Output()
	if strings.Contains(string(out), "user in-context preview feedback fixes") {
		t.Fatal("review commit landed while the project lock was held")
	}

	unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("review commit did not finish after the lock was released")
	}
	logCmd = exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = repo
	out, _ = logCmd.Output()
	if !strings.Contains(string(out), "user in-context preview feedback fixes") {
		t.Fatalf("expected commit after lock release, got %s", string(out))
	}
}

// Concurrent review commits over the same project must serialize on the lock
// and never corrupt the index or lose a staged fix. Note: a fix may be swept
// into the other caller's commit (both write before either acquires the
// lock, and `git add -A` stages everything) — the invariant is that BOTH
// files are committed and the repo stays consistent, not a fixed commit count.
func TestCommitAndPushReviewChangesConcurrentSerialize(t *testing.T) {
	repo := newReviewCommitRepo(t)
	s, _ := newConnectionGrantPolicyServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			task := &entity.Task{
				ID:          "t-race-" + time.Now().Format("150405") + "-" + string(rune('a'+n)),
				BranchName:  "main",
				WorktreeDir: repo,
			}
			_ = os.WriteFile(filepath.Join(repo, "fix_"+string(rune('a'+n))+".txt"), []byte("fix"), 0644)
			s.commitAndPushReviewChanges("sample", "pm", task)
		}(i)
	}
	wg.Wait()

	for _, f := range []string{"fix_a.txt", "fix_b.txt"} {
		showCmd := exec.Command("git", "ls-files", f)
		showCmd.Dir = repo
		out, err := showCmd.Output()
		if err != nil || !strings.Contains(string(out), f) {
			t.Fatalf("concurrent review commits lost %s (ls-files: %s, err=%v)", f, string(out), err)
		}
	}
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = repo
	statusOut, _ := statusCmd.Output()
	if len(strings.TrimSpace(string(statusOut))) > 0 {
		t.Fatalf("working tree not clean after serialized review commits: %s", string(statusOut))
	}
}
