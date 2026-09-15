package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/rbac"
)

// Task 1.1 (§2.1 control-plane lockdown) test matrix. Actor states × the
// five control endpoints:
//
//	anonymous             -> 401
//	share token only      -> 403 (feedback/chat/live/stop), 401 (status)
//	logged-in member      -> 403 (operator required)
//	operator              -> allowed on task surfaces
//
// Plus: Bearer-only semantics (a stolen pvt next to a non-operator Bearer
// must not soften anything), legacy no-Cap tokens read but carry no
// capability, and comment authorship carries the real username.

func newPreviewControlServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	return s
}

func newPreviewOperator(t *testing.T, s *Server, username string) {
	t.Helper()
	if err := s.users.CreateUser(username, "pass123", RoleMember, "", "", "", "", ""); err != nil {
		t.Fatalf("create %s: %v", username, err)
	}
	if err := s.users.UpdateUser(username, nil, nil, nil, nil, nil, nil, nil,
		[]projectAccess{{Project: "proj", Role: string(rbac.ProjectRoleOperator)}}, nil, nil); err != nil {
		t.Fatalf("grant operator on proj: %v", err)
	}
}

func previewAuthedRequest(s *Server, method, path, username, pvt string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if username != "" {
		token := s.users.IssueToken(username, time.Hour)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if pvt != "" {
		req.Header.Set(previewTokenHeader, pvt)
	}
	return req
}

// previewControlCases enumerates the locked control surfaces with a handler
// dispatch key. All paths bind project "proj" and a task id.
func previewControlCases() []struct {
	key    string
	method string
	path   string
} {
	return []struct {
		key    string
		method string
		path   string
	}{
		{"feedback", http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/feedback"},
		{"chat", http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat"},
		{"live", http.MethodGet, "/api/v1/projects/proj/tasks/t-1/preview/live"},
		{"stop", http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/stop"},
		{"status", http.MethodGet, "/api/v1/projects/proj/tasks/t-1/preview/status"},
	}
}

func dispatchPreviewControl(s *Server, key string, w http.ResponseWriter, req *http.Request) {
	req.SetPathValue("name", "proj")
	req.SetPathValue("taskId", pathTaskID(req.URL.Path))
	switch key {
	case "feedback":
		s.handlePostTaskPreviewFeedback(w, req)
	case "chat":
		s.handlePostTaskPreviewChat(w, req)
	case "live":
		s.handleGetTaskPreviewLive(w, req)
	case "stop":
		s.handlePostTaskPreviewStop(w, req)
	case "status":
		s.handleGetTaskPreviewStatus(w, req)
	}
}

func pathTaskID(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "tasks" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func TestPreviewControlAnonymousDenied(t *testing.T) {
	s := newPreviewControlServer(t)
	for _, tc := range previewControlCases() {
		w := httptest.NewRecorder()
		dispatchPreviewControl(s, tc.key, w, previewAuthedRequest(s, tc.method, tc.path, "", ""))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: anonymous must get 401, got %d: %s", tc.key, w.Code, w.Body.String())
		}
	}
}

func TestPreviewControlShareTokenOnlyDenied(t *testing.T) {
	s := newPreviewControlServer(t)
	token := s.signPreviewToken("t-1", "proj")
	for _, tc := range previewControlCases() {
		w := httptest.NewRecorder()
		dispatchPreviewControl(s, tc.key, w, previewAuthedRequest(s, tc.method, tc.path, "", token))
		// feedback/chat/live/stop are capability-denied (403); status is
		// login-required (401) pending its response-body leakage audit.
		want := http.StatusForbidden
		if tc.key == "status" {
			want = http.StatusUnauthorized
		}
		if w.Code != want {
			t.Fatalf("%s: share-token-only must get %d, got %d: %s", tc.key, want, w.Code, w.Body.String())
		}
	}
}

func TestPreviewControlBearerOnlyStolenPvtDoesNotElevate(t *testing.T) {
	s := newPreviewControlServer(t)
	token := s.signPreviewToken("t-1", "proj")
	// A logged-in member WITHOUT operator rights carrying a (stolen) valid
	// share token: Bearer-only semantics — the pvt must not soften anything;
	// the operator gate decides. status is login-level and stays open.
	if err := s.users.CreateUser("member", "pass123", RoleMember, "", "", "", "", ""); err != nil {
		t.Fatalf("create member: %v", err)
	}
	for _, tc := range previewControlCases() {
		if tc.key == "status" {
			continue
		}
		w := httptest.NewRecorder()
		dispatchPreviewControl(s, tc.key, w, previewAuthedRequest(s, tc.method, tc.path, "member", token))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: non-operator member with pvt must get 403, got %d: %s", tc.key, w.Code, w.Body.String())
		}
	}
}

func TestPreviewControlStatusOpenToLoggedInMember(t *testing.T) {
	s := newPreviewControlServer(t)
	// Round-11 P0 fix: status needs login AND project read membership.
	// A member without proj membership gets 403; a proj member gets 200.
	if err := s.users.CreateUser("outsider", "pass123", RoleMember, "", "", "", "", ""); err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	w := httptest.NewRecorder()
	dispatchPreviewControl(s, "status", w, previewAuthedRequest(s, http.MethodGet, "/api/v1/projects/proj/tasks/t-1/preview/status", "outsider", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status for logged-in user WITHOUT proj membership must be 403, got %d: %s", w.Code, w.Body.String())
	}

	newPreviewOperator(t, s, "projmember")
	w = httptest.NewRecorder()
	dispatchPreviewControl(s, "status", w, previewAuthedRequest(s, http.MethodGet, "/api/v1/projects/proj/tasks/t-1/preview/status", "projmember", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status for logged-in proj member must be 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPreviewControlOperatorAllowed(t *testing.T) {
	s := newPreviewControlServer(t)
	newPreviewOperator(t, s, "opuser")
	for _, tc := range previewControlCases() {
		w := httptest.NewRecorder()
		dispatchPreviewControl(s, tc.key, w, previewAuthedRequest(s, tc.method, tc.path, "opuser", ""))
		// status/live/stop/feedback/chat all pass the authz gate for an
		// operator; downstream state (missing task/session) yields 404/200,
		// never 401/403.
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Fatalf("%s: operator must pass the control gate, got %d: %s", tc.key, w.Code, w.Body.String())
		}
	}
}

func TestPreviewProxyStillAcceptsTokenRead(t *testing.T) {
	s := newPreviewControlServer(t)
	token := s.signPreviewToken("t-1", "proj")
	req := httptest.NewRequest(http.MethodGet, "/preview/t-1/", nil)
	req.Header.Set(previewTokenHeader, token)
	w := httptest.NewRecorder()
	s.handleTaskPreviewProxy(w, req)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Fatalf("proxy read with fresh view-capable token must not be rejected, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLegacyTokenWithoutCapVerifiesViewOnly(t *testing.T) {
	s := newPreviewControlServer(t)
	// Hand-mint a legacy token with no Cap field (pre-Task-1.1 share link).
	legacyClaims := previewTokenClaims{TaskID: "t-1", Project: "proj", Exp: time.Now().Add(time.Hour).Unix()}
	legacy := s.signPreviewTokenClaims(legacyClaims)
	if _, ok := s.verifyPreviewToken(legacy, "t-1"); !ok {
		t.Fatal("legacy token (no Cap) must still verify for reads (backward compatible)")
	}
	if previewTokenHasCapability(legacyClaims, previewCapabilityView) {
		t.Fatal("legacy claims without Cap must not carry any capability")
	}
	// Fresh mints carry the view capability explicitly.
	freshClaims, ok := s.verifyPreviewToken(s.signPreviewToken("t-1", "proj"), "t-1")
	if !ok || !previewTokenHasCapability(freshClaims, previewCapabilityView) {
		t.Fatalf("fresh tokens must mint with %s capability", previewCapabilityView)
	}
}

func TestPreviewCommentAuthorIsPrincipal(t *testing.T) {
	s := newPreviewControlServer(t)
	newPreviewOperator(t, s, "opuser")
	workspaceID := s.currentWorkspaceIDValue(nil)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "proj", "pm", "aw-prev", "pm-prev")
	task := &entity.Task{
		ID:        "t-author",
		Title:     "author check",
		Assignee:  "proj/pm",
		Status:    entity.TaskStatusDoneSuccess,
		Prompt:    "author check",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.ts.AddTask("proj", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	before, _ := s.ts.ListComments("proj", "pm", "t-author")

	w := httptest.NewRecorder()
	req := previewAuthedRequest(s, http.MethodPost, "/api/v1/projects/proj/tasks/t-author/preview/feedback", "opuser", "")
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(strings.NewReader(`{"feedback":"fix the button"}`))
	dispatchPreviewControl(s, "feedback", w, req)

	after, _ := s.ts.ListComments("proj", "pm", "t-author")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(after) != len(before)+1 {
		t.Fatalf("expected +1 comment, got %d -> %d", len(before), len(after))
	}
	last := after[len(after)-1]
	if last.Author != "opuser" {
		t.Fatalf("comment author must be the authenticated principal, got %q", last.Author)
	}
	if !strings.HasPrefix(last.Body, "[Preview Feedback] ") {
		t.Fatalf("unexpected comment body: %q", last.Body)
	}
}
