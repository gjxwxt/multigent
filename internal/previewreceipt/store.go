package previewreceipt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

const (
	receiptTable = "preview_receipts"
	slotTable    = "preview_receipt_slots"

	DefaultLeaseDuration = 10 * time.Minute
)

func receiptKey(project, taskID, receiptID string) []string {
	return []string{"receipt", project + "\x00" + taskID, receiptID}
}

func receiptPrefix(project, taskID string) []string {
	return []string{"receipt", project + "\x00" + taskID}
}

func slotKey(project, taskID string) []string {
	return []string{"slot", project + "\x00" + taskID, ""}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Store provides persistence, CAS transitions, and single-active slot gating
// for preview receipts.
type Store struct {
	db        controldb.Store
	workspace string
}

func NewStore(db controldb.Store, workspace string) *Store {
	return &Store{
		db:        db,
		workspace: workspace,
	}
}

type CreateParams struct {
	Project          string
	TaskID           string
	TurnID           string
	BaselineTree     string
	BaselineCommit   string
	BaselineRef      string
	RequestDigest    string
	RedactedPrompt   string
	Creator          string
	OperationalPatch string
	DisplayDiff      string
	LeaseDuration    time.Duration
}

// Create persists a new receipt in PENDING state and occupies the task slot in
// one atomic BEGIN IMMEDIATE transaction. If an active, non-expired receipt already
// occupies the slot, ErrSlotOccupied is returned.
func (s *Store) Create(ctx context.Context, params CreateParams) (*PreviewReceipt, error) {
	if params.Project == "" || params.TaskID == "" || params.TurnID == "" {
		return nil, fmt.Errorf("project, taskID, and turnID are required")
	}
	if params.BaselineTree == "" || params.BaselineCommit == "" {
		return nil, fmt.Errorf("baselineTree and baselineCommit are required")
	}

	leaseDur := params.LeaseDuration
	if leaseDur <= 0 {
		leaseDur = DefaultLeaseDuration
	}

	now := time.Now().UTC()
	receiptID := fmt.Sprintf("rcpt_%d_%s", now.UnixNano(), randHex(4))

	r := &PreviewReceipt{
		ID:               receiptID,
		WorkspaceID:      s.workspace,
		Project:          params.Project,
		TaskID:           params.TaskID,
		TurnID:           params.TurnID,
		Status:           StatusPending,
		Revision:         1,
		BaselineTree:     params.BaselineTree,
		BaselineCommit:   params.BaselineCommit,
		BaselineRef:      params.BaselineRef,
		RequestDigest:    params.RequestDigest,
		RedactedPrompt:   params.RedactedPrompt,
		Creator:          params.Creator,
		CreatedAt:        now,
		UpdatedAt:        now,
		LeaseExpiresAt:   now.Add(leaseDur),
		OperationalPatch: params.OperationalPatch,
		DisplayDiff:      params.DisplayDiff,
	}

	err := s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		// 1. Read existing slot inside BEGIN IMMEDIATE lock
		slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(params.Project, params.TaskID))
		if err != nil {
			return nil, fmt.Errorf("read task slot: %w", err)
		}

		var writes []controldb.KVWrite

		if sFound && strings.TrimSpace(slotPayload) != "" {
			var slot TaskSlot
			if err := json.Unmarshal([]byte(slotPayload), &slot); err == nil {
				// Rule 1: A commit_intent group can NEVER be taken over by lease expiration
				if slot.IsCommitIntentGroup() {
					return nil, fmt.Errorf("%w: task slot is held by commit intent group %s (lease takeover forbidden)", ErrSlotOccupied, slot.CommitIntentID)
				}

				if slot.ReceiptID != "" {
					// Read current slot holder
					hPayload, _, hFound, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(params.Project, params.TaskID, slot.ReceiptID))
					if err != nil {
						return nil, fmt.Errorf("read slot holder receipt: %w", err)
					}
					if hFound && strings.TrimSpace(hPayload) != "" {
						var holderRec receiptRecord
						if err := json.Unmarshal([]byte(hPayload), &holderRec); err == nil {
							if IsActiveHolder(holderRec.Status) {
								// Rule 2: COMMITTING, REVERTING, and REVERT_FAILED can NEVER be lease taken over
								if holderRec.Status == StatusCommitting || holderRec.Status == StatusReverting || holderRec.Status == StatusRevertFailed {
									return nil, fmt.Errorf("%w: active receipt %s is in %s state (lease takeover forbidden)", ErrSlotOccupied, slot.ReceiptID, holderRec.Status)
								}

								// Active holder (PENDING/EXECUTING/CAPTURING). Check lease expiration:
								if now.Before(slot.LeaseExpiresAt) {
									return nil, fmt.Errorf("%w: active receipt %s holds slot until %s", ErrSlotOccupied, slot.ReceiptID, slot.LeaseExpiresAt.Format(time.RFC3339))
								}

								// Lease is expired! Take over in the SAME transaction:
								holderRec.Status = StatusFailed
								holderRec.FailureReason = "lease expired"
								holderRec.UpdatedAt = now
								hBytes, err := json.Marshal(holderRec)
								if err != nil {
									return nil, fmt.Errorf("marshal expired holder: %w", err)
								}
								writes = append(writes, controldb.KVWrite{
									Table:     receiptTable,
									Workspace: s.workspace,
									Key:       receiptKey(params.Project, params.TaskID, holderRec.ID),
									Payload:   string(hBytes),
								})
							}
						}
					}
				}
			}
		}

		// 2. Add write for the new receipt
		rec := receiptRecord{
			PreviewReceipt: *r,
			StoredPatch:    r.OperationalPatch,
		}
		recBytes, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("marshal new receipt: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     receiptTable,
			Workspace: s.workspace,
			Key:       receiptKey(params.Project, params.TaskID, r.ID),
			Payload:   string(recBytes),
		})

		// 3. Add write for the task slot
		newSlot := TaskSlot{
			HolderType:     HolderTypeTurn,
			ReceiptID:      r.ID,
			Project:        params.Project,
			TaskID:         params.TaskID,
			UpdatedAt:      now,
			LeaseExpiresAt: r.LeaseExpiresAt,
		}
		slotBytes, err := json.Marshal(newSlot)
		if err != nil {
			return nil, fmt.Errorf("marshal task slot: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     slotTable,
			Workspace: s.workspace,
			Key:       slotKey(params.Project, params.TaskID),
			Payload:   string(slotBytes),
		})

		return writes, nil
	})

	if err != nil {
		return nil, err
	}
	return r, nil
}

