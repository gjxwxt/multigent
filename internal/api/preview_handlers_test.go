package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/preview"
)

func TestPreviewHandlers(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("testproj", &entity.Project{Name: "testproj"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// 1. Get preview for stopped task
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/testproj/tasks/t-123/preview", "admin", nil)
	req.SetPathValue("name", "testproj")
	req.SetPathValue("taskId", "t-123")
	w := httptest.NewRecorder()

	s.handleGetTaskPreview(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var res map[string]any
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if res["taskId"] != "t-123" || res["status"] != "stopped" {
		t.Fatalf("unexpected preview response: %v", res)
	}
	if res["drawerEnabled"] != false {
		t.Fatalf("expected drawerEnabled to be false by default, got %v", res["drawerEnabled"])
	}

	// 1b. Get preview with drawerEnabled active (requires matching origins)
	s.SetPreviewCopilotDrawerEnabled(true)
	// Without origins configured, must remain fail-closed false
	wFailClosed := httptest.NewRecorder()
	s.handleGetTaskPreview(wFailClosed, req)
	var resFailClosed map[string]any
	if err := json.NewDecoder(wFailClosed.Body).Decode(&resFailClosed); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if resFailClosed["drawerEnabled"] != false {
		t.Fatalf("expected drawerEnabled to be false when origins are missing, got %v", resFailClosed["drawerEnabled"])
	}

	// Now configure valid matching origins
	s.consoleOrigin = "http://console.example.com"
	s.previewOrigin = "http://preview.example.com"
	wActive := httptest.NewRecorder()
	s.handleGetTaskPreview(wActive, req)
	var resActive map[string]any
	if err := json.NewDecoder(wActive.Body).Decode(&resActive); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if resActive["drawerEnabled"] != true {
		t.Fatalf("expected drawerEnabled to be true when flag is set and origins match, got %v", resActive["drawerEnabled"])
	}

	// 2. Stop preview
	stopReq := providerTestRequest(http.MethodPost, "/api/v1/projects/testproj/tasks/t-123/preview/stop", "admin", nil)
	stopReq.SetPathValue("name", "testproj")
	stopReq.SetPathValue("taskId", "t-123")
	stopW := httptest.NewRecorder()

	s.handlePostTaskPreviewStop(stopW, stopReq)
	if stopW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", stopW.Code)
	}

	// 3. Test Proxy returns 503 when preview is stopped (token gate passes
	// first — round-13 ordering puts authorization before runtime state).
	proxyReq := httptest.NewRequest(http.MethodGet, "/preview/t-123/", nil)
	proxyReq.Header.Set(previewTokenHeader, s.signPreviewToken("t-123", "testproj"))
	proxyW := httptest.NewRecorder()
	s.handleTaskPreviewProxy(proxyW, proxyReq)
	if proxyW.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for stopped preview, got %d", proxyW.Code)
	}
}

func TestResolveTaskWorktreeDir(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	_ = s.st.SaveProject("myproj", &entity.Project{Name: "myproj"})

	// Setup fake workspace dir
	wsDir := filepath.Join(s.st.ProjectDir("myproj"), "workspace")
	_ = os.MkdirAll(wsDir, 0755)

	res := s.resolveTaskWorktreeDir("myproj", "t-123")
	if !strings.Contains(res, "workspace") && !strings.Contains(res, "myproj") {
		t.Fatalf("expected resolved workspace dir, got %s", res)
	}

	// 4. Test status (authenticated via identity context)
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/myproj/tasks/t-123/preview/status", nil).WithContext(context.WithValue(context.Background(), ctxUserKey, "admin"))
	statusReq.SetPathValue("name", "myproj")
	statusReq.SetPathValue("taskId", "t-123")
	statusW := httptest.NewRecorder()
	s.handleGetTaskPreviewStatus(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", statusW.Code)
	}
	var statusRes map[string]any
	if err := json.NewDecoder(statusW.Body).Decode(&statusRes); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if statusRes["busy"] != false {
		t.Fatalf("expected busy=false, got %v", statusRes["busy"])
	}
}

// §2.0.4: the injected interceptor keeps rewriting app subresource URLs under
// the /preview/ prefix, but carries no credentials and no Copilot plumbing.
func TestRewriteHTMLKeepsControlPlanePreviewAPIOutsideProjectPrefix(t *testing.T) {
	html := rewriteHTML(`<html><head></head><body><script>fetch('/api/v1/projects/testproj/tasks/t-123/preview/chat')</script><script>fetch('/api/data')</script></body></html>`, "t-123", "testproj")
	if !strings.Contains(html, "window.__MG_PREVIEW_BASE__") {
		t.Fatal("expected preview base marker in injected script")
	}
	if strings.Contains(html, "__MG_PREVIEW_TOKEN__") {
		t.Fatal("preview documents must not carry the share token (origin isolation)")
	}
	if strings.Contains(html, "feedback.js") {
		t.Fatal("preview documents must not inject the Copilot widget (origin isolation)")
	}
	if strings.Contains(html, "prefix + u.slice(1)") == false {
		t.Fatal("expected project URL rewriting to remain enabled")
	}
}

func TestTaskRemoteSyncRetryKeepsTerminalTaskOnPushFailure(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	s.worktreeMgr = gitworktree.NewManager()
	if err := s.st.SaveProject("syncproj", &entity.Project{Name: "syncproj", RemoteProvider: "github"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "syncproj", "agent", "aw-sync", "pm-sync")
	task := &entity.Task{
		ID:               "t-sync-retry",
		Title:            "sync",
		Assignee:         "syncproj/agent",
		Status:           entity.TaskStatusDoneSuccess,
		Prompt:           "sync",
		CompletionCommit: strings.Repeat("a", 40),
		BranchName:       "feature/t-sync-retry",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	if err := s.ts.AddTask("syncproj", "agent", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/syncproj/tasks/t-sync-retry/remote-sync/retry", "admin", nil)
	req.SetPathValue("name", "syncproj")
	req.SetPathValue("taskId", task.ID)
	w := httptest.NewRecorder()
	s.handlePostTaskRemoteSyncRetry(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected retry response 200, got %d: %s", w.Code, w.Body.String())
	}
	updated, _, err := s.findTaskInProject("syncproj", task.ID)
	if err != nil || updated == nil {
		t.Fatalf("find updated task: %v", err)
	}
	if updated.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("retry changed terminal task status to %s", updated.Status)
	}
	if updated.RemoteSyncStatus != "failed" || updated.RemoteSyncAttempts != 1 {
		t.Fatalf("unexpected retry metadata: status=%q attempts=%d error=%q", updated.RemoteSyncStatus, updated.RemoteSyncAttempts, updated.RemoteSyncError)
	}
}

func TestBuildPreviewEnvSnapshot(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	t.Run("no engine instance reports not started", func(t *testing.T) {
		s.previewEngine = nil
		snap := s.buildPreviewEnvSnapshot("p", "t-1", t.TempDir())
		if !strings.Contains(snap, "未启动") {
			t.Fatalf("expected not-started fact, got: %s", snap)
		}
		if !strings.Contains(snap, "以此为准") {
			t.Fatalf("expected authority declaration, got: %s", snap)
		}
	})

	t.Run("running instance and dirty git worktree", func(t *testing.T) {
		wt := t.TempDir()
		run := exec.Command("git", "-C", wt, "init", "-b", "main")
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v (%s)", err, out)
		}
		run = exec.Command("git", "-C", wt, "config", "user.email", "t@t")
		_ = run.Run()
		run = exec.Command("git", "-C", wt, "config", "user.name", "t")
		_ = run.Run()
		if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		run = exec.Command("git", "-C", wt, "add", ".")
		_ = run.Run()
		run = exec.Command("git", "-C", wt, "commit", "-m", "init")
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("git commit: %v (%s)", err, out)
		}
		if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("dirty"), 0o644); err != nil {
			t.Fatal(err)
		}

		s.previewEngine = preview.NewEngine()
		// Seed a running instance without launching containers.
		s.previewEngine.SeedInstanceForTest(&preview.PreviewInstance{
			TaskID: "t-2", Project: "p", Type: preview.ProjectTypeFullstack,
			Port: 8080, Status: "running",
		})

		snap := s.buildPreviewEnvSnapshot("p", "t-2", wt)
		if !strings.Contains(snap, "运行中") || !strings.Contains(snap, "8080") {
			t.Fatalf("expected running fact with port, got: %s", snap)
		}
		if !strings.Contains(snap, "feature/") && !strings.Contains(snap, "main") {
			t.Fatalf("expected branch fact, got: %s", snap)
		}
		if !strings.Contains(snap, "b.txt") || !strings.Contains(snap, "1 个文件") {
			t.Fatalf("expected dirty fact, got: %s", snap)
		}
	})

	t.Run("non-git directory reports no git instead of failing", func(t *testing.T) {
		s.previewEngine = nil
		snap := s.buildPreviewEnvSnapshot("p", "t-3", t.TempDir())
		if !strings.Contains(snap, "未检测到 Git 仓库") {
			t.Fatalf("expected no-git note, got: %s", snap)
		}
		if strings.Contains(snap, "当前分支") {
			t.Fatalf("must skip branch info without .git, got: %s", snap)
		}
	})
}

