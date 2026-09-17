package previewreceipt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/secretbox"
)

func TestTurnEngineLifecycleAndCapture(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-1"
	taskID := "task-1"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		// Modify existing file
		newContent := "foo modified by agent\n"
		if err := os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte(newContent), 0644); err != nil {
			return err
		}
		// Create new file
		if err := os.WriteFile(filepath.Join(cloneDir, "extra.txt"), []byte("created by agent\n"), 0644); err != nil {
			return err
		}
		return nil
	})

	receipt, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "update foo.txt and add extra.txt",
		Actor:          "tester",
		Runner:         runner,
		LeaseDuration:  5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	if receipt.Status != StatusCaptured {
		t.Fatalf("expected receipt status %s, got %s", StatusCaptured, receipt.Status)
	}
	if !secretbox.IsSealed(receipt.OperationalPatch) {
		t.Fatalf("expected OperationalPatch to be strictly sealed, got: %s", receipt.OperationalPatch)
	}
	if len(receipt.TouchedPaths) != 2 {
		t.Fatalf("expected 2 touched paths, got %d: %v", len(receipt.TouchedPaths), receipt.TouchedPaths)
	}

	// Verify main worktree was updated
	fooContent, err := os.ReadFile(filepath.Join(repoDir, "foo.txt"))
	if err != nil {
		t.Fatalf("read foo.txt in worktree: %v", err)
	}
	if string(fooContent) != "foo modified by agent\n" {
		t.Fatalf("expected updated content in worktree foo.txt, got: %s", string(fooContent))
	}

	extraContent, err := os.ReadFile(filepath.Join(repoDir, "extra.txt"))
	if err != nil {
		t.Fatalf("read extra.txt in worktree: %v", err)
	}
	if strings.TrimSpace(string(extraContent)) != "created by agent" {
		t.Fatalf("unexpected extra.txt content: %s", string(extraContent))
	}

	// Verify slot was released upon CAPTURED
	slot, holding, err := store.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatalf("GetActiveSlot failed: %v", err)
	}
	if holding != nil {
		t.Fatalf("expected slot to be released upon CAPTURED, but holding receipt is %s", holding.ID)
	}
	if slot != nil && slot.ReceiptID != "" {
		t.Fatalf("expected empty slot receipt ID, got %s", slot.ReceiptID)
	}

	// Test surgical rollback
	err = engine.RollbackTurn(ctx, RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		TurnID:         receipt.ID,
		WorktreeDir:    repoDir,
		Actor:          "tester",
	})
	if err != nil {
		t.Fatalf("RollbackTurn failed: %v", err)
	}

	// Verify foo.txt restored to initial
	restoredFoo, _ := os.ReadFile(filepath.Join(repoDir, "foo.txt"))
	if string(restoredFoo) != "foo original\n" {
		t.Fatalf("expected foo.txt restored to initial, got: %s", string(restoredFoo))
	}

	// Verify extra.txt removed
	if _, err := os.Stat(filepath.Join(repoDir, "extra.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected extra.txt to be removed after rollback, got err: %v", err)
	}

	// Verify receipt status is ROLLED_BACK
	afterRollback, err := store.Get(ctx, project, taskID, receipt.ID)
	if err != nil {
		t.Fatalf("get receipt after rollback: %v", err)
	}
	if afterRollback.Status != StatusRolledBack {
		t.Fatalf("expected status %s, got %s", StatusRolledBack, afterRollback.Status)
	}
}

func TestTurnEngineRollbackConflictOnUserEdit(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-conflict"
	taskID := "task-conflict"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo modified by agent\n"), 0644)
	})

	receipt, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "modify foo",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	// Now user manually edits foo.txt in the worktree
	userEdit := "foo modified by user\n"
	if err := os.WriteFile(filepath.Join(repoDir, "foo.txt"), []byte(userEdit), 0644); err != nil {
		t.Fatalf("write user edit: %v", err)
	}

	// Rollback should detect postimage conflict and reject
	err = engine.RollbackTurn(ctx, RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		TurnID:         receipt.ID,
		WorktreeDir:    repoDir,
		Actor:          "tester",
	})
	if err == nil {
		t.Fatal("expected RollbackTurn to fail due to postimage mismatch, but it succeeded")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}

	// Verify user's file was NOT altered
	curContent, _ := os.ReadFile(filepath.Join(repoDir, "foo.txt"))
	if string(curContent) != userEdit {
		t.Fatalf("user modification was overwritten! got: %s", string(curContent))
	}
}

