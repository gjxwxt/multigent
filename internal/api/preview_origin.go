package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/multigent/multigent/internal/secretbox"
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
	// PreviewCopilotDrawerEnv is the deployment config key to enable the preview
	// copilot drawer feature flag (controlled rollout, default disabled).
	PreviewCopilotDrawerEnv = "MULTIGENT_ENABLE_PREVIEW_COPILOT_DRAWER"
	// PreviewTurnReceiptsEnv is the deployment config key to enable turn receipts
	// and isolated transactional modification (controlled rollout, default disabled).
	PreviewTurnReceiptsEnv = "MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS"
	// PreviewTurnReceiptsProjectsEnv is the deployment config key for the comma-separated
	// list of exact project names permitted to use turn receipts (gray rollout).
	// Empty, whitespace, or "*" matches nothing (fail-closed).
	PreviewTurnReceiptsProjectsEnv = "MULTIGENT_PREVIEW_TURN_RECEIPTS_PROJECTS"
)

// SetPreviewCopilotDrawerEnabled enables or disables the preview copilot drawer.
func (s *Server) SetPreviewCopilotDrawerEnabled(enabled bool) {
	s.enablePreviewCopilotDrawer = enabled
}

// PreviewCopilotDrawerEnabled returns whether the preview drawer is enabled.
// Fail-closed rule (Phase 0): If MULTIGENT_ENABLE_PREVIEW_COPILOT_DRAWER is true,
// both MULTIGENT_PREVIEW_ORIGIN and MULTIGENT_CONSOLE_ORIGIN must be configured,
// non-empty, and share the exact same schemeful site. Otherwise, it is disabled.
func (s *Server) PreviewCopilotDrawerEnabled() bool {
	if s == nil || !s.enablePreviewCopilotDrawer {
		return false
	}
	if s.previewOrigin == "" || s.consoleOrigin == "" {
		return false
	}
	if SchemefulSiteMismatchWarning(s.consoleOrigin, s.previewOrigin) != "" {
		return false
	}
	return true
}

// PreviewCopilotDrawerDisabledReason returns the reason why drawer is disabled.
func (s *Server) PreviewCopilotDrawerDisabledReason() string {
	if s == nil || !s.enablePreviewCopilotDrawer {
		return "drawer feature flag is disabled"
	}
	if s.previewOrigin == "" {
		return "MULTIGENT_PREVIEW_ORIGIN is not configured"
	}
	if s.consoleOrigin == "" {
		return "MULTIGENT_CONSOLE_ORIGIN is not configured"
	}
	if warn := SchemefulSiteMismatchWarning(s.consoleOrigin, s.previewOrigin); warn != "" {
		return warn
	}
	return ""
}

// SetPreviewTurnReceiptsEnabled enables or disables turn receipts and transactional isolation.
func (s *Server) SetPreviewTurnReceiptsEnabled(enabled bool) {
	s.enablePreviewTurnReceipts = enabled
}

// SetPreviewTurnReceiptsProjects sets the comma-separated project allowlist for turn receipts.
func (s *Server) SetPreviewTurnReceiptsProjects(projects string) {
	s.previewTurnReceiptsProjects = projects
}

// PreviewTurnReceiptsProjects returns the configured project allowlist.
func (s *Server) PreviewTurnReceiptsProjects() string {
	if s == nil {
		return ""
	}
	return s.previewTurnReceiptsProjects
}

// PreviewTurnReceiptsEnabled returns whether preview turn receipts are enabled globally.
// Fail-closed rule (Phase 1):
// 1. MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS must be enabled.
// 2. PreviewCopilotDrawerEnabled() must be true (valid origins, same schemeful site).
// 3. Strict encryption self-test must pass (secretbox.StrictEncryptionSelfTest() == nil).
func (s *Server) PreviewTurnReceiptsEnabled() bool {
	if s == nil || !s.enablePreviewTurnReceipts {
		return false
	}
	if !s.PreviewCopilotDrawerEnabled() {
		return false
	}
	if err := secretbox.StrictEncryptionSelfTest(); err != nil {
		return false
	}
	return true
}

// PreviewTurnReceiptsDisabledReason returns the reason why turn receipts are disabled globally.
func (s *Server) PreviewTurnReceiptsDisabledReason() string {
	if s == nil || !s.enablePreviewTurnReceipts {
		return "turn receipts feature flag is disabled"
	}
	if !s.PreviewCopilotDrawerEnabled() {
		return s.PreviewCopilotDrawerDisabledReason()
	}
	if err := secretbox.StrictEncryptionSelfTest(); err != nil {
		return fmt.Sprintf("strict encryption unavailable: %v", err)
	}
	return ""
}

