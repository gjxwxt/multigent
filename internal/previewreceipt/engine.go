package previewreceipt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/secretbox"
)

var (
	ErrConflict = errors.New("conflict: touched files have drifted or slot is occupied")
	ErrNotFound = ErrReceiptNotFound
)

// AgentRunner executes code modifications against an isolated clone.
type AgentRunner interface {
	RunAgent(ctx context.Context, cloneDir string, prompt string) error
}

// AgentRunnerFunc is an adapter to allow the use of ordinary functions as AgentRunner.
type AgentRunnerFunc func(ctx context.Context, cloneDir string, prompt string) error

func (f AgentRunnerFunc) RunAgent(ctx context.Context, cloneDir string, prompt string) error {
	return f(ctx, cloneDir, prompt)
}

// TurnEngine orchestrates Turn isolation, execution, verification, capture and rollback.
type TurnEngine struct {
	Store               *Store
	onAfterReverseApply func(receipt *PreviewReceipt) // test seam for rollback final CAS fault injection
}

func NewTurnEngine(store *Store) *TurnEngine {
	return &TurnEngine{
		Store: store,
	}
}

type ExecuteTurnParams struct {
	WorkspaceID    string
	Project        string
	ProjectGitRoot string
	TaskID         string
	WorktreeDir    string
	Prompt         string
	Actor          string
	Runner         AgentRunner
	LeaseDuration  time.Duration
}

type RollbackTurnParams struct {
	Project        string
	ProjectGitRoot string
	TaskID         string
	TurnID         string
	WorktreeDir    string
	Actor          string
}