// Transition performs a CAS state transition on a receipt in a single guarded transaction.
// If targetStatus is a non-holding state (e.g. terminal or CAPTURED), and this receipt
// currently holds the task slot, the slot is updated/released in the same transaction.
// Dedicated group methods (FinalizeCommitIntentGroup, AbortCommitIntentGroup, RecoverCommitIntentGroup)
// MUST be used for commit_intent group members.
func (s *Store) Transition(ctx context.Context, project, taskID, receiptID string, expectedRev int64, targetStatus string, mutate func(r *PreviewReceipt) error) (*PreviewReceipt, error) {
	var result *PreviewReceipt

	err := s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		payload, rev, found, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(project, taskID, receiptID))
		if err != nil {
			return nil, fmt.Errorf("read receipt: %w", err)
		}
		if !found || strings.TrimSpace(payload) == "" {
			return nil, ErrReceiptNotFound
		}

		if rev != expectedRev {
			return nil, fmt.Errorf("%w: expected revision %d, current is %d", ErrCASConflict, expectedRev, rev)
		}

		var rec receiptRecord
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal receipt: %w", err)
		}

		current := rec.PreviewReceipt
		current.OperationalPatch = rec.StoredPatch
		current.Revision = rev

		// Check if task slot is held by a commit_intent group
		slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(project, taskID))
		var slot TaskSlot
		hasSlot := false
		if err == nil && sFound && strings.TrimSpace(slotPayload) != "" {
			if err := json.Unmarshal([]byte(slotPayload), &slot); err == nil {
				hasSlot = true
			}
		}

		if hasSlot && slot.IsCommitIntentGroup() && (slot.MatchesHolder(receiptID, current.CommitIntentID) || current.CommitIntentID != "") {
			return nil, fmt.Errorf("%w: receipt %s belongs to commit intent group %s; single-turn Transition forbidden", ErrInvalidState, receiptID, slot.CommitIntentID)
		}

		if err := ValidateTransition(current.Status, targetStatus); err != nil {
			return nil, err
		}

		if mutate != nil {
			if err := mutate(&current); err != nil {
				return nil, err
			}
		}

		now := time.Now().UTC()
		current.Status = targetStatus
		current.UpdatedAt = now
		current.Revision = rev + 1

		var writes []controldb.KVWrite

		rec.PreviewReceipt = current
		rec.StoredPatch = current.OperationalPatch
		recBytes, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("marshal updated receipt: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     receiptTable,
			Workspace: s.workspace,
			Key:       receiptKey(project, taskID, receiptID),
			Payload:   string(recBytes),
		})

		// If transitioning to a terminal state (or non-holding state), release slot if held by this receipt
		if !IsActiveHolder(targetStatus) {
			if hasSlot && slot.HolderType == HolderTypeTurn && slot.ReceiptID == receiptID {
				slot.HolderType = ""
				slot.ReceiptID = ""
				slot.UpdatedAt = now
				slotBytes, err := json.Marshal(slot)
				if err == nil {
					writes = append(writes, controldb.KVWrite{
						Table:     slotTable,
						Workspace: s.workspace,
						Key:       slotKey(project, taskID),
						Payload:   string(slotBytes),
					})
				}
			}
		} else {
			// Active holder state (PENDING, EXECUTING, CAPTURING, REVERTING, COMMITTING, REVERT_FAILED)
			if hasSlot {
				if slot.IsCommitIntentGroup() {
					return nil, fmt.Errorf("%w: task slot is held by commit intent group %s", ErrSlotOccupied, slot.CommitIntentID)
				}
				if slot.ReceiptID != "" && slot.ReceiptID != receiptID && now.Before(slot.LeaseExpiresAt) {
					return nil, fmt.Errorf("%w: active receipt %s holds slot until %s", ErrSlotOccupied, slot.ReceiptID, slot.LeaseExpiresAt.Format(time.RFC3339))
				}
			}
			slot.HolderType = HolderTypeTurn
			slot.ReceiptID = receiptID
			slot.CommitIntentID = ""
			slot.ReceiptIDs = nil
			slot.Project = project
			slot.TaskID = taskID
			slot.UpdatedAt = now
			if current.LeaseExpiresAt.After(now) {
				slot.LeaseExpiresAt = current.LeaseExpiresAt
			} else {
				slot.LeaseExpiresAt = now.Add(DefaultLeaseDuration)
			}
			slotBytes, err := json.Marshal(slot)
			if err == nil {
				writes = append(writes, controldb.KVWrite{
					Table:     slotTable,
					Workspace: s.workspace,
					Key:       slotKey(project, taskID),
					Payload:   string(slotBytes),
				})
			}
		}

		result = &current
		return writes, nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// Get loads a single receipt by ID.
