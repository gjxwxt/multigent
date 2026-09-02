package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Design gate endpoints (OpenDesign integration, docs/opendesign-integration-plan.md).
// All routes live on the authenticated main mux (never publicMux) and start
// with checkProjectAccess via the shared project/task guard. The OD API token
// never leaves the server: it is decrypted from connection_secrets per call
// and injected into the upstream Authorization header or byokProvider body.

const (
	designTokenQuery    = "odt"
	designTokenCookieP  = "mg_od_"
	designTokenTTL      = 4 * time.Hour
	designStaticTimeout = 15 * time.Second
)

// Proxy whitelist: OD SPA surface only. Admin/auth surfaces of the OD
// daemon stay unreachable through the proxy.
var (
	designAllowedPrefixes = []string{"/projects/", "/api/projects/", "/api/runs", "/api/health", "/assets/", "/_next/", "/design-systems/", "/api/design-templates", "/api/templates", "/api/app-config", "/api/version", "/api/active"}
	designDeniedPrefixes  = []string{"/api/auth/", "/admin/", "/settings/", "/api/connectors", "/api/integrations"}
)

// designTaskGuard resolves the project + task and enforces access.
func (s *Server) designTaskGuard(w http.ResponseWriter, r *http.Request, project, taskID string) *entity.Task {
	return s.projectTaskResourceGuard(w, r, project, taskID)
}

// signDesignToken mints a design-proxy token bound to one task (same HMAC
// scheme as preview tokens, distinct value domain via the claims shape).
func (s *Server) signDesignToken(taskID, project string) string {
	return s.signPreviewTokenWithTTL(taskID, project, designTokenTTL)
}

// designRequestAuthorized admits console traffic (cookie/Bearer via main mux)
// or a task-scoped design token (iframe bootstrap ?odt=).
func (s *Server) designRequestAuthorized(w http.ResponseWriter, r *http.Request, project, taskID string) bool {
	if tok := strings.TrimSpace(r.URL.Query().Get(designTokenQuery)); tok != "" {
		if _, ok := s.verifyPreviewToken(tok, taskID); ok {
			return true
		}
	}
	if cookie, err := r.Cookie(designTokenCookieP + taskID); err == nil {
		if tok := strings.TrimSpace(cookie.Value); tok != "" {
			if _, ok := s.verifyPreviewToken(tok, taskID); ok {
				return true
			}
		}
	}
	return s.previewRequestAuthorized(w, r, project, taskID)
}

// ---- GET .../design/projects ----

func (s *Server) handleDesignListProjects(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if s.designTaskGuard(w, r, project, taskID) == nil {
		return
	}
	projects, err := s.defaultODClient().ListProjects(r.Context())
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects})
}

// ---- POST .../design/start ----

type designStartRequest struct {
	DesignSystemID string `json:"designSystemId"`
	Regenerate     bool   `json:"regenerate"`
	Model          string `json:"model"`
}

type designStartResponse struct {
	ProjectID      string `json:"projectId"`
	ProxyURL       string `json:"proxyUrl"`
	LaunchURL      string `json:"launchUrl"`
	StudioURL      string `json:"studioUrl"`
	Regenerated    bool   `json:"regenerated"`
	ConversationID string `json:"conversationId,omitempty"`
}

var designLocks sync.Map // taskID -> *sync.Mutex

