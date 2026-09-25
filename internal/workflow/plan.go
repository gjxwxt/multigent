package workflow

// Delivery plan materialization (minimal closed loop, slice 1 of 3).
//
// A delivery plan is the structured, human-approved decomposition of an
// approved requirement into work packages with an explicit dependency DAG.
// It is the SINGLE SOURCE OF TRUTH for run-time branch materialization: the
// parallel stage no longer fans out over static template branches when a
// frozen plan exists for the run, it derives one branch per READY work
// package from the frozen record.
//
// This file is deliberately pure: validation, canonical JSON, digest, wave
// derivation and the ready/blocked progress evaluation. Persistence lives in
// plan_store.go (freeze + tamper-evident reads); the API layer (slice 3)
// owns materialization.
//
// TRUST MODEL
//   - The plan is a control-plane artifact. It is only trusted when it comes
//     from a FROZEN record whose digest verifies (plan_store.go); nothing is
//     ever derived from agent prose, run inputs or canvas state at
//     materialization time.
//   - The digest covers every material field (requirement snapshot, work
//     packages, dependencies, acceptance references, bindings, policies) so
//     a one-byte edit of a stored plan is refused, not silently accepted.
//   - Work package ids become git branch name components, so they must
//     already be sanitized (lowercase alphanumeric + '-'/'_').
//
// INVARIANTS this layer must keep (see the design doc §8):
//   - Dependencies affect ORDER only. They never gate the join, never skip a
//     work package, and never rewrite the plan.
//   - infra:* acceptance references relax the REQUIREMENT LINKAGE of a work
//     package, not its identity cardinality: every work package — infra
//     included — obeys 1:1:1:1 (one task, one branch, one worktree, one QA
//     baseline record). See ValidateDeliveryPlan.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/gitworktree"
)

// DeliveryPlanSchemaVersion is bumped on incompatible plan format changes.
// The schema version is part of the digest: an old reader refuses a newer
// plan instead of mis-materializing it.
const DeliveryPlanSchemaVersion = 1

// PlanStatusCode counts plan lifecycle states. draft plans are never
// persisted as trusted records; only frozen ones are materializable.
const (
	PlanStatusDraft  = "draft"
	PlanStatusFrozen = "frozen"
)

// PlanInfraPrefix marks acceptance criteria that are infrastructure work
// (no requirement item linkage) — e.g. "infra:shared-migration-runner".
const PlanInfraPrefix = "infra:"

// DeliveryPlan is the frozen decomposition artifact.
type DeliveryPlan struct {
	SchemaVersion int    `json:"schemaVersion"`
	PlanID        string `json:"planId"`
	Version       int    `json:"version"`
	// RunID is pinned by the store BEFORE the digest is computed, so a frozen
	// record and its digest can never disagree about which run they belong
	// to (FreezeDeliveryPlan validates it is set).
	RunID string `json:"runId,omitempty"`
	// ResolvedAtBaseCommit is informational: the run's base commit is still
	// resolved once and pinned by the materialization path. Freezing a plan
	// never re-baselines code.
	ResolvedAtBaseCommit string `json:"resolvedAtBaseCommit,omitempty"`
	// RequirementItems is the SNAPSHOT of the requirement items the plan was
	// approved against. This is what makes requirement item ids stable for
	// the lifetime of the frozen plan: the human approval locks this snapshot
	// (and the digest covers it), so an upstream renaming after approval
	// cannot silently re-point a work package's acceptance criteria.
	RequirementItems  []PlanRequirementItem `json:"requirementItems,omitempty"`
	SharedContract    []PlanContractItem    `json:"sharedContract,omitempty"`
	WorkPackages      []PlanWorkPackage     `json:"workPackages"`
	IntegrationPolicy string                `json:"integrationPolicy,omitempty"`
	QAPolicy          string                `json:"qaPolicy,omitempty"`
}

// PlanRequirementItem is one anchor of the requirement snapshot.
type PlanRequirementItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

