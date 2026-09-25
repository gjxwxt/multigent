package workflow

// Delivery plan persistence (minimal closed loop, slice 2 of 3): the frozen
// plan record in the control plane's kv store.
//
// WRITE ATOMICITY (precise, per GPT pre-commit review item 4)
//
// One run owns exactly ONE record, keyed (project, runID). Every write is a
// REVISION-CAS read-modify-write inside a bounded retry loop
// (ListRecordsWithRevision → mutate → UpdateRecordIfRevision): a reader sees
// either the previous complete record or the new complete record, and two
// concurrent writers can never overwrite each other's entries — the loser's
// CAS fails and its change is re-applied on top of the winner's record.
//
// What this does NOT claim: there is no separate approval record, so there is
// no cross-record "approval + freeze" transaction. The approval fields
// (ApprovedBy/ApprovedAt) live INSIDE the version record, i.e. inside the
// single record swap. When a future slice adds a formal approval entry
// (human review output persisted elsewhere), that entry and the freeze must
// share one transaction or this wording must change with it.
//
// TRUST
//
// Reads that feed materialization go through LoadFrozenPlanForRun, which
// RECOMPUTES the digest of the stored plan and refuses on mismatch (same
// trust shape as the QA baseline mirror: the record digest is the root of
// trust, the payload must match it byte for byte). There is no path that
// materializes a plan from anywhere else.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// planRecordTable is the kv_records namespace for frozen delivery plans.
// Key: (project, runID).
const planRecordTable = "workflow_plans"

// planRecordCASAttempts bounds the CAS retry loop. Contention here is between
// sibling branch completions (a handful of writers), so 8 attempts is far
// beyond the observed need; exhausting it is a refusal, never a silent drop.
const planRecordCASAttempts = 8

// planClaimLease bounds how long a claim with no task id blocks other
// materialization attempts: a claim is written BEFORE the child task exists,
// so a crash in that window must not park the work package forever. Inside
// the lease the claimant is presumed alive (another attempt must not fork a
// second identity); after it, the next activation may take the claim over.
const planClaimLease = 2 * time.Minute

// PlanMaterializationEntry pins one runtime-derived branch materialization:
// which work package produced which branch definition (snapshot hash), task
// and child run. Written idempotently per (wpId, branchId) so a re-drive can
// never mint a second identity for the same work package.
type PlanMaterializationEntry struct {
	WPID     string `json:"wpId"`
	BranchID string `json:"branchId"`
	// ClaimToken/ClaimedAt make the single-winner claim auditable: the token
	// belongs to the one materialization attempt that owns this work package,
	// and ClaimedAt bounds the claim lease (see ClaimPlanWorkPackage).
	ClaimToken       string `json:"claimToken,omitempty"`
	ClaimedAt        string `json:"claimedAt,omitempty"`
	DefinitionID     string `json:"definitionId,omitempty"`
	DefinitionDigest string `json:"definitionDigest,omitempty"`
	TaskID           string `json:"taskId,omitempty"`
	ChildRunID       string `json:"childRunId,omitempty"`
	WaveIndex        int    `json:"waveIndex"`
	MaterializedAt   string `json:"materializedAt,omitempty"`
}

// PlanApprovalProvenance records WHICH human review froze a version: the
// review step instance is the machine pointer back to the decision record
// (its outputValues carry the decision + comments).
type PlanApprovalProvenance struct {
	StepID     string `json:"stepId,omitempty"`
	InstanceID string `json:"instanceId,omitempty"`
	Comments   string `json:"comments,omitempty"`
}

// PlanVersionRecord is one frozen (or superseded) version of a plan.
type PlanVersionRecord struct {
	Version          int                     `json:"version"`
	Status           string                  `json:"status"`
	Digest           string                  `json:"digest"`
	ApprovedBy       string                  `json:"approvedBy,omitempty"`
	ApprovedAt       string                  `json:"approvedAt,omitempty"`
	FrozenAt         string                  `json:"frozenAt,omitempty"`
	SupersedesDigest string                  `json:"supersedesDigest,omitempty"`
	Approval         *PlanApprovalProvenance `json:"approval,omitempty"`
	Plan             DeliveryPlan            `json:"plan"`
}

