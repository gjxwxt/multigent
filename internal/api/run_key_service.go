package api

import (
	"encoding/json"
	"strings"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Q0 PR-1: run_key resolution at enqueue time. The key is derived from the
// dispatch intent so the partial unique index on runtime_runs can dedupe
// concurrent enqueues of the same intent. A run without a derivable intent
// (legacy rows, exec prompts, fork sessions) keeps an empty key and bypasses
// dedup entirely — an empty key NEVER silences a real duplicate.

const (
	wakeupIntentScheduled = "scheduled"
)

// runtimeRunKeyForTask derives the run key for a task run from the task's
// dispatch metadata. Priority order:
//  1. workflow step run   — task carries workflow_run_id + workflow_step_id
//  2. attention wakeup    — task carries attention signal IDs
//  3. scheduled wakeup    — task is a scheduler wakeup task
//  4. plain task run      — the default for concrete tasks
func (s *Server) runtimeRunKeyForTask(workspaceID, project, agent string, task *entity.Task) (string, error) {
	if task == nil {
		return "", nil
	}
	vars := task.Vars
	if vars != nil {
		runID := strings.TrimSpace(vars[workflowRunIDVar])
		stepID := strings.TrimSpace(vars[workflowStepIDVar])
		if runID != "" && stepID != "" {
			stepInstanceID, err := s.workflowStepInstanceID(workspaceID, runID, stepID)
			if err != nil {
				return "", err
			}
			if stepInstanceID != "" {
				return controldb.RuntimeRunKeyWorkflowStep(workspaceID, runID, stepInstanceID)
			}
			// The step instance row is missing (should not happen on healthy
			// data) — fall back to the (runID, stepID) identity rather than
			// silently disabling dedup for a workflow step.
			return controldb.RuntimeRunKeyWorkflowStep(workspaceID, runID, stepID)
		}
	}
	if isWakeupRunForAgent(task) {
		intent := wakeupIntentScheduled
		if ids := attentionSignalIDsFromTask(task); len(ids) > 0 {
			intent = "attention:" + strings.Join(ids, ",")
		}
		return controldb.RuntimeRunKeyWakeup(workspaceID, project, agent, intent)
	}
	return controldb.RuntimeRunKeyTask(workspaceID, project, task.ID)
}

// workflowStepInstanceID resolves the server-generated step instance ID for a
// (workflow run, definition step) pair. Step instances are stored as records
// keyed (runID, stepID, instanceID); rework re-uses the same instance row.
func (s *Server) workflowStepInstanceID(workspaceID, workflowRunID, stepID string) (string, error) {
	if s == nil || s.controlDB == nil {
		return "", nil
	}
	recs, err := s.controlDB.ListRecords("workflow_step_instances", workspaceID, []string{workflowRunID, stepID})
	if err != nil {
		return "", err
	}
	for _, rec := range recs {
		var inst entity.WorkflowStepInstance
		if json.Unmarshal([]byte(rec.Payload), &inst) == nil && strings.TrimSpace(inst.ID) != "" {
			return inst.ID, nil
		}
	}
	return "", nil
}

// isWakeupRunForAgent mirrors ensurePendingAttentionWakeupTask's wakeup-task
// identity: Type=wakeup created by the scheduler attention pipeline.
func isWakeupRunForAgent(task *entity.Task) bool {
	if task == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(string(task.Type)), "wakeup") {
		return false
	}
	return strings.TrimSpace(task.CreatedBy) == attentionWakeupTaskCreatedBy
}

// attentionSignalIDsFromTask extracts the attention signal IDs carried by a
// wakeup task (MULTIGENT_ATTENTION_SIGNAL_IDS_JSON, set by the scheduler).
// Multiple concurrent attention signals collapse into one intent: the wakeup
// dedupes on the full set, and ensurePendingAttentionWakeupTask already
// merges new signals into the single pending wakeup task.
func attentionSignalIDsFromTask(task *entity.Task) []string {
	if task == nil || task.Vars == nil {
		return nil
	}
	raw := strings.TrimSpace(task.Vars["MULTIGENT_ATTENTION_SIGNAL_IDS_JSON"])
	if raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}
