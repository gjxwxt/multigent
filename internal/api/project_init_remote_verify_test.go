package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// The init-flow verify endpoint exists because bind-remote initialization
// clones in the sandbox and leaves the platform with no verified remote
// binding — every later GitLab operation (ci_ready pipeline evidence, remote
// adoption, runner binding) fails closed without one. These tests pin the
// endpoint's contract: project-manager authorization, live forge lookup, and
// host-matching on the URL hint.

func TestProjectInitRemoteVerifyWritesBindingFromURLHint(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/react-components-ci", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// A project manager who is not a workspace admin must be able to verify
	// their own project's remote during initialization — the workspace-admin
	// verify endpoint is out of reach for them by design.
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify-init", "admin",
		map[string]any{"connectionId": "conn-gitlab", "cloneUrl": gitlab.URL + "/gao/react-components-ci.git"})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectInitRemoteVerify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	binding, ok, err := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj")
	if err != nil || !ok {
		t.Fatalf("binding not persisted: ok=%v err=%v", ok, err)
	}
	if binding.RemoteProjectID != "58" || binding.PathWithNamespace != "gao/react-components-ci" {
		t.Fatalf("binding = %+v", binding)
	}
	if binding.Source != controldb.BindingSourceExplicitVerify {
		t.Fatalf("source = %q, want explicit-verify", binding.Source)
	}
	if len(runnerBinds) != 0 {
		t.Fatalf("verify is read-only on the forge, got runner binds %v", runnerBinds)
	}
}

func TestProjectInitRemoteVerifyRejectsNonManager(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/react-components-ci", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// "owner" is a plain workspace member with no project access: the URL hint
	// must not become a way to probe or claim a remote for a project one does
	// not manage.
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify-init", "owner",
		map[string]any{"connectionId": "conn-gitlab", "cloneUrl": gitlab.URL + "/gao/react-components-ci.git"})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectInitRemoteVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 before any forge call", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("unauthorized verify must not write a binding")
	}
}

func TestProjectInitRemoteVerifyRejectsHintOnForeignHost(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/react-components-ci", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// The hint only names a path on the PINNED connection's GitLab host: a URL
	// pointing at some other host yields no path at all rather than a lookup.
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify-init", "admin",
		map[string]any{"connectionId": "conn-gitlab", "cloneUrl": "http://evil.example/gao/react-components-ci.git"})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectInitRemoteVerify(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 for off-host hint", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no platform-recorded remote path") {
		t.Fatalf("error should name the missing path problem, got %s", rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("off-host hint must not write a binding")
	}
}

func TestProjectInitRemoteVerifyRejectsUnknownRepo(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/real-repo", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// The forge lookup is the authorization: a hint naming a repository that
	// does not exist on the pinned host cannot produce a binding.
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify-init", "admin",
		map[string]any{"connectionId": "conn-gitlab", "cloneUrl": gitlab.URL + "/gao/nonexistent.git"})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectInitRemoteVerify(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for unknown repo", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("failed verification must not write a binding")
	}
}
