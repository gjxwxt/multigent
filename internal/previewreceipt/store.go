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
			if err := json.Unmarshal([]byte(slotPayload), &slot); err == nil && slot.ReceiptID != "" {
				// Read current slot holder
				hPayload, _, hFound, err := tx.GetRecordWithRevision(receiptTable, s.workspace, receiptKey(params.Project, params.TaskID, slot.ReceiptID))
				if err != nil {
					return nil, fmt.Errorf("read slot holder receipt: %w", err)
				}
				if hFound && strings.TrimSpace(hPayload) != "" {
					var holderRec receiptRecord
					if err := json.Unmarshal([]byte(hPayload), &holderRec); err == nil {
						if IsActiveHolder(holderRec.Status) {
							// Active holder exists. Check lease expiration:
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
			slotPayload, _, sFound, err := tx.GetRecordWithRevision(slotTable, s.workspace, slotKey(project, taskID))
			if err == nil && sFound && strings.TrimSpace(slotPayload) != "" {
				var slot TaskSlot
				if err := json.Unmarshal([]byte(slotPayload), &slot); err == nil && slot.ReceiptID == receiptID {
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
