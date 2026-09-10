package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func attentionWorktreeSignal(id, workspaceID, agent, taskID, reason string, worktreeDir string) controldb.AttentionSignal {
	refs := map[string]string{"project": "sample", "agent": agent}
	if taskID != "" {
		refs["taskId"] = taskID
	}
	refsJSON, _ := json.Marshal(refs)
	payload := map[string]any{}
	if worktreeDir != "" {
		payload["worktreeDir"] = worktreeDir
	}
	payloadJSON, _ := json.Marshal(payload)
	return controldb.AttentionSignal{
		ID:            id,
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-lina",
		DedupeKey:     "test:" + id,
		SourceKind:    "task",
		SourceID:      taskID,
		Reason:        reason,
		RefsJSON:      string(refsJSON),
		PayloadJSON:   string(payloadJSON),
		Summary:       "workflow step assigned",
		Status:        "pending",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
}

func TestAttentionWakeupWorktreeTargetResolvesAssignedTaskWorktree(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "lina", true)

	wtDir := filepath.Join(t.TempDir(), "wt-order-1")
	assigned := &entity.Task{
		ID:          "t-impl-1",
		Title:       "implement order api",
		Status:      entity.TaskStatusAwaitingConfirmation,
		WorktreeDir: wtDir,
		BranchName:  "task/t-impl-1",
	}
	if err := s.ts.AddTask("sample", "lina", assigned); err != nil {
		t.Fatalf("add assigned task: %v", err)
	}

	signal := attentionWorktreeSignal("sig-wt-1", workspaceID, "lina", "t-impl-1", "workflow_step_assigned", wtDir)
	dir, branch := s.attentionWakeupWorktreeTarget(workspaceID, "sample", []controldb.AttentionSignal{signal})
	if dir != wtDir {
		t.Fatalf("expected worktree dir %q, got %q", wtDir, dir)
	}
	if branch != "task/t-impl-1" {
		t.Fatalf("expected branch task/t-impl-1, got %q", branch)
	}
}

func TestAttentionWakeupWorktreeTargetIgnoresIMSignalsAndMissingTasks(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "lina", true)

	imSignal := attentionWorktreeSignal("sig-im-1", workspaceID, "lina", "", "im_mention", "")
	imSignal.SourceKind = "im_message"
	dir, branch := s.attentionWakeupWorktreeTarget(workspaceID, "sample", []controldb.AttentionSignal{imSignal})
	if dir != "" || branch != "" {
		t.Fatalf("IM signal must not resolve a worktree, got dir=%q branch=%q", dir, branch)
	}

	// Task-kind signal whose task does not exist must fail open (no worktree,
	// no error), keeping the wakeup on the agent home.
	ghost := attentionWorktreeSignal("sig-ghost", workspaceID, "lina", "t-missing", "workflow_step_assigned", "")
	dir, branch = s.attentionWakeupWorktreeTarget(workspaceID, "sample", []controldb.AttentionSignal{ghost})
	if dir != "" || branch != "" {
		t.Fatalf("missing task must not resolve a worktree, got dir=%q branch=%q", dir, branch)
	}

	// Task without a worktree (e.g. IM-assigned task) stays on agent home.
	noWt := &entity.Task{ID: "t-plain", Title: "plain", Status: entity.TaskStatusPending}
	if err := s.ts.AddTask("sample", "lina", noWt); err != nil {
		t.Fatalf("add plain task: %v", err)
	}
	plain := attentionWorktreeSignal("sig-plain", workspaceID, "lina", "t-plain", "task_assigned", "")
	dir, branch = s.attentionWakeupWorktreeTarget(workspaceID, "sample", []controldb.AttentionSignal{plain})
	if dir != "" || branch != "" {
		t.Fatalf("task without worktree must not resolve a worktree, got dir=%q branch=%q", dir, branch)
	}
}

