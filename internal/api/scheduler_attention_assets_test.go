package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// seedAttentionTargetTaskAssets binds two input attachments (requirement_input
// + reference) and one deliverable to a target task so attention wakeup tests
// can verify which roles get copied onto the synthetic wakeup task.
func seedAttentionTargetTaskAssets(t *testing.T, s *Server, workspaceID, project, targetTaskID string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", targetTaskID, i)))
		sha := hex.EncodeToString(sum[:])
		if err := s.controlDB.UpsertAssetBlob(controldb.AssetBlob{
			Sha256: sha, Size: int64(10 + i), Mime: "text/plain",
			StoragePath: "/tmp/" + sha, CreatedBy: "test", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
		fileID := fmt.Sprintf("af-%s-%d", targetTaskID, i)
		if err := s.controlDB.InsertAssetFile(&controldb.AssetFile{
			ID: fileID, WorkspaceID: workspaceID, ProjectID: project,
			DisplayName: fmt.Sprintf("doc-%d.md", i), CurrentSha: sha,
			CreatedBy: "test", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("insert file: %v", err)
		}
		role := controldb.AssetRoleRequirementInput
		switch i {
		case 1:
			role = controldb.AssetRoleReference
		case 2:
			role = controldb.AssetRoleDeliverable
		}
		if err := s.controlDB.InsertAssetAttachment(&controldb.AssetAttachment{
			FileID: fileID, Sha256: sha, ProjectID: project, TaskID: targetTaskID,
			Role: role, AddedBy: "test", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("insert attachment: %v", err)
		}
	}
}

func seedAttentionTaskSignal(t *testing.T, s *Server, workspaceID, targetTaskID, signalID string) {
	t.Helper()
	if err := s.controlDB.UpsertAttentionSignal(controldb.AttentionSignal{
		ID:            signalID,
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-pm",
		DedupeKey:     "test:" + signalID,
		SourceKind:    "task",
		SourceID:      targetTaskID,
		Reason:        "task_assigned",
		Priority:      "high",
		Summary:       "Continue the requirement work",
		Status:        "pending",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		RefsJSON:      `{"project":"sample","agent":"pm","taskId":"` + targetTaskID + `"}`,
	}); err != nil {
		t.Fatalf("upsert attention: %v", err)
	}
	// The target-task resolver only votes for tasks that exist in the task
	// store; persist the target so MULTIGENT_WAKEUP_TARGET_TASK_ID gets set.
	// NotBefore in the future keeps it out of the direct pending-task
	// selection so the wakeup path is the one exercised.
	notBefore := time.Now().UTC().Add(time.Hour)
	if err := s.ts.AddTask("sample", "pm", &entity.Task{
		ID: targetTaskID, Title: "Requirement work", Status: entity.TaskStatusPending,
		NotBefore: &notBefore, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("add target task: %v", err)
	}
}

func TestAttentionWakeupTaskInheritsTargetTaskAssets(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	seedAttentionTargetTaskAssets(t, s, workspaceID, "sample", "task-target")
	seedAttentionTaskSignal(t, s, workspaceID, "task-target", "sig-assets-1")

	task, _, err := s.ensurePendingAttentionWakeupTask(workspaceID, "sample", "pm", "sig-assets-1")
	if err != nil {
		t.Fatalf("ensure wakeup task: %v", err)
	}
	if task == nil {
		t.Fatal("expected wakeup task")
	}
	if task.Vars["MULTIGENT_WAKEUP_TARGET_TASK_ID"] != "task-target" {
		t.Fatalf("expected target task var, vars=%v", task.Vars)
	}
	atts, err := s.controlDB.ListAssetAttachmentsForTask(task.ID)
	if err != nil {
		t.Fatalf("list wakeup attachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("expected 2 input bindings copied (deliverable excluded), got %d: %+v", len(atts), atts)
	}
	for _, att := range atts {
		if att.Role == controldb.AssetRoleDeliverable {
			t.Fatalf("deliverable role must not be copied onto wakeup task: %+v", att)
		}
	}
}

func TestAttentionWakeupTaskAssetCopyIsIdempotent(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	seedAttentionTargetTaskAssets(t, s, workspaceID, "sample", "task-target")
	seedAttentionTaskSignal(t, s, workspaceID, "task-target", "sig-assets-idem")

	first, _, err := s.ensurePendingAttentionWakeupTask(workspaceID, "sample", "pm", "sig-assets-idem")
	if err != nil || first == nil {
		t.Fatalf("first ensure: task=%+v err=%v", first, err)
	}
	// Second signal round reuses the same wakeup task; the copy must not
	// duplicate bindings.
	seedAttentionTaskSignal(t, s, workspaceID, "task-target", "sig-assets-idem-2")
	second, _, err := s.ensurePendingAttentionWakeupTask(workspaceID, "sample", "pm", "sig-assets-idem-2")
	if err != nil || second == nil {
		t.Fatalf("second ensure: task=%+v err=%v", second, err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected wakeup task reuse, got %s then %s", first.ID, second.ID)
	}
	atts, err := s.controlDB.ListAssetAttachmentsForTask(second.ID)
	if err != nil {
		t.Fatalf("list wakeup attachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("expected idempotent copy (still 2 bindings), got %d", len(atts))
	}
}

func TestAttentionWakeupWithoutTargetTaskSkipsAssetCopy(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	// IM signal without a target task: no asset copy should happen.
	if err := s.controlDB.UpsertAttentionSignal(controldb.AttentionSignal{
		ID:            "sig-im-only",
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-pm",
		DedupeKey:     "test:im-only",
		SourceKind:    "im_message",
		SourceID:      "msg-1",
		Reason:        "im_mention",
		Summary:       "Chat ping",
		Status:        "pending",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("upsert attention: %v", err)
	}
	task, _, err := s.ensurePendingAttentionWakeupTask(workspaceID, "sample", "pm", "sig-im-only")
	if err != nil {
		t.Fatalf("ensure wakeup task: %v", err)
	}
	if task == nil {
		t.Fatal("expected wakeup task")
	}
	atts, err := s.controlDB.ListAssetAttachmentsForTask(task.ID)
	if err != nil {
		t.Fatalf("list wakeup attachments: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("expected no asset bindings without a target task, got %d", len(atts))
	}
}

func TestRuntimeWakeupTaskInheritsTargetTaskAssets(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	seedAttentionTargetTaskAssets(t, s, workspaceID, "sample", "task-target")
	seedAttentionTaskSignal(t, s, workspaceID, "task-target", "sig-assets-runtime")

	task, _, err := s.nextRuntimeWakeupTask(workspaceID, "sample", "pm", &entity.HeartbeatConfig{})
	if err != nil {
		t.Fatalf("next runtime wakeup task: %v", err)
	}
	if task == nil {
		t.Fatal("expected wakeup task")
	}
	if task.Vars["MULTIGENT_WAKEUP_TARGET_TASK_ID"] != "task-target" {
		t.Fatalf("expected target task var, vars=%v", task.Vars)
	}
	if !strings.HasPrefix(task.CreatedBy, "heartbeat:") {
		t.Fatalf("expected synthetic wakeup task, createdBy=%s", task.CreatedBy)
	}
	atts, err := s.controlDB.ListAssetAttachmentsForTask(task.ID)
	if err != nil {
		t.Fatalf("list wakeup attachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("expected 2 input bindings on runtime wakeup task, got %d", len(atts))
	}
}
