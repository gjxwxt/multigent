package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/ciready"
	"github.com/multigent/multigent/internal/deliverymode"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runtimeauth"
)

// seedCIReadyProject materializes a minimal git repo whose deterministic
// checks can pass (Ensure seeds the baseline files itself) and binds the
// project record to it. Remote binding state is decided per test.
func seedCIReadyProject(t *testing.T, s *Server, name string, remoteBound bool) *entity.Project {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "repo")
	p := &entity.Project{Name: name, Repo: repoDir}
	if remoteBound {
		p.RemoteProvider = "gitlab"
		p.RemoteProjectID = "77"
	}
	if err := s.st.SaveProject(name, p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return p
}

// runtimeRequest builds a request carrying a runtime principal in the same
// context key the auth middleware uses, so handleRuntimeCIReady sees a valid
// task.use principal without issuing a real token.
func runtimeRequest(method, target, project string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	principal := runtimeauth.Principal{
		WorkspaceID:  "ws",
		Project:      project,
		Agent:        "runner",
		Capabilities: []string{"task.use"},
	}
	return req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, principal))
}

func runCIReady(s *Server, project string, waitSeconds string) (int, *ciReadyResponse) {
	req := runtimeRequest(http.MethodPost, "/api/v1/runtime/ci-ready?wait_seconds="+waitSeconds, project)
	req.SetPathValue("name", project)
	rec := httptest.NewRecorder()
	s.handleRuntimeCIReady(rec, req)
	resp := &ciReadyResponse{}
	_ = decodeJSONBody(rec, resp)
	return rec.Code, resp
}

func decodeJSONBody(rec *httptest.ResponseRecorder, v any) error {
	return json.Unmarshal([]byte(rec.Body.String()), v)
}

// Resolution matrix for the declared semantics (review 2026-09-14: the old
// bound-inference left "unbound + wait" semantics undefined).
func TestCIRemotePipelineRequiredResolution(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		env      string
		want     bool
	}{
		{"declared required wins over env off", "required", "", true},
		{"declared local wins over env on", "local", "true", false},
		{"env on fills empty declaration", "", "required", true},
		{"env true fills empty declaration", "", "1", true},
		{"empty declaration with no env is local", "", "", false},
		{"unknown declaration falls through to env", "bogus", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(deliverymode.RemotePipelineRequiredEnv, tc.env)
			p := &entity.Project{RemotePipelineRequired: tc.declared}
			if got := deliverymode.RemotePipelineRequired(p); got != tc.want {
				t.Fatalf("RemotePipelineRequired(declared=%q env=%q) = %v, want %v", tc.declared, tc.env, got, tc.want)
			}
		})
	}
}

// Required + unbound: the deterministic checks may be all green, but the
// missing remote is a FAILING pipeline_evidence — an explicit declaration
// error, not a bound-inference skip.
func TestCIReadyRemoteRequiredUnboundFailsClosed(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	p := seedCIReadyProject(t, s, "strict", false)
	p.RemotePipelineRequired = "required"
	if err := s.st.SaveProject("strict", p); err != nil {
		t.Fatal(err)
	}

	code, resp := runCIReady(s, "strict", "1")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, recBody(t, s, "strict"))
	}
	if resp.Overall != ciready.OverallNotReady {
		t.Fatalf("overall=%s, want not_ready", resp.Overall)
	}
	var evidence *ciready.Check
	for i := range resp.Checks {
		if resp.Checks[i].Name == "pipeline_evidence" {
			evidence = &resp.Checks[i]
		}
	}
	if evidence == nil {
		t.Fatalf("pipeline_evidence check missing under required semantics")
	}
	if evidence.Status != ciready.StatusFail {
		t.Fatalf("pipeline_evidence=%s, want fail", evidence.Status)
	}
	if !strings.Contains(evidence.Detail, "remote pipeline required") {
		t.Fatalf("detail should state the required-remote error, got %q", evidence.Detail)
	}
}

// Local-only + unbound: no pipeline_evidence check at all — the deterministic
// verdict stands on its own.
func TestCIReadyLocalOnlyUnboundHasNoEvidenceCheck(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	seedCIReadyProject(t, s, "lenient", false)

	_, resp := runCIReady(s, "lenient", "1")
	for _, c := range resp.Checks {
		if c.Name == "pipeline_evidence" {
			t.Fatalf("local-only semantics must not append pipeline_evidence, got %+v", c)
		}
	}
	if resp.Overall != ciready.OverallReady {
		t.Fatalf("overall=%s, want ready for local-only project", resp.Overall)
	}
}

// Unbound + no declaration: inherits the server default (local-only), matching
// the historical behavior for projects that never opted in.
func TestCIReadyUndeclaredUnboundInheritsServerDefault(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	seedCIReadyProject(t, s, "inherit", false)

	_, resp := runCIReady(s, "inherit", "1")
	for _, c := range resp.Checks {
		if c.Name == "pipeline_evidence" {
			t.Fatalf("undeclared + env-off must not fail the gate, got %+v", c)
		}
	}
}

func recBody(t *testing.T, s *Server, project string) string {
	t.Helper()
	req := runtimeRequest(http.MethodPost, "/api/v1/runtime/ci-ready", project)
	req.SetPathValue("name", project)
	rec := httptest.NewRecorder()
	s.handleRuntimeCIReady(rec, req)
	return rec.Body.String()
}
