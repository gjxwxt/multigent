package previewreceipt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/gitworktree"
)

func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s in %s failed: %v\nOutput: %s", name, strings.Join(args, " "), dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.name", "Test User")
	runCmd(t, dir, "git", "config", "user.email", "test@example.com")

	// 1. Initial commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Add a tracked file that will later be ignored by .gitignore
	if err := os.WriteFile(filepath.Join(dir, "tracked_ignored.txt"), []byte("tracked initially\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Add foo.txt
	if err := os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("foo original\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runCmd(t, dir, "git", "add", "README.md", "tracked_ignored.txt", "foo.txt")
	runCmd(t, dir, "git", "commit", "-m", "initial commit")

	// 2. Add .gitignore
	gitignore := "*.ignored\n.env\ntracked_ignored.txt\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, dir, "git", "add", ".gitignore")
	runCmd(t, dir, "git", "commit", "-m", "add gitignore")

	return dir
}

func TestMaterializeBaselineAndVerifyTree(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx := context.Background()

	// 1. Prepare working tree state:
	// - Modify foo.txt (unstaged)
	if err := os.WriteFile(filepath.Join(repoDir, "foo.txt"), []byte("foo modified unstaged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// - Modify tracked_ignored.txt (unstaged)
	if err := os.WriteFile(filepath.Join(repoDir, "tracked_ignored.txt"), []byte("tracked ignored modified\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// - Add untracked bar.txt
	if err := os.WriteFile(filepath.Join(repoDir, "bar.txt"), []byte("bar untracked content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// - Add ignored files
	if err := os.WriteFile(filepath.Join(repoDir, ".env"), []byte("SECRET_KEY=12345\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "debug.ignored"), []byte("log data\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Capture git status before materialization
	statusBefore := runCmd(t, repoDir, "git", "status", "--porcelain")
	headBefore := runCmd(t, repoDir, "git", "rev-parse", "HEAD")

	// 2. Run MaterializeBaseline
	turnID := "turn-test-42"
	res, err := MaterializeBaseline(ctx, repoDir, turnID)
	if err != nil {
		t.Fatalf("MaterializeBaseline failed: %v", err)
	}
	defer res.Cleanup()

	// 3. Verify main repo status and HEAD are 100% UNTOUCHED
	statusAfter := runCmd(t, repoDir, "git", "status", "--porcelain")
	headAfter := runCmd(t, repoDir, "git", "rev-parse", "HEAD")

	if statusBefore != statusAfter {
		t.Fatalf("MaterializeBaseline mutated main repo status!\nbefore: %s\nafter:  %s", statusBefore, statusAfter)
	}
	if headBefore != headAfter {
		t.Fatalf("MaterializeBaseline mutated main repo HEAD!\nbefore: %s\nafter:  %s", headBefore, headAfter)
	}

	// 4. Verify ref was created and commit parent is HEAD
	refCommit := runCmd(t, repoDir, "git", "rev-parse", "refs/mg-turns/"+turnID)
	if refCommit != res.Commit {
		t.Fatalf("ref %s points to %s, expected %s", res.Ref, refCommit, res.Commit)
	}
	parentCommit := runCmd(t, repoDir, "git", "rev-parse", res.Commit+"^")
	if parentCommit != headBefore {
		t.Fatalf("commit parent %s != headBefore %s", parentCommit, headBefore)
	}

	// 5. Inspect tree contents in the materialized commit
	treeFiles := runCmd(t, repoDir, "git", "ls-tree", "-r", "--name-only", res.Tree)
	treeList := strings.Split(treeFiles, "\n")
	treeMap := make(map[string]bool)
	for _, f := range treeList {
		treeMap[strings.TrimSpace(f)] = true
	}

	// Expected to be included:
	if !treeMap["foo.txt"] {
		t.Errorf("expected foo.txt to be in baseline tree")
	}
	if !treeMap["bar.txt"] {
		t.Errorf("expected bar.txt to be in baseline tree")
	}
	if !treeMap["tracked_ignored.txt"] {
		t.Errorf("expected tracked_ignored.txt to remain in baseline tree (already tracked)")
	}

	// Expected to be EXCLUDED:
	if treeMap[".env"] {
		t.Errorf("forbidden: .env should have been ignored by .gitignore")
	}
	if treeMap["debug.ignored"] {
		t.Errorf("forbidden: debug.ignored should have been ignored by .gitignore")
	}

	// Verify foo.txt content in tree is the modified unstaged content
	fooCat := runCmd(t, repoDir, "git", "cat-file", "-p", res.Tree+":foo.txt")
	if fooCat != "foo modified unstaged" {
		t.Errorf("expected foo.txt in tree to have modified content, got %q", fooCat)
	}

	// 6. Verify Worktree Matches Tree
	matches, err := VerifyWorktreeMatchesTree(ctx, repoDir, res.Tree)
	if err != nil {
		t.Fatalf("VerifyWorktreeMatchesTree error: %v", err)
	}
	if !matches {
		t.Fatalf("expected worktree to match baseline tree %s", res.Tree)
	}

	// Drift detection: mutate foo.txt
	if err := os.WriteFile(filepath.Join(repoDir, "foo.txt"), []byte("foo modified AGAIN\n"), 0644); err != nil {
		t.Fatal(err)
	}
	matchesAfterDrift, err := VerifyWorktreeMatchesTree(ctx, repoDir, res.Tree)
	if err != nil {
		t.Fatalf("VerifyWorktreeMatchesTree after drift error: %v", err)
	}
	if matchesAfterDrift {
		t.Fatalf("expected VerifyWorktreeMatchesTree to detect drift and return false")
	}
}

func TestCaptureAndVerifyPostimages(t *testing.T) {
	repoDir := setupTestGitRepo(t)

	// Create test files
	f1 := filepath.Join(repoDir, "doc.txt")
	if err := os.WriteFile(f1, []byte("initial document"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create symlink
	sym := filepath.Join(repoDir, "symlink.txt")
	if err := os.Symlink("doc.txt", sym); err != nil {
		t.Fatal(err)
	}

	touched := []string{"doc.txt", "symlink.txt", "nonexistent.txt"}
	entries, err := CapturePostimages(repoDir, touched)
	if err != nil {
		t.Fatalf("CapturePostimages failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Check doc.txt
	if !entries[0].Exists || entries[0].Path != "doc.txt" || entries[0].SHA256 == "" {
		t.Errorf("unexpected doc.txt entry: %+v", entries[0])
	}
	// Check symlink.txt: should exist and have ModeSymlink
	if !entries[1].Exists || entries[1].Path != "symlink.txt" || entries[1].Mode&os.ModeSymlink == 0 {
		t.Errorf("unexpected symlink.txt entry: %+v", entries[1])
	}
	// Check nonexistent.txt
	if entries[2].Exists || entries[2].Path != "nonexistent.txt" {
		t.Errorf("unexpected nonexistent.txt entry: %+v", entries[2])
	}

	// Verify match: currently should be true
	match, err := VerifyWorktreeMatchesPostimages(repoDir, entries)
	if err != nil {
		t.Fatalf("VerifyWorktreeMatchesPostimages failed: %v", err)
	}
	if !match {
		t.Fatalf("expected postimages to match current worktree")
	}

	// Mutate doc.txt
	if err := os.WriteFile(f1, []byte("tampered content"), 0644); err != nil {
		t.Fatal(err)
	}
	matchAfterTamper, err := VerifyWorktreeMatchesPostimages(repoDir, entries)
	if err != nil {
		t.Fatalf("VerifyWorktreeMatchesPostimages after tamper failed: %v", err)
	}
	if matchAfterTamper {
		t.Fatalf("expected postimages match to fail after file tampering")
	}

	// Test path traversal rejection
	_, err = CapturePostimages(repoDir, []string{"../escape.txt"})
	if err == nil {
		t.Fatalf("expected path traversal to be rejected")
	}
}

func TestMaterializeBaselineRejectsInvalidTurnID(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx := context.Background()

	invalidIDs := []string{
		"",
		"turn/with/slash",
		"turn..traversal",
		"turn with space",
		"turn*star",
	}

	for _, badID := range invalidIDs {
		_, err := MaterializeBaseline(ctx, repoDir, badID)
		if err == nil {
			t.Errorf("expected invalid turnID %q to be rejected", badID)
		}
	}
}

func TestMaterializeBaselineRejectsUnmergedFiles(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx := context.Background()

	// Simulate unmerged index entry (stage 2 and stage 3)
	// write blob into object database
	blob1 := runCmd(t, repoDir, "git", "hash-object", "-w", "--stdin")
	// write unmerged entry using update-index --index-info
	updateCmd := exec.Command("git", "update-index", "--index-info")
	updateCmd.Dir = repoDir
	updateCmd.Stdin = strings.NewReader("100644 " + blob1 + " 2\tconflict.txt\n100644 " + blob1 + " 3\tconflict.txt\n")
	if out, err := updateCmd.CombinedOutput(); err != nil {
		t.Fatalf("update-index failed: %v (%s)", err, string(out))
	}

	_, err := MaterializeBaseline(ctx, repoDir, "turn-conflict")
	if err == nil {
		t.Fatal("expected MaterializeBaseline to fail on unmerged files")
	}
	if !strings.Contains(err.Error(), "unmerged files") {
		t.Fatalf("expected error mentioning unmerged files, got: %v", err)
	}
}

func TestMaterializeBaselineWithPurifiedClone(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx := context.Background()

	// Add an untracked file to worktree
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("feature work"), 0644); err != nil {
		t.Fatal(err)
	}

	turnID := "turn-clone-test"
	res, err := MaterializeBaseline(ctx, repoDir, turnID)
	if err != nil {
		t.Fatalf("MaterializeBaseline failed: %v", err)
	}
	defer res.Cleanup()

	// Create Purified Clone using the materialized commit
	cloneDir := filepath.Join(t.TempDir(), "purified-clone")
	cleanupClone, err := gitworktree.EnsurePurifiedClone(gitworktree.PurifiedCloneOptions{
		SourceRepo: repoDir,
		Commit:     res.Commit,
		Ref:        res.Ref,
		Dest:       cloneDir,
	})
	if err != nil {
		t.Fatalf("EnsurePurifiedClone failed: %v", err)
	}
	defer cleanupClone()

	// Verify clone contains feature.txt from baseline commit
	featureContent, err := os.ReadFile(filepath.Join(cloneDir, "feature.txt"))
	if err != nil {
		t.Fatalf("failed to read feature.txt in clone: %v", err)
	}
	if string(featureContent) != "feature work" {
		t.Fatalf("expected feature work in clone, got %q", string(featureContent))
	}

	// Verify clone has no remotes
	remotes := runCmd(t, cloneDir, "git", "remote")
	if strings.TrimSpace(remotes) != "" {
		t.Fatalf("expected no remotes in purified clone, got %q", remotes)
	}
}
