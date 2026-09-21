package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/multigent/multigent/internal/ciready"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Gate step resolution lives in internal/workflow/gate.go (Task 0.1 single
// source of truth: explicit platform_gate marker first, legacy ID fallback
// until the Task 0.5 deprecation). The ciReadyGateKind constant is kept for
// the cause-classification code below that references the gate kind string.
const platformGateConfigKey = workflowstore.GateConfigKey

// ciReadyGateKind is the ci_ready gate kind.
const ciReadyGateKind = workflowstore.GateKindCIReady

// Gate causes. Each one has a different owner, which is the whole point of
// splitting what used to be a single "not_ready".
const (
	// gateCauseRepairable: the repository itself is wrong (missing lockfile,
	// unparsable CI file, runner tag mismatch). The developer agent owns this.
	gateCauseRepairable = "repairable"
	// gateCauseWaitingCI: CI for the current HEAD exists but has not finished,
	// or has not been created yet. Nobody owns it; it needs time, not a person
	// pressing a button.
	gateCauseWaitingCI = "waiting_pipeline"
	// gateCauseCIFailed: the pipeline ran and did not succeed. The developer
	// agent owns this once, and a person owns it if it keeps failing.
	gateCauseCIFailed = "ci_failed"
	// gateCauseEnvironment: remote not bound or credentials missing. Only a
	// person can fix this, so it must never be reported as "the agent failed".
	gateCauseEnvironment = "environment"
)

// ciReadyGateDecision is the platform's verdict on a CI readiness gate step.
type ciReadyGateDecision struct {
	Blocked        bool
	Cause          string
	Detail         string
	Retryable      bool
	NeedsHuman     bool
	StepID         string
	FailedCheckIds []string
}

// classifyCIReadyGate maps a readiness report onto who should act next.
//
// Repository defects win over pipeline state: telling someone to wait for CI
// while the CI file cannot even be parsed wastes the wait.
func classifyCIReadyGate(response ciReadyResponse) ciReadyGateDecision {
	if strings.TrimSpace(response.Overall) != ciready.OverallNotReady {
		return ciReadyGateDecision{}
	}
	decision := ciReadyGateDecision{Blocked: true, Cause: gateCauseRepairable}
	var pipelineFailure *ciready.Check
	for i := range response.Checks {
		check := response.Checks[i]
		if check.Status != ciready.StatusFail && check.Status != "error" {
			continue
		}
		if check.Name == "pipeline_evidence" {
			pipelineFailure = &response.Checks[i]
			continue
		}
		decision.FailedCheckIds = append(decision.FailedCheckIds, check.Name)
	}
	if len(decision.FailedCheckIds) > 0 {
		decision.Detail = fmt.Sprintf("%d repository check(s) failed: %s", len(decision.FailedCheckIds), strings.Join(decision.FailedCheckIds, ", "))
		return decision
	}
	if pipelineFailure == nil {
		decision.Detail = "readiness report says not_ready without a failing check; refusing to advance"
		return decision
	}
	evidenceNote := strings.TrimSpace(response.PipelineError)
	if evidenceNote == "" {
		evidenceNote = strings.TrimSpace(pipelineFailure.Detail)
	}
	switch {
	case strings.Contains(evidenceNote, errRemoteRequired.Error()):
		decision.Cause = gateCauseEnvironment
		decision.NeedsHuman = true
		decision.Detail = evidenceNote
	case respPipelineRunning(response):
		decision.Cause = gateCauseWaitingCI
		decision.Retryable = true
		decision.Detail = fmt.Sprintf("pipeline %d for HEAD is %s; waiting for it to finish", response.Pipeline.ID, response.Pipeline.Status)
	case response.Pipeline != nil:
		decision.Cause = gateCauseCIFailed
		decision.Retryable = true
		decision.Detail = fmt.Sprintf("pipeline %d for HEAD finished %s", response.Pipeline.ID, response.Pipeline.Status)
	default:
		// No pipeline observed for HEAD yet: either GitLab has not created it or
		// the commit was never pushed. Waiting is the honest reading; a human
		// ping here is how a platform ends up with a person polling CI by hand.
		decision.Cause = gateCauseWaitingCI
		decision.Retryable = true
		decision.Detail = evidenceNote
	}
	return decision
}

