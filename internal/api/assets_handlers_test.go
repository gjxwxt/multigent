package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

// setupAssetsTestServer isolates the blob store under a temp data dir and
// seeds a project task to bind against.
func setupAssetsTestServer(t *testing.T) (*Server, string, *entity.Task) {
	t.Helper()
	t.Setenv("MULTIGENT_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	task := &entity.Task{
		ID:        entity.NewTaskID(),
		Title:     "task with assets",
		Prompt:    "do the thing",
		Status:    entity.TaskStatusPending,
		Assignee:  "sample/pm",
		CreatedBy: "admin",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return s, workspaceID, task
}

func assetsAuthedRequest(t *testing.T, s *Server, method, target string, body []byte, contentType string) *http.Request {
	t.Helper()
	token := s.users.signJWT(jwtPayload{
		Sub: "admin",
		Exp: time.Now().Add(time.Hour).Unix(),
		Iat: time.Now().Add(-time.Minute).Unix(),
	})
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func assetsUploadFile(t *testing.T, s *Server, project, filename, content string) (fileID, sha string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	mw.Close()

	rec := httptest.NewRecorder()
	req := assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/"+project+"/assets", buf.Bytes(), mw.FormDataContentType())
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s: status=%d body=%s", filename, rec.Code, rec.Body.String())
	}
	var files []struct {
		ID         string `json:"id"`
		CurrentSha string `json:"currentSha"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &files); err != nil || len(files) != 1 {
		t.Fatalf("upload decode: err=%v body=%s", err, rec.Body.String())
	}
	sum := sha256.Sum256([]byte(content))
	return files[0].ID, hex.EncodeToString(sum[:])
}

func TestProjectAssetUploadListDownload(t *testing.T) {
	s, _, _ := setupAssetsTestServer(t)

	fileID, sha := assetsUploadFile(t, s, "sample", "需求说明.md", "# 需求\n\n## 1. 范围\n正文")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodGet, "/api/v1/projects/sample/assets", nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("list decode: err=%v body=%s", err, rec.Body.String())
	}
	if rows[0]["currentSha"] != sha || rows[0]["usageCount"].(float64) != 0 {
		t.Fatalf("list row mismatch: %+v", rows[0])
	}

	// Same content again dedups to the same sha, as a separate library file.
	fileID2, sha2 := assetsUploadFile(t, s, "sample", "copy.md", "# 需求\n\n## 1. 范围\n正文")
	if sha2 != sha {
		t.Fatalf("identical content must hash identically")
	}
	if fileID2 == fileID {
		t.Fatalf("distinct uploads produce distinct file rows")
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodGet, "/api/v1/projects/sample/assets/"+fileID+"/download", nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("download: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "# 需求\n\n## 1. 范围\n正文" {
		t.Fatalf("download bytes mismatch: %q", got)
	}
}

func TestTaskAssetBindUnbindFlow(t *testing.T) {
	s, _, task := setupAssetsTestServer(t)
	fileID, sha := assetsUploadFile(t, s, "sample", "需求说明.md", "# 需求\n正文")

	bindBody := `{"fileId":"` + fileID + `","role":"requirement_input","required":true,"sha256":"` + sha + `"}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/assets", []byte(bindBody), "application/json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("bind: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Task listing surfaces fileCurrentSha for the drift indicator.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodGet, "/api/v1/projects/sample/tasks/"+task.ID+"/assets", nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("task assets: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("task assets decode: err=%v body=%s", err, rec.Body.String())
	}
	if rows[0]["sha256"] != sha || rows[0]["fileCurrentSha"] != sha || rows[0]["role"] != "requirement_input" {
		t.Fatalf("task asset row mismatch: %+v", rows[0])
	}
	attachmentID := rows[0]["id"].(string)

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodDelete, "/api/v1/projects/sample/tasks/"+task.ID+"/assets/"+attachmentID, nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodGet, "/api/v1/projects/sample/tasks/"+task.ID+"/assets", nil, ""))
	var empty []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &empty)
	if len(empty) != 0 {
		t.Fatalf("unbind must empty the task's asset list: %+v", empty)
	}
}

// TestTaskAssetBindByHashClosure pins the security invariant: bindings only
// go through a file row visible in the caller's project. A bare SHA of
// content the caller cannot read is never a binding source.
func TestTaskAssetBindByHashClosure(t *testing.T) {
	s, _, task := setupAssetsTestServer(t)
	fileID, _ := assetsUploadFile(t, s, "sample", "internal-notes.md", "confidential content")

	if err := s.st.SaveProject("other", &entity.Project{Name: "other"}); err != nil {
		t.Fatalf("save other project: %v", err)
	}
	otherTask := &entity.Task{ID: entity.NewTaskID(), Title: "t", Prompt: "p", Status: entity.TaskStatusPending, Assignee: "other/pm", CreatedAt: time.Now().UTC()}
	if err := s.ts.AddTask("other", "pm", otherTask); err != nil {
		t.Fatalf("AddTask other: %v", err)
	}

	// (a) Cross-project bind attempt: the file belongs to "sample".
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/other/tasks/"+otherTask.ID+"/assets",
		[]byte(`{"fileId":"`+fileID+`","role":"reference"}`), "application/json"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project bind must be rejected, got %d body=%s", rec.Code, rec.Body.String())
	}

	// (b) Sha mismatch with the file's current version.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/assets",
		[]byte(`{"fileId":"`+fileID+`","sha256":"`+strings.Repeat("0", 64)+`"}`), "application/json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sha mismatch bind must be rejected, got %d", rec.Code)
	}

	// (c) Unknown file id.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/assets",
		[]byte(`{"fileId":"asf-does-not-exist"}`), "application/json"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown file bind must 404, got %d", rec.Code)
	}
}

func TestTaskAssetBudget(t *testing.T) {
	s, _, task := setupAssetsTestServer(t)
	for i := 0; i < 8; i++ {
		fileID, _ := assetsUploadFile(t, s, "sample", "doc-"+string(rune('a'+i))+".md", "content "+string(rune('a'+i)))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/assets",
			[]byte(`{"fileId":"`+fileID+`","role":"reference"}`), "application/json"))
		if rec.Code != http.StatusOK {
			t.Fatalf("bind %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	// The 9th binding must be an explicit refusal, never a silent truncation.
	fileID, _ := assetsUploadFile(t, s, "sample", "doc-z.md", "content z")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, assetsAuthedRequest(t, s, http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/assets",
		[]byte(`{"fileId":"`+fileID+`","role":"reference"}`), "application/json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-budget bind must be rejected, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maximum") {
		t.Fatalf("over-budget error must be explicit: %s", rec.Body.String())
	}
}
