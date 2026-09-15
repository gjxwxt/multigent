package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Preview tokens are short-lived HMAC credentials minted when a preview
// session starts. They authorize requests scoped to exactly one task's
// preview surface (the proxied app and its copilot endpoints) without
// requiring the console Bearer token inside the preview iframe, which
// shares its origin with the previewed application.

const (
	previewTokenHeader = "X-Multigent-Preview-Token"
	previewTokenQuery  = "pvt"
	previewTokenTTL    = 12 * time.Hour
)

type previewTokenClaims struct {
	TaskID  string `json:"t"`
	Project string `json:"p,omitempty"`
	Exp     int64  `json:"exp"`
	// Cap carries the token's capability set (space-separated). Current
	// capability: "preview.view" (read-only browsing of the preview surface).
	// Legacy tokens minted before this field existed carry no Cap and are
	// treated as view-only (backward-compatible fail-closed): old share links
	// keep working read-only without a re-mint, and no legacy token can write.
	Cap string `json:"cap,omitempty"`
}

// previewTokenHasCapability reports whether the token carries the capability.
// A token without any Cap field is legacy and gets view-only treatment —
// never write capability (fail-closed for old tokens).
func previewTokenHasCapability(claims previewTokenClaims, capability string) bool {
	for _, c := range strings.Fields(claims.Cap) {
		if c == capability {
			return true
		}
	}
	return false
}

func (s *Server) signPreviewToken(taskID, project string) string {
	return s.signPreviewTokenWithTTL(taskID, project, previewTokenTTL)
}

func (s *Server) signPreviewTokenWithTTL(taskID, project string, ttl time.Duration) string {
	claims := previewTokenClaims{
		TaskID:  taskID,
		Project: project,
		Exp:     time.Now().Add(ttl).Unix(),
		Cap:     previewCapabilityView,
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return ""
	}
	payload := base64Encode(raw)
	return payload + "." + base64Encode(s.previewTokenMAC(payload))
}

func (s *Server) previewTokenMAC(payload string) []byte {
	mac := hmac.New(sha256.New, []byte(s.users.Secret()))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func (s *Server) verifyPreviewToken(token, taskID string) (previewTokenClaims, bool) {
	claims, ok := s.verifyPreviewTokenAny(token)
	if !ok || claims.TaskID != taskID {
		return previewTokenClaims{}, false
	}
	return claims, true
}

// verifyPreviewTokenAny verifies a token's MAC and expiry without knowing the
// bound task up front — used by the root-shape design proxy, where the task
// is derived FROM the credential (cookie name or token claims) instead of the
// URL path.
func (s *Server) verifyPreviewTokenAny(token string) (previewTokenClaims, bool) {
	var claims previewTokenClaims
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return claims, false
	}
	if !hmac.Equal([]byte(base64Encode(s.previewTokenMAC(parts[0]))), []byte(parts[1])) {
		return claims, false
	}
	raw, err := base64Decode(parts[0])
	if err != nil || json.Unmarshal(raw, &claims) != nil {
		return claims, false
	}
	if time.Now().Unix() > claims.Exp {
		return claims, false
	}
	return claims, true
}

// previewRequestToken extracts a preview token from the request: explicit
// header, pvt query parameter, or the path-scoped cookie set when the
// preview document was first loaded (sub-resource requests carry the
// cookie automatically without propagating the query parameter).
func previewRequestToken(r *http.Request, taskID string) string {
	if tok := strings.TrimSpace(r.Header.Get(previewTokenHeader)); tok != "" {
		return tok
	}
	if tok := strings.TrimSpace(r.URL.Query().Get(previewTokenQuery)); tok != "" {
		return tok
	}
	if cookie, err := r.Cookie("mg_pvt_" + taskID); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

// previewRequestAuthorized admits a request to a task's preview surface if
// it carries a valid preview token bound to that task, or if it
// authenticates as a user with access to the owning project. It mirrors
// withTokenAuth's context setup so downstream currentUser lookups work for
// console traffic that arrives without going through withTokenAuth.
func (s *Server) previewRequestAuthorized(w http.ResponseWriter, r *http.Request, project, taskID string) bool {
	if tok := previewRequestToken(r, taskID); tok != "" {
		if _, ok := s.verifyPreviewToken(tok, taskID); ok {
			return true
		}
	}
	// Requests that already passed withTokenAuth (or tests that seed the
	// identity context directly) carry the username in context.
	if username, _ := r.Context().Value(ctxUserKey).(string); username != "" {
		return s.authorizePreviewProject(w, r, project)
	}
	identity, ok := s.authenticateRequest(r)
	if !ok || identity.Err != nil {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "preview token required")
		return false
	}
	ctx := context.WithValue(r.Context(), ctxUserKey, identity.Username)
	ctx = context.WithValue(ctx, ctxAuthSourceKey, identity.Source)
	if len(identity.Scopes) > 0 {
		ctx = context.WithValue(ctx, ctxAuthScopesKey, identity.Scopes)
	}
	if identity.Entitlements != nil {
		ctx = context.WithValue(ctx, ctxEntitlementsKey, *identity.Entitlements)
	}
	return s.authorizePreviewProject(w, r.WithContext(ctx), project)
}

