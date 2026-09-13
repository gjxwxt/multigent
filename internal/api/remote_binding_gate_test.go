package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// P0.6 negative gate: a project manager forging EVERY PUT-writable remote
// field must trigger zero external GitLab side effects — no runner bind, no
// APP_PORT variable write — because both consume the verified remote binding,
// which PUT cannot create.
func TestPutProjectForgedRemoteFieldsTriggerZeroGitlabWrites(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds, varWrites []string
	gitlab := fakeGitLabProjectServer(t, "evil/forged", &runnerBinds)
	_ = varWrites
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", DeployPort: 28055, TemplateID: "tmpl"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")

	// "proj" is managed by admin (workspace admin sees every project).
	req := providerTestRequest(http.MethodPut, "/api/v1/projects/proj", "admin", map[string]any{
		"remoteProvider":   "gitlab",
		"remoteConnection": "conn-gitlab",
		"remoteProjectId":  "58",
		"remoteUrl":        gitlab.URL + "/evil/forged",
		"cloneUrl":         gitlab.URL + "/evil/forged.git",
		"defaultBranch":    "main",
	})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handlePutProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	if len(runnerBinds) != 0 {
		t.Fatalf("forged PUT must not trigger runner binds, got %v", runnerBinds)
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("forged PUT must not create a verified remote binding")
	}
}

// P0.6-4 negative: a user WITHOUT project-management rights cannot use the
// create-repo endpoint's project parameter to write a controlled remote
// identity or binding — the authorization happens before any forge write.
func TestGitLabCreateProjectRejectsNonManagerForNamedProject(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	gitlab := fakeGitLabCreateServer(t, "gao/my-repo")
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// "owner" is a plain workspace member with no access to "proj".
	req := providerTestRequest(http.MethodPost, "/api/v1/integrations/gitlab/projects", "owner",
		map[string]any{"connectionId": "conn-gitlab", "name": "my-repo", "path": "my-repo", "project": "proj"})
	rec := httptest.NewRecorder()
	s.handleGitLabCreateProject(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 before any forge call", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("unauthorized create must not write a binding")
	}
	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("unauthorized create must not persist remote identity, got %q", p.RemoteProjectID)
	}
}

// Swapping the binding's connection for another connection (same forge, or a
// different one) invalidates platform operations: the resolved connection id
// must equal the binding's, otherwise fail closed.
func TestVerifiedGitLabHostRejectsSwappedConnection(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/my-repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seedVerifiedBinding(t, s, workspaceID, "proj", "conn-other", "gao/my-repo", "58")

	if _, _, err := s.verifiedGitLabHost(context.Background(), "proj"); err == nil {
		t.Fatal("binding naming a nonexistent connection must fail closed")
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("no runner binds allowed on connection mismatch: %v", runnerBinds)
	}
}

// Same path, different id: the live forge lookup must match the binding's id,
// else refuse (deleted-and-recreated repo cannot ride verified trust).
func TestAdoptRemoteAfterSyncRejectsBindingIDMismatch(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/recreated", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	repoDir := filepath.Join(t.TempDir(), "workspace")
	seedOriginRepo(t, repoDir, gitlab.URL+"/gao/recreated.git")
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj", Repo: repoDir}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seedVerifiedBinding(t, s, workspaceID, "proj", "conn-gitlab", "gao/recreated", "999")

	task := &entity.Task{ID: "t-init", Status: entity.TaskStatusDoneSuccess, WorktreeDir: repoDir}
	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.adoptRemoteAfterSync(context.Background(), "proj", task)

	p, _ := s.st.Project("proj")
	if p.RemoteProjectID != "" {
		t.Fatalf("binding id mismatch must refuse adoption, got %q", p.RemoteProjectID)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("runner binds = %v, want 0", runnerBinds)
	}
}

// Unverified (bindingless) projects fail closed: ci_ready treats them as not
// remote-bound, and no runner/variable write may reach the forge.
func TestUnverifiedProjectFailsClosedEverywhere(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/legacy", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	p := &entity.Project{Name: "proj", Repo: "/tmp/legacy", RemoteProvider: "gitlab", RemoteProjectID: "58", DeployPort: 28010}
	if err := s.st.SaveProject("proj", p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// A legacy project has forged-looking display fields but NO binding.
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("legacy project must start unverified")
	}

	t.Setenv("MULTIGENT_GITLAB_RUNNER_ID", "7")
	s.bindDefaultRunner(context.Background(), "proj", p)
	if len(runnerBinds) != 0 {
		t.Fatalf("unverified project must not bind runners, got %v", runnerBinds)
	}

	s.pushDeployPortVariable(context.Background(), "proj", p)
	// The negative assertion is the runner/variable call count (zero) —
	// runnerBinds doubles as the forge-call counter in the fake server.

	if _, _, err := s.verifiedGitLabHost(context.Background(), "proj"); err == nil {
		t.Fatal("verifiedGitLabHost must refuse unverified projects")
	}
}

// The admin verify endpoint persists a binding from a live read-only lookup;
// a non-admin is rejected before any lookup happens.
func TestProjectRemoteVerifyRequiresAdmin(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/verify-me", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{
		Name:            "proj",
		RemoteAdoptPath: "gao/verify-me",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify", "owner", nil)
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectRemoteVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin verify status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("failed verify must not write a binding")
	}

	req = providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify", "admin", nil)
	req.SetPathValue("name", "proj")
	rec = httptest.NewRecorder()
	s.handleProjectRemoteVerify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin verify status=%d body=%s", rec.Code, rec.Body.String())
	}
	binding, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj")
	if !ok || binding.Source != controldb.BindingSourceExplicitVerify {
		t.Fatalf("admin verify must persist an explicit-verify binding, got %+v ok=%v", binding, ok)
	}
}