func (s *Store) Get(ctx context.Context, project, taskID, receiptID string) (*PreviewReceipt, error) {
	recs, err := s.db.ListRecordsWithRevision(receiptTable, s.workspace, receiptKey(project, taskID, receiptID))
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		// Fallback: check if receiptID was specified as TurnID
		all, listErr := s.List(ctx, project, taskID)
		if listErr == nil {
			for _, item := range all {
				if item.TurnID == receiptID {
					return s.Get(ctx, project, taskID, item.ID)
				}
			}
		}
		return nil, ErrReceiptNotFound
	}

	var rec receiptRecord
	if err := json.Unmarshal([]byte(recs[0].Payload), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal receipt: %w", err)
	}

	rev, _ := strconv.ParseInt(recs[0].Revision, 10, 64)
	r := rec.PreviewReceipt
	r.OperationalPatch = rec.StoredPatch
	r.Revision = rev
	return &r, nil
}

// GetActiveSlot returns the active slot and its holding receipt (if active and not expired).
// For commit_intent groups, it returns a representative active receipt if any receipt is in
// COMMITTING or REVERT_FAILED.
func (s *Store) GetActiveSlot(ctx context.Context, project, taskID string) (*TaskSlot, *PreviewReceipt, error) {
	recs, err := s.db.ListRecords(slotTable, s.workspace, slotKey(project, taskID))
	if err != nil {
		return nil, nil, err
	}
	if len(recs) == 0 || strings.TrimSpace(recs[0].Payload) == "" {
		return nil, nil, nil
	}

	var slot TaskSlot
	if err := json.Unmarshal([]byte(recs[0].Payload), &slot); err != nil {
		return nil, nil, fmt.Errorf("unmarshal slot: %w", err)
	}

	if slot.IsCommitIntentGroup() {
		var representativeActive *PreviewReceipt
		for _, rid := range slot.ReceiptIDs {
			r, err := s.Get(ctx, project, taskID, rid)
			if err != nil {
				continue
			}
			if r.Status == StatusCommitting || r.Status == StatusRevertFailed {
				representativeActive = r
				break
			}
		}
		return &slot, representativeActive, nil
	}

	if slot.ReceiptID == "" {
		return &slot, nil, nil
	}

	r, err := s.Get(ctx, project, taskID, slot.ReceiptID)
	if err != nil {
		if errors.Is(err, ErrReceiptNotFound) {
			return &slot, nil, nil
		}
		return &slot, nil, err
	}
	if !IsActiveHolder(r.Status) {
		return &slot, nil, nil
	}
	return &slot, r, nil
}

