package previewreceipt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/multigent/multigent/internal/gitworktree"
)

var turnIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// BaselineResult contains the materialized baseline Git artifacts.
type BaselineResult struct {
	Tree    string
	Commit  string
	Ref     string
	Cleanup func()
}

// MaterializeBaseline constructs an immutable baseline tree and commit from the current
// working tree without mutating the main .git/index, HEAD, or working tree branch.
// It uses an alternate GIT_INDEX_FILE located outside projectRoot.
func MaterializeBaseline(ctx context.Context, projectRoot, turnID string) (*BaselineResult, error) {
	if !turnIDRegex.MatchString(turnID) {
		return nil, fmt.Errorf("invalid turnID %q: must match %s", turnID, turnIDRegex.String())
	}
	cleanRoot := filepath.Clean(projectRoot)
	if cleanRoot == "" {
		return nil, fmt.Errorf("projectRoot is required")
	}

	// 1. Verify working tree has no unmerged files (conflicts)
	unmergedOut, err := runGitIn(ctx, cleanRoot, gitworktree.SanitizedGitEnv(), "ls-files", "-u")
	if err != nil {
		return nil, fmt.Errorf("check unmerged files: %w", err)
	}
	if strings.TrimSpace(unmergedOut) != "" {
		return nil, fmt.Errorf("working tree has unmerged files / merge conflicts; cannot materialize baseline")
	}

	// 2. Create isolated alternate index directory outside projectRoot
	tempIndexDir, err := os.MkdirTemp("", "mg-preview-index-*")
	if err != nil {
		return nil, fmt.Errorf("create temp index dir: %w", err)
	}
	indexPath := filepath.Join(tempIndexDir, "index")

	refName := "refs/mg-turns/" + turnID
	cleanup := func() {
		_ = os.RemoveAll(tempIndexDir)
	}

	// Helper to run git with the alternate index and sanitized environment
	gitEnv := append(
		gitworktree.SanitizedGitEnv(
			"GIT_AUTHOR_NAME=Multigent",
			"GIT_AUTHOR_EMAIL=multigent@internal",
			"GIT_COMMITTER_NAME=Multigent",
			"GIT_COMMITTER_EMAIL=multigent@internal",
		),
		"GIT_INDEX_FILE="+indexPath,
	)

	runIndexGit := func(args ...string) (string, error) {
		return runGitIn(ctx, cleanRoot, gitEnv, args...)
	}

	// 3. Read HEAD tree into alternate index
	if _, err := runIndexGit("read-tree", "HEAD"); err != nil {
		cleanup()
		return nil, fmt.Errorf("git read-tree HEAD: %w", err)
	}

	// 4. Stage working tree changes into alternate index (respects .gitignore)
	if _, err := runIndexGit("add", "--all"); err != nil {
		cleanup()
		return nil, fmt.Errorf("git add --all in alternate index: %w", err)
	}

	// 5. Write tree to repository object store
	treeOut, err := runIndexGit("write-tree")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("git write-tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)
	if len(tree) != 40 {
		cleanup()
		return nil, fmt.Errorf("invalid tree hash %q from write-tree", tree)
	}

	// 6. Read current HEAD commit SHA
	headOut, err := runIndexGit("rev-parse", "HEAD")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	headCommit := strings.TrimSpace(headOut)

	// 7. Commit tree with parent HEAD using fixed non-interactive message
	commitMsg := fmt.Sprintf("multigent: preview turn baseline %s", turnID)
	commitOut, err := runIndexGit("commit-tree", tree, "-p", headCommit, "-m", commitMsg)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("git commit-tree: %w", err)
	}
	commit := strings.TrimSpace(commitOut)
	if len(commit) != 40 {
		cleanup()
		return nil, fmt.Errorf("invalid commit hash %q from commit-tree", commit)
	}

	// 8. Update ref to protect the commit from GC
	if _, err := runIndexGit("update-ref", refName, commit); err != nil {
		cleanup()
		return nil, fmt.Errorf("git update-ref %s: %w", refName, err)
	}

	// Full cleanup removes temp directory AND ref if caller desires or on teardown
	fullCleanup := func() {
		cleanup()
		_, _ = runGitIn(context.Background(), cleanRoot, gitworktree.SanitizedGitEnv(), "update-ref", "-d", refName)
	}

	return &BaselineResult{
		Tree:    tree,
		Commit:  commit,
		Ref:     refName,
		Cleanup: fullCleanup,
	}, nil
}

