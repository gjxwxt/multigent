package api

import (
	"path/filepath"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Q0 PR-1 service-level: the four dispatch intents get deterministic run
// keys, and a duplicate enqueue of the same intent returns the existing run
// instead of creating a second one.

func runKeyTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("rkproj", &entity.Project{Name: "rkproj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return s, workspaceID
}

// Plain task runs: same task → same key; different tasks → different keys.
func TestRunKeyPlainTaskDeterministic(t *testing.T) {
	s, ws := runKeyTestServer(t)
	taskA := &entity.Task{ID: "t-a", Status: entity.TaskStatusPending}
	taskB := &entity.Task{ID: "t-b", Status: entity.TaskStatusPending}

	keyA1, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", taskA)
	if err != nil {
		t.Fatalf("key a1: %v", err)
	}
	keyA2, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", taskA)
	if err != nil {
		t.Fatalf("key a2: %v", err)
	}
	keyB, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", taskB)
	if err != nil {
		t.Fatalf("key b: %v", err)
	}
	if keyA1 == "" || keyA1 != keyA2 {
		t.Fatalf("same task must produce identical non-empty key: %q vs %q", keyA1, keyA2)
	}
	if keyA1 == keyB {
		t.Fatal("different tasks must produce different keys")
	}
}

// Workflow step runs: the key is the (workflowRunID, stepInstanceID) identity
// from the task's workflow vars, NOT the task ID — rework reuses one task.
func TestRunKeyWorkflowStepIdentity(t *testing.T) {
	s, ws := runKeyTestServer(t)
	// Seed a step instance so the resolver can find the server identity.
	raw := `{"id":"wfs-abc","runId":"wfr-1","stepId":"implement","status":"running"}`
	if err := s.controlDB.UpsertRecord("workflow_step_instances", ws, []string{"wfr-1", "implement", "wfs-abc"}, raw); err != nil {
		t.Fatalf("seed step instance: %v", err)
	}
	task := &entity.Task{ID: "t-wf", Status: entity.TaskStatusPending, Vars: map[string]string{
		workflowRunIDVar:  "wfr-1",
		workflowStepIDVar: "implement",
	}}
	key, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", task)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if key != "wf:"+ws+":wfr-1:wfs-abc" {
		t.Fatalf("wf key = %q, want server step-instance identity", key)
	}
	// Rework: the SAME task re-dispatched resolves the same key.
	key2, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", task)
	if err != nil || key2 != key {
		t.Fatalf("rework key drift: %q vs %q (err=%v)", key2, key, err)
	}
}

// Wakeup runs: scheduled and attention intents differ.
func TestRunKeyWakeupIntents(t *testing.T) {
	s, ws := runKeyTestServer(t)
	scheduled := &entity.Task{ID: "t-w1", Status: entity.TaskStatusPending, Type: "wakeup", CreatedBy: attentionWakeupTaskCreatedBy}
	attention := &entity.Task{ID: "t-w2", Status: entity.TaskStatusPending, Type: "wakeup", CreatedBy: attentionWakeupTaskCreatedBy,
		Vars: map[string]string{"MULTIGENT_ATTENTION_SIGNAL_IDS_JSON": `["att-1","att-2"]`}}

	keyS, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", scheduled)
	if err != nil {
		t.Fatalf("scheduled key: %v", err)
	}
	keyA, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", attention)
	if err != nil {
		t.Fatalf("attention key: %v", err)
	}
	if keyS != "wakeup:"+ws+":rkproj:agent:scheduled" {
		t.Fatalf("scheduled wakeup key = %q", keyS)
	}
	if keyA != "wakeup:"+ws+":rkproj:agent:attention:att-1,att-2" {
		t.Fatalf("attention wakeup key = %q", keyA)
	}
	if keyS == keyA {
		t.Fatal("scheduled and attention wakeups must have different keys")
	}
	// A normal task is never mistaken for a wakeup even with the same ID.
	plain := &entity.Task{ID: "t-w1", Status: entity.TaskStatusPending}
	keyPlain, err := s.runtimeRunKeyForTask(ws, "rkproj", "agent", plain)
	if err != nil {
		t.Fatalf("plain key: %v", err)
	}
	if keyPlain == keyS {
		t.Fatal("plain task must not share the wakeup key namespace")
	}
}

// The end-to-end dedup: enqueue the same task twice through
// enqueueRuntimeTaskRun → one active run, second call returns the first.
func TestEnqueueRuntimeTaskRunIdempotent(t *testing.T) {
	s, ws := runKeyTestServer(t)
	now := "2026-09-13T00:00:00Z"
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID: "aw-rk", WorkspaceID: ws, Name: "rk-agent", DisplayName: "rk-agent", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
		ID: "pm-rk", WorkspaceID: ws, ProjectID: "rkproj", MemberType: "agent_worker", MemberID: "aw-rk",
		Role: "developer", Title: "rk-agent", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("membership: %v", err)
	}
	task := &entity.Task{ID: "t-dup", Title: "dup", Status: entity.TaskStatusPending}

	run1, err := s.enqueueRuntimeTaskRun(ws, "rkproj", "rk-agent", task, "", "http://127.0.0.1:1", "admin")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	run2, err := s.enqueueRuntimeTaskRun(ws, "rkproj", "rk-agent", task, "", "http://127.0.0.1:1", "admin")
	if err != nil {
		t.Fatalf("enqueue 2 must dedupe, not error: %v", err)
	}
	if run1.ID != run2.ID {
		t.Fatalf("duplicate enqueue returned %s, want the original %s", run2.ID, run1.ID)
	}
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: ws})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	active := 0
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "running" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active runs = %d, want 1", active)
	}
	if run1.RunKey == "" {
		t.Fatal("task run must carry a run_key")
	}
}

// Legacy path preserved: a run with no derivable intent (empty key) still
// enqueues fine — empty keys bypass dedup, they never error.
func TestEnqueueWithoutDerivableKeyStillWorks(t *testing.T) {
	store, err := controldb.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertWorkspace(controldb.Workspace{ID: "ws-lg", Name: "ws-lg"}); err != nil {
		t.Fatalf("ws: %v", err)
	}
	// Fork-style run: empty key by design.
	run := controldb.RuntimeRun{
		ID: "run-fork-1", WorkspaceID: "ws-lg", ProjectID: "p", AgentID: "a",
		ForkSessionID: "fs-1", Status: "queued", RunKey: "",
	}
	if _, inserted, err := store.UpsertRuntimeRunIdempotent(run); err != nil || !inserted {
		t.Fatalf("empty-key enqueue: inserted=%v err=%v", inserted, err)
	}
}
