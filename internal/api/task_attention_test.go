package api

import (
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func TestRecordTaskAttentionSignalCreatesAndDedupes(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	task := &entity.Task{
		ID:        "task-attention-1",
		Title:     "Review customer issue",
		Priority:  1,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	firstID := s.recordTaskAttentionSignal(workspaceID, "sample", "pm", task, "task assigned")
	if firstID == "" {
		t.Fatal("expected attention signal id")
	}
	secondID := s.recordTaskAttentionSignal(workspaceID, "sample", "pm", task, "task assigned")
	if secondID == "" {
		t.Fatal("expected duplicate attention signal id")
	}

	signals, err := s.controlDB.ListAttentionSignals(controldb.AttentionSignalFilter{
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-pm",
	})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("expected one deduped signal, got %d: %+v", len(signals), signals)
	}
	if signals[0].Reason != "task_assigned" || signals[0].Priority != "high" || signals[0].Status != "pending" {
		t.Fatalf("unexpected signal: %+v", signals[0])
	}
	if signals[0].SourceKind != "task" || signals[0].SourceID != task.ID || signals[0].SourceChannel != "project:sample" {
		t.Fatalf("unexpected signal source: %+v", signals[0])
	}
	trust := attentionSignalTrust(signals[0])
	if trust["trustLevel"] != "system" || trust["actorAuthorized"] != true || trust["instructionsTrusted"] != true {
		t.Fatalf("unexpected task trust context: %#v", trust)
	}
}

func TestRecordTaskAttentionSignalSkipsDisabledMembership(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", false)

	id := s.recordTaskAttentionSignal(workspaceID, "sample", "pm", &entity.Task{
		ID:       "task-attention-disabled",
		Title:    "Disabled",
		Assignee: "sample/pm",
		Status:   entity.TaskStatusPending,
	}, "task assigned")
	if id != "" {
		t.Fatalf("expected no signal id, got %q", id)
	}
	signals, err := s.controlDB.ListAttentionSignals(controldb.AttentionSignalFilter{WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("expected no signals, got %+v", signals)
	}
}

func TestNormalizeTaskAttentionReasonWorkflow(t *testing.T) {
	if got := normalizeTaskAttentionReason("workflow next task"); got != string(entity.TriggerOnWorkflowStepAssigned) {
		t.Fatalf("expected workflow reason, got %q", got)
	}
}

func TestRecordTaskAttentionSignalWorkspaceInstructionForProjectInitialization(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	task := &entity.Task{
		ID:          "task-init-1",
		Title:       "Scaffold project",
		Labels:      []string{"project-initialization"},
		WorktreeDir: "/opt/multigent/data/projects/sample/workspace",
		Assignee:    "sample/pm",
		Status:      entity.TaskStatusPending,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	sigID := s.recordTaskAttentionSignal(workspaceID, "sample", "pm", task, "task assigned")
	if sigID == "" {
		t.Fatal("expected attention signal id")
	}

	signals, err := s.controlDB.ListAttentionSignals(controldb.AttentionSignalFilter{
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-pm",
	})
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(signals))
	}
	if !strings.Contains(signals[0].PayloadJSON, "【工程初始化工作区说明】当前沙箱根目录 /workspace 即为项目代码库根目录") {
		t.Fatalf("expected initialization workspace instruction, got %s", signals[0].PayloadJSON)
	}
	if strings.Contains(signals[0].PayloadJSON, "/workspace/.multigent/worktrees/workspace") {
		t.Fatalf("expected no fabricated worktree path, got %s", signals[0].PayloadJSON)
	}
}

func seedTaskAttentionWorker(t *testing.T, s *Server, workspaceID, project, agent string, attentionEnabled bool) {
	t.Helper()
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:          "aw-" + agent,
		WorkspaceID: workspaceID,
		Name:        agent,
		DisplayName: agent,
	}); err != nil {
		t.Fatalf("upsert worker: %v", err)
	}
	if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
		ID:               "pm-" + project + "-" + agent,
		WorkspaceID:      workspaceID,
		ProjectID:        project,
		MemberType:       "agent_worker",
		MemberID:         "aw-" + agent,
		Role:             "developer",
		Title:            agent,
		AttentionEnabled: attentionEnabled,
	}); err != nil {
		t.Fatalf("upsert membership: %v", err)
	}
}
