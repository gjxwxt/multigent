package api

// Delivery plan materialization (minimal closed loop, slice 3 of 3).
//
// When a run has a FROZEN delivery plan (workflow_plans record, slice 2), the
// parallel stage materializes its branches from the plan instead of the
// static template branch list:
//
//   - every READY work package (dependencies completed) becomes exactly one
//     branch: one child task (idempotency key plan/<run>/<version>/<wpId>),
//     one worktree, one QA baseline record, one branch definition snapshot
//     whose canonical hash is pinned in the plan record;
//   - wave k+1 is materialized from the EXISTING branch-completion path
//     (advanceParentAfterBranchCompletion): a completed branch re-evaluates
//     plan progress and either materializes the next ready wave or lets the
//     stage advance;
//   - a failed/skipped work package blocks its transitive dependents and the
//     blocked set is surfaced (error + root task error text) — never a
//     silent stall;
//   - with no plan record the stage keeps the legacy static-branch behavior
//     UNLESS the step config demands a frozen plan
//     (planMaterialization=frozen), in which case the stage refuses.
//
// 1:1:1:1: a plan-driven branch never reuses another work package's identity.
// A re-drive re-materializes the SAME task (idempotency key + the existing
// branch-instance guard) and the plan record refuses a second task id for the
// same work package.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// planMaterializationConfigKey marks a parallel stage as plan-driven when set
// to "frozen": the stage then REQUIRES a frozen plan record and refuses to
// fall back to static branches (fail-closed).
const planMaterializationConfigKey = "planMaterialization"

// Task Vars carrying the frozen plan identity on every materialized branch
// task. These are the audit trail from a branch back to the approved plan.
const (
	workflowPlanIDVar      = "planId"
	workflowPlanVersionVar = "planVersion"
	workflowPlanDigestVar  = "planDigest"
	workflowPlanWPIDVar    = "wpId"
	workflowPlanDependsVar = "wpDepends"
)

// planStageHints threads the frozen-plan identity into the branch
// materialization loop (idempotency key, task vars, actor binding, and the
// materialization record written after the branch instance exists).
type planStageHints struct {
	PlanID    string
	Version   int
	Digest    string
	WaveIndex int
	Plan      workflowstore.DeliveryPlan
	WPByID    map[string]workflowstore.PlanWorkPackage
}

// PlanRefusalError marks a fail-closed delivery-plan refusal. It is a
// typed error so HTTP callers can surface the REASON to the agent/operator
// instead of a generic internal error (round-6 P1-1 diagnosability lesson).
type PlanRefusalError struct{ Reason string }

func (e *PlanRefusalError) Error() string { return e.Reason }

func planRefusal(format string, args ...any) error {
	return &PlanRefusalError{Reason: fmt.Sprintf(format, args...)}
}

// planRefusalReason extracts the refusal text for HTTP responses.
func planRefusalReason(err error) (string, bool) {
	var refusal *PlanRefusalError
	if errors.As(err, &refusal) {
		return refusal.Reason, true
	}
	return "", false
}

// planDriven reports whether the parallel step must have a frozen plan.
func planDriven(step entity.WorkflowStep) bool {
	return strings.EqualFold(strings.TrimSpace(step.Config[planMaterializationConfigKey]), "frozen")
}