func TestEnsurePendingAttentionWakeupTaskCarriesWorktreeContext(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "lina", true)

	wtDir := filepath.Join(t.TempDir(), "wt-order-2")
	assigned := &entity.Task{
		ID:          "t-impl-2",
		Title:       "implement order api",
		Status:      entity.TaskStatusAwaitingConfirmation,
		WorktreeDir: wtDir,
		BranchName:  "task/t-impl-2",
	}
	if err := s.ts.AddTask("sample", "lina", assigned); err != nil {
		t.Fatalf("add assigned task: %v", err)
	}
	if err := s.controlDB.UpsertAttentionSignal(attentionWorktreeSignal("sig-wt-2", workspaceID, "lina", "t-impl-2", "workflow_step_assigned", wtDir)); err != nil {
		t.Fatalf("upsert attention: %v", err)
	}

	task, ids, err := s.ensurePendingAttentionWakeupTask(workspaceID, "sample", "lina")
	if err != nil {
		t.Fatalf("ensure wakeup task: %v", err)
	}
	if task == nil {
		t.Fatal("expected wakeup task")
	}
	if len(ids) != 1 || ids[0] != "sig-wt-2" {
		t.Fatalf("unexpected attention ids: %+v", ids)
	}
	if task.Type != "wakeup" {
		t.Fatalf("expected wakeup type, got %q", task.Type)
	}
	if got := strings.TrimSpace(task.Vars["MULTIGENT_WAKEUP_WORKTREE_DIR"]); got != wtDir {
		t.Fatalf("expected worktree var %q, got %q", wtDir, got)
	}
	if got := strings.TrimSpace(task.Vars["MULTIGENT_WAKEUP_BRANCH"]); got != "task/t-impl-2" {
		t.Fatalf("expected branch var task/t-impl-2, got %q", got)
	}
	if task.WorktreeDir != wtDir {
		t.Fatalf("entity WorktreeDir should mirror the var, got %q", task.WorktreeDir)
	}
	if task.BranchName != "task/t-impl-2" {
		t.Fatalf("entity BranchName should mirror the var, got %q", task.BranchName)
	}

	// A second wakeup with a pure IM signal must not clobber the resolved
	// worktree on the existing attention task.
	imSignal := attentionWorktreeSignal("sig-im-2", workspaceID, "lina", "", "im_mention", "")
	imSignal.SourceKind = "im_message"
	if err := s.controlDB.UpsertAttentionSignal(imSignal); err != nil {
		t.Fatalf("upsert im signal: %v", err)
	}
	task2, _, err := s.ensurePendingAttentionWakeupTask(workspaceID, "sample", "lina")
	if err != nil {
		t.Fatalf("ensure second wakeup task: %v", err)
	}
	if task2.ID != task.ID {
		t.Fatalf("attention wakeup task should be reused, got %s vs %s", task2.ID, task.ID)
	}
	if task2.WorktreeDir != wtDir {
		t.Fatalf("reused wakeup task must keep its worktree target, got %q", task2.WorktreeDir)
	}
}

func TestNextRuntimeWakeupTaskFallbackCarriesWorktreeVars(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "lina", true)

	wtDir := filepath.Join(t.TempDir(), "wt-order-3")
	assigned := &entity.Task{
		ID:          "t-impl-3",
		Title:       "implement order api",
		Status:      entity.TaskStatusAwaitingConfirmation,
		WorktreeDir: wtDir,
		BranchName:  "task/t-impl-3",
	}
	if err := s.ts.AddTask("sample", "lina", assigned); err != nil {
		t.Fatalf("add assigned task: %v", err)
	}
	if err := s.controlDB.UpsertAttentionSignal(attentionWorktreeSignal("sig-wt-3", workspaceID, "lina", "t-impl-3", "workflow_step_assigned", wtDir)); err != nil {
		t.Fatalf("upsert attention: %v", err)
	}

	// Block the ensurePendingAttentionWakeupTask path indirectly is not
	// possible here; instead verify the primary path result — this mirrors
	// what enqueueRuntimeWakeupRunFromRequest consumes.
	task, ids, err := s.nextRuntimeWakeupTask(workspaceID, "sample", "lina", &entity.HeartbeatConfig{
		WakeupPrompt: "Check your work queue.",
	})
	if err != nil {
		t.Fatalf("next wakeup task: %v", err)
	}
	if task == nil {
		t.Fatal("expected wakeup task")
	}
	_ = ids
	if got := strings.TrimSpace(task.Vars["MULTIGENT_WAKEUP_WORKTREE_DIR"]); got != wtDir {
		t.Fatalf("expected worktree var %q on wakeup task, got %q (vars=%v)", wtDir, got, task.Vars)
	}
	if task.WorktreeDir != wtDir {
		t.Fatalf("entity WorktreeDir should be set for the local run path, got %q", task.WorktreeDir)
	}
}

func TestAttentionWakeupWorktreeTarget_MultipleDistinctTasks_FailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "lina", true)

	task1 := &entity.Task{
		ID:          "t-task-1",
		Title:       "task 1",
		Status:      entity.TaskStatusAwaitingConfirmation,
		WorktreeDir: "/tmp/wt-1",
		BranchName:  "task/t-task-1",
	}
	task2 := &entity.Task{
		ID:          "t-task-2",
		Title:       "task 2",
		Status:      entity.TaskStatusAwaitingConfirmation,
		WorktreeDir: "/tmp/wt-2",
		BranchName:  "task/t-task-2",
	}
	_ = s.ts.AddTask("sample", "lina", task1)
	_ = s.ts.AddTask("sample", "lina", task2)

	sig1 := attentionWorktreeSignal("sig-wt-a", workspaceID, "lina", "t-task-1", "workflow_step_assigned", "/tmp/wt-1")
	sig2 := attentionWorktreeSignal("sig-wt-b", workspaceID, "lina", "t-task-2", "workflow_step_assigned", "/tmp/wt-2")

	// Both worktree target and target task must fail-closed to empty when multiple distinct tasks exist
	dir, branch := s.attentionWakeupWorktreeTarget(workspaceID, "sample", []controldb.AttentionSignal{sig1, sig2})
	if dir != "" || branch != "" {
		t.Fatalf("multiple distinct tasks must fail-closed for worktree, got dir=%q, branch=%q", dir, branch)
	}

	targetTaskID, targetProj := s.attentionWakeupTargetTask(workspaceID, "sample", []controldb.AttentionSignal{sig1, sig2})
	if targetTaskID != "" || targetProj != "" {
		t.Fatalf("multiple distinct tasks must fail-closed for target task, got id=%q, proj=%q", targetTaskID, targetProj)
	}
}

