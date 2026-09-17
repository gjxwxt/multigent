package previewreceipt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

func TestValidateTransition(t *testing.T) {
	// 1. Legal transitions
	legalCases := [][2]string{
		{StatusPending, StatusExecuting},
		{StatusPending, StatusFailed},
		{StatusExecuting, StatusCapturing},
		{StatusExecuting, StatusFailed},
		{StatusCapturing, StatusCaptured},
		{StatusCapturing, StatusFailed},
		{StatusCaptured, StatusReverting},
		{StatusCaptured, StatusSuperseded},
		{StatusCaptured, StatusCommitting},
		{StatusCaptured, StatusFailed},
		{StatusReverting, StatusRolledBack},
		{StatusReverting, StatusRevertFailed},
		{StatusCommitting, StatusCommitted},
		{StatusCommitting, StatusRevertFailed},
		{StatusRevertFailed, StatusFailed},
	}

	for _, tc := range legalCases {
		if err := ValidateTransition(tc[0], tc[1]); err != nil {
			t.Errorf("expected transition %s -> %s to be valid, got err: %v", tc[0], tc[1], err)
		}
	}

	// 2. Illegal transitions
	illegalCases := [][2]string{
		{StatusPending, StatusCaptured},
		{StatusPending, StatusRolledBack},
		{StatusExecuting, StatusCaptured},
		{StatusCapturing, StatusReverting},
		// Strict contract: COMMITTING -> CAPTURED is forbidden in general state machine
		{StatusCommitting, StatusCaptured},
		{StatusRolledBack, StatusPending},
		{StatusSuperseded, StatusCaptured},
		{StatusCommitted, StatusReverting},
		{StatusFailed, StatusPending},
	}

	for _, tc := range illegalCases {
		if err := ValidateTransition(tc[0], tc[1]); err == nil {
			t.Errorf("expected transition %s -> %s to be invalid, got nil err", tc[0], tc[1])
		}
	}
}

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.UpsertWorkspace(controldb.Workspace{
		ID:        "ws-test",
		Name:      "Test WS",
		CreatedAt: "2026-09-17T00:00:00Z",
		UpdatedAt: "2026-09-17T00:00:00Z",
	}); err != nil {
		t.Fatalf("upsert workspace: %v", err)
	}

	return NewStore(db, "ws-test")
}

