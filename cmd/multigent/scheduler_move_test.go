package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/taskstore"
)

// writeFailingStore delegates to a real store but fails every destination
// write, reproducing the field failure that lost the lifecycle task of the
// hooks-relay rehearsal (2026-09-25) when its step actor changed.
type writeFailingStore struct {
	taskstore.Store
}

func (writeFailingStore) AddTask(project, agent string, t *entity.Task) error {
	return fmt.Errorf("injected destination task write failure")
}

func (writeFailingStore) MoveTask(project, fromAgent, toAgent string, task *entity.Task) error {
	return fmt.Errorf("injected destination task write failure")
}

func newMoveTestStore(t *testing.T) taskstore.Store {
	t.Helper()
	ts, _, _ := newMoveTestStoreWithDB(t)
	return ts
}

func newMoveTestStoreWithDB(t *testing.T) (taskstore.Store, *controldb.SQLiteStore, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Without an agency config the store derives a workspace id without
	// registering the workspace row, and the tasks FK then rejects writes.
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Move Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := taskstore.NewDB(root, db)
	workspaceID := filepath.Base(root)
	if rows, err := db.ListWorkspaces(); err == nil {
		for _, row := range rows {
			if row.Root == root || row.ID != "" {
				workspaceID = row.ID
				break
			}
		}
	}
	return ts, db, workspaceID
}

