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
