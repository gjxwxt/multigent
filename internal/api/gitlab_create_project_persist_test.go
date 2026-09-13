package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// fakeGitLabCreateServer answers POST /api/v4/projects with a fixed
// create-repository response so handleGitLabCreateProject can be exercised
// without a real GitLab.
func fakeGitLabCreateServer(t *testing.T, pathWithNamespace string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":58,"name":"my-repo","path_with_namespace":"` + pathWithNamespace + `","web_url":"http://gitlab.internal/` + pathWithNamespace + `","http_url_to_repo":"http://gitlab.internal/` + pathWithNamespace + `.git","default_branch":"main"}`))
	})
	return httptest.NewServer(mux)
}

// The create-repo endpoint must persist the platform-controlled remote
// identity server-side when the request names a project: every field comes
// from the GitLab API response, so the record is independent of anything the
// client could have echoed. This record is what originAdoptAuthorized trusts.
func TestGitLabCreateProjectPersistsRemoteIdentity(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	gitlab := fakeGitLabCreateServer(t, "gao/my-repo")
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/integrations/gitlab/projects", "admin",
		map[string]any{"connectionId": "conn-gitlab", "name": "my-repo", "path": "my-repo", "project": "proj"})
	rec := httptest.NewRecorder()
	s.handleGitLabCreateProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create project: status %d body %s", rec.Code, rec.Body.String())
	}

	p, err := s.st.Project("proj")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RemoteProvider != "gitlab" || p.RemoteProjectID != "58" {
		t.Fatalf("remote identity not persisted: provider=%q id=%q", p.RemoteProvider, p.RemoteProjectID)
	}
	if p.CloneURL != "http://gitlab.internal/gao/my-repo.git" {
		t.Fatalf("CloneURL = %q, want the API-response clone URL", p.CloneURL)
	}
	if p.RemoteURL != "http://gitlab.internal/gao/my-repo" {
		t.Fatalf("RemoteURL = %q, want the API-response web URL", p.RemoteURL)
	}
	if p.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want main", p.DefaultBranch)
	}
}

// The persisted identity must come from the API response only: a client
// cannot use the project field to smuggle in arbitrary remote fields — there
// is no field in the request for that, and a nonexistent project is an error,
// not a silent write.
func TestGitLabCreateProjectPersistRejectsUnknownProject(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	gitlab := fakeGitLabCreateServer(t, "gao/my-repo")
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab", gitlab.URL)

	req := providerTestRequest(http.MethodPost, "/api/v1/integrations/gitlab/projects", "admin",
		map[string]any{"connectionId": "conn-gitlab", "name": "my-repo", "path": "my-repo", "project": "no-such-project"})
	rec := httptest.NewRecorder()
	s.handleGitLabCreateProject(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unknown project: status %d, want 500 (remote exists but record failed)", rec.Code)
	}
}
