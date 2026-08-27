package gitworktree

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Manager manages git worktrees for isolated task execution.
type Manager struct {
	mu sync.Mutex
}

// NewManager creates a new git worktree manager.
func NewManager() *Manager {
	return &Manager{}
}

// WorktreeDir returns the absolute path for a task's worktree.
func WorktreeDir(projectRoot, taskID string) string {
	return filepath.Join(projectRoot, ".multigent", "worktrees", sanitizeTaskID(taskID))
}

func sanitizeTaskID(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-")
	return r.Replace(taskID)
}

// EnsureWorktree prepares a dedicated git worktree for a task.
// If the worktree already exists, it returns its path and checked-out branch.
// Otherwise, it fetches the base branch, creates the feature branch, and adds the worktree.
func (m *Manager) EnsureWorktree(projectRoot, taskID, baseBranch, featureBranch string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return "", "", fmt.Errorf("project root is required")
	}

	// Verify that projectRoot is a git repository
	gitDir := filepath.Join(projectRoot, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return "", "", fmt.Errorf("project root is not a git repository: %w", err)
	}

	targetDir := WorktreeDir(projectRoot, taskID)
	if _, err := os.Stat(targetDir); err == nil {
		// Worktree directory already exists
		branch, err := checkedOutBranch(targetDir)
		if err != nil {
			return "", "", fmt.Errorf("read existing worktree branch: %w", err)
		}
		return targetDir, branch, nil
	}

	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	featureBranch = strings.TrimSpace(featureBranch)
	if featureBranch == "" {
		featureBranch = fmt.Sprintf("feature/%s", sanitizeTaskID(taskID))
	}

	// Create parent directory for worktrees
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return "", "", fmt.Errorf("create worktrees parent dir: %w", err)
	}

	// Check if the feature branch already exists locally
	branchExists := false
	cmdCheck := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+featureBranch)
	cmdCheck.Dir = projectRoot
	if err := cmdCheck.Run(); err == nil {
		branchExists = true
	}

	var cmdWorktree *exec.Cmd
	if branchExists {
		// Checkout existing branch into worktree
		cmdWorktree = exec.Command("git", "worktree", "add", targetDir, featureBranch)
	} else {
		// Create new branch based on baseBranch (or origin/baseBranch)
		startPoint := baseBranch
		// Try resolving base branch reference
		cmdRev := exec.Command("git", "rev-parse", "--verify", baseBranch)
		cmdRev.Dir = projectRoot
		if err := cmdRev.Run(); err != nil {
			// Try origin/<baseBranch>
			startPoint = "origin/" + baseBranch
		}
		cmdWorktree = exec.Command("git", "worktree", "add", "-b", featureBranch, targetDir, startPoint)
	}

	cmdWorktree.Dir = projectRoot
	var stderr bytes.Buffer
	cmdWorktree.Stderr = &stderr
	if err := cmdWorktree.Run(); err != nil {
		return "", "", fmt.Errorf("git worktree add failed: %w (stderr: %s)", err, stderr.String())
	}

	branch, err := checkedOutBranch(targetDir)
	if err != nil {
		return "", "", fmt.Errorf("read created worktree branch: %w", err)
	}
	return targetDir, branch, nil
}

func checkedOutBranch(worktreeDir string) (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = worktreeDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("worktree has no checked-out branch")
	}
	return branch, nil
}

// CleanupWorktree removes the git worktree and prunes worktree metadata.
func (m *Manager) CleanupWorktree(projectRoot, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	targetDir := WorktreeDir(projectRoot, taskID)
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		return nil
	}

	// Run git worktree remove --force
	cmd := exec.Command("git", "worktree", "remove", "--force", targetDir)
	cmd.Dir = projectRoot
	_ = cmd.Run()

	// Ensure directory is completely removed
	_ = os.RemoveAll(targetDir)

	// Run git worktree prune to clean up stale metadata
	cmdPrune := exec.Command("git", "worktree", "prune")
	cmdPrune.Dir = projectRoot
	_ = cmdPrune.Run()

	return nil
}

// ListWorktrees lists all active worktree paths in the repository.
func (m *Manager) ListWorktrees(projectRoot string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	realProjectRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		realProjectRoot = filepath.Clean(projectRoot)
	}

	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			realP, err := filepath.EvalSymlinks(p)
			if err != nil {
				realP = filepath.Clean(p)
			}
			if realP != "" && realP != realProjectRoot {
				paths = append(paths, p)
			}
		}
	}
	return paths, nil
}

// ListBranches returns all local and remote branches in the repository,
// deduplicated and sorted with main/master first.
func (m *Manager) ListBranches(projectRoot string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cmd := exec.Command("git", "branch", "-a", "--format=%(refname:short)")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		return []string{"main"}, nil
	}

	seen := make(map[string]bool)
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		b := strings.TrimSpace(line)
		if b == "" || strings.Contains(b, "HEAD") {
			continue
		}
		// Strip origin/ prefix for deduplication
		clean := strings.TrimPrefix(b, "origin/")
		if !seen[clean] {
			seen[clean] = true
			branches = append(branches, clean)
		}
	}

	if len(branches) == 0 {
		return []string{"main"}, nil
	}

	// Sort with main/master first
	for i := 0; i < len(branches); i++ {
		if branches[i] == "main" && i > 0 {
			branches[0], branches[i] = branches[i], branches[0]
			break
		}
	}

	return branches, nil
}

// GetCommitHash returns the short commit hash (7 chars) for a ref, or empty if unavailable.
func (m *Manager) GetCommitHash(projectRoot, ref string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "HEAD"
	}
	cmd := exec.Command("git", "rev-parse", "--short=7", ref)
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
