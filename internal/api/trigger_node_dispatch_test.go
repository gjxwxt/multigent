package api

import (
	"encoding/json"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// A task trigger for an agent pinned to a runtime node must be routed into
// the runtime dispatch queue, never the local wakeup cycle: the local cycle
// runs the agent CLI on the console host (not installed there) and archives
// the task done_failed on the first exec error while the node run may still
// be executing (production: t-20260914-mv6qm1).
func TestNodeAgentTaskTriggerDispatchesToRuntimeNotLocal(t *testing.T) {
	s, workspaceID := slotTestServer(t)
	node := slotTestNode(t, s, workspaceID)
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-pm")
	if err != nil || !ok {
		t.Fatalf("load aw-pm: %v %v", ok, err)
	}
	worker.DefaultRuntimeNodeID = node.ID
	if worker.DefaultModelAccountID == "" {
		worker.DefaultModelAccountID = "acct-test"
	}
	schedule, err := json.Marshal(entity.HeartbeatConfig{Enabled: false, Triggers: []entity.TriggerType{entity.TriggerOnTask}})
	if err != nil {
		t.Fatalf("marshal schedule: %v", err)
	}
	worker.ScheduleJSON = string(schedule)
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("bind node: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{ID: "task-node-trigger", Title: "Node trigger", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// The hook must claim the trigger and enqueue a runtime run.
	handled := s.dispatchTaskTriggerViaRuntime("sample", "pm", "poller: pending task", nil)
	if !handled {
		t.Fatal("node-assigned agent's task trigger must be handled by the runtime dispatch path")
	}
	runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{WorkspaceID: workspaceID, TaskID: task.ID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != "queued" && runs[0].Status != "running" {
		t.Fatalf("expected one active runtime run for the node agent, got %+v", runs)
	}
}

// A locally-run agent (no runtime node pin) must NOT be claimed by the hook —
// its triggers keep using the local wakeup cycle.
func TestLocalAgentTaskTriggerNotClaimedByRuntimeHook(t *testing.T) {
	s, _ := slotTestServer(t)
	now := time.Now().UTC()
	task := &entity.Task{ID: "task-local-trigger", Title: "Local trigger", Status: entity.TaskStatusPending, Priority: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	if s.dispatchTaskTriggerViaRuntime("sample", "pm", "poller: pending task", nil) {
		t.Fatal("locally-run agent must not be claimed by the runtime dispatch hook")
	}
}