func TestTurnEngineAgentFailureMarksFailed(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-fail"
	taskID := "task-fail"

	failingRunner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return errors.New("command exited with code 1")
	})

	_, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "fail me",
		Actor:          "tester",
		Runner:         failingRunner,
	})
	if err == nil {
		t.Fatal("expected ExecuteTurn to fail, but got nil")
	}

	// Receipt should be recorded as FAILED
	receipts, err := store.List(ctx, project, taskID)
	if err != nil {
		t.Fatalf("List receipts failed: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 receipt recorded, got %d", len(receipts))
	}
	if receipts[0].Status != StatusFailed {
		t.Fatalf("expected status %s, got %s", StatusFailed, receipts[0].Status)
	}
	if !strings.Contains(receipts[0].FailureReason, "command exited with code 1") {
		t.Fatalf("unexpected error reason: %s", receipts[0].FailureReason)
	}

	// Main worktree must be untouched
	content, _ := os.ReadFile(filepath.Join(repoDir, "foo.txt"))
	if string(content) != "foo original\n" {
		t.Fatalf("foo.txt was touched on failure: %s", string(content))
	}
}

func TestTurnEngineWorktreeDriftRejection(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-drift"
	taskID := "task-drift"

	driftingRunner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		// Agent makes valid changes in clone
		if err := os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo clone\n"), 0644); err != nil {
			return err
		}
		// Simulate concurrent user/third-party drift in main worktree while agent was working
		if err := os.WriteFile(filepath.Join(repoDir, "drift.txt"), []byte("drift\n"), 0644); err != nil {
			return err
		}
		return nil
	})

	_, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "drift test",
		Actor:          "tester",
		Runner:         driftingRunner,
	})
	if err == nil {
		t.Fatal("expected ExecuteTurn to reject drifted worktree, but got nil")
	}
	if !strings.Contains(err.Error(), "worktree drifted") {
		t.Fatalf("expected worktree drifted error, got: %v", err)
	}

	// Receipt should be marked FAILED
	receipts, _ := store.List(ctx, project, taskID)
	if len(receipts) == 0 || receipts[0].Status != StatusFailed {
		t.Fatalf("expected FAILED receipt for drifted run, got: %v", receipts)
	}

	// The drift.txt should be preserved!
	if _, err := os.Stat(filepath.Join(repoDir, "drift.txt")); err != nil {
		t.Fatalf("drift.txt was wiped: %v", err)
	}
}

func TestTurnEngineSensitiveSinksRedactedAndDigest(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-redact"
	taskID := "task-redact"

	rawPrompt := "Use secret token ghp_123456789012345678901234567890123456 to fix foo.txt"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo with secret fixed\n"), 0644)
	})

	receipt, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         rawPrompt,
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	// 1. Verify RedactedPrompt has no plaintext token
	if strings.Contains(receipt.RedactedPrompt, "ghp_") {
		t.Fatalf("RedactedPrompt leaked secret token: %s", receipt.RedactedPrompt)
	}
	if !strings.Contains(receipt.RedactedPrompt, "[REDACTED_SECRET]") {
		t.Fatalf("expected [REDACTED_SECRET] in RedactedPrompt, got: %s", receipt.RedactedPrompt)
	}

	// 2. Verify RequestDigest matches SHA-256 of raw prompt
	expectedDigest := ComputeRequestDigest(rawPrompt)
	if receipt.RequestDigest != expectedDigest {
		t.Fatalf("expected RequestDigest %s, got %s", expectedDigest, receipt.RequestDigest)
	}

	// 3. Verify Turn Snapshot directory was created and contains no .git
	snapshotDir := TurnSnapshotDir(repoDir, taskID, receipt.TurnID)
	if fi, err := os.Stat(snapshotDir); err != nil || !fi.IsDir() {
		t.Fatalf("expected snapshot dir to exist, err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory leaked .git directory: %s", filepath.Join(snapshotDir, ".git"))
	}
	snapshotFoo, err := os.ReadFile(filepath.Join(snapshotDir, "foo.txt"))
	if err != nil || string(snapshotFoo) != "foo with secret fixed\n" {
		t.Fatalf("snapshot foo.txt content mismatch: %v (%s)", err, string(snapshotFoo))
	}

	// 4. Test failure error redaction
	failingRunner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return errors.New("failed with secret key: sk-ant-api03-abcdef1234567890abcdef1234567890-test")
	})
	_, failErr := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         "task-redact-fail",
		WorktreeDir:    repoDir,
		Prompt:         "fail run",
		Actor:          "tester",
		Runner:         failingRunner,
	})
	if failErr == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(failErr.Error(), "sk-ant-") {
		t.Fatalf("returned error leaked secret: %v", failErr)
	}
	failReceipts, _ := store.List(ctx, project, "task-redact-fail")
	if len(failReceipts) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(failReceipts))
	}
	if strings.Contains(failReceipts[0].FailureReason, "sk-ant-") {
		t.Fatalf("persisted FailureReason leaked secret: %s", failReceipts[0].FailureReason)
	}
}

