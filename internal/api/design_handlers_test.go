package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// fakeODClient records calls so idempotency and guards can be asserted
// without a live OD daemon.
type fakeODClient struct {
	projects    map[string]bool
	createCalls []string
	runCalls    []string
	deleteCalls []string
	runStatus   string
	failCreate  bool
}

func newFakeODClient() *fakeODClient {
	return &fakeODClient{projects: map[string]bool{}, runStatus: "succeeded"}
}

func (f *fakeODClient) ListProjects(_ context.Context) ([]ODProject, error) {
	out := make([]ODProject, 0, len(f.projects))
	for id := range f.projects {
		out = append(out, ODProject{ID: id, Name: id})
	}
	return out, nil
}

func (f *fakeODClient) CreateProject(_ context.Context, id, _, _, _ string) error {
	if f.failCreate {
		return &odAPIError{Status: 502, Detail: "upstream down"}
	}
	f.createCalls = append(f.createCalls, id)
	f.projects[id] = true
	return nil
}

func (f *fakeODClient) StartRun(_ context.Context, projectID, _, _, _ string) (string, error) {
	f.runCalls = append(f.runCalls, projectID)
	return "conv-" + projectID, nil
}

func (f *fakeODClient) LatestRunStatus(_ context.Context, projectID string) (string, string, error) {
	return "run-" + projectID, f.runStatus, nil
}

func (f *fakeODClient) DeleteProject(_ context.Context, projectID string) error {
	f.deleteCalls = append(f.deleteCalls, projectID)
	delete(f.projects, projectID)
	return nil
}

func seedDesignTask(t *testing.T, status entity.TaskStatus) (*Server, *entity.Task) {
	t.Helper()
	s, _, task, _ := seedResourceProject(t, status)
	return s, task
}

// seedODConnection registers an opendesign connection (baseUrl in
// ProfileJSON, apiKey in the encrypted secret) so design handlers can resolve
// the upstream without env coordination.
func seedODConnection(t *testing.T, s *Server) {
	t.Helper()
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID: "conn-od", WorkspaceID: workspaceID, Provider: "opendesign",
		ConnectionName: "default", OwnerType: ConnectionOwnerUser, OwnerID: "admin",
		AuthType: ConnectionAuthAPIKey, Status: "active",
		ProfileJSON: `{"baseUrl":"http://127.0.0.1:7456"}`,
	}); err != nil {
		t.Fatal(err)
	}
	secret, err := sealConnectionSecret(map[string]string{"apiKey": "od-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	secret.ConnectionID = "conn-od"
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatal(err)
	}
}

func designPost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", "t-res-1")
	w := httptest.NewRecorder()
	s.handleDesignStart(w, req)
	return w
}

func TestDesignStartCreatesProjectAndIsIdempotent(t *testing.T) {
	s, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	fake := newFakeODClient()
	s.designClient = fake

	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"designSystemId":"ant"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if len(fake.createCalls) != 1 || fake.createCalls[0] != designProjectIDForTask(task.ID) {
		t.Fatalf("createCalls = %v", fake.createCalls)
	}
	if len(fake.runCalls) != 1 {
		t.Fatalf("runCalls = %v", fake.runCalls)
	}

	// Second start reuses the project without touching OD.
	w2 := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("second start: %d", w2.Code)
	}
	if len(fake.createCalls) != 1 || len(fake.runCalls) != 1 {
		t.Fatalf("idempotency broken: create=%d run=%d", len(fake.createCalls), len(fake.runCalls))
	}

	// Task now carries the design reference.
	_, _, fresh, err := s.ts.FindTaskByID(task.ID)
	if err != nil || fresh.DesignProjectID != designProjectIDForTask(task.ID) || fresh.DesignSource != "generated" {
		t.Fatalf("task fields not persisted: %+v err=%v", fresh, err)
	}
}

func TestDesignStartRejectsNonAwaitingTask(t *testing.T) {
	s, _ := seedDesignTask(t, entity.TaskStatusDoneSuccess)
	fake := newFakeODClient()
	s.designClient = fake

	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for non-awaiting task, got %d %s", w.Code, w.Body.String())
	}
	if len(fake.createCalls) != 0 || len(fake.runCalls) != 0 {
		t.Fatalf("OD must not be touched on guard rejection: %v %v", fake.createCalls, fake.runCalls)
	}
}

func TestDesignStartRegeneratesWithDelete(t *testing.T) {
	s, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	fake := newFakeODClient()
	s.designClient = fake

	if w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"designSystemId":"ant"}`); w.Code != http.StatusOK {
		t.Fatalf("first start: %d", w.Code)
	}
	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"regenerate":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("regenerate: %d %s", w.Code, w.Body.String())
	}
	if len(fake.deleteCalls) != 1 {
		t.Fatalf("old project not deleted: %v", fake.deleteCalls)
	}
	if len(fake.createCalls) != 2 {
		t.Fatalf("expected 2 creates, got %v", fake.createCalls)
	}
	_ = task
}

func TestDesignProxyWhitelist(t *testing.T) {
	for _, p := range []string{"/projects/x", "/api/projects/x/files", "/assets/a.js", "/_next/static/x"} {
		if !designPathAllowed(p) {
			t.Fatalf("path %q should be allowed", p)
		}
	}
	for _, p := range []string{"/api/auth/login", "/admin/panel", "/settings/general", "/api/connectors/x", "/unknown"} {
		if designPathAllowed(p) {
			t.Fatalf("path %q should be denied", p)
		}
	}
}

func TestRewriteDesignHTMLInjectsBaseAndInterceptor(t *testing.T) {
	in := `<html><head><title>x</title></head><body>hi</body></html>`
	out := rewriteDesignHTML(in, "resproj", "t-res-1", "tok123")
	if !strings.Contains(out, `<base href="/api/v1/projects/resproj/tasks/t-res-1/design/proxy/">`) {
		t.Fatalf("base tag missing: %s", out)
	}
	if !strings.Contains(out, "patchUrl") || !strings.Contains(out, "window.fetch") {
		t.Fatalf("interceptor missing")
	}
	// No rewrite when there is no head tag.
	out2 := rewriteDesignHTML("<html><body>plain</body></html>", "resproj", "t-res-1", "tok")
	if !strings.Contains(out2, "<script>") {
		t.Fatalf("fallback injection missing")
	}
}

func TestDesignLaunchRedirects(t *testing.T) {
	s, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	task.DesignProjectID = "proj_mg_t-res-1"
	if err := s.ts.UpdateTask("resproj", taskAgentFromAssignee(task), task); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/launch", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.SetPathValue("name", "resproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handleDesignLaunch(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("launch: %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/projects/proj_mg_t-res-1") {
		t.Fatalf("location = %q", loc)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control missing")
	}
}
