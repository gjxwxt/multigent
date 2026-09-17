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