func TestTurnEngineRollbackMismatchLeavesCapturedState(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-mismatch"
	taskID := "task-mismatch"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo modified\n"), 0644)
	})

	receipt, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "modify foo",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	// External modification
	_ = os.WriteFile(filepath.Join(repoDir, "foo.txt"), []byte("foo external edit\n"), 0644)

	err = engine.RollbackTurn(ctx, RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		TurnID:         receipt.ID,
		WorktreeDir:    repoDir,
		Actor:          "tester",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}

	// Invariant: Receipt MUST NOT be stuck in REVERTING; it must remain CAPTURED
	currentReceipt, err := store.Get(ctx, project, taskID, receipt.ID)
	if err != nil {
		t.Fatalf("Get receipt failed: %v", err)
	}
	if currentReceipt.Status != StatusCaptured {
		t.Fatalf("expected receipt status %s after rejected rollback, got %s", StatusCaptured, currentReceipt.Status)
	}
}

func TestTurnEngineRollbackReverseApplyFailureTransitionsToRevertFailed(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-revfail"
	taskID := "task-revfail"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo modified\n"), 0644)
	})

	receipt, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "modify foo",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	// Tamper with the sealed patch so that OpenBytesStrict succeeds but git apply --reverse fails
	corruptedPatch := `--- a/foo.txt
+++ b/foo.txt
@@ -1,1 +1,1 @@
-nonexistent line that fails reverse apply
+different line
`
	sealedCorrupt, err := secretbox.SealBytesStrict([]byte(corruptedPatch))
	if err != nil {
		t.Fatal(err)
	}

	// Update the stored patch in store
	_, err = store.Transition(ctx, project, taskID, receipt.ID, receipt.Revision, StatusCaptured, func(r *PreviewReceipt) error {
		r.OperationalPatch = sealedCorrupt
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Now execute RollbackTurn
	err = engine.RollbackTurn(ctx, RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		TurnID:         receipt.ID,
		WorktreeDir:    repoDir,
		Actor:          "tester",
	})
	if err == nil {
		t.Fatal("expected RollbackTurn to fail during reverse apply")
	}

	// Verify state transitioned to REVERT_FAILED
	failedReceipt, err := store.Get(ctx, project, taskID, receipt.ID)
	if err != nil {
		t.Fatalf("Get receipt failed: %v", err)
	}
	if failedReceipt.Status != StatusRevertFailed {
		t.Fatalf("expected status %s, got %s", StatusRevertFailed, failedReceipt.Status)
	}
	if !strings.Contains(failedReceipt.FailureReason, "reverse apply failed") {
		t.Fatalf("expected failure reason to mention reverse apply failed, got: %s", failedReceipt.FailureReason)
	}

	// Verify that REVERT_FAILED holds the slot to prevent new turns on dirty state
	if !IsActiveHolder(StatusRevertFailed) {
		t.Fatal("expected StatusRevertFailed to be an active slot holder")
	}

	// Verify that new turn creation is blocked by the occupied slot
	_, err = engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "another turn",
		Actor:          "tester",
		Runner:         runner,
	})
	if err == nil || !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("expected ErrSlotOccupied while in REVERT_FAILED, got: %v", err)
	}
}