func (s *Server) authorizePreviewProject(w http.ResponseWriter, r *http.Request, project string) bool {
	project = strings.TrimSpace(project)
	if project == "" || project == "current" {
		// Task-scoped endpoints whose owning project cannot be resolved
		// from the path; authentication alone is sufficient.
		return true
	}
	return s.checkProjectAccess(w, r, project)
}

// previewPrincipal is the authenticated actor on a preview control endpoint.
// Preview share tokens never produce one — they are view-only (Task 1.1).
type previewPrincipal struct {
	Username string
}

// previewWritePrincipal authenticates a WRITE-surface preview control request
// and returns the request carrying the authenticated context, the principal,
// and whether both operator authorization and (when applicable) the
// human-review assignee check passed. Hard rules (Task 1.1, rounds 6-8):
//
//   - Bearer-only: a preview share token never satisfies this gate — no
//     dual-principal transition. A request authenticated solely by pvt gets
//     403 (not 401): the token itself was valid, the capability is missing.
//   - The authenticated context is bound into the RETURNED request; callers
//     must use it for authorization decisions, audit records, and comment
//     authorship — never a "user" fallback.
//   - Authorization is operator-level (checkProjectOperator), not the read-
//     level checkProjectAccess.
//   - Fail-closed: an unresolvable project, empty username, or a workflow
//     query error during the approver check all deny.
func (s *Server) previewWritePrincipal(w http.ResponseWriter, r *http.Request, project, taskID string) (*http.Request, previewPrincipal, bool) {
	authReq, principal, ok := s.authenticatedPreviewPrincipal(w, r, taskID)
	if !ok {
		return nil, previewPrincipal{}, false
	}
	if !s.previewWriteAuthorize(w, authReq, principal, project, taskID) {
		return nil, previewPrincipal{}, false
	}
	return authReq, principal, true
}

// authenticatedPreviewPrincipal resolves a REAL user identity for the request
// (context-injected or freshly authenticated from the Bearer token) and
// rejects share-token-only requests with 403.
func (s *Server) authenticatedPreviewPrincipal(w http.ResponseWriter, r *http.Request, taskID string) (*http.Request, previewPrincipal, bool) {
	username, _ := r.Context().Value(ctxUserKey).(string)
	if username == "" {
		// Detect share-token-only requests: a valid preview token for this
		// task with no user identity means a share token reached a write
		// surface — 403 capability denied (the token itself was valid).
		if tok := previewRequestToken(r, taskID); tok != "" {
			if _, ok := s.verifyPreviewTokenAny(tok); ok {
				s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden,
					"preview share tokens are read-only; sign in to use this surface")
				return nil, previewPrincipal{}, false
			}
		}
		identity, ok := s.authenticateRequest(r)
		if !ok || identity.Err != nil {
			s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")
			return nil, previewPrincipal{}, false
		}
		if identity.Source == identitySourceClientToken || identity.Source == identitySourceStaticAPIKey {
			s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "principal type not permitted on preview write surfaces")
			return nil, previewPrincipal{}, false
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, identity.Username)
		ctx = context.WithValue(ctx, ctxAuthSourceKey, identity.Source)
		if len(identity.Scopes) > 0 {
			ctx = context.WithValue(ctx, ctxAuthScopesKey, identity.Scopes)
		}
		if identity.Entitlements != nil {
			ctx = context.WithValue(ctx, ctxEntitlementsKey, *identity.Entitlements)
		}
		r = r.WithContext(ctx)
		username = identity.Username
	}
	username = strings.TrimSpace(username)
	if username == "" || username == "apikey" {
		// A machine credential or an empty name is not a human principal on
		// preview write surfaces (fail-closed).
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "authenticated user required")
		return nil, previewPrincipal{}, false
	}
	return r, previewPrincipal{Username: username}, true
}

