package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/preview"
)

var errFakeStop = errors.New("fake stop failure")

// fakePreviewEngine records stop calls in order so cleanup sequencing can be
// asserted without touching docker. GetInstance/Stop always succeed unless
// the task is flagged in failStopFor.
type fakePreviewEngine struct {
	instances   map[string]*preview.PreviewInstance
	stopped     []string
	stopErr     error
	failStopFor map[string]bool
}

func newFakePreviewEngine() *fakePreviewEngine {
	return &fakePreviewEngine{
		instances:   map[string]*preview.PreviewInstance{},
		failStopFor: map[string]bool{},
	}
}

func (f *fakePreviewEngine) GetInstance(taskID string) (*preview.PreviewInstance, bool) {
	inst, ok := f.instances[taskID]
	return inst, ok
}

func (f *fakePreviewEngine) StartEphemeralPreviewWithRuntime(_ context.Context, taskID, projectName, worktreeDir string, _ preview.RuntimeSelection) (*preview.PreviewInstance, error) {
	return nil, nil
}

func (f *fakePreviewEngine) StartSnapshotPreviewWithRuntime(_ context.Context, taskID, projectName, worktreeDir string, _ preview.RuntimeSelection) (*preview.PreviewInstance, error) {
	return nil, nil
}

func (f *fakePreviewEngine) StopEphemeralPreview(taskID string) error {
	if f.failStopFor[taskID] {
		return f.stopErr
	}
	f.stopped = append(f.stopped, taskID)
	delete(f.instances, taskID)
	return nil
}

func (f *fakePreviewEngine) Reconcile(_ context.Context) error { return nil }

func (f *fakePreviewEngine) SeedInstanceForTest(inst *preview.PreviewInstance) {
	f.instances[inst.TaskID] = inst
}

func newTestWorktreeManager(t *testing.T) *gitworktree.Manager {
	t.Helper()
	return gitworktree.NewManager()
}