func TestStageAllIn_NeutralizesArbitraryFilterDrivers(t *testing.T) {
	td := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = td
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %v (%s)", strings.Join(args, " "), err, string(out))
		}
	}

	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")

	canaryFile := filepath.Join(td, "pwned_filter_canary.txt")
	evilScript := filepath.Join(td, "evil_filter.sh")
	scriptContent := fmt.Sprintf("#!/bin/sh\necho PWNED > %q\n", canaryFile)
	if err := os.WriteFile(evilScript, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	// Untrusted clone: .gitattributes sets a custom malicious filter driver
	gitattributes := filepath.Join(td, ".gitattributes")
	if err := os.WriteFile(gitattributes, []byte("*.txt filter=evil_driver\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Agent pollutes .git/config with custom clean and process commands pointing to evilScript
	gitConfig := filepath.Join(td, ".git", "config")
	filterConfig := fmt.Sprintf("\n[filter \"evil_driver\"]\n\tclean = %s\n\tprocess = %s\n\tsmudge = %s\n", evilScript, evilScript, evilScript)
	f, err := os.OpenFile(gitConfig, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(filterConfig); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// Write a file matching the filter pattern
	if err := os.WriteFile(filepath.Join(td, "file.txt"), []byte("payload content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Execute stageAllIn
	ctx := context.Background()
	if err := stageAllIn(ctx, td); err != nil {
		t.Fatalf("stageAllIn failed: %v", err)
	}

	// Invariant: Malicious filter script MUST NEVER execute on host; canary file must not exist!
	if _, err := os.Stat(canaryFile); !os.IsNotExist(err) {
		t.Fatalf("SECURITY VULNERABILITY: malicious filter driver executed on host! Canary file %s exists", canaryFile)
	}

	// Invariant: file must be properly staged in Git index
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = td
	out, err := statusCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v (%s)", err, string(out))
	}
	if !strings.Contains(string(out), "A  file.txt") {
		t.Fatalf("expected file.txt to be staged (A  file.txt), got: %s", string(out))
	}
}

func TestTurnEngineRollbackFinalCASFailureTransitionsToRevertFailed(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-casfail"
	taskID := "task-casfail"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo modified for cas fail\n"), 0644)
	})

	receipt, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "modify foo for cas fail",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	// Verify file was modified
	data, _ := os.ReadFile(filepath.Join(repoDir, "foo.txt"))
	if string(data) != "foo modified for cas fail\n" {
		t.Fatalf("expected worktree to have turn modification, got: %s", string(data))
	}

	// Fault injection: right after reverseApply succeeds, advance revision in store so step 5 (CAS to ROLLED_BACK) conflicts
	engine.onAfterReverseApply = func(r *PreviewReceipt) {
		// Advance revision by transitioning to StatusReverting again
		_, _ = store.Transition(ctx, project, taskID, r.ID, r.Revision, StatusReverting, nil)
	}

	err = engine.RollbackTurn(ctx, RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		TurnID:         receipt.ID,
		WorktreeDir:    repoDir,
		Actor:          "tester",
	})
	if err == nil {
		t.Fatal("expected RollbackTurn to fail when final CAS fails")
	}

	// Invariant 1: reverse apply succeeded on disk (worktree restored to original)
	restored, _ := os.ReadFile(filepath.Join(repoDir, "foo.txt"))
	if string(restored) != "foo original\n" {
		t.Fatalf("expected worktree to be reverse-applied on disk, got: %s", string(restored))
	}

	// Invariant 2: receipt state MUST transition to StatusRevertFailed (not left in REVERTING)
	updatedReceipt, err := store.Get(ctx, project, taskID, receipt.ID)
	if err != nil {
		t.Fatalf("Get receipt failed: %v", err)
	}
	if updatedReceipt.Status != StatusRevertFailed {
		t.Fatalf("expected status %s after final CAS failure, got: %s", StatusRevertFailed, updatedReceipt.Status)
	}

	// Invariant 3: FailureReason must explain manual inspection required
	if !strings.Contains(updatedReceipt.FailureReason, "manual inspection required") {
		t.Fatalf("expected failure reason to mention manual inspection required, got: %s", updatedReceipt.FailureReason)
	}

	// Invariant 4: Task slot must remain held
	if !IsActiveHolder(StatusRevertFailed) {
		t.Fatal("expected StatusRevertFailed to be an active slot holder")
	}
	_, err = engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "attempt turn while revert failed",
		Actor:          "tester",
		Runner:         runner,
	})
	if err == nil || !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("expected ErrSlotOccupied while in REVERT_FAILED, got: %v", err)
	}
}