// PlanContractItem is one shared-contract artifact every workstream depends
// on (schema, error codes, API skeleton, ...). Produced/committed by the
// contract batch step; referenced by work packages here.
type PlanContractItem struct {
	ID       string `json:"id"`
	Artifact string `json:"artifact"`
	Path     string `json:"path,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// PlanWorkPackage is one materializable unit of delivery.
type PlanWorkPackage struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Domain  string `json:"domain,omitempty"`
	Details string `json:"details,omitempty"`
	// DependsOn lists work package ids that must COMPLETE (successfully)
	// before this one may be materialized and dispatched.
	DependsOn []string `json:"dependsOn,omitempty"`
	// AcceptanceCriteria references requirement item ids, or "infra:<name>"
	// for infrastructure packages. At least one entry is required.
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	// AgentBinding is the workspace agent that owns this package. Plan
	// validation refuses a package with neither AgentBinding nor ActorRole:
	// a materialization that cannot resolve an actor deadlocks the stage
	// (round-5 D-A lesson).
	AgentBinding string `json:"agentBinding,omitempty"`
	ActorRole    string `json:"actorRole,omitempty"`
	// ExpectedDelivery is the declared path surface (advisory: used for
	// overlap warnings and for the acceptance table, never as a gate).
	ExpectedDelivery []string `json:"expectedDelivery,omitempty"`
	ContractRefs     []string `json:"contractRefs,omitempty"`
}

// PlanProgress is the deterministic evaluation of a plan against observed
// work package states.
type PlanProgress struct {
	// Ready: dependencies all completed, package not yet materialized.
	Ready []string
	// Waiting: not materialized, at least one dependency neither completed
	// nor failed.
	Waiting []string
	// InProgress: materialized and running.
	InProgress []string
	// Completed: materialized and completed successfully.
	Completed []string
	// Blocked: the package itself failed/skipped, or transitively depends on
	// a failed/skipped package. Blocked is VISIBLE on purpose: a failure
	// upstream must never look like "nothing to do".
	Blocked []string
}

// PlanWPState values accepted by EvaluatePlanProgress. Empty string means
// "not materialized yet".
const (
	PlanWPStateRunning   = "running"
	PlanWPStateCompleted = "completed"
	PlanWPStateFailed    = "failed"
	PlanWPStateSkipped   = "skipped"
)

// ---------------------------------------------------------------------------
// Canonical form + digest
// ---------------------------------------------------------------------------

// canonicalDeliveryPlan returns a deep, canonically ordered copy of the plan.
// Canonicalization rules (mirrored by the digest test): strings trimmed,
// slices sorted by id / value, schema version pinned to the current value.
// Struct field order is fixed by the type, so json.Marshal over this copy is
// byte-stable.
func canonicalDeliveryPlan(plan DeliveryPlan) DeliveryPlan {
	out := plan
	out.SchemaVersion = DeliveryPlanSchemaVersion
	// The plan TEXT is version-agnostic: the frozen record owns versioning
	// (every re-approval of edited text becomes version+1). Pinning the
	// canonical version to 1 keeps a version bump from changing the digest, so
	// "same text, already frozen" stays idempotent and a digest always
	// identifies plan CONTENT.
	out.Version = 1
	out.PlanID = strings.TrimSpace(out.PlanID)
	out.RunID = strings.TrimSpace(out.RunID)
	out.ResolvedAtBaseCommit = strings.TrimSpace(out.ResolvedAtBaseCommit)
	out.IntegrationPolicy = strings.TrimSpace(out.IntegrationPolicy)
	out.QAPolicy = strings.TrimSpace(out.QAPolicy)

	out.RequirementItems = append([]PlanRequirementItem{}, plan.RequirementItems...)
	for i := range out.RequirementItems {
		out.RequirementItems[i].ID = strings.TrimSpace(out.RequirementItems[i].ID)
		out.RequirementItems[i].Text = strings.TrimSpace(out.RequirementItems[i].Text)
		out.RequirementItems[i].Source = strings.TrimSpace(out.RequirementItems[i].Source)
	}
	sort.SliceStable(out.RequirementItems, func(i, j int) bool {
		return out.RequirementItems[i].ID < out.RequirementItems[j].ID
	})

	out.SharedContract = append([]PlanContractItem{}, plan.SharedContract...)
	for i := range out.SharedContract {
		out.SharedContract[i].ID = strings.TrimSpace(out.SharedContract[i].ID)
		out.SharedContract[i].Artifact = strings.TrimSpace(out.SharedContract[i].Artifact)
		out.SharedContract[i].Path = strings.TrimSpace(out.SharedContract[i].Path)
		out.SharedContract[i].Owner = strings.TrimSpace(out.SharedContract[i].Owner)
		out.SharedContract[i].Evidence = strings.TrimSpace(out.SharedContract[i].Evidence)
	}
	sort.SliceStable(out.SharedContract, func(i, j int) bool {
		return out.SharedContract[i].ID < out.SharedContract[j].ID
	})

	out.WorkPackages = make([]PlanWorkPackage, 0, len(plan.WorkPackages))
	for _, wp := range plan.WorkPackages {
		cp := wp
		cp.ID = strings.TrimSpace(cp.ID)
		cp.Title = strings.TrimSpace(cp.Title)
		cp.Domain = strings.TrimSpace(cp.Domain)
		cp.Details = strings.TrimSpace(cp.Details)
		cp.AgentBinding = strings.TrimSpace(cp.AgentBinding)
		cp.ActorRole = strings.TrimSpace(cp.ActorRole)
		cp.DependsOn = sortedTrimmed(cp.DependsOn)
		cp.AcceptanceCriteria = sortedTrimmed(cp.AcceptanceCriteria)
		cp.ExpectedDelivery = sortedTrimmed(cp.ExpectedDelivery)
		cp.ContractRefs = sortedTrimmed(cp.ContractRefs)
		out.WorkPackages = append(out.WorkPackages, cp)
	}
	sort.SliceStable(out.WorkPackages, func(i, j int) bool {
		return out.WorkPackages[i].ID < out.WorkPackages[j].ID
	})
	return out
}

func sortedTrimmed(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// CanonicalDeliveryPlanJSON returns the byte-stable canonical form of a plan.
func CanonicalDeliveryPlanJSON(plan DeliveryPlan) ([]byte, error) {
	return json.Marshal(canonicalDeliveryPlan(plan))
}

// PlanDigest returns the canonical SHA-256 hex digest of a plan. It covers
// the requirement snapshot and every material field: a one-byte edit of any
// covered value changes the digest, and readers refuse on mismatch.
func PlanDigest(plan DeliveryPlan) (string, error) {
	payload, err := CanonicalDeliveryPlanJSON(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// ---------------------------------------------------------------------------
// Validation (fail-closed, deterministic error text)
// ---------------------------------------------------------------------------

// ValidateDeliveryPlan refuses a plan the materialization path must not
// trust. Errors are deterministic: validation walks sorted ids so the first
// reported problem is stable for a given plan.
func ValidateDeliveryPlan(plan DeliveryPlan) error {
	if plan.SchemaVersion != DeliveryPlanSchemaVersion {
		return fmt.Errorf("delivery plan schemaVersion = %d, want %d", plan.SchemaVersion, DeliveryPlanSchemaVersion)
	}
	if strings.TrimSpace(plan.PlanID) == "" {
		return fmt.Errorf("delivery plan requires a planId")
	}
	if plan.Version < 1 {
		return fmt.Errorf("delivery plan %q requires version >= 1", plan.PlanID)
	}
	if len(plan.WorkPackages) == 0 {
		return fmt.Errorf("delivery plan %q has no work packages", plan.PlanID)
	}

	reqIDs := map[string]bool{}
	for _, item := range plan.RequirementItems {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return fmt.Errorf("delivery plan requirement item with empty id")
		}
		if reqIDs[id] {
			return fmt.Errorf("delivery plan requirement item %q is duplicated", id)
		}
		if strings.TrimSpace(item.Text) == "" {
			return fmt.Errorf("delivery plan requirement item %q has no text", id)
		}
		reqIDs[id] = true
	}

	contractIDs := map[string]bool{}
	for _, item := range plan.SharedContract {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return fmt.Errorf("delivery plan shared contract entry with empty id")
		}
		if contractIDs[id] {
			return fmt.Errorf("delivery plan shared contract %q is duplicated", id)
		}
		contractIDs[id] = true
	}

	wpIDs := map[string]bool{}
	for _, wp := range plan.WorkPackages {
		id := strings.TrimSpace(wp.ID)
		if id == "" {
			return fmt.Errorf("delivery plan work package with empty id")
		}
		if id != gitworktree.SanitizeTaskID(id) {
			return fmt.Errorf("delivery plan work package id %q is not sanitized (lowercase alphanumeric, '-' or '_' only)", id)
		}
		if wpIDs[id] {
			return fmt.Errorf("delivery plan work package %q is duplicated", id)
		}
		wpIDs[id] = true
		if strings.TrimSpace(wp.Title) == "" {
			return fmt.Errorf("delivery plan work package %q has no title", id)
		}
		if len(wp.AcceptanceCriteria) == 0 {
			return fmt.Errorf("delivery plan work package %q declares no acceptance criteria (reference a requirement item id or use the %q prefix)", id, PlanInfraPrefix)
		}
		if strings.TrimSpace(wp.AgentBinding) == "" && strings.TrimSpace(wp.ActorRole) == "" {
			return fmt.Errorf("delivery plan work package %q has neither agentBinding nor actorRole; a materialized branch with no resolvable actor deadlocks the stage", id)
		}
	}

	// Second pass: cross-references (ids are unique by now).
	for _, wp := range plan.WorkPackages {
		id := strings.TrimSpace(wp.ID)
		for _, dep := range wp.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == id {
				return fmt.Errorf("delivery plan work package %q depends on itself", id)
			}
			if !wpIDs[dep] {
				return fmt.Errorf("delivery plan work package %q depends on unknown work package %q", id, dep)
			}
		}
		for _, ref := range wp.AcceptanceCriteria {
			ref = strings.TrimSpace(ref)
			if strings.HasPrefix(ref, PlanInfraPrefix) {
				if strings.TrimSpace(strings.TrimPrefix(ref, PlanInfraPrefix)) == "" {
					return fmt.Errorf("delivery plan work package %q has an empty %q acceptance reference", id, PlanInfraPrefix)
				}
				continue
			}
			if !reqIDs[ref] {
				return fmt.Errorf("delivery plan work package %q references unknown requirement item %q", id, ref)
			}
		}
		for _, ref := range wp.ContractRefs {
			ref = strings.TrimSpace(ref)
			if !contractIDs[ref] {
				return fmt.Errorf("delivery plan work package %q references unknown shared contract %q", id, ref)
			}
		}
	}

	if _, err := PlanWaves(plan); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave derivation + progress evaluation
// ---------------------------------------------------------------------------

// PlanWaves derives the delivery waves from the dependency DAG: wave(k) is
// the set of work packages whose dependencies all live in earlier waves.
// Batches are NEVER stored on the plan — they are derived here, so the
// execution order can never drift from the approved dependencies.
func PlanWaves(plan DeliveryPlan) ([][]string, error) {
	remaining := map[string]bool{}
	deps := map[string][]string{}
	for _, wp := range plan.WorkPackages {
		id := strings.TrimSpace(wp.ID)
		if id == "" {
			return nil, fmt.Errorf("delivery plan work package with empty id")
		}
		if remaining[id] {
			return nil, fmt.Errorf("delivery plan work package %q is duplicated", id)
		}
		remaining[id] = true
		deps[id] = sortedTrimmed(wp.DependsOn)
	}
	var waves [][]string
	for len(remaining) > 0 {
		var wave []string
		for id := range remaining {
			ready := true
			for _, dep := range deps[id] {
				if remaining[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			cycle := make([]string, 0, len(remaining))
			for id := range remaining {
				cycle = append(cycle, id)
			}
			sort.Strings(cycle)
			return nil, fmt.Errorf("delivery plan dependency cycle among work packages %s", strings.Join(cycle, ", "))
		}
		sort.Strings(wave)
		waves = append(waves, wave)
		for _, id := range wave {
			delete(remaining, id)
		}
	}
	return waves, nil
}

// EvaluatePlanProgress classifies every work package against observed states
// (branch instance status per work package id; "" = not materialized).
//
// Semantics that callers rely on:
//   - Ready is the materialization set for the next wave. It is empty while
//     any dependency is still running.
//   - A failed/skipped package blocks every transitive dependent, and the
//     blocked set is returned explicitly so the stage can surface it instead
//     of silently stalling.
//   - A dependency cycle is a validation error here too (fail-closed).
func EvaluatePlanProgress(plan DeliveryPlan, states map[string]string) (PlanProgress, error) {
	var progress PlanProgress
	waves, err := PlanWaves(plan)
	if err != nil {
		return progress, err
	}
	deps := map[string][]string{}
	for _, wp := range plan.WorkPackages {
		deps[strings.TrimSpace(wp.ID)] = sortedTrimmed(wp.DependsOn)
	}
	state := func(id string) string {
		return strings.TrimSpace(strings.ToLower(states[id]))
	}
	unsatisfiable := map[string]bool{}
	// Wave order guarantees dependencies are classified before dependents.
	for _, wave := range waves {
		for _, id := range wave {
			blocked := false
			waiting := false
			for _, dep := range deps[id] {
				switch state(dep) {
				case PlanWPStateCompleted:
				case PlanWPStateFailed, PlanWPStateSkipped:
					blocked = true
				default:
					if unsatisfiable[dep] {
						blocked = true
					} else {
						waiting = true
					}
				}
			}
			if blocked {
				unsatisfiable[id] = true
				progress.Blocked = append(progress.Blocked, id)
				continue
			}
			switch state(id) {
			case PlanWPStateCompleted:
				progress.Completed = append(progress.Completed, id)
			case PlanWPStateFailed, PlanWPStateSkipped:
				unsatisfiable[id] = true
				progress.Blocked = append(progress.Blocked, id)
			case PlanWPStateRunning:
				progress.InProgress = append(progress.InProgress, id)
			default:
				if waiting {
					progress.Waiting = append(progress.Waiting, id)
				} else {
					progress.Ready = append(progress.Ready, id)
				}
			}
		}
	}
	for _, s := range [][]string{progress.Ready, progress.Waiting, progress.InProgress, progress.Completed, progress.Blocked} {
		sort.Strings(s)
	}
	return progress, nil
}

// PlanWaveIndex returns the 0-based wave index of a work package, or -1.
func PlanWaveIndex(plan DeliveryPlan, wpID string) int {
	waves, err := PlanWaves(plan)
	if err != nil {
		return -1
	}
	wpID = strings.TrimSpace(wpID)
	for i, wave := range waves {
		for _, id := range wave {
			if id == wpID {
				return i
			}
		}
	}
	return -1
}
