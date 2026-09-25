package api

import (
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

// Regression coverage for the hooks-relay rehearsal loss (2026-09-25): when a
// workflow step changed actor (Mira -> s2-dev-a) the lifecycle task vanished
// from every queue while its run kept advancing. Root cause was a delete-first
// task relocation: the source record was removed before the destination write
// was known to have succeeded. These tests pin the read paths the field failure
// broke — runtimeFindTask (agent runtime reports), findTaskInProject (console
// review + /start) and the GET /tasks listing resolution — against the required
// ordering (write destination, verify readable, only then remove source).

func seedLifecycleTask(t *testing.T, s *Server, agent string) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	task := &entity.Task{
		ID:        "t-move-repro",
		Title:     "hooks-relay lifecycle task",
		Prompt:    "deliver WP",
		Type:      "feature",
		Status:    entity.TaskStatusInProgress,
		Assignee:  "sample/" + agent,
		CreatedBy: "workflow",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", agent, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task
}

func assertTaskVisibleOnEveryReadPath(t *testing.T, s *Server, workspaceID, agent, taskID, context string) {
	t.Helper()
	principal := runtimeAgentPrincipal{WorkspaceID: workspaceID, Project: "sample", Agent: agent}
	if _, resolved, archived, err := s.runtimeFindTask(principal, taskID, ""); err != nil || archived {
		t.Fatalf("%s: runtimeFindTask(%s) = err %v archived %v, want found and active", context, agent, err, archived)
	} else if strings.TrimSpace(resolved) == "" {
		t.Fatalf("%s: runtimeFindTask(%s) resolved no agent", context, agent)
	}
	if _, _, err := s.findTaskInProject("sample", taskID); err != nil {
		t.Fatalf("%s: findTaskInProject = %v, want found", context, err)
	}
	// GET /api/v1/projects/{name}/tasks resolves tasks through projectAgentNames
	// plus the same store reads asserted above; run that resolution verbatim.
	agents, err := s.projectAgentNames(workspaceID, "sample")
	if err != nil {
		t.Fatalf("%s: projectAgentNames: %v", context, err)
	}
	found := false
	for _, name := range agents {
		if _, err := s.ts.GetTask("sample", name, taskID); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%s: GET tasks listing resolution cannot see task %s (agents=%v)", context, taskID, agents)
	}
}

func TestMovedWorkflowTaskStaysVisibleOnEveryReadPath(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task := seedLifecycleTask(t, s, "pm")
	assertTaskVisibleOnEveryReadPath(t, s, workspaceID, "pm", task.ID, "before move")

	if err := s.ts.MoveTask("sample", "pm", "backend", task); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	assertTaskVisibleOnEveryReadPath(t, s, workspaceID, "backend", task.ID, "after move")

	// The source queue must no longer hold it, otherwise the same task would be
	// dispatched to two agents.
	if _, err := s.ts.GetTask("sample", "pm", task.ID); err == nil {
		t.Fatalf("task still readable under the previous agent after the move")
	}
}

// A relocation whose destination write cannot happen must leave the task
// readable. This is the exact red/green pair for the delete-first ordering:
// with the lossy implementation the blank target deletes the source first and
// the record disappears from every queue.
func TestFailedWorkflowTaskMoveKeepsTaskReadable(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task := seedLifecycleTask(t, s, "pm")

	if err := s.ts.MoveTask("sample", "pm", "", task); err == nil {
		t.Fatalf("MoveTask with a blank target must fail")
	}
	assertTaskVisibleOnEveryReadPath(t, s, workspaceID, "pm", task.ID, "after failed move (blank target)")

	if err := s.ts.MoveTask("sample", "pm", "   ", task); err == nil {
		t.Fatalf("MoveTask with a whitespace target must fail")
	}
	assertTaskVisibleOnEveryReadPath(t, s, workspaceID, "pm", task.ID, "after failed move (whitespace target)")

	// Repeating a move whose source no longer holds the record must not remove
	// the destination copy either.
	if err := s.ts.MoveTask("sample", "pm", "backend", task); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	if err := s.ts.MoveTask("sample", "pm", "pm", task); err == nil {
		t.Fatalf("second move from an emptied source must fail")
	}
	assertTaskVisibleOnEveryReadPath(t, s, workspaceID, "backend", task.ID, "after replaying the move")
}