func (s *Server) handleDesignStart(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	agent := taskAgentFallback(project)
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	// A task parked at a human_review workflow step still reports Status
	// in_progress (pitfall 7); accept awaiting_confirmation or an active run
	// stopped at any human_review step, fail-closed otherwise.
	if task.Status != entity.TaskStatusAwaitingConfirmation {
		workspaceID, _ := s.currentWorkspaceID()
		if !s.isTaskAtHumanReviewStep(workspaceID, project, taskID) {
			s.jsonError(w, http.StatusConflict, "task is not awaiting confirmation at the design gate")
			return
		}
	}
	var body designStartRequest
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid request body")
		return
	}

	mu, _ := designLocks.LoadOrStore(taskID, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	if !lock.TryLock() {
		s.jsonError(w, http.StatusServiceUnavailable, "design start already in progress")
		return
	}
	defer lock.Unlock()

	// Re-read the task under the lock (double-check idempotency).
	freshProject, freshAgent, fresh, err := s.ts.FindTaskByID(taskID)
	if err != nil || fresh == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	task = fresh
	if strings.TrimSpace(freshProject) != "" {
		project = freshProject
	}
	agent = freshAgent
	client := s.defaultODClient()

	if task.DesignProjectID != "" && !body.Regenerate {
		s.writeDesignStartResponse(w, project, taskID, task.DesignProjectID, false, "")
		return
	}

	// Regenerate: delete the old OD project first so orphans don't accumulate.
	if task.DesignProjectID != "" && body.Regenerate {
		if err := client.DeleteProject(r.Context(), task.DesignProjectID); err != nil {
			// Not fatal: continue creating the replacement, but leave a trail.
			s.addComment(task, project, agent, "design regenerate: old OD project delete failed: "+err.Error())
		}
	}

	projID := designProjectIDForTask(taskID)
	name := "task_" + taskID
	if err := client.CreateProject(r.Context(), projID, name, body.DesignSystemID, designPendingPrompt(task)); err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	// The design agent runs against the task agent's model account (e.g.
	// ccr-qwen), not the OD connection: byokProvider is the model endpoint OD
	// forwards to opencode. Sending the OD daemon's own URL made opencode
	// POST to OD itself (live incident 2026-09-02).
	creds, err := s.resolveDesignModelProvider(project, agent, designModelOrDefault(body.Model))
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	conversationID, err := client.StartRun(r.Context(), projID, designPendingPrompt(task), body.DesignSystemID, body.Model, creds)
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}

	task.DesignProjectID = projID
	task.DesignSource = "generated"
	task.DesignSystemID = body.DesignSystemID
	if task.DesignSystemID == "" {
		task.DesignSystemID = "ant"
	}
	if agent == "" {
		agent = taskAgentFromAssignee(task)
	}
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		s.jsonError(w, http.StatusInternalServerError, "persist design reference failed")
		return
	}
	s.recordDesignAudit(r, workspaceIDOfProject(project), "design.start", project, taskID, map[string]any{
		"projectId": projID, "regenerated": task.DesignProjectID != "" && body.Regenerate,
	})
	s.writeDesignStartResponse(w, project, taskID, projID, body.Regenerate, conversationID)
}

func (s *Server) writeDesignStartResponse(w http.ResponseWriter, project, taskID, projID string, regenerated bool, conversationID string) {
	base := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design", url.PathEscape(project), url.PathEscape(taskID))
	odt := s.signDesignToken(taskID, project)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(designStartResponse{
		ProjectID:      projID,
		ProxyURL:       base + "/proxy/projects/" + projID + "?" + designTokenQuery + "=" + odt,
		LaunchURL:      base + "/launch?" + designTokenQuery + "=" + odt,
		StudioURL:      "/projects/" + projID,
		Regenerated:    regenerated,
		ConversationID: conversationID,
	})
}

// ---- GET .../design/status ----

func (s *Server) handleDesignStatus(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	projID := task.DesignProjectID
	if projID == "" {
		// The "existing design" confirm path never persists DesignProjectID on
		// the task — the frozen outputs are the only record. Fall back to the
		// approved_design_project_id the gate wrote so the follow panel can
		// still mint a fresh signed link to that design.
		projID = s.approvedDesignProjectIDFromRun(project, taskID)
	}
	if projID == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"projectId": "", "runStatus": "none"})
		return
	}
	_, status, err := s.defaultODClient().LatestRunStatus(r.Context(), projID)
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	// A fresh signed proxy URL lets the client (re)open the studio iframe or a
	// read-only preview without another start call when the previous otd expired.
	base := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design", url.PathEscape(project), url.PathEscape(taskID))
	odt := s.signDesignToken(taskID, project)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"projectId": projID,
		"runStatus": status,
		"proxyUrl":  base + "/proxy/projects/" + projID + "?" + designTokenQuery + "=" + odt,
		"launchUrl": base + "/launch?" + designTokenQuery + "=" + odt,
	})
}

