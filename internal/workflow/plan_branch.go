package workflow

// Plan-derived stage branches (shared by the API materialization path and the
// store's branch-report resolution).
//
// Why this file exists: a plan-driven parallel stage derives its branch list
// at RUN time from the frozen plan. Earlier that derivation also wrote the
// derived branches into the run's definition snapshot, which is a
// read-modify-write on a record that concurrent branch completions also
// touch — a lost-update surface. The branch definition is fully derivable
// from the frozen plan (work package + the stage's declared field contract),
// so nothing needs to be written: the branch-report path resolves a branch
// from the plan with the SAME helper the materialization path uses. One
// source of truth, no shared-state mutation.

import (
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// PlanBranchFieldTemplate returns the input/output field contract a
// plan-derived branch inherits: the stage's own declared branch fields when
// it has any (the template's generic branch, e.g. branch_summary /
// touched_paths), otherwise the canonical pair. Both the materialization
// path and the report-resolution path use this, so a branch can never be
// materialized with one contract and judged against another.
func PlanBranchFieldTemplate(step entity.WorkflowStep) (in []entity.WorkflowField, out []entity.WorkflowField) {
	for _, candidate := range step.Branches {
		if len(candidate.OutputFields) > 0 {
			return append([]entity.WorkflowField{}, candidate.InputFields...), append([]entity.WorkflowField{}, candidate.OutputFields...)
		}
	}
	return nil, []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}}
}

// PlanBranchDescription renders the approved scope of a work package into the
// branch description the agent receives.
func PlanBranchDescription(plan DeliveryPlan, wp PlanWorkPackage) string {
	var b strings.Builder
	b.WriteString("Approved delivery plan work package ")
	b.WriteString(wp.ID)
	if wp.Domain != "" {
		b.WriteString(" (domain: ")
		b.WriteString(wp.Domain)
		b.WriteString(")")
	}
	b.WriteString(".")
	if len(wp.DependsOn) > 0 {
		b.WriteString("\nDepends on (already completed): ")
		b.WriteString(strings.Join(wp.DependsOn, ", "))
		b.WriteString(".")
	}
	if len(wp.AcceptanceCriteria) > 0 {
		b.WriteString("\nAcceptance criteria: ")
		b.WriteString(strings.Join(wp.AcceptanceCriteria, ", "))
		b.WriteString(".")
	}
	if len(wp.ContractRefs) > 0 {
		b.WriteString("\nShared contract entries: ")
		b.WriteString(strings.Join(wp.ContractRefs, ", "))
		b.WriteString(".")
	}
	if len(wp.ExpectedDelivery) > 0 {
		b.WriteString("\nExpected delivery paths: ")
		b.WriteString(strings.Join(wp.ExpectedDelivery, ", "))
		b.WriteString(".")
	}
	if wp.Details != "" {
		b.WriteString("\n")
		b.WriteString(wp.Details)
	}
	b.WriteString("\nPlan: ")
	b.WriteString(plan.PlanID)
	b.WriteString(" (frozen; scope changes require a new approved version).")
	return b.String()
}

// PlanBranchFromWorkPackage builds the stage branch entity for one work
// package. Deterministic: the same (step, plan, wp) always yields the same
// branch, which is what makes materialization idempotent and report
// resolution exact.
func PlanBranchFromWorkPackage(step entity.WorkflowStep, plan DeliveryPlan, wp PlanWorkPackage) entity.WorkflowBranch {
	in, out := PlanBranchFieldTemplate(step)
	return entity.WorkflowBranch{
		ID:           strings.TrimSpace(wp.ID),
		Title:        strings.TrimSpace(wp.Title),
		Description:  PlanBranchDescription(plan, wp),
		ActorRole:    strings.TrimSpace(wp.ActorRole),
		InputFields:  in,
		OutputFields: out,
	}
}

// PlanWorkPackageByID returns a work package from a plan.
func PlanWorkPackageByID(plan DeliveryPlan, wpID string) (PlanWorkPackage, bool) {
	wpID = strings.TrimSpace(wpID)
	for _, wp := range plan.WorkPackages {
		if strings.TrimSpace(wp.ID) == wpID {
			return wp, true
		}
	}
	return PlanWorkPackage{}, false
}

// ResolveStageBranch resolves a branch-id to its stage definition for a
// branch report: the stage's static branches first (legacy stages), then the
// run's FROZEN plan (plan-driven stages). Trust rules match the
// materialization path exactly — the plan record must be frozen and its
// digest must verify; an unreadable or unapproved plan is an error, never a
// silent miss.
func (s *Store) ResolveStageBranch(project, runID string, step entity.WorkflowStep, branchID string) (entity.WorkflowBranch, bool, error) {
	if b, ok := workflowBranchByID(step.Branches, branchID); ok {
		return b, true, nil
	}
	_, version, ok, err := s.LoadFrozenPlanForRun(project, runID)
	if err != nil {
		return entity.WorkflowBranch{}, false, err
	}
	if !ok {
		return entity.WorkflowBranch{}, false, nil
	}
	wp, found := PlanWorkPackageByID(version.Plan, branchID)
	if !found {
		return entity.WorkflowBranch{}, false, nil
	}
	return PlanBranchFromWorkPackage(step, version.Plan, wp), true, nil
}