func TestTurnEngine_SupersedesOlderOverlappingCapturedTurn(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-supersede"
	taskID := "task-supersede"

	// Turn 1 modifies foo.txt
	runner1 := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo turn 1\n"), 0644)
	})

	turn1, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn 1",
		Actor:          "tester",
		Runner:         runner1,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn 1 failed: %v", err)
	}
	if turn1.Status != StatusCaptured {
		t.Fatalf("expected Turn 1 to be CAPTURED, got %s", turn1.Status)
	}

	snap1 := TurnSnapshotDir(repoDir, taskID, turn1.TurnID)
	if _, err := os.Stat(snap1); err != nil {
		t.Fatalf("expected Turn 1 snapshot to exist: %v", err)
	}

	// Turn 2 also modifies foo.txt (overlapping path)
	runner2 := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo turn 2\n"), 0644)
	})

	turn2, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn 2",
		Actor:          "tester",
		Runner:         runner2,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn 2 failed: %v", err)
	}
	if turn2.Status != StatusCaptured {
		t.Fatalf("expected Turn 2 to be CAPTURED, got %s", turn2.Status)
	}

	// Invariant: Turn 1 must now be SUPERSEDED
	updatedTurn1, err := store.Get(ctx, project, taskID, turn1.ID)
	if err != nil {
		t.Fatalf("Get Turn 1 failed: %v", err)
	}
	if updatedTurn1.Status != StatusSuperseded {
		t.Fatalf("expected Turn 1 status to be %s, got %s", StatusSuperseded, updatedTurn1.Status)
	}

	// Invariant: Turn 1 snapshot directory should be cleaned up
	if _, err := os.Stat(snap1); !os.IsNotExist(err) {
		t.Fatalf("expected Turn 1 snapshot to be removed after being superseded, but stat gave: %v", err)
	}

	// Turn 2 snapshot should still exist
	snap2 := TurnSnapshotDir(repoDir, taskID, turn2.TurnID)
	if _, err := os.Stat(snap2); err != nil {
		t.Fatalf("expected Turn 2 snapshot to exist: %v", err)
	}
}

func TestTurnEngine_CommitReceiptsLifecycle(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-commit"
	taskID := "task-commit"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("foo committed\n"), 0644)
	})

	turn, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn to commit",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	snapDir := TurnSnapshotDir(repoDir, taskID, turn.TurnID)
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("snapshot should exist: %v", err)
	}

	// 1. Prepare commit
	intentID := "intent-12345"
	preSHA := "0123456789012345678901234567890123456789"
	committing, err := engine.PrepareCommitReceipts(ctx, PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		CommitIntentID: intentID,
		PreCommitSHA:   preSHA,
	})
	if err != nil {
		t.Fatalf("PrepareCommitReceipts failed: %v", err)
	}
	if len(committing) != 1 {
		t.Fatalf("expected 1 committing receipt, got %d", len(committing))
	}
	if committing[0].Status != StatusCommitting || committing[0].CommitIntentID != intentID {
		t.Fatalf("unexpected committing state: %+v", committing[0])
	}

	// Invariant: Rollback during COMMITTING must be rejected
	err = engine.RollbackTurn(ctx, RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		TurnID:         turn.ID,
		WorktreeDir:    repoDir,
		Actor:          "tester",
	})
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict when rolling back during COMMITTING, got: %v", err)
	}

	// 2. Finalize commit
	fakeCommitSHA := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	err = engine.FinalizeCommitReceipts(ctx, project, taskID, committing, fakeCommitSHA, repoDir)
	if err != nil {
		t.Fatalf("FinalizeCommitReceipts failed: %v", err)
	}

	committed, err := store.Get(ctx, project, taskID, turn.ID)
	if err != nil {
		t.Fatalf("Get receipt failed: %v", err)
	}
	if committed.Status != StatusCommitted || committed.CommittedSHA != fakeCommitSHA {
		t.Fatalf("expected COMMITTED with SHA %s, got: %+v", fakeCommitSHA, committed)
	}

	// Invariant: snapshot directory is cleaned up
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir should be cleaned up on commit, but stat: %v", err)
	}
}

