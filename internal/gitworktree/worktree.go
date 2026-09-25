package gitworktree

import (
	"bytes"
	"context"
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

// Git operation timeouts, tuned per operation class. All Manager methods run
// under the manager mutex AND the per-project directory lock, so one hung git
// call would freeze every other task's worktree operations on the project
// (and, for the manager mutex, across all projects). Local operations answer
// in milliseconds; network operations get a larger budget that still caps a
// wedged remote or credential-helper prompt.
//
// The network budget is overridable with MULTIGENT_GIT_NETWORK_TIMEOUT (Go
// duration, e.g. 300s): intranet GitLab deployments behind saturated uplinks
// or cold runners can legitimately exceed 90s, while the default stays tight
// so a wedged remote fails promptly. Read once at first use; resolution
// failure falls back to the default (fail-open to the safe value, never to
// zero/unbounded).
const (
	gitLocalTimeout = 15 * time.Second
	// gitNetworkTimeoutEnv lets a deployment widen the network git budget.
	gitNetworkTimeoutEnv = "MULTIGENT_GIT_NETWORK_TIMEOUT"
)

func gitNetworkTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv(gitNetworkTimeoutEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("[gitworktree] invalid %s=%q, falling back to 90s", gitNetworkTimeoutEnv, v)
	}
	return 90 * time.Second
}

// gitLocal runs a local, metadata-only git command (rev-parse, status,
// show-ref, worktree add/prune) with a hard timeout. Callers must invoke the
// returned cancel after Wait to release the timer.
func gitLocal(dir string, args ...string) (*exec.Cmd, context.CancelFunc) {
	return gitTimed(gitLocalTimeout, dir, args...)
}

// gitRemote runs a network git command (fetch, push, ls-remote) with a hard
// timeout. A timeout here usually means a credential helper is waiting for
// interactive input or the remote is unreachable — both are failures the
// caller should see promptly. Callers must invoke the returned cancel after
// Wait to release the timer.
func gitRemote(dir string, args ...string) (*exec.Cmd, context.CancelFunc) {
	return gitTimed(gitNetworkTimeout(), dir, args...)
}

func gitTimed(timeout time.Duration, dir string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd, cancel
}

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
	return AcquireProjectLock(projectRoot)
}

