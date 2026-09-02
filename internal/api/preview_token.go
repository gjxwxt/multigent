package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"
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
}

func (s *Server) signPreviewToken(taskID, project string) string {
	return s.signPreviewTokenWithTTL(taskID, project, previewTokenTTL)
}

func (s *Server) signPreviewTokenWithTTL(taskID, project string, ttl time.Duration) string {
	claims := previewTokenClaims{TaskID: taskID, Project: project, Exp: time.Now().Add(ttl).Unix()}
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

// previewChatRateLimit caps how many copilot chat messages one task may
// dispatch per window, bounding the unauthenticated-driveable agent wakeup
// surface even with a stolen-but-valid preview token.
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