func TestTurnEngine_PrepareCommitRejectsActiveMutatingTurn(t *testing.T) {
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-mutating"
	taskID := "task-mutating"

	// Create a receipt in EXECUTING state
	rec, err := store.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-active",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	_, err = store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition to executing: %v", err)
	}

	// PrepareCommitReceipts must fail with ErrConflict
	_, err = engine.PrepareCommitReceipts(ctx, PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: "/tmp/fake",
		TaskID:         taskID,
		CommitIntentID: "intent-1",
		PreCommitSHA:   "pre-sha-fake",
	})
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict when a turn is actively executing, got: %v", err)
	}
}

func TestTurnEngine_RecoverStaleReceipts(t *testing.T) {
	store := setupTestStore(t)
	engine := NewTurnEngine(store)
	tmpDir := t.TempDir()

	ctx := context.Background()
	project := "proj-recover"
	taskID := "task-recover"

	// 1. Create stale EXECUTING receipt
	rExec, err := store.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-stale-exec",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create rExec: %v", err)
	}
	_, _ = store.Transition(ctx, project, taskID, rExec.ID, rExec.Revision, StatusExecuting, nil)

	// Create snapshot dir for it
	snapDir := TurnSnapshotDir(tmpDir, taskID, rExec.TurnID)
	_ = os.MkdirAll(snapDir, 0755)

	res, err := engine.RecoverStaleReceipts(ctx, project, taskID, "", tmpDir)
	if err != nil {
		t.Fatalf("RecoverStaleReceipts failed: %v", err)
	}
	if res.RecoveredFailedCount != 1 {
		t.Fatalf("expected 1 recovered failed, got %d", res.RecoveredFailedCount)
	}

	updated, err := store.Get(ctx, project, taskID, rExec.ID)
	if err != nil {
		t.Fatalf("get updated: %v", err)
	}
	if updated.Status != StatusFailed || !strings.Contains(updated.FailureReason, "server restarted") {
		t.Fatalf("expected FAILED status with restart reason, got: %+v", updated)
	}
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir should be removed on stale recovery: %v", err)
	}
}

func TestTurnEngine_CancelActiveTurn(t *testing.T) {
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-cancel"
	taskID := "task-cancel"

	rec, err := store.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-cancel",
		BaselineTree:   "tree1",
		BaselineCommit: "commit1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = store.Transition(ctx, project, taskID, rec.ID, rec.Revision, StatusExecuting, nil)

	err = engine.CancelActiveTurn(ctx, project, taskID, "operator aborted from UI")
	if err != nil {
		t.Fatalf("CancelActiveTurn failed: %v", err)
	}

	updated, err := store.Get(ctx, project, taskID, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status != StatusFailed || !strings.Contains(updated.FailureReason, "operator aborted from UI") {
		t.Fatalf("expected StatusFailed with cancel reason, got: %+v", updated)
	}

	// Slot must be free
	slot, holder, err := store.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatalf("GetActiveSlot failed: %v", err)
	}
	if holder != nil || (slot != nil && slot.ReceiptID != "") {
		t.Fatalf("expected slot to be released after cancellation, got slot: %+v, holder: %+v", slot, holder)
	}
}

