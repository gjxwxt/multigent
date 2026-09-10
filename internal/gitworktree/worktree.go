package gitworktree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
		preserveRuntimeContract(projectRoot, targetDir)
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
	preserveRuntimeContract(projectRoot, targetDir)
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
		cmdFetch.Env = gitNetworkEnv()
		var stderr bytes.Buffer
		cmdFetch.Stderr = &stderr
		if err := cmdFetch.Run(); err != nil {
			// Host-side fetches often lack sandbox credentials (the runtime
			// injects them only inside the container). Fall back to the last
			// locally known remote tip instead of failing task creation.
			localRef := "origin/" + baseBranch
			if localErr := cmdLocalRevParse(projectRoot, localRef); localErr == nil {
				return gitRevParse(projectRoot, localRef)
			}
			return "", fmt.Errorf("git fetch origin %s failed: %w (%s)", baseBranch, err, strings.TrimSpace(stderr.String()))
		}
		ref = "origin/" + baseBranch
	}

	commit, err := gitRevParse(projectRoot, ref)
	if err != nil {
		return "", err
	}
	return commit, nil
}

// cmdLocalRevParse checks (without error formatting) that a ref resolves.
func cmdLocalRevParse(projectRoot, ref string) error {
	cmd := exec.Command("git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = projectRoot
	return cmd.Run()
}

// gitRevParse verifies a ref and returns its full commit SHA.
func gitRevParse(projectRoot, ref string) (string, error) {
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

// gitNetworkEnv neutralizes sandbox leftovers for host-side git network
// operations: container runs write credential.helper entries pointing at
// in-container paths into the repo's local .git/config, which aborts any
// host-side fetch/push before it even reaches the network. The explicit
// empty override wins over repo-local config. Authentication relies on
// environment-injected credentials; private-remote fetch failures fall back
// to the last locally known remote tip (see resolveBaseCommit).
func gitNetworkEnv() []string {
	return PushNetworkEnv()
}

// PushNetworkEnv is the exported form of gitNetworkEnv for callers outside
// this package that run host-side git push (e.g. checkpoint pushes after
// worktree cleanup). Credentials still come from the environment only —
// never from repo config or remote URLs.
func PushNetworkEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
	)
}

// RepairWorkspaceOwnership returns files under root to the current service
// user when a sandbox run left them owned by another uid (containers run as
// root while the multigent server typically runs unprivileged). It is
// best-effort: entries that cannot be repaired are skipped, and the walk
// never fails the caller.
func RepairWorkspaceOwnership(root string) {
	root = strings.TrimSpace(root)
	if root == "" {
		return
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return
	}
	if !uidOwnedByOther(root) {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: skip unreadable entries
		}
		if uidOwnedByOther(path) {
			_ = os.Chown(path, os.Getuid(), os.Getgid())
		}
		return nil
	})
}

func uidOwnedByOther(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) != os.Getuid()
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
	sanitizeSharedGitConfig(projectRoot)

	targetDir := WorktreeDir(projectRoot, taskID)
	if _, err := os.Stat(targetDir); err == nil {
		// Worktree directory already exists
		branch, err := checkedOutBranch(targetDir)
		if err != nil {
			return "", "", fmt.Errorf("read existing worktree branch: %w", err)
		}
		preserveRuntimeContract(projectRoot, targetDir)
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
	preserveRuntimeContract(projectRoot, targetDir)
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
	var removeErr error
	cmd := exec.Command("git", "worktree", "remove", "--force", targetDir)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		removeErr = fmt.Errorf("git worktree remove: %w (%s)", err, redactGitOutput(strings.TrimSpace(string(out))))
		log.Printf("[worktree-cleanup] %v", removeErr)
	}

	// Ensure directory is completely removed; only fail when it survives both
	// git remove and the raw delete.
	if err := os.RemoveAll(targetDir); err != nil {
		if removeErr != nil {
			return fmt.Errorf("%v; rm %s: %w", removeErr, targetDir, err)
		}
		return fmt.Errorf("rm %s: %w", targetDir, err)
	}

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

	sanitizeSharedGitConfig(projectRoot)
	cmdRemote := exec.Command("git", "remote")
	cmdRemote.Dir = projectRoot
	out, err := cmdRemote.Output()
	if err != nil || !hasRemote(string(out), "origin") {
		// No remote: nothing to fetch, treat as success.
		return nil
	}
	cmd := exec.Command("git", "fetch", "origin", "--prune")
	cmd.Dir = projectRoot
	cmd.Env = gitNetworkEnv()
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
// CommitWorktreeState folds uncommitted working-tree changes into a
// checkpoint commit on the checked-out branch. It is the reusable core of the
// review auto-commit flow and the manual worktree cleanup: unlike
// CaptureSnapshot it tolerates dirt — a clean tree is a no-op. The status
// re-scan happens inside the project lock, so the dirty check and the commit
// are atomic against other lock-holding git operations. Pushing is left to
// the caller (credentials live at the push boundary).
func (m *Manager) CommitWorktreeState(worktreeDir, message string) (sha string, wasDirty bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRootForWorktree(worktreeDir))
	if err != nil {
		return "", false, err
	}
	defer unlock()

	return commitWorktreeStateLocked(worktreeDir, message)
}

