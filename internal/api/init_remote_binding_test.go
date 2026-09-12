package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// fakeGitLabProjectServer answers GET /api/v4/projects/:path with a fixed
// project record and records every POST /projects/:id/runners binding, so
// adoptRemoteAfterSync can be exercised without a real GitLab.
func fakeGitLabProjectServer(t *testing.T, projectPath string, gotRunnerBinds *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		decoded, err := url.PathUnescape(raw)
		if err != nil || !strings.EqualFold(decoded, projectPath) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":58,"name":"v2","path_with_namespace":"` + projectPath + `","http_url_to_repo":"http://gitlab.internal/` + projectPath + `.git","default_branch":"main"}`))
	})
	mux.HandleFunc("POST /api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/runners") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		*gotRunnerBinds = append(*gotRunnerBinds, r.FormValue("runner_id"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	return httptest.NewServer(mux)
}

func seedOriginRepo(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo dir: %v", err)
	}
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "remote", "add", "origin", originURL},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args[1:], err, out)
		}
	}
}

// seedGitLabConnection registers a workspace-default gitlab connection whose
// base URL points at the fake server, mirroring the production resolveCodeHost
// path (profile JSON carries the baseUrl).
func seedGitLabConnection(t *testing.T, s *Server, workspaceID, connID, baseURL string) {
	t.Helper()
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "gitlab",
		ConnectionName: "local-gitlab",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "api_key",
		Status:         "active",
		IsDefault:      true,
		ProfileJSON:    `{"connectionName":"local-gitlab","baseUrl":"` + baseURL + `"}`,
	}); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"apiKey": "test-token", "baseUrl": baseURL})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("upsert secret: %v", err)
	}
}

