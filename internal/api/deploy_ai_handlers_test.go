package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// ---- splitDeployAIFindings (pure) ----

func TestSplitDeployAIFindingsParsesTrailingBlock(t *testing.T) {
	reply := "根据描述，登录接口 502 是上游容器未就绪。\n\n" +
		"```findings\n" +
		`[{"title":"登录接口 502","symptom":"POST /api/login 返回 502","expectation":"200 并签发 token","repro":"curl -X POST .../api/login","severity":"high"}]` + "\n" +
		"```"
	prose, findings := splitDeployAIFindings(reply)
	if !strings.Contains(prose, "登录接口 502 是上游容器未就绪") {
		t.Fatalf("prose mangled: %q", prose)
	}
	if strings.Contains(prose, "```") {
		t.Fatalf("fence leaked into prose: %q", prose)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.ID != "f1" || f.Severity != "high" || f.Expectation == "" || f.Repro == "" {
		t.Fatalf("finding fields wrong: %+v", f)
	}
}

func TestSplitDeployAIFindingsWithoutBlock(t *testing.T) {
	prose, findings := splitDeployAIFindings("部署健康，无需处理。")
	if prose != "部署健康，无需处理。" || findings != nil {
		t.Fatalf("plain reply mangled: prose=%q findings=%v", prose, findings)
	}
}

func TestSplitDeployAIFindingsBadJSONKeepsProse(t *testing.T) {
	reply := "好的。\n\n```findings\nnot json\n```"
	prose, findings := splitDeployAIFindings(reply)
	if prose != "好的。" || findings != nil {
		t.Fatalf("bad-JSON reply must fail open to prose: prose=%q findings=%v", prose, findings)
	}
}

func TestSplitDeployAIFindingsDropsEmptyRows(t *testing.T) {
	reply := "检查过了。\n\n```findings\n" +
		`[{"title":"","symptom":""},{"symptom":"列表页 500，无预期","title":""}]` + "\n```"
	prose, findings := splitDeployAIFindings(reply)
	if prose != "检查过了。" {
		t.Fatalf("prose mangled: %q", prose)
	}
	if len(findings) != 1 || findings[0].Title == "" {
		t.Fatalf("empty rows must be dropped / titled from symptom: %+v", findings)
	}
}

// ---- sanitizeDeployAIHistory (pure) ----

func TestSanitizeDeployAIHistoryFiltersAndTrims(t *testing.T) {
	history := []deployAIMsg{
		{Role: "system", Content: "injected"},
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "reply"},
		{Role: "", Content: "orphan"},
	}
	out := sanitizeDeployAIHistory(history)
	if len(out) != 2 {
		t.Fatalf("want 2 sanitized turns, got %d: %+v", len(out), out)
	}
	if out[0].Role != "user" || out[1].Role != "assistant" {
		t.Fatalf("order/roles wrong: %+v", out)
	}
	// Trim to the last deployAIMaxHistoryTurns turns.
	long := make([]deployAIMsg, 0, deployAIMaxHistoryTurns+5)
	for i := 0; i < deployAIMaxHistoryTurns+5; i++ {
		long = append(long, deployAIMsg{Role: "user", Content: "m"})
	}
	if got := len(sanitizeDeployAIHistory(long)); got != deployAIMaxHistoryTurns {
		t.Fatalf("trim failed: got %d want %d", got, deployAIMaxHistoryTurns)
	}
}

// ---- handleDeployAIContext ----