// FrozenPlanRecord is the single control-plane record for a run's plan.
type FrozenPlanRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	PlanID        string `json:"planId"`
	Project       string `json:"project"`
	RunID         string `json:"runId"`
	// CurrentVersion is the ONLY version materialization may read. Earlier
	// versions remain listed for audit.
	CurrentVersion   int                        `json:"currentVersion"`
	Versions         []PlanVersionRecord        `json:"versions"`
	Materializations []PlanMaterializationEntry `json:"materializations,omitempty"`
	CreatedAt        string                     `json:"createdAt,omitempty"`
	UpdatedAt        string                     `json:"updatedAt,omitempty"`
}

// Current returns the current version record.
func (r FrozenPlanRecord) Current() (PlanVersionRecord, bool) {
	for _, v := range r.Versions {
		if v.Version == r.CurrentVersion {
			return v, true
		}
	}
	return PlanVersionRecord{}, false
}

// mutatePlanRecord applies mutate to the run's plan record atomically.
//
// create builds the initial record when none exists (its second return value
// reports whether creation is allowed at all — LoadFrozenPlanForRun callers
// pass false so a mutation never silently creates a record).
//
// mutate returns changed=false for a no-op (idempotent re-approval, duplicate
// materialization report) — no write happens then.
func (s *Store) mutatePlanRecord(project, runID string, create func() (FrozenPlanRecord, bool), mutate func(*FrozenPlanRecord) (bool, error)) (FrozenPlanRecord, error) {
	project, runID = strings.TrimSpace(project), strings.TrimSpace(runID)
	if s == nil || s.db == nil {
		return FrozenPlanRecord{}, fmt.Errorf("delivery plan persistence requires a control-plane store")
	}
	if project == "" || runID == "" {
		return FrozenPlanRecord{}, fmt.Errorf("delivery plan persistence requires project and runID")
	}
	key := []string{project, runID}
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	for attempt := 0; attempt < planRecordCASAttempts; attempt++ {
		recs, err := s.db.ListRecordsWithRevision(planRecordTable, s.workspaceID, key)
		if err != nil {
			return FrozenPlanRecord{}, err
		}
		if len(recs) == 0 {
			if create == nil {
				return FrozenPlanRecord{}, fmt.Errorf("delivery plan record for run %s does not exist", runID)
			}
			base, allowed := create()
			if !allowed {
				return FrozenPlanRecord{}, fmt.Errorf("delivery plan record for run %s does not exist", runID)
			}
			base.SchemaVersion = DeliveryPlanSchemaVersion
			base.Project, base.RunID = project, runID
			if base.CreatedAt == "" {
				base.CreatedAt = now()
			}
			base.UpdatedAt = base.CreatedAt
			payload, err := json.Marshal(base)
			if err != nil {
				return FrozenPlanRecord{}, err
			}
			inserted, err := s.db.InsertRecordIfAbsent(planRecordTable, s.workspaceID, key, string(payload))
			if err != nil {
				return FrozenPlanRecord{}, err
			}
			if inserted {
				return base, nil
			}
			// Lost the creation race: fall through and apply mutate on top of
			// the winner's record (never over it).
			continue
		}
		row := recs[0]
		var record FrozenPlanRecord
		if err := json.Unmarshal([]byte(row.Payload), &record); err != nil {
			return FrozenPlanRecord{}, fmt.Errorf("decode delivery plan record: %w", err)
		}
		changed, err := mutate(&record)
		if err != nil {
			return FrozenPlanRecord{}, err
		}
		if !changed {
			return record, nil
		}
		record.SchemaVersion = DeliveryPlanSchemaVersion
		record.Project, record.RunID = project, runID
		record.UpdatedAt = now()
		payload, err := json.Marshal(record)
		if err != nil {
			return FrozenPlanRecord{}, err
		}
		swapped, err := s.db.UpdateRecordIfRevision(planRecordTable, s.workspaceID, key, string(payload), row.Revision)
		if err != nil {
			return FrozenPlanRecord{}, err
		}
		if swapped {
			return record, nil
		}
		// Lost the CAS: a sibling writer appended in between. Re-read and
		// re-apply — dropping the update is exactly the lost-update bug this
		// loop exists to prevent.
	}
	return FrozenPlanRecord{}, fmt.Errorf("delivery plan record for run %s kept changing under %d CAS attempts; refusing to drop an update", runID, planRecordCASAttempts)
}