func TestAdoptRemoteAfterSyncBindsRunner(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "root/p15-init-spring-v2", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/root/p15-init-spring-v2.git")
	if err := s.st.SaveProject("p15-init-spring-v2", &entity.Project{
		Name:     "p15-init-spring-v2",
		Repo:     repoDir,
		CloneURL: gitlab.URL + "/root/p15-init-spring-v2.git", // platform create-repo record
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	s.adoptRemoteAfterSync(context.Background(), "p15-init-spring-v2", task)

	p, err := s.st.Project("p15-init-spring-v2")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RemoteProvider != "gitlab" {
		t.Fatalf("RemoteProvider = %q, want gitlab", p.RemoteProvider)
	}
	if p.RemoteProjectID != "58" {
		t.Fatalf("RemoteProjectID = %q, want 58", p.RemoteProjectID)
	}
	if p.RemoteURL == "" || !strings.Contains(p.RemoteURL, "p15-init-spring-v2") {
		t.Fatalf("RemoteURL not persisted: %q", p.RemoteURL)
	}
	if p.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", p.DefaultBranch)
	}
	if len(runnerBinds) != 1 || runnerBinds[0] != "7" {
		t.Fatalf("runner binds = %v, want [7]", runnerBinds)
	}
}

// Regression: the step-complete hook must judge the STEP outcome, not the task
// record. The workflow reuses one task across steps, so by hook time a
// mid-workflow transition has already reset the task to pending for the next
// step — the v3 canary's sync step completed with the task in "pending" and
// the old done_success guard skipped adoption.
func TestAdoptRemoteIfNeededAfterStepFiresForPendingTask(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "root/mid-step", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/root/mid-step.git")
	if err := s.st.SaveProject("proj", &entity.Project{
		Name:     "proj",
		Repo:     repoDir,
		CloneURL: gitlab.URL + "/root/mid-step.git", // platform create-repo record
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{
		ID:          "t-init",
		Status:      entity.TaskStatusPending, // reset for the next step
		WorktreeDir: repoDir,
	}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.adoptRemoteIfNeededAfterStep("proj", task, "completed", workflowstore.ProjectInitializationWorkflowID, "sync")

	p, err := s.st.Project("proj")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RemoteProjectID != "58" {
		t.Fatalf("RemoteProjectID = %q, want 58 (mid-workflow pending task must still adopt)", p.RemoteProjectID)
	}
	if len(runnerBinds) != 1 || runnerBinds[0] != "7" {
		t.Fatalf("runner binds = %v, want [7]", runnerBinds)
	}
}

func TestAdoptRemoteIfNeededAfterStepSkipsFailedStep(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "root/failed", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/root/failed.git")
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", Repo: repoDir}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusPending, WorktreeDir: repoDir}
	s.adoptRemoteIfNeededAfterStep("proj", task, "failed", workflowstore.ProjectInitializationWorkflowID, "sync")

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("failed step must not adopt, got %q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("unexpected runner binds: %v", runnerBinds)
	}
}

func TestAdoptRemoteAfterSyncSkipsWhenAlreadyBound(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "root/x", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	if err := s.st.SaveProject("proj", &entity.Project{
		Name:            "proj",
		RemoteProvider:  "gitlab",
		RemoteProjectID: "42",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess}
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "42" {
		t.Fatalf("RemoteProjectID changed to %q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("unexpected runner binds: %v", runnerBinds)
	}
}

func TestAdoptRemoteAfterSyncIgnoresForeignOrigin(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "root/other", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, "https://github.com/acme/widgets.git")
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", Repo: repoDir}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("foreign origin must not be adopted, got %q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("unexpected runner binds: %v", runnerBinds)
	}
}

// Regression (P0, review 2026-09-14): the worktree origin is agent-writable.
// An origin pointing at ANOTHER repository on the SAME pinned GitLab must not
// be adopted — otherwise the agent could steer RemoteProjectID (and with it
// the default runner binding) at a repo it does not own. Adoption requires a
// platform-recorded remote identity (CloneURL), not just same-host.
func TestAdoptRemoteAfterSyncRejectsSameHostForeignRepo(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "victims/secret-repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/victims/secret-repo.git")
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", Repo: repoDir}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("same-host foreign repo must not be adopted, got %q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("runner must not bind to agent-chosen repo, got %v", runnerBinds)
	}
}

// A RemoteURL alone must never authorize adoption. remoteUrl is accepted by
// handlePutProject from any project manager, so a forged PUT (or the standard
// init flow's own metadata echo) can make RemoteURL match whatever origin the
// agent points at — the agent would become its own adoption authority. The
// only client-independent record is CloneURL (see the A2 hardening: the
// create-repo endpoint persists the platform identity server-side).
func TestAdoptRemoteAfterSyncRemoteURLAloneDoesNotAuthorize(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/my-repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/gao/my-repo.git")
	if err := s.st.SaveProject("proj", &entity.Project{
		Name:      "proj",
		Repo:      repoDir,
		RemoteURL: gitlab.URL + "/gao/my-repo", // exactly what the agent pushed to; still no authority
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("client-writable RemoteURL must not authorize adoption, got RemoteProjectID=%q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("runner must not bind via RemoteURL-only record, got %v", runnerBinds)
	}
}

// A stale agent-controlled RemoteURL must not authorize either: the standard
// init PUT carries remote metadata from the client, so a value merely echoing
// the agent's chosen origin would make the agent its own adoption authority.
// Only CloneURL — the platform create-repo flow's API-response record —
// authorizes.
func TestAdoptRemoteAfterSyncCloneURLAuthorizes(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/my-repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/gao/my-repo.git")
	if err := s.st.SaveProject("proj", &entity.Project{
		Name:      "proj",
		Repo:      repoDir,
		CloneURL:  gitlab.URL + "/gao/my-repo.git",
		RemoteURL: gitlab.URL + "/gao/other-repo", // stale/echoed value must NOT authorize
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, err := s.st.Project("proj")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RemoteProjectID != "58" {
		t.Fatalf("platform-recorded CloneURL must authorize adoption, got RemoteProjectID=%q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 1 {
		t.Fatalf("runner binds = %v, want 1", runnerBinds)
	}
}

// The operator allowlist env is the second authorized path: origins inside an
// explicitly trusted namespace may be adopted even without a CloneURL record.
func TestAdoptRemoteAfterSyncAllowlistedNamespaceAuthorizes(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	t.Setenv(originAdoptAuthorizedNamespaceEnv, "platform/sandbox,others")

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "platform/sandbox/init-repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/platform/sandbox/init-repo.git")
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", Repo: repoDir}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "58" {
		t.Fatalf("allowlisted namespace must adopt, got RemoteProjectID=%q", p.RemoteProjectID)
	}
}

// Allowlist matching is prefix-on-path-segment: "platform" must not match
// "platform-evil/...", and a same-host repo outside the allowlist stays
// rejected.
func TestAdoptRemoteAfterSyncAllowlistPrefixIsSegmentExact(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	t.Setenv(originAdoptAuthorizedNamespaceEnv, "platform")

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "platform-evil/repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/platform-evil/repo.git")
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", Repo: repoDir}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("namespace prefix must match on segment boundary, got %q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("unexpected runner binds: %v", runnerBinds)
	}
}

func TestGitlabProjectPathFromURL(t *testing.T) {
	host := codehost.NewGitLabHost(codehost.GitLabConfig{BaseURL: "http://192.168.139.3:8083", Token: "t"})
	cases := []struct {
		url  string
		want string
	}{
		{"http://192.168.139.3:8083/root/p15-init-spring-v2.git", "root/p15-init-spring-v2"},
		{"http://192.168.139.3:8083/root/plain", "root/plain"},
		// a genuinely foreign host is rejected — including "localhost", which
		// from the server's own network position is NOT the GitLab
		{"https://github.com/acme/widgets.git", ""},
		{"http://gitlab.example.com/root/other.git", ""},
		{"http://localhost:8083/root/other.git", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := gitlabProjectPathFromURL(tc.url, host); got != tc.want {
			t.Fatalf("gitlabProjectPathFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
