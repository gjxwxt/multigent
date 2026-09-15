package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Task 1.0 (§2.0 origin isolation) test matrix:
//   - preview origin unset  -> /preview/* 404 on every origin (fail-closed)
//   - preview origin set    -> served on that origin only
//   - console-origin /preview/* -> 302 to preview origin
//   - foreign Host -> 404 (no open redirect, no request-input echo)
//   - token exchange -> HttpOnly cookie + 302 clean URL, hardened headers
//   - CORS allowlist -> only the configured console origin gets headers;
//     the preview origin (and every other Origin) gets none
//   - no credentials in preview documents (widget/token injection removed)

func newPreviewOriginServer(t *testing.T, previewOrigin, consoleOrigin string) *Server {
	t.Helper()
	s, _ := newConnectionGrantPolicyServer(t)
	s.SetPreviewOrigin(previewOrigin)
	s.SetConsoleOrigin(consoleOrigin)
	return s
}

// previewOriginRequest builds a request as the deployed reverse proxy would
// deliver it to the given origin (scheme resolved via X-Forwarded-Proto).
func previewOriginRequest(method, target, origin string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if u, err := url.Parse(origin); err == nil {
		req.Host = u.Host
		req.Header.Set("X-Forwarded-Proto", u.Scheme)
	}
	return req
}

func TestPreviewOriginUnconfiguredFailsClosed(t *testing.T) {
	s := newPreviewOriginServer(t, "", "")

	handler := s.Handler()
	for _, origin := range []string{"https://console.example.com", "https://preview.example.com", "https://evil.example.com"} {
		req := previewOriginRequest(http.MethodGet, "/preview/t-1/", origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("origin %q: expected 404 fail-closed when preview origin unset, got %d", origin, w.Code)
		}
	}
}

func TestPreviewOriginGateRoutesByConfiguredOrigin(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "https://console.example.com")

	// Preview origin: request passes the gate (503 — no running preview
	// instance in the test — but never 404/401 from the gate itself).
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, previewOriginRequest(http.MethodGet, "/preview/t-gate/", "https://preview.example.com"))
	if w.Code == http.StatusNotFound {
		t.Fatalf("preview origin must pass the gate, got 404: %s", w.Body.String())
	}

	// Console origin: redirected to the preview origin (old links keep working).
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, previewOriginRequest(http.MethodGet, "/preview/t-gate/", "https://console.example.com"))
	if w.Code != http.StatusFound {
		t.Fatalf("console origin /preview/ must redirect, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "https://preview.example.com/preview/") {
		t.Fatalf("redirect must target the configured preview origin, got %q", loc)
	}

	// Foreign Host: 404, and the response must not redirect or echo it.
	req := previewOriginRequest(http.MethodGet, "/preview/t-gate/", "https://evil.example.com")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign host must 404, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "evil.example.com") {
		t.Fatal("error output must not echo the request Host")
	}
}

func TestPreviewTokenExchangeSetsCookieAndCleansURL(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "")
	token := s.signPreviewToken("t-exc", "proj")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, previewOriginRequest(http.MethodGet, "/preview/t-exc/?pvt="+token+"&x=keep", "https://preview.example.com"))

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 from token exchange, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("exchange response must set Cache-Control: no-store")
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("exchange response must set Referrer-Policy: no-referrer")
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "pvt=") {
		t.Fatalf("Location must be token-free, got %q", loc)
	}
	if !strings.HasPrefix(loc, "https://preview.example.com/preview/t-exc/") {
		t.Fatalf("Location must stay on the preview origin, got %q", loc)
	}

	cookies := w.Result().Cookies()
	var pvt *http.Cookie
	for _, c := range cookies {
		if c.Name == previewTokenCookiePrefix+"t-exc" {
			pvt = c
		}
	}
	if pvt == nil {
		t.Fatalf("expected %st-exc cookie, got %v", previewTokenCookiePrefix, cookies)
	}
	if !pvt.HttpOnly {
		t.Fatal("exchange cookie must be HttpOnly")
	}
	if !pvt.Secure {
		t.Fatal("exchange cookie must be Secure on an https origin")
	}
	if pvt.SameSite != http.SameSiteLaxMode {
		t.Fatal("exchange cookie must be SameSite=Lax")
	}
	if pvt.Path != "/preview/t-exc/" {
		t.Fatalf("cookie must be path-scoped to the task preview, got %q", pvt.Path)
	}
	if pvt.Value != token {
		t.Fatal("cookie must carry the validated token")
	}
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("exchange response body must never echo the token")
	}
}

func TestPreviewTokenExchangeRejectsInvalidTokenWithoutEcho(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "")

	stolen := "forged-token-value"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, previewOriginRequest(http.MethodGet, "/preview/t-exc/?pvt="+stolen, "https://preview.example.com"))

	if w.Code != http.StatusFound {
		t.Fatalf("invalid token still redirects to the clean URL, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), stolen) {
		t.Fatal("invalid token must not be echoed back")
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == previewTokenCookiePrefix+"t-exc" {
			t.Fatal("invalid token must not set a cookie")
		}
	}
}

