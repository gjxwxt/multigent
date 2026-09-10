package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/secretbox"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// fakeODClient records calls so idempotency and guards can be asserted
// without a live OD daemon.
type fakeODClient struct {
	projects     map[string]bool
	createCalls  []string
	runCalls     []string
	deleteCalls  []string
	lastCreds    *designModelCreds
	lastPrompt   string
	lastMessage  string
	runStatus    string
	failCreate   bool
	files        []ODProjectFile
	fileContents map[string][]byte
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

func (f *fakeODClient) CreateProject(_ context.Context, id, _, _, pendingPrompt string) error {
	if f.failCreate {
		return &odAPIError{Status: 502, Detail: "upstream down"}
	}
	f.createCalls = append(f.createCalls, id)
	f.projects[id] = true
	f.lastPrompt = pendingPrompt
	return nil
}

func (f *fakeODClient) StartRun(_ context.Context, projectID, message, _, _ string, creds *designModelCreds) (string, error) {
	f.runCalls = append(f.runCalls, projectID)
	f.lastCreds = creds
	f.lastMessage = message
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

func (f *fakeODClient) ListProjectFiles(_ context.Context, _ string) ([]ODProjectFile, error) {
	return f.files, nil
}

func (f *fakeODClient) GetProjectFile(_ context.Context, _, path string) ([]byte, error) {
	if raw, ok := f.fileContents[path]; ok {
		return raw, nil
	}
	return nil, &odAPIError{Status: 404, Detail: "not found"}
}

func seedDesignTask(t *testing.T, status entity.TaskStatus) (*Server, string, *entity.Task) {
	t.Helper()
	s, workspaceID, task, _ := seedResourceProject(t, status)
	seedDesignModelProvider(t, s, workspaceID)
	return s, workspaceID, task
}

// seedDesignModelProvider wires the seeded task agent ("agent") to an
// anthropic-protocol model account so resolveDesignModelProvider resolves it.
func seedDesignModelProvider(t *testing.T, s *Server, workspaceID string) {
	t.Helper()
	key, err := secretbox.SealString("sk-gateway-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.controlDB.UpsertModelProvider(workspaceID, controldb.ModelProvider{
		ID:         "prov-ccr-test",
		Name:       "ccr-qwen",
		Type:       "anthropic",
		BaseURL:    "http://127.0.0.1:3456",
		APIKey:     key,
		ModelsJSON: `["qwen3.8-27b"]`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:                    "aw-res-agent",
		WorkspaceID:           workspaceID,
		Name:                  "agent",
		DisplayName:           "agent",
		Model:                 "claudecode",
		RuntimeModel:          "qwen3.8-27b",
		DefaultModelAccountID: "prov-ccr-test",
		Status:                "available",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
		ID:          "pm-res-agent",
		WorkspaceID: workspaceID,
		ProjectID:   "resproj",
		MemberType:  "agent_worker",
		MemberID:    "aw-res-agent",
		Title:       "agent",
	}); err != nil {
		t.Fatal(err)
	}
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
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
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
	// byokProvider must carry the agent's model-gateway creds (ccr-qwen),
	// never the OD connection's own URL/token.
	if fake.lastCreds == nil {
		t.Fatal("StartRun received no model creds")
	}
	if fake.lastCreds.Protocol != "anthropic" || fake.lastCreds.BaseURL != "http://127.0.0.1:3456" {
		t.Fatalf("creds = %+v", fake.lastCreds)
	}
	if fake.lastCreds.APIKey != "sk-gateway-test" {
		t.Fatal("creds api key mismatch")
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
	s, _, _ := seedDesignTask(t, entity.TaskStatusDoneSuccess)
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

// A task parked at a human_review workflow step reports Status in_progress
// (pitfall 7); the design start must admit it via the workflow-state check.
func TestDesignStartAllowsTaskAtHumanReviewStep(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusInProgress)
	fake := newFakeODClient()
	s.designClient = fake
	seedODConnection(t, s)

	// Start a greenfield-shaped workflow run stopped at a human_review step.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "design gate test")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	// Park the run on the design_review (human_review) step with an open instance.
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatalf("run not found: %v", err)
	}
	stepID := "design_review"
	run.ActiveStepID = stepID
	run.Status = "active"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	parked := false
	for i := range instances {
		if instances[i].StepID == stepID {
			instances[i].Status = "open"
			instances[i].StartedAt = now
			instances[i].InputValues = map[string]string{
				"approved_requirement": "澄清后的需求：用户认证中心界面规格",
			}
			if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
				t.Fatal(err)
			}
			parked = true
		}
	}
	if !parked {
		t.Fatal("design_review instance not created by StartRun")
	}

	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"designSystemId":"ant"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected start allowed at human_review step, got %d %s", w.Code, w.Body.String())
	}
	if len(fake.createCalls) != 1 {
		t.Fatalf("createCalls = %v", fake.createCalls)
	}
	if !strings.Contains(fake.lastPrompt, "澄清后的需求：用户认证中心界面规格") {
		t.Fatalf("lastPrompt missing approved requirement: %s", fake.lastPrompt)
	}
	if !strings.Contains(fake.lastPrompt, "【角色与使命】") || !strings.Contains(fake.lastPrompt, "严禁修改工程代码") {
		t.Fatalf("lastPrompt missing role framing: %s", fake.lastPrompt)
	}
	if fake.lastMessage != fake.lastPrompt {
		t.Fatalf("StartRun message should match CreateProject prompt")
	}
}