// commitWorktreeStateLocked is the lock-free body of CommitWorktreeState;
// callers must already hold m.mu and the project lock.
func commitWorktreeStateLocked(worktreeDir, message string) (string, bool, error) {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return "", false, fmt.Errorf("worktree directory is required")
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = worktreeDir
	out, err := status.Output()
	if err != nil {
		return "", false, fmt.Errorf("check worktree status: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		sha, err := gitRevParse(worktreeDir, "HEAD")
		if err != nil {
			return "", false, fmt.Errorf("read worktree HEAD: %w", err)
		}
		return sha, false, nil
	}

	add := exec.Command("git", "add", "-A")
	add.Dir = worktreeDir
	if addOut, err := add.CombinedOutput(); err != nil {
		return "", true, fmt.Errorf("git add: %w (%s)", err, redactGitOutput(strings.TrimSpace(string(addOut))))
	}
	commit := exec.Command("git", "commit", "-m", message)
	commit.Dir = worktreeDir
	if commitOut, err := commit.CombinedOutput(); err != nil {
		return "", true, fmt.Errorf("git commit: %w (%s)", err, redactGitOutput(strings.TrimSpace(string(commitOut))))
	}
	sha, err := gitRevParse(worktreeDir, "HEAD")
	if err != nil {
		return "", true, fmt.Errorf("read committed HEAD: %w", err)
	}
	return sha, true, nil
}

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
	sanitizeSharedGitConfig(projectRoot)

	cmd := exec.Command("git", "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	cmd.Dir = projectRoot
	cmd.Env = gitNetworkEnv()
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

// PushTag creates an annotated tag on the given commit, pushes it to origin,
// and verifies the remote tag points at the expected commit. The ancestry
// gate is fail-closed: when expectedAncestorOf is a non-empty ref (normally
// origin/main), the tagged commit must be reachable from it — a tag on a
// commit that never landed on the integration branch is a dangling release
// (the v1.0.0-rc1 "断头 tag" failure in the ias-auth-center pilot) and is
// rejected before anything is pushed.
func (m *Manager) PushTag(projectRoot, tag, commit, message, expectedAncestorOf string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return err
	}
	defer unlock()

	projectRoot = strings.TrimSpace(projectRoot)
	tag = strings.TrimSpace(tag)
	commit = strings.TrimSpace(commit)
	message = strings.TrimSpace(message)
	expectedAncestorOf = strings.TrimSpace(expectedAncestorOf)
	if projectRoot == "" || tag == "" || commit == "" {
		return fmt.Errorf("project root, tag, and commit are required")
	}
	sanitizeSharedGitConfig(projectRoot)

	resolve := exec.Command("git", "rev-parse", "--verify", commit+"^{commit}")
	resolve.Dir = projectRoot
	out, err := resolve.Output()
	if err != nil {
		return fmt.Errorf("resolve tag target %s failed: %w", commit, err)
	}
	targetSHA := strings.TrimSpace(string(out))

	if expectedAncestorOf != "" {
		ancestry, err := m.isAncestorLocked(projectRoot, commit, expectedAncestorOf)
		if err != nil {
			return err
		}
		if !ancestry {
			return fmt.Errorf("refusing to push tag %s: commit %s is not reachable from %s (dangling release)",
				tag, targetSHA[:min(len(targetSHA), 7)], expectedAncestorOf)
		}
	}

	tagCmd := exec.Command("git", "tag", "-a", tag, "-m", message, targetSHA)
	tagCmd.Dir = projectRoot
	var tagOut bytes.Buffer
	tagCmd.Stdout = &tagOut
	tagCmd.Stderr = &tagOut
	if err := tagCmd.Run(); err != nil {
		if strings.Contains(tagOut.String(), "already exists") {
			return fmt.Errorf("tag %s already exists", tag)
		}
		return fmt.Errorf("create tag %s failed: %w (%s)", tag, err, redactGitOutput(tagOut.String()))
	}

	push := exec.Command("git", "push", "origin", "refs/tags/"+tag+":refs/tags/"+tag)
	push.Dir = projectRoot
	push.Env = gitNetworkEnv()
	var pushOut bytes.Buffer
	push.Stdout = &pushOut
	push.Stderr = &pushOut
	if err := push.Run(); err != nil {
		// Do not leave a local-only tag behind: a failed push must not make
		// the next attempt "already exists" against a tag nobody can fetch.
		_ = exec.Command("git", "tag", "-d", tag).Run()
		return fmt.Errorf("push tag %s failed: %w (%s)", tag, err, redactGitOutput(pushOut.String()))
	}

	verify := exec.Command("git", "ls-remote", "--tags", "origin", "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	verify.Dir = projectRoot
	out, err = verify.Output()
	if err != nil {
		return fmt.Errorf("verify remote tag %s failed: %w", tag, err)
	}
	// Annotated tags carry their own tag object: ls-remote lists the object
	// sha on refs/tags/<tag> and the peeled commit sha on refs/tags/<tag>^{}.
	// Either matching means the remote points at the tagged commit.
	matches := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], targetSHA) {
			matches = true
			break
		}
	}
	if !matches {
		actual := strings.TrimSpace(string(out))
		if actual == "" {
			actual = "(missing)"
		}
		return fmt.Errorf("remote tag %s does not point to %q: %s", tag, targetSHA, actual)
	}
	return nil
}