// PreviewTurnReceiptsEnabledForProject returns whether turn receipts are enabled for a specific project.
// Fail-closed rule:
// 1. Global PreviewTurnReceiptsEnabled() must be true.
// 2. The project must be explicitly in MULTIGENT_PREVIEW_TURN_RECEIPTS_PROJECTS.
// Empty string, whitespace, or "*" matches nothing (fail-closed).
func (s *Server) PreviewTurnReceiptsEnabledForProject(project string) bool {
	if s == nil || !s.PreviewTurnReceiptsEnabled() {
		return false
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return false
	}
	allowlistRaw := strings.TrimSpace(s.previewTurnReceiptsProjects)
	if allowlistRaw == "" {
		return false
	}
	for _, p := range strings.Split(allowlistRaw, ",") {
		p = strings.TrimSpace(p)
		if p != "" && p != "*" && p == project {
			return true
		}
	}
	return false
}

// PreviewTurnReceiptsDisabledReasonForProject returns the reason why turn receipts are disabled for this project.
func (s *Server) PreviewTurnReceiptsDisabledReasonForProject(project string) string {
	if s == nil || !s.enablePreviewTurnReceipts {
		return "turn receipts feature flag is disabled"
	}
	if !s.PreviewCopilotDrawerEnabled() {
		return s.PreviewCopilotDrawerDisabledReason()
	}
	if err := secretbox.StrictEncryptionSelfTest(); err != nil {
		return fmt.Sprintf("strict encryption unavailable: %v", err)
	}
	if !s.PreviewTurnReceiptsEnabledForProject(project) {
		return fmt.Sprintf("project %q is not in preview turn receipts allowlist (%s)", project, PreviewTurnReceiptsProjectsEnv)
	}
	return ""
}

// IsTruthyEnv returns true if val is "1", "true", "yes", or "on" (case-insensitive).
func IsTruthyEnv(val string) bool {
	val = strings.ToLower(strings.TrimSpace(val))
	return val == "1" || val == "true" || val == "yes" || val == "on"
}

// previewCapabilityView is the only capability preview share tokens carry
// (Task 1.1). Tokens without a Cap field (pre-Cap legacy share links) carry
// NO capability and are rejected on all surfaces — breaking migration, old
// links must be re-minted (round-13).
const previewCapabilityView = "preview.view"

const previewTokenCookiePrefix = "mg_pvt_"

// SetPreviewOrigin configures the explicit preview origin (deployment config).
// Call before Handler() is served. An empty or invalid value disables preview
// surfaces entirely (fail-closed) — there is no same-origin fallback.
func (s *Server) SetPreviewOrigin(origin string) {
	s.previewOrigin = normalizeConfiguredOrigin(origin, PreviewOriginEnv,
		"preview surfaces stay disabled (fail-closed)")
	s.logSchemefulSiteCheck()
}

// SetConsoleOrigin configures the console public origin for the CORS
// allowlist (deployment config). Empty restores the wildcard legacy behavior.
func (s *Server) SetConsoleOrigin(origin string) {
	s.consoleOrigin = normalizeConfiguredOrigin(origin, ConsoleOriginEnv,
		"CORS allowlist falls back to the wildcard legacy behavior")
	s.logSchemefulSiteCheck()
}

func (s *Server) logSchemefulSiteCheck() {
	if warn := SchemefulSiteMismatchWarning(s.consoleOrigin, s.previewOrigin); warn != "" {
		log.Printf("[preview-origin] WARNING: %s", warn)
	}
}

// SchemefulSiteMismatchWarning checks whether the configured consoleOrigin and
// previewOrigin share the same schemeful site (scheme + registrable domain).
// Returns a non-empty warning message if they differ or are incompatible.
func SchemefulSiteMismatchWarning(consoleOrigin, previewOrigin string) string {
	consoleOrigin = strings.TrimSpace(consoleOrigin)
	previewOrigin = strings.TrimSpace(previewOrigin)
	if consoleOrigin == "" || previewOrigin == "" {
		return ""
	}
	uConsole, err1 := url.Parse(consoleOrigin)
	uPreview, err2 := url.Parse(previewOrigin)
	if err1 != nil || err2 != nil || uConsole.Host == "" || uPreview.Host == "" {
		return ""
	}
	if uConsole.Scheme != uPreview.Scheme {
		return fmt.Sprintf("console scheme (%s) and preview scheme (%s) differ; cross-origin preview cookies with SameSite=Lax will be blocked in iframes (preview drawer requires the same schemeful site)", uConsole.Scheme, uPreview.Scheme)
	}
	siteConsole := registrableSite(uConsole.Hostname())
	sitePreview := registrableSite(uPreview.Hostname())
	if siteConsole != sitePreview {
		return fmt.Sprintf("console host (%s) and preview host (%s) do not share the same registrable domain (%s vs %s); cross-origin preview cookies with SameSite=Lax will be blocked in iframes (preview drawer requires the same schemeful site)", uConsole.Hostname(), uPreview.Hostname(), siteConsole, sitePreview)
	}
	return ""
}

