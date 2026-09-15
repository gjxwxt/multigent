// Apply engine for Change Run (batch plan §4.1 v6): a proposal's patch is
// validated inside a purified clone FIRST (no project state touched), then
// applied to the real worktree under the cross-process project Git lock with
// a CAS re-check, recording a per-path postimage hash for surgical rollback.
// V1 contract (approval §8.2): the worktree baseline must be clean; rollback
// restores touched paths only — never `git checkout -- .` / `reset --hard`.
package changerun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/gitworktree"
)

// highRiskPathSubstrings are rejected outright in proposal Paths and in the
// paths a patch actually touches (deterministic blacklist, §4.1 v6): CI/CD,
// deploy, secrets, agent-identity files, and git internals must not be
// rewritten through the change-run shortcut.
var highRiskPathSubstrings = []string{
	".gitlab-ci", ".github/workflows", "jenkinsfile",
	"dockerfile", ".dockerignore",
	"deploy/", "deployment/", "k8s/", "helm/", "terraform/",
	".env", "credentials", "secret", "id_rsa", ".pem", ".p12", ".pfx", ".key",
	"agents.md", "claude.md", ".multigent/", ".git/",
}

// ValidatePaths enforces the deterministic high-risk blacklist on declared
// proposal paths. Line-less errors name the offending path.
func ValidatePaths(paths []string) error {
	for _, p := range paths {
		if isHighRiskPath(p) {
			return fmt.Errorf("path %q is high-risk and rejected by the change-run blacklist", p)
		}
	}
	return nil
}

func isHighRiskPath(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(filepath.ToSlash(strings.TrimSpace(path)), "\\", "/"))
	if normalized == "" {
		return true
	}
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, "..") {
		return true
	}
	for _, banned := range highRiskPathSubstrings {
		if strings.Contains(normalized, banned) {
			return true
		}
	}
	return false
}

// PatchTouchedPaths extracts the file paths a unified diff touches, for
// cross-checking against the declared Paths and the blacklist.
func PatchTouchedPaths(patch string) []string {
	var paths []string
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			paths = append(paths, strings.TrimPrefix(line, "+++ b/"))
		} else if strings.HasPrefix(line, "+++ ") && line != "+++ /dev/null" {
			paths = append(paths, strings.TrimPrefix(line, "+++ "))
		}
	}
	sort.Strings(paths)
	return paths
}

// patchIsBinary rejects binary patches: `git apply` on binary data requires
// literals in the patch body which V1 will not carry.
func patchIsBinary(patch string) bool {
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") {
			return true
		}
	}
	return false
}

// ApplyEngine executes proposals against a task worktree.
type ApplyEngine struct {
	// WorktreeDir is the task's main worktree (the thing the patch targets).
	WorktreeDir string
	// ProjectRoot is the project root owning the cross-process Git lock.
	ProjectRoot string
	// CloneParent is the directory under which validation clones are created.
	CloneParent string
}

// ApplyResult reports what the apply did.
type ApplyResult struct {
	AppliedSHA string
	Postimage  map[string]string
}

