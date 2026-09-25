package api

// Delivery-plan freeze + acceptance-table data plane (slice 4).
//
// The approving contract review is the ONLY entry that freezes a batched run's
// structured delivery plan: the plan is parsed and validated BEFORE the
// transition starts, and the frozen record is written INSIDE the transition's
// guarded transaction (CompleteAndAdvanceWithExtras). Consequently:
//
//   - the run cannot advance to the parallel stage without its frozen plan
//     (the freeze rides the same commit; a freeze failure aborts both), and
//   - a frozen plan cannot exist unless the approving transition committed
//     (nothing else writes frozen versions).
//
// The read-only endpoint below exposes the acceptance table's data plane so
// every row's evidence is a machine pointer (plan record, task vars, branch
// instance, child run, worktree, QA baseline, human sign-off) rather than a
// hand-written claim.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// isPlanFreezeStep reports whether a human review step freezes the delivery
// plan on approval (template config, never a hard-coded step id).
func isPlanFreezeStep(step entity.WorkflowStep) bool {
	return strings.TrimSpace(step.Type) == "human_review" &&
		strings.EqualFold(strings.TrimSpace(step.Config[workflowstore.PlanFreezeConfigKey]), "true")
}

// buildDeliveryPlanForFreeze assembles the plan from the run's own step
// outputs: the structured plan produced by scale_gate (falling back to
// contract_batch) and the requirement anchor snapshot from requirement_draft.
func (s *Server) buildDeliveryPlanForFreeze(wfStore *workflowstore.Store, run entity.WorkflowRun) (workflowstore.DeliveryPlan, error) {
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return workflowstore.DeliveryPlan{}, err
	}
	outputs := make(map[string]map[string]string, len(instances))
	for _, inst := range instances {
		if len(inst.OutputValues) > 0 {
			outputs[inst.StepID] = inst.OutputValues
		}
	}
	ordered := stepsByRecency(instances)
	planJSONs := stepOutputCandidates(outputs, ordered,
		[]string{"scale_gate", "contract_batch"},
		[]string{"delivery_plan", "batch_plan"})
	anchorJSONs := stepOutputCandidates(outputs, ordered,
		[]string{"requirement_draft", "requirement_review", "scale_gate", "contract_batch"},
		[]string{"requirement_items"})
	return workflowstore.BuildDeliveryPlanFromRunOutputs(run.ID, planJSONs, anchorJSONs)
}

