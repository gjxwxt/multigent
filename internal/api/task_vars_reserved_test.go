package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestSanitizeTaskVarsDropsReservedWakeupKeys(t *testing.T) {
	got := sanitizeTaskVars(map[string]string{
		"MULTIGENT_WAKEUP_WORKTREE_DIR":   "/opt/multigent/data",
		"MULTIGENT_WAKEUP_BRANCH":         "task/evil",
		"MULTIGENT_WAKEUP_TARGET_TASK_ID": "t-forged-123",
		"MULTIGENT_WAKEUP_PROJECT":        "sample",
		"legit_var":                       "ok",
	})
	if _, ok := got["MULTIGENT_WAKEUP_WORKTREE_DIR"]; ok {
		t.Fatal("user input must not set MULTIGENT_WAKEUP_WORKTREE_DIR")
	}
	if _, ok := got["MULTIGENT_WAKEUP_BRANCH"]; ok {
		t.Fatal("user input must not set MULTIGENT_WAKEUP_BRANCH")
	}
	if _, ok := got["MULTIGENT_WAKEUP_TARGET_TASK_ID"]; ok {
		t.Fatal("user input must not set MULTIGENT_WAKEUP_TARGET_TASK_ID")
	}
	if _, ok := got["MULTIGENT_WAKEUP_PROJECT"]; ok {
		t.Fatal("user input must not set MULTIGENT_WAKEUP_PROJECT")
	}
	if got["legit_var"] != "ok" {
		t.Fatalf("legit var should survive, got %v", got)
	}

	if got := sanitizeTaskVars(map[string]string{"MULTIGENT_WAKEUP_WORKTREE_DIR": "/etc"}); got != nil {
		t.Fatalf("only reserved vars should sanitize to nil, got %v", got)
	}
}

// TestVisibleTaskListVarsWhitelist pins the read-side contract of the task
// list API: the tree table gets exactly the batch identity keys
// (wpId/waveIndex/wpDepends) and nothing else from a task's internal vars.
func TestVisibleTaskListVarsWhitelist(t *testing.T) {
	got := visibleTaskListVars(map[string]string{
		"wpId":                          "WP-CONTRACT",
		"waveIndex":                     "2",
		"wpDepends":                     "WP-A,WP-B",
		"planId":                        "plan-1",
		"planVersion":                   "3",
		"planDigest":                    "sha256:deadbeef",
		"MULTIGENT_WAKEUP_WORKTREE_DIR": "/tmp/wt",
		"MULTIGENT_DELIVERY_CONTRACT":   `{"requireGitCommit":true}`,
		"initialization_repo":           "https://example.invalid/repo",
		"wpEmpty":                       "",
	})
	if got["wpId"] != "WP-CONTRACT" || got["waveIndex"] != "2" || got["wpDepends"] != "WP-A,WP-B" {
		t.Fatalf("whitelisted batch keys must survive, got %v", got)
	}
	for _, leak := range []string{"planId", "planVersion", "planDigest", "MULTIGENT_WAKEUP_WORKTREE_DIR", "MULTIGENT_DELIVERY_CONTRACT", "initialization_repo", "wpEmpty"} {
		if _, ok := got[leak]; ok {
			t.Fatalf("internal var %q must not reach the task list response, got %v", leak, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("response vars must carry only the 3 whitelist keys, got %v", got)
	}

	if got := visibleTaskListVars(nil); got != nil {
		t.Fatalf("nil vars should yield nil, got %v", got)
	}
	if got := visibleTaskListVars(map[string]string{"planId": "plan-1"}); got != nil {
		t.Fatalf("vars without whitelist keys should yield nil, got %v", got)
	}
}

func TestRuntimeTaskControlEnvWakeupKeysNeedWakeupType(t *testing.T) {
	wakeup := &entity.Task{
		Type: "wakeup",
		Vars: map[string]string{
			"MULTIGENT_WAKEUP_WORKTREE_DIR": "/tmp/wt",
			"MULTIGENT_WAKEUP_BRANCH":       "task/t-1",
			"MULTIGENT_FORK_SESSION_ID":     "fs-1",
		},
	}
	env := runtimeTaskControlEnv(wakeup)
	if env["MULTIGENT_WAKEUP_WORKTREE_DIR"] != "/tmp/wt" || env["MULTIGENT_WAKEUP_BRANCH"] != "task/t-1" {
		t.Fatalf("wakeup task should forward wakeup vars, got %v", env)
	}
	if env["MULTIGENT_FORK_SESSION_ID"] != "fs-1" {
		t.Fatalf("fork session var should forward, got %v", env)
	}

	normal := &entity.Task{
		Type: entity.TaskTypeFeature,
		Vars: map[string]string{
			"MULTIGENT_WAKEUP_WORKTREE_DIR": "/tmp/wt",
			"MULTIGENT_WAKEUP_BRANCH":       "task/t-1",
			"MULTIGENT_FORK_SESSION_ID":     "fs-1",
		},
	}
	env = runtimeTaskControlEnv(normal)
	if _, ok := env["MULTIGENT_WAKEUP_WORKTREE_DIR"]; ok {
		t.Fatal("non-wakeup task must not forward MULTIGENT_WAKEUP_WORKTREE_DIR (mount primitive)")
	}
	if _, ok := env["MULTIGENT_WAKEUP_BRANCH"]; ok {
		t.Fatal("non-wakeup task must not forward MULTIGENT_WAKEUP_BRANCH")
	}
	if env["MULTIGENT_FORK_SESSION_ID"] != "fs-1" {
		t.Fatalf("other scheduler vars must still forward, got %v", env)
	}

	if got := runtimeTaskControlEnv(nil); got != nil {
		t.Fatalf("nil task should yield nil env, got %v", got)
	}
}