// List returns all receipts for a project and task, sorted chronologically.
func (s *Store) List(ctx context.Context, project, taskID string) ([]*PreviewReceipt, error) {
	recs, err := s.db.ListRecordsWithRevision(receiptTable, s.workspace, receiptPrefix(project, taskID))
	if err != nil {
		return nil, err
	}

	var out []*PreviewReceipt
	for _, rec := range recs {
		var rRec receiptRecord
		if err := json.Unmarshal([]byte(rec.Payload), &rRec); err != nil {
			continue
		}
		rev, _ := strconv.ParseInt(rec.Revision, 10, 64)
		r := rRec.PreviewReceipt
		r.OperationalPatch = rRec.StoredPatch
		r.Revision = rev
		out = append(out, &r)
	}
	return out, nil
}

type BatchPrepareCommitParams struct {
	Project        string
	TaskID         string
	CommitIntentID string
	PreCommitSHA   string
}

// BatchTransitionToCommitting atomically transitions all CAPTURED receipts for a task
// to COMMITTING state in a single guarded database transaction, binding CommitIntentID and PreCommitSHA.
// If any receipt for the task is in a mutating state (PENDING, EXECUTING, CAPTURING, REVERTING),
// or if an active receipt holds the task slot, it aborts the entire transaction with ErrConflict.
func (s *Store) BatchTransitionToCommitting(ctx context.Context, params BatchPrepareCommitParams) ([]*PreviewReceipt, error) {
	if params.Project == "" || params.TaskID == "" || params.CommitIntentID == "" || params.PreCommitSHA == "" {
		return nil, fmt.Errorf("project, taskID, commitIntentID, and preCommitSHA are required")
	}

	allReceipts, err := s.List(ctx, params.Project, params.TaskID)
	if err != nil {
		return nil, fmt.Errorf("list receipts: %w", err)
	}

	for _, rec := range allReceipts {
		if rec.Status == StatusPending || rec.Status == StatusExecuting || rec.Status == StatusCapturing || rec.Status == StatusReverting {
			return nil, fmt.Errorf("%w: turn %s is actively %s", ErrConflict, rec.TurnID, rec.Status)
		}
	}

	var capturedTargets []*PreviewReceipt
	for _, rec := range allReceipts {
		if rec.Status == StatusCaptured {
			capturedTargets = append(capturedTargets, rec)
		}
	}
	if len(capturedTargets) == 0 {
		return nil, nil
	}

	var transitioned []*PreviewReceipt
	now := time.Now().UTC()

	txErr := s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(params.Project, params.TaskID))
		if err == nil && sFound && strings.TrimSpace(slotPayload) != "" {
			var slot TaskSlot
			if err := json.Unmarshal([]byte(slotPayload), &slot); err == nil {
				if slot.IsCommitIntentGroup() {
					return nil, fmt.Errorf("%w: slot already held by commit intent group %s", ErrConflict, slot.CommitIntentID)
				}
				if slot.ReceiptID != "" && now.Before(slot.LeaseExpiresAt) {
					isOurCaptured := false
					for _, c := range capturedTargets {
						if c.ID == slot.ReceiptID {
							isOurCaptured = true
							break
						}
					}
					if !isOurCaptured {
						return nil, fmt.Errorf("%w: slot held by active receipt %s until %s", ErrConflict, slot.ReceiptID, slot.LeaseExpiresAt.Format(time.RFC3339))
					}
				}
			}
		}

		var writes []controldb.KVWrite
		var batchResult []*PreviewReceipt
		var targetIDs []string

		for _, target := range capturedTargets {
			payload, rev, found, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(params.Project, params.TaskID, target.ID))
			if err != nil {
				return nil, fmt.Errorf("read receipt %s: %w", target.ID, err)
			}
			if !found || strings.TrimSpace(payload) == "" {
				return nil, fmt.Errorf("receipt %s not found: %w", target.ID, ErrReceiptNotFound)
			}
			if rev != target.Revision {
				return nil, fmt.Errorf("%w: receipt %s revision changed (expected %d, got %d)", ErrCASConflict, target.ID, target.Revision, rev)
			}

			var rec receiptRecord
			if err := json.Unmarshal([]byte(payload), &rec); err != nil {
				return nil, fmt.Errorf("unmarshal receipt %s: %w", target.ID, err)
			}

			current := rec.PreviewReceipt
			current.OperationalPatch = rec.StoredPatch
			current.Revision = rev

			if current.Status != StatusCaptured {
				return nil, fmt.Errorf("%w: receipt %s is in status %s (must be CAPTURED)", ErrConflict, current.ID, current.Status)
			}

			current.Status = StatusCommitting
			current.CommitIntentID = params.CommitIntentID
			current.PreCommitSHA = params.PreCommitSHA
			current.UpdatedAt = now
			current.Revision = rev + 1

			rec.PreviewReceipt = current
			rec.StoredPatch = current.OperationalPatch

			recBytes, err := json.Marshal(rec)
			if err != nil {
				return nil, fmt.Errorf("marshal updated receipt %s: %w", target.ID, err)
			}

			writes = append(writes, controldb.KVWrite{
				Table:     receiptTable,
				Workspace: s.workspace,
				Key:       receiptKey(params.Project, params.TaskID, target.ID),
				Payload:   string(recBytes),
			})
			batchResult = append(batchResult, &current)
			targetIDs = append(targetIDs, target.ID)
		}

		newSlot := TaskSlot{
			HolderType:     HolderTypeCommitIntent,
			ReceiptID:      params.CommitIntentID,
			CommitIntentID: params.CommitIntentID,
			ReceiptIDs:     targetIDs,
			Project:        params.Project,
			TaskID:         params.TaskID,
			UpdatedAt:      now,
			LeaseExpiresAt: now.Add(DefaultLeaseDuration),
		}
		slotBytes, err := json.Marshal(newSlot)
		if err != nil {
			return nil, fmt.Errorf("marshal task slot: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     slotTable,
			Workspace: s.workspace,
			Key:       slotKey(params.Project, params.TaskID),
			Payload:   string(slotBytes),
		})

		transitioned = batchResult
		return writes, nil
	})

	if txErr != nil {
		return nil, txErr
	}
	return transitioned, nil
}