// ExecuteTurn runs the complete Slice B Turn lifecycle:
// 1. Lock project worktree
// 2. Materialize baseline commit into refs/mg-turns/<turnID>
// 3. Create receipt (PENDING) and lock single-active slot
// 4. Create purified clone at baseline
// 5. Run agent in clone and consume exit code
// 6. Verify main worktree has not drifted from baseline
// 7. Extract, scan, and validate patch
// 8. Seal OperationalPatch strictly and sanitize DisplayDiff
// 9. Apply patch to main worktree and record postimages
// 10. CAS transition to CAPTURED (with reverse apply compensation on failure)
func (e *TurnEngine) ExecuteTurn(ctx context.Context, params ExecuteTurnParams) (*PreviewReceipt, error) {
	if params.ProjectGitRoot == "" || params.WorktreeDir == "" || params.TaskID == "" || params.Runner == nil {
		return nil, errors.New("invalid execute turn parameters")
	}

	turnID := "turn-" + randHex(8)

	// 1. Acquire project lock
	unlock, err := gitworktree.AcquireProjectLock(params.ProjectGitRoot)
	if err != nil {
		return nil, fmt.Errorf("acquire project lock: %w", err)
	}
	defer unlock()

	// 2. Materialize baseline commit
	baseRes, err := MaterializeBaseline(ctx, params.WorktreeDir, turnID)
	if err != nil {
		return nil, fmt.Errorf("materialize baseline: %w", err)
	}
	defer baseRes.Cleanup()

	// 3. Create receipt with slot in PENDING state
	lease := params.LeaseDuration
	if lease <= 0 {
		lease = DefaultLeaseDuration
	}
	receipt, err := e.Store.Create(ctx, CreateParams{
		Project:        params.Project,
		TaskID:         params.TaskID,
		TurnID:         turnID,
		BaselineTree:   baseRes.Tree,
		BaselineCommit: baseRes.Commit,
		BaselineRef:    baseRes.Ref,
		RequestDigest:  ComputeRequestDigest(params.Prompt),
		RedactedPrompt: CapAndRedact(params.Prompt, MaxPromptBytes),
		Creator:        params.Actor,
		LeaseDuration:  lease,
	})
	if err != nil {
		return nil, fmt.Errorf("create receipt with slot: %w", err)
	}

	// Helper to fail receipt cleanly
	fail := func(reason string) (*PreviewReceipt, error) {
		redactedReason := CapAndRedact(reason, MaxFailureReasonBytes)
		// Use detached context so that if ctx was canceled (e.g. operator stop),
		// the DB transition to StatusFailed still succeeds cleanly.
		transCtx, transCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer transCancel()
		failed, _ := e.Store.Transition(transCtx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusFailed, func(r *PreviewReceipt) error {
			r.FailureReason = redactedReason
			return nil
		})
		return failed, fmt.Errorf("%s", redactedReason)
	}

	// 4. Transition to EXECUTING
	receipt, err = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusExecuting, nil)
	if err != nil {
		return nil, fmt.Errorf("transition to executing: %w", err)
	}

	// 5. Ensure purified clone
	cloneParent, err := os.MkdirTemp("", "mg-turn-clone-parent-*")
	if err != nil {
		return fail("create temp clone directory: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(cloneParent) }()
	cloneDir := filepath.Join(cloneParent, "clone")

	cloneCleanup, err := gitworktree.EnsurePurifiedClone(gitworktree.PurifiedCloneOptions{
		SourceRepo: params.WorktreeDir,
		Commit:     baseRes.Commit,
		Ref:        baseRes.Ref,
		Dest:       cloneDir,
	})
	if err != nil {
		return fail("create purified clone: " + err.Error())
	}
	defer cloneCleanup()

	// 6. Run agent in isolated clone
	if err := params.Runner.RunAgent(ctx, cloneDir, params.Prompt); err != nil {
		return fail(fmt.Sprintf("agent execution failed: %v", err))
	}

	// 7. Transition to CAPTURING
	receipt, err = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusCapturing, nil)
	if err != nil {
		return nil, fmt.Errorf("transition to capturing: %w", err)
	}

	// 8. Re-verify main worktree matches baseline tree (drift detection)
	match, err := VerifyWorktreeMatchesTree(ctx, params.WorktreeDir, baseRes.Tree)
	if err != nil || !match {
		msg := "worktree drifted; aborting to avoid overwriting external changes"
		if err != nil {
			msg = fmt.Sprintf("worktree verify error: %v", err)
		}
		return fail(msg)
	}

	// 9. Stage all changes in clone and generate diff
	if err := stageAllIn(ctx, cloneDir); err != nil {
		return fail("stage clone changes: " + err.Error())
	}

	rawDiff, err := generateDiffIn(ctx, cloneDir, baseRes.Commit)
	if err != nil {
		return fail("generate diff: " + err.Error())
	}
	if strings.TrimSpace(rawDiff) == "" {
		return fail("agent completed successfully but produced no code modifications")
	}

	// 10. Security scanner on diff and touched paths
	touched := PatchTouchedPaths(rawDiff)
	if err := ValidatePatchPaths(touched); err != nil {
		return fail("security validation rejected paths: " + err.Error())
	}
	if err := ScanSensitiveDiff(rawDiff); err != nil {
		return fail("security validation rejected sensitive diff: " + err.Error())
	}

	// 11. Pre-check patch applicability on main worktree
	if err := checkPatchIn(params.WorktreeDir, rawDiff); err != nil {
		return fail("patch cannot be cleanly applied: " + err.Error())
	}

	// 12. Strictly seal operational patch and sanitize display diff
	sealedPatch, err := secretbox.SealBytesStrict([]byte(rawDiff))
	if err != nil {
		return fail("strict encryption failed: " + err.Error())
	}
	displayDiff := SanitizeDisplayDiff(rawDiff)

	// 13. Apply patch to main worktree
	if err := applyPatchIn(params.WorktreeDir, rawDiff); err != nil {
		return fail("apply patch to worktree: " + err.Error())
	}

	// 14. Capture postimages of touched files
	postimages, err := CapturePostimages(params.WorktreeDir, touched)
	if err != nil {
		// Compensatory rollback
		revErr := reverseApplyPatchIn(params.WorktreeDir, rawDiff)
		if revErr != nil {
			failed, _ := e.Store.Transition(context.Background(), params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
				r.FailureReason = CapAndRedact(fmt.Sprintf("capture postimages failed (%v) and compensation reverse apply failed (%v)", err, revErr), MaxFailureReasonBytes)
				return nil
			})
			return failed, fmt.Errorf("compensation reverse apply failed: %w", revErr)
		}
		return fail("capture postimages: " + err.Error())
	}

	// 15. Materialize turn snapshot in .multigent/turns/<taskID>/<turnID>/snapshot
	snapshotDir := TurnSnapshotDir(params.ProjectGitRoot, params.TaskID, turnID)
	if err := MaterializeSnapshot(params.WorktreeDir, snapshotDir); err != nil {
		revErr := reverseApplyPatchIn(params.WorktreeDir, rawDiff)
		if revErr != nil {
			failed, _ := e.Store.Transition(context.Background(), params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
				r.FailureReason = CapAndRedact(fmt.Sprintf("materialize snapshot failed (%v) and compensation reverse apply failed (%v)", err, revErr), MaxFailureReasonBytes)
				return nil
			})
			return failed, fmt.Errorf("compensation reverse apply failed: %w", revErr)
		}
		return fail("materialize snapshot: " + err.Error())
	}

	// 16. CAS transition to CAPTURED
	captured, err := e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusCaptured, func(r *PreviewReceipt) error {
		r.OperationalPatch = sealedPatch
		r.DisplayDiff = displayDiff
		r.TouchedPaths = touched
		r.Postimages = postimages
		return nil
	})
	if err != nil {
		// Compensatory rollback on CAS failure
		revErr := reverseApplyPatchIn(params.WorktreeDir, rawDiff)
		_ = os.RemoveAll(snapshotDir)
		if revErr != nil {
			failed, _ := e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
				r.FailureReason = CapAndRedact(fmt.Sprintf("cas commit failed (%v) and compensation reverse apply failed (%v)", err, revErr), MaxFailureReasonBytes)
				return nil
			})
			return failed, fmt.Errorf("compensation reverse apply failed: %w", revErr)
		}
		return fail("commit captured receipt state failed: " + err.Error())
	}

	// 17. Atomically supersede older overlapping CAPTURED receipts
	allRecs, listErr := e.Store.List(ctx, params.Project, params.TaskID)
	if listErr == nil {
		touchedSet := make(map[string]struct{}, len(touched))
		for _, p := range touched {
			touchedSet[p] = struct{}{}
		}

		for _, oldRec := range allRecs {
			if oldRec.ID == captured.ID || oldRec.Status != StatusCaptured {
				continue
			}
			overlap := false
			for _, oldPath := range oldRec.TouchedPaths {
				if _, ok := touchedSet[oldPath]; ok {
					overlap = true
					break
				}
			}
			if overlap {
				_, _ = e.Store.Transition(ctx, params.Project, params.TaskID, oldRec.ID, oldRec.Revision, StatusSuperseded, nil)
				oldSnapshotDir := TurnSnapshotDir(params.ProjectGitRoot, params.TaskID, oldRec.TurnID)
				_ = os.RemoveAll(oldSnapshotDir)
			}
		}
	}

	return captured, nil
}

