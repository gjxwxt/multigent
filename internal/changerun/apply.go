// Apply engine for Change Run (batch plan §4.1 v6): a proposal's patch is
// validated inside a purified clone FIRST (no project state touched), then
// applied to the real worktree under the cross-process project Git lock with
// a CAS re-check, recording a per-path postimage hash for surgical rollback.
// V1 contract (approval §8.2): the worktree baseline must be clean; rollback
// restores touched paths only — never `git checkout -- .` / `reset --hard`.
package changerun

import (
	"context"
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
// cross-checking against the declared Paths and the blacklist. It reads both
// sides of each file header: +++ misses pure deletions and pure renames
// (--git a/x b/y with no ---/+++ hunk lines), and every touched path is a
// rollback surface, so the union of old/new paths is the touched set.
func PatchTouchedPaths(patch string) []string {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		// Headers may carry the a/ or b/ prefix in either position
		// (--- a/x, +++ b/x, --git a/x b/y, pure deletes keep a/).
		p = strings.TrimPrefix(p, "a/")
		p = strings.TrimPrefix(p, "b/")
		if p != "" && p != "/dev/null" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			add(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			add(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "diff --git "):
			// diff --git a/old b/new — rename/delete headers carry no
			// ---/+++ lines for pure operations; parse both sides. The
			// a/ prefix must be stripped explicitly: paths without a
			// b/ counterpart (pure deletes) keep their a/ form here.
			body := strings.TrimPrefix(line, "diff --git ")
			oldPart, newPart, ok := strings.Cut(body, " ")
			if ok {
				add(strings.TrimPrefix(oldPart, "a/"))
				add(newPart)
			}
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
	// Scope binds every operation to one (project, taskID) pair. Non-empty
	// values are enforced against the proposal record BEFORE any state
	// change or worktree touch — a proposal from another project/task must
	// surface as not-found, never as a cross-target apply (round-18 P0-1).
	Scope Scope
	// Validator runs the project's verification suite inside the validation
	// clone after the patch applies there and BEFORE the main worktree is
	// touched (§4.1 v6 unique ordering). Optional: nil = clone-apply-only
	// check, which the design contract calls an applicability check, not a
	// verification pass — the API layer wires the container validator so
	// project argv never runs on the host (round-18 P0-3).
	Validator Validator
}

// Scope is the (project, task) binding an engine (or handler) operates on.
type Scope struct {
	Project string
	TaskID  string
}

// matches reports whether the proposal belongs to this scope. Empty scope
// fields are skipped (library-level callers without a URL context); the
// HTTP layer always sets both.
func (sc Scope) matches(p *Proposal) bool {
	if sc.Project != "" && p.Project != sc.Project {
		return false
	}
	if sc.TaskID != "" && p.TaskID != sc.TaskID {
		return false
	}
	return true
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
func (e *ApplyEngine) Apply(store *Store, proposalID, actor string, ctx context.Context) (*ApplyResult, error) {
	p, err := store.Get(proposalID)
	if err != nil {
		return nil, err
	}
	if !e.Scope.matches(p) {
		return nil, &NotFoundError{Kind: "proposal", ID: proposalID}
	}
	if p.State != StateAwaitingApproval {
		return nil, fmt.Errorf("proposal %s in state %s, must be awaiting_approval", p.ID, p.State)
	}
	if err := ValidatePaths(p.Paths); err != nil {
		// Pre-validation failures leave the proposal in awaiting_approval
		// forever occupying the single-active slot — terminate it here so
		// the operator must re-propose (round-18 P1-2).
		_, _ = store.Reject(p.ID, "system")
		return nil, err
	}
	if patchIsBinary(p.Patch) {
		_, _ = store.Reject(p.ID, "system")
		return nil, fmt.Errorf("binary patches are not supported by change run v1")
	}
	touched := PatchTouchedPaths(p.Patch)
	for _, tp := range touched {
		if isHighRiskPath(tp) {
			_, _ = store.Reject(p.ID, "system")
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
				_, _ = store.Reject(p.ID, "system")
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

	// In-clone verification (§4.1 v6 unique ordering: ALL verification —
	// including source-writing commands — happens HERE, before the project
	// lock and the main-worktree apply). A validator failure parks the
	// proposal as verification_failed with the per-command trail; the
	// worktree was never touched. Without a Validator this is an
	// applicability check only — the API layer always wires the container
	// validator (round-18 P0-3).
	if e.Validator != nil {
		results, err := e.Validator.VerifyDir(ctx, cloneDir)
		if err != nil {
			detail := verificationSummary(results)
			detail["stage"] = "clone_verify"
			detail["error"] = err.Error()
			_, _ = store.MarkVerificationFailed(p.ID, detail)
			return nil, fmt.Errorf("clone verification failed (worktree untouched): %w", err)
		}
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
	// on the hash snapshot. A hash failure here means the worktree now holds
	// applied content with no trustworthy rollback key — reverse the patch
	// immediately, then park the proposal as failed. Never return with the
	// row stuck in `applying` (round-18 P1-2).
	postimage, err := e.recordPostimage(touched)
	if err != nil {
		if revErr := applyPatchReverse(e.WorktreeDir, p.Patch); revErr != nil {
			postimage, _ = e.recordPostimage(touched)
			_, _ = store.MarkVerificationFailed(p.ID, map[string]string{
				"stage":     "postimage_record",
				"error":     err.Error(),
				"rollback":  fmt.Sprintf("reverse apply also failed (%v); worktree may hold unrolled content", revErr),
				"postimage": fmt.Sprintf("%v", postimage),
			})
			return nil, fmt.Errorf("record postimage failed (%v) AND reverse apply failed (%v) — manual inspection required", err, revErr)
		}
		_, _ = store.MarkVerificationFailed(p.ID, map[string]string{
			"stage":    "postimage_record",
			"error":    err.Error(),
			"rollback": "reverse apply succeeded — worktree restored to baseline",
		})
		return nil, fmt.Errorf("record postimage failed; patch reversed out of the worktree: %w", err)
	}
	final, err := store.MarkApplied(p.ID, postimage, head)
	if err != nil {
		// Applied content + no applied-state row: reverse out, then park.
		if revErr := applyPatchReverse(e.WorktreeDir, p.Patch); revErr != nil {
			_, _ = store.MarkVerificationFailed(p.ID, map[string]string{
				"stage":    "mark_applied",
				"error":    err.Error(),
				"rollback": fmt.Sprintf("reverse apply also failed (%v); worktree may hold unrolled content", revErr),
			})
			return nil, fmt.Errorf("mark applied failed (%v) AND reverse apply failed (%v) — manual inspection required", err, revErr)
		}
		_, _ = store.MarkVerificationFailed(p.ID, map[string]string{
			"stage":    "mark_applied",
			"error":    err.Error(),
			"rollback": "reverse apply succeeded — worktree restored to baseline",
		})
		return nil, fmt.Errorf("mark applied failed; patch reversed out of the worktree: %w", err)
	}
	return &ApplyResult{AppliedSHA: final.AppliedSHA, Postimage: final.Postimage}, nil
}

// applyPatchReverse applies the ORIGINAL patch with git's own --reverse to
// dir (round-19 P1): git understands every header variant and recomputes
// inverted hunk ranges — the hand-rolled inversion this replaces produced
// illegal headers (e.g. "@@ +1,7 -1,5") and stale line counts, so the
// recordPostimage/MarkApplied failure compensation it served could not be
// trusted. Failure text goes to the caller; nothing is parsed from git.
func applyPatchReverse(dir, patch string) error {
	cmd := exec.Command("git", "apply", "--reverse", "--whitespace=nowarn", "-")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(patch)
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		_ = out
		return fmt.Errorf("git apply --reverse: %w", err)
	}
	return nil
}

// Rollback restores every touched path to its recorded postimage hash state.
// V1 semantic: verify-then-restore per path — if any current hash still
// matches the postimage, restore from HEAD; if a path was modified AFTER the
// apply (hash differs from postimage AND from HEAD), refuse: a surgical
// rollback would silently destroy newer work (approval §8.2.5 keeps copilot
// direct-writes out of scope for the same reason).
// fileState is the existence+content+mode snapshot of one path, at the
// worktree now or at HEAD. Absence is a first-class state (postimage
// "absent" for deletions, HEAD-absent for additions) — a hash comparison
// alone cannot express it (round-18 P0-2).
type fileState struct {
	Absent bool
	Hash   string
	Mode   os.FileMode
}

// currentFileState snapshots a path in the worktree.
func currentFileState(worktreeDir, path string) (fileState, error) {
	abs := filepath.Join(worktreeDir, filepath.FromSlash(path))
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return fileState{Absent: true}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	if !info.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("path %s is not a regular file (mode %v) — change run v1 only manages regular files", path, info.Mode())
	}
	h, err := hashFile(abs)
	if err != nil {
		return fileState{}, err
	}
	return fileState{Hash: h, Mode: info.Mode().Perm()}, nil
}

// headFileState snapshots a path at HEAD (absent when git knows no blob).
func headFileState(worktreeDir, path string) (fileState, error) {
	content, err := gitFileContent(worktreeDir, path)
	if err != nil {
		if gitFileMissing(err) {
			return fileState{Absent: true}, nil
		}
		return fileState{}, err
	}
	mode := fileModeFromGitMode(gitFileModeAtHead(worktreeDir, path))
	return fileState{Hash: sha256Hex(content), Mode: mode}, nil
}

// gitFileMissing reports whether a git show failure means "no blob at HEAD".
func gitFileMissing(err error) bool {
	if err == nil {
		return false
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode() == 128
	}
	return strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "exists on disk, but not in")
}

// gitFileModeAtHead reads the blob's 6-digit git mode ("100644"/"100755");
// unknown → 100644.
func gitFileModeAtHead(worktreeDir, path string) string {
	out, err := gitOut(worktreeDir, "ls-tree", "HEAD", "--", path)
	if err != nil || out == "" {
		return "100644"
	}
	fields := strings.Fields(out)
	if len(fields) >= 1 {
		return fields[0]
	}
	return "100644"
}

// fileModeFromGitMode converts a git blob mode to an os.FileMode.
func fileModeFromGitMode(gitMode string) os.FileMode {
	if gitMode == "100755" {
		return 0o755
	}
	return 0o644
}

// Rollback surgically restores every touched path to its pre-apply state
// (the recorded postimage), supporting additions (delete the file),
// deletions (restore from HEAD), and modifications (restore HEAD content).
// Preflight verifies ALL paths first and the restore loop runs under the
// project lock; any path edited after the apply aborts the whole rollback
// before a single file is touched, so a half-rolled-back state cannot arise
// from policy violations (round-18 P0-2). Content-restore failures mid-loop
// are reported with the per-path outcome list.
func (e *ApplyEngine) Rollback(store *Store, proposalID string) (*Proposal, error) {
	p, err := store.Get(proposalID)
	if err != nil {
		return nil, err
	}
	if !e.Scope.matches(p) {
		return nil, &NotFoundError{Kind: "proposal", ID: proposalID}
	}
	if p.State != StateApplied {
		return nil, fmt.Errorf("proposal %s in state %s, only applied proposals roll back", p.ID, p.State)
	}
	unlock, err := gitworktree.AcquireProjectLock(e.ProjectRoot)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// ── Preflight: classify every touched path without touching anything.
	// Semantics: the postimage IS the applied state; rollback means returning
	// to the HEAD state. A path still holding applied content is the NORMAL
	// restore case, not a no-op. A path holding NEITHER applied content NOR
	// HEAD content was edited after the apply — refuse (never clobber newer
	// work; the preflight refusal touches nothing, so no half-rollback).
	type plan struct {
		path   string
		action string // "delete" (apply created it), "restore" (return to HEAD), "none"
	}
	var plans []plan
	for path, postHash := range p.Postimage {
		now, err := currentFileState(e.WorktreeDir, path)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		head, err := headFileState(e.WorktreeDir, path)
		if err != nil {
			return nil, fmt.Errorf("read HEAD state of %s: %w", path, err)
		}
		if postHash == "absent" {
			// The apply DELETED this file; rollback = restore from HEAD
			// (HEAD must have had it). If the file reappeared afterwards
			// (now present), that's newer work on top — refuse unless it
			// already matches HEAD.
			if now.Absent {
				plans = append(plans, plan{path: path, action: "restore"})
			} else {
				head2, err := headFileState(e.WorktreeDir, path)
				if err != nil {
					return nil, fmt.Errorf("read HEAD state of %s: %w", path, err)
				}
				if !head2.Absent && now.Hash == head2.Hash {
					plans = append(plans, plan{path: path, action: "none"})
				} else {
					return nil, fmt.Errorf("path %s reappeared after the apply — refusing to clobber newer work", path)
				}
			}
			continue
		}
		switch {
		case now.Absent:
			// Applied content was removed after apply. HEAD had content
			// (postimage wasn't absent) → restore it; HEAD-absent means the
			// apply deleted the file and someone confirmed the deletion.
			if head.Absent {
				plans = append(plans, plan{path: path, action: "none"})
			} else {
				plans = append(plans, plan{path: path, action: "restore"})
			}
		case head.Absent:
			// No blob at HEAD but content exists now → the apply added it
			// (postimage hashed it). Rollback = delete. Postimage mismatch
			// means the file was edited after apply — refuse.
			if postHash == "absent" || now.Hash == postHash {
				plans = append(plans, plan{path: path, action: "delete"})
			} else {
				return nil, fmt.Errorf("path %s changed after the apply (postimage %s, current %s) — refusing to clobber newer work", path, short(postHash), short(now.Hash))
			}
		case now.Hash == head.Hash && now.Mode == head.Mode:
			// Already back at HEAD state — nothing to do.
			plans = append(plans, plan{path: path, action: "none"})
		case now.Hash == postHash:
			// Still holding the applied content — the normal restore case.
			plans = append(plans, plan{path: path, action: "restore"})
		default:
			return nil, fmt.Errorf("path %s changed after the apply (postimage %s, current %s, HEAD %s) — refusing to clobber newer work", path, short(postHash), short(now.Hash), short(head.Hash))
		}
	}

	// ── Restore loop: no policy failures possible past this point.
	for _, pl := range plans {
		switch pl.action {
		case "delete":
			if err := os.Remove(filepath.Join(e.WorktreeDir, filepath.FromSlash(pl.path))); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("rollback delete %s: %w", pl.path, err)
			}
		case "restore":
			if err := restoreFromHead(e.WorktreeDir, pl.path); err != nil {
				return nil, err
			}
		}
	}
	return store.transitionTerminal(p.ID, StateApplied, StateRejected, func(np *Proposal) {
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