// approvedDesignProjectIDFromRun reads the approved_design_project_id output
// from the task's workflow run design_review step, if any. Read-only fallback;
// misses just leave the status response without a project.
func (s *Server) approvedDesignProjectIDFromRun(project, taskID string) string {
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return ""
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil || !found {
		return ""
	}
	insts, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return ""
	}
	// Scan newest-first; the first completed step that carries a non-empty
	// approved_design_project_id wins.
	for i := len(insts) - 1; i >= 0; i-- {
		inst := insts[i]
		if inst.Status != "completed" {
			continue
		}
		if v := strings.TrimSpace(inst.OutputValues["approved_design_project_id"]); v != "" {
			return v
		}
	}
	return ""
}

// ---- POST .../design/chat (multi-turn modification via platform, A.5-3) ----

func (s *Server) handleDesignChat(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	if task.DesignProjectID == "" {
		s.jsonError(w, http.StatusConflict, "no design project for this task; start one first")
		return
	}
	if !s.allowDesignRequest(taskID) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "too many design messages, slow down")
		return
	}
	var body struct {
		ConversationID string `json:"conversationId"`
		Message        string `json:"message"`
		Model          string `json:"model"`
	}
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		s.jsonError(w, http.StatusBadRequest, "message required")
		return
	}
	// The design agent runs against the task agent's model account (e.g.
	// ccr-qwen); the OD connection itself is only used for the API call the
	// proxy pass performs below.
	agent := taskAgentFromAssignee(task)
	creds, err := s.resolveDesignModelProvider(project, agent, designModelOrDefault(body.Model))
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	payload := map[string]any{
		"projectId":   task.DesignProjectID,
		"agentId":     odAgentID,
		"message":     body.Message,
		"sessionMode": "design",
		"model":       designModelOrDefault(body.Model),
		"byokProvider": odByokProvider{
			Protocol: creds.Protocol,
			APIKey:   creds.APIKey,
			BaseURL:  creds.BaseURL,
			Model:    designModelOrDefault(body.Model),
		},
	}
	if body.ConversationID != "" {
		payload["conversationId"] = body.ConversationID
	}
	s.designProxyPass(w, r, project, taskID, "POST", "/api/chat", payload)
}

// ---- GET .../design/launch (Plan B 302) ----

func (s *Server) handleDesignLaunch(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	if !s.designRequestAuthorized(w, r, project, taskID) {
		return
	}
	projID := task.DesignProjectID
	if projID == "" {
		// Existing-design path: the gate froze approved_design_project_id into
		// the workflow outputs without persisting DesignProjectID — the launch
		// link the follow panel shows must still resolve.
		projID = s.approvedDesignProjectIDFromRun(project, taskID)
	}
	if projID == "" {
		s.jsonError(w, http.StatusConflict, "no design project for this task; start one first")
		return
	}
	cfg, err := s.resolveDesignConnection()
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	target := cfg.BaseURL + "/projects/" + projID
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

// ---- method-agnostic design proxy ----

func (s *Server) handleDesignProxy(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	if !s.designRequestAuthorized(w, r, project, taskID) {
		return
	}
	subpath := r.PathValue("path")
	if subpath == "" {
		subpath = ""
	}
	full := "/" + strings.TrimPrefix(subpath, "/")
	if !designPathAllowed(full) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "design proxy path not allowed")
		return
	}
	if !s.allowDesignRequest(taskID) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "design proxy rate limited")
		return
	}
	s.designProxyPass(w, r, project, taskID, r.Method, full, nil)
}

func designPathAllowed(path string) bool {
	for _, d := range designDeniedPrefixes {
		if strings.HasPrefix(path, d) {
			return false
		}
	}
	for _, a := range designAllowedPrefixes {
		if path == strings.TrimRight(a, "/") || strings.HasPrefix(path, a) {
			return true
		}
	}
	return false
}