// RollbackTurn performs a surgical rollback of a captured turn:
// 1. Lock project worktree
// 2. Fetch receipt and verify state is CAPTURED
// 3. Verify all touched files still match postimage fingerprints byte-for-byte BEFORE transitioning
// 4. Pre-verify decrypt of OperationalPatch BEFORE transitioning to REVERTING
// 5. Transition to REVERTING
// 6. Apply reverse patch (if fails, transition to REVERT_FAILED, hold slot, and require manual inspection)
// 7. Transition to ROLLED_BACK
func (e *TurnEngine) RollbackTurn(ctx context.Context, params RollbackTurnParams) error {
	if params.ProjectGitRoot == "" || params.WorktreeDir == "" || params.TaskID == "" || params.TurnID == "" {
		return errors.New("invalid rollback turn parameters")
	}

	unlock, err := gitworktree.AcquireProjectLock(params.ProjectGitRoot)
	if err != nil {
		return fmt.Errorf("acquire project lock: %w", err)
	}
	defer unlock()

	receipt, err := e.Store.Get(ctx, params.Project, params.TaskID, params.TurnID)
	if err != nil {
		return fmt.Errorf("get receipt: %w", err)
	}

	if receipt.Status != StatusCaptured {
		if receipt.Status == StatusCommitting {
			return fmt.Errorf("%w: turn %s is currently committing", ErrConflict, params.TurnID)
		}
		if receipt.Status == StatusRolledBack {
			return fmt.Errorf("turn %s has already been rolled back", params.TurnID)
		}
		return fmt.Errorf("turn %s in status %s cannot be rolled back (must be captured)", params.TurnID, receipt.Status)
	}

	// 1. Verify all touched files still match postimage BEFORE any state transition
	matches, err := VerifyWorktreeMatchesPostimages(params.WorktreeDir, receipt.Postimages)
	if err != nil || !matches {
		msg := "touched files have been modified or deleted since the turn was captured; rollback aborted to prevent data loss"
		if err != nil {
			msg = fmt.Sprintf("verify postimages failed: %v", err)
		}
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	}

	// 2. Decrypt patch BEFORE transitioning to REVERTING
	patchBytes, err := secretbox.OpenBytesStrict(receipt.OperationalPatch)
	if err != nil {
		return fmt.Errorf("decrypt operational patch: %w", err)
	}

	// 3. Transition to REVERTING
	receipt, err = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusReverting, nil)
	if err != nil {
		return fmt.Errorf("transition to reverting: %w", err)
	}

	// 4. Apply reverse patch
	if err := reverseApplyPatchIn(params.WorktreeDir, string(patchBytes)); err != nil {
		// Reverse apply failed: mark as REVERT_FAILED to flag dirty worktree requiring manual inspection
		_, _ = e.Store.Transition(context.Background(), params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
			r.FailureReason = CapAndRedact("reverse apply failed: "+err.Error(), MaxFailureReasonBytes)
			return nil
		})
		return fmt.Errorf("reverse apply patch: %w", err)
	}

	// Clean up snapshot dir on successful rollback
	snapshotDir := TurnSnapshotDir(params.ProjectGitRoot, params.TaskID, receipt.TurnID)
	_ = os.RemoveAll(snapshotDir)

	if e.onAfterReverseApply != nil {
		e.onAfterReverseApply(receipt)
	}

	// 5. Transition to ROLLED_BACK
	_, err = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusRolledBack, nil)
	if err != nil {
		failErr := err
		// Critical fence: reverse apply succeeded on disk, but final CAS to ROLLED_BACK failed.
		// Transition to StatusRevertFailed so the receipt is marked for manual inspection and slot remains held.
		targetRev := receipt.Revision
		if latest, getErr := e.Store.Get(context.Background(), params.Project, params.TaskID, receipt.ID); getErr == nil && latest != nil {
			targetRev = latest.Revision
		}
		_, _ = e.Store.Transition(context.Background(), params.Project, params.TaskID, receipt.ID, targetRev, StatusRevertFailed, func(r *PreviewReceipt) error {
			r.FailureReason = CapAndRedact("reverse apply succeeded on disk but final CAS transition to ROLLED_BACK failed: "+failErr.Error()+" (manual inspection required)", MaxFailureReasonBytes)
			return nil
		})
		return fmt.Errorf("transition to rolled_back failed (receipt marked for manual inspection): %w", failErr)
	}

	return nil
}

