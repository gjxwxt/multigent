package api

// E1 closeout: the verify-init flow's forge lookup is its authorization, but
// an UNAUTHENTICATED GET still returns 200 for a public repository. A
// connection with no stored credential would therefore "verify" a binding it
// can never use — private clone, push, and pipeline-evidence reads all fail
// later. The verify step now refuses credentialless connections up front.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// seedTokenlessGitLabConnection registers a workspace-default gitlab
// connection whose secret carries an empty apiKey — the reachable shape of a
// placeholder connection (resolution succeeds, every authenticated call is
// anonymous).
func seedTokenlessGitLabConnection(t *testing.T, s *Server, workspaceID, connID, baseURL string) {
	t.Helper()
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "gitlab",
		ConnectionName: "conn-" + connID,
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "api_key",
		Status:         "active",
		IsDefault:      true,
		ProfileJSON:    `{"connectionName":"conn-` + connID + `","baseUrl":"` + baseURL + `"}`,
	}); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"apiKey": "", "baseUrl": baseURL})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("upsert secret: %v", err)
	}
}

// TestProjectInitRemoteVerifyRejectsTokenlessConnection: a credentialless
// connection must not mint a verified binding even though the anonymous
// lookup of a public repo succeeds (fake server answers 200 unconditionally).
func TestProjectInitRemoteVerifyRejectsTokenlessConnection(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/react-components-ci", &runnerBinds)
	defer gitlab.Close()
	seedTokenlessGitLabConnection(t, s, workspaceID, "conn-gitlab-nokey", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify-init", "admin",
		map[string]any{"connectionId": "conn-gitlab-nokey", "cloneUrl": gitlab.URL + "/gao/react-components-ci.git"})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectInitRemoteVerify(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tokenless connection must be refused with 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no stored credential") {
		t.Fatalf("rejection must name the missing credential: %s", rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); ok {
		t.Fatal("a credentialless connection must not write a verified binding")
	}
}

// TestProjectInitRemoteVerifyStillAcceptsCredentialedConnection: the guard
// must not break the normal path — the same seeded shape used by the
// neighboring tests (apiKey present) still writes the binding.
func TestProjectInitRemoteVerifyStillAcceptsCredentialedConnection(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)

	var runnerBinds []string
	gitlab := fakeGitLabProjectServer(t, "gao/react-components-ci", &runnerBinds)
	defer gitlab.Close()
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab-ok", gitlab.URL)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/proj/remote/verify-init", "admin",
		map[string]any{"connectionId": "conn-gitlab-ok", "cloneUrl": gitlab.URL + "/gao/react-components-ci.git"})
	req.SetPathValue("name", "proj")
	rec := httptest.NewRecorder()
	s.handleProjectInitRemoteVerify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("credentialed verify must still succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.controlDB.VerifiedRemoteBindingFor(workspaceID, "proj"); !ok {
		t.Fatal("credentialed verify must write the binding")
	}
}
