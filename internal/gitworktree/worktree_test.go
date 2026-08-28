package gitworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitWorktreeLifecycle(t *testing.T) {
	// Create a temporary git repository
	tempDir, err := os.MkdirTemp("", "gitworktree-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Init git repo
	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")

	// Create initial commit
	readme := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readme, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "initial commit")

	mgr := NewManager()
	taskID := "task-test-123"

	// 1. Ensure worktree
	wtDir, branch, err := mgr.EnsureWorktree(tempDir, taskID, "main", "feature/task-test-123")
	if err != nil {
		t.Fatalf("EnsureWorktree failed: %v", err)
	}
	if branch != "feature/task-test-123" {
		t.Fatalf("expected feature branch, got %q", branch)
	}

	if _, err := os.Stat(filepath.Join(wtDir, "README.md")); err != nil {
		t.Fatalf("worktree README.md not found: %v", err)
	}

	// 2. Modify file in worktree
	newFile := filepath.Join(wtDir, "feature.txt")
	if err := os.WriteFile(newFile, []byte("feature content\n"), 0644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGit(t, wtDir, "add", "feature.txt")
	runGit(t, wtDir, "commit", "-m", "add feature")

	// 3. Ensure worktree idempotent
	wtDir2, branch2, err := mgr.EnsureWorktree(tempDir, taskID, "main", "feature/task-test-123")
	if err != nil {
		t.Fatalf("EnsureWorktree second time failed: %v", err)
	}
	if wtDir2 != wtDir {
		t.Fatalf("expected same worktree dir, got %s != %s", wtDir2, wtDir)
	}
	if branch2 != branch {
		t.Fatalf("expected same branch, got %q != %q", branch2, branch)
	}

	// 4. List worktrees
	list, err := mgr.ListWorktrees(tempDir)
	if err != nil {
		t.Fatalf("ListWorktrees failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 worktree in list, got %d", len(list))
	}

	// 5. Cleanup worktree
	if err := mgr.CleanupWorktree(tempDir, taskID); err != nil {
		t.Fatalf("CleanupWorktree failed: %v", err)
	}

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists after cleanup")
	}
}

func TestEnsureWorktreeReturnsGeneratedBranch(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-generated-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	readme := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readme, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "initial commit")

	wtDir, branch, err := NewManager().EnsureWorktree(tempDir, "task-generated", "main", "")
	if err != nil {
		t.Fatalf("EnsureWorktree failed: %v", err)
	}
	if branch != "feature/task-generated" {
		t.Fatalf("expected generated branch, got %q", branch)
	}
	if wtDir != WorktreeDir(tempDir, "task-generated") {
		t.Fatalf("unexpected worktree dir: %q", wtDir)
	}
}

