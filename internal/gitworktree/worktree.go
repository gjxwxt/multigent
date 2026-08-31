package gitworktree

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager manages git worktrees for isolated task execution.
type Manager struct {
	mu sync.Mutex
}

const (
	projectLockWait  = 30 * time.Second
	projectLockStale = 10 * time.Minute
)

// NewManager creates a new git worktree manager.
func NewManager() *Manager {
	return &Manager{}
}

// acquireProjectLock serializes Git metadata operations across Manager
// instances and processes. The directory creation is atomic on the supported
// filesystems; stale locks are recoverable after a crashed process.
func acquireProjectLock(projectRoot string) (func(), error) {
	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return nil, fmt.Errorf("project root is required")
	}
	lockParent := filepath.Join(projectRoot, ".multigent")
	if err := os.MkdirAll(lockParent, 0755); err != nil {
		return nil, fmt.Errorf("create git lock directory: %w", err)
	}
	lockDir := filepath.Join(lockParent, "git-operation.lock")
	deadline := time.Now().Add(projectLockWait)
	for {
		err := os.Mkdir(lockDir, 0755)
		if err == nil {
			return func() { _ = os.Remove(lockDir) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create git operation lock: %w", err)
		}
		if info, statErr := os.Stat(lockDir); statErr == nil && time.Since(info.ModTime()) > projectLockStale {
			if removeErr := os.Remove(lockDir); removeErr == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for project Git lock")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func projectRootForWorktree(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if filepath.Base(filepath.Dir(path)) == "worktrees" && filepath.Base(filepath.Dir(filepath.Dir(path))) == ".multigent" {
		return filepath.Dir(filepath.Dir(filepath.Dir(path)))
	}
	return path
}

// WorktreeDir returns the absolute path for a task's worktree.
func WorktreeDir(projectRoot, taskID string) string {
	return filepath.Join(projectRoot, ".multigent", "worktrees", sanitizeTaskID(taskID))
}

func sanitizeTaskID(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-")
	cleaned := r.Replace(taskID)
	// Dot-prefixed segments (e.g. "..") would escape the worktrees
	// directory when joined; flatten them into a safe name.
	if strings.HasPrefix(cleaned, ".") {
		cleaned = "task" + strings.TrimLeft(cleaned, ".")
	}
	if cleaned == "" {
		cleaned = "task"
	}
	return cleaned
}

// EnsureWorktree prepares a dedicated git worktree for a task.
// If the worktree already exists, it returns its path and checked-out branch.
// Otherwise, it fetches the base branch, creates the feature branch, and adds the worktree.
func (m *Manager) EnsureWorktree(projectRoot, taskID, baseBranch, featureBranch string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", "", err
	}
	defer unlock()

	// Re-opening an existing task worktree must remain possible while the
	// remote is temporarily unavailable; only a new worktree needs a fresh
	// remote base revision.
	if _, err := os.Stat(WorktreeDir(strings.TrimSpace(projectRoot), taskID)); err == nil {
		return m.ensureWorktree(projectRoot, taskID, baseBranch, "", featureBranch)
	}
	baseCommit, err := m.resolveBaseCommit(projectRoot, baseBranch)
	if err != nil {
		return "", "", err
	}
	return m.ensureWorktree(projectRoot, taskID, baseBranch, baseCommit, featureBranch)
}

// ResolveBaseCommit fetches the requested base branch when origin is configured
// and returns the exact full SHA that a new task should use.
func (m *Manager) ResolveBaseCommit(projectRoot, baseBranch string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", err
	}
	defer unlock()
	return m.resolveBaseCommit(projectRoot, baseBranch)
}

// EnsureWorktreeAt prepares a task worktree from an already resolved commit.
// Callers should persist the same commit as the task's baseCommit.
func (m *Manager) EnsureWorktreeAt(projectRoot, taskID, baseCommit, featureBranch string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	return m.ensureWorktree(projectRoot, taskID, "", baseCommit, featureBranch)
}

