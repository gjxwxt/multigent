package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	taskstore "github.com/multigent/multigent/internal/taskstore"
)

// Regression: the CLI wakeup child resolved attention-signal worktree targets
// through the FS task store (tasks.yaml), which is empty on control-plane
// SQLite deployments — so workflow_step_assigned wakeups ran on the agent home
// with a git-forbidden boundary instead of the task's worktree. The target
// lookup must go through the same store the deployment writes tasks to.
func TestPendingAttentionSectionResolvesWorktreeFromDBTask(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", "")
	t.Setenv("MULTIGENT_DATA_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Test\nlang: zh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAgentWorker(controldb.AgentWorker{ID: "aw-lina", WorkspaceID: "ws", Name: "lina", DisplayName: "lina", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertProjectMembership(controldb.ProjectMembership{
		ID:         "pm-1",
		WorkspaceID: "ws",
		ProjectID:   "sample",
		MemberType:  "agent_worker",
		MemberID:    "aw-lina",
		Role:        "dev",
		Title:       "lina",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	signalRefs := `{"project":"sample","agent":"lina","taskId":"t-init-1","workflowId":"project-initialization-v1","workflowStepId":"ready"}`
	signalPayload := `{"reason":"workflow_step_assigned","worktreeDir":"/tmp/wt/sample/workspace","status":"in_progress"}`
	if err := db.UpsertAttentionSignal(controldb.AttentionSignal{
		ID:            "sig-wf",
		WorkspaceID:   "ws",
		AgentWorkerID: "aw-lina",
		DedupeKey:     "task:sample:t-init-1:lina:workflow_step_assigned",
		SourceKind:    "task",
		SourceID:      "t-init-1",
		SourceChannel: "project:sample",
		Reason:        "workflow_step_assigned",
		Priority:      "critical",
		ActorType:     "system",
		Summary:       "项目初始化",
		RefsJSON:      signalRefs,
		PayloadJSON:   signalPayload,
		Status:        "pending",
		CreatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}

	// The task exists ONLY in the DB-backed store (kv_records), as on
	// control-plane deployments. No tasks.yaml is present.
	dbStore := taskstore.NewDB(root, db)
	wtDir := "/opt/data/ws/projects/sample/workspace"
	if err := dbStore.AddTask("sample", "lina", &entity.Task{
		ID:          "t-init-1",
		Title:       "项目初始化: sample",
		Status:      entity.TaskStatusInProgress,
		Type:        "chore",
		CreatedBy:   "admin",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		WorktreeDir: wtDir,
		BranchName:  "main",
		Vars:        map[string]string{"initialization_repo": wtDir},
	}); err != nil {
		t.Fatal(err)
	}

	section, ids, gotDir, gotBranch, targetTaskID, targetProj, err := pendingAttentionSection(root, "sample", "lina", wakeupStrings("zh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sig-wf" {
		t.Fatalf("unexpected ids: %#v", ids)
	}
	if targetTaskID != "t-init-1" || targetProj != "sample" {
		t.Fatalf("DB-backed target task not resolved: task=%q proj=%q", targetTaskID, targetProj)
	}
	if gotDir != wtDir || gotBranch != "main" {
		t.Fatalf("DB-backed worktree not resolved: dir=%q branch=%q, want %q/main", gotDir, gotBranch, wtDir)
	}
	for _, want := range []string{"sig-wf", "workflow_step_assigned"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
}