func TestCORSAllowlistExcludesPreviewOriginAndUnknown(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "https://console.example.com")

	// Allowlisted console origin: preflight gets full CORS headers.
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/projects", nil)
	req.Host = "console.example.com"
	req.Header.Set("Origin", "https://console.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://console.example.com" {
		t.Fatalf("allowlisted preflight must pass, got %d ACAO=%q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}

	// The preview origin is deliberately NOT on the console allowlist.
	req = httptest.NewRequest(http.MethodOptions, "/api/v1/projects", nil)
	req.Host = "console.example.com"
	req.Header.Set("Origin", "https://preview.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preview origin must not receive console CORS headers, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}

	// Arbitrary origins get nothing (reflection is gone).
	req = httptest.NewRequest(http.MethodOptions, "/api/v1/projects", nil)
	req.Host = "console.example.com"
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unlisted origin must not receive CORS headers, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}

	// No Origin header (same-origin tooling): no ACAO either — the wildcard
	// reflection fallback is removed.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Host = "console.example.com"
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("requests without Origin must not receive ACAO, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestPreviewDocumentHasNoInjectedCredentials(t *testing.T) {
	html := `<!DOCTYPE html><html><head><title>app</title></head><body><a href="/about">x</a></body></html>`
	out := rewriteHTML(html, "t-doc", "proj")

	if strings.Contains(out, "__MG_PREVIEW_TOKEN__") {
		t.Fatal("preview documents must not carry __MG_PREVIEW_TOKEN__ (§2.0.4)")
	}
	if strings.Contains(out, "feedback.js") {
		t.Fatal("preview documents must not inject the Copilot widget (§2.0.4)")
	}
	// The URL-rewriting interceptor itself remains (app assets must keep
	// working under the /preview/ prefix).
	if !strings.Contains(out, "__MG_PREVIEW_BASE__") {
		t.Fatal("URL-rewrite interceptor must remain for app subresources")
	}
}

func TestPreviewProxyExchangeServesCleanDocument(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "")
	token := s.signPreviewToken("t-x2", "proj")

	// End-to-end through Handler(): the pvt-carrying GET must be exchanged
	// (cookie + clean 302) before any instance check or document render.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, previewOriginRequest(http.MethodGet, "/preview/t-x2/?pvt="+token, "https://preview.example.com"))
	if w.Code != http.StatusFound {
		t.Fatalf("proxy must exchange pvt before instance check, got %d: %s", w.Code, w.Body.String())
	}
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == previewTokenCookiePrefix+"t-x2" && c.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("proxy exchange must set the HttpOnly cookie")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("proxy exchange must carry the hardened headers")
	}
}

// Round-11 P0: the cookie Secure flag and the scheme check derive from the
// CONFIGURED preview origin — never from the request's X-Forwarded-Proto.
// When the resolved request scheme contradicts the configured scheme, the
// exchange fails closed: no token cookie is issued at all (the response is a
// token-free redirect, never a document).
func TestPreviewCookieSecureDecidedByConfiguredOriginScheme(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "")
	token := s.signPreviewToken("t-sec", "proj")

	// XFP present and consistent with the configured https origin: exchange
	// succeeds and the cookie is Secure.
	w := httptest.NewRecorder()
	req := previewOriginRequest(http.MethodGet, "/preview/t-sec/?pvt="+token, "https://preview.example.com")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("consistent-scheme exchange must 302, got %d", w.Code)
	}
	var secureCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == previewTokenCookiePrefix+"t-sec" {
			secureCookie = c
		}
	}
	if secureCookie == nil || !secureCookie.Secure {
		t.Fatalf("cookie must exist and be Secure on the https-configured origin, got %+v", secureCookie)
	}

	// Proxy DROPS X-Forwarded-Proto (scheme resolves to http != configured
	// https): fail-closed — no token cookie, response is a token-free 302.
	req = httptest.NewRequest(http.MethodGet, "/preview/t-sec/?pvt="+token, nil)
	req.Host = "preview.example.com"
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == previewTokenCookiePrefix+"t-sec" {
			t.Fatalf("scheme-mismatched request must not receive the token cookie, got %+v", c)
		}
	}
	if strings.Contains(w.Header().Get("Location"), "pvt=") || strings.Contains(w.Body.String(), token) {
		t.Fatal("scheme-mismatched exchange must never echo the token")
	}
}

// Round-11 P0: a request whose resolved scheme contradicts the configured
// preview origin never receives a token cookie (fail-closed).
func TestPreviewCookieRefusedOnSchemeMismatch(t *testing.T) {
	s := newPreviewOriginServer(t, "https://preview.example.com", "")
	token := s.signPreviewToken("t-mix", "proj")

	// Attacker or misdirected traffic hits the backend directly over http
	// while the deployment declares https — forged XFP or not, no cookie.
	req := httptest.NewRequest(http.MethodGet, "/preview/t-mix/?pvt="+token, nil)
	req.Host = "preview.example.com"
	req.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == previewTokenCookiePrefix+"t-mix" {
			t.Fatalf("scheme mismatch must refuse the token cookie, got %+v", c)
		}
	}
	// The exchange must fail closed on scheme mismatch: any rejection is
	// acceptable (404 from the cookie boundary or 302), but never a document,
	// never a cookie, never a token echo.
	if strings.Contains(w.Header().Get("Location"), "pvt=") || strings.Contains(w.Body.String(), token) {
		t.Fatalf("mismatch must reject without echoing the token, got %d body=%.100s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusOK {
		t.Fatalf("scheme mismatch must not render the document, got %d", w.Code)
	}
}

// Round-11 P0 companion: an http-configured preview origin (http deployments)
// gets non-Secure cookies and accepts http-scheme requests.
func TestPreviewCookieAllowsConfiguredHTTPOrigin(t *testing.T) {
	s := newPreviewOriginServer(t, "http://preview.internal:8080", "")
	token := s.signPreviewToken("t-http", "proj")

	req := httptest.NewRequest(http.MethodGet, "/preview/t-http/?pvt="+token, nil)
	req.Host = "preview.internal:8080"
	req.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == previewTokenCookiePrefix+"t-http" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("http-configured origin must still issue the exchange cookie")
	}
	if cookie.Secure {
		t.Fatal("Secure must be false when the configured origin is http")
	}
}
