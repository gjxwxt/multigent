package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// Preview origin isolation (batch plan §2.0, review rounds 7-10 verdict).
//
// The previewed application is untrusted: any script it executes must not be
// able to reach console credentials. While preview and console shared one
// origin, app scripts could read localStorage["multigent-token"] and call the
// console API with a real Bearer. The fix is origin separation: preview is
// served only on an explicitly configured origin (MULTIGENT_PREVIEW_ORIGIN),
// never on the console origin, and the console CORS allowlist never includes
// the preview origin. Both origins come from deployment config — never
// derived from the request Host, which an attacker can forge or misconfigure.

const (
	// PreviewOriginEnv is the deployment config key for the explicit preview
	// origin, e.g. "https://preview.example.com". Empty (default) means
	// preview surfaces are disabled (fail-closed), never silently same-origin.
	PreviewOriginEnv = "MULTIGENT_PREVIEW_ORIGIN"
	// ConsoleOriginEnv is the deployment config key for the console's public
	// origin, used solely to build the CORS allowlist. Empty (default) means
	// the legacy wildcard fallback for non-browser API clients.
	ConsoleOriginEnv = "MULTIGENT_CONSOLE_ORIGIN"
)

// previewCapabilityView is the only capability preview share tokens carry
// (Task 1.1). Tokens minted before the Cap field existed verify without a Cap
// and are treated as read-only by callers that require it.
const previewCapabilityView = "preview.view"

const previewTokenCookiePrefix = "mg_pvt_"

// SetPreviewOrigin configures the explicit preview origin (deployment config).
// Call before Handler() is served. An empty or invalid value disables preview
// surfaces entirely (fail-closed) — there is no same-origin fallback.
func (s *Server) SetPreviewOrigin(origin string) {
	s.previewOrigin = normalizeConfiguredOrigin(origin, PreviewOriginEnv,
		"preview surfaces stay disabled (fail-closed)")
}

// SetConsoleOrigin configures the console public origin for the CORS
// allowlist (deployment config). Empty restores the wildcard legacy behavior.
func (s *Server) SetConsoleOrigin(origin string) {
	s.consoleOrigin = normalizeConfiguredOrigin(origin, ConsoleOriginEnv,
		"CORS allowlist falls back to the wildcard legacy behavior")
}

// normalizeConfiguredOrigin trims, scheme-defaults, and validates an origin
// from deployment config. Invalid non-empty values log a warning and disable
// the feature the origin drives (fail-closed over guesswork).
func normalizeConfiguredOrigin(origin, env, fallback string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ""
	}
	if !strings.Contains(origin, "://") {
		origin = "https://" + origin
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		log.Printf("invalid %s %q: %s", env, origin, fallback)
		return ""
	}
	return origin
}

// PreviewOrigin returns the configured preview origin ("" when disabled).
func (s *Server) PreviewOrigin() string { return s.previewOrigin }

// previewOriginDisabled reports whether preview surfaces must fail closed.
func (s *Server) previewOriginDisabled() bool { return s.previewOrigin == "" }

// requestOnPreviewOrigin reports whether the request arrived on the
// configured preview origin. The configured origin is the only input to the
// decision — the request Host is compared, never consulted as configuration.
// The scheme is compared by RESOLVED request scheme; see previewHostMatches
// for the Host-only comparison the origin gate uses before scheme checks.
func (s *Server) requestOnPreviewOrigin(r *http.Request) bool {
	if s.previewOriginDisabled() {
		return false
	}
	u := &url.URL{Scheme: requestScheme(r), Host: r.Host}
	return u.String() == s.previewOrigin
}

// previewHostOnConfiguredOrigin compares only the Host against the
// configured preview origin's host. The origin gate uses this so that a
// dropped X-Forwarded-Proto (misconfigured proxy) does not 404 legitimate
// preview traffic; scheme fidelity is enforced at the cookie boundary
// (setPreviewCookie) instead, where the Secure flag is also decided by the
// configured scheme.
func (s *Server) previewHostOnConfiguredOrigin(r *http.Request) bool {
	if s.previewOriginDisabled() {
		return false
	}
	u, err := url.Parse(s.previewOrigin)
	if err != nil {
		return false
	}
	return strings.EqualFold(r.Host, u.Host)
}

// requestScheme resolves the request scheme, honoring the proxy header set by
// the deployed reverse proxy. It is used only for origin comparison and
// cookie Secure flags — never to mint URLs from request input.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		parts := strings.Split(proto, ",")
		return strings.ToLower(strings.TrimSpace(parts[0]))
	}
	return "http"
}

// enforcePreviewOriginGate rejects preview-surface requests that did not
// arrive on the configured preview origin. Returns true when the request was
// handled (rejection or redirect written) and the caller must stop.
//
//   - Preview not configured -> 404 on every origin (fail-closed).
//   - Request Host == configured preview host -> pass (scheme fidelity is
//     enforced at the cookie boundary, so a dropped X-Forwarded-Proto on a
//     misconfigured proxy degrades to a cookie refusal, not a 404).
//   - Request on the configured console origin -> 302 to the equivalent
//     preview-origin URL so pre-split console links keep working.
//   - Any other Host -> 404; the response never echoes request input.
func (s *Server) enforcePreviewOriginGate(w http.ResponseWriter, r *http.Request) bool {
	if s.previewOriginDisabled() {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound,
			"preview origin not configured; set "+PreviewOriginEnv+" to enable preview sharing")
		return true
	}
	if s.previewHostOnConfiguredOrigin(r) {
		return false
	}
	if s.requestOnConsoleOrigin(r) {
		if u, err := url.Parse(s.previewOrigin); err == nil {
			target := *r.URL
			target.Scheme = u.Scheme
			target.Host = u.Host
			http.Redirect(w, r, target.String(), http.StatusFound)
			return true
		}
	}
	s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "preview is served from "+s.previewOrigin)
	return true
}