// FinalizeCommitIntentGroup atomically transitions all receipts in a commit intent group
// to COMMITTED with the given commitSHA and releases the task slot in a single guarded transaction.
// The receipts are marked with SnapshotCleanupPending=true so that snapshots can be purged
// subsequently and cleared.
func (s *Store) FinalizeCommitIntentGroup(ctx context.Context, project, taskID, commitIntentID, commitSHA string, receipts []*PreviewReceipt) ([]*PreviewReceipt, error) {
	if project == "" || taskID == "" || commitIntentID == "" {
		return nil, errors.New("project, taskID, and commitIntentID are required")
	}
	if len(receipts) == 0 {
		return nil, nil
	}

	var finalized []*PreviewReceipt
	now := time.Now().UTC()

	err := s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(project, taskID))
		if err != nil {
			return nil, fmt.Errorf("read slot: %w", err)
		}
		var slot TaskSlot
		if sFound && strings.TrimSpace(slotPayload) != "" {
			_ = json.Unmarshal([]byte(slotPayload), &slot)
		}
		if !slot.MatchesHolder("", commitIntentID) && slot.ReceiptID != commitIntentID {
			return nil, fmt.Errorf("%w: slot is not held by commit intent %s", ErrConflict, commitIntentID)
		}

		var writes []controldb.KVWrite
		var batchResult []*PreviewReceipt

		for _, target := range receipts {
			payload, rev, found, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(project, taskID, target.ID))
			if err != nil {
				return nil, fmt.Errorf("read receipt %s: %w", target.ID, err)
			}
			if !found || strings.TrimSpace(payload) == "" {
				return nil, fmt.Errorf("receipt %s not found: %w", target.ID, ErrReceiptNotFound)
			}
			if rev != target.Revision {
				return nil, fmt.Errorf("%w: receipt %s revision changed (expected %d, got %d)", ErrCASConflict, target.ID, target.Revision, rev)
			}

			var rec receiptRecord
			if err := json.Unmarshal([]byte(payload), &rec); err != nil {
				return nil, fmt.Errorf("unmarshal receipt %s: %w", target.ID, err)
			}
			current := rec.PreviewReceipt
			current.OperationalPatch = rec.StoredPatch
			current.Revision = rev

			if current.Status != StatusCommitting {
				return nil, fmt.Errorf("%w: receipt %s is in status %s (expected %s)", ErrInvalidState, current.ID, current.Status, StatusCommitting)
			}
			if current.CommitIntentID != commitIntentID {
				return nil, fmt.Errorf("%w: receipt %s has intent %s (expected %s)", ErrInvalidState, current.ID, current.CommitIntentID, commitIntentID)
			}

			current.Status = StatusCommitted
			current.CommittedSHA = commitSHA
			current.SnapshotCleanupPending = true
			current.UpdatedAt = now
			current.Revision = rev + 1

			rec.PreviewReceipt = current
			rec.StoredPatch = current.OperationalPatch
			recBytes, err := json.Marshal(rec)
			if err != nil {
				return nil, fmt.Errorf("marshal finalized receipt %s: %w", target.ID, err)
			}
			writes = append(writes, controldb.KVWrite{
				Table:     receiptTable,
				Workspace: s.workspace,
				Key:       receiptKey(project, taskID, target.ID),
				Payload:   string(recBytes),
			})
			batchResult = append(batchResult, &current)
		}

		// Release slot atomically with the receipt transitions
		slot.HolderType = ""
		slot.ReceiptID = ""
		slot.CommitIntentID = ""
		slot.ReceiptIDs = nil
		slot.UpdatedAt = now
		slotBytes, err := json.Marshal(slot)
		if err != nil {
			return nil, fmt.Errorf("marshal released slot: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     slotTable,
			Workspace: s.workspace,
			Key:       slotKey(project, taskID),
			Payload:   string(slotBytes),
		})

		finalized = batchResult
		return writes, nil
	})

	if err != nil {
		return nil, err
	}
	return finalized, nil
}