func TestPreviewChatDisabledFailsWith409FeatureDisabled(t *testing.T) {
	s := newPreviewControlServer(t)
	s.SetPreviewCopilotDrawerEnabled(true)
	s.consoleOrigin = "http://console.example.com"
	s.previewOrigin = "http://preview.example.com"
	newPreviewOperator(t, s, "opuser")

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"change button color"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "proj")
	req.SetPathValue("taskId", "t-1")

	// Seed task in progress
	task := &entity.Task{ID: "t-1", Status: entity.TaskStatusInProgress}
	_ = s.ts.AddTask("proj", "agent", task)

	// Auth request as operator
	authedReq := previewAuthedRequest(s, http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", "opuser", "")
	authedReq.SetPathValue("name", "proj")
	authedReq.SetPathValue("taskId", "t-1")
	authedReq.Body = io.NopCloser(strings.NewReader(`{"message":"change button color"}`))

	s.handlePostTaskPreviewChat(w, authedReq)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for preview chat when Slice B is disabled, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "feature_disabled") {
		t.Fatalf("expected feature_disabled error code, got: %s", w.Body.String())
	}

	comments, _ := s.ts.ListComments("proj", "agent", "t-1")
	if len(comments) != 0 {
		t.Fatalf("expected 0 task comments created on rejected chat, got %d", len(comments))
	}
}