// EnsureSnapshotWorktree materializes a detached, read-only preview checkout
// at an exact completion commit. It never reuses a moving task branch, so a
// later push cannot change what a completed-task preview displays.
func (m *Manager) EnsureSnapshotWorktree(projectRoot, taskID, commit string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", err
	}
	defer unlock()

	projectRoot = strings.TrimSpace(projectRoot)
	commit = strings.TrimSpace(commit)
	if projectRoot == "" || commit == "" {
		return "", fmt.Errorf("project root and snapshot commit are required")
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err != nil {
		return "", fmt.Errorf("project root is not a git repository: %w", err)
	}
	targetDir := WorktreeDir(projectRoot, taskID)
	if _, err := os.Stat(targetDir); err == nil {
		current, revErr := gitRevision(targetDir)
		if revErr != nil || current != commit {
			return "", fmt.Errorf("snapshot worktree already exists at a different revision")
		}
		return targetDir, nil
	}
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return "", fmt.Errorf("create worktrees parent dir: %w", err)
	}
	cmd := exec.Command("git", "worktree", "add", "--detach", targetDir, commit)
	cmd.Dir = projectRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git snapshot worktree add failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return targetDir, nil
}

func gitRevision(worktreeDir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD^{commit}")
	cmd.Dir = worktreeDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) resolveBaseCommit(projectRoot, baseBranch string) (string, error) {
	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return "", fmt.Errorf("project root is required")
	}
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err != nil {
		return "", fmt.Errorf("project root is not a git repository: %w", err)
	}

	ref := baseBranch
	cmdRemote := exec.Command("git", "remote")
	cmdRemote.Dir = projectRoot
	if out, err := cmdRemote.Output(); err == nil && hasRemote(string(out), "origin") {
		cmdFetch := exec.Command("git", "fetch", "origin", baseBranch)
		cmdFetch.Dir = projectRoot
		var stderr bytes.Buffer
		cmdFetch.Stderr = &stderr
		if err := cmdFetch.Run(); err != nil {
			return "", fmt.Errorf("git fetch origin %s failed: %w (%s)", baseBranch, err, strings.TrimSpace(stderr.String()))
		}
		ref = "origin/" + baseBranch
	}

	cmd := exec.Command("git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve base commit %s failed: %w", ref, err)
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return "", fmt.Errorf("resolve base commit %s returned an empty SHA", ref)
	}
	return commit, nil
}