// planStageBranches resolves the frozen plan for the run and derives the
// branch list for the work packages that are READY and not yet materialized.
//
// Returns (nil, nil, nil) for a legacy stage without a frozen plan; every
// plan-related failure is an error (never a silent fallback), because a
// partly materialized plan that falls back to static branches would create a
// second, unapproved identity surface.
func (s *Server) planStageBranches(project, runID string, step entity.WorkflowStep, existing []entity.WorkflowBranchInstance, wfStore *workflowstore.Store, legacyBranches []entity.WorkflowBranch) ([]entity.WorkflowBranch, *planStageHints, error) {
	_, version, ok, err := wfStore.LoadFrozenPlanForRun(project, runID)
	if err != nil {
		return nil, nil, planRefusal("parallel stage %q: frozen delivery plan unreadable: %v", step.Title, err)
	}
	if !ok {
		if planDriven(step) {
			return nil, nil, planRefusal("parallel stage %q is plan-driven (config %s=frozen) but run %s has no frozen delivery plan; refusing to materialize unapproved branches", step.Title, planMaterializationConfigKey, runID)
		}
		return nil, nil, nil
	}
	plan := version.Plan

	states := make(map[string]string, len(existing))
	for _, inst := range existing {
		states[inst.BranchID] = strings.TrimSpace(inst.Status)
	}
	progress, err := workflowstore.EvaluatePlanProgress(plan, states)
	if err != nil {
		return nil, nil, planRefusal("parallel stage %q: evaluate frozen plan %s v%d: %v", step.Title, plan.PlanID, version.Version, err)
	}
	if len(progress.Blocked) > 0 {
		return nil, nil, planRefusal("parallel stage %q: delivery plan %s v%d has blocked work packages (%s) behind a failed/exhausted dependency; refusing to materialize and refusing to advance — resolve the upstream branch or re-plan (a new version requires human re-approval)",
			step.Title, plan.PlanID, version.Version, strings.Join(progress.Blocked, ", "))
	}

	wpByID := make(map[string]workflowstore.PlanWorkPackage, len(plan.WorkPackages))
	for _, wp := range plan.WorkPackages {
		wpByID[strings.TrimSpace(wp.ID)] = wp
	}
	existingIDs := make(map[string]bool, len(existing))
	for _, inst := range existing {
		existingIDs[inst.BranchID] = true
	}
	branches := make([]entity.WorkflowBranch, 0, len(progress.Ready))
	for _, wpID := range progress.Ready {
		if existingIDs[wpID] {
			continue
		}
		wp, found := wpByID[wpID]
		if !found {
			return nil, nil, planRefusal("parallel stage %q: plan %s v%d lists ready work package %q with no definition", step.Title, plan.PlanID, version.Version, wpID)
		}
		// Same helper the store's branch-report resolution uses: one source of
		// truth for the derived branch's shape (fields, description, role).
		branches = append(branches, workflowstore.PlanBranchFromWorkPackage(step, plan, wp))
	}
	if len(branches) == 0 && len(existing) == 0 {
		return nil, nil, planRefusal("parallel stage %q: frozen delivery plan %s v%d has no materializable work package (ready: 0 of %d) and no branch exists yet; refusing to leave the stage without materialized work",
			step.Title, plan.PlanID, version.Version, len(plan.WorkPackages))
	}
	sort.SliceStable(branches, func(i, j int) bool { return branches[i].ID < branches[j].ID })

	waveIndex := -1
	if len(branches) > 0 {
		waveIndex = workflowstore.PlanWaveIndex(plan, branches[0].ID)
	}
	// The plan record is the source of truth for work package identity; the
	// hints carry it into the materialization loop.
	return branches, &planStageHints{
		PlanID:    plan.PlanID,
		Version:   version.Version,
		Digest:    version.Digest,
		WaveIndex: waveIndex,
		Plan:      plan,
		WPByID:    wpByID,
	}, nil
}