// previewLoginPrincipal authenticates a control-surface request that must
// belong to a real user but does not require operator rights (preview/status
// pending its leakage audit). Share-token-only requests get 401 here — the
// endpoint is login-required, not capability-denied.
func (s *Server) previewLoginPrincipal(w http.ResponseWriter, r *http.Request, taskID string) (*http.Request, previewPrincipal, bool) {
	username, _ := r.Context().Value(ctxUserKey).(string)
	if username == "" {
		identity, ok := s.authenticateRequest(r)
		if !ok || identity.Err != nil {
			s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")
			return nil, previewPrincipal{}, false
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, identity.Username)
		ctx = context.WithValue(ctx, ctxAuthSourceKey, identity.Source)
		r = r.WithContext(ctx)
		username = identity.Username
	}
	username = strings.TrimSpace(username)
	if username == "" || username == "apikey" {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authenticated user required")
		return nil, previewPrincipal{}, false
	}
	return r, previewPrincipal{Username: username}, true
}

// previewWriteAuthorize applies operator-level authorization plus the
// human-review assignee match, failing closed on query errors.
func (s *Server) previewWriteAuthorize(w http.ResponseWriter, r *http.Request, principal previewPrincipal, project, taskID string) bool {
	project = strings.TrimSpace(project)
	if project == "" || project == "current" {
		// Legacy unresolvable-project shape: require operator against the
		// current workspace scope; unknown projects deny inside the check.
		return s.checkProjectOperator(w, r, "current")
	}
	if !s.checkProjectOperator(w, r, project) {
		return false
	}
	return s.previewApproverCheck(w, r, principal, project, taskID)
}

// previewApproverCheck enforces: when the task's active workflow run sits at
// a human_review step assigned to a human, the acting principal must BE that
// assignee (same bar as the review decision endpoint). Agent assignees or an
// unassigned step fall back to the operator check already performed. Any
// workflow query failure denies (fail-closed three-state).
func (s *Server) previewApproverCheck(w http.ResponseWriter, r *http.Request, principal previewPrincipal, project, taskID string) bool {
	if s == nil || s.controlDB == nil {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "workflow state unavailable; write denied")
		return false
	}
	wfStore := workflowstore.NewStore(s.controlDB, s.currentWorkspaceIDValue(r))
	run, runFound, err := wfStore.RunForTask(project, taskID)
	if err != nil {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "workflow state unavailable; write denied")
		return false
	}
	if !runFound || run.Status != "active" || strings.TrimSpace(run.ActiveStepID) == "" {
		// No active run / not parked on a step: operator check is the gate.
		return true
	}
	def, defFound, err := wfStore.RunDefinition(run)
	if err != nil || !defFound {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "workflow state unavailable; write denied")
		return false
	}
	for _, step := range def.Steps {
		if step.ID != run.ActiveStepID {
			continue
		}
		if step.Type != "human_review" {
			return true
		}
		if run.CurrentAssigneeType != "user" || strings.TrimSpace(run.CurrentAssigneeID) == "" {
			// Agent or unassigned: the operator check is the gate.
			return true
		}
		if principal.Username != strings.TrimSpace(run.CurrentAssigneeID) {
			s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden,
				"only the assigned reviewer may act on this review step")
			return false
		}
		return true
	}
	// Active step not found in the definition snapshot: fail closed.
	s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "workflow state unavailable; write denied")
	return false
}

// currentWorkspaceIDValue is the error-tolerant workspace id lookup used by
// fail-closed preview checks: on error it returns "" (which makes downstream
// workflow lookups miss → deny), never panics.
func (s *Server) currentWorkspaceIDValue(r *http.Request) string {
	if id, err := s.currentWorkspaceID(); err == nil {
		return id
	}
	return ""
}

// previewChatRateLimit caps how many copilot chat messages one task may
// dispatch per window, bounding the driveable agent wakeup surface even with
// a stolen-but-valid preview token.
const (
	previewChatRateLimit = 15
	previewChatWindow    = time.Minute
)

type previewChatBucket struct {
	start time.Time
	count int
}

func (s *Server) allowPreviewChat(taskID string) bool {
	s.previewChatMu.Lock()
	defer s.previewChatMu.Unlock()
	if s.previewChatSeen == nil {
		s.previewChatSeen = make(map[string]*previewChatBucket)
	}
	now := time.Now()
	bucket, ok := s.previewChatSeen[taskID]
	if !ok || now.Sub(bucket.start) >= previewChatWindow {
		if len(s.previewChatSeen) > 1024 {
			for k, v := range s.previewChatSeen {
				if now.Sub(v.start) >= previewChatWindow {
					delete(s.previewChatSeen, k)
				}
			}
		}
		bucket = &previewChatBucket{start: now}
		s.previewChatSeen[taskID] = bucket
	}
	bucket.count++
	return bucket.count <= previewChatRateLimit
}