// isAncestorLocked expects m.mu held; see IsAncestor for the public form.
func (m *Manager) isAncestorLocked(projectRoot, ancestor, descendant string) (bool, error) {
	ancestor = strings.TrimSpace(ancestor)
	descendant = strings.TrimSpace(descendant)
	if strings.TrimSpace(projectRoot) == "" || ancestor == "" || descendant == "" {
		return false, fmt.Errorf("project root, ancestor, and descendant are required")
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = projectRoot
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check ancestor relationship: %w", err)
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

// sanitizeSharedGitConfig removes stale credential.helper entries that point
// into per-run runtime-tools directories from the shared repository config.
// Such entries appear when an agent configures a helper with a container-
// internal path (e.g. /workspace/.multigent/runtime-tools/<run>/...) inside a
// sandbox; that path never exists on the host or in later containers, so
// every credential lookup afterwards — including the platform's own fetch and
// push — fails. The platform injects credentials per run via GIT_CONFIG_GLOBAL,
// so the shared config must stay free of run-scoped helpers.
func sanitizeSharedGitConfig(projectRoot string) {
	cfgPath := filepath.Join(strings.TrimSpace(projectRoot), ".git", "config")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(raw), "\n")
	kept := lines[:0]
	changed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "helper") && strings.Contains(trimmed, ".multigent/runtime-tools/") {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Join(kept, "\n")), 0644); err != nil {
		log.Printf("[gitworktree] sanitize shared git config failed for %s: %v", projectRoot, err)
		return
	}
	log.Printf("[gitworktree] removed stale runtime-tools credential.helper from %s", cfgPath)
}

func redactGitOutput(value string) string {
	if strings.TrimSpace(value) == "" {
		return "no output"
	}
	// Git helper diagnostics can contain a remote URL. Do not surface raw
	// command output until a structured redaction layer exists.
	return "git command returned diagnostics (details redacted)"
}

// RedactGitOutput is the exported form of redactGitOutput for callers outside
// this package that log git command output (credentials must never reach
// logs, task comments, or API responses).
func RedactGitOutput(value string) string {
	return redactGitOutput(value)
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

func preserveRuntimeContract(projectRoot, targetDir string) {
	projectRoot = strings.TrimSpace(projectRoot)
	targetDir = strings.TrimSpace(targetDir)
	if projectRoot == "" || targetDir == "" {
		return
	}
	rootContract := filepath.Join(projectRoot, ".multigent", "runtime.json")
	targetContract := filepath.Join(targetDir, ".multigent", "runtime.json")

	rootRaw, err := os.ReadFile(rootContract)
	if err != nil {
		return
	}
	var rootSpec struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(rootRaw, &rootSpec); err != nil || rootSpec.Version != 1 {
		return
	}

	targetRaw, err := os.ReadFile(targetContract)
	if os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(targetContract), 0755)
		_ = os.WriteFile(targetContract, rootRaw, 0644)
		return
	}
	if err == nil {
		var targetSpec struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(targetRaw, &targetSpec) != nil || targetSpec.Version != 1 {
			_ = os.WriteFile(targetContract, rootRaw, 0644)
		}
	}
}
