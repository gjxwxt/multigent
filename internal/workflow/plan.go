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

// Dependency edge kinds (dependsKind). Absent = PlanDepKindArtifact.
const (
	// PlanDepKindContract: the edge points at the shared contract baseline,
	// which is committed before the parallel stage starts and therefore
	// exists regardless of whether the upstream package executes. It takes
	// no part in progress propagation (no defer/block transmission).
	PlanDepKindContract = "contract"
	// PlanDepKindArtifact: the dependent needs the upstream deliverable; a
	// deferred upstream defers the dependent (pre-existing default).
	PlanDepKindArtifact = "artifact"
)

// DependsKindMap is the map form of PlanWorkPackage.DependsKind. It decodes
// from either the canonical map or the string shorthand; see the field doc.
type DependsKindMap map[string]string

// UnmarshalJSON accepts the map form {"wp-3":"contract"} and the string
// shorthand "contract"/"artifact" (every edge in DependsOn gets that kind).
// Any other JSON shape — including a string with an unknown kind — fails the
// decode: a silently-dropped dependsKind would silently restore defer
// propagation on what the plan author meant to be a contract edge.
func (m *DependsKindMap) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "null":
		*m = nil
		return nil
	case strings.HasPrefix(trimmed, "{"):
		var raw map[string]string
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		*m = raw
		return nil
	case strings.HasPrefix(trimmed, "\""):
		var kind string
		if err := json.Unmarshal(data, &kind); err != nil {
			return err
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind != PlanDepKindContract && kind != PlanDepKindArtifact {
			return fmt.Errorf("dependsKind %q (want %q or %q, either as a string or as a {workPackageId: kind} map)", kind, PlanDepKindContract, PlanDepKindArtifact)
		}
		// The per-edge expansion happens in canonicalDeliveryPlan, which sees
		// the whole work package (DependsOn included).
		*m = DependsKindMap{"*": kind}
		return nil
	default:
		return fmt.Errorf("dependsKind must be a string or an object, got %s", trimmed)
	}
}

// MarshalJSON always emits the canonical map form, so digests never see the
// shorthand and old binaries read back frozen records unchanged.
func (m DependsKindMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	return json.Marshal(map[string]string(m))
}

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
	// DependsKind maps a dependency work package id to the kind of the
	// dependency edge. "contract": the dependency only has to land its
	// contract artifact — the shared contract baseline is committed by the
	// contract batch step BEFORE the parallel stage starts, so it exists
	// regardless of whether the upstream package ever executes; deferring
	// (or failing) the upstream package must not defer (or block) the
	// dependent, and the edge takes no part in progress propagation at all.
	// "artifact": the dependent needs the real deliverable — a deferred
	// upstream defers the dependent. Absent key = "artifact" (pre-existing
	// semantics). Keys must reference ids already listed in DependsOn
	// (validated); the field is canonicalized only when non-empty, so plans
	// written before it existed keep their exact digest.
	//
	// The JSON form is union: the canonical map form {"wp-3":"contract"},
	// or the string shorthand "contract" / "artifact" (models reliably emit
	// this — live-fire round t-20261002-6l65om), meaning EVERY edge in
	// DependsOn has that kind. An unrecognized shape fails the decode (it
	// must never be silently dropped — a lost contract edge silently
	// restores defer propagation). See UnmarshalJSON below.
	DependsKind DependsKindMap `json:"dependsKind,omitempty"`
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
	// Deferred: the package itself was deferred by a human decision, or
	// transitively depends on a deferred package. Deferred is a HUMAN
	// terminal ruling ("approved out of this round") derived from a failed
	// branch — unlike failed/skipped it does not block the stage: guards
	// treat Deferred as settled. The transitive derivation is deterministic
	// (recomputed from the DAG on every evaluation, never stored), so no
	// manual cascading of the ruling onto dependents is needed or possible.
	Deferred []string
}

// DeferredJoinRuling is the over-deferral gate's verdict for a plan whose
// stage is otherwise settled: a deferral wave this large means the ROUND's
// plan itself failed, not just isolated packages.
type DeferredJoinRuling struct {
	// Blocked reports the join must be refused even though every package is
	// settled.
	Blocked bool
	// Reason is a human-readable, actionable refusal text (empty when not
	// blocked).
	Reason string
}

