// Package changerun implements the controlled Change Run (batch plan §4.1
// v6): a proposal (patch) against a task's worktree that is applied inside a
// purified clone, verified there, then applied to the main worktree under
// the cross-process project Git lock — with rollback keyed on the recorded
// postimage. V1 scope per approval record §8.2: clean baseline only, one
// active proposal per task, backend-first (no UI).
//
// This file is the proposal persistence layer: kv_records rows
// (table "preview_change_proposals") with payload+revision CAS state
// transitions — the same primitive the sandbox leases and workflow
// transition claims use — plus size caps and secret redaction before
// anything durable is written.
package changerun

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// Proposal states (§4.1 v6 state machine).
const (
	StateProposed          = "proposed"
	StateAwaitingApproval  = "awaiting_approval"
	StateApplying          = "applying"
	StateApplied           = "applied"
	StateVerificationFaile = "verification_failed"
	StateRejected          = "rejected"
)

// Size caps (§4.1 v6: request 16KB, patch/diff 256KB). Oversized input is
// truncated with a flag — full content never lands in kv_records.
const (
	MaxRequestBytes = 16 << 10
	MaxPatchBytes   = 256 << 10
)

// Proposal is the persisted change-run record.
type Proposal struct {
	ID            string            `json:"id"`
	WorkspaceID   string            `json:"workspaceId"`
	Project       string            `json:"project"`
	TaskID        string            `json:"taskId"`
	State         string            `json:"state"`
	Actor         string            `json:"actor"`
	Request       string            `json:"request"`
	RequestCapped bool              `json:"requestCapped"`
	Patch         string            `json:"patch"`
	PatchCapped   bool              `json:"patchCapped"`
	Diff          string            `json:"diff"`
	DiffCapped    bool              `json:"diffCapped"`
	Paths         []string          `json:"paths"`
	Verification  map[string]string `json:"verification,omitempty"`
	Postimage     map[string]string `json:"postimage,omitempty"`
	AppliedSHA    string            `json:"appliedSha,omitempty"`
	RollbackState string            `json:"rollbackState,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

const proposalTable = "preview_change_proposals"

// Store persists proposals for one workspace.
type Store struct {
	db        controldb.Store
	workspace string
}

// NewStore opens the proposal store.
func NewStore(db controldb.Store, workspace string) *Store {
	return &Store{db: db, workspace: workspace}
}

func proposalKey(id string) []string { return []string{"proposal", id, ""} }

// NewProposalID mints an opaque id.
func NewProposalID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("crp-%d", time.Now().UnixNano())
	}
	return "crp-" + hex.EncodeToString(b[:])
}

// taskSlot is the per-task single-active-proposal lock row. It is written in
// the SAME transaction as the proposal row it guards (Create, terminal
// transitions) via CommitRecordWrites — a crash between the two writes is
// impossible, so no ghost slot can outlive its proposal (round-19 P1).
// Concurrent Creates still race, but now on the atomic swap itself: the
// claim condition is enforced with a CAS on the slot row's payload+revision
// INSIDE the transaction body — SQLite serializes IMMEDIATE transactions,
// so exactly one concurrent claimant commits (round-18 P1-1).
type taskSlot struct {
	ProposalID string `json:"proposalId"`
	UpdatedAt  string `json:"updatedAt"`
}

func taskSlotKey(project, taskID string) []string {
	// kv_records keys have exactly 3 segments; the proposal's (project,
	// task) identity compresses into k2. The trailing empty k3 matches the
	// convention of the proposal rows.
	return []string{"slot", project + "\x00" + taskID, ""}
}

// acquireTaskSlot was the round-18 stopgap (slot row written separately from
// the proposal row). Round-19 P1 replaces it: slot and proposal live and die
// in ONE CommitRecordWritesGuarded transaction — see Create and
// transitionTerminal.

// freeTaskSlot was the round-18 stopgap with the same flaw. Replaced by the
// transactional terminal transition.

// orphanedSlotHoldState reports whether a slot row's holder no longer exists
// as an active proposal (crash debris from a pre-transaction build, or a
// proposal row lost after a partial legacy write). Such slots are repaired
// on sight: the next Create reclaims them instead of erroring forever.
func (s *Store) slotHolderIsActive(project, taskID string, tx controldb.KVTx) (bool, error) {
	slotKey := taskSlotKey(project, taskID)
	payload, found, err := tx.GetRecord(proposalTable, s.workspace, slotKey)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	var current taskSlot
	if err := json.Unmarshal([]byte(payload), &current); err != nil {
		// Unparseable slot payload: crash debris — treat as orphaned.
		return false, nil
	}
	if current.ProposalID == "" {
		return false, nil
	}
	// The holder's proposal row MUST be read through tx: the guard runs on
	// the connection holding the IMMEDIATE lock — a pool read here would
	// wait for that very lock and deadlock (proven by the round-19 hang).
	holdPayload, found, err := tx.GetRecord(proposalTable, s.workspace, proposalKey(current.ProposalID))
	if err != nil || !found {
		return false, nil
	}
	p, err := decodeProposal(holdPayload)
	if err != nil {
		return false, nil
	}
	return isActiveState(p.State), nil
}

// Create persists a new proposal in StateAwaitingApproval. Only one ACTIVE
// proposal (not applied/rejected) may exist per task — V1 serialization
// decision (approval §8.2.2). The proposal row and its task slot are written
// in ONE guarded transaction whose guard re-reads the slot under the
// IMMEDIATE write lock: concurrent Creates serialize, exactly one commits,
// and a crash can never strand a slot without its proposal (round-18 P1-1,
// round-19 P1). The ActiveForTask scan up front only produces a friendlier
// error; the transactional guard is the real gate.
func (s *Store) Create(project, taskID, actor, request, patch, diff string, paths []string) (*Proposal, error) {
	existing, err := s.ActiveForTask(project, taskID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// Ghost repair (round-19 P1): a slot holder that no longer exists or
		// sits in a terminal state is legacy crash debris — the transactional
		// guard below re-verifies under the lock and overwrites the stale
		// slot, so reclaim instead of erroring forever.
		recs, recErr := s.db.ListRecordsWithRevision(proposalTable, s.workspace, proposalKey(existing.ID))
		genuine := recErr == nil && len(recs) == 1
		if genuine {
			if p, decErr := decodeProposal(recs[0].Payload); decErr == nil && isActiveState(p.State) {
				genuine = true
			} else {
				genuine = false
			}
		}
		if genuine {
			return nil, fmt.Errorf("task %s already has an active proposal %s (state %s) — resolve or reject it first", taskID, existing.ID, existing.State)
		}
	}
	now := time.Now().UTC()
	requestCapped, request := capAndRedact(request, MaxRequestBytes)
	patchCapped, patch := capAndRedact(patch, MaxPatchBytes)
	diffCapped, diff := capAndRedact(diff, MaxPatchBytes)
	p := &Proposal{
		ID:            NewProposalID(),
		WorkspaceID:   s.workspace,
		Project:       project,
		TaskID:        taskID,
		State:         StateAwaitingApproval,
		Actor:         actor,
		Request:       request,
		RequestCapped: requestCapped,
		Patch:         patch,
		PatchCapped:   patchCapped,
		Diff:          diff,
		DiffCapped:    diffCapped,
		Paths:         paths,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	slot := taskSlot{ProposalID: p.ID, UpdatedAt: now.Format(time.RFC3339Nano)}
	slotRaw, err := json.Marshal(slot)
	if err != nil {
		return nil, err
	}
	guardErr := s.db.CommitRecordWritesGuarded(s.workspace, func(tx controldb.KVTx) error {
		// IMMEDIATE lock held: this read cannot interleave with another
		// Create's commit, so check-then-write is race-free.
		active, err := s.slotHolderIsActive(project, taskID, tx)
		if err != nil {
			return err
		}
		if active {
			return errSlotHeld{project, taskID}
		}
		return nil
	}, []controldb.KVWrite{
		{Table: proposalTable, Workspace: s.workspace, Key: proposalKey(p.ID), Payload: string(raw)},
		{Table: proposalTable, Workspace: s.workspace, Key: taskSlotKey(project, taskID), Payload: string(slotRaw)},
	})
	if guardErr != nil {
		if errors.As(guardErr, &errSlotHeld{}) {
			return nil, fmt.Errorf("task %s already has an active proposal — concurrent create lost the race", taskID)
		}
		return nil, guardErr
	}
	return p, nil
}

// errSlotHeld signals a slot occupied by an active proposal inside the
// guarded create transaction.
type errSlotHeld struct{ project, taskID string }

func (e errSlotHeld) Error() string { return "slot held" }

func isMissingRecordErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such record") || strings.Contains(msg, "not found") || strings.Contains(msg, "no rows")
}

// Create persists a new proposal in StateAwaitingApproval. See the
// transactional implementation above (the doc comment there covers the
// single-active and atomicity contract).

// Get loads one proposal.
func (s *Store) Get(id string) (*Proposal, error) {
	recs, err := s.db.ListRecordsWithRevision(proposalTable, s.workspace, proposalKey(id))
	if err != nil {
		return nil, err
	}
	if len(recs) != 1 {
		return nil, fmt.Errorf("proposal %s: %d rows", id, len(recs))
	}
	return decodeProposal(recs[0].Payload)
}

// ActiveForTask returns the task's single active proposal, or nil.
func (s *Store) ActiveForTask(project, taskID string) (*Proposal, error) {
	recs, err := s.db.ListRecords(proposalTable, s.workspace, nil)
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		p, err := decodeProposal(rec.Payload)
		if err != nil {
			continue
		}
		if p.Project == project && p.TaskID == taskID && isActiveState(p.State) {
			return p, nil
		}
	}
	return nil, nil
}

func isActiveState(state string) bool {
	switch state {
	case StateProposed, StateAwaitingApproval, StateApplying:
		return true
	}
	return false
}

// transition CAS-swaps the proposal row only while it still holds
// expectState — concurrent operators cannot double-apply or double-reject.
func (s *Store) transition(id, expectState, newState string, mutate func(*Proposal)) (*Proposal, error) {
	recs, err := s.db.ListRecordsWithRevision(proposalTable, s.workspace, proposalKey(id))
	if err != nil {
		return nil, err
	}
	if len(recs) != 1 {
		return nil, fmt.Errorf("proposal %s: %d rows", id, len(recs))
	}
	current, err := decodeProposal(recs[0].Payload)
	if err != nil {
		return nil, err
	}
	if current.State != expectState {
		return nil, fmt.Errorf("proposal %s in state %s, expected %s (lost race)", id, current.State, expectState)
	}
	next := *current
	next.State = newState
	next.UpdatedAt = time.Now().UTC()
	if mutate != nil {
		mutate(&next)
	}
	raw, err := json.Marshal(&next)
	if err != nil {
		return nil, err
	}
	swapped, err := s.db.UpdateRecordIfPayloadAndRevision(proposalTable, s.workspace, proposalKey(id), string(raw), recs[0].Payload, recs[0].Revision)
	if err != nil {
		return nil, err
	}
	if !swapped {
		return nil, fmt.Errorf("proposal %s: lost transition race (%s -> %s)", id, expectState, newState)
	}
	return &next, nil
}

// BeginApply claims the exclusive applying state.
func (s *Store) BeginApply(id string) (*Proposal, error) {
	return s.transition(id, StateAwaitingApproval, StateApplying, nil)
}

// MarkApplied records the postimage and final SHA after the main-worktree
// apply succeeded, and frees the task's slot (applied is terminal).
func (s *Store) MarkApplied(id string, postimage map[string]string, appliedSHA string) (*Proposal, error) {
	return s.transitionTerminal(id, StateApplying, StateApplied, func(p *Proposal) {
		p.Postimage = postimage
		p.AppliedSHA = appliedSHA
	})
}

// MarkVerificationFailed parks a proposal whose in-clone verification failed
// (terminal) and frees the task's slot.
func (s *Store) MarkVerificationFailed(id string, verification map[string]string) (*Proposal, error) {
	return s.transitionTerminal(id, StateApplying, StateVerificationFaile, func(p *Proposal) {
		p.Verification = verification
	})
}

// AbortApply unwinds an apply that failed before touching the worktree
// (environment errors, moved baseline): back to awaiting_approval with the
// abort reason recorded. The slot stays held — awaiting_approval is active.
func (s *Store) AbortApply(id string, detail map[string]string) (*Proposal, error) {
	return s.transition(id, StateApplying, StateAwaitingApproval, func(p *Proposal) {
		p.Verification = detail
	})
}

// Reject terminates a proposal without applying (terminal) and frees the
// task's slot.
func (s *Store) Reject(id, actor string) (*Proposal, error) {
	return s.transitionTerminal(id, StateAwaitingApproval, StateRejected, func(p *Proposal) {
		p.Actor = actor
	})
}

// transitionTerminal performs the CAS state change and frees the task slot
// in ONE guarded transaction (round-19 P1): the guard re-verifies the
// expected state under the IMMEDIATE write lock, then the batch swaps the
// proposal payload to the terminal state and clears the slot atomically. A
// crash mid-transition leaves either both rows old or both new — never a
// terminal proposal still holding a slot (ghost slot).
func (s *Store) transitionTerminal(id, expectState, newState string, mutate func(*Proposal)) (*Proposal, error) {
	recs, err := s.db.ListRecordsWithRevision(proposalTable, s.workspace, proposalKey(id))
	if err != nil {
		return nil, err
	}
	if len(recs) != 1 {
		return nil, fmt.Errorf("proposal %s: %d rows", id, len(recs))
	}
	current, err := decodeProposal(recs[0].Payload)
	if err != nil {
		return nil, err
	}
	next := *current
	next.State = newState
	next.UpdatedAt = time.Now().UTC()
	if mutate != nil {
		mutate(&next)
	}
	raw, err := json.Marshal(&next)
	if err != nil {
		return nil, err
	}
	slot := taskSlot{UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	slotRaw, err := json.Marshal(slot)
	if err != nil {
		return nil, err
	}
	err = s.db.CommitRecordWritesGuarded(s.workspace, func(tx controldb.KVTx) error {
		// Claim check inside the lock: the proposal must still be in
		// expectState, and the slot (if present) must still name us. Either
		// violated = another transitioner won; abort with nothing written.
		fresh, found, err := tx.GetRecord(proposalTable, s.workspace, proposalKey(id))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("proposal %s vanished (lost race)", id)
		}
		p, err := decodeProposal(fresh)
		if err != nil {
			return err
		}
		if p.State != expectState {
			return fmt.Errorf("proposal %s in state %s, expected %s (lost race)", id, p.State, expectState)
		}
		// The slot may legitimately be missing (pre-slot legacy rows) or
		// held by us; held by ANOTHER active proposal means a concurrent
		// create already replaced us as the active proposal. All reads ride
		// tx — the IMMEDIATE lock holder cannot borrow pool connections.
		slotKey := taskSlotKey(p.Project, p.TaskID)
		payload, found, err := tx.GetRecord(proposalTable, s.workspace, slotKey)
		if err != nil {
			return err
		}
		if found {
			var held taskSlot
			if json.Unmarshal([]byte(payload), &held) == nil && held.ProposalID != "" && held.ProposalID != id {
				return fmt.Errorf("task %s slot held by proposal %s (lost race)", p.TaskID, held.ProposalID)
			}
		}
		return nil
	}, []controldb.KVWrite{
		{Table: proposalTable, Workspace: s.workspace, Key: proposalKey(id), Payload: string(raw)},
		{Table: proposalTable, Workspace: s.workspace, Key: taskSlotKey(current.Project, current.TaskID), Payload: string(slotRaw)},
	})
	if err != nil {
		// A lost race renders as a state mismatch in the error text — keep
		// the transition() shape of the message for callers/tests.
		return nil, err
	}
	return &next, nil
}

// decodeProposal parses a stored row.
func decodeProposal(payload string) (*Proposal, error) {
	var p Proposal
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// capAndRedact scrubs secret-looking content then truncates oversized input.
// It returns the capped flag separately so the struct assembly stays honest.
func capAndRedact(v string, capBytes int) (capped bool, out string) {
	var b strings.Builder
	for _, line := range strings.Split(v, "\n") {
		b.WriteString(RedactSecrets(line))
		b.WriteString("\n")
	}
	s := strings.TrimSuffix(b.String(), "\n")
	if len(s) > capBytes {
		return true, s[:capBytes] + "\n… [truncated]"
	}
	return false, s
}

// secretPatterns covers the same classes as the git-output redaction
// contract: key=value assignments, common token prefixes, and auth headers.
var (
	secretAssign  = regexp.MustCompile(`(?i)\b((?:api[_-]?key|secret|token|password|passwd|pwd|credential|private[_-]?key)[a-z0-9_-]*\s*[:=]\s*)([^\s"']{6,})`)
	knownPrefixes = regexp.MustCompile(`(?i)\b(sk-[a-z0-9_-]{8,}|ghp_[a-z0-9]{20,}|gho_[a-z0-9]{20,}|xox[bpao]-[a-z0-9-]{10,}|glpat-[a-z0-9_-]{15,}|ey[a-z0-9_-]{6,}\.[a-z0-9_-]{6,}\.[a-z0-9_-]{6,})`)
	authHeader    = regexp.MustCompile(`(?i)\b((?:authorization|proxy-authorization)\s*:\s*)(bearer\s+)?(\S{8,})`)
)

// RedactSecrets masks secret-looking spans in one line.
func RedactSecrets(line string) string {
	out := knownPrefixes.ReplaceAllString(line, "[REDACTED]")
	out = secretAssign.ReplaceAllString(out, "${1}[REDACTED]")
	// ${2} is the optional "bearer" scheme word; ${3} is the credential and
	// must NOT be re-emitted (re-emitting it would undo the redaction).
	out = authHeader.ReplaceAllString(out, "${1}${2}[REDACTED]")
	return out
}