// designProxyPass forwards the request to OD with server-side auth injection.
// When body is non-nil (design/chat) it replaces the incoming body; HTML
// responses get the preview-style rewrite so root-absolute subresources stay
// inside the proxy prefix.
func (s *Server) designProxyPass(w http.ResponseWriter, r *http.Request, project, taskID, method, odPath string, body map[string]any) {
	cfg, err := s.resolveDesignConnection()
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	target, err := url.Parse(cfg.BaseURL)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "bad od base url")
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = odPath
		req.URL.RawQuery = ""
		if body == nil {
			req.URL.RawQuery = r.URL.RawQuery
		}
		req.Host = target.Host
		req.Method = method
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Del("Cookie")
		if body != nil {
			raw, merr := json.Marshal(body)
			if merr == nil {
				req.Body = io.NopCloser(strings.NewReader(string(raw)))
				req.ContentLength = int64(len(raw))
				req.Header.Set("Content-Type", "application/json")
			}
		}
	}

	// Streaming (SSE event streams) must not be buffered or timed out; static
	// paths get the 15s upstream timeout (§7.7).
	if designPathIsStreaming(odPath) {
		proxy.FlushInterval = -1
	} else {
		proxy.FlushInterval = 100 * time.Millisecond
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") || designPathIsStreaming(odPath) {
			return nil
		}
		reader := resp.Body
		isGzip := strings.Contains(resp.Header.Get("Content-Encoding"), "gzip")
		if isGzip {
			gz, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil
			}
			defer gz.Close()
			reader = gz
		}
		raw, err := io.ReadAll(io.LimitReader(reader, 4<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil
		}
		odt := s.signDesignToken(taskID, project)
		html := rewriteDesignHTML(string(raw), project, taskID, odt)
		cookie := &http.Cookie{
			Name:     designTokenCookieP + taskID,
			Value:    odt,
			Path:     fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design/proxy/", url.PathEscape(project), url.PathEscape(taskID)),
			MaxAge:   int(designTokenTTL.Seconds()),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		}
		resp.Header.Add("Set-Cookie", cookie.String())
		out := []byte(html)
		resp.Body = io.NopCloser(strings.NewReader(string(out)))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", fmt.Sprint(len(out)))
		resp.Header.Del("Content-Encoding")
		return nil
	}

	// Static upstream timeout: wrap the request context.
	if !designPathIsStreaming(odPath) {
		ctx, cancel := context.WithTimeout(r.Context(), designStaticTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	proxy.ServeHTTP(w, r)
}

func designPathIsStreaming(path string) bool {
	return strings.HasPrefix(path, "/api/runs") ||
		strings.Contains(path, "/events") ||
		strings.Contains(path, "/agui") ||
		strings.HasPrefix(path, "/api/chat")
}

// rewriteDesignHTML ports the preview-proxy SPA mechanism: a <base> tag plus
// a runtime interceptor that keeps root-absolute fetch/XHR/WS/history URLs
// under the design proxy prefix.
func rewriteDesignHTML(html, project, taskID, odt string) string {
	prefix := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design/proxy/", url.PathEscape(project), url.PathEscape(taskID))
	base := fmt.Sprintf(`<base href=%q><script>(function(){
  var prefix = %q;
  function patchUrl(u){
    if (typeof u !== 'string' || !u.startsWith('/') || u.startsWith(prefix)) return u;
    return prefix + u.slice(1);
  }
  var of = window.fetch;
  if (of) { window.fetch = function(i, init){
    if (typeof i === 'string') i = patchUrl(i);
    else if (i && typeof i.url === 'string') i = new Request(patchUrl(i.url), i);
    return of.call(this, i, init);
  }; }
  var oo = XMLHttpRequest.prototype.open;
  if (oo) { XMLHttpRequest.prototype.open = function(m, u){
    var args = Array.prototype.slice.call(arguments);
    args[1] = patchUrl(u);
    return oo.apply(this, args);
  }; }
  var ow = window.WebSocket;
  if (ow) { window.WebSocket = function(u, p){
    if (typeof u === 'string') {
      try { var parsed = new URL(u, location.origin);
        if (parsed.origin === location.origin) u = patchUrl(parsed.pathname + parsed.search);
      } catch(e) {}
    }
    return new ow(u, p);
  }; }
  var ops = window.history.pushState;
  if (ops) { window.history.pushState = function(s, t, u){ return ops.call(this, s, t, patchUrl(u)); }; }
  var ors = window.history.replaceState;
  if (ors) { window.history.replaceState = function(s, t, u){ return ors.call(this, s, t, patchUrl(u)); };
  }
})();</script>`, prefix, prefix)
	return injectAfterHead(html, base)
}

func injectAfterHead(html, snippet string) string {
	lower := strings.ToLower(html)
	for _, tag := range []string{"<head>", "<head "} {
		if i := strings.Index(lower, tag); i >= 0 {
			end := strings.Index(lower[i:], ">")
			if end >= 0 {
				at := i + end + 1
				return html[:at] + snippet + html[at:]
			}
		}
	}
	return snippet + html
}

// ---- shared helpers ----

func taskAgentFallback(project string) string {
	_ = project
	return ""
}

func taskAgentFromAssignee(task *entity.Task) string {
	if task == nil {
		return ""
	}
	assignee := task.Assignee
	if i := strings.Index(assignee, "/"); i >= 0 {
		return assignee[i+1:]
	}
	return assignee
}

func designProjectIDForTask(taskID string) string {
	return "proj_mg_" + sanitizeDesignID(taskID)
}

func sanitizeDesignID(id string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func designPendingPrompt(task *entity.Task) string {
	if task == nil {
		return ""
	}
	if task.Prompt != "" {
		return task.Prompt
	}
	return task.Description
}

func designModelOrDefault(m string) string {
	if strings.TrimSpace(m) == "" {
		return odDefaultModel
	}
	return m
}

func (s *Server) writeDesignUpstreamError(w http.ResponseWriter, err error) {
	detail := err.Error()
	if len(detail) > 300 {
		detail = detail[:300]
	}
	s.jsonError(w, http.StatusBadGateway, "OD service error: "+detail)
}

func (s *Server) allowDesignRequest(taskID string) bool {
	s.designRateMu.Lock()
	defer s.designRateMu.Unlock()
	if s.designRateSeen == nil {
		s.designRateSeen = map[string]*previewChatBucket{}
	}
	now := time.Now()
	bucket, ok := s.designRateSeen[taskID]
	if !ok || now.Sub(bucket.start) >= previewChatWindow {
		if len(s.designRateSeen) > 1024 {
			for k, v := range s.designRateSeen {
				if now.Sub(v.start) >= previewChatWindow {
					delete(s.designRateSeen, k)
				}
			}
		}
		bucket = &previewChatBucket{start: now}
		s.designRateSeen[taskID] = bucket
	}
	bucket.count++
	return bucket.count <= 60
}

func (s *Server) recordDesignAudit(r *http.Request, workspaceID, action, project, taskID string, after map[string]any) {
	actorType, actorID := "system", "system"
	if username, _ := r.Context().Value(ctxUserKey).(string); username != "" {
		actorType, actorID = "user", username
	}
	_ = s.controlDB.CreateAuditEvent(controldb.AuditEvent{
		ID:           newAuditID(),
		WorkspaceID:  workspaceID,
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: "task",
		ResourceID:   project + "/" + taskID,
		Summary:      action,
		AfterJSON:    auditJSON(after),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		IP:           requestIP(r),
		UserAgent:    r.UserAgent(),
	})
}

// addComment attaches a system comment to a task (best-effort trail). The
// store keys comments by bare agent name, not the "project/agent" assignee.
func (s *Server) addComment(task *entity.Task, project, agent, text string) {
	_ = s.ts.AddComment(project, agent, &entity.TaskComment{
		ID:     entity.NewCommentID(),
		TaskID: task.ID,
		Author: "system",
		Body:   text,
	})
}

func workspaceIDOfProject(project string) string {
	_ = project
	// Audit rows are workspace-scoped; the current workspace is the only
	// scope reachable through this API surface.
	return ""
}