// Apply runs the full §4.1 v6 sequence:
//
//  1. begin apply (CAS claim) and blacklist/consistency checks;
//  2. purified clone at the worktree's HEAD → apply patch → clean apply = OK;
//  3. project lock + CAS recheck (still applying) + dirty-baseline check;
//  4. real worktree apply + per-path postimage record + MarkApplied.
//
// On verification failure the proposal parks in verification_failed and the
// worktree is untouched. Returned errors are the operator's message; store
// state carries the machine truth.
func (e *ApplyEngine) Apply(store *Store, proposalID, actor string) (*ApplyResult, error) {
	p, err := store.Get(proposalID)
	if err != nil {
		return nil, err
	}
	if p.State != StateAwaitingApproval {
		return nil, fmt.Errorf("proposal %s in state %s, must be awaiting_approval", p.ID, p.State)
	}
	if err := ValidatePaths(p.Paths); err != nil {
		return nil, err
	}
	if patchIsBinary(p.Patch) {
		return nil, fmt.Errorf("binary patches are not supported by change run v1")
	}
	touched := PatchTouchedPaths(p.Patch)
	for _, tp := range touched {
		if isHighRiskPath(tp) {
			return nil, fmt.Errorf("patch touches high-risk path %q — rejected", tp)
		}
	}
	if len(touched) > 0 && len(p.Paths) > 0 {
		declared := map[string]bool{}
		for _, dp := range p.Paths {
			declared[strings.TrimSpace(dp)] = true
		}
		for _, tp := range touched {
			if !declared[tp] {
				return nil, fmt.Errorf("patch touches undeclared path %q", tp)
			}
		}
	}

	if _, err := store.BeginApply(p.ID); err != nil {
		return nil, err
	}
	// From here on every failure path must park the proposal somewhere
	// terminal — a stranded `applying` row would block the task forever
	// (single-active-proposal rule). Apply aborts back to awaiting_approval
	// so the operator can retry; verification failures park as failed.
	abort := func(stage, msg string) {
		if _, err := store.AbortApply(p.ID, map[string]string{"stage": stage, "error": msg}); err != nil {
			// Cannot happen without a DB fault; keep the original error live.
			_ = err
		}
	}

	// Baseline for the validation clone: the worktree's current HEAD.
	head, err := gitOut(e.WorktreeDir, "rev-parse", "HEAD")
	if err != nil {
		msg := fmt.Sprintf("read worktree HEAD: %v", err)
		abort("read_head", msg)
		return nil, fmt.Errorf("%s", msg)
	}
	head = strings.TrimSpace(head)

	cleanup, err := gitworktree.EnsurePurifiedClone(gitworktree.PurifiedCloneOptions{
		SourceRepo: e.WorktreeDir,
		Commit:     head,
		Dest:       filepath.Join(e.CloneParent, "changerun-validate-"+p.ID),
	})
	if err != nil {
		msg := fmt.Sprintf("validation clone: %v", err)
		abort("clone", msg)
		return nil, fmt.Errorf("%s", msg)
	}
	defer cleanup()
	cloneDir := filepath.Join(e.CloneParent, "changerun-validate-"+p.ID)

	if err := applyPatchIn(cloneDir, p.Patch); err != nil {
		_, _ = store.MarkVerificationFailed(p.ID, map[string]string{"stage": "clone_apply", "error": err.Error()})
		return nil, fmt.Errorf("clone apply failed (worktree untouched): %w", err)
	}

	// Re-acquire state under the project lock: the CAS transition to applying
	// already serialized concurrent appliers, and the project lock serializes
	// against review commits / worktree setup touching the same repo.
	unlock, err := gitworktree.AcquireProjectLock(e.ProjectRoot)
	if err != nil {
		msg := fmt.Sprintf("acquire project lock: %v", err)
		abort("project_lock", msg)
		return nil, fmt.Errorf("%s", msg)
	}
	defer unlock()

	// Dirty baseline is rejected (V1 decision, approval §8.2.1): the patch
	// context and postimage are only trustworthy against a clean tree.
	dirty, err := worktreeDirty(e.WorktreeDir)
	if err != nil {
		abort("status", err.Error())
		return nil, err
	}
	if dirty {
		msg := "worktree has uncommitted changes — commit or stash them before change run apply"
		abort("dirty_baseline", msg)
		return nil, fmt.Errorf("%s", msg)
	}

	// Fresh HEAD (the lock window may have followed a review commit).
	currentHead, err := gitOut(e.WorktreeDir, "rev-parse", "HEAD")
	if err != nil {
		abort("reread_head", err.Error())
		return nil, fmt.Errorf("re-read worktree HEAD: %w", err)
	}
	if strings.TrimSpace(currentHead) != head {
		_, _ = store.MarkVerificationFailed(p.ID, map[string]string{"stage": "baseline_moved", "expected": head, "actual": strings.TrimSpace(currentHead)})
		return nil, fmt.Errorf("worktree baseline moved during validation (%s -> %s) — re-propose against the new HEAD", head, strings.TrimSpace(currentHead))
	}

	if err := applyPatchIn(e.WorktreeDir, p.Patch); err != nil {
		_, _ = store.MarkVerificationFailed(p.ID, map[string]string{"stage": "worktree_apply", "error": err.Error()})
		return nil, fmt.Errorf("worktree apply failed: %w", err)
	}
	// Record postimage BEFORE declaring success: rollback integrity depends
	// on the hash snapshot, so a hash failure must abort the apply.
	postimage, err := e.recordPostimage(touched)
	if err != nil {
		return nil, fmt.Errorf("record postimage: %w", err)
	}
	final, err := store.MarkApplied(p.ID, postimage, head)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{AppliedSHA: final.AppliedSHA, Postimage: final.Postimage}, nil
}