// seedResourceProject builds a project whose workspace is a real git repo so
// worktree paths resolve naturally.
func seedResourceProject(t *testing.T, taskStatus entity.TaskStatus) (*Server, string, *entity.Task, string) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	s.worktreeMgr = newTestWorktreeManager(t)

	root := s.st.ProjectDir("resproj")
	wsDir := filepath.Join(root, "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wsDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(wsDir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "init")

	if err := s.st.SaveProject("resproj", &entity.Project{Name: "resproj", Repo: wsDir}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "resproj", "agent", "aw-res", "pm-res")
	task := &entity.Task{
		ID:         "t-res-1",
		Title:      "resource test",
		Assignee:   "resproj/agent",
		Status:     taskStatus,
		Prompt:     "x",
		BaseBranch: "main",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	if err := s.ts.AddTask("resproj", "agent", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return s, workspaceID, task, wsDir
}

func TestGetTaskResourcesReportsWorktreeAndPreview(t *testing.T) {
	s, _, task, wsDir := seedResourceProject(t, entity.TaskStatusDoneSuccess)
	engine := newFakePreviewEngine()
	engine.SeedInstanceForTest(&preview.PreviewInstance{TaskID: task.ID, Status: "running", Port: 33111})
	s.previewEngine = engine

	// Materialize a real worktree so the resource report has something to read.
	wtDir := taskWorktreeDir(task, wsDir, task.ID)
	if out, err := exec.Command("git", "-C", wsDir, "worktree", "add", wtDir, "-b", "feature/t-res-1").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}

	req := providerTestRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/resources", "admin", nil)
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handleGetTaskResources(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp taskResourcesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Worktree.Exists {
		t.Fatalf("expected worktree to exist: %s", w.Body.String())
	}
	if resp.Worktree.Branch != "feature/t-res-1" {
		t.Fatalf("expected branch feature/t-res-1, got %q", resp.Worktree.Branch)
	}
	if resp.Worktree.DirtyFiles != 0 {
		t.Fatalf("expected clean tree, got dirty=%d", resp.Worktree.DirtyFiles)
	}
	if resp.Worktree.DiskBytes == nil || *resp.Worktree.DiskBytes <= 0 {
		t.Fatalf("expected positive disk bytes: %v", resp.Worktree.DiskBytes)
	}
	if resp.Preview == nil || resp.Preview.Status != "running" || resp.Preview.Port != 33111 {
		t.Fatalf("unexpected preview: %+v", resp.Preview)
	}
	if resp.Locked {
		t.Fatalf("terminal task must not be locked")
	}
}

func TestGetTaskResourcesLockedForRunningTask(t *testing.T) {
	s, _, task, _ := seedResourceProject(t, entity.TaskStatusInProgress)
	s.previewEngine = newFakePreviewEngine()

	req := providerTestRequest(http.MethodGet, "/api/v1/projects/resproj/tasks/"+task.ID+"/resources", "admin", nil)
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handleGetTaskResources(w, req)
	var resp taskResourcesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Locked || resp.LockReason == "" {
		t.Fatalf("expected locked running task, got %+v", resp)
	}
}

func TestCleanupRejectsNonTerminalTask(t *testing.T) {
	s, _, task, _ := seedResourceProject(t, entity.TaskStatusInProgress)
	s.previewEngine = newFakePreviewEngine()

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/resources/worktree/cleanup", "admin", nil)
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorktreeCleanup(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for in_progress task, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCleanupStopsPreviewBeforeCommittingAndRemoving(t *testing.T) {
	s, _, task, wsDir := seedResourceProject(t, entity.TaskStatusDoneSuccess)
	engine := newFakePreviewEngine()
	engine.SeedInstanceForTest(&preview.PreviewInstance{TaskID: task.ID, Status: "running", Port: 33111})
	s.previewEngine = engine

	// Materialize a real worktree with dirty content; cleanup must checkpoint
	// it into a commit before removing the directory.
	wtDir := taskWorktreeDir(task, wsDir, task.ID)
	if out, err := exec.Command("git", "-C", wsDir, "worktree", "add", wtDir, "-b", "feature/t-res-1").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "note.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/resources/worktree/cleanup", "admin", nil)
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorktreeCleanup(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Cleaned       bool   `json:"cleaned"`
		CheckpointSHA string `json:"checkpointSHA"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Cleaned || resp.CheckpointSHA == "" {
		t.Fatalf("expected cleaned with checkpoint, got %+v", resp)
	}

	// Order: the preview was stopped, and the worktree directory is gone.
	if len(engine.stopped) != 1 || engine.stopped[0] != task.ID {
		t.Fatalf("expected exactly one preview stop for the task: %v", engine.stopped)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree directory should be removed, stat err: %v", err)
	}
	// The checkpoint commit lives on the branch ref in the main repo.
	branch := exec.Command("git", "-C", wsDir, "log", "--format=%s", "feature/t-res-1", "-1")
	out, err := branch.Output()
	if err != nil {
		t.Fatalf("checkpoint commit must survive on the branch: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty branch log")
	}
}

func TestCleanupAbortsWhenPreviewStopFails(t *testing.T) {
	s, _, task, wsDir := seedResourceProject(t, entity.TaskStatusDoneSuccess)
	engine := newFakePreviewEngine()
	engine.SeedInstanceForTest(&preview.PreviewInstance{TaskID: task.ID, Status: "running"})
	engine.failStopFor[task.ID] = true
	engine.stopErr = errFakeStop
	s.previewEngine = engine

	wtDir := taskWorktreeDir(task, wsDir, task.ID)
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/resources/worktree/cleanup", "admin", nil)
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorktreeCleanup(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 when stop fails, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("worktree must be untouched when preview stop fails: %v", err)
	}
}

func TestCleanupIdempotentWhenNothingExists(t *testing.T) {
	s, _, task, _ := seedResourceProject(t, entity.TaskStatusCancelled)
	s.previewEngine = newFakePreviewEngine()

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/resproj/tasks/"+task.ID+"/resources/worktree/cleanup", "admin", nil)
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskWorktreeCleanup(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected idempotent 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCommitWorktreeStateCheckpointsDirtyTree(t *testing.T) {
	// Direct unit test of the gitworktree primitive the cleanup relies on.
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "init")

	m := newTestWorktreeManager(t)
	sha, wasDirty, err := m.CommitWorktreeState(repo, "clean pass")
	if err != nil || wasDirty {
		t.Fatalf("clean tree: sha=%s wasDirty=%v err=%v", sha, wasDirty, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha2, wasDirty2, err := m.CommitWorktreeState(repo, "checkpoint")
	if err != nil || !wasDirty2 {
		t.Fatalf("dirty tree: sha=%s wasDirty=%v err=%v", sha2, wasDirty2, err)
	}
	if sha2 == sha {
		t.Fatal("checkpoint commit did not advance HEAD")
	}
	if out := git("status", "--porcelain"); out != "" {
		t.Fatalf("tree not clean after checkpoint: %q", out)
	}
	logOut := git("log", "--format=%s", "-1")
	if want := "checkpoint"; !containsLine(logOut, want) {
		t.Fatalf("expected checkpoint commit message, got %q", logOut)
	}
}

func containsLine(text, want string) bool {
	for _, line := range splitLines(text) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}
