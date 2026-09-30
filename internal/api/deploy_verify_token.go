package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Deploy verify tokens are short-lived HMAC credentials minted at deploy
// trigger time and injected into the GitLab pipeline as the
// MULTIGENT_DEPLOY_TOKEN CI variable. The branch-deploy CI job calls back
// GET /api/v1/deploy-verify with the token before running `compose up`,
// letting the platform fail-closed-refuse any pipeline whose commit was not
// actually approved in the deploy ledger.
//
// Unlike preview tokens there is no nonce: the token is bound to one
// (project, SHA) pair and a short TTL, so replay inside the TTL window is
// harmless (it only re-checks the same approval) and expiry retires it.

const (
	deployTokenHeader = "X-Multigent-Deploy-Token"
	// deployTokenTTL covers runner queue latency: the pipeline may sit
	// queued behind other jobs for a while before the deploy job runs the
	// gate callback. 30 minutes is the reviewed margin.
	deployTokenTTL = 30 * time.Minute
)

type deployTokenClaims struct {
	Project   string `json:"p"`
	SHA       string `json:"s"`
	RequestID string `json:"r"`
	Exp       int64  `json:"exp"`
}

func (s *Server) signDeployVerifyToken(project, sha, requestID string, ttl time.Duration) (string, error) {
	claims := deployTokenClaims{
		Project:   project,
		SHA:       sha,
		RequestID: requestID,
		Exp:       time.Now().Add(ttl).Unix(),
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64Encode(raw)
	return payload + "." + base64Encode(s.deployTokenMAC(payload)), nil
}

func (s *Server) deployTokenMAC(payload string) []byte {
	mac := hmac.New(sha256.New, []byte(s.users.Secret()))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func (s *Server) verifyDeployVerifyToken(token string) (deployTokenClaims, bool) {
	var claims deployTokenClaims
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return claims, false
	}
	if !hmac.Equal([]byte(base64Encode(s.deployTokenMAC(parts[0]))), []byte(parts[1])) {
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

// allowDeployVerify rate-limits the public deploy-verify endpoint per
// (project, sha) key, mirroring the previewChat bucket pattern. The endpoint
// is unauthenticated by design (the CI runner has no console credential), so
// the limiter bounds brute-forcing of token material and approval polling.
const (
	deployVerifyRateLimit = 15
	deployVerifyWindow    = time.Minute
)

type deployVerifyBucket struct {
	start time.Time
	count int
}

func (s *Server) allowDeployVerify(key string) bool {
	s.deployVerifyMu.Lock()
	defer s.deployVerifyMu.Unlock()
	if s.deployVerifySeen == nil {
		s.deployVerifySeen = make(map[string]*deployVerifyBucket)
	}
	now := time.Now()
	bucket, ok := s.deployVerifySeen[key]
	if !ok || now.Sub(bucket.start) >= deployVerifyWindow {
		if len(s.deployVerifySeen) > 1024 {
			for k, v := range s.deployVerifySeen {
				if now.Sub(v.start) >= deployVerifyWindow {
					delete(s.deployVerifySeen, k)
				}
			}
		}
		bucket = &deployVerifyBucket{start: now}
		s.deployVerifySeen[key] = bucket
	}
	bucket.count++
	return bucket.count <= deployVerifyRateLimit
}

// handleDeployVerify is the CI deploy gate callback. It is the single
// deliberate publicMux exemption (see the security invariants in AGENTS.md):
// the endpoint is read-only, self-authenticates via the HMAC token minted at
// trigger time, and is rate-limited. Every rejection path returns the same
// opaque 403 body so the response never distinguishes which check failed.
func (s *Server) handleDeployVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	reject := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "deploy gate rejected"})
	}

	project := strings.TrimSpace(r.URL.Query().Get("project"))
	sha := strings.TrimSpace(r.URL.Query().Get("sha"))
	token := strings.TrimSpace(r.Header.Get(deployTokenHeader))
	if project == "" || sha == "" || token == "" {
		reject()
		return
	}
	if !s.allowDeployVerify(project + "|" + sha) {
		reject()
		return
	}
	claims, ok := s.verifyDeployVerifyToken(token)
	if !ok || claims.Project != project || claims.SHA != sha {
		reject()
		return
	}

	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		reject()
		return
	}
	approved, err := s.controlDB.HasApprovedDeployRequestForSHA(workspaceID, project, sha)
	if err != nil || !approved {
		reject()
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