func TestPreviewChatRejectsWhenDrawerDisabled(t *testing.T) {
	s := newPreviewControlServer(t)
	s.SetPreviewCopilotDrawerEnabled(false) // Drawer explicitly disabled
	newPreviewOperator(t, s, "opuser")

	w := httptest.NewRecorder()
	authedReq := previewAuthedRequest(s, http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", "opuser", "")
	authedReq.SetPathValue("name", "proj")
	authedReq.SetPathValue("taskId", "t-1")
	authedReq.Body = io.NopCloser(strings.NewReader(`{"message":"change button color"}`))

	s.handlePostTaskPreviewChat(w, authedReq)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when drawer is disabled, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "feature_disabled") {
		t.Fatalf("expected feature_disabled error code, got: %s", w.Body.String())
	}
}

func TestPreviewChatRejectsWhenOriginsMissingOrMismatched(t *testing.T) {
	s := newPreviewControlServer(t)
	s.SetPreviewCopilotDrawerEnabled(true)
	newPreviewOperator(t, s, "opuser")

	task := &entity.Task{ID: "t-1", Status: entity.TaskStatusInProgress}
	_ = s.ts.AddTask("proj", "agent", task)

	// Missing origins
	s.consoleOrigin = ""
	s.previewOrigin = ""
	w := httptest.NewRecorder()
	authedReq := previewAuthedRequest(s, http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", "opuser", "")
	authedReq.SetPathValue("name", "proj")
	authedReq.SetPathValue("taskId", "t-1")
	authedReq.Body = io.NopCloser(strings.NewReader(`{"message":"change button color"}`))
	s.handlePostTaskPreviewChat(w, authedReq)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when origins are missing, got %d: %s", w.Code, w.Body.String())
	}

	// Mismatched origins
	s.consoleOrigin = "https://console.company-a.com"
	s.previewOrigin = "https://preview.company-b.com"
	w2 := httptest.NewRecorder()
	authedReq2 := previewAuthedRequest(s, http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", "opuser", "")
	authedReq2.SetPathValue("name", "proj")
	authedReq2.SetPathValue("taskId", "t-1")
	authedReq2.Body = io.NopCloser(strings.NewReader(`{"message":"change button color"}`))
	s.handlePostTaskPreviewChat(w2, authedReq2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when origins mismatch, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestPreviewChatTerminalTaskRejected(t *testing.T) {
	s := newPreviewControlServer(t)
	s.SetPreviewCopilotDrawerEnabled(true)
	newPreviewOperator(t, s, "opuser")

	// Seed completed task
	task := &entity.Task{ID: "t-completed", Status: entity.TaskStatusDoneSuccess}
	_ = s.ts.AddTask("proj", "agent", task)

	w := httptest.NewRecorder()
	authedReq := previewAuthedRequest(s, http.MethodPost, "/api/v1/projects/proj/tasks/t-completed/preview/chat", "opuser", "")
	authedReq.SetPathValue("name", "proj")
	authedReq.SetPathValue("taskId", "t-completed")
	authedReq.Body = io.NopCloser(strings.NewReader(`{"message":"change button color"}`))

	s.handlePostTaskPreviewChat(w, authedReq)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for terminal task, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRewriteHTMLSanitizesDOMAndSetsExpectedConsoleOrigin(t *testing.T) {
	rawHTML := `<html><head></head><body><input id="pwd" type="password" name="password" value="mysecretpassword" placeholder="enter password"><button id="btn" class="btn btn-primary">Submit</button></body></html>`
	consoleOrigin := "https://console.example.com"
	rewritten := rewriteHTML(rawHTML, "t-secure", "myproj", consoleOrigin)

	// 1. Must carry expectedConsoleOrigin
	if !strings.Contains(rewritten, `var expectedConsoleOrigin = "https://console.example.com";`) {
		t.Fatalf("expected consoleOrigin to be configured in injected script, got: %s", rewritten)
	}

	// 2. Must carry fail-closed origin check in message handler
	if !strings.Contains(rewritten, `if (!expectedConsoleOrigin || e.origin !== expectedConsoleOrigin) return;`) {
		t.Fatal("expected fail-closed origin verification check in injected message event listener")
	}

	// 2b. Must ONLY post to window.parent, never window.top or wildcard '*'
	if strings.Contains(rewritten, `postMessage(msg, '*')`) || strings.Contains(rewritten, `window.top`) {
		t.Fatal("SECURITY VIOLATION: injected script must never post to window.top or use wildcard '*' origin")
	}

	// 3. Security: extractElementContext must NOT access .value or .placeholder or text
	if strings.Contains(rewritten, `el.value`) || strings.Contains(rewritten, `el.placeholder`) || strings.Contains(rewritten, `innerText`) {
		t.Fatal("SECURITY VIOLATION: injected script must NEVER read el.value, placeholder, or innerText (prevents credential harvesting)")
	}

	// 4. Security: extractElementContext must NOT extract outerHTML or parent:
	if strings.Contains(rewritten, `outerHTML`) || strings.Contains(rewritten, `parent:`) {
		t.Fatal("SECURITY VIOLATION: injected script must not extract raw outerHTML or parent tree")
	}
}

func TestRewriteHTMLOmitsInspectorWhenConsoleOriginMissing(t *testing.T) {
	rawHTML := `<html><head></head><body><h1>Hello</h1></body></html>`
	rewritten := rewriteHTML(rawHTML, "t-secure", "myproj", "")
	if strings.Contains(rewritten, `isInspectorActive`) || strings.Contains(rewritten, `mg-inspector-overlay`) {
		t.Fatal("expected inspector script to be completely omitted when consoleOrigin is empty")
	}
}
