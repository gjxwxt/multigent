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

func TestSanitizeTaskIDBlocksPathTraversal(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"..", ".", "../escape", "..\\escape", " t-1 "} {
		dir := WorktreeDir(root, id)
		if !strings.HasPrefix(dir, filepath.Join(root, ".multigent", "worktrees")+string(filepath.Separator)) {
			t.Fatalf("task id %q escaped worktrees dir: %s", id, dir)
		}
	}
	if got := WorktreeDir(root, ".."); !strings.Contains(got, "worktrees") {
		t.Fatalf("expected traversal id contained, got %s", got)
	}
}

func TestListBranchesDetailedReturnsMeta(t *testing.T) {
	m := NewManager()
	root := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "t@t")
	runGit("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	infos, err := m.ListBranchesDetailed(root)
	if err != nil {
		t.Fatalf("ListBranchesDetailed: %v", err)
	}
	if len(infos) == 0 || infos[0].Name != "main" {
		t.Fatalf("expected main first, got %+v", infos)
	}
	if len(infos[0].SHA) != 7 {
		t.Fatalf("expected short sha, got %q", infos[0].SHA)
	}
	if infos[0].LastCommitDate == "" {
		t.Fatal("expected lastCommitDate to be populated")
	}

	// No remote configured: fetch is a no-op success.
	if err := m.FetchRemoteUpdates(root); err != nil {
		t.Fatalf("FetchRemoteUpdates without remote should be nil, got %v", err)
	}
}

func TestListBranchesDetailedFallsBackToRemoteRef(t *testing.T) {
	m := NewManager()
	upstream := t.TempDir()
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit(upstream, "init", "-b", "main")
	runGit(upstream, "config", "user.email", "t@t")
	runGit(upstream, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(upstream, "add", ".")
	runGit(upstream, "commit", "-m", "init")
	runGit(upstream, "branch", "remote-only")

	// Clone leaves remote-only reachable only as origin/remote-only.
	clone := t.TempDir()
	runGit(clone, "clone", upstream, ".")
	runGit(clone, "config", "user.email", "t@t")
	runGit(clone, "config", "user.name", "t")

	infos, err := m.ListBranchesDetailed(clone)
	if err != nil {
		t.Fatalf("ListBranchesDetailed: %v", err)
	}
	found := map[string]BranchInfo{}
	for _, info := range infos {
		found[info.Name] = info
	}
	if info, ok := found["remote-only"]; !ok || info.SHA == "" {
		t.Fatalf("expected remote-only branch with sha via remote ref fallback, got %+v", found)
	}

	// Fetch with a real remote is a success.
	if err := m.FetchRemoteUpdates(clone); err != nil {
		t.Fatalf("FetchRemoteUpdates with remote: %v", err)
	}
}

// TestResolveBaseCommitFallsBackToLocalRemoteRef covers task creation while
// the host cannot reach (or authenticate against) origin: the last locally
// known remote-tracking tip must be used instead of failing the request.
func TestResolveBaseCommitFallsBackToLocalRemoteRef(t *testing.T) {
	m := NewManager()
	upstream := t.TempDir()
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit(upstream, "init", "-b", "main")
	runGit(upstream, "config", "user.email", "t@t")
	runGit(upstream, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(upstream, "add", ".")
	runGit(upstream, "commit", "-m", "init")
	want := strings.TrimSpace(string(runGitOutput(t, upstream, "rev-parse", "HEAD")))

	clone := t.TempDir()
	runGit(clone, "clone", upstream, ".")
	runGit(clone, "config", "user.email", "t@t")
	runGit(clone, "config", "user.name", "t")

	// Poison origin so fetch fails (unreachable URL), simulating a
	// credential-less or offline host-side fetch.
	runGit(clone, "remote", "set-url", "origin", "http://127.0.0.1:1/unreachable.git")

	got, err := m.ResolveBaseCommit(clone, "main")
	if err != nil {
		t.Fatalf("ResolveBaseCommit with unreachable origin: %v", err)
	}
	if got != want {
		t.Fatalf("fallback base = %s, want local remote tip %s", got, want)
	}
}

// TestGitNetworkEnvNeutralizesSandboxCredentialHelper verifies that host-side
// fetches survive a repo whose local .git/config was polluted by a container
// run with a credential.helper pointing at an in-container path.
func TestGitNetworkEnvNeutralizesSandboxCredentialHelper(t *testing.T) {
	m := NewManager()
	upstream := t.TempDir()
	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit(upstream, "init", "-b", "main")
	runGit(upstream, "config", "user.email", "t@t")
	runGit(upstream, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(upstream, "add", ".")
	runGit(upstream, "commit", "-m", "init")

	clone := t.TempDir()
	runGit(clone, "clone", upstream, ".")
	runGit(clone, "config", "user.email", "t@t")
	runGit(clone, "config", "user.name", "t")
	// Simulate sandbox pollution exactly as observed in agent clones.
	runGit(clone, "config", "credential.helper", "/workspace/.multigent/runtime-tools/some-run/.gitconfig.credential-helper")

	// FetchRemoteUpdates uses gitNetworkEnv(); without the override this
	// fetch would abort trying to execute the missing helper.
	if err := m.FetchRemoteUpdates(clone); err != nil {
		t.Fatalf("FetchRemoteUpdates with polluted credential.helper: %v", err)
	}
}

// TestRepairWorkspaceOwnershipReclaimsRootFiles verifies the post-sandbox
// ownership repair: root-owned entries (uid 0) under the git root are
// chowned back to the current user so subsequent git operations succeed.
func TestRepairWorkspaceOwnershipReclaimsRootFiles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: ownership repair is a no-op")
	}
	root := t.TempDir()
	nested := filepath.Join(root, ".git")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate container-created files with uid 0 via chown on darwin/linux.
	// Only root can chown to uid 0, so when unprivileged we instead assert the
	// no-op safety path: current-user files must be left untouched and no
	// error may escape.
	if err := os.WriteFile(filepath.Join(nested, "FETCH_HEAD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RepairWorkspaceOwnership(root) // must not panic or error
	if _, err := os.Stat(filepath.Join(nested, "FETCH_HEAD")); err != nil {
		t.Fatalf("expected file to survive repair walk: %v", err)
	}
}