// PlanWPState values accepted by EvaluatePlanProgress. Empty string means
// "not materialized yet".
const (
	PlanWPStateRunning   = "running"
	PlanWPStateCompleted = "completed"
	PlanWPStateFailed    = "failed"
	PlanWPStateSkipped   = "skipped"
	// PlanWPStateDeferred marks a work package a human ruled out of this
	// round. It is a terminal state recorded on the branch instance; unlike
	// failed/skipped it does NOT poison dependents into Blocked — they defer
	// too. A deferred package still shows in the acceptance surface with its
	// original failure reason; dropping that history would let integration
	// review mistake "human waived" for "never attempted".
	PlanWPStateDeferred = "deferred"
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
		// dependsKind is canonicalized only when non-empty: an absent map on
		// the pre-field plans must serialize to nothing at all (omitempty on
		// a nil map drops the key), keeping their canonical JSON — and thus
		// their frozen digest — byte-for-byte identical across the upgrade.
		if len(cp.DependsKind) > 0 {
			kinds := make(map[string]string, len(cp.DependsKind))
			for dep, kind := range cp.DependsKind {
				dep = strings.TrimSpace(dep)
				kind = strings.ToLower(strings.TrimSpace(kind))
				if dep == "" || kind == "" {
					continue
				}
				kinds[dep] = kind
			}
			// String-shorthand decode ("contract") lands on the "*" key:
			// expand it to every declared edge. With no declared edges the
			// shorthand is meaningless — validation rejects it below.
			if wildcard, ok := kinds["*"]; ok && len(cp.DependsKind) == 1 {
				delete(kinds, "*")
				for _, dep := range cp.DependsOn {
					dep = strings.TrimSpace(dep)
					if dep != "" {
						kinds[dep] = wildcard
					}
				}
			}
			if len(kinds) == 0 {
				cp.DependsKind = nil
			} else {
				cp.DependsKind = kinds
			}
		}
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
		dependsOn := map[string]bool{}
		for _, dep := range wp.DependsOn {
			dependsOn[strings.TrimSpace(dep)] = true
		}
		for dep, kind := range wp.DependsKind {
			kind = strings.ToLower(strings.TrimSpace(kind))
			if dep == "*" {
				// String-shorthand form ("contract") as decoded from source:
				// every declared edge carries the kind. Validate the shorthand
				// against the declared edges here rather than rejecting it —
				// the freeze path validates the RAW parsed plan before
				// canonicalization ever expands the wildcard (live fire,
				// wfr-kwm2nckn: a valid shorthand plan died at approve). A
				// shorthand with no dependsOn has nothing to apply to.
				if len(wp.DependsOn) == 0 {
					return fmt.Errorf("delivery plan work package %q declares dependsKind %q as a bare shorthand with no dependsOn to expand it onto", id, kind)
				}
				for _, dep2 := range wp.DependsOn {
					dep2 = strings.TrimSpace(dep2)
					if dep2 == "" {
						continue
					}
					if kind != PlanDepKindContract && kind != PlanDepKindArtifact {
						return fmt.Errorf("delivery plan work package %q declares dependsKind %q for %q (want %q or %q)", id, kind, dep2, PlanDepKindContract, PlanDepKindArtifact)
					}
				}
				continue
			}
			if kind != PlanDepKindContract && kind != PlanDepKindArtifact {
				return fmt.Errorf("delivery plan work package %q declares dependsKind %q for %q (want %q or %q)", id, kind, dep, PlanDepKindContract, PlanDepKindArtifact)
			}
			if !dependsOn[strings.TrimSpace(dep)] {
				return fmt.Errorf("delivery plan work package %q declares dependsKind for %q which is not in its dependsOn", id, dep)
			}
		}
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
	kindOf := map[string]map[string]string{}
	for _, wp := range plan.WorkPackages {
		id := strings.TrimSpace(wp.ID)
		deps[id] = sortedTrimmed(wp.DependsOn)
		if len(wp.DependsKind) > 0 {
			kinds := make(map[string]string, len(wp.DependsKind))
			for dep, kind := range wp.DependsKind {
				dep = strings.TrimSpace(dep)
				kind = strings.ToLower(strings.TrimSpace(kind))
				if dep == "*" {
					// String-shorthand form ("contract") as stored pre-
					// canonicalization: every declared edge gets the kind.
					// (canonicalDeliveryPlan expands it; plans read back from
					// records frozen before that expansion need the same
					// semantics here — never a silently-dropped edge kind.)
					for _, dep2 := range deps[id] {
						kinds[dep2] = kind
					}
					continue
				}
				kinds[dep] = kind
			}
			kindOf[id] = kinds
		}
	}
	state := func(id string) string {
		return strings.TrimSpace(strings.ToLower(states[id]))
	}
	unsatisfiable := map[string]bool{}
	deferredSet := map[string]bool{}
	// Wave order guarantees dependencies are classified before dependents.
	for _, wave := range waves {
		for _, id := range wave {
			blocked := false
			waiting := false
			for _, dep := range deps[id] {
				// A contract edge takes no part in propagation at all: the
				// shared contract baseline predates the stage (committed by
				// the contract batch step), so the upstream package's fate —
				// deferred, failed, or never materialized — cannot touch the
				// dependent. Filtering HERE, at the loop head, covers both
				// transmission sites below (the direct deferred edge and the
				// transitive deferredSet/block fallthrough) with one gate.
				if kindOf[id][dep] == PlanDepKindContract {
					continue
				}
				switch state(dep) {
				case PlanWPStateCompleted:
				case PlanWPStateDeferred:
					deferredSet[id] = true
				case PlanWPStateFailed, PlanWPStateSkipped:
					blocked = true
				default:
					if unsatisfiable[dep] {
						blocked = true
					} else if deferredSet[dep] {
						// A deferred dependency defers the dependent too —
						// deterministic propagation, no manual cascade.
						deferredSet[id] = true
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
			case PlanWPStateDeferred:
				deferredSet[id] = true
				progress.Deferred = append(progress.Deferred, id)
			case PlanWPStateFailed, PlanWPStateSkipped:
				unsatisfiable[id] = true
				progress.Blocked = append(progress.Blocked, id)
			case PlanWPStateRunning:
				progress.InProgress = append(progress.InProgress, id)
			default:
				if deferredSet[id] {
					progress.Deferred = append(progress.Deferred, id)
				} else if waiting {
					progress.Waiting = append(progress.Waiting, id)
				} else {
					progress.Ready = append(progress.Ready, id)
				}
			}
		}
	}
	for _, s := range [][]string{progress.Ready, progress.Waiting, progress.InProgress, progress.Completed, progress.Blocked, progress.Deferred} {
		sort.Strings(s)
	}
	return progress, nil
}

// deferredJoinRatioThreshold: when MORE than half of the plan's work packages
// are human-deferred, the deferral is not an isolated waiver but a verdict on
// this round's plan itself — integration review must not receive a stage
// built from a minority of its packages. Strictly greater: deferring exactly
// half still joins.
const deferredJoinRatioThreshold = 0.5

// EvaluateDeferredJoinRuling is the over-deferral gate (pure function): a
// stage whose packages are all settled may STILL refuse the join when
// (a) strictly more than half the packages deferred, or (b) some requirement
// item lost its entire delivery surface — every package referencing it is
// deferred (guarded against the vacuous truth of a zero-reference item).
// Unlike the settled/unfinished checks this does NOT heal by waiting: the
// deferred ratio never drops on its own, so the ruling carries the escape
// hatches in its reason text (retry the deferred work, or abandon the round).
func EvaluateDeferredJoinRuling(plan DeliveryPlan, progress PlanProgress) DeferredJoinRuling {
	total := len(plan.WorkPackages)
	if total == 0 || len(progress.Deferred) == 0 {
		return DeferredJoinRuling{}
	}
	if float64(len(progress.Deferred))/float64(total) > deferredJoinRatioThreshold {
		return DeferredJoinRuling{
			Blocked: true,
			Reason: sprintfDeferredRuling(len(progress.Deferred), total, progress.Deferred),
		}
	}
	// Requirement-surface check: item is "covered" when it has at least one
	// referencing package and every referencing package is deferred. A
	// requirement item nobody references cannot be "covered" by deferral —
	// the vacuous-truth guard keeps an orphan item from blocking every join.
	deferredSet := make(map[string]bool, len(progress.Deferred))
	for _, id := range progress.Deferred {
		deferredSet[id] = true
	}
	for _, item := range plan.RequirementItems {
		referencing := 0
		deferredRefs := 0
		for _, wp := range plan.WorkPackages {
			references := false
			for _, ref := range wp.AcceptanceCriteria {
				if strings.TrimSpace(ref) == strings.TrimSpace(item.ID) {
					references = true
					break
				}
			}
			if !references {
				continue
			}
			referencing++
			if deferredSet[strings.TrimSpace(wp.ID)] {
				deferredRefs++
			}
		}
		if referencing > 0 && referencing == deferredRefs {
			return DeferredJoinRuling{
				Blocked: true,
				Reason: sprintfRequirementRuling(item.ID, item.Text, referencing, len(progress.Deferred), total),
			}
		}
	}
	return DeferredJoinRuling{}
}

func sprintfDeferredRuling(deferred, total int, ids []string) string {
	return fmt.Sprintf("delivery plan: %d of %d work packages deferred (>50%%: %s); join blocked — retry the deferred work or abandon this round and re-plan (a new plan version requires human re-approval)",
		deferred, total, strings.Join(ids, ", "))
}

func sprintfRequirementRuling(itemID, itemText string, refs, deferred, total int) string {
	text := strings.TrimSpace(itemText)
	if len(text) > 80 {
		text = text[:80] + "…"
	}
	return fmt.Sprintf("delivery plan: requirement item %q (%s) lost its entire delivery surface — all %d package(s) referencing it are deferred; join blocked — retry the deferred work or abandon this round and re-plan (a new plan version requires human re-approval)",
		itemID, text, refs)
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
