package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentdir "github.com/multigent/multigent/internal/agentdir"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Security fix batch (post-P0.6 review): every test here asserts a fail-closed
// gate or an authorization boundary added by the fix(security) commit.

// seedTaskWithWorker seeds an agent worker + project membership so
// findTaskInProject can resolve the task, then stores it in the task store.
func seedTaskWithWorker(t *testing.T, s *Server, workspaceID, project, taskID, mrIID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:          "aw-fix-" + taskID,
		WorkspaceID: workspaceID,
		Name:        "fix-agent-" + taskID,
		DisplayName: "fix-agent-" + taskID,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
		ID:          "pm-fix-" + taskID,
		WorkspaceID: workspaceID,
		ProjectID:   project,
		MemberType:  agentdir.MemberTypeAgentWorker,
		MemberID:    "aw-fix-" + taskID,
		Role:        "developer",
		Title:       "fix-agent-" + taskID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := s.ts.AddTask(project, "fix-agent-"+taskID, &entity.Task{
		ID:          taskID,
		Title:       "fix gate task",
		Status:      entity.TaskStatusDoneSuccess,
		RemoteMRIID: mrIID,
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// 1. handleMergeTaskMR: a caller without project-management rights gets 403
// before any task lookup, remote MR read/merge, or local task mutation.
func TestMergeTaskMRRejectsNonManagerWithZeroSideEffects(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/merge-target", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	if err := s.st.SaveProject("proj", &entity.Project{
		Name:             "proj",
		RemoteProvider:   "gitlab",
		RemoteProjectID:  "58",
		RemoteConnection: "conn-gitlab",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seedVerifiedBinding(t, s, workspaceID, "proj", "conn-gitlab", "gao/merge-target", "58")
	seedTaskWithWorker(t, s, workspaceID, "proj", "t-1", "7")

	// "owner" is a plain workspace member with no rights on "proj".
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/tasks/t-1/merge", "owner", nil)
	req.SetPathValue("name", "proj")
	req.SetPathValue("id", "t-1")
	rec := httptest.NewRecorder()
	s.handleMergeTaskMR(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}

	// Zero side effects: task untouched in the store.
	task, _, err := s.findTaskInProject("proj", "t-1")
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if task.RemoteMRState != "" {
		t.Fatalf("task RemoteMRState mutated by unauthorized call: %q", task.RemoteMRState)
	}
	if task.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("task status mutated: %q", task.Status)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("upstream saw calls from unauthorized request: %v", runnerBinds)
	}
}

// 2. prepareTaskDelivery GitHub branch: no verified GitHub binding exists, so
// the remote-write path must fail closed with an explicit error and perform
// no forge call.
func TestPrepareTaskDeliveryGitHubFailsClosed(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	s.worktreeMgr = newTestWorktreeManager(t)

	if err := s.st.SaveProject("proj", &entity.Project{
		Name:            "proj",
		RemoteProvider:  "github",
		RemoteProjectID: "octocat/repo",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	task := &entity.Task{ID: "t-gh", Status: entity.TaskStatusDoneSuccess, RemoteMRIID: "3"}

	err := s.prepareTaskDelivery(providerTestRequest(http.MethodPost, "/api/v1/projects/proj/tasks/t-gh/merge", "admin", nil), "proj", task, "")
	if err == nil {
		t.Fatal("GitHub remote delivery must fail closed without a verified binding")
	}
	if !strings.Contains(err.Error(), "verified GitHub remote binding") {
		t.Fatalf("error = %v, want explicit controlled-binding message", err)
	}
	if task.RemoteMRState == "merged" {
		t.Fatal("task must not be marked merged when delivery fails closed")
	}
}

// 3. adoptRemoteAfterSync: a forged RemoteConnection pointing at ANOTHER
// connection must never be consulted — the binding's connection is the only
// host, and the adoption still works against the binding path.
func TestAdoptRemoteAfterSyncForgedRemoteConnectionSeesZeroUpstream(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var forgedHits []string
	forged := fakeGitLabProjectServer(t, "evil/forged", &forgedHits)
	defer forged.Close()

	var controlledBinds []string
	controlled := fakeGitLabProjectServer(t, "gao/controlled", &controlledBinds)
	defer controlled.Close()

	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", controlled.URL)
	seedGitLabConnection(t, s, workspaceID, "conn-forged", forged.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, controlled.URL+"/gao/controlled.git")
	if err := s.st.SaveProject("proj", &entity.Project{
		Name:             "proj",
		Repo:             repoDir,
		RemoteProvider:   "gitlab",
		RemoteConnection: "conn-forged", // forged via PUT — must be ignored
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seedVerifiedBinding(t, s, workspaceID, "proj", "conn-gitlab", "gao/controlled", "58")

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, err := s.st.Project("proj")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RemoteProjectID != "58" {
		t.Fatalf("controlled binding path must adopt, got RemoteProjectID=%q", p.RemoteProjectID)
	}
	if len(forgedHits) != 0 {
		t.Fatalf("forged connection upstream must see zero calls, got %v", forgedHits)
	}
	if len(controlledBinds) != 1 || controlledBinds[0] != "7" {
		t.Fatalf("controlled host must serve adoption + runner bind, got %v", controlledBinds)
	}
}

// 4. Project deletion: the verified remote binding DELETE must live inside
// DeleteProjectControlPlaneScope's single transaction. Failure injection for
// this lives in internal/db (project_channel_links_atomic_test.go) — the
// store's raw handle is package-private.

// 5. GitLab create with project param: the success path persists the binding
// with exactly connectionId / remoteProjectId / path / source=platform-create.
func TestGitLabCreateProjectWritesPlatformCreateBinding(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	gitlab := fakeGitLabCreateServer(t, "gao/new-repo")
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/integrations/gitlab/projects", "admin",
		map[string]any{"connectionId": "conn-gitlab", "name": "new-repo", "path": "new-repo", "project": "proj"})
	rec := httptest.NewRecorder()
	s.handleGitLabCreateProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}

	binding, ok, err := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj")
	if err != nil || !ok {
		t.Fatalf("binding must exist after create: ok=%v err=%v", ok, err)
	}
	if binding.ConnectionID != "conn-gitlab" {
		t.Fatalf("binding connectionId = %q, want conn-gitlab", binding.ConnectionID)
	}
	if binding.RemoteProjectID != "58" {
		t.Fatalf("binding remoteProjectId = %q, want 58", binding.RemoteProjectID)
	}
	if binding.PathWithNamespace != "gao/new-repo" {
		t.Fatalf("binding path = %q, want gao/new-repo", binding.PathWithNamespace)
	}
	if binding.Source != controldb.BindingSourcePlatformCreate {
		t.Fatalf("binding source = %q, want platform-create", binding.Source)
	}
}
