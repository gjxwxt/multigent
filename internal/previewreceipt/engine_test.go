package previewreceipt

import (
	"context"
	"errors"
	"os"
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


