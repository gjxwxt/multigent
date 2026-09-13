package db

import (
	"fmt"
	"strings"
)

// Run key derivation (Q0 PR-1): deterministic per dispatch intent so the
// partial unique index idx_runtime_runs_active_key can dedupe concurrent
// enqueues of the same intent. All builders are pure functions; invalid
// input returns an error rather than an empty key (empty keys bypass the
// unique index, so an accidental empty key would silently disable dedup).

// RuntimeRunKeyTask is the key for a run that executes a specific task.
func RuntimeRunKeyTask(workspaceID, projectID, taskID string) (string, error) {
	ws, p, t := strings.TrimSpace(workspaceID), strings.TrimSpace(projectID), strings.TrimSpace(taskID)
	if ws == "" || p == "" || t == "" {
		return "", fmt.Errorf("run key task: workspace, project and task are required")
	}
	return "task:" + ws + ":" + p + ":" + t, nil
}

// RuntimeRunKeyWakeup is the key for a scheduler/attention wakeup run.
// intent is "scheduled" or "attention:{attentionID}".
func RuntimeRunKeyWakeup(workspaceID, projectID, agentID, intent string) (string, error) {
	ws, p, a := strings.TrimSpace(workspaceID), strings.TrimSpace(projectID), strings.TrimSpace(agentID)
	intent = strings.TrimSpace(intent)
	if ws == "" || p == "" || a == "" || intent == "" {
		return "", fmt.Errorf("run key wakeup: workspace, project, agent and intent are required")
	}
	return "wakeup:" + ws + ":" + p + ":" + a + ":" + intent, nil
}

// RuntimeRunKeyWorkflowStep is the key for a workflow-step run. Identity is
// the server-generated (workflowRunID, stepInstanceID) pair — there is no
// attempt counter; step rework re-uses the same step instance, so rework
// enqueues dedupe against the still-active run of that instance.
func RuntimeRunKeyWorkflowStep(workspaceID, workflowRunID, stepInstanceID string) (string, error) {
	ws, r, s := strings.TrimSpace(workspaceID), strings.TrimSpace(workflowRunID), strings.TrimSpace(stepInstanceID)
	if ws == "" || r == "" || s == "" {
		return "", fmt.Errorf("run key workflow: workspace, workflow run and step instance are required")
	}
	return "wf:" + ws + ":" + r + ":" + s, nil
}

// RuntimeRunKeyExec builds the caller-supplied Idempotency-Key into an exec
// run key. The key is bound to workspace + caller + project + agent so the
// same caller key in a different context can never collide. Callers must not
// log or audit the raw key — use RuntimeIdempotencyKeyFingerprint.
func RuntimeRunKeyExec(workspaceID, caller, projectID, agentID, key string) (string, error) {
	ws, c, p, a := strings.TrimSpace(workspaceID), strings.TrimSpace(caller), strings.TrimSpace(projectID), strings.TrimSpace(agentID)
	key = strings.TrimSpace(key)
	if ws == "" || c == "" || p == "" || a == "" {
		return "", fmt.Errorf("run key exec: workspace, caller, project and agent are required")
	}
	if err := validateIdempotencyKey(key); err != nil {
		return "", err
	}
	return "exec:" + ws + ":" + c + ":" + p + ":" + a + ":" + key, nil
}

// ValidateIdempotencyKey checks the caller-supplied key format: only
// [A-Za-z0-9._-], length 1..128.
func ValidateIdempotencyKey(key string) error {
	return validateIdempotencyKey(strings.TrimSpace(key))
}

func validateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("idempotency key must not be empty")
	}
	if len(key) > 128 {
		return fmt.Errorf("idempotency key exceeds 128 characters")
	}
	for _, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("idempotency key contains invalid character %q (allowed: A-Za-z0-9._-)", r)
		}
	}
	return nil
}
