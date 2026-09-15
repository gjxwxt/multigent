package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

func TestPreviewTokenRoundtrip(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	token := s.signPreviewToken("t-abc", "proj")
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	claims, ok := s.verifyPreviewToken(token, "t-abc")
	if !ok {
		t.Fatal("expected token to verify for its task")
	}
	if claims.Project != "proj" {
		t.Fatalf("unexpected project claim: %q", claims.Project)
	}
	if _, ok := s.verifyPreviewToken(token, "t-other"); ok {
		t.Fatal("token must not verify for a different task")
	}
	if _, ok := s.verifyPreviewToken(token+"x", "t-abc"); ok {
		t.Fatal("tampered token must not verify")
	}
	if _, ok := s.verifyPreviewToken(s.signPreviewTokenWithTTL("t-abc", "proj", -time.Minute), "t-abc"); ok {
		t.Fatal("expired token must not verify")
	}
}

func TestPreviewRequestAuthorized(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("proj", &entity.Project{Name: "proj"}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// No token and no identity -> unauthorized.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", nil)
	if s.previewRequestAuthorized(w, req, "proj", "t-1") {
		t.Fatal("expected unauthorized request to be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// Valid preview token bound to the task -> admitted without a user.
	token := s.signPreviewToken("t-1", "proj")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat?pvt="+token, nil)
	w = httptest.NewRecorder()
	if !s.previewRequestAuthorized(w, req, "proj", "t-1") {
		t.Fatalf("expected preview token to authorize, got %d: %s", w.Code, w.Body.String())
	}

	// Header placement also works.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/tasks/t-1/preview/chat", nil)
	req.Header.Set(previewTokenHeader, token)
	w = httptest.NewRecorder()
	if !s.previewRequestAuthorized(w, req, "proj", "t-1") {
		t.Fatal("expected preview token header to authorize")
	}

	// Token bound to another task must be rejected even for an endpoint
	// that would otherwise accept any authenticated user.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/tasks/t-1/preview/status?pvt="+s.signPreviewToken("t-2", "proj"), nil)
	w = httptest.NewRecorder()
	if s.previewRequestAuthorized(w, req, "proj", "t-1") {
		t.Fatal("token for a different task must not authorize")
	}
}

func TestAllowPreviewChatRateLimit(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	admitted := 0
	for i := 0; i < previewChatRateLimit+5; i++ {
		if s.allowPreviewChat("t-rate") {
			admitted++
		}
	}
	if admitted != previewChatRateLimit {
		t.Fatalf("expected exactly %d admitted, got %d", previewChatRateLimit, admitted)
	}
	if !s.allowPreviewChat("t-other") {
		t.Fatal("rate limit must be per task")
	}
}

func TestPreviewProxyRequiresToken(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	// Anonymous request: the token gate now runs BEFORE the instance check
	// (round-13 ordering — authorization must not depend on runtime state).
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/preview/t-404/", nil)
	s.handleTaskPreviewProxy(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tokenless request, got %d", w.Code)
	}

	// Stopped preview with a valid view-capable token keeps its actionable
	// 503 ("launch it from the task review panel").
	token := s.signPreviewToken("t-404", "current")
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/preview/t-404/", nil)
	req.Header.Set(previewTokenHeader, token)
	s.handleTaskPreviewProxy(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for stopped preview, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not running") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}
