package workflow

// Delivery plan SOURCE parsing (slice 4): turning the batched path's agent
// outputs into the structured plan that the approving human review freezes.
//
// The template asks scale_gate for a structured plan (field `delivery_plan`,
// falling back to the historical `batch_plan`) plus a requirement-items
// snapshot from requirement_draft. Everything here is pure and fail-closed:
// a malformed plan, a missing requirement anchor, an unknown reference or a
// dependency cycle returns an error naming the offending field/value, which
// the API surfaces as a diagnosable 400/409 BEFORE any run mutation.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// planFreezeConfigKey marks the human review step whose approval freezes the
// delivery plan. Set by the template on contract_review; the API keys the
// freeze path on it (never on a hard-coded step id).
const PlanFreezeConfigKey = "planFreeze"

// planSourceJSON is the structured shape the batched template asks agents for.
// Field aliases are accepted so an agent following the historical batch_plan
// wording ("branch_id", "expected UC set") still produces a valid plan.
type planSourceJSON struct {
	PlanID            string               `json:"planId"`
	IntegrationPolicy string               `json:"integrationPolicy"`
	QAPolicy          string               `json:"qaPolicy"`
	SharedContract    []planSourceContract `json:"sharedContract"`
	WorkPackages      []planSourceWorkPack `json:"workPackages"`
}

type planSourceContract struct {
	ID       string `json:"id"`
	Artifact string `json:"artifact"`
	Path     string `json:"path"`
	Owner    string `json:"owner"`
	Evidence string `json:"evidence"`
}

type planSourceWorkPack struct {
	ID     string `json:"id"`
	Branch string `json:"branchId"`
	WPID   string `json:"wpId"`
	Title  string `json:"title"`
	// Domain/title aliases: template wording ("cohesive domain rationale").
	Domain    string `json:"domain"`
	Scope     string `json:"scope"`
	Details   string `json:"details"`
	Rationale string `json:"rationale"`

	DependsOn          []string `json:"dependsOn"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	UCs                []string `json:"ucs"`
	UseCases           []string `json:"useCases"`
	UCLegacy           []string `json:"use_cases"`

	AgentBinding     string   `json:"agentBinding"`
	ActorRole        string   `json:"actorRole"`
	ExpectedDelivery []string `json:"expectedDelivery"`
	ContractRefs     []string `json:"contractRefs"`
}

type planSourceRequirement struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source"`
	// Agent-friendliness aliases.
	Description string `json:"description"`
	Statement   string `json:"statement"`
}

