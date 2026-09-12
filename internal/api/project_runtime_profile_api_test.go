package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/sandbox"
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

// Empty-string PUT is a deliberate clear: the declaration goes back to
// auto/inherit semantics. It must persist as "" (not folded to "base" by
// NormalizeProfile) and GET must return empty, so a subsequent ResolveRuntime
// inherits the agent preference or server default again.
func TestPutProjectRuntimeProfileEmptyStringClearsDeclaration(t *testing.T) {
	s, workspaceID := newRuntimeProfileServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "legacy", "picky", "aw-picky-clear", "pm-legacy-clear")
	picky, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-picky-clear")
	if err != nil || !ok {
		t.Fatalf("load worker: ok=%v err=%v", ok, err)
	}
	picky.RuntimeConfigJSON = `{"sandbox":{"provider":"docker","docker":{"profile":"jvm21"}}}`
	if err := s.controlDB.UpsertAgentWorker(picky); err != nil {
		t.Fatalf("update worker: %v", err)
	}

	clear := func(project string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodPut, "/api/v1/projects/"+project, "admin", map[string]any{
			"runtimeProfile": "",
		})
		req.SetPathValue("name", project)
		s.handlePutProject(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("clear %s status=%d body=%s", project, rec.Code, rec.Body.String())
		}
	}
	stored := func(project string) string {
		t.Helper()
		p, err := s.st.Project(project)
		if err != nil {
			t.Fatalf("reload %s: %v", project, err)
		}
		return p.RuntimeProfile
	}
	getProfile := func(project string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodGet, "/api/v1/projects/"+project, "admin", nil)
		req.SetPathValue("name", project)
		s.handleProject(rec, req)
		got, _ := projectProfileFromResponse(t, rec.Body.Bytes())
		return got
	}

	// jvm21 declaration → "" clears it; base declaration → "" clears it too.
	// "legacy" is seeded undeclared by newRuntimeProfileServer, so it must
	// first explicitly declare base for the base→"" case to exercise a real
	// clear rather than a no-op on an empty value.
	clear("jvmproj")
	if stored("jvmproj") != "" {
		t.Fatalf("clearing a jvm21 declaration must persist empty, got %q", stored("jvmproj"))
	}
	if got := getProfile("jvmproj"); got != "" {
		t.Fatalf("GET after clearing jvm21 must return empty, got %q", got)
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPut, "/api/v1/projects/legacy", "admin", map[string]any{
		"runtimeProfile": "base",
	})
	req.SetPathValue("name", "legacy")
	s.handlePutProject(rec, req)
	if rec.Code != http.StatusOK || stored("legacy") != "base" {
		t.Fatalf("legacy must declare base first, status=%d stored=%q", rec.Code, stored("legacy"))
	}
	clear("legacy")
	if stored("legacy") != "" {
		t.Fatalf("clearing a base declaration must persist empty, got %q", stored("legacy"))
	}
	if got := getProfile("legacy"); got != "" {
		t.Fatalf("GET after clearing base must return empty, got %q", got)
	}

	// After the clear, ResolveRuntime must fall back to inheritance: the agent
	// worker's jvm21 preference applies again (auto semantics restored).
	p, err := s.st.Project("jvmproj")
	if err != nil {
		t.Fatalf("reload jvmproj: %v", err)
	}
	sel, err := sandbox.ResolveRuntime(sandbox.RuntimeRequest{ProjectProfile: p.RuntimeProfile, AgentProfile: "jvm21"})
	if err != nil {
		t.Fatalf("resolve after clear: %v", err)
	}
	if sel.Profile != sandbox.ProfileJVM21 {
		t.Fatalf("cleared project must inherit the agent jvm21 preference, got profile=%q", sel.Profile)
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

// Three-state semantics: "" = auto/undeclared (inherits agent preference or
// server default), "base" = explicit project-level base (must WIN over an
// agent jvm21 preference and the server default), "jvm21" = explicit
// project-level jvm21. Auto and explicit base are distinct requests with
// distinct persisted values.
func TestPutProjectRuntimeProfileThreeStates(t *testing.T) {
	s, _ := newRuntimeProfileServer(t)
	put := func(project, profile string) *httptest.ResponseRecorder {
		body := map[string]any{"description": "d"}
		if profile != "<omit>" {
			body["runtimeProfile"] = profile
		}
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodPut, "/api/v1/projects/"+project, "admin", body)
		req.SetPathValue("name", project)
		s.handlePutProject(rec, req)
		return rec
	}
	stored := func(project string) string {
		p, err := s.st.Project(project)
		if err != nil {
			t.Fatalf("reload %s: %v", project, err)
		}
		return p.RuntimeProfile
	}

	// auto (field omitted) never overwrites a declared value; explicit values
	// persist verbatim — base is NOT folded to "".
	if rec := put("jvmproj", "<omit>"); rec.Code != http.StatusOK || stored("jvmproj") != "jvm21" {
		t.Fatalf("omit must keep jvm21, status=%d stored=%q", rec.Code, stored("jvmproj"))
	}
	if rec := put("jvmproj", "base"); rec.Code != http.StatusOK || stored("jvmproj") != "base" {
		t.Fatalf("explicit base must persist as base, status=%d stored=%q", rec.Code, stored("jvmproj"))
	}
	if rec := put("jvmproj", "jvm21"); rec.Code != http.StatusOK || stored("jvmproj") != "jvm21" {
		t.Fatalf("explicit jvm21 must persist, status=%d stored=%q", rec.Code, stored("jvmproj"))
	}
	if rec := put("legacy", "base"); rec.Code != http.StatusOK || stored("legacy") != "base" {
		t.Fatalf("undeclared project can declare explicit base, status=%d stored=%q", rec.Code, stored("legacy"))
	}

	// GET round-trips all three states.
	getProfile := func(project string) string {
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodGet, "/api/v1/projects/"+project, "admin", nil)
		req.SetPathValue("name", project)
		s.handleProject(rec, req)
		got, _ := projectProfileFromResponse(t, rec.Body.Bytes())
		return got
	}
	if got := getProfile("jvmproj"); got != "jvm21" {
		t.Fatalf("GET after explicit jvm21 = %q", got)
	}
	if got := getProfile("legacy"); got != "base" {
		t.Fatalf("GET after explicit base = %q, want base (not folded)", got)
	}
}