// AbortCommitIntentGroup atomically transitions all receipts in a commit intent group
// to REVERT_FAILED with the given reason in a single guarded transaction.
// The slot remains held in REVERT_FAILED status to prevent concurrent turn executions.
func (s *Store) AbortCommitIntentGroup(ctx context.Context, project, taskID, commitIntentID, reason string, receipts []*PreviewReceipt) ([]*PreviewReceipt, error) {
	if project == "" || taskID == "" || commitIntentID == "" {
		return nil, errors.New("project, taskID, and commitIntentID are required")
	}
	if len(receipts) == 0 {
		return nil, nil
	}

	var aborted []*PreviewReceipt
	now := time.Now().UTC()

	err := s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(project, taskID))
		if err != nil {
			return nil, fmt.Errorf("read slot: %w", err)
		}
		var slot TaskSlot
		if sFound && strings.TrimSpace(slotPayload) != "" {
			_ = json.Unmarshal([]byte(slotPayload), &slot)
		}
		if !slot.MatchesHolder("", commitIntentID) && slot.ReceiptID != commitIntentID {
			return nil, fmt.Errorf("%w: slot is not held by commit intent %s", ErrConflict, commitIntentID)
		}

		var writes []controldb.KVWrite
		var batchResult []*PreviewReceipt

		for _, target := range receipts {
			payload, rev, found, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(project, taskID, target.ID))
			if err != nil {
				return nil, fmt.Errorf("read receipt %s: %w", target.ID, err)
			}
			if !found || strings.TrimSpace(payload) == "" {
				return nil, fmt.Errorf("receipt %s not found: %w", target.ID, ErrReceiptNotFound)
			}
			if rev != target.Revision {
				return nil, fmt.Errorf("%w: receipt %s revision changed (expected %d, got %d)", ErrCASConflict, target.ID, target.Revision, rev)
			}

			var rec receiptRecord
			if err := json.Unmarshal([]byte(payload), &rec); err != nil {
				return nil, fmt.Errorf("unmarshal receipt %s: %w", target.ID, err)
			}
			current := rec.PreviewReceipt
			current.OperationalPatch = rec.StoredPatch
			current.Revision = rev

			if current.Status != StatusCommitting {
				return nil, fmt.Errorf("%w: receipt %s is in status %s (expected %s)", ErrInvalidState, current.ID, current.Status, StatusCommitting)
			}

			current.Status = StatusRevertFailed
			current.FailureReason = CapAndRedact("review commit failed: "+reason, MaxFailureReasonBytes)
			current.UpdatedAt = now
			current.Revision = rev + 1

			rec.PreviewReceipt = current
			rec.StoredPatch = current.OperationalPatch
			recBytes, err := json.Marshal(rec)
			if err != nil {
				return nil, fmt.Errorf("marshal aborted receipt %s: %w", target.ID, err)
			}
			writes = append(writes, controldb.KVWrite{
				Table:     receiptTable,
				Workspace: s.workspace,
				Key:       receiptKey(project, taskID, target.ID),
				Payload:   string(recBytes),
			})
			batchResult = append(batchResult, &current)
		}

		// Slot remains held in REVERT_FAILED
		slot.UpdatedAt = now
		slotBytes, err := json.Marshal(slot)
		if err != nil {
			return nil, fmt.Errorf("marshal slot: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     slotTable,
			Workspace: s.workspace,
			Key:       slotKey(project, taskID),
			Payload:   string(slotBytes),
		})

		aborted = batchResult
		return writes, nil
	})

	if err != nil {
		return nil, err
	}
	return aborted, nil
}