// BuildDeliveryPlanFromRunOutputs assembles the plan to freeze from the run's
// own step outputs. Candidates are passed newest-producer-first; the first
// NON-EMPTY candidate is the plan under review and any defect in it is fatal —
// there is deliberately no "skip the broken newest plan and freeze an older
// one" fallback, because that would freeze a plan the reviewer never saw. Every
// failure names the field it came from.
func BuildDeliveryPlanFromRunOutputs(runID string, planJSONs, requirementItemJSONs []string) (DeliveryPlan, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return DeliveryPlan{}, fmt.Errorf("build delivery plan: runID is required")
	}
	var (
		source planSourceJSON
		parsed bool
		first  error
	)
	for _, raw := range planJSONs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var candidate planSourceJSON
		if err := json.Unmarshal([]byte(raw), &candidate); err != nil {
			// Fail-closed on the newest carrier: older candidates are stale
			// drafts, not a rescue path.
			return DeliveryPlan{}, fmt.Errorf("delivery plan JSON is not parseable (%v); payload starts with %q", err, snippet(raw))
		}
		source, parsed = candidate, true
		break
	}
	if !parsed {
		if first != nil {
			return DeliveryPlan{}, first
		}
		return DeliveryPlan{}, fmt.Errorf("delivery plan source is empty: the batched path requires a structured delivery_plan (or batch_plan) JSON output before the contract review can freeze it")
	}
	if len(source.WorkPackages) == 0 {
		return DeliveryPlan{}, fmt.Errorf("delivery plan carries no work packages: a batched plan must decompose the requirement into at least one work package")
	}

	plan := DeliveryPlan{
		SchemaVersion:     DeliveryPlanSchemaVersion,
		PlanID:            strings.TrimSpace(source.PlanID),
		Version:           1,
		RunID:             runID,
		IntegrationPolicy: strings.TrimSpace(source.IntegrationPolicy),
		QAPolicy:          strings.TrimSpace(source.QAPolicy),
	}
	if plan.PlanID == "" {
		plan.PlanID = "plan-" + runID
	}
	for _, item := range source.SharedContract {
		plan.SharedContract = append(plan.SharedContract, PlanContractItem{
			ID:       strings.TrimSpace(item.ID),
			Artifact: strings.TrimSpace(item.Artifact),
			Path:     strings.TrimSpace(item.Path),
			Owner:    strings.TrimSpace(item.Owner),
			Evidence: strings.TrimSpace(item.Evidence),
		})
	}
	for i, wp := range source.WorkPackages {
		id := firstNonEmptyString(wp.ID, wp.Branch, wp.WPID)
		if id == "" {
			return DeliveryPlan{}, fmt.Errorf("delivery plan work package #%d has no id (accepts id, branchId or wpId)", i+1)
		}
		acs := append([]string{}, wp.AcceptanceCriteria...)
		acs = append(acs, wp.UCs...)
		acs = append(acs, wp.UseCases...)
		acs = append(acs, wp.UCLegacy...)
		plan.WorkPackages = append(plan.WorkPackages, PlanWorkPackage{
			ID:                 id,
			Title:              strings.TrimSpace(wp.Title),
			Domain:             firstNonEmptyString(wp.Domain, wp.Scope),
			Details:            firstNonEmptyString(wp.Details, wp.Rationale),
			DependsOn:          wp.DependsOn,
			AcceptanceCriteria: acs,
			AgentBinding:       strings.TrimSpace(wp.AgentBinding),
			ActorRole:          strings.TrimSpace(wp.ActorRole),
			ExpectedDelivery:   wp.ExpectedDelivery,
			ContractRefs:       wp.ContractRefs,
		})
	}

	// Requirement snapshot: the anchor the human approval freezes. Without it a
	// work package's acceptance criteria cannot be traced back to the approved
	// requirement, so the plan is refused unless EVERY work package is
	// infrastructure-only (infra:* relaxes the requirement LINK, never the
	// identity cardinality).
	items, itemErr := parseRequirementItems(requirementItemJSONs)
	if itemErr != nil {
		// A malformed snapshot on the batched path is a contract breach, not a
		// soft miss: surface it instead of freezing a plan without anchors.
		return DeliveryPlan{}, itemErr
	}
	plan.RequirementItems = items
	if len(plan.RequirementItems) == 0 && planHasNonInfraAnchor(plan) {
		return DeliveryPlan{}, fmt.Errorf("delivery plan has no requirement-items snapshot: the batched path must carry the human-approved requirement anchors (requirement_items) so acceptance criteria can be traced; only a plan whose work packages are all %q may omit it", PlanInfraPrefix)
	}

	if err := ValidateDeliveryPlan(plan); err != nil {
		return DeliveryPlan{}, err
	}
	return plan, nil
}

// planHasNonInfraAnchor reports whether any work package references a
// requirement item (i.e. is not infrastructure-only).
func planHasNonInfraAnchor(plan DeliveryPlan) bool {
	for _, wp := range plan.WorkPackages {
		for _, ref := range wp.AcceptanceCriteria {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				// Blank refs are rejected by ValidateDeliveryPlan; they must
				// not make an infra-only package look requirement-linked here.
				continue
			}
			if !strings.HasPrefix(ref, PlanInfraPrefix) {
				return true
			}
		}
	}
	return false
}

// parseRequirementItems accepts the requirement snapshot as a JSON array or as
// {"items": [...]}. A candidate that is non-empty but malformed is an error
// (fail-closed); empty candidates are skipped.
func parseRequirementItems(candidates []string) ([]PlanRequirementItem, error) {
	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var direct []planSourceRequirement
		if err := json.Unmarshal([]byte(raw), &direct); err != nil {
			var wrapped struct {
				Items []planSourceRequirement `json:"items"`
			}
			if err2 := json.Unmarshal([]byte(raw), &wrapped); err2 != nil {
				return nil, fmt.Errorf("requirement_items is not parseable (%v); payload starts with %q", err2, snippet(raw))
			}
			direct = wrapped.Items
		}
		out := make([]PlanRequirementItem, 0, len(direct))
		for _, item := range direct {
			id := strings.TrimSpace(item.ID)
			text := firstNonEmptyString(item.Text, item.Description, item.Statement)
			if id == "" {
				return nil, fmt.Errorf("requirement_items contains an entry without an id; every anchor needs a stable id")
			}
			out = append(out, PlanRequirementItem{ID: id, Text: text, Source: strings.TrimSpace(item.Source)})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("requirement_items parsed to an empty anchor list")
		}
		return out, nil
	}
	return nil, nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func snippet(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 120 {
		return raw[:120] + "…"
	}
	return raw
}
