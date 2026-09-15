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

// Create persists a new proposal in StateAwaitingApproval. Only one ACTIVE
// proposal (not applied/rejected) may exist per task — V1 serialization
// decision (approval §8.2.2).
func (s *Store) Create(project, taskID, actor, request, patch, diff string, paths []string) (*Proposal, error) {
	if existing, err := s.ActiveForTask(project, taskID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("task %s already has an active proposal %s (state %s) — resolve or reject it first", taskID, existing.ID, existing.State)
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
	if err := s.db.UpsertRecord(proposalTable, s.workspace, proposalKey(p.ID), string(raw)); err != nil {
		return nil, err
	}
	return p, nil
}

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
// apply succeeded.
func (s *Store) MarkApplied(id string, postimage map[string]string, appliedSHA string) (*Proposal, error) {
	return s.transition(id, StateApplying, StateApplied, func(p *Proposal) {
		p.Postimage = postimage
		p.AppliedSHA = appliedSHA
	})
}

// MarkVerificationFailed parks a proposal whose in-clone verification failed.
func (s *Store) MarkVerificationFailed(id string, verification map[string]string) (*Proposal, error) {
	return s.transition(id, StateApplying, StateVerificationFaile, func(p *Proposal) {
		p.Verification = verification
	})
}

// AbortApply unwinds an apply that failed before touching the worktree
// (environment errors, moved baseline): back to awaiting_approval with the
// abort reason recorded, so a stranded `applying` row can never block the
// task's single-active-proposal slot forever.
func (s *Store) AbortApply(id string, detail map[string]string) (*Proposal, error) {
	return s.transition(id, StateApplying, StateAwaitingApproval, func(p *Proposal) {
		p.Verification = detail
	})
}

// Reject terminates a proposal without applying.
func (s *Store) Reject(id, actor string) (*Proposal, error) {
	return s.transition(id, StateAwaitingApproval, StateRejected, func(p *Proposal) {
		p.Actor = actor
	})
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
	out = authHeader.ReplaceAllString(out, "${1}${3}[REDACTED]")
	return out
}