// stepsByRecency orders the instance step ids newest-first: a plan refined
// by a reworked contract_batch must win over the earlier scale_gate draft it
// supersedes.
func stepsByRecency(instances []entity.WorkflowStepInstance) []string {
	sorted := append([]entity.WorkflowStepInstance(nil), instances...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return stepRecency(sorted[i]).After(stepRecency(sorted[j]))
	})
	seen := map[string]bool{}
	var out []string
	for _, inst := range sorted {
		id := strings.TrimSpace(inst.StepID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func stepRecency(inst entity.WorkflowStepInstance) time.Time {
	if !inst.FinishedAt.IsZero() {
		return inst.FinishedAt
	}
	return inst.UpdatedAt
}

// stepOutputCandidates collects non-empty output values for the given steps
// and keys, in priority order (recency first, then key order).
func stepOutputCandidates(outputs map[string]map[string]string, orderedStepIDs, wantSteps, keys []string) []string {
	want := make(map[string]bool, len(wantSteps))
	for _, id := range wantSteps {
		want[id] = true
	}
	var out []string
	for _, stepID := range orderedStepIDs {
		if !want[stepID] {
			continue
		}
		values := outputs[stepID]
		if values == nil {
			continue
		}
		for _, key := range keys {
			if v := strings.TrimSpace(values[key]); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// preparePlanFreezeForReview is the single freeze entry shared by every human
// review path (in-console review submit and trigger/ChatOps callback), so the
// "approving review is the ONLY freeze entry" invariant holds for all of them.
func (s *Server) preparePlanFreezeForReview(wfStore *workflowstore.Store, project string, run entity.WorkflowRun, step entity.WorkflowStep, reviewer, comments string) (workflowstore.TransitionExtraWrites, int, error) {
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.preparePlanFreezeTransition(wfStore, project, run, step, reviewer, comments, instances)
}

// preparePlanFreezeTransition computes the extras for an approving review (nil
// for everything else). A malformed plan or a missing anchor returns a 400; a
// record-level conflict returns 409 — both BEFORE any run mutation.
func (s *Server) preparePlanFreezeTransition(wfStore *workflowstore.Store, project string, run entity.WorkflowRun, step entity.WorkflowStep, reviewer, comments string, instances []entity.WorkflowStepInstance) (workflowstore.TransitionExtraWrites, int, error) {
	plan, err := s.buildDeliveryPlanForFreeze(wfStore, run)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	instanceID := ""
	for _, inst := range instances {
		if inst.StepID == step.ID {
			instanceID = inst.ID
			break
		}
	}
	extras, err := wfStore.PreparePlanFreezeExtras(project, run.ID, plan, reviewer, workflowstore.PlanApprovalProvenance{
		StepID:     step.ID,
		InstanceID: instanceID,
		Comments:   comments,
	})
	if err != nil {
		return nil, http.StatusConflict, err
	}
	return extras, 0, nil
}

// planAcceptanceRow is one materialized work package in the acceptance table.
type planAcceptanceRow struct {
	WPID             string            `json:"wpId"`
	BranchID         string            `json:"branchId"`
	WaveIndex        int               `json:"waveIndex"`
	TaskID           string            `json:"taskId,omitempty"`
	ChildRunID       string            `json:"childRunId,omitempty"`
	ChildRunStatus   string            `json:"childRunStatus,omitempty"`
	BranchInstanceID string            `json:"branchInstanceId,omitempty"`
	BranchStatus     string            `json:"branchStatus,omitempty"`
	DefinitionID     string            `json:"definitionId,omitempty"`
	DefinitionDigest string            `json:"definitionDigest,omitempty"`
	BaseCommit       string            `json:"baseCommit,omitempty"`
	BranchName       string            `json:"branchName,omitempty"`
	WorktreeDir      string            `json:"worktreeDir,omitempty"`
	DeliveryCommit   string            `json:"deliveryCommit,omitempty"`
	QABaselineKey    string            `json:"qaBaselineKey,omitempty"`
	QABaselineExists bool              `json:"qaBaselinePresent"`
	MaterializedAt   string            `json:"materializedAt,omitempty"`
	TaskVars         map[string]string `json:"taskVars,omitempty"`
}

// handleGetTaskDeliveryPlan is the read-only acceptance-table data plane: it
// returns the frozen plan's approval provenance, its materialization rows and
// the human sign-offs recorded so far — every cell a machine pointer.
func (s *Server) handleGetTaskDeliveryPlan(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	view, status, err := s.deliveryPlanView(project, taskID)
	if err != nil {
		s.jsonError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}

// deliveryPlanView assembles the acceptance table's data plane; the HTTP
// handler is a thin access-checked wrapper so the assembly stays testable.
func (s *Server) deliveryPlanView(project, taskID string) (map[string]any, int, error) {
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !found {
		return nil, http.StatusNotFound, fmt.Errorf("task has no workflow run")
	}
	record, opened, err := wfStore.LoadPlanRecord(project, run.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !opened {
		return nil, http.StatusNotFound, fmt.Errorf("no delivery plan is frozen for this run")
	}
	current, _ := record.Current()

	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	byBranch := map[string]entity.WorkflowBranchInstance{}
	allBranches, _ := wfStore.ListBranchInstances(run.ID)
	for _, inst := range allBranches {
		byBranch[inst.BranchID] = inst
	}

	rows := make([]planAcceptanceRow, 0, len(record.Materializations))
	for _, m := range record.Materializations {
		row := planAcceptanceRow{
			WPID: m.WPID, BranchID: m.BranchID, WaveIndex: m.WaveIndex,
			TaskID: m.TaskID, ChildRunID: m.ChildRunID,
			DefinitionID: m.DefinitionID, DefinitionDigest: m.DefinitionDigest,
			MaterializedAt: m.MaterializedAt,
		}
		if inst, ok := byBranch[m.BranchID]; ok {
			row.BranchInstanceID = inst.ID
			row.BranchStatus = inst.Status
		}
		if m.ChildRunID != "" {
			if childRun, ok, err := wfStore.RunByID(project, m.ChildRunID); err == nil && ok {
				row.ChildRunStatus = childRun.Status
			}
		}
		if m.TaskID != "" {
			if task, agent, err := s.findTaskInProject(project, m.TaskID); err == nil && task != nil {
				row.BaseCommit = task.BaseCommit
				row.BranchName = task.BranchName
				row.WorktreeDir = task.WorktreeDir
				row.DeliveryCommit = strings.TrimSpace(task.CompletionCommit)
				if len(task.Vars) > 0 {
					vars := map[string]string{}
					for _, key := range []string{workflowPlanIDVar, workflowPlanVersionVar, workflowPlanDigestVar, workflowPlanWPIDVar, workflowPlanDependsVar} {
						if v := strings.TrimSpace(task.Vars[key]); v != "" {
							vars[key] = v
						}
					}
					row.TaskVars = vars
				}
				_ = agent
				if _, ok, err := wfStore.LoadQABaselinePayload(project, m.TaskID); err == nil {
					row.QABaselineExists = ok
					row.QABaselineKey = project + "/" + m.TaskID
				}
			}
		}
		rows = append(rows, row)
	}

	type signoffRow struct {
		StepID     string `json:"stepId"`
		InstanceID string `json:"instanceId"`
		ActorType  string `json:"actorType,omitempty"`
		ActorID    string `json:"actorId,omitempty"`
		Status     string `json:"status"`
		Decision   string `json:"decision,omitempty"`
		Comments   string `json:"comments,omitempty"`
		FinishedAt string `json:"finishedAt,omitempty"`
	}
	var signoffs []signoffRow
	for _, inst := range instances {
		if strings.TrimSpace(inst.OutputValues["decision"]) == "" {
			continue
		}
		finished := ""
		if !inst.FinishedAt.IsZero() {
			finished = inst.FinishedAt.UTC().Format(time.RFC3339)
		}
		signoffs = append(signoffs, signoffRow{
			StepID: inst.StepID, InstanceID: inst.ID,
			ActorType: inst.ActorType, ActorID: inst.ActorID, Status: inst.Status,
			Decision:   strings.TrimSpace(inst.OutputValues["decision"]),
			Comments:   strings.TrimSpace(inst.OutputValues["comments"]),
			FinishedAt: finished,
		})
	}

	approval := map[string]any{}
	if current.Version != 0 {
		approval = map[string]any{
			"planId":     record.PlanID,
			"runId":      record.RunID,
			"version":    current.Version,
			"digest":     current.Digest,
			"status":     current.Status,
			"approvedBy": current.ApprovedBy,
			"approvedAt": current.ApprovedAt,
			"approval":   current.Approval,
			"versions":   len(record.Versions),
		}
	}
	return map[string]any{
		"ok":               true,
		"runId":            run.ID,
		"runStatus":        run.Status,
		"activeStepId":     run.ActiveStepID,
		"approval":         approval,
		"materializations": rows,
		"signoffs":         signoffs,
	}, http.StatusOK, nil
}