// requestOnConsoleOrigin reports whether the request Host matches the
// deployment-configured console origin (exact host comparison; scheme is
// resolved the same way as for the preview origin).
func (s *Server) requestOnConsoleOrigin(r *http.Request) bool {
	if s.consoleOrigin == "" {
		return false
	}
	u := &url.URL{Scheme: requestScheme(r), Host: r.Host}
	return u.String() == s.consoleOrigin
}

// extractPreviewTaskID pulls the task id from a /preview/{task}/... path.
func extractPreviewTaskID(path string) string {
	trimmed := strings.TrimPrefix(path, "/preview/")
	if trimmed == "" {
		return ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if parts[0] == "" {
		return ""
	}
	return parts[0]
}

// handlePreviewTokenExchange implements GET /preview/{task}/?pvt=<token> ->
// validate -> Set-Cookie (HttpOnly, path-scoped) -> 302 to the token-free
// URL. Hardening per round 9: Cache-Control: no-store (token never enters a
// cache), Referrer-Policy: no-referrer (token never leaks via Referer), and
// the Location URL carries no token. Project scripts therefore never see the
// token in location.search of any subsequent document.
func (s *Server) handlePreviewTokenExchange(w http.ResponseWriter, r *http.Request) {
	taskID := extractPreviewTaskID(r.URL.Path)
	if taskID == "" {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get(previewTokenQuery))
	if _, ok := s.verifyPreviewToken(token, taskID); token == "" || !ok {
		// Redirect to the clean URL even on failure: an expired or invalid
		// token must not render an error page that echoes the token, and the
		// review panel will prompt for a fresh share link anyway.
		s.redirectCleanPreviewURL(w, r, taskID, http.StatusFound)
		return
	}
	if !s.setPreviewCookie(w, r, taskID, token) {
		http.NotFound(w, r)
		return
	}
	s.redirectCleanPreviewURL(w, r, taskID, http.StatusFound)
}

// redirectCleanPreviewURL writes the hardened 302 to the token-free URL.
// All pvt-redaction guarantees live here: no-store, no-referrer, and a
// Location built from the configured preview origin + validated taskID only
// (never from attacker-controllable Host input). Preserves unrelated query
// parameters (e.g. app deep links) minus the token.
func (s *Server) redirectCleanPreviewURL(w http.ResponseWriter, r *http.Request, taskID string, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	base := previewCookiePath(taskID)
	if u, err := url.Parse(s.previewOrigin); err == nil && s.previewOrigin != "" {
		clean := *r.URL
		clean.Scheme = u.Scheme
		clean.Host = u.Host
		clean.Path = base
		q := clean.Query()
		q.Del(previewTokenQuery)
		clean.RawQuery = q.Encode()
		http.Redirect(w, r, clean.String(), status)
		return
	}
	// Preview origin unconfigured: the gate would have 404'd before exchange;
	// this relative fallback only exists for direct handler invocations.
	clean := *r.URL
	q := clean.Query()
	q.Del(previewTokenQuery)
	clean.RawQuery = q.Encode()
	http.Redirect(w, r, clean.String(), status)
}

// setPreviewCookie mints the path-scoped HttpOnly cookie for a validated
// token. The Secure flag is decided by the CONFIGURED preview origin's
// scheme (round-11 P0 fix) — never by the request's X-Forwarded-Proto, which
// a misconfigured or absent reverse proxy can drop or which an attacker can
// forge on direct connections. Returns false when the taskID is unusable in
// a cookie name or the request scheme contradicts the configured origin
// (fail-closed: a token cookie must never be issued over a channel that does
// not match the deployment's declared TLS posture).
func (s *Server) setPreviewCookie(w http.ResponseWriter, r *http.Request, taskID, token string) bool {
	if taskID == "" || strings.ContainsAny(taskID, " \t\r\n;,()<>@:\\\"[]?={}") {
		return false
	}
	cfg, err := url.Parse(s.previewOrigin)
	if err != nil || s.previewOrigin == "" || (cfg.Scheme != "http" && cfg.Scheme != "https") {
		return false
	}
	// The deployment declares the preview origin's scheme; a request whose
	// resolved scheme disagrees is not on the configured surface.
	if requestScheme(r) != cfg.Scheme {
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     previewTokenCookiePrefix + taskID,
		Value:    token,
		Path:     previewCookiePath(taskID),
		MaxAge:   int(previewTokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   cfg.Scheme == "https",
		SameSite: http.SameSiteLaxMode,
	})
	return true
}

// previewCookiePath scopes the cookie to the task's preview surface.
func previewCookiePath(taskID string) string {
	return fmt.Sprintf("/preview/%s/", taskID)
}

// signPreviewTokenClaims mints a token from explicit claims (test seam for
// legacy-shaped tokens).
func (s *Server) signPreviewTokenClaims(claims previewTokenClaims) string {
	raw, err := json.Marshal(claims)
	if err != nil {
		return ""
	}
	payload := base64Encode(raw)
	return payload + "." + base64Encode(s.previewTokenMAC(payload))
}

// previewShareTokenExchanged reports whether this request is the pvt-bearing
// navigation that must be exchanged to an HttpOnly cookie (§2.0.3): a GET on
// the preview proxy surface carrying the token in the URL query. The exchange
// must happen BEFORE any document is served — a lingering pvt would leak via
// browser history, Referer, and location.search reads by project scripts.
func previewShareTokenExchanged(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	return strings.TrimSpace(r.URL.Query().Get(previewTokenQuery)) != ""
}