// AcquireProjectLock is the exported cross-process project Git lock. Change
// Run apply spans (and any other flow that touches the project's Git state)
// must hold it for their entire git-touching window — the same lock the
// worktree manager uses, so review commits, worktree setup, and controlled
// applies serialize instead of interleaving.
func AcquireProjectLock(projectRoot string) (func(), error) {
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

// ProjectRootForWorktree maps a worktree path back to its project root (the
// directory holding .multigent/), so callers that only have a worktree can
// still acquire the cross-process project lock. Paths outside a worktree
// layout are returned unchanged — they are their own project root.
func ProjectRootForWorktree(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if filepath.Base(filepath.Dir(path)) == "worktrees" && filepath.Base(filepath.Dir(filepath.Dir(path))) == ".multigent" {
		return filepath.Dir(filepath.Dir(filepath.Dir(path)))
	}
	return path
}

func projectRootForWorktree(path string) string {
	return ProjectRootForWorktree(path)
}

// WorktreeDir returns the absolute path for a task's worktree.
func WorktreeDir(projectRoot, taskID string) string {
	return filepath.Join(projectRoot, ".multigent", "worktrees", sanitizeTaskID(taskID))
}

// SanitizeTaskID sanitizes a task ID for use in filesystem paths.
func SanitizeTaskID(taskID string) string {
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

func sanitizeTaskID(taskID string) string {
	return SanitizeTaskID(taskID)
}

// EnsureWorktree prepares a dedicated git worktree for a task.
// If the worktree already exists, it returns its path and checked-out branch.
// Otherwise, it fetches the base branch, creates the feature branch, and adds the worktree.
// The QABaselineCapture result (S2-2) carries a freshly captured baseline
// document to the caller for control-plane persistence; the zero value
// means "no new capture" (existing worktree or capture failure).
func (m *Manager) EnsureWorktree(projectRoot, taskID, baseBranch, featureBranch string) (string, string, error, QABaselineCapture) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", "", err, QABaselineCapture{}
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
		return "", "", err, QABaselineCapture{}
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
// Callers should persist the same commit as the task's baseCommit. The
// QABaselineCapture result (S2-2) carries a freshly captured baseline
// document to the caller for control-plane persistence; the zero value
// means "no new capture" (existing worktree or capture failure).
func (m *Manager) EnsureWorktreeAt(projectRoot, taskID, baseCommit, featureBranch string) (string, string, error, QABaselineCapture) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unlock, err := acquireProjectLock(projectRoot)
	if err != nil {
		return "", "", err, QABaselineCapture{}
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
		if err := preserveRuntimeContract(projectRoot, targetDir); err != nil {
			log.Printf("[worktree] preserve runtime contract warning for %s: %v", targetDir, err)
		}
		return targetDir, nil
	}
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return "", fmt.Errorf("create worktrees parent dir: %w", err)
	}
	cmd, cancel := gitLocal(projectRoot, "worktree", "add", "--detach", targetDir, commit)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cancel()
		return "", fmt.Errorf("git snapshot worktree add failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	cancel()
	if err := preserveRuntimeContract(projectRoot, targetDir); err != nil {
		log.Printf("[worktree] preserve runtime contract warning for %s: %v", targetDir, err)
	}
	return targetDir, nil
}

func gitRevision(worktreeDir string) (string, error) {
	cmd, cancel := gitLocal(worktreeDir, "rev-parse", "HEAD^{commit}")
	defer cancel()
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
	cmdRemote, remoteCancel := gitLocal(projectRoot, "remote")
	if out, err := cmdRemote.Output(); func() bool { remoteCancel(); return err == nil }() && hasRemote(string(out), "origin") {
		cmdFetch, fetchCancel := gitRemote(projectRoot, "fetch", "origin", baseBranch)
		defer fetchCancel()
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
	cmd, cancel := gitLocal(projectRoot, "rev-parse", "--verify", ref+"^{commit}")
	defer cancel()
	return cmd.Run()
}

// gitRevParse verifies a ref and returns its full commit SHA.
func gitRevParse(projectRoot, ref string) (string, error) {
	cmd, cancel := gitLocal(projectRoot, "rev-parse", "--verify", ref+"^{commit}")
	defer cancel()
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

// QABaselineCapture carries a successfully captured baseline document out
// of ensureWorktree so the API layer can persist it to the control plane
// (the gitworktree package has no DB access by design).
type QABaselineCapture struct {
	Baseline gitworktreeBaseline
}

type gitworktreeBaseline = QABaseline

func (m *Manager) ensureWorktree(projectRoot, taskID, baseBranch, baseCommit, featureBranch string) (string, string, error, QABaselineCapture) {

	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return "", "", fmt.Errorf("project root is required"), QABaselineCapture{}
	}

	// Verify that projectRoot is a git repository
	gitDir := filepath.Join(projectRoot, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return "", "", fmt.Errorf("project root is not a git repository: %w", err), QABaselineCapture{}
	}
	sanitizeSharedGitConfig(projectRoot)

	targetDir := WorktreeDir(projectRoot, taskID)
	if _, err := os.Stat(targetDir); err == nil {
		// Worktree directory already exists
		branch, err := checkedOutBranch(targetDir)
		if err != nil {
			return "", "", fmt.Errorf("read existing worktree branch: %w", err), QABaselineCapture{}
		}
		if err := preserveRuntimeContract(projectRoot, targetDir); err != nil {
			log.Printf("[worktree] preserve runtime contract warning for %s: %v", targetDir, err)
		}
		return targetDir, branch, nil, QABaselineCapture{}
	}

	baseBranch = strings.TrimSpace(baseBranch)
	featureBranch = strings.TrimSpace(featureBranch)
	if featureBranch == "" {
		featureBranch = fmt.Sprintf("feature/%s", sanitizeTaskID(taskID))
	}

	// Create parent directory for worktrees
	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return "", "", fmt.Errorf("create worktrees parent dir: %w", err), QABaselineCapture{}
	}

	startPoint := strings.TrimSpace(baseCommit)
	if startPoint == "" {
		if baseBranch == "" {
			baseBranch = "main"
		}
		var err error
		startPoint, err = m.resolveBaseCommit(projectRoot, baseBranch)
		if err != nil {
			return "", "", err, QABaselineCapture{}
		}
	}

	// Check if the feature branch already exists locally
	branchExists := false
	cmdCheck, checkCancel := gitLocal(projectRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+featureBranch)
	if err := cmdCheck.Run(); err == nil {
		branchExists = true
	}
	checkCancel()

	var cmdWorktree *exec.Cmd
	var worktreeCancel context.CancelFunc
	if branchExists {
		// Checkout existing branch into worktree
		cmdWorktree, worktreeCancel = gitLocal(projectRoot, "worktree", "add", targetDir, featureBranch)
	} else {
		cmdWorktree, worktreeCancel = gitLocal(projectRoot, "worktree", "add", "-b", featureBranch, targetDir, startPoint)
	}
	defer worktreeCancel()

	var stderr bytes.Buffer
	cmdWorktree.Stderr = &stderr
	if err := cmdWorktree.Run(); err != nil {
		return "", "", fmt.Errorf("git worktree add failed: %w (stderr: %s)", err, stderr.String()), QABaselineCapture{}
	}

	branch, err := checkedOutBranch(targetDir)
	if err != nil {
		return "", "", fmt.Errorf("read created worktree branch: %w", err), QABaselineCapture{}
	}
	if err := preserveRuntimeContract(projectRoot, targetDir); err != nil {
		log.Printf("[worktree] preserve runtime contract warning for %s: %v", targetDir, err)
	}
	// QA baseline (fix round S2-1, Fix B; S2-2 hardened): fingerprint the
	// freshly materialized worktree BEFORE any agent runs and hand the
	// document to the caller for CONTROL-PLANE persistence — the gate
	// trusts only the control-plane copy, never a file inside the
	// agent-writable worktree (reviewer S2-2 P0). Capture failure stays
	// best-effort (no manifest, no baseline → legacy absolute surface).
	qaBaseline, qaBaselineErr := CaptureQABaseline(targetDir)
	if qaBaselineErr != nil {
		log.Printf("[worktree] qa baseline capture warning for %s: %v", targetDir, qaBaselineErr)
		return targetDir, branch, nil, QABaselineCapture{}
	}
	return targetDir, branch, nil, QABaselineCapture{Baseline: qaBaseline}
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
	cmd, cancel := gitLocal(worktreeDir, "branch", "--show-current")
	defer cancel()
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
	cmd, removeCancel := gitLocal(projectRoot, "worktree", "remove", "--force", targetDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		removeCancel()
		removeErr = fmt.Errorf("git worktree remove: %w (%s)", err, redactGitOutput(strings.TrimSpace(string(out))))
		log.Printf("[worktree-cleanup] %v", removeErr)
	} else {
		removeCancel()
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
	cmdPrune, pruneCancel := gitLocal(projectRoot, "worktree", "prune")
	_ = cmdPrune.Run()
	pruneCancel()

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

	cmd, listCancel := gitLocal(projectRoot, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	listCancel()
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
	cmdRemote, remoteCancel := gitLocal(projectRoot, "remote")
	out, err := cmdRemote.Output()
	remoteCancel()
	if err != nil || !hasRemote(string(out), "origin") {
		// No remote: nothing to fetch, treat as success.
		return nil
	}
	cmd, fetchCancel := gitRemote(projectRoot, "fetch", "origin", "--prune")
	cmd.Env = gitNetworkEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fetchCancel()
		return fmt.Errorf("git fetch origin --prune failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	fetchCancel()
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
		cmdResolve, resolveCancel := gitLocal(projectRoot, "rev-parse", "--verify", "--short=7", "refs/heads/"+name)
		out, err := cmdResolve.Output()
		resolveCancel()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			info.SHA = strings.TrimSpace(string(out))
		} else {
			cmd, remoteResolveCancel := gitLocal(projectRoot, "rev-parse", "--verify", "--short=7", "refs/remotes/origin/"+name)
			o, rerr := cmd.Output()
			remoteResolveCancel()
			if rerr == nil {
				info.SHA = strings.TrimSpace(string(o))
			}
		}
		if info.SHA != "" {
			cmdDate, dateCancel := gitLocal(projectRoot, "log", "-1", "--format=%cr", info.SHA)
			dout, derr := cmdDate.Output()
			dateCancel()
			if derr == nil {
				info.LastCommitDate = strings.TrimSpace(string(dout))
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func listBranches(projectRoot string) ([]string, error) {
	cmd, cancel := gitLocal(projectRoot, "branch", "-a", "--format=%(refname:short)")
	out, err := cmd.Output()
	cancel()
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
	cmd, cancel := gitLocal(projectRoot, "rev-parse", "--short=7", ref)
	out, err := cmd.Output()
	cancel()
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
	status, statusCancel := gitLocal(worktreeDir, "status", "--porcelain")
	out, err := status.Output()
	statusCancel()
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

	add, addCancel := gitLocal(worktreeDir, "add", "-A")
	if addOut, err := add.CombinedOutput(); err != nil {
		addCancel()
		return "", true, fmt.Errorf("git add: %w (%s)", err, redactGitOutput(strings.TrimSpace(string(addOut))))
	}
	addCancel()
	commit, commitCancel := gitLocal(worktreeDir, "commit", "-m", message)
	if commitOut, err := commit.CombinedOutput(); err != nil {
		commitCancel()
		return "", true, fmt.Errorf("git commit: %w (%s)", err, redactGitOutput(strings.TrimSpace(string(commitOut))))
	}
	commitCancel()
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
	status, statusCancel := gitLocal(worktreeDir, "status", "--porcelain")
	out, err := status.Output()
	statusCancel()
	if err != nil {
		return "", fmt.Errorf("check worktree status: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return "", fmt.Errorf("worktree has uncommitted changes")
	}

	cmd, headCancel := gitLocal(worktreeDir, "rev-parse", "--verify", "HEAD^{commit}")
	out, err = cmd.Output()
	headCancel()
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
	return m.PushBranchWithEnv(projectRoot, branch, expectedCommit, nil)
}

// PushBranchWithEnv is PushBranch with a caller-provided command environment.
//
// S2 round 8: the console host deliberately keeps NO credential anywhere on
// disk (credentials are injected per operation), so the historical
// "ambient environment only" push could never reach a private remote — the
// real fan-out's implementation step merged the branches, committed, and then
// hit `could not read Username` on every push attempt, leaving the delivery
// contract's remote-SHA check unsatisfiable. Callers on a credential-less
// host pass the same transient, process-scoped environment the push-evidence
// check uses (see Server.deliveryEvidenceEnv); nil keeps the historical
// gitNetworkEnv() behaviour exactly.
func (m *Manager) PushBranchWithEnv(projectRoot, branch, expectedCommit string, env []string) error {
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

	cmd, pushCancel := gitRemote(projectRoot, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	if len(env) > 0 {
		// The injected environment already carries the process environment plus
		// the transient credential config, so it replaces gitNetworkEnv()
		// wholesale rather than merging two GIT_CONFIG_* blocks.
		cmd.Env = env
	} else {
		cmd.Env = gitNetworkEnv()
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		pushCancel()
		return fmt.Errorf("git push origin %s failed: %w (%s)", branch, err, redactGitOutput(output.String()))
	}
	pushCancel()

	verify, verifyCancel := gitRemote(projectRoot, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if len(env) > 0 {
		// The verification reads the same remote the push just wrote; it needs
		// the same transient credential on a credential-less host.
		verify.Env = env
	}
	out, err := verify.Output()
	verifyCancel()
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

	resolve, resolveCancel := gitLocal(projectRoot, "rev-parse", "--verify", commit+"^{commit}")
	out, err := resolve.Output()
	resolveCancel()
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

	tagCmd, tagCancel := gitLocal(projectRoot, "tag", "-a", tag, "-m", message, targetSHA)
	var tagOut bytes.Buffer
	tagCmd.Stdout = &tagOut
	tagCmd.Stderr = &tagOut
	if err := tagCmd.Run(); err != nil {
		tagCancel()
		if strings.Contains(tagOut.String(), "already exists") {
			return fmt.Errorf("tag %s already exists", tag)
		}
		return fmt.Errorf("create tag %s failed: %w (%s)", tag, err, redactGitOutput(tagOut.String()))
	}
	tagCancel()

	push, tagPushCancel := gitRemote(projectRoot, "push", "origin", "refs/tags/"+tag+":refs/tags/"+tag)
	push.Env = gitNetworkEnv()
	var pushOut bytes.Buffer
	push.Stdout = &pushOut
	push.Stderr = &pushOut
	if err := push.Run(); err != nil {
		// Do not leave a local-only tag behind: a failed push must not make
		// the next attempt "already exists" against a tag nobody can fetch.
		delCmd, delCancel := gitLocal(projectRoot, "tag", "-d", tag)
		_ = delCmd.Run()
		delCancel()
		tagPushCancel()
		return fmt.Errorf("push tag %s failed: %w (%s)", tag, err, redactGitOutput(pushOut.String()))
	}
	tagPushCancel()

	verify, tagVerifyCancel := gitRemote(projectRoot, "ls-remote", "--tags", "origin", "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	out, err = verify.Output()
	tagVerifyCancel()
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
	cmd, cancel := gitLocal(projectRoot, "merge-base", "--is-ancestor", ancestor, descendant)
	err := cmd.Run()
	cancel()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check ancestor relationship: %w", err)
}

// MainMatchesRemote reports whether the repository's current default-branch
// HEAD equals origin's HEAD for that branch, using a local fetch-free
// comparison against the last-known remote tracking ref. Initialization tasks
// deliver directly on the default branch inside the sandbox (the agent
// pushes), so this — not a branch push — is their remote-sync evidence. A
// fetch is deliberately NOT run here: the agent's sync step just pushed, the
// tracking ref is fresh, and this check runs in the task-completion path
// where a network failure must not overwrite an honest local judgment with
// an error.
func (m *Manager) MainMatchesRemote(projectRoot, defaultBranch string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	projectRoot = strings.TrimSpace(projectRoot)
	defaultBranch = strings.TrimSpace(defaultBranch)
	if projectRoot == "" {
		return false, fmt.Errorf("project root is required")
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	cmdRemote, remoteCancel := gitLocal(projectRoot, "remote")
	remoteOut, remoteErr := cmdRemote.Output()
	remoteCancel()
	if remoteErr != nil || !hasRemote(string(remoteOut), "origin") {
		return false, fmt.Errorf("repository has no origin remote")
	}
	cmdLocal, localCancel := gitLocal(projectRoot, "rev-parse", "refs/heads/"+defaultBranch)
	localOut, err := cmdLocal.Output()
	localCancel()
	if err != nil {
		return false, fmt.Errorf("resolve local %s: %w", defaultBranch, err)
	}
	cmdTracking, trackingCancel := gitLocal(projectRoot, "rev-parse", "refs/remotes/origin/"+defaultBranch)
	trackingOut, err := cmdTracking.Output()
	trackingCancel()
	if err != nil {
		return false, fmt.Errorf("resolve origin/%s: %w", defaultBranch, err)
	}
	local := strings.TrimSpace(string(localOut))
	tracking := strings.TrimSpace(string(trackingOut))
	return local != "" && local == tracking, nil
}

// IsAncestor reports whether ancestor is reachable from descendant. It is
// used to detect squash/rebase integration where a task completion commit is
// no longer in the default branch history.
func (m *Manager) IsAncestor(projectRoot, ancestor, descendant string) (bool, error) {	m.mu.Lock()
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
	cmd, cancel := gitLocal(projectRoot, "merge-base", "--is-ancestor", ancestor, descendant)
	err = cmd.Run()
	cancel()
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
	cmdStatus, statusCancel := gitLocal(projectRoot, "status", "--porcelain")
	statusOut, err := cmdStatus.Output()
	statusCancel()
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
	cmdCheckout, checkoutCancel := gitLocal(projectRoot, "checkout", targetBranch)
	var stderrCheckout bytes.Buffer
	cmdCheckout.Stderr = &stderrCheckout
	if err := cmdCheckout.Run(); err != nil {
		checkoutCancel()
		return "", fmt.Errorf("checkout target branch %s failed: %w (%s)", targetBranch, err, stderrCheckout.String())
	}
	checkoutCancel()

	// 3. Perform merge with --no-ff
	if commitMessage == "" {
		commitMessage = fmt.Sprintf("merge: branch '%s' into '%s'", sourceBranch, targetBranch)
	}
	cmdMerge, mergeCancel := gitLocal(projectRoot, "merge", "--no-ff", "-m", commitMessage, sourceBranch)
	var stderrMerge bytes.Buffer
	cmdMerge.Stderr = &stderrMerge
	if err := cmdMerge.Run(); err != nil {
		// Attempt clean abort on failure / conflict
		cmdAbort, abortCancel := gitLocal(projectRoot, "merge", "--abort")
		_ = cmdAbort.Run()
		abortCancel()
		mergeCancel()
		return "", fmt.Errorf("merge branch %s into %s failed: %w (%s)", sourceBranch, targetBranch, err, stderrMerge.String())
	}
	mergeCancel()

	// 4. Retrieve resulting HEAD commit hash
	cmdRev, revCancel := gitLocal(projectRoot, "rev-parse", "HEAD")
	out, err := cmdRev.Output()
	revCancel()
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
	cmdRemote, remoteCancel := gitLocal(projectRoot, "remote")
	remoteOut, remoteErr := cmdRemote.Output()
	remoteCancel()
	if remoteErr == nil && hasRemote(string(remoteOut), "origin") {
		cmdBranch, branchCancel := gitLocal(projectRoot, "branch", "--show-current")
		branchOut, err := cmdBranch.Output()
		branchCancel()
		if err != nil {
			return fmt.Errorf("read current branch before sync: %w", err)
		}
		if current := strings.TrimSpace(string(branchOut)); current != defaultBranch {
			return fmt.Errorf("cannot sync %s: repository is on branch %s", defaultBranch, current)
		}

		cmdStatus, syncStatusCancel := gitLocal(projectRoot, "status", "--porcelain")
		statusOut, err := cmdStatus.Output()
		syncStatusCancel()
		if err != nil {
			return fmt.Errorf("check repository status before sync: %w", err)
		}
		if strings.TrimSpace(string(statusOut)) != "" {
			return fmt.Errorf("cannot sync %s: repository has uncommitted changes", defaultBranch)
		}

		cmdFetch, syncFetchCancel := gitRemote(projectRoot, "fetch", "origin", defaultBranch)
		cmdFetch.Env = gitNetworkEnv()
		if err := cmdFetch.Run(); err != nil {
			syncFetchCancel()
			return fmt.Errorf("fetch origin %s failed: %w", defaultBranch, err)
		}
		syncFetchCancel()
		cmdMerge, mergeCancel := gitLocal(projectRoot, "merge", "--ff-only", "origin/"+defaultBranch)
		if err := cmdMerge.Run(); err != nil {
			mergeCancel()
			return fmt.Errorf("fast-forward %s from origin failed: %w", defaultBranch, err)
		}
		mergeCancel()
	}
	return nil
}

func preserveRuntimeContract(projectRoot, targetDir string) error {
	projectRoot = strings.TrimSpace(projectRoot)
	targetDir = strings.TrimSpace(targetDir)
	if projectRoot == "" || targetDir == "" {
		return nil
	}
	rootContract := filepath.Join(projectRoot, ".multigent", "runtime.json")
	targetContract := filepath.Join(targetDir, ".multigent", "runtime.json")

	rootRaw, err := os.ReadFile(rootContract)
	if err != nil {
		return nil
	}
	var rootSpec struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(rootRaw, &rootSpec); err != nil || rootSpec.Version != 1 {
		return nil
	}

	targetRaw, err := os.ReadFile(targetContract)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(targetContract), 0755); err != nil {
			log.Printf("[worktree] failed to create runtime contract directory %s: %v", filepath.Dir(targetContract), err)
			return fmt.Errorf("create runtime contract directory: %w", err)
		}
		if err := os.WriteFile(targetContract, rootRaw, 0644); err != nil {
			log.Printf("[worktree] failed to copy runtime contract to %s: %v", targetContract, err)
			return fmt.Errorf("write runtime contract: %w", err)
		}
		return nil
	}
	if err == nil {
		var targetSpec struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(targetRaw, &targetSpec) != nil || targetSpec.Version != 1 {
			if err := os.WriteFile(targetContract, rootRaw, 0644); err != nil {
				log.Printf("[worktree] failed to heal invalid runtime contract in %s: %v", targetContract, err)
				return fmt.Errorf("heal runtime contract: %w", err)
			}
		}
	}
	return nil
}