// planDefinitionDigest pins the canonical hash of a runtime-derived branch
// definition. Timestamps are zeroed so the hash identifies CONTENT, not the
// moment of materialization (a re-drive of the same work package must hash
// identically).
func planDefinitionDigest(def entity.WorkflowDefinition) (string, error) {
	canonical := def
	canonical.CreatedAt = time.Time{}
	canonical.UpdatedAt = time.Time{}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// planBranchIdempotencyKey is the plan-scoped identity of a work package
// branch (mirrors the existing fanout/<run>/<step>/<branch> shape).
func planBranchIdempotencyKey(runID string, planVersion int, wpID string) string {
	return "plan/" + strings.TrimSpace(runID) + "/" + strconv.Itoa(planVersion) + "/" + strings.TrimSpace(wpID)
}

// branchIdempotencyKey picks the plan-scoped key for plan-derived branches
// and keeps the historical key for legacy static branches.
func branchIdempotencyKey(hints *planStageHints, runID, stepID, branchID string) string {
	if hints != nil {
		return planBranchIdempotencyKey(runID, hints.Version, branchID)
	}
	return "fanout/" + runID + "/" + stepID + "/" + branchID
}

// stageAuthoritativelyComplete re-checks a parallel stage against the CURRENT
// branch instances (and the frozen plan, when one exists) before the parent
// run may advance.
//
// Why this exists: BranchTransitionResult.AllDone is computed by the store
// against the branch instance set AS OF that branch's completion. Two branch
// completions can interleave so that the earlier one observed every instance
// terminal (AllDone=true) just before the later one materialized a new plan
// wave; trusting that stale true would advance the parent run past a RUNNING
// wave (and the running branch would then bounce off "step is no longer
// active"). For a plan-driven stage the authoritative answer is recomputed
// here from the live instance set: every work package materialized, none
// running/waiting/ready/blocked, every instance completed. Legacy stages
// without a frozen plan keep the store's answer unchanged.
func (s *Server) stageAuthoritativelyComplete(workspaceID, project string, result workflowstore.BranchTransitionResult) (bool, error) {
	if s.controlDB == nil {
		return result.AllDone, nil
	}
	run := result.Transition.Run
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	_, version, ok, err := wfStore.LoadFrozenPlanForRun(project, run.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return result.AllDone, nil
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, result.Branch.StepID)
	if err != nil {
		return false, err
	}
	states := make(map[string]string, len(instances))
	for _, inst := range instances {
		states[inst.BranchID] = strings.TrimSpace(inst.Status)
	}
	progress, err := workflowstore.EvaluatePlanProgress(version.Plan, states)
	if err != nil {
		return false, err
	}
	if len(progress.Ready)+len(progress.Waiting)+len(progress.InProgress)+len(progress.Blocked) > 0 {
		return false, nil
	}
	// Every work package must be materialized. Checked PER WORK PACKAGE
	// rather than by counting instances, so a stage that also carries static
	// branches (mixed mode) can neither look complete early nor be judged on
	// the wrong denominator.
	for _, wp := range version.Plan.WorkPackages {
		if _, materialized := states[strings.TrimSpace(wp.ID)]; !materialized {
			return false, nil
		}
	}
	// And every existing branch of THIS stage must be completed: a static
	// branch that is still running (mixed mode) blocks the advance too.
	for _, inst := range instances {
		if strings.TrimSpace(inst.Status) != workflowstore.PlanWPStateCompleted {
			return false, nil
		}
	}
	return true, nil
}

// materializeNextPlanWave is the wave k+1 trigger, hung on the EXISTING
// branch-completion path. It returns true when it materialized at least one
// work package (the caller must then keep the stage running instead of
// advancing).
func (s *Server) materializeNextPlanWave(workspaceID, project, rootAgent string, root *entity.Task, result workflowstore.BranchTransitionResult, r *http.Request) (bool, error) {
	if s.controlDB == nil || root == nil {
		return false, nil
	}
	run := result.Transition.Run
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	_, version, ok, err := wfStore.LoadFrozenPlanForRun(project, run.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, result.Branch.StepID)
	if err != nil {
		return false, err
	}
	states := make(map[string]string, len(instances))
	for _, inst := range instances {
		states[inst.BranchID] = strings.TrimSpace(inst.Status)
	}
	progress, err := workflowstore.EvaluatePlanProgress(version.Plan, states)
	if err != nil {
		return false, err
	}
	if len(progress.Blocked) > 0 {
		// Visible fail-closed: the blocked set is recorded on the root task so
		// an operator sees WHY the stage cannot finish.
		note := fmt.Sprintf("delivery plan %s v%d blocked work packages: %s", version.Plan.PlanID, version.Version, strings.Join(progress.Blocked, ", "))
		if !strings.Contains(root.LastError, note) {
			root.LastError = strings.TrimSpace(strings.TrimSpace(root.LastError + "; " + note))
			root.UpdatedAt = time.Now().UTC()
			if err := s.ts.PersistTask(project, rootAgent, root); err != nil {
				return false, err
			}
		}
		return false, planRefusal("%s", note)
	}
	if len(progress.Ready) == 0 {
		return false, nil
	}

	step, found, err := s.parallelStageStepForRun(wfStore, run, result.Branch.StepID)
	if err != nil || !found {
		return false, err
	}
	stageInst, err := s.parallelStageInstance(wfStore, run.ID, result.Branch.StepID)
	if err != nil {
		return false, err
	}
	log.Printf("[plan-materialize] run %s step %s: materializing wave with work packages %v (plan %s v%d)",
		run.ID, result.Branch.StepID, progress.Ready, version.Plan.PlanID, version.Version)
	if err := s.activateParallelWorkflowStep(workspaceID, project, rootAgent, root, workflowstore.TransitionResult{
		Run:     run,
		Current: stageInst,
		Next:    &step,
		NextInst: &entity.WorkflowStepInstance{
			RunID:  run.ID,
			StepID: step.ID,
			Status: "pending",
		},
	}, r); err != nil {
		return false, err
	}
	return true, nil
}

// parallelStageStepForRun resolves the parallel stage definition from the
// run's definition snapshot (preferred) or the persisted definition.
func (s *Server) parallelStageStepForRun(wfStore *workflowstore.Store, run entity.WorkflowRun, stepID string) (entity.WorkflowStep, bool, error) {
	if run.DefinitionSnapshot != nil {
		if step, ok := workflowStepByID(run.DefinitionSnapshot.Steps, stepID); ok {
			return step, true, nil
		}
	}
	def, found, err := wfStore.Definition(run.DefinitionID)
	if err != nil {
		return entity.WorkflowStep{}, false, err
	}
	if !found {
		return entity.WorkflowStep{}, false, fmt.Errorf("run %s definition %s not found while materializing a plan wave", run.ID, run.DefinitionID)
	}
	step, ok := workflowStepByID(def.Steps, stepID)
	if !ok {
		return entity.WorkflowStep{}, false, fmt.Errorf("run %s definition %s has no step %s", run.ID, run.DefinitionID, stepID)
	}
	return step, true, nil
}

// parallelStageInstance returns the parent stage's step instance; its
// InputValues are the upstream inputs every wave hands to its branches (the
// same surface wave 1 received).
func (s *Server) parallelStageInstance(wfStore *workflowstore.Store, runID, stepID string) (entity.WorkflowStepInstance, error) {
	instances, err := wfStore.ListStepInstances(runID)
	if err != nil {
		return entity.WorkflowStepInstance{}, err
	}
	for _, inst := range instances {
		if inst.StepID == stepID {
			return inst, nil
		}
	}
	return entity.WorkflowStepInstance{
		RunID:  runID,
		StepID: stepID,
		Status: "pending",
	}, nil
}
