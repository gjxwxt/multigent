package previewreceipt

import (
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	StatusPending      = "PENDING"
	StatusExecuting    = "EXECUTING"
	StatusCapturing    = "CAPTURING"
	StatusCaptured     = "CAPTURED"
	StatusReverting    = "REVERTING"
	StatusRolledBack   = "ROLLED_BACK"
	StatusRevertFailed = "REVERT_FAILED"
	StatusSuperseded   = "SUPERSEDED"
	StatusCommitting   = "COMMITTING"
	StatusCommitted    = "COMMITTED"
	StatusFailed       = "FAILED"
)

var (
	ErrSlotOccupied    = errors.New("task already has an active preview receipt slot")
	ErrCASConflict     = errors.New("preview receipt CAS revision conflict")
	ErrInvalidState    = errors.New("invalid preview receipt state transition")
	ErrReceiptNotFound = errors.New("preview receipt not found")
)

// IsTerminal reports whether status is an immutable terminal state.
func IsTerminal(status string) bool {
	switch status {
	case StatusRolledBack, StatusSuperseded, StatusCommitted, StatusFailed:
		return true
	default:
		return false
	}
}

// IsActiveHolder reports whether status actively occupies the task slot.
func IsActiveHolder(status string) bool {
	switch status {
	case StatusPending, StatusExecuting, StatusCapturing, StatusReverting, StatusCommitting, StatusRevertFailed:
		return true
	default:
		return false
	}
}

// ValidateTransition validates state machine transitions.
// Note: COMMITTING -> CAPTURED is strictly forbidden in the general state machine
// (a rollback or compensation requires proof from a CommitIntent checkpoint).
func ValidateTransition(from, to string) error {
	if IsTerminal(from) {
		return fmt.Errorf("%w: terminal state %s cannot transition to %s", ErrInvalidState, from, to)
	}
	if from == to {
		return nil
	}

	valid := false
	switch from {
	case StatusPending:
		valid = to == StatusExecuting || to == StatusFailed
	case StatusExecuting:
		valid = to == StatusCapturing || to == StatusFailed
	case StatusCapturing:
		valid = to == StatusCaptured || to == StatusFailed || to == StatusRevertFailed
	case StatusCaptured:
		valid = to == StatusReverting || to == StatusSuperseded || to == StatusCommitting || to == StatusFailed
	case StatusReverting:
		valid = to == StatusRolledBack || to == StatusRevertFailed
	case StatusCommitting:
		valid = to == StatusCommitted || to == StatusRevertFailed
	case StatusRevertFailed:
		valid = to == StatusFailed
	}

	if !valid {
		return fmt.Errorf("%w: cannot transition from %s to %s", ErrInvalidState, from, to)
	}
	return nil
}

// PostimageEntry records the state of a touched path after patch application.
type PostimageEntry struct {
	Path   string      `json:"path"`
	Exists bool        `json:"exists"`
	Mode   os.FileMode `json:"mode,omitempty"`
	SHA256 string      `json:"sha256,omitempty"`
}

// PreviewReceipt represents a single preview turn change receipt.
type PreviewReceipt struct {
	ID               string           `json:"id"`
	WorkspaceID      string           `json:"workspaceId"`
	Project          string           `json:"project"`
	TaskID           string           `json:"taskId"`
	TurnID           string           `json:"turnId"`
	Status           string           `json:"status"`
	Revision         int64            `json:"revision"`
	BaselineTree     string           `json:"baselineTree"`
	BaselineCommit   string           `json:"baselineCommit"`
	BaselineRef      string           `json:"baselineRef"`
	RequestDigest    string           `json:"requestDigest"`
	RedactedPrompt   string           `json:"redactedPrompt"`
	Creator          string           `json:"creator"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	LeaseExpiresAt   time.Time        `json:"leaseExpiresAt"`
	OperationalPatch string           `json:"-"` // Sealed with SealBytesStrict; never serialized to public JSON
	DisplayDiff      string           `json:"displayDiff,omitempty"`
	TouchedPaths     []string         `json:"touchedPaths,omitempty"`
	Postimages       []PostimageEntry `json:"postimages,omitempty"`
	FailureReason    string           `json:"failureReason,omitempty"`
	CommitIntentID   string           `json:"commitIntentId,omitempty"`
	CommittedSHA     string           `json:"committedSha,omitempty"`
}

// receiptRecord is used exclusively for internal storage in kv_records to persist
// OperationalPatch without exposing it on PreviewReceipt's default JSON representation.
type receiptRecord struct {
	PreviewReceipt
	StoredPatch string `json:"storedPatch,omitempty"`
}

// TaskSlot tracks the active receipt slot for a project/task pair.
type TaskSlot struct {
	ReceiptID      string    `json:"receiptId"`
	Project        string    `json:"project"`
	TaskID         string    `json:"taskId"`
	UpdatedAt      time.Time `json:"updatedAt"`
	LeaseExpiresAt time.Time `json:"leaseExpiresAt"`
}