// TurnSnapshotDir returns the directory path where a turn's snapshot is stored.
func TurnSnapshotDir(projectGitRoot, taskID, turnID string) string {
	return filepath.Join(projectGitRoot, ".multigent", "turns", gitworktree.SanitizeTaskID(taskID), sanitizeTurnID(turnID), "snapshot")
}

func sanitizeTurnID(turnID string) string {
	turnID = strings.TrimSpace(turnID)
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-")
	cleaned := r.Replace(turnID)
	if strings.HasPrefix(cleaned, ".") {
		cleaned = "turn" + strings.TrimLeft(cleaned, ".")
	}
	if cleaned == "" {
		cleaned = "turn"
	}
	return cleaned
}

// MaterializeSnapshot copies project files from sourceDir to snapshotDir,
// skipping .git and .multigent directories.
func MaterializeSnapshot(sourceDir, snapshotDir string) error {
	_ = os.RemoveAll(snapshotDir)
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	return copyDir(sourceDir, snapshotDir)
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || name == ".multigent" {
			continue
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dstPath); err != nil {
				return err
			}
			continue
		}
		if entry.IsDir() {
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return err
			}
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dstPath, data, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func stageAllIn(ctx context.Context, dir string) error {
	if err := gitworktree.SanitizeUntrustedClone(dir); err != nil {
		return fmt.Errorf("sanitize untrusted clone: %w", err)
	}
	args := gitworktree.SanitizedExecutionArgsForDir(dir, "add", "--all")
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add --all: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

func generateDiffIn(ctx context.Context, dir, baselineCommit string) (string, error) {
	args := gitworktree.SanitizedExecutionArgsForDir(dir, "diff", "--no-ext-diff", "--no-textconv", "--no-color", baselineCommit)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return string(out), nil
}

func applyPatchIn(dir, patch string) error {
	args := gitworktree.SanitizedExecutionArgs("apply", "--whitespace=nowarn", "-")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

func reverseApplyPatchIn(dir, patch string) error {
	args := gitworktree.SanitizedExecutionArgs("apply", "--reverse", "--whitespace=nowarn", "-")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --reverse: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

func checkPatchIn(dir, patch string) error {
	args := gitworktree.SanitizedExecutionArgs("apply", "--check", "--whitespace=nowarn", "-")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --check: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

type PrepareCommitReceiptsParams struct {
	Project        string
	ProjectGitRoot string
	TaskID         string
	CommitIntentID string
}

// PrepareCommitReceipts transitions all CAPTURED receipts for a task to COMMITTING state.
// It returns an error if any receipt is currently in a mutating state (PENDING, EXECUTING, CAPTURING, REVERTING).
func (e *TurnEngine) PrepareCommitReceipts(ctx context.Context, params PrepareCommitReceiptsParams) ([]*PreviewReceipt, error) {
	if params.Project == "" || params.TaskID == "" {
		return nil, errors.New("invalid prepare commit parameters")
	}

	receipts, err := e.Store.List(ctx, params.Project, params.TaskID)
	if err != nil {
		return nil, fmt.Errorf("list receipts: %w", err)
	}

	// Invariant: cannot commit while a turn is actively executing or reverting
	for _, rec := range receipts {
		if rec.Status == StatusPending || rec.Status == StatusExecuting || rec.Status == StatusCapturing || rec.Status == StatusReverting {
			return nil, fmt.Errorf("%w: turn %s is actively %s", ErrConflict, rec.TurnID, rec.Status)
		}
	}

	var committing []*PreviewReceipt
	for _, rec := range receipts {
		if rec.Status != StatusCaptured {
			continue
		}
		cRec, err := e.Store.Transition(ctx, params.Project, params.TaskID, rec.ID, rec.Revision, StatusCommitting, func(r *PreviewReceipt) error {
			r.CommitIntentID = params.CommitIntentID
			return nil
		})
		if err != nil {
			// If any transition fails, abort and flag all already-transitioned receipts
			_ = e.AbortCommitReceipts(ctx, params.Project, params.TaskID, committing, "failed transitioning to committing: "+err.Error())
			return nil, fmt.Errorf("transition receipt %s to committing failed: %w", rec.ID, err)
		}
		committing = append(committing, cRec)
	}
	return committing, nil
}

// FinalizeCommitReceipts transitions all COMMITTING receipts for a task to COMMITTED state
// with the resulting Git commit SHA and cleans up their snapshot directories.
func (e *TurnEngine) FinalizeCommitReceipts(ctx context.Context, project, taskID string, receipts []*PreviewReceipt, commitSHA, projectGitRoot string) error {
	for _, rec := range receipts {
		latest, err := e.Store.Get(ctx, project, taskID, rec.ID)
		if err != nil {
			continue
		}
		if latest.Status != StatusCommitting {
			continue
		}
		_, _ = e.Store.Transition(ctx, project, taskID, latest.ID, latest.Revision, StatusCommitted, func(r *PreviewReceipt) error {
			r.CommittedSHA = commitSHA
			return nil
		})
		if projectGitRoot != "" {
			_ = os.RemoveAll(TurnSnapshotDir(projectGitRoot, taskID, latest.TurnID))
		}
	}
	return nil
}

// AbortCommitReceipts transitions all COMMITTING receipts to REVERT_FAILED (requiring manual inspection)
// with the failure reason.
func (e *TurnEngine) AbortCommitReceipts(ctx context.Context, project, taskID string, receipts []*PreviewReceipt, reason string) error {
	for _, rec := range receipts {
		latest, err := e.Store.Get(ctx, project, taskID, rec.ID)
		if err != nil {
			continue
		}
		if latest.Status != StatusCommitting {
			continue
		}
		_, _ = e.Store.Transition(ctx, project, taskID, latest.ID, latest.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
			r.FailureReason = CapAndRedact("review commit failed: "+reason, MaxFailureReasonBytes)
			return nil
		})
	}
	return nil
}

type StaleRecoveryResult struct {
	RecoveredFailedCount       int
	RecoveredRevertFailedCount int
	RecoveredCommittedCount    int
}

// RecoverStaleReceipts sweeps through receipts for a task and cleans up any stale or orphaned
// states after server restart.
func (e *TurnEngine) RecoverStaleReceipts(ctx context.Context, project, taskID, worktreeDir, projectGitRoot string) (StaleRecoveryResult, error) {
	var res StaleRecoveryResult
	receipts, err := e.Store.List(ctx, project, taskID)
	if err != nil {
		return res, err
	}

	for _, rec := range receipts {
		switch rec.Status {
		case StatusPending, StatusExecuting, StatusCapturing:
			_, _ = e.Store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusFailed, func(r *PreviewReceipt) error {
				r.FailureReason = "server restarted during turn execution; recovered stale turn"
				return nil
			})
			if worktreeDir != "" {
				_ = exec.Command("git", "-C", worktreeDir, "update-ref", "-d", "refs/mg-turns/"+rec.TurnID).Run()
				_ = os.Remove(filepath.Join(worktreeDir, ".git", "index.mg-turn-"+rec.TurnID))
			}
			if projectGitRoot != "" {
				_ = os.RemoveAll(TurnSnapshotDir(projectGitRoot, taskID, rec.TurnID))
			}
			res.RecoveredFailedCount++

		case StatusReverting:
			_, _ = e.Store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
				r.FailureReason = "server restarted during rollback; manual inspection required"
				return nil
			})
			res.RecoveredRevertFailedCount++

		case StatusCommitting:
			// Check if worktree git HEAD has a commit ahead of baseline
			var headSHA string
			if worktreeDir != "" {
				out, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "HEAD").Output()
				if err == nil {
					headSHA = strings.TrimSpace(string(out))
				}
			}
			if headSHA != "" && headSHA != rec.BaselineCommit {
				_, _ = e.Store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusCommitted, func(r *PreviewReceipt) error {
					r.CommittedSHA = headSHA
					return nil
				})
				if projectGitRoot != "" {
					_ = os.RemoveAll(TurnSnapshotDir(projectGitRoot, taskID, rec.TurnID))
				}
				res.RecoveredCommittedCount++
			} else {
				_, _ = e.Store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusRevertFailed, func(r *PreviewReceipt) error {
					r.FailureReason = "server restarted during review commit; checkpoint commit not found in HEAD"
					return nil
				})
				res.RecoveredRevertFailedCount++
			}
		}
	}
	return res, nil
}

// CancelActiveTurn cancels the active slot-holding turn if it is in PENDING, EXECUTING, or CAPTURING.
func (e *TurnEngine) CancelActiveTurn(ctx context.Context, project, taskID, reason string) error {
	slot, rec, err := e.Store.GetActiveSlot(ctx, project, taskID)
	if err != nil || slot == nil || rec == nil {
		return nil
	}
	if rec.Status == StatusPending || rec.Status == StatusExecuting || rec.Status == StatusCapturing {
		redactedReason := CapAndRedact("cancelled by operator: "+reason, MaxFailureReasonBytes)
		_, err := e.Store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusFailed, func(r *PreviewReceipt) error {
			r.FailureReason = redactedReason
			return nil
		})
		return err
	}
	return nil
}