func seedMoveTask(t *testing.T, ts taskstore.Store, agent string) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	task := &entity.Task{
		ID:        "t-move-repro",
		Title:     "hooks-relay lifecycle task",
		Type:      "feature",
		Status:    entity.TaskStatusInProgress,
		Assignee:  "hooks-relay/" + agent,
		CreatedBy: "workflow",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := ts.AddTask("hooks-relay", agent, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task
}

// The red/green pair for the delete-first regression: a relocation whose
// destination write fails must leave the task readable. With the lossy ordering
// (delete source, then write destination) the failing write removed the record
// from every queue and the run could never be reported again.
func TestRelocateWorkflowTaskKeepsTaskReadableWhenDestinationWriteFails(t *testing.T) {
	ts := newMoveTestStore(t)
	task := seedMoveTask(t, ts, "Mira")

	err := relocateWorkflowTaskToStepActor(writeFailingStore{Store: ts}, "hooks-relay", "Mira", "s2-dev-a", task)
	if err == nil {
		t.Fatalf("relocation with a failing destination write must report an error")
	}
	if _, err := ts.GetTask("hooks-relay", "Mira", task.ID); err != nil {
		t.Fatalf("task lost from the source queue after a failed relocation: %v", err)
	}
	records, err := ts.ListAllTaskRecords("hooks-relay")
	if err != nil {
		t.Fatalf("ListAllTaskRecords: %v", err)
	}
	found := false
	for _, record := range records {
		if record.Task != nil && record.Task.ID == task.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("task missing from the project-wide task listing after a failed relocation")
	}
}

func TestRelocateWorkflowTaskHandsTaskToStepActor(t *testing.T) {
	ts := newMoveTestStore(t)
	task := seedMoveTask(t, ts, "Mira")
	archived := time.Now().UTC()
	task.ArchivedAt = &archived
	task.Status = entity.TaskStatusPending
	task.Assignee = "hooks-relay/s2-dev-a"

	if err := relocateWorkflowTaskToStepActor(ts, "hooks-relay", "Mira", "s2-dev-a", task); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	fresh, err := ts.GetTask("hooks-relay", "s2-dev-a", task.ID)
	if err != nil {
		t.Fatalf("task not readable under the step actor: %v", err)
	}
	if fresh.ArchivedAt != nil {
		t.Fatalf("stale ArchivedAt survived the relocation: %v", fresh.ArchivedAt)
	}
	if fresh.Status != entity.TaskStatusPending || fresh.Assignee != "hooks-relay/s2-dev-a" {
		t.Fatalf("relocated payload not the caller's: status=%s assignee=%s", fresh.Status, fresh.Assignee)
	}
	if _, err := ts.GetTask("hooks-relay", "Mira", task.ID); err == nil {
		t.Fatalf("task still readable under the previous agent after relocation")
	}
}

// When the platform-side mover already handed the task to the step actor, the
// scheduler must persist the fresh payload there instead of failing the move.
func TestRelocateWorkflowTaskPersistsWhenTaskAlreadyMoved(t *testing.T) {
	ts := newMoveTestStore(t)
	task := seedMoveTask(t, ts, "Mira")
	if err := ts.MoveTask("hooks-relay", "Mira", "s2-dev-a", task); err != nil {
		t.Fatalf("pre-move: %v", err)
	}
	task.Status = entity.TaskStatusPending
	task.Assignee = "hooks-relay/s2-dev-a"
	if err := relocateWorkflowTaskToStepActor(ts, "hooks-relay", "Mira", "s2-dev-a", task); err != nil {
		t.Fatalf("relocate after external move: %v", err)
	}
	fresh, err := ts.GetTask("hooks-relay", "s2-dev-a", task.ID)
	if err != nil {
		t.Fatalf("task not readable under the step actor: %v", err)
	}
	if fresh.Status != entity.TaskStatusPending {
		t.Fatalf("payload not persisted to the step actor: status=%s", fresh.Status)
	}
}

// The file backend has no alias keys, so its read-back reads the destination
// queue from disk directly. A destination write that cannot be persisted (here:
// the queue file path is occupied by a directory) must leave the task under its
// current agent.
func TestFileBackendMoveKeepsTaskReadableWhenDestinationWriteFails(t *testing.T) {
	root := t.TempDir()
	ts := taskstore.New(root)
	task := seedMoveTask(t, ts, "Mira")

	queueDir := filepath.Join(root, "projects", "hooks-relay", "agents", "s2-dev-a")
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		t.Fatalf("sabotage destination: %v", err)
	}
	if err := os.Chmod(queueDir, 0o555); err != nil {
		t.Fatalf("sabotage destination perms: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(queueDir, 0o755) })
	if err := relocateWorkflowTaskToStepActor(ts, "hooks-relay", "Mira", "s2-dev-a", task); err == nil {
		t.Fatalf("relocation with an unwritable destination must fail")
	}
	if _, err := ts.GetTask("hooks-relay", "Mira", task.ID); err != nil {
		t.Fatalf("file backend lost the task from the source queue after a failed move: %v", err)
	}
	records, err := ts.ListAllTaskRecords("hooks-relay")
	if err != nil {
		t.Fatalf("ListAllTaskRecords: %v", err)
	}
	for _, record := range records {
		if record.Task != nil && record.Task.ID == task.ID {
			return
		}
	}
	t.Fatalf("file backend task missing from the project-wide listing after a failed move")
}

// The scheduler addresses an agent by the directory name it runs ("S2 Dev A")
// while a workflow step's actor id is the worker name ("s2-dev-a"). Both
// spellings resolve to the same AgentWorker, so their alias sets overlap:
// taskAgentKeys("S2 Dev A") also yields worker.Name == "s2-dev-a". A
// relocation whose source removal loops over every source alias therefore
// deletes the copy that was just written under the destination key. This is
// the hooks-relay loss, reproduced deterministically.
func TestMoveTaskKeepsDestinationWhenSourceAndTargetShareWorkerAliases(t *testing.T) {
	ts, db, workspaceID := newMoveTestStoreWithDB(t)
	seedAliasedWorkerForTest(t, db, workspaceID, "hooks-relay", "S2 Dev A", "s2-dev-a", "S2 Dev A")
	task := seedMoveTask(t, ts, "S2 Dev A")

	if err := ts.MoveTask("hooks-relay", "S2 Dev A", "s2-dev-a", task); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	if _, err := ts.GetTask("hooks-relay", "s2-dev-a", task.ID); err != nil {
		t.Fatalf("task lost after relocating between two spellings of the same agent: %v", err)
	}
	records, err := ts.ListAllTaskRecords("hooks-relay")
	if err != nil {
		t.Fatalf("ListAllTaskRecords: %v", err)
	}
	for _, record := range records {
		if record.Task != nil && record.Task.ID == task.ID {
			return
		}
	}
	t.Fatalf("task missing from the project-wide listing after an alias-overlapping move")
}

func seedAliasedWorkerForTest(t *testing.T, db *controldb.SQLiteStore, workspaceID, project, memberTitle, workerName, displayName string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertAgentWorker(controldb.AgentWorker{
		ID:           "aw-s2-dev-a",
		WorkspaceID:  workspaceID,
		Name:         workerName,
		DisplayName:  displayName,
		Model:        "claude-code",
		ScheduleJSON: "{}",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := db.UpsertProjectMembership(controldb.ProjectMembership{
		ID:          "pm-aw-s2-dev-a",
		WorkspaceID: workspaceID,
		ProjectID:   project,
		MemberType:  "agent_worker",
		MemberID:    "aw-s2-dev-a",
		Role:        "developer",
		Title:       memberTitle,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}
