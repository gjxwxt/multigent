package api

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func TestRecoverActiveWorkflowRunsFiltersHumanReviewAndCompleted(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	now := time.Now().UTC()

	// Task 1: on human review step
	taskHuman := &entity.Task{
		ID:        "t-human-review",
		Title:     "Human review task",
		Priority:  2,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", taskHuman); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// Task 2: on agent step (should be resumed)
	taskAgent := &entity.Task{
		ID:        "t-agent-step",
		Title:     "Agent step task",
		Priority:  2,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", taskAgent); err != nil {
		t.Fatalf("add task: %v", err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	defHuman := &entity.WorkflowDefinition{
		ID:          "wf-human-test",
		Name:        "Human Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "review",
		Steps: []entity.WorkflowStep{{
			ID:       "review",
			Type:     "human_review",
			Title:    "Review",
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(defHuman); err != nil {
		t.Fatalf("save human def: %v", err)
	}
	if _, _, err := wfStore.StartRun("sample", taskHuman.ID, defHuman.ID, nil); err != nil {
		t.Fatalf("start human run: %v", err)
	}

	defAgent := &entity.WorkflowDefinition{
		ID:          "wf-agent-test",
		Name:        "Agent Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "qa",
		Steps: []entity.WorkflowStep{{
			ID:       "qa",
			Type:     "agent_task",
			Title:    "QA Step",
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(defAgent); err != nil {
		t.Fatalf("save agent def: %v", err)
	}
	if _, _, err := wfStore.StartRun("sample", taskAgent.ID, defAgent.ID, nil); err != nil {
		t.Fatalf("start agent run: %v", err)
	}

	resumed := s.recoverActiveWorkflowRunsWithDelay(0)
	if resumed != 1 {
		t.Fatalf("expected exactly 1 task to be resumed, got %d", resumed)
	}
}

func TestIsTaskAtHumanReviewStep(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	now := time.Now().UTC()

	taskHuman := &entity.Task{
		ID:        "t-hr-check",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.ts.AddTask("sample", "pm", taskHuman)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID:          "wf-hr-check",
		Name:        "HR Check",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "rev",
		Steps: []entity.WorkflowStep{
			{ID: "rev", Type: "human_review", Title: "Rev"},
			{ID: "dev", Type: "agent_task", Title: "Dev"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = wfStore.SaveDefinition(def)
	_, _, _ = wfStore.StartRun("sample", taskHuman.ID, def.ID, nil)

	if !s.isTaskAtHumanReviewStep(workspaceID, "sample", taskHuman.ID) {
		t.Fatal("expected isTaskAtHumanReviewStep to return true for human_review step")
	}

	// Advance to dev step
	_, _ = wfStore.CompleteAndAdvance("sample", taskHuman.ID, "ok", "", map[string]string{"decision": "approve"}, "completed")
	if s.isTaskAtHumanReviewStep(workspaceID, "sample", taskHuman.ID) {
		t.Fatal("expected isTaskAtHumanReviewStep to return false for agent_task step")
	}
}

func TestCommitAndPushReviewChanges(t *testing.T) {
	tmpDir := t.TempDir()
	gitInit := exec.Command("git", "init", "-b", "main")
	gitInit.Dir = tmpDir
	if err := gitInit.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	gitConfigEmail := exec.Command("git", "config", "user.email", "test@example.com")
	gitConfigEmail.Dir = tmpDir
	_ = gitConfigEmail.Run()
	gitConfigName := exec.Command("git", "config", "user.name", "Test Runner")
	gitConfigName.Dir = tmpDir
	_ = gitConfigName.Run()

	// Initial commit
	readme := filepath.Join(tmpDir, "README.md")
	_ = os.WriteFile(readme, []byte("# Test"), 0644)
	gitAdd := exec.Command("git", "add", "README.md")
	gitAdd.Dir = tmpDir
	_ = gitAdd.Run()
	gitCommit := exec.Command("git", "commit", "-m", "initial commit")
	gitCommit.Dir = tmpDir
	_ = gitCommit.Run()

	s, _ := newConnectionGrantPolicyServer(t)
	task := &entity.Task{
		ID:          "t-preview-fix",
		BranchName:  "main",
		WorktreeDir: tmpDir,
	}

	// 1. Clean workspace -> nothing committed
	s.commitAndPushReviewChanges("sample", "pm", task)
	logCmd := exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = tmpDir
	out, _ := logCmd.Output()
	if !strings.Contains(string(out), "initial commit") {
		t.Fatalf("expected no new commit on clean workspace, got %s", string(out))
	}

	// 2. Modified workspace from Preview Copilot -> automatically committed
	_ = os.WriteFile(filepath.Join(tmpDir, "copilot_fix.txt"), []byte("reviewed and fixed"), 0644)
	s.commitAndPushReviewChanges("sample", "pm", task)

	logCmd = exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = tmpDir
	out, _ = logCmd.Output()
	if !strings.Contains(string(out), "user in-context preview feedback fixes") {
		t.Fatalf("expected commit created with review message, got %s", string(out))
	}
}

func TestRecoverablePendingAttentionWakeupTargetsExcludesActiveWorkflowTasks(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	now := time.Now().UTC()

	task := &entity.Task{
		ID:        "t-active-workflow-task",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.ts.AddTask("sample", "pm", task)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID:          "wf-active-test",
		Name:        "Active Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "step1",
		Steps: []entity.WorkflowStep{{
			ID:       "step1",
			Type:     "agent_task",
			Title:    "Step 1",
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("save def: %v", err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, nil); err != nil {
		t.Fatalf("start run: %v", err)
	}

	// Add an attention signal referencing this active workflow task
	if err := s.controlDB.UpsertAttentionSignal(controldb.AttentionSignal{
		ID:            "sig-active-workflow-task",
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-pm",
		DedupeKey:     "test:active-wf-task",
		SourceKind:    "task",
		SourceID:      task.ID,
		Reason:        "task_assigned",
		Summary:       "Task assigned signal",
		Status:        "pending",
		RefsJSON:      fmt.Sprintf(`{"project":"sample","agent":"pm","taskId":%q}`, task.ID),
		CreatedAt:     now.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("upsert attention: %v", err)
	}

	targets, err := s.recoverablePendingAttentionWakeupTargets(100)
	if err != nil {
		t.Fatalf("recoverable targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected active workflow task signal to be excluded from wakeup recovery targets, got: %+v", targets)
	}
}
