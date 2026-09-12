package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func newRuntimeProfileServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("legacy", &entity.Project{Name: "legacy"}); err != nil {
		t.Fatalf("seed legacy project: %v", err)
	}
	if err := s.st.SaveProject("jvmproj", &entity.Project{Name: "jvmproj", RuntimeProfile: "jvm21"}); err != nil {
		t.Fatalf("seed jvm project: %v", err)
	}
	return s, workspaceID
}

func projectProfileFromResponse(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode project response: %v body=%s", err, body)
	}
	raw, ok := payload["runtimeProfile"]
	if !ok || raw == nil {
		return "", false
	}
	val, ok := raw.(string)
	if !ok {
		t.Fatalf("runtimeProfile is not a string: %T", raw)
	}
	return val, true
}

func TestProjectDetailSerializesRuntimeProfile(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)

	for _, tc := range []struct {
		project string
		want    string
	}{
		{project: "legacy", want: ""},
		{project: "jvmproj", want: "jvm21"},
	} {
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodGet, "/api/v1/projects/"+tc.project, "admin", nil)
		req.SetPathValue("name", tc.project)
		s.handleProject(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s status=%d body=%s", tc.project, rec.Code, rec.Body.String())
		}
		got, _ := projectProfileFromResponse(t, rec.Body.Bytes())
		if got != tc.want {
			t.Fatalf("project %s runtimeProfile got %q, want %q", tc.project, got, tc.want)
		}
	}
}

func TestProjectListSerializesRuntimeProfile(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects", "admin", nil)
	s.handleProjects(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list projects status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	seen := map[string]string{}
	for _, row := range rows {
		name, _ := row["name"].(string)
		profile, _ := row["runtimeProfile"].(string)
		seen[name] = profile
	}
	if got := seen["jvmproj"]; got != "jvm21" {
		t.Fatalf("list runtimeProfile for jvmproj = %q, want jvm21", got)
	}
	if got, ok := seen["legacy"]; ok && got != "" {
		t.Fatalf("legacy project must not invent a profile, got %q", got)
	}
}

func TestPutProjectSetsRuntimeProfile(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPut, "/api/v1/projects/legacy", "admin", map[string]any{
		"description":    "d",
		"repo":           "repo",
		"runtimeProfile": "jvm21",
	})
	req.SetPathValue("name", "legacy")
	s.handlePutProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put legacy status=%d body=%s", rec.Code, rec.Body.String())
	}
	p, err := s.st.Project("legacy")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RuntimeProfile != "jvm21" {
		t.Fatalf("runtimeProfile persisted as %q, want jvm21", p.RuntimeProfile)
	}

	// The declared value must round-trip through GET.
	getRec := httptest.NewRecorder()
	getReq := providerTestRequest(http.MethodGet, "/api/v1/projects/legacy", "admin", nil)
	getReq.SetPathValue("name", "legacy")
	s.handleProject(getRec, getReq)
	got, _ := projectProfileFromResponse(t, getRec.Body.Bytes())
	if got != "jvm21" {
		t.Fatalf("GET after PUT returned runtimeProfile %q, want jvm21", got)
	}
}

func TestPutProjectRejectsUnknownRuntimeProfile(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)

	for _, profile := range []string{"jvm", "node22", "openjdk-99"} {
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodPut, "/api/v1/projects/legacy", "admin", map[string]any{
			"runtimeProfile": profile,
		})
		req.SetPathValue("name", "legacy")
		s.handlePutProject(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("put profile %q status=%d body=%s", profile, rec.Code, rec.Body.String())
		}
	}
	p, err := s.st.Project("legacy")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RuntimeProfile != "" {
		t.Fatalf("failed PUT must not persist a profile, got %q", p.RuntimeProfile)
	}
}

func TestPutProjectProfileOmittedKeepsDeclaredValue(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)

	// A settings page that only edits description/repo must not silently
	// clear a profile a template or admin declared earlier.
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPut, "/api/v1/projects/jvmproj", "admin", map[string]any{
		"description": "edited",
	})
	req.SetPathValue("name", "jvmproj")
	s.handlePutProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put jvmproj status=%d body=%s", rec.Code, rec.Body.String())
	}
	p, err := s.st.Project("jvmproj")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RuntimeProfile != "jvm21" {
		t.Fatalf("omitted runtimeProfile cleared a declared value, got %q", p.RuntimeProfile)
	}
}

func TestPutProjectRuntimeProfileRequiresManager(t *testing.T) {
	s, workspaceID := newRuntimeProfileServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	grantProjectRoleForTest(t, s, workspaceID, "viewer", ProjectRoleViewer)
	grantProjectRoleForTest(t, s, workspaceID, "operator", ProjectRoleOperator)

	for _, username := range []string{"viewer", "operator"} {
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodPut, "/api/v1/projects/legacy", username, map[string]any{
			"runtimeProfile": "jvm21",
		})
		req.SetPathValue("name", "legacy")
		s.handlePutProject(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s set runtimeProfile status=%d body=%s", username, rec.Code, rec.Body.String())
		}
	}
	p, err := s.st.Project("legacy")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RuntimeProfile != "" {
		t.Fatalf("non-manager PUT must not persist a profile, got %q", p.RuntimeProfile)
	}
}

func TestPutProjectRuntimeProfileClearsViaBaseAlias(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPut, "/api/v1/projects/jvmproj", "admin", map[string]any{
		"runtimeProfile": "base",
	})
	req.SetPathValue("name", "jvmproj")
	s.handlePutProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put base status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Explicit "base" normalizes to "" on disk (legacy-undistinguishable is
	// acceptable: base is the resolved default either way) and GET must
	// return "base" semantics as empty.
	p, err := s.st.Project("jvmproj")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if p.RuntimeProfile != "" {
		t.Fatalf("explicit base must clear the declared profile, got %q", p.RuntimeProfile)
	}
	getRec := httptest.NewRecorder()
	getReq := providerTestRequest(http.MethodGet, "/api/v1/projects/jvmproj", "admin", nil)
	getReq.SetPathValue("name", "jvmproj")
	s.handleProject(getRec, getReq)
	got, _ := projectProfileFromResponse(t, getRec.Body.Bytes())
	if got != "" {
		t.Fatalf("GET after explicit base returned %q, want empty", got)
	}
}
