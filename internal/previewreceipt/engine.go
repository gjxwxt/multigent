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
	Store *Store
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
		RedactedPrompt: params.Prompt,
		Creator:        params.Actor,
		LeaseDuration:  lease,
	})
	if err != nil {
		return nil, fmt.Errorf("create receipt with slot: %w", err)
	}

	// Helper to fail receipt cleanly
	fail := func(reason string) (*PreviewReceipt, error) {
		failed, _ := e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusFailed, func(r *PreviewReceipt) error {
			r.FailureReason = reason
			return nil
		})
		return failed, fmt.Errorf("%s", reason)
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
		_ = reverseApplyPatchIn(params.WorktreeDir, rawDiff)
		return fail("capture postimages: " + err.Error())
	}

	// 15. CAS transition to CAPTURED
	captured, err := e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusCaptured, func(r *PreviewReceipt) error {
		r.OperationalPatch = sealedPatch
		r.DisplayDiff = displayDiff
		r.TouchedPaths = touched
		r.Postimages = postimages
		return nil
	})
	if err != nil {
		// Compensatory rollback on CAS failure
		_ = reverseApplyPatchIn(params.WorktreeDir, rawDiff)
		return fail("commit captured receipt state failed: " + err.Error())
	}

	return captured, nil
}

// RollbackTurn performs a surgical rollback of a captured turn:
// 1. Lock project worktree
// 2. Fetch receipt and verify state is CAPTURED
// 3. Verify all touched files still match postimage fingerprints byte-for-byte
// 4. Transition to REVERTING
// 5. Decrypt OperationalPatch and apply reverse patch
// 6. Transition to ROLLED_BACK
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

	// Verify all touched files still match postimage
	matches, err := VerifyWorktreeMatchesPostimages(params.WorktreeDir, receipt.Postimages)
	if err != nil || !matches {
		msg := "touched files have been modified or deleted since the turn was captured; rollback aborted to prevent data loss"
		if err != nil {
			msg = fmt.Sprintf("verify postimages failed: %v", err)
		}
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	}

	// Transition to REVERTING
	receipt, err = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusReverting, nil)
	if err != nil {
		return fmt.Errorf("transition to reverting: %w", err)
	}

	// Decrypt patch
	patchBytes, err := secretbox.OpenBytesStrict(receipt.OperationalPatch)
	if err != nil {
		// Restore status back to CAPTURED if decrypt fails
		_, _ = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusCaptured, nil)
		return fmt.Errorf("decrypt operational patch: %w", err)
	}

	// Apply reverse patch
	if err := reverseApplyPatchIn(params.WorktreeDir, string(patchBytes)); err != nil {
		// Restore status back to CAPTURED
		_, _ = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusCaptured, nil)
		return fmt.Errorf("reverse apply patch: %w", err)
	}

	// Transition to ROLLED_BACK
	_, err = e.Store.Transition(ctx, params.Project, params.TaskID, receipt.ID, receipt.Revision, StatusRolledBack, nil)
	if err != nil {
		return fmt.Errorf("transition to rolled_back: %w", err)
	}

	return nil
}

func stageAllIn(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "add", "--all")
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add --all: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

func generateDiffIn(ctx context.Context, dir, baselineCommit string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-color", "--no-ext-diff", baselineCommit)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return string(out), nil
}

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

func reverseApplyPatchIn(dir, patch string) error {
	cmd := exec.Command("git", "apply", "--reverse", "--whitespace=nowarn", "-")
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --reverse: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}

func checkPatchIn(dir, patch string) error {
	cmd := exec.Command("git", "apply", "--check", "--whitespace=nowarn", "-")
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	cmd.Stdin = strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --check: %w (%s)", err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
	}
	return nil
}