// FreezeDeliveryPlan validates and freezes a plan for a run.
//
// Idempotency: freezing the SAME canonical plan again (same digest) does not
// mint a new version — a double approval click cannot fork the plan. A
// different plan text (any covered byte) becomes version+1 and requires its
// own approval record (approvedBy must be non-empty). Concurrent freezes of
// DIFFERENT texts both land: one wins the CAS, the loser re-applies its
// version on top of the winner's record.
func (s *Store) FreezeDeliveryPlan(project string, plan DeliveryPlan, approvedBy string) (FrozenPlanRecord, PlanVersionRecord, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return FrozenPlanRecord{}, PlanVersionRecord{}, fmt.Errorf("freeze delivery plan: project is required")
	}
	if strings.TrimSpace(approvedBy) == "" {
		return FrozenPlanRecord{}, PlanVersionRecord{}, fmt.Errorf("freeze delivery plan: approvedBy is required (an unapproved plan must never be frozen)")
	}
	runID := strings.TrimSpace(plan.RunID)
	if runID == "" {
		return FrozenPlanRecord{}, PlanVersionRecord{}, fmt.Errorf("freeze delivery plan: plan.runId is required (plans are frozen against exactly one run)")
	}
	// The digest is computed over the plan WITH the run pinned, so the stored
	// payload and its digest can never disagree about which run it belongs to.
	plan.RunID = runID
	if err := ValidateDeliveryPlan(plan); err != nil {
		return FrozenPlanRecord{}, PlanVersionRecord{}, err
	}
	var frozen PlanVersionRecord
	var createErr error
	record, err := s.mutatePlanRecord(project, runID,
		func() (FrozenPlanRecord, bool) {
			// Build the initial record WITH the frozen version already applied:
			// mutatePlanRecord returns right after a successful insert, so a
			// bare empty record would persist a plan with no version.
			base := FrozenPlanRecord{}
			changed, entry, err := applyPlanFreeze(&base, plan, approvedBy, PlanApprovalProvenance{})
			if err != nil {
				createErr = err
				return FrozenPlanRecord{}, false
			}
			if !changed {
				createErr = fmt.Errorf("freeze delivery plan: plan text was not applied to the new record")
				return FrozenPlanRecord{}, false
			}
			frozen = entry
			return base, true
		},
		func(record *FrozenPlanRecord) (bool, error) {
			changed, entry, err := applyPlanFreeze(record, plan, approvedBy, PlanApprovalProvenance{})
			if err != nil {
				return false, err
			}
			frozen = entry
			return changed, nil
		})
	if err != nil {
		return FrozenPlanRecord{}, PlanVersionRecord{}, err
	}
	if createErr != nil {
		return FrozenPlanRecord{}, PlanVersionRecord{}, createErr
	}
	if frozen.Version == 0 {
		// Mutate reported "no change" (idempotent re-freeze) — surface the
		// stored current version so callers always get the frozen truth.
		current, ok := record.Current()
		if !ok {
			return FrozenPlanRecord{}, PlanVersionRecord{}, fmt.Errorf("delivery plan %s v%d missing after freeze", record.PlanID, record.CurrentVersion)
		}
		frozen = current
	}
	return record, frozen, nil
}

// LoadPlanRecord reads the raw record (no trust check) — diagnostics/tests.
func (s *Store) LoadPlanRecord(project, runID string) (FrozenPlanRecord, bool, error) {
	if s == nil || s.db == nil {
		return FrozenPlanRecord{}, false, fmt.Errorf("delivery plan lookup requires a control-plane store")
	}
	project, runID = strings.TrimSpace(project), strings.TrimSpace(runID)
	if project == "" || runID == "" {
		return FrozenPlanRecord{}, false, fmt.Errorf("delivery plan lookup requires project and runID")
	}
	payload, ok, err := s.db.GetRecord(planRecordTable, s.workspaceID, []string{project, runID})
	if err != nil || !ok {
		return FrozenPlanRecord{}, ok, err
	}
	var record FrozenPlanRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return FrozenPlanRecord{}, false, fmt.Errorf("decode delivery plan record: %w", err)
	}
	return record, true, nil
}