func TestTurnEngine_RecoverStale_CrashBeforeCommit_RevertsToCaptured(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-crash-pre"
	taskID := "task-crash-pre"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("modified for pre-crash\n"), 0644)
	})

	turn, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn pre crash",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	snapDir := TurnSnapshotDir(repoDir, taskID, turn.TurnID)
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("snapshot should exist before crash: %v", err)
	}

	headBytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(headBytes))

	// Prepare commit in single atomic DB transaction
	intentID := "intent-pre-crash-123"
	committing, err := engine.PrepareCommitReceipts(ctx, PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		CommitIntentID: intentID,
		PreCommitSHA:   baseSHA,
	})
	if err != nil {
		t.Fatalf("PrepareCommitReceipts failed: %v", err)
	}
	if len(committing) != 1 || committing[0].Status != StatusCommitting {
		t.Fatalf("expected 1 committing receipt: %+v", committing)
	}

	// Simulating crash: git commit was NEVER executed. HEAD is still baseSHA.
	// Server restarts and runs RecoverStaleReceipts:
	res, err := engine.RecoverStaleReceipts(ctx, project, taskID, repoDir, repoDir)
	if err != nil {
		t.Fatalf("RecoverStaleReceipts failed: %v", err)
	}
	if res.RecoveredCapturedCount != 1 {
		t.Fatalf("expected RecoveredCapturedCount == 1, got %+v", res)
	}

	// Invariant: Receipt reverted back to CAPTURED
	updated, err := store.Get(ctx, project, taskID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusCaptured {
		t.Fatalf("expected status CAPTURED after pre-commit crash recovery, got %s", updated.Status)
	}
	if updated.CommitIntentID != "" || updated.PreCommitSHA != "" {
		t.Fatalf("expected CommitIntentID and PreCommitSHA to be cleared, got intent=%s pre=%s", updated.CommitIntentID, updated.PreCommitSHA)
	}

	// Invariant: Snapshot directory is PRESERVED so review changes are not lost
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("expected snapshot dir to remain intact after pre-commit crash, but stat gave: %v", err)
	}

	// Invariant: Task slot is released
	slot, holder, err := store.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if holder != nil || (slot != nil && slot.ReceiptID != "") {
		t.Fatalf("expected slot to be released, got: %+v, holder: %+v", slot, holder)
	}
}

func TestTurnEngine_RecoverStale_CrashAfterCommitBeforeFinalize_FinalizesToCommitted(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-crash-post"
	taskID := "task-crash-post"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("modified for post-crash\n"), 0644)
	})

	turn, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn post crash",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	snapDir := TurnSnapshotDir(repoDir, taskID, turn.TurnID)
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("snapshot should exist before crash: %v", err)
	}

	headBytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(headBytes))

	// 1. Prepare commit
	intentID := "intent-post-crash-456"
	committing, err := engine.PrepareCommitReceipts(ctx, PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		CommitIntentID: intentID,
		PreCommitSHA:   baseSHA,
	})
	if err != nil {
		t.Fatalf("PrepareCommitReceipts failed: %v", err)
	}
	if len(committing) != 1 {
		t.Fatalf("expected 1 committing receipt")
	}

	// 2. git commit SUCCEEDS with trailer
	commitMsg := fmt.Sprintf("chore(review): user in-context preview feedback fixes\n\n%s: %s", CommitIntentTrailerKey, intentID)
	_ = exec.Command("git", "-C", repoDir, "add", "-A").Run()
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", commitMsg).CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v (%s)", err, string(out))
	}
	committedHeadBytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	checkpointSHA := strings.TrimSpace(string(committedHeadBytes))

	// 3. Simulating crash before FinalizeCommitReceipts!
	// Server restarts and runs RecoverStaleReceipts:
	res, err := engine.RecoverStaleReceipts(ctx, project, taskID, repoDir, repoDir)
	if err != nil {
		t.Fatalf("RecoverStaleReceipts failed: %v", err)
	}
	if res.RecoveredCommittedCount != 1 {
		t.Fatalf("expected RecoveredCommittedCount == 1, got %+v", res)
	}

	// Invariant: Receipt transitioned to COMMITTED with exact commit SHA
	updated, err := store.Get(ctx, project, taskID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusCommitted || updated.CommittedSHA != checkpointSHA {
		t.Fatalf("expected COMMITTED with SHA %s, got status=%s, sha=%s", checkpointSHA, updated.Status, updated.CommittedSHA)
	}

	// Invariant: Snapshot directory is cleaned up
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot dir to be cleaned up, but stat gave: %v", err)
	}

	// Invariant: Slot is released
	slot, holder, err := store.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if holder != nil || (slot != nil && slot.ReceiptID != "") {
		t.Fatalf("expected slot to be released, got: %+v, holder: %+v", slot, holder)
	}
}