// Rollback restores every touched path to its recorded postimage hash state.
// V1 semantic: verify-then-restore per path — if any current hash still
// matches the postimage, restore from HEAD; if a path was modified AFTER the
// apply (hash differs from postimage AND from HEAD), refuse: a surgical
// rollback would silently destroy newer work (approval §8.2.5 keeps copilot
// direct-writes out of scope for the same reason).
func (e *ApplyEngine) Rollback(store *Store, proposalID string) (*Proposal, error) {
	p, err := store.Get(proposalID)
	if err != nil {
		return nil, err
	}
	if p.State != StateApplied {
		return nil, fmt.Errorf("proposal %s in state %s, only applied proposals roll back", p.ID, p.State)
	}
	unlock, err := gitworktree.AcquireProjectLock(e.ProjectRoot)
	if err != nil {
		return nil, err
	}
	defer unlock()

	for path, postHash := range p.Postimage {
		abs := filepath.Join(e.WorktreeDir, filepath.FromSlash(path))
		current, err := hashFile(abs)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("hash %s: %w", path, err)
		}
		headContent, headErr := gitFileContent(e.WorktreeDir, path)
		if headErr != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read HEAD copy of %s: %w", path, headErr)
		}
		headHash := ""
		if headErr == nil {
			headHash = sha256Hex(headContent)
		}
		switch {
		case current == postHash:
			// Untouched since apply: safe to restore from HEAD.
			if os.IsNotExist(err) && headErr != nil {
				continue // file absent both now and at HEAD — nothing to do
			}
			if err := restoreFromHead(e.WorktreeDir, path); err != nil {
				return nil, err
			}
		case current == headHash:
			continue // already back at HEAD state
		default:
			return nil, fmt.Errorf("path %s changed after the apply (postimage %s, current %s) — refusing to clobber newer work", path, short(postHash), short(current))
		}
	}
	return store.transition(p.ID, StateApplied, StateRejected, func(np *Proposal) {
		np.RollbackState = "rolled_back"
	})
}

// recordPostimage hashes every touched path right after apply.
func (e *ApplyEngine) recordPostimage(touched []string) (map[string]string, error) {
	post := map[string]string{}
	for _, path := range touched {
		abs := filepath.Join(e.WorktreeDir, filepath.FromSlash(path))
		h, err := hashFile(abs)
		if os.IsNotExist(err) {
			// A patch may delete files; absence is a valid postimage.
			post[path] = "absent"
			continue
		}
		if err != nil {
			return nil, err
		}
		post[path] = h
	}
	return post, nil
}

// worktreeDirty reports uncommitted changes, ignoring platform runtime dirs.
func worktreeDirty(dir string) (bool, error) {
	out, err := gitOut(dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "?? .multigent") || strings.HasPrefix(line, "?? .git") {
			continue
		}
		return true, nil
	}
	return false, nil
}

// applyPatchIn runs git apply with the sanitized environment (no host
// hooks/fsmonitor/config; apply is a pure tree operation so the platform
// identity fallbacks are unnecessary).
func applyPatchIn(dir, patch string) error {
	cmd := exec.Command("git", "apply", "--whitespace=nowarn", "-")
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

// restoreFromHead checks out one path from HEAD (surgical, path-scoped).
func restoreFromHead(dir, path string) error {
	cmd := exec.Command("git", "checkout", "HEAD", "--", path)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restore %s: %w (%s)", path, err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

// gitFileContent reads a path's blob content at HEAD.
func gitFileContent(dir, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", "HEAD:"+path)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// gitOut runs git in dir with sanitized env, returning stdout only. Stderr
// is dropped (CombinedOutput would corrupt porcelain/rev-parse parsing).
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func short(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}