func (m *Manager) ensureWorktree(projectRoot, taskID, baseBranch, baseCommit, featureBranch string) (string, string, error) {

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
	featureBranch = strings.TrimSpace(featureBranch)
	if featureBranch == "" {
		featureBranch = fmt.Sprintf("feature/%s", sanitizeTaskID(taskID))
	}

	// Create parent directory for worktrees
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return "", "", fmt.Errorf("create worktrees parent dir: %w", err)
	}

	startPoint := strings.TrimSpace(baseCommit)
	if startPoint == "" {
		if baseBranch == "" {
			baseBranch = "main"
		}
		var err error
		startPoint, err = m.resolveBaseCommit(projectRoot, baseBranch)
		if err != nil {
			return "", "", err
		}
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

func hasRemote(remoteList, wanted string) bool {
	wanted = strings.TrimSpace(wanted)
	for _, remote := range strings.Split(remoteList, "\n") {
		if strings.TrimSpace(remote) == wanted {
			return true
		}
	}
	return false
}

// CheckedOutBranch returns the currently checked-out branch name for a git directory or worktree.
func (m *Manager) CheckedOutBranch(worktreeDir string) (string, error) {
	return checkedOutBranch(worktreeDir)
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
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return err
	}
	defer unlock()

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

// BranchInfo describes one branch for UI pickers: display name plus the
// commit it currently points at and when that commit was authored. The
// staleness of lastCommitDate is the user-visible signal that the local
// refs may be behind the remote and a refresh is worthwhile.
type BranchInfo struct {
	Name           string `json:"name"`
	SHA            string `json:"sha,omitempty"`
	LastCommitDate string `json:"lastCommitDate,omitempty"`
}

// ListBranches returns all local and remote branches in the repository,
// deduplicated and sorted with main/master first.
func (m *Manager) ListBranches(projectRoot string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return listBranches(projectRoot)
}

// FetchRemoteUpdates runs git fetch --prune against origin when one is
// configured, so subsequent branch listings reflect the remote state.
// It is intended for explicit user-triggered refreshes, not automatic
// paths; task creation always fetches the target branch on its own.
func (m *Manager) FetchRemoteUpdates(projectRoot string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return err
	}
	defer unlock()

	cmdRemote := exec.Command("git", "remote")
	cmdRemote.Dir = projectRoot
	out, err := cmdRemote.Output()
	if err != nil || !hasRemote(string(out), "origin") {
		// No remote: nothing to fetch, treat as success.
		return nil
	}
	cmd := exec.Command("git", "fetch", "origin", "--prune")
	cmd.Dir = projectRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch origin --prune failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ListBranchesDetailed returns structured branch metadata (name, short
// SHA, relative last-commit date) for UI display.
func (m *Manager) ListBranchesDetailed(projectRoot string) ([]BranchInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	names, err := listBranches(projectRoot)
	if err != nil {
		return nil, err
	}
	infos := make([]BranchInfo, 0, len(names))
	for _, name := range names {
		info := BranchInfo{Name: name}
		cmdResolve := exec.Command("git", "rev-parse", "--verify", "--short=7", "refs/heads/"+name)
		cmdResolve.Dir = projectRoot
		if out, err := cmdResolve.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			info.SHA = strings.TrimSpace(string(out))
		} else {
			cmd := exec.Command("git", "rev-parse", "--verify", "--short=7", "refs/remotes/origin/"+name)
			cmd.Dir = projectRoot
			if o, err := cmd.Output(); err == nil {
				info.SHA = strings.TrimSpace(string(o))
			}
		}
		if info.SHA != "" {
			cmdDate := exec.Command("git", "log", "-1", "--format=%cr", info.SHA)
			cmdDate.Dir = projectRoot
			if out, err := cmdDate.Output(); err == nil {
				info.LastCommitDate = strings.TrimSpace(string(out))
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func listBranches(projectRoot string) ([]string, error) {
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

// CaptureSnapshot returns the full HEAD SHA when a worktree is clean. A clean
// worktree is required so the SHA is a truthful immutable task snapshot.
func (m *Manager) CaptureSnapshot(worktreeDir string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRootForWorktree(worktreeDir))
	if err != nil {
		return "", err
	}
	defer unlock()

	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return "", fmt.Errorf("worktree directory is required")
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = worktreeDir
	out, err := status.Output()
	if err != nil {
		return "", fmt.Errorf("check worktree status: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return "", fmt.Errorf("worktree has uncommitted changes")
	}

	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD^{commit}")
	cmd.Dir = worktreeDir
	out, err = cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read worktree HEAD: %w", err)
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return "", fmt.Errorf("worktree HEAD is empty")
	}
	return commit, nil
}

// PushBranch pushes a task branch to origin and verifies that the remote ref
// points to expectedCommit. The command never embeds credentials in its args.
func (m *Manager) PushBranch(projectRoot, branch, expectedCommit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return err
	}
	defer unlock()

	projectRoot = strings.TrimSpace(projectRoot)
	branch = strings.TrimSpace(branch)
	expectedCommit = strings.TrimSpace(expectedCommit)
	if projectRoot == "" || branch == "" || expectedCommit == "" {
		return fmt.Errorf("project root, branch, and expected commit are required")
	}

	cmd := exec.Command("git", "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	cmd.Dir = projectRoot
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push origin %s failed: %w (%s)", branch, err, redactGitOutput(output.String()))
	}

	verify := exec.Command("git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	verify.Dir = projectRoot
	out, err := verify.Output()
	if err != nil {
		return fmt.Errorf("verify remote branch %s failed: %w", branch, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 1 || !strings.EqualFold(fields[0], expectedCommit) {
		actual := ""
		if len(fields) > 0 {
			actual = fields[0]
		}
		return fmt.Errorf("remote branch %s points to %q, expected %q", branch, actual, expectedCommit)
	}
	return nil
}

// IsAncestor reports whether ancestor is reachable from descendant. It is
// used to detect squash/rebase integration where a task completion commit is
// no longer in the default branch history.
func (m *Manager) IsAncestor(projectRoot, ancestor, descendant string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return false, err
	}
	defer unlock()

	ancestor = strings.TrimSpace(ancestor)
	descendant = strings.TrimSpace(descendant)
	if strings.TrimSpace(projectRoot) == "" || ancestor == "" || descendant == "" {
		return false, fmt.Errorf("project root, ancestor, and descendant are required")
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = projectRoot
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check ancestor relationship: %w", err)
}

func redactGitOutput(value string) string {
	if strings.TrimSpace(value) == "" {
		return "no output"
	}
	// Git helper diagnostics can contain a remote URL. Do not surface raw
	// command output until a structured redaction layer exists.
	return "git command returned diagnostics (details redacted)"
}

// MergeBranchLocally merges a feature branch into a target branch (e.g. main) locally with safety checks.
// It returns the resulting commit hash or an error if there are uncommitted changes or conflicts.
func (m *Manager) MergeBranchLocally(projectRoot, targetBranch, sourceBranch, commitMessage string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", err
	}
	defer unlock()

	projectRoot = strings.TrimSpace(projectRoot)
	targetBranch = strings.TrimSpace(targetBranch)
	if targetBranch == "" {
		targetBranch = "main"
	}
	sourceBranch = strings.TrimSpace(sourceBranch)
	if sourceBranch == "" {
		return "", fmt.Errorf("source branch is required")
	}

	// 1. Check if root repository is clean (ignoring .multigent runtime artifacts)
	cmdStatus := exec.Command("git", "status", "--porcelain")
	cmdStatus.Dir = projectRoot
	statusOut, err := cmdStatus.Output()
	if err != nil {
		return "", fmt.Errorf("check repository status: %w", err)
	}
	var dirtyLines []string
	for _, line := range strings.Split(string(statusOut), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "?? .multigent") || strings.HasPrefix(line, "?? .git") {
			continue
		}
		dirtyLines = append(dirtyLines, line)
	}
	if len(dirtyLines) > 0 {
		return "", fmt.Errorf("cannot merge: root repository has uncommitted changes: %s", strings.Join(dirtyLines, ", "))
	}

	// 2. Checkout target branch
	cmdCheckout := exec.Command("git", "checkout", targetBranch)
	cmdCheckout.Dir = projectRoot
	var stderrCheckout bytes.Buffer
	cmdCheckout.Stderr = &stderrCheckout
	if err := cmdCheckout.Run(); err != nil {
		return "", fmt.Errorf("checkout target branch %s failed: %w (%s)", targetBranch, err, stderrCheckout.String())
	}

	// 3. Perform merge with --no-ff
	if commitMessage == "" {
		commitMessage = fmt.Sprintf("merge: branch '%s' into '%s'", sourceBranch, targetBranch)
	}
	cmdMerge := exec.Command("git", "merge", "--no-ff", "-m", commitMessage, sourceBranch)
	cmdMerge.Dir = projectRoot
	var stderrMerge bytes.Buffer
	cmdMerge.Stderr = &stderrMerge
	if err := cmdMerge.Run(); err != nil {
		// Attempt clean abort on failure / conflict
		cmdAbort := exec.Command("git", "merge", "--abort")
		cmdAbort.Dir = projectRoot
		_ = cmdAbort.Run()
		return "", fmt.Errorf("merge branch %s into %s failed: %w (%s)", sourceBranch, targetBranch, err, stderrMerge.String())
	}

	// 4. Retrieve resulting HEAD commit hash
	cmdRev := exec.Command("git", "rev-parse", "HEAD")
	cmdRev.Dir = projectRoot
	out, err := cmdRev.Output()
	if err != nil {
		return "", fmt.Errorf("read merged commit hash: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// SyncMain pulls latest changes from origin for the given branch if a remote is configured.
func (m *Manager) SyncMain(projectRoot, defaultBranch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return err
	}
	defer unlock()

	defaultBranch = strings.TrimSpace(defaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return fmt.Errorf("project root is required")
	}

	// Check if origin remote exists
	cmdRemote := exec.Command("git", "remote")
	cmdRemote.Dir = projectRoot
	if out, err := cmdRemote.Output(); err == nil && hasRemote(string(out), "origin") {
		cmdBranch := exec.Command("git", "branch", "--show-current")
		cmdBranch.Dir = projectRoot
		branchOut, err := cmdBranch.Output()
		if err != nil {
			return fmt.Errorf("read current branch before sync: %w", err)
		}
		if current := strings.TrimSpace(string(branchOut)); current != defaultBranch {
			return fmt.Errorf("cannot sync %s: repository is on branch %s", defaultBranch, current)
		}

		cmdStatus := exec.Command("git", "status", "--porcelain")
		cmdStatus.Dir = projectRoot
		statusOut, err := cmdStatus.Output()
		if err != nil {
			return fmt.Errorf("check repository status before sync: %w", err)
		}
		if strings.TrimSpace(string(statusOut)) != "" {
			return fmt.Errorf("cannot sync %s: repository has uncommitted changes", defaultBranch)
		}

		cmdFetch := exec.Command("git", "fetch", "origin", defaultBranch)
		cmdFetch.Dir = projectRoot
		if err := cmdFetch.Run(); err != nil {
			return fmt.Errorf("fetch origin %s failed: %w", defaultBranch, err)
		}
		cmdMerge := exec.Command("git", "merge", "--ff-only", "origin/"+defaultBranch)
		cmdMerge.Dir = projectRoot
		if err := cmdMerge.Run(); err != nil {
			return fmt.Errorf("fast-forward %s from origin failed: %w", defaultBranch, err)
		}
	}
	return nil
}