// VerifyWorktreeMatchesTree checks if the current working tree state matches expectedTree.
// It builds a temporary alternate tree outside projectRoot and compares the resulting hash.
func VerifyWorktreeMatchesTree(ctx context.Context, projectRoot, expectedTree string) (bool, error) {
	cleanRoot := filepath.Clean(projectRoot)
	tempIndexDir, err := os.MkdirTemp("", "mg-preview-verify-index-*")
	if err != nil {
		return false, fmt.Errorf("create verify temp index dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempIndexDir) }()

	indexPath := filepath.Join(tempIndexDir, "index")
	gitEnv := append(gitworktree.SanitizedGitEnv(), "GIT_INDEX_FILE="+indexPath)

	if _, err := runGitIn(ctx, cleanRoot, gitEnv, "read-tree", "HEAD"); err != nil {
		return false, fmt.Errorf("verify read-tree: %w", err)
	}
	if _, err := runGitIn(ctx, cleanRoot, gitEnv, "add", "--all"); err != nil {
		return false, fmt.Errorf("verify add --all: %w", err)
	}
	treeOut, err := runGitIn(ctx, cleanRoot, gitEnv, "write-tree")
	if err != nil {
		return false, fmt.Errorf("verify write-tree: %w", err)
	}
	actualTree := strings.TrimSpace(treeOut)
	return actualTree == expectedTree, nil
}

// CapturePostimages inspects touchedPaths in projectRoot, taking cryptographic
// fingerprints (existence, file mode, and SHA-256). Symlinks are hashed based
// on their link target payload without following targets, preventing symlink traversal.
func CapturePostimages(projectRoot string, touchedPaths []string) ([]PostimageEntry, error) {
	cleanRoot := filepath.Clean(projectRoot)
	entries := make([]PostimageEntry, 0, len(touchedPaths))

	for _, p := range touchedPaths {
		cleanPath := filepath.Clean(p)
		if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(p) {
			return nil, fmt.Errorf("invalid relative path %q (path traversal forbidden)", p)
		}
		fullPath := filepath.Join(cleanRoot, cleanPath)

		fi, err := os.Lstat(fullPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				entries = append(entries, PostimageEntry{
					Path:   cleanPath,
					Exists: false,
				})
				continue
			}
			return nil, fmt.Errorf("lstat %s: %w", cleanPath, err)
		}

		var sha string
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(fullPath)
			if err != nil {
				return nil, fmt.Errorf("readlink %s: %w", cleanPath, err)
			}
			sum := sha256.Sum256([]byte(target))
			sha = hex.EncodeToString(sum[:])
		} else if fi.IsDir() {
			sha = "directory"
		} else {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("read file %s: %w", cleanPath, err)
			}
			sum := sha256.Sum256(data)
			sha = hex.EncodeToString(sum[:])
		}

		entries = append(entries, PostimageEntry{
			Path:   cleanPath,
			Exists: true,
			Mode:   fi.Mode(),
			SHA256: sha,
		})
	}

	return entries, nil
}

// VerifyWorktreeMatchesPostimages verifies that all touched files in projectRoot
// still match their recorded postimage entries byte-for-byte.
func VerifyWorktreeMatchesPostimages(projectRoot string, entries []PostimageEntry) (bool, error) {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}

	current, err := CapturePostimages(projectRoot, paths)
	if err != nil {
		return false, err
	}

	if len(current) != len(entries) {
		return false, nil
	}

	for i := range entries {
		expected := entries[i]
		actual := current[i]
		if expected.Path != actual.Path {
			return false, nil
		}
		if expected.Exists != actual.Exists {
			return false, nil
		}
		if expected.Exists {
			if expected.Mode != actual.Mode || expected.SHA256 != actual.SHA256 {
				return false, nil
			}
		}
	}
	return true, nil
}

func runGitIn(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errOut := gitworktree.RedactGitOutput(strings.TrimSpace(stderr.String()))
		if errOut == "" {
			errOut = gitworktree.RedactGitOutput(strings.TrimSpace(stdout.String()))
		}
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, errOut)
	}
	return stdout.String(), nil
}