// applyPlanFreeze appends (or re-uses) the frozen version for a plan inside a
// record. Shared by the out-of-transaction CAS path (FreezeDeliveryPlan) and
// the in-transaction approval path (PreparePlanFreezeExtras), so versioning,
// idempotency and provenance rules cannot drift between them.
//
// Returns changed=false when the identical plan text is already the current
// frozen version: no new version, no write.
func applyPlanFreeze(record *FrozenPlanRecord, plan DeliveryPlan, approvedBy string, approval PlanApprovalProvenance) (bool, PlanVersionRecord, error) {
	if record == nil {
		return false, PlanVersionRecord{}, fmt.Errorf("apply plan freeze requires a record")
	}
	approvedBy = strings.TrimSpace(approvedBy)
	if approvedBy == "" {
		return false, PlanVersionRecord{}, fmt.Errorf("freeze delivery plan: approvedBy is required (an unapproved plan must never be frozen)")
	}
	plan.RunID = strings.TrimSpace(plan.RunID)
	plan.Version = 1 // version-agnostic text; the record owns versioning
	if err := ValidateDeliveryPlan(plan); err != nil {
		return false, PlanVersionRecord{}, err
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return false, PlanVersionRecord{}, err
	}
	if current, ok := record.Current(); ok && current.Status == PlanStatusFrozen && current.Digest == digest {
		return false, current, nil // identical text already frozen: no new version
	}
	supersedes := ""
	if current, ok := record.Current(); ok {
		supersedes = current.Digest
	}
	version := 1
	for _, v := range record.Versions {
		if v.Version >= version {
			version = v.Version + 1
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	stored := plan
	stored.Version = version
	entry := PlanVersionRecord{
		Version:          version,
		Status:           PlanStatusFrozen,
		Digest:           digest,
		ApprovedBy:       approvedBy,
		ApprovedAt:       now,
		FrozenAt:         now,
		SupersedesDigest: supersedes,
		Plan:             stored,
	}
	if approval.StepID != "" || approval.InstanceID != "" || approval.Comments != "" {
		entry.Approval = &PlanApprovalProvenance{
			StepID:     strings.TrimSpace(approval.StepID),
			InstanceID: strings.TrimSpace(approval.InstanceID),
			Comments:   strings.TrimSpace(approval.Comments),
		}
	}
	record.PlanID = strings.TrimSpace(plan.PlanID)
	record.CurrentVersion = version
	record.Versions = append(append([]PlanVersionRecord{}, record.Versions...), entry)
	return true, entry, nil
}

// PreparePlanFreezeExtras validates a plan and returns the transition extra
// writes that freeze it INSIDE the approving review's guarded transaction:
// same record CAS-free read (the transaction holds the IMMEDIATE lock), same
// versioning rules. Plan validation happens HERE, before the transition starts,
// so a bad plan fails closed without any run mutation.
func (s *Store) PreparePlanFreezeExtras(project, runID string, plan DeliveryPlan, approvedBy string, approval PlanApprovalProvenance) (TransitionExtraWrites, error) {
	project, runID = strings.TrimSpace(project), strings.TrimSpace(runID)
	if project == "" || runID == "" {
		return nil, fmt.Errorf("prepare plan freeze requires project and runID")
	}
	plan.RunID = runID
	plan.Version = 1
	if err := ValidateDeliveryPlan(plan); err != nil {
		return nil, err
	}
	if strings.TrimSpace(approvedBy) == "" {
		return nil, fmt.Errorf("freeze delivery plan: approvedBy is required (an unapproved plan must never be frozen)")
	}
	key := []string{project, runID}
	return func(tx controldb.KVTxReader) ([]controldb.KVWrite, error) {
		payload, found, err := tx.GetRecord(planRecordTable, s.workspaceID, key)
		if err != nil {
			return nil, err
		}
		record := FrozenPlanRecord{
			SchemaVersion: DeliveryPlanSchemaVersion,
			PlanID:        strings.TrimSpace(plan.PlanID),
			Project:       project,
			RunID:         runID,
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		if found {
			if err := json.Unmarshal([]byte(payload), &record); err != nil {
				return nil, fmt.Errorf("decode delivery plan record: %w", err)
			}
		}
		if _, _, err := applyPlanFreeze(&record, plan, approvedBy, approval); err != nil {
			return nil, err
		}
		record.SchemaVersion = DeliveryPlanSchemaVersion
		record.Project, record.RunID = project, runID
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		raw, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		return []controldb.KVWrite{{
			Table: planRecordTable, Workspace: s.workspaceID, Key: key, Payload: string(raw),
		}}, nil
	}, nil
}

// LoadFrozenPlanForRun is the ONLY trust path to a materializable plan.
//
// Fail-closed rules (each is a refusal, never a fallback to the static
// branch list):
//   - no record for the run            → found=false (caller decides; a stage
//     that REQUIRES a plan refuses, see slice 3);
//   - record without a current version → error;
//   - current version not frozen       → error;
//   - stored payload digest mismatch   → error (one-byte tamper case);
//   - stored plan not passing validate → error.
func (s *Store) LoadFrozenPlanForRun(project, runID string) (FrozenPlanRecord, PlanVersionRecord, bool, error) {
	record, found, err := s.LoadPlanRecord(project, runID)
	if err != nil || !found {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, err
	}
	if record.Project != "" && record.Project != strings.TrimSpace(project) {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan record %s/%s belongs to project %q", record.PlanID, record.RunID, record.Project)
	}
	if record.RunID != "" && record.RunID != strings.TrimSpace(runID) {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan record %s is pinned to run %q, not %q", record.PlanID, record.RunID, runID)
	}
	current, ok := record.Current()
	if !ok {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan record %s/%s has no current version %d", record.PlanID, record.RunID, record.CurrentVersion)
	}
	if current.Status != PlanStatusFrozen {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan %s v%d status %q is not frozen; materialization refuses unapproved plans", record.PlanID, current.Version, current.Status)
	}
	recomputed, err := PlanDigest(current.Plan)
	if err != nil {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, err
	}
	if recomputed != current.Digest {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan %s v%d digest mismatch: stored %s, recomputed %s (record tampered or corrupted); refusing to materialize", record.PlanID, current.Version, current.Digest, recomputed)
	}
	if err := ValidateDeliveryPlan(current.Plan); err != nil {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan %s v%d failed validation on read: %w", record.PlanID, current.Version, err)
	}
	if current.Plan.RunID != "" && current.Plan.RunID != strings.TrimSpace(runID) {
		return FrozenPlanRecord{}, PlanVersionRecord{}, false, fmt.Errorf("delivery plan %s v%d is pinned to run %q, not %q", record.PlanID, current.Version, current.Plan.RunID, runID)
	}
	return record, current, true, nil
}

// PlanClaim is the result of ClaimPlanWorkPackage.
type PlanClaim struct {
	WPID           string
	Claimed        bool
	Token          string
	ExistingTaskID string
}

// ClaimPlanWorkPackage atomically claims a work package for materialization.
//
// Two concurrent activations of the same stage (e.g. two branch completions
// racing the wave trigger) would otherwise both pass the "no instance yet"
// check and create two branch instances for ONE work package — a second
// identity, violating 1:1:1:1. The plan record is the CAS arbiter and admits
// exactly one live claimer per work package:
//
//   - no entry                        → claim (this attempt owns it);
//   - entry already materialized      → NOT ours, refused: a recorded task id
//     is never taken over (second identity);
//   - entry claimed, no task id, lease
//     not expired                     → NOT ours, refused (a sibling attempt is
//     mid-flight; forking would duplicate the identity);
//   - entry claimed, no task id, lease
//     expired                         → taken over (the previous claimant died
//     between claim and task creation; the next attempt must be able to
//     finish the work package).
//
// The returned token must be presented by RecordPlanMaterialization so the
// owner — and only the owner — can fill in the real task identity.
func (s *Store) ClaimPlanWorkPackage(project, runID, wpID string) (PlanClaim, error) {
	wpID = strings.TrimSpace(wpID)
	if wpID == "" {
		return PlanClaim{}, fmt.Errorf("claim plan work package requires a work package id")
	}
	token, err := planClaimToken()
	if err != nil {
		return PlanClaim{}, err
	}
	claim := PlanClaim{WPID: wpID, Token: token}
	_, err = s.mutatePlanRecord(project, runID, nil, func(record *FrozenPlanRecord) (bool, error) {
		now := time.Now().UTC()
		for i, entry := range record.Materializations {
			if entry.WPID != wpID || entry.BranchID != wpID {
				continue
			}
			if strings.TrimSpace(entry.TaskID) != "" {
				claim.Claimed, claim.ExistingTaskID = false, strings.TrimSpace(entry.TaskID)
				return false, nil
			}
			// Age the claim from its timestamp. A missing or corrupt timestamp
			// is NOT treated as expired: taking a claim over requires positive
			// evidence that the previous claimant is gone (its lease elapsed),
			// otherwise a storage fault would fork the work package identity.
			claimedAtRaw := strings.TrimSpace(entry.ClaimedAt)
			if claimedAtRaw == "" {
				claim.Claimed, claim.ExistingTaskID = false, ""
				return false, nil
			}
			claimedAt, perr := time.Parse(time.RFC3339, claimedAtRaw)
			if perr != nil {
				return false, fmt.Errorf("delivery plan work package %q has a corrupt claim timestamp %q; refusing to take the claim over", wpID, claimedAtRaw)
			}
			if now.Sub(claimedAt) <= planClaimLease {
				claim.Claimed, claim.ExistingTaskID = false, ""
				return false, nil
			}
			record.Materializations[i].ClaimToken = token
			record.Materializations[i].ClaimedAt = now.Format(time.RFC3339)
			record.Materializations[i].TaskID = ""
			claim.Claimed = true
			return true, nil
		}
		record.Materializations = append(append([]PlanMaterializationEntry{}, record.Materializations...), PlanMaterializationEntry{
			WPID: wpID, BranchID: wpID, ClaimToken: token, ClaimedAt: now.Format(time.RFC3339),
		})
		claim.Claimed = true
		return true, nil
	})
	if err != nil {
		return PlanClaim{}, err
	}
	return claim, nil
}

// planClaimToken returns a fresh opaque claim token.
func planClaimToken() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "claim-" + hex.EncodeToString(buf), nil
}

// RecordPlanMaterialization appends (idempotently) one materialization entry
// to the record via the CAS read-modify-write loop, so two branch
// completions appending DIFFERENT work packages concurrently both land.
//
// Re-driving the same work package re-writes the same identity; a conflicting
// re-materialization of the same work package with a DIFFERENT task id is
// refused (that would be a second identity for one work package, violating
// 1:1:1:1).
func (s *Store) RecordPlanMaterialization(project, runID string, entry PlanMaterializationEntry) (FrozenPlanRecord, error) {
	entry.WPID = strings.TrimSpace(entry.WPID)
	entry.BranchID = strings.TrimSpace(entry.BranchID)
	entry.DefinitionID = strings.TrimSpace(entry.DefinitionID)
	entry.DefinitionDigest = strings.TrimSpace(entry.DefinitionDigest)
	entry.TaskID = strings.TrimSpace(entry.TaskID)
	entry.ChildRunID = strings.TrimSpace(entry.ChildRunID)
	if entry.WPID == "" || entry.BranchID == "" {
		return FrozenPlanRecord{}, fmt.Errorf("record plan materialization requires wpId and branchId")
	}
	if entry.MaterializedAt == "" {
		entry.MaterializedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return s.mutatePlanRecord(project, runID, nil, func(record *FrozenPlanRecord) (bool, error) {
		for i, existing := range record.Materializations {
			if existing.WPID != entry.WPID || existing.BranchID != entry.BranchID {
				continue
			}
			if existing.ClaimToken != "" && entry.ClaimToken != "" && existing.ClaimToken != entry.ClaimToken {
				return false, fmt.Errorf("delivery plan work package %q is owned by another materialization attempt (%s); refusing to fill its identity", entry.WPID, existing.ClaimToken)
			}
			if existing.TaskID != "" && entry.TaskID != "" && existing.TaskID != entry.TaskID {
				return false, fmt.Errorf("delivery plan work package %q already materialized as task %s; refusing a second identity %s", entry.WPID, existing.TaskID, entry.TaskID)
			}
			if existing.ClaimToken != "" {
				entry.ClaimToken, entry.ClaimedAt = existing.ClaimToken, existing.ClaimedAt
			}
			// Same identity: refresh the volatile fields (child run id can
			// appear on a later re-drive) but keep the original
			// materializedAt.
			entry.MaterializedAt = existing.MaterializedAt
			record.Materializations[i] = entry
			return true, nil
		}
		record.Materializations = append(append([]PlanMaterializationEntry{}, record.Materializations...), entry)
		return true, nil
	})
}

// PlanMaterializationCount is a small read helper for diagnostics/tests.
func (s *Store) PlanMaterializationCount(project, runID string) (int, error) {
	record, found, err := s.LoadPlanRecord(project, runID)
	if err != nil || !found {
		return 0, err
	}
	return len(record.Materializations), nil
}

// planRecordRevisionString exposes the stored revision (diagnostics/tests).
func (s *Store) planRecordRevisionString(project, runID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("delivery plan lookup requires a control-plane store")
	}
	recs, err := s.db.ListRecordsWithRevision(planRecordTable, s.workspaceID, []string{project, runID})
	if err != nil || len(recs) == 0 {
		return "", false, err
	}
	return recs[0].Revision, true, nil
}

// planRecordRevisionInt is a convenience wrapper for tests that compare
// revisions numerically.
func (s *Store) planRecordRevisionInt(project, runID string) (int64, bool, error) {
	raw, ok, err := s.planRecordRevisionString(project, runID)
	if err != nil || !ok {
		return 0, ok, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}