func TestResolveBaseCommitUsesFetchedRemoteRevision(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-remote-base-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	remoteDir := filepath.Join(tempDir, "origin.git")
	rootDir := filepath.Join(tempDir, "repo")
	runGit(t, tempDir, "init", "--bare", remoteDir)
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}
	runGit(t, rootDir, "init", "-b", "main")
	runGit(t, rootDir, "config", "user.email", "test@multigent.ai")
	runGit(t, rootDir, "config", "user.name", "Multigent Tester")
	readme := filepath.Join(rootDir, "README.md")
	if err := os.WriteFile(readme, []byte("initial\n"), 0644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGit(t, rootDir, "add", "README.md")
	runGit(t, rootDir, "commit", "-m", "initial")
	runGit(t, rootDir, "remote", "add", "origin", remoteDir)
	runGit(t, rootDir, "push", "-u", "origin", "main")
	remoteBase := strings.TrimSpace(string(runGitOutput(t, rootDir, "rev-parse", "HEAD")))

	// Advance the local branch without pushing. A new task must still use the
	// freshly fetched remote base, not this stale local branch.
	if err := os.WriteFile(readme, []byte("local-only\n"), 0644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	runGit(t, rootDir, "add", "README.md")
	runGit(t, rootDir, "commit", "-m", "local only")

	mgr := NewManager()
	resolved, err := mgr.ResolveBaseCommit(rootDir, "main")
	if err != nil {
		t.Fatalf("ResolveBaseCommit failed: %v", err)
	}
	if resolved != remoteBase {
		t.Fatalf("resolved base = %s, want remote %s", resolved, remoteBase)
	}

	wtDir, _, err := mgr.EnsureWorktreeAt(rootDir, "task-remote-base", resolved, "feature/task-remote-base")
	if err != nil {
		t.Fatalf("EnsureWorktreeAt failed: %v", err)
	}
	defer mgr.CleanupWorktree(rootDir, "task-remote-base")
	checkedOut := strings.TrimSpace(string(runGitOutput(t, wtDir, "rev-parse", "HEAD")))
	if checkedOut != remoteBase {
		t.Fatalf("worktree HEAD = %s, want remote %s", checkedOut, remoteBase)
	}
}

func TestCaptureSnapshotRequiresCleanWorktree(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-snapshot-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	file := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(file, []byte("clean\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "initial")

	mgr := NewManager()
	commit, err := mgr.CaptureSnapshot(tempDir)
	if err != nil || len(commit) != 40 {
		t.Fatalf("CaptureSnapshot clean result = %q, %v", commit, err)
	}
	if err := os.WriteFile(file, []byte("dirty\n"), 0644); err != nil {
		t.Fatalf("modify file: %v", err)
	}
	if _, err := mgr.CaptureSnapshot(tempDir); err == nil {
		t.Fatal("expected dirty worktree snapshot to fail")
	}
}

func TestEnsureSnapshotWorktreePinsCompletionCommit(t *testing.T) {
	tempDir := t.TempDir()
	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	file := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(file, []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "v1")
	completion := strings.TrimSpace(string(runGitOutput(t, tempDir, "rev-parse", "HEAD")))
	if err := os.WriteFile(file, []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tempDir, "commit", "-am", "v2")

	mgr := NewManager()
	snapshot, err := mgr.EnsureSnapshotWorktree(tempDir, "task-completed", completion)
	if err != nil {
		t.Fatalf("EnsureSnapshotWorktree: %v", err)
	}
	defer mgr.CleanupWorktree(tempDir, "task-completed")
	got := strings.TrimSpace(string(runGitOutput(t, snapshot, "rev-parse", "HEAD")))
	if got != completion {
		t.Fatalf("snapshot HEAD = %s, want %s", got, completion)
	}
	if content, err := os.ReadFile(filepath.Join(snapshot, "README.md")); err != nil || string(content) != "v1\n" {
		t.Fatalf("snapshot content = %q, err=%v", content, err)
	}
}

func TestPushBranchVerifiesRemoteSHA(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-push-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	remoteDir := filepath.Join(tempDir, "origin.git")
	rootDir := filepath.Join(tempDir, "repo")
	runGit(t, tempDir, "init", "--bare", remoteDir)
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}
	runGit(t, rootDir, "init", "-b", "main")
	runGit(t, rootDir, "config", "user.email", "test@multigent.ai")
	runGit(t, rootDir, "config", "user.name", "Multigent Tester")
	file := filepath.Join(rootDir, "README.md")
	if err := os.WriteFile(file, []byte("base\n"), 0644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	runGit(t, rootDir, "add", "README.md")
	runGit(t, rootDir, "commit", "-m", "initial")
	runGit(t, rootDir, "remote", "add", "origin", remoteDir)
	runGit(t, rootDir, "push", "-u", "origin", "main")

	_, branch, err := NewManager().EnsureWorktree(rootDir, "task-push", "main", "feature/task-push")
	if err != nil {
		t.Fatalf("EnsureWorktree failed: %v", err)
	}
	defer NewManager().CleanupWorktree(rootDir, "task-push")
	wtDir := WorktreeDir(rootDir, "task-push")
	if err := os.WriteFile(filepath.Join(wtDir, "feature.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGit(t, wtDir, "add", "feature.txt")
	runGit(t, wtDir, "commit", "-m", "feature")
	commit := strings.TrimSpace(string(runGitOutput(t, wtDir, "rev-parse", "HEAD")))

	if err := NewManager().PushBranch(rootDir, branch, commit); err != nil {
		t.Fatalf("PushBranch failed: %v", err)
	}
	remote := strings.TrimSpace(string(runGitOutput(t, rootDir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)))
	if !strings.HasPrefix(remote, commit+"\t") {
		t.Fatalf("remote ref = %q, want commit %s", remote, commit)
	}
}

func TestIsAncestorDistinguishesSquashLikeHistory(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-ancestor-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	file := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(file, []byte("base\n"), 0644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "base")
	base := strings.TrimSpace(string(runGitOutput(t, tempDir, "rev-parse", "HEAD")))
	if err := os.WriteFile(file, []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGit(t, tempDir, "commit", "-am", "feature")
	feature := strings.TrimSpace(string(runGitOutput(t, tempDir, "rev-parse", "HEAD")))

	mgr := NewManager()
	if ok, err := mgr.IsAncestor(tempDir, base, feature); err != nil || !ok {
		t.Fatalf("base should be ancestor of feature: ok=%v err=%v", ok, err)
	}
	if ok, err := mgr.IsAncestor(tempDir, feature, base); err != nil || ok {
		t.Fatalf("feature should not be ancestor of base: ok=%v err=%v", ok, err)
	}
}

func TestMergeBranchLocally(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-merge-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	readme := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readme, []byte("# Base\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "initial commit")

	mgr := NewManager()

	// 1. Create feature branch and commit
	wtDir, branch, err := mgr.EnsureWorktree(tempDir, "task-merge-1", "main", "feature/merge-1")
	if err != nil {
		t.Fatalf("EnsureWorktree failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "new_feature.txt"), []byte("cool feature\n"), 0644); err != nil {
		t.Fatalf("write new feature: %v", err)
	}
	runGit(t, wtDir, "add", "new_feature.txt")
	runGit(t, wtDir, "commit", "-m", "feat: cool feature")

	// 2. Merge feature branch locally into main
	commitSHA, err := mgr.MergeBranchLocally(tempDir, "main", branch, "merge: feat cool feature")
	if err != nil {
		t.Fatalf("MergeBranchLocally failed: %v", err)
	}
	if len(commitSHA) < 7 {
		t.Fatalf("expected valid commit SHA, got %q", commitSHA)
	}

	// 3. Verify that main now contains the new file
	if _, err := os.Stat(filepath.Join(tempDir, "new_feature.txt")); err != nil {
		t.Fatalf("new_feature.txt missing from main after merge")
	}
}

func TestMergeBranchLocallyDirty(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-dirty-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	readme := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readme, []byte("# Base\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "initial commit")

	mgr := NewManager()
	_, branch, err := mgr.EnsureWorktree(tempDir, "task-dirty-1", "main", "feature/dirty-1")
	if err != nil {
		t.Fatalf("EnsureWorktree failed: %v", err)
	}

	// Make root repo dirty
	if err := os.WriteFile(readme, []byte("# Modified uncommitted\n"), 0644); err != nil {
		t.Fatalf("write dirty readme: %v", err)
	}

	// Merge should fail because of uncommitted changes
	_, err = mgr.MergeBranchLocally(tempDir, "main", branch, "merge")
	if err == nil {
		t.Fatalf("expected MergeBranchLocally to fail on dirty working directory")
	}
}

func TestMergeBranchLocallyAbortsConflictInRepository(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gitworktree-merge-conflict-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	runGit(t, tempDir, "init", "-b", "main")
	runGit(t, tempDir, "config", "user.email", "test@multigent.ai")
	runGit(t, tempDir, "config", "user.name", "Multigent Tester")
	readme := filepath.Join(tempDir, "README.md")
	if err := os.WriteFile(readme, []byte("base\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "initial commit")

	mgr := NewManager()
	wtDir, branch, err := mgr.EnsureWorktree(tempDir, "task-conflict", "main", "feature/conflict")
	if err != nil {
		t.Fatalf("EnsureWorktree failed: %v", err)
	}
	defer mgr.CleanupWorktree(tempDir, "task-conflict")

	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write feature readme: %v", err)
	}
	runGit(t, wtDir, "add", "README.md")
	runGit(t, wtDir, "commit", "-m", "feature change")

	if err := os.WriteFile(readme, []byte("main\n"), 0644); err != nil {
		t.Fatalf("write main readme: %v", err)
	}
	runGit(t, tempDir, "add", "README.md")
	runGit(t, tempDir, "commit", "-m", "main change")

	if _, err := mgr.MergeBranchLocally(tempDir, "main", branch, "merge conflict"); err == nil {
		t.Fatal("expected merge conflict")
	}
	if status := string(runGitOutput(t, tempDir, "status", "--porcelain", "--untracked-files=no")); status != "" {
		t.Fatalf("repository remained dirty after aborted merge: %q", status)
	}
	if _, err := exec.Command("git", "-C", tempDir, "rev-parse", "--verify", "MERGE_HEAD").Output(); err == nil {
		t.Fatal("MERGE_HEAD remained after aborted merge")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed in %s: %v\nOutput: %s", args, dir, err, string(out))
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed in %s: %v", args, dir, err)
	}
	return out
}