func TestTurnEngine_RecoverStale_UnrelatedHeadChange_TransitionsToRevertFailed(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-crash-unrelated"
	taskID := "task-crash-unrelated"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("modified for unrelated\n"), 0644)
	})

	turn, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn unrelated",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	snapDir := TurnSnapshotDir(repoDir, taskID, turn.TurnID)

	headBytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(headBytes))

	// 1. Prepare commit
	intentID := "intent-unrelated-789"
	_, err = engine.PrepareCommitReceipts(ctx, PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		CommitIntentID: intentID,
		PreCommitSHA:   baseSHA,
	})
	if err != nil {
		t.Fatalf("PrepareCommitReceipts failed: %v", err)
	}

	// 2. An unrelated commit lands without the intent trailer!
	_ = os.WriteFile(filepath.Join(repoDir, "other.txt"), []byte("unrelated change\n"), 0644)
	_ = exec.Command("git", "-C", repoDir, "add", "-A").Run()
	_ = exec.Command("git", "-C", repoDir, "commit", "-m", "unrelated commit without trailer").Run()

	// 3. Server restarts and runs RecoverStaleReceipts:
	res, err := engine.RecoverStaleReceipts(ctx, project, taskID, repoDir, repoDir)
	if err != nil {
		t.Fatalf("RecoverStaleReceipts failed: %v", err)
	}
	if res.RecoveredRevertFailedCount != 1 {
		t.Fatalf("expected RecoveredRevertFailedCount == 1, got %+v", res)
	}

	// Invariant: Receipt transitioned to REVERT_FAILED (manual inspection required)
	updated, err := store.Get(ctx, project, taskID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusRevertFailed {
		t.Fatalf("expected REVERT_FAILED status, got: %s", updated.Status)
	}
	if !strings.Contains(updated.FailureReason, "without commit intent trailer") {
		t.Fatalf("expected failure reason mentioning missing intent trailer, got: %s", updated.FailureReason)
	}

	// Invariant: Snapshot directory is preserved for operator inspection
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("snapshot dir should be preserved when unattributable: %v", err)
	}
}

func TestTurnEngine_FinalizeFailureDoesNotDeleteSnapshot(t *testing.T) {
	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")
	repoDir := setupTestGitRepo(t)
	store := setupTestStore(t)
	engine := NewTurnEngine(store)

	ctx := context.Background()
	project := "proj-finalize-fail"
	taskID := "task-finalize-fail"

	runner := AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		return os.WriteFile(filepath.Join(cloneDir, "foo.txt"), []byte("finalize fail test\n"), 0644)
	})

	turn, err := engine.ExecuteTurn(ctx, ExecuteTurnParams{
		WorkspaceID:    "ws-test-engine",
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		WorktreeDir:    repoDir,
		Prompt:         "turn finalize fail",
		Actor:          "tester",
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}

	snapDir := TurnSnapshotDir(repoDir, taskID, turn.TurnID)

	headBytes, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(headBytes))

	committing, err := engine.PrepareCommitReceipts(ctx, PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: repoDir,
		TaskID:         taskID,
		CommitIntentID: "intent-fail",
		PreCommitSHA:   baseSHA,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Artificially clobber receipt revision in DB to simulate a CAS conflict during Finalize
	_, _ = store.Transition(ctx, project, taskID, committing[0].ID, committing[0].Revision, StatusRevertFailed, nil)

	// Now attempt FinalizeCommitReceipts with the old committing slice (which has stale revision)
	err = engine.FinalizeCommitReceipts(ctx, project, taskID, committing, "fake-sha", repoDir)
	if err == nil {
		t.Fatal("expected FinalizeCommitReceipts to return error on CAS failure, got nil")
	}

	// Invariant: Snapshot MUST NOT be deleted when Finalize fails!
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("snapshot was deleted despite Finalize transition failure: %v", err)
	}
}