// RecoverCommitIntentGroup atomically transitions all COMMITTING receipts for a commit intent group
// to targetStatus in a single guarded transaction.
// If targetStatus is COMMITTED or CAPTURED, the slot is released.
// If targetStatus is REVERT_FAILED, the slot remains held.
func (s *Store) RecoverCommitIntentGroup(ctx context.Context, project, taskID, commitIntentID string, targetStatus string, mutate func(r *PreviewReceipt) error) ([]*PreviewReceipt, error) {
	if project == "" || taskID == "" || commitIntentID == "" {
		return nil, errors.New("project, taskID, and commitIntentID are required")
	}

	allReceipts, err := s.List(ctx, project, taskID)
	if err != nil {
		return nil, fmt.Errorf("list receipts: %w", err)
	}

	var targets []*PreviewReceipt
	for _, rec := range allReceipts {
		if rec.CommitIntentID == commitIntentID && rec.Status == StatusCommitting {
			targets = append(targets, rec)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no COMMITTING receipts found for commit intent %s", commitIntentID)
	}

	var recovered []*PreviewReceipt
	now := time.Now().UTC()

	err = s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(project, taskID))
		if err != nil {
			return nil, fmt.Errorf("read slot: %w", err)
		}
		var slot TaskSlot
		if sFound && strings.TrimSpace(slotPayload) != "" {
			_ = json.Unmarshal([]byte(slotPayload), &slot)
		}

		var writes []controldb.KVWrite
		var batchResult []*PreviewReceipt

		for _, target := range targets {
			payload, rev, found, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(project, taskID, target.ID))
			if err != nil {
				return nil, fmt.Errorf("read receipt %s: %w", target.ID, err)
			}
			if !found || strings.TrimSpace(payload) == "" {
				return nil, fmt.Errorf("receipt %s not found: %w", target.ID, ErrReceiptNotFound)
			}
			if rev != target.Revision {
				return nil, fmt.Errorf("%w: receipt %s revision changed (expected %d, got %d)", ErrCASConflict, target.ID, target.Revision, rev)
			}

			var rec receiptRecord
			if err := json.Unmarshal([]byte(payload), &rec); err != nil {
				return nil, fmt.Errorf("unmarshal receipt %s: %w", target.ID, err)
			}

			current := rec.PreviewReceipt
			current.OperationalPatch = rec.StoredPatch
			current.Revision = rev

			if current.Status != StatusCommitting {
				return nil, fmt.Errorf("%w: receipt %s is in status %s (expected %s)", ErrInvalidState, current.ID, current.Status, StatusCommitting)
			}
			if err := ValidateTransition(current.Status, targetStatus); err != nil {
				return nil, err
			}
			if mutate != nil {
				if err := mutate(&current); err != nil {
					return nil, err
				}
			}

			current.Status = targetStatus
			current.UpdatedAt = now
			current.Revision = rev + 1

			rec.PreviewReceipt = current
			rec.StoredPatch = current.OperationalPatch
			recBytes, err := json.Marshal(rec)
			if err != nil {
				return nil, fmt.Errorf("marshal recovered receipt %s: %w", current.ID, err)
			}
			writes = append(writes, controldb.KVWrite{
				Table:     receiptTable,
				Workspace: s.workspace,
				Key:       receiptKey(project, taskID, current.ID),
				Payload:   string(recBytes),
			})
			batchResult = append(batchResult, &current)
		}

		if targetStatus == StatusCommitted || targetStatus == StatusCaptured {
			// Slot is released
			slot.HolderType = ""
			slot.ReceiptID = ""
			slot.CommitIntentID = ""
			slot.ReceiptIDs = nil
			slot.UpdatedAt = now
		} else {
			// Slot remains occupied
			slot.UpdatedAt = now
		}
		slotBytes, err := json.Marshal(slot)
		if err != nil {
			return nil, fmt.Errorf("marshal slot: %w", err)
		}
		writes = append(writes, controldb.KVWrite{
			Table:     slotTable,
			Workspace: s.workspace,
			Key:       slotKey(project, taskID),
			Payload:   string(slotBytes),
		})

		recovered = batchResult
		return writes, nil
	})

	if err != nil {
		return nil, err
	}
	return recovered, nil
}

// ClearSnapshotCleanupPending resets the SnapshotCleanupPending flag on a receipt after successful snapshot purge.
func (s *Store) ClearSnapshotCleanupPending(ctx context.Context, project, taskID, receiptID string) error {
	return s.db.CommitRecordWritesGuardedTx(s.workspace, func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		payload, rev, found, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(project, taskID, receiptID))
		if err != nil {
			return nil, fmt.Errorf("read receipt: %w", err)
		}
		if !found || strings.TrimSpace(payload) == "" {
			return nil, ErrReceiptNotFound
		}

		var rec receiptRecord
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal receipt: %w", err)
		}

		rec.SnapshotCleanupPending = false
		rec.UpdatedAt = time.Now().UTC()
		rec.Revision = rev + 1

		recBytes, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("marshal receipt: %w", err)
		}

		return []controldb.KVWrite{
			{
				Table:     receiptTable,
				Workspace: s.workspace,
				Key:       receiptKey(project, taskID, receiptID),
				Payload:   string(recBytes),
			},
		}, nil
	})
}

