package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
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

	// 2. Stop preview
	stopReq := providerTestRequest(http.MethodPost, "/api/v1/projects/testproj/tasks/t-123/preview/stop", "admin", nil)
	stopReq.SetPathValue("name", "testproj")
	stopReq.SetPathValue("taskId", "t-123")
	stopW := httptest.NewRecorder()

	s.handlePostTaskPreviewStop(stopW, stopReq)
	if stopW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", stopW.Code)
	}

	// 3. Test Proxy returns 503 when preview is stopped
	proxyReq := httptest.NewRequest(http.MethodGet, "/preview/t-123/", nil)
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

	// 4. Test status
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects/myproj/tasks/t-123/preview/status", nil)
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