func registrableSite(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err == nil {
		return etld1
	}
	return host
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
// configured preview origin (full scheme + host comparison). The configured
// origin is the only input to the decision — the request Host is compared,
// never consulted as configuration. This is the gate condition for ALL
// /preview/* traffic (round-13 P0): header tokens and hand-made cookies
// reach the proxy without passing through setPreviewCookie, so scheme
// fidelity must be enforced here, not only at the cookie boundary.
func (s *Server) requestOnPreviewOrigin(r *http.Request) bool {
	if s.previewOriginDisabled() {
		return false
	}
	u := &url.URL{Scheme: requestScheme(r), Host: r.Host}
	return u.String() == s.previewOrigin
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
//   - Full scheme+host match -> pass. A dropped or forged X-Forwarded-Proto
//     resolves to a different scheme and the whole preview is rejected
//     (round-13 P0: header tokens / existing cookies would otherwise reach
//     the proxy over the wrong scheme without ever touching the cookie
//     boundary).
//   - Request on the configured console origin -> 302 to the equivalent
//     preview-origin URL so pre-split console links keep working.
//   - Any other Host -> 404; the response never echoes request input.
//
// Deployment contract: the reverse proxy MUST overwrite any client-supplied
// X-Forwarded-Proto, and the backend must not be directly reachable.
func (s *Server) enforcePreviewOriginGate(w http.ResponseWriter, r *http.Request) bool {
	if s.previewOriginDisabled() {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound,
			"preview origin not configured; set "+PreviewOriginEnv+" to enable preview sharing")
		return true
	}
	if s.requestOnPreviewOrigin(r) {
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

func isValidPreviewTaskID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func extractPreviewTaskIDFromReferer(ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || !strings.HasPrefix(u.Path, "/preview/") {
		return ""
	}
	return extractPreviewTaskID(u.Path)
}

func extractPreviewPrefixFromReferer(ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || !strings.HasPrefix(u.Path, "/preview/") {
		return ""
	}
	trimmed := strings.TrimPrefix(u.Path, "/preview/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 3 && parts[1] == "turn" && isValidPreviewTaskID(parts[0]) && isValidPreviewTaskID(parts[2]) {
		return fmt.Sprintf("/preview/%s/turn/%s/", parts[0], parts[2])
	}
	if len(parts) >= 1 && isValidPreviewTaskID(parts[0]) {
		return fmt.Sprintf("/preview/%s/", parts[0])
	}
	return ""
}

// IsPreviewSessionRequest reports whether a request belongs to the preview surface
// (arrived on the preview origin, carries a Referer from a /preview/ URL, or
// carries a preview token cookie).
func (s *Server) IsPreviewSessionRequest(r *http.Request) bool {
	if s == nil {
		return false
	}
	if s.requestOnPreviewOrigin(r) {
		return true
	}
	if extractPreviewTaskIDFromReferer(r.Header.Get("Referer")) != "" {
		return true
	}
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, previewTokenCookiePrefix) {
			return true
		}
	}
	return false
}

// handlePreviewOriginRootFallback intercepts loose root-relative requests (e.g.
// /src/main.tsx, /@vite/client, /node_modules/...) made by preview pages or
// module imports. Because browsers resolve root-relative imports against the
// origin root rather than the document <base>, these requests lack the /preview/{task}/
// prefix and would otherwise hit console SPA fallback or 401.
// By checking the Referer header (or preview cookies on the dedicated preview origin),
// we 307-redirect them into their proper /preview/{task}/ subpath where the preview
// cookie is sent and the proxy serves the assets.
func (s *Server) handlePreviewOriginRootFallback(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/preview/") || strings.HasPrefix(path, "/projects/") || strings.HasPrefix(path, "/_multigent_") {
		return false
	}

	prefix := extractPreviewPrefixFromReferer(r.Header.Get("Referer"))
	taskID := extractPreviewTaskIDFromReferer(r.Header.Get("Referer"))
	if taskID == "" && s.requestOnPreviewOrigin(r) {
		// On dedicated preview origin, if Referer was omitted or stripped, check for preview cookies
		for _, c := range r.Cookies() {
			if strings.HasPrefix(c.Name, previewTokenCookiePrefix) {
				taskID = strings.TrimPrefix(c.Name, previewTokenCookiePrefix)
				prefix = fmt.Sprintf("/preview/%s/", taskID)
				break
			}
		}
	}

	if taskID == "" || !isValidPreviewTaskID(taskID) {
		return false
	}

	if s.previewEngine != nil {
		instanceKey := taskID
		if strings.Contains(prefix, "/turn/") {
			parts := strings.Split(strings.Trim(strings.TrimPrefix(prefix, "/preview/"), "/"), "/")
			if len(parts) >= 3 && parts[1] == "turn" {
				instanceKey = fmt.Sprintf("turn:%s:%s", parts[0], parts[2])
			}
		}
		if _, ok := s.previewEngine.GetInstance(instanceKey); !ok {
			if _, ok := s.previewEngine.GetInstance(taskID); !ok {
				return false
			}
			prefix = fmt.Sprintf("/preview/%s/", taskID)
		}
	}

	if prefix == "" {
		prefix = fmt.Sprintf("/preview/%s/", taskID)
	}

	target := fmt.Sprintf("%s%s", prefix, strings.TrimPrefix(r.URL.RequestURI(), "/"))
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	return true
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
