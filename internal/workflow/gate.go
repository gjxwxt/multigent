// Package workflow: platform gate registry.
//
// Single source of truth for "which steps does the platform own the decision
// on" (reduction plan Task 0.1). Before this file, the same question was
// answered by four scattered ID/title matchers (internal/api/workflow_handlers.go,
// internal/api/ci_ready_gate.go) plus an inline ID check in the store's
// output whitelist. Any rename could silently drop a gate.
//
// Resolution order per step:
//  1. Explicit marker: step.Config["platform_gate"] = <kind>. The declarative
//     contract; new templates must use it.
//  2. Legacy fallback: step-ID shape matching. Kept ONLY for definitions
//     instantiated before the marker existed — deletion is gated on the
//     Task 0.5 deprecation policy (no in-flight runs, full V2 coverage, two
//     iterations without new legacy instantiations), not on convenience.
package workflow

import (
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// GateConfigKey marks a workflow step whose route the platform decides, not
// the agent. Declaring it on the step (rather than hard-coding step IDs) is
// what keeps a gate from becoming prompt-shaped: the agent's own report is
// never an input to this decision.
const GateConfigKey = "platform_gate"

// Gate kinds.
const (
	GateKindCIReady    = "ci_ready"
	GateKindQASignoff  = "qa_signoff"
	GateKindDesignGate = "design_gate"
)

// PlatformGate identifies a platform-owned gate on a workflow step.
type PlatformGate struct {
	Kind string
}

// gateKindByMarker returns the gate declared via the explicit config marker.
// Matching is exact (after trim): the original consumer compared the raw
// config value to the kind constant case-sensitively, and equivalence with
// the pre-registry behavior is a hard requirement until Task 0.5 deprecates
// the fallbacks.
func gateKindByMarker(step entity.WorkflowStep) (string, bool) {
	if step.Config == nil {
		return "", false
	}
	kind := strings.TrimSpace(step.Config[GateConfigKey])
	switch kind {
	case GateKindCIReady, GateKindQASignoff, GateKindDesignGate:
		return kind, true
	default:
		return "", false
	}
}

// gateKindByLegacyID returns the gate inferred from step ID shape. This is the
// pre-marker fallback described in the package comment.
//
// Per-kind case semantics mirror the ORIGINAL consumers exactly:
//   - ci_ready: the original gate map matched raw step IDs exactly (no
//     lowercasing) — keep that here.
//   - qa_signoff / design_review: the original handler matchers lowercased
//     the ID before matching — keep that here.
func gateKindByLegacyID(step entity.WorkflowStep) (string, bool) {
	rawID := strings.TrimSpace(step.ID)
	if rawID == "" {
		return "", false
	}
	if rawID == "ci_ready" || rawID == "ci_ready_gate" {
		return GateKindCIReady, true
	}
	id := strings.ToLower(rawID)
	switch {
	case id == "qa_signoff" || strings.Contains(id, "qa_signoff"):
		return GateKindQASignoff, true
	case strings.Contains(id, "design_review"):
		return GateKindDesignGate, true
	default:
		return "", false
	}
}

// gateKindByLegacyDesignFlag keeps the designGate config flag working: it
// predates the unified marker and some templates still carry it.
func gateKindByLegacyDesignFlag(step entity.WorkflowStep) bool {
	return step.Config != nil && strings.EqualFold(strings.TrimSpace(step.Config["designGate"]), "true")
}

// GateForStep resolves the platform gate declared on a step.
func GateForStep(step entity.WorkflowStep) (PlatformGate, bool) {
	if kind, ok := gateKindByMarker(step); ok {
		return PlatformGate{Kind: kind}, true
	}
	if gateKindByLegacyDesignFlag(step) {
		return PlatformGate{Kind: GateKindDesignGate}, true
	}
	if kind, ok := gateKindByLegacyID(step); ok {
		return PlatformGate{Kind: kind}, true
	}
	return PlatformGate{}, false
}

// IsCIReadyGateStep reports whether the step is the platform's CI readiness
// gate (explicit marker or legacy ci_ready/ci_ready_gate ID).
func IsCIReadyGateStep(step entity.WorkflowStep) bool {
	gate, ok := GateForStep(step)
	return ok && gate.Kind == GateKindCIReady
}

// IsQASignoffStep reports whether the step is the platform's QA sign-off gate.
func IsQASignoffStep(step entity.WorkflowStep) bool {
	gate, ok := GateForStep(step)
	return ok && gate.Kind == GateKindQASignoff
}

// IsDesignGateStep reports whether the step is the platform's design gate
// (explicit marker, legacy designGate flag, or legacy design_review ID).
func IsDesignGateStep(step entity.WorkflowStep) bool {
	gate, ok := GateForStep(step)
	return ok && gate.Kind == GateKindDesignGate
}

// IsPullRequestReviewStep reports whether completing this step triggers
// terminal delivery side effects (merge/MR handling). This is delivery-behavior
// routing, not a platform gate: the platform does not own the decision, it
// just must run side effects in the right order.
//
// The title-substring match is the fragile shape the reduction plan flagged —
// renaming a step title drops the delivery preparation. It is kept because
// legacy definitions rely on it; new templates should declare
// platform_gate: <kind> on the gate steps AND rely on step IDs from the
// unified template. Falls back through the same resolution as the gates.
func IsPullRequestReviewStep(step entity.WorkflowStep) bool {
	id := strings.ToLower(strings.TrimSpace(step.ID))
	title := strings.ToLower(strings.TrimSpace(step.Title))
	return strings.Contains(id, "pr_review") ||
		strings.Contains(id, "mr_review") ||
		strings.Contains(id, "merge_and_sync") ||
		strings.Contains(id, "push_to_gitlab") ||
		strings.Contains(title, "pull request") ||
		strings.Contains(title, "merge request") ||
		strings.Contains(title, "merge and sync")
}

// Task 0.5: V2 delivery-contract deprecation line. Definitions at or above
// DefinitionVersionV2 must declare delivery steps explicitly (see
// StepConfigPlatformDelivery); the title/ID shape matching above is a legacy
// compatibility path only. Definitions below the line get marker-first
// resolution with the legacy matcher as fallback — identical behavior for
// existing definitions, no in-flight migration.
const (
	// StepConfigPlatformDelivery marks a step as a platform-managed delivery
	// step. Value "pr_review" means completing it triggers terminal delivery
	// side effects (merge/MR handling).
	StepConfigPlatformDelivery = "platform_delivery"
	// DeliveryKindPullRequestReview is the platform_delivery value for steps
	// whose completion triggers delivery preparation.
	DeliveryKindPullRequestReview = "pr_review"
	// DefinitionVersionV2 is the first definition version that requires
	// explicit platform_delivery / platform_gate declarations (no shape
	// guessing). Existing built-in definitions are version 1; bump only
	// together with the deprecation policy in
	// docs/workflow-definition-v2-deprecation.md.
	DefinitionVersionV2 = 2
)

// DeliveryForStep resolves the explicit platform_delivery marker, if any.
// Matching is exact after trim, mirroring the gate marker contract.
func DeliveryForStep(step entity.WorkflowStep) (string, bool) {
	if step.Config == nil {
		return "", false
	}
	kind := strings.TrimSpace(step.Config[StepConfigPlatformDelivery])
	if kind == "" {
		return "", false
	}
	return kind, true
}

// IsPullRequestReviewStepStrict reports V2 semantics: only an explicit
// platform_delivery: pr_review marker counts. No ID or title inference.
func IsPullRequestReviewStepStrict(step entity.WorkflowStep) bool {
	kind, ok := DeliveryForStep(step)
	return ok && kind == DeliveryKindPullRequestReview
}

// PullRequestReviewStepMatches resolves delivery routing for a step under a
// definition of the given version: marker first, then — only for pre-V2
// definitions — the legacy shape matcher. V2 definitions never fall back to
// shape guessing, which is the whole point of the version line.
func PullRequestReviewStepMatches(step entity.WorkflowStep, definitionVersion int) bool {
	if IsPullRequestReviewStepStrict(step) {
		return true
	}
	if definitionVersion >= DefinitionVersionV2 {
		return false
	}
	return IsPullRequestReviewStep(step)
}