func respPipelineRunning(response ciReadyResponse) bool {
	return response.Pipeline != nil && !isPipelineTerminal(response.Pipeline.Status)
}

// ciReadyGateDecisionForTask evaluates the gate attached to the task's active
// workflow step. ok=false means this step is not a CI gate and the caller must
// let the agent's completion through unchanged.
func (s *Server) ciReadyGateDecisionForTask(ctx context.Context, workspaceID, project string, task *entity.Task) (ciReadyGateDecision, bool, error) {
	if s == nil || s.controlDB == nil || task == nil || strings.TrimSpace(workspaceID) == "" {
		return ciReadyGateDecision{}, false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, task.ID)
	if err != nil {
		return ciReadyGateDecision{}, false, err
	}
	if !found {
		return ciReadyGateDecision{}, false, nil
	}
	step, isGate := ciReadyGateStep(wfStore, run)
	if !isGate {
		return ciReadyGateDecision{}, false, nil
	}
	decision, _, err := s.evaluateCIReadyGate(ctx, project)
	if err != nil {
		return ciReadyGateDecision{}, false, err
	}
	decision.StepID = step.ID
	if decision.Blocked {
		return decision, true, nil
	}
	// A gate that passes still reports its step so the caller can log which
	// evidence unlocked the route.
	return decision, true, nil
}

// ciReadyGateStep resolves the active step of a run and reports whether it is a
// CI readiness gate.
func ciReadyGateStep(wfStore *workflowstore.Store, run entity.WorkflowRun) (entity.WorkflowStep, bool) {
	def, found, err := wfStore.RunDefinition(run)
	if err != nil || !found {
		return entity.WorkflowStep{}, false
	}
	for _, step := range def.Steps {
		if step.ID != run.ActiveStepID {
			continue
		}
		if workflowstore.IsCIReadyGateStep(step) {
			return step, true
		}
		return entity.WorkflowStep{}, false
	}
	return entity.WorkflowStep{}, false
}

// evaluateCIReadyGate runs the platform-side readiness check for a project. The
// snapshot is instantaneous: the agent already had its chance to wait, so the
// gate reads whatever CI says right now rather than blocking the caller longer.
func (s *Server) evaluateCIReadyGate(ctx context.Context, project string) (ciReadyGateDecision, ciReadyResponse, error) {
	record, err := s.st.Project(project)
	if err != nil {
		return ciReadyGateDecision{}, ciReadyResponse{}, err
	}
	if strings.TrimSpace(record.Repo) == "" {
		return ciReadyGateDecision{}, ciReadyResponse{}, fmt.Errorf("project %q has no repository path", project)
	}
	response, err := s.ciReadyEvaluation(ctx, ciReadyOptions{
		project:        record,
		gatherEvidence: true,
	})
	if err != nil {
		return ciReadyGateDecision{}, ciReadyResponse{}, err
	}
	return classifyCIReadyGate(response), response, nil
}

// ciReadyGateMessage is what the agent sees when the platform will not let the
// step close. It names the owner and the next action, because "not_ready" alone
// has historically produced agents that retry the same failing command forever.
func ciReadyGateMessage(decision ciReadyGateDecision) string {
	switch decision.Cause {
	case gateCauseWaitingCI:
		return fmt.Sprintf("CI gate held: pipeline evidence for the current HEAD is not finished yet (%s). Keep the step open and re-submit once it is terminal; do not change code to work around a pending pipeline.", decision.Detail)
	case gateCauseCIFailed:
		return fmt.Sprintf("CI gate held: %s. Fix the failing job and re-submit this step once the pipeline for the new HEAD succeeds.", decision.Detail)
	case gateCauseEnvironment:
		return fmt.Sprintf("CI gate held for an environment reason only a person can clear: %s. Escalate this task to its project owner instead of retrying.", decision.Detail)
	default:
		detail := decision.Detail
		if detail == "" {
			detail = "one or more readiness checks failed"
		}
		return fmt.Sprintf("CI gate held: %s. Fix the repository so these checks pass, then re-submit this step.", detail)
	}
}

// ciReadyGateIsRetryable reports whether holding the step is a wait rather than a
// defect, which decides HTTP 409 versus 400 and whether a human gets pinged.
func ciReadyGateIsRetryable(decision ciReadyGateDecision) bool {
	return decision.Retryable && decision.Cause == gateCauseWaitingCI
}