func TestDesignStartWorkflowFallbackToRequirementDraft(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusInProgress)
	fake := newFakeODClient()
	s.designClient = fake
	seedODConnection(t, s)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "design gate test")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatalf("run not found: %v", err)
	}
	stepID := "design_review"
	run.ActiveStepID = stepID
	run.Status = "active"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range instances {
		if instances[i].StepID == stepID {
			instances[i].Status = "open"
			instances[i].StartedAt = now
			instances[i].InputValues = map[string]string{
				"requirement_draft": "草案需求：设计原型规格v1",
			}
			if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
				t.Fatal(err)
			}
		}
	}

	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"designSystemId":"ant"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected start allowed with requirement_draft fallback, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(fake.lastPrompt, "草案需求：设计原型规格v1") {
		t.Fatalf("lastPrompt missing requirement_draft: %s", fake.lastPrompt)
	}
}

func TestDesignStartWorkflowFailsClosedWhenRequirementEmpty(t *testing.T) {
	s, workspaceID, task := seedDesignTask(t, entity.TaskStatusInProgress)
	fake := newFakeODClient()
	s.designClient = fake
	seedODConnection(t, s)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "design gate test")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	run, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatalf("run not found: %v", err)
	}
	stepID := "design_review"
	run.ActiveStepID = stepID
	run.Status = "active"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range instances {
		if instances[i].StepID == stepID {
			instances[i].Status = "open"
			instances[i].StartedAt = now
			instances[i].InputValues = map[string]string{}
			if err := wfStore.SaveStepInstance(&instances[i]); err != nil {
				t.Fatal(err)
			}
		}
	}

	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"designSystemId":"ant"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 fail-closed when requirements missing, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workflow design gate requires approved_requirement or requirement_draft") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
	if len(fake.createCalls) != 0 || len(fake.runCalls) != 0 {
		t.Fatalf("OD must not be called when requirement is missing: %v %v", fake.createCalls, fake.runCalls)
	}
}

func TestDesignStartStandaloneTaskUsesPrompt(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	fake := newFakeODClient()
	s.designClient = fake
	task.Prompt = "独立任务：设计一个看板页面"
	if err := s.ts.UpdateTask("resproj", "agent", task); err != nil {
		t.Fatal(err)
	}

	w := designPost(t, s, "/api/v1/projects/resproj/tasks/t-res-1/design/start", `{"designSystemId":"ant"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected start allowed for standalone task, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(fake.lastPrompt, "独立任务：设计一个看板页面") {
		t.Fatalf("lastPrompt missing task prompt: %s", fake.lastPrompt)
	}
	if !strings.Contains(fake.lastPrompt, "【角色与使命】") {
		t.Fatalf("lastPrompt missing role framing: %s", fake.lastPrompt)
	}
}

func TestDesignStartRegeneratesWithDelete(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
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

// The root-shape studio proxy forwards OD's HTML byte-for-byte: OD sees
// native paths on both sides, so no base/patcher surgery exists to test —
// the HTML pass only mints the scoped session cookie.

func TestDesignLaunchRedirects(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
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

// The iframe bootstraps with only the odt signature token (no Bearer header,
// no _token); the root-shape proxy must admit it via handler-side validation
// (pitfall 6 pattern: these routes live on publicMux) and reject odt-less
// requests.
func TestDesignProxyAdmitsSignatureTokenWithoutBearer(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	s.designClient = newFakeODClient()
	task.DesignProjectID = "proj_mg_t-res-1"
	if err := s.ts.UpdateTask("resproj", taskAgentFromAssignee(task), task); err != nil {
		t.Fatal(err)
	}
	odt := s.signDesignToken(task.ID, "resproj")

	// No test OD daemon exists; reaching OD's Basic Auth challenge proves the
	// request passed platform auth and was forwarded upstream. An odt-less
	// request must be rejected by the platform itself and never reach OD.
	req := httptest.NewRequest(http.MethodGet, "/projects/proj_mg_"+task.ID+"?odt="+odt, nil)
	w := httptest.NewRecorder()
	if !s.handleDesignRootProject(w, req) {
		t.Fatal("design project path should be claimed by the design proxy")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("proxy with odt: %d (expected upstream 401)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OpenDesign authentication required") {
		t.Fatalf("expected OD upstream challenge, got: %s", w.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/projects/proj_mg_"+task.ID, nil)
	w2 := httptest.NewRecorder()
	s.handleDesignRootProject(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("proxy without odt: %d (expected 401)", w2.Code)
	}
	if strings.Contains(w2.Body.String(), "OpenDesign authentication required") {
		t.Fatal("odt-less request must not reach the OD upstream")
	}
}

// Console project paths must never be shadowed by the root-shape design
// proxy: /projects/<console-name>/… falls through to the console SPA.
func TestDesignRootProjectFallsThroughForConsolePaths(t *testing.T) {
	s, _, task := seedDesignTask(t, entity.TaskStatusAwaitingConfirmation)
	seedODConnection(t, s)
	task.DesignProjectID = "proj_mg_" + task.ID
	if err := s.ts.UpdateTask("resproj", taskAgentFromAssignee(task), task); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/resproj/tasks", nil)
	w := httptest.NewRecorder()
	if s.handleDesignRootProject(w, req) {
		t.Fatal("console project path must fall through to the console SPA")
	}
}
