package gitworktree

import (
	"os"
	"os/exec"
	"path/filepath"
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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed in %s: %v\nOutput: %s", args, dir, err, string(out))
	}
}