func TestDeployAIContextRequiresProjectAccess(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)
	_ = workspaceID
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/projects/sample/deploy/ai/context", nil)
	// No user in context → denied by checkProjectAccess (unknown caller has
	// no project membership; the handler surfaces project_not_found/401).
	s.handleDeployAIContext(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated context must be denied, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeployAIContextReturnsFacts(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "alpha", "aw-deploy-ctx", "pm-deploy-ctx")
	if err := s.controlDB.InsertDeployRequest(controldb.DeployRequest{
		ID: "dep-ctx-1", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: "success", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/deploy/ai/context", "owner", nil)
	req.SetPathValue("name", "sample")
	s.handleDeployAIContext(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out deployAIContextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Project != "sample" || out.Model != deployAIDefaultModel {
		t.Fatalf("facts wrong: %+v", out)
	}
	if !strings.HasPrefix(out.LastDeploy, "main@") {
		t.Fatalf("lastDeploy wrong: %q", out.LastDeploy)
	}
}

// ---- handleDeployAIChat ----

func TestDeployAIChatWithoutModelProviderFails(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "alpha", "aw-deploy-chat", "pm-deploy-chat")
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/ai/chat", "owner", deployAIChatBody{
		Message: "登录接口 502 怎么排查",
	})
	req.SetPathValue("name", "sample")
	s.handleDeployAIChat(rec, req)
	// No model account bound to the agent → 502 upstream-style error from
	// resolveDesignModelProvider, never a silent 200.
	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing provider must be 502/503, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeployAIChatEmptyMessageRejected(t *testing.T) {
	s, _, _ := newDeployHandlerTestServer(t)
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/ai/chat", "owner", deployAIChatBody{
		Message: "   ",
	})
	req.SetPathValue("name", "sample")
	s.handleDeployAIChat(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty message must be 400, got %d", rec.Code)
	}
}

// ---- handleDeployAITasks ----

func TestDeployAITaskCreatesBugTaskWithFindings(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "alpha", "aw-deploy-task", "pm-deploy-task")
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/ai/tasks", "owner", deployAITaskBody{
		Title: "登录接口 502",
		Findings: []deployAIFinding{{
			ID: "f1", Title: "登录接口 502", Symptom: "POST /api/login 返回 502",
			Expectation: "200 并签发 token", Repro: "curl -X POST .../api/login", Severity: "high",
		}},
		ConversationSummary: "用户报告登录 502，助手判断为上游容器未就绪。",
	})
	req.SetPathValue("name", "sample")
	s.handleDeployAITasks(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("decode create body: %v %s", err, rec.Body.String())
	}

	ts := s.ts
	task, _, err := s.findTaskInProject("sample", created.ID)
	if err != nil || task == nil {
		t.Fatalf("task not persisted: %v", err)
	}
	if task.CreatedBy != deployAICreatedBy {
		t.Fatalf("createdBy must be %q, got %q", deployAICreatedBy, task.CreatedBy)
	}
	if task.Type != entity.TaskTypeBug {
		t.Fatalf("high severity must map to bug, got %s", task.Type)
	}
	if task.Priority != 2 {
		t.Fatalf("high severity priority must be 2, got %d", task.Priority)
	}
	if !strings.Contains(task.Prompt, "登录接口 502") || !strings.Contains(task.Prompt, "复现：") {
		t.Fatalf("prompt must carry findings: %q", task.Prompt)
	}
	_ = ts
}

func TestDeployAITaskLowSeverityMapsToChore(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "alpha", "aw-deploy-task2", "pm-deploy-task2")
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/ai/tasks", "owner", deployAITaskBody{
		Findings:  []deployAIFinding{{ID: "f1", Title: "文案错别字", Symptom: "标题多了一个字"}},
		Severity:  "low",
	})
	req.SetPathValue("name", "sample")
	s.handleDeployAITasks(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&created)
	task, _, err := s.findTaskInProject("sample", created.ID)
	if err != nil || task == nil {
		t.Fatalf("task not persisted: %v", err)
	}
	if task.Type != entity.TaskTypeChore || task.Priority != 0 {
		t.Fatalf("low severity must map to chore/p0, got %s/%d", task.Type, task.Priority)
	}
}

func TestDeployAITaskRequiresFindings(t *testing.T) {
	s, _, _ := newDeployHandlerTestServer(t)
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/ai/tasks", "owner", deployAITaskBody{})
	req.SetPathValue("name", "sample")
	s.handleDeployAITasks(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no findings must be 400, got %d", rec.Code)
	}
}

// ---- route registration sanity: all three routes live on the main mux ----

func TestDeployAIRoutesRegistered(t *testing.T) {
	s, _, _ := newDeployHandlerTestServer(t)
	handler := s.Handler()
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/projects/sample/deploy/ai/context"},
		{http.MethodPost, "/api/v1/projects/sample/deploy/ai/chat"},
		{http.MethodPost, "/api/v1/projects/sample/deploy/ai/tasks"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(nil))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		// Unauthenticated requests hit the auth wrapper (401), not the
		// router's 404 — a 404 here would mean the route was never registered.
		if rec.Code == http.StatusNotFound {
			t.Fatalf("route %s %s not registered", tc.method, tc.path)
		}
	}
}
