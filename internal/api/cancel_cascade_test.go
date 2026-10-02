package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedCascadeFixture builds a root task on a parallel-stage run with the given
// branch children (branchID -> child task, pre-set status).
func seedCascadeFixture(t *testing.T, s *Server, workspaceID string, children map[string]*entity.Task) (*entity.Task, string) {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	root := &entity.Task{
		ID: "task-casc-root", Title: "Cascade root", Status: entity.TaskStatusInProgress,
		Assignee: "sample/pm", Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", root); err != nil {
		t.Fatal(err)
	}
	def := &entity.WorkflowDefinition{
		ID: "wf-casc-root", Name: "Cascade", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{
			{ID: "parallel", Type: "parallel_stage", Title: "Stage", JoinPolicy: "all"},
		},
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	run, _, err := wfStore.StartRun("sample", root.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"parallel": {Type: "agent", ID: "pm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for branchID, child := range children {
		if err := s.ts.AddTask("sample", "pm", child); err != nil {
			t.Fatal(err)
		}
		childRunID := ""
		if child.Status == entity.TaskStatusInProgress {
			childRun, _, err := wfStore.StartRun("sample", child.ID, def.ID, map[string]entity.WorkflowActorBinding{
				"parallel": {Type: "agent", ID: "pm"},
			})
			if err != nil {
				t.Fatal(err)
			}
			childRunID = childRun.ID
		}
		status := "running"
		if child.Status.IsTerminal() {
			status = "completed"
		}
		if err := wfStore.SaveBranchInstance(&entity.WorkflowBranchInstance{
			ID: "wbi-" + branchID, RunID: run.ID, StepID: "parallel", BranchID: branchID,
			Status: status, ChildTaskID: child.ID, ChildRunID: childRunID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return root, run.ID
}

// TestCancelTaskCascadeCancelsBranchChildren drives the P0-3 cascade at the
// store level: cancelling a root task with live branch children must cancel
// every non-terminal child (task status, child run, inbox) and settle the
// parent's branch instances, while terminal children stay untouched.
func TestCancelTaskCascadeCancelsBranchChildren(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	liveChild := &entity.Task{
		ID: "task-casc-live", Title: "Live child", Status: entity.TaskStatusInProgress,
		Assignee: "sample/pm", ParentID: "task-casc-root", Vars: map[string]string{},
	}
	doneChild := &entity.Task{
		ID: "task-casc-done", Title: "Done child", Status: entity.TaskStatusDoneSuccess,
		Assignee: "sample/pm", ParentID: "task-casc-root", Vars: map[string]string{},
	}
	root, runID := seedCascadeFixture(t, s, workspaceID, map[string]*entity.Task{
		"wp-live": liveChild,
		"wp-done": doneChild,
	})

	cancelled, err := s.cancelTaskCascade(workspaceID, "sample", root.ID, "root task cancelled")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatalf("exactly the live child must be cancelled here, got %d", cancelled)
	}

	gotLive, err := s.ts.GetTask("sample", "pm", liveChild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLive.Status != entity.TaskStatusCancelled {
		t.Fatalf("live child must be cancelled, got %s", gotLive.Status)
	}
	gotDone, err := s.ts.GetTask("sample", "pm", doneChild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDone.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("terminal child must stay untouched, got %s", gotDone.Status)
	}
	liveRunAfter, _, err := wfStore.RunForTask("sample", liveChild.ID)
	if err != nil {
		t.Fatal(err)
	}
	if liveRunAfter.Status != "cancelled" {
		t.Fatalf("live child run must be cancelled, got %s", liveRunAfter.Status)
	}
	rootRunAfter, _, err := wfStore.RunForTask("sample", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.ListBranchInstances(rootRunAfter.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.Status == "running" || inst.Status == "pending" {
			t.Fatalf("branch instance %s must be settled after the cascade, got %s", inst.BranchID, inst.Status)
		}
	}
	_ = runID
}

// TestCancelTaskCascadeWithoutRunOrBranchesIsNoop keeps the cascade honest for
// tasks that never fanned out: no run, or a run without branch instances, must
// be a clean zero (the common single-task cancel path).
func TestCancelTaskCascadeWithoutRunOrBranchesIsNoop(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)

	plain := &entity.Task{
		ID: "task-casc-plain", Title: "Plain task", Status: entity.TaskStatusInProgress,
		Assignee: "sample/pm", Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", plain); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.cancelTaskCascade(workspaceID, "sample", plain.ID, "root task cancelled")
	if err != nil || cancelled != 0 {
		t.Fatalf("a task without a run must cascade to nothing: n=%d err=%v", cancelled, err)
	}
}