func TestStoreLifecycleAndCAS(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	project := "proj-1"
	taskID := "task-100"

	// 1. Create Receipt
	r, err := s.Create(ctx, CreateParams{
		Project:          project,
		TaskID:           taskID,
		TurnID:           "turn-1",
		BaselineTree:     "0123456789012345678901234567890123456789",
		BaselineCommit:   "1123456789012345678901234567890123456789",
		BaselineRef:      "refs/mg-turns/turn-1",
		RequestDigest:    "req-sha-1",
		RedactedPrompt:   "Add test button",
		Creator:          "operator-alice",
		OperationalPatch: "sealed:encrypted-patch-content",
		DisplayDiff:      "--- a/b +++ b/b",
		LeaseDuration:    5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}

	if r.Status != StatusPending {
		t.Fatalf("expected status PENDING, got %s", r.Status)
	}
	if r.Revision != 1 {
		t.Fatalf("expected revision 1, got %d", r.Revision)
	}

	// Verify slot is occupied
	slot, holder, err := s.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatalf("get slot: %v", err)
	}
	if slot == nil || slot.ReceiptID != r.ID {
		t.Fatalf("expected slot to hold receipt %s, got %+v", r.ID, slot)
	}
	if holder == nil || holder.ID != r.ID {
		t.Fatalf("expected holder receipt %s, got %+v", r.ID, holder)
	}

	// 2. Second Create while active should fail with ErrSlotOccupied
	_, err = s.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-2",
		BaselineTree:   "0123456789012345678901234567890123456789",
		BaselineCommit: "1123456789012345678901234567890123456789",
		Creator:        "operator-bob",
	})
	if err == nil || !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("expected ErrSlotOccupied, got %v", err)
	}

	// 3. CAS Conflict check on Transition
	// Trying with wrong revision (e.g. 999)
	_, err = s.Transition(ctx, project, taskID, r.ID, 999, StatusExecuting, nil)
	if err == nil || !errors.Is(err, ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict with wrong revision, got %v", err)
	}

	// 4. Valid transition to EXECUTING
	r2, err := s.Transition(ctx, project, taskID, r.ID, 1, StatusExecuting, nil)
	if err != nil {
		t.Fatalf("transition to EXECUTING: %v", err)
	}
	if r2.Status != StatusExecuting || r2.Revision != 2 {
		t.Fatalf("expected EXECUTING rev 2, got %s rev %d", r2.Status, r2.Revision)
	}

	// 5. Transition through to CAPTURED
	r3, err := s.Transition(ctx, project, taskID, r.ID, 2, StatusCapturing, nil)
	if err != nil {
		t.Fatalf("transition to CAPTURING: %v", err)
	}
	r4, err := s.Transition(ctx, project, taskID, r.ID, r3.Revision, StatusCaptured, func(rec *PreviewReceipt) error {
		rec.TouchedPaths = []string{"src/app.ts"}
		rec.Postimages = []PostimageEntry{
			{Path: "src/app.ts", Exists: true, SHA256: "abc123sha"},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transition to CAPTURED: %v", err)
	}
	if r4.Status != StatusCaptured || len(r4.TouchedPaths) != 1 {
		t.Fatalf("unexpected CAPTURED state: %+v", r4)
	}

	// 6. Transition to REVERTING then ROLLED_BACK (terminal state)
	r5, err := s.Transition(ctx, project, taskID, r.ID, r4.Revision, StatusReverting, nil)
	if err != nil {
		t.Fatalf("transition to REVERTING: %v", err)
	}
	r6, err := s.Transition(ctx, project, taskID, r.ID, r5.Revision, StatusRolledBack, nil)
	if err != nil {
		t.Fatalf("transition to ROLLED_BACK: %v", err)
	}
	if r6.Status != StatusRolledBack {
		t.Fatalf("expected ROLLED_BACK, got %s", r6.Status)
	}

	// 7. Verify slot has been released after terminal status
	slotAfterTerminal, holderAfterTerminal, err := s.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatalf("get slot after terminal: %v", err)
	}
	if slotAfterTerminal != nil && slotAfterTerminal.ReceiptID != "" {
		t.Fatalf("expected slot to be released, got receiptId=%s", slotAfterTerminal.ReceiptID)
	}
	if holderAfterTerminal != nil {
		t.Fatalf("expected nil active holder, got %v", holderAfterTerminal)
	}

	// 8. Now a new receipt can occupy the slot
	newR, err := s.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-3",
		BaselineTree:   "0123456789012345678901234567890123456789",
		BaselineCommit: "1123456789012345678901234567890123456789",
		Creator:        "operator-alice",
	})
	if err != nil {
		t.Fatalf("create second receipt after release failed: %v", err)
	}
	if newR.ID == r.ID {
		t.Fatalf("expected new receipt ID, got same %s", newR.ID)
	}

	// 9. List receipts
	list, err := s.List(ctx, project, taskID)
	if err != nil {
		t.Fatalf("list receipts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 receipts, got %d", len(list))
	}
}

func TestStoreExpiredSlotTakeover(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	project := "proj-expire"
	taskID := "task-expire"

	// Create with a very short lease (10 milliseconds)
	shortLease := 10 * time.Millisecond
	r1, err := s.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-1",
		BaselineTree:   "0123456789012345678901234567890123456789",
		BaselineCommit: "1123456789012345678901234567890123456789",
		Creator:        "operator-alice",
		LeaseDuration:  shortLease,
	})
	if err != nil {
		t.Fatalf("create r1: %v", err)
	}

	// Wait for lease to expire
	time.Sleep(30 * time.Millisecond)

	// Create second receipt on same task
	r2, err := s.Create(ctx, CreateParams{
		Project:        project,
		TaskID:         taskID,
		TurnID:         "turn-2",
		BaselineTree:   "0123456789012345678901234567890123456789",
		BaselineCommit: "1123456789012345678901234567890123456789",
		Creator:        "operator-bob",
		LeaseDuration:  5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create r2 should have taken over expired slot, got: %v", err)
	}

	// Verify old receipt r1 was marked FAILED with reason "lease expired"
	r1Updated, err := s.Get(ctx, project, taskID, r1.ID)
	if err != nil {
		t.Fatalf("get r1: %v", err)
	}
	if r1Updated.Status != StatusFailed {
		t.Fatalf("expected r1 to be FAILED, got %s", r1Updated.Status)
	}
	if r1Updated.FailureReason != "lease expired" {
		t.Fatalf("expected failureReason 'lease expired', got %q", r1Updated.FailureReason)
	}

	// Verify slot now belongs to r2
	slot, holder, err := s.GetActiveSlot(ctx, project, taskID)
	if err != nil {
		t.Fatalf("get active slot: %v", err)
	}
	if slot.ReceiptID != r2.ID || holder.ID != r2.ID {
		t.Fatalf("expected slot to hold r2 %s, got slot.ReceiptID=%s", r2.ID, slot.ReceiptID)
	}
}