// An agent worker with a jvm21 preference joined to a project that explicitly
// declared base: the project declaration wins and the effective runtime is
// base. A project that never declared (auto) inherits the agent preference.
func TestRuntimeProfileExplicitBaseOverridesAgentPreference(t *testing.T) {
	s, workspaceID := newRuntimeProfileServer(t)
	if err := s.st.SaveProject("explicitbase", &entity.Project{Name: "explicitbase", RuntimeProfile: "base"}); err != nil {
		t.Fatalf("seed explicitbase: %v", err)
	}
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "explicitbase", "picky", "aw-picky2", "pm-explicitbase-picky")
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "legacy", "picky2", "aw-picky3", "pm-legacy-picky2")
	for id := range map[string]string{
		"aw-picky2": "jvm21",
		"aw-picky3": "jvm21",
	} {
		w, ok, err := s.controlDB.AgentWorkerByID(workspaceID, id)
		if err != nil || !ok {
			t.Fatalf("load %s: ok=%v err=%v", id, ok, err)
		}
		w.RuntimeConfigJSON = `{"sandbox":{"provider":"docker","docker":{"profile":"jvm21"}}}`
		if err := s.controlDB.UpsertAgentWorker(w); err != nil {
			t.Fatalf("update %s: %v", id, err)
		}
	}

	for _, project := range []string{"explicitbase", "legacy"} {
		p, err := s.st.Project(project)
		if err != nil {
			t.Fatalf("load %s: %v", project, err)
		}
		// Resolve through the same authority chain the runner and previews use.
		sel, err := sandbox.ResolveRuntime(sandbox.RuntimeRequest{ProjectProfile: p.RuntimeProfile, AgentProfile: "jvm21"})
		if err != nil {
			t.Fatalf("resolve %s: %v", project, err)
		}
		if project == "explicitbase" && sel.Profile != "base" {
			t.Fatalf("explicit base project resolved profile=%q, want base (agent jvm21 preference must lose)", sel.Profile)
		}
		if project == "legacy" && sel.Profile != "jvm21" {
			t.Fatalf("auto project must inherit agent jvm21 preference, got %q", sel.Profile)
		}
	}
}
