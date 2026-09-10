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
	"os"
	"strings"
	"sync"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Design gate endpoints for the OpenDesign integration.
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
	// Studio boot calls a fixed set of read-only endpoints before the canvas
	// renders; a 403 on any of them (e.g. analytics/config) throws during the
	// boot chain and leaves the iframe on "Loading OpenDesign…" forever. Keep
	// credential-bearing surfaces (auth/admin/settings/connectors/integrations)
	// denied — the studio tolerates their absence like any upstream outage.
	designAllowedPrefixes = []string{"/projects/", "/api/projects/", "/api/runs", "/api/health", "/assets/", "/_next/", "/design-systems/", "/api/design-templates", "/api/templates", "/api/app-config", "/api/version", "/api/active", "/api/analytics/", "/api/workspace/", "/api/skills", "/api/agents", "/api/media/", "/api/prompt-templates", "/api/amr/", "/fonts/", "/logo-scan.svg", "/favicon.ico"}
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

// designMockWait simulates the OD CreateProject+StartRun latency so the gate
// UI can be exercised end-to-end without a live OD daemon. Enabled by env
// MULTIGENT_DESIGN_MOCK=1; responses carry the same shape as the real path
// but no OD project is created and no run is started.
func designMockEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MULTIGENT_DESIGN_MOCK")), "1")
}

// designMockReadyAfter is how long after a mock start the fake run flips from
// "running" to "succeeded", so the gate's ready/confirm UI states stay
// drivable in mock mode without a real OD run.
const designMockReadyAfter = 15 * time.Second

// designMockStarts records the last mock start per task; mock status derives
// its fake run status from the elapsed time. In-memory only — a restart drops
// the mock sessions, which is fine for a UI-acceptance switch.
var designMockStarts sync.Map // taskID -> time.Time

// designMockCanvasHTML is the placeholder document served as the mock studio
// (iframe proxy target and launch redirect target). Self-contained: no
// external subresources, so the proxy needs no HTML rewrite in mock mode.
func designMockCanvasHTML(taskID string) string {
	return `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>Mock 设计画布</title>
<style>
 body{margin:0;font-family:system-ui,-apple-system,"PingFang SC",sans-serif;background:#f4f5f7;color:#374151}
 .bar{height:48px;background:#fff;border-bottom:1px solid #e5e7eb;display:flex;align-items:center;padding:0 16px;gap:8px}
 .dot{width:10px;height:10px;border-radius:50%;background:#0ea5e9}
 .wrap{max-width:720px;margin:40px auto;padding:0 24px}
 .card{background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:24px}
 h1{font-size:16px;margin:0 0 8px}
 p{font-size:13px;line-height:1.7;margin:6px 0;color:#6b7280}
 .sk{height:12px;border-radius:6px;background:#e5e7eb;margin:10px 0;animation:p 1.2s ease-in-out infinite}
 @keyframes p{50%{opacity:.45}}
</style></head>
<body>
<div class="bar"><span class="dot"></span><strong style="font-size:13px">Mock 设计画布</strong></div>
<div class="wrap"><div class="card">
<h1>Mock 模式占位画布</h1>
<p>服务当前以 MULTIGENT_DESIGN_MOCK=1 运行，未连接真实 OpenDesign。</p>
<p>任务：<b>` + taskID + `</b></p>
<div class="sk" style="width:80%"></div>
<div class="sk" style="width:60%"></div>
<div class="sk" style="width:70%"></div>
</div></div>
</body></html>
`
}

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
	if designMockEnabled() {
		// Mock mode: deterministic fake session, ~2.5s latency so the staged
		// loading UI is observable. No OD calls, no task mutation; status
		// derives its fake run state from the timer below so both the
		// "generating" and "ready" confirm variants can be exercised.
		time.Sleep(2500 * time.Millisecond)
		designMockStarts.Store(taskID, time.Now())
		s.recordDesignAudit(r, workspaceIDOfProject(project), "design.start.mock", project, taskID, nil)
		s.writeDesignStartResponse(w, project, taskID, "mock-"+taskID, body.Regenerate, "mock-conv-"+taskID)
		return
	}
	client := s.defaultODClient()

	if task.DesignProjectID != "" && !body.Regenerate {
		s.writeDesignStartResponse(w, project, taskID, task.DesignProjectID, false, "")
		return
	}

	projID := designProjectIDForTask(taskID)
	name := "task_" + taskID

	// Regenerate: delete the old OD project first so orphans don't accumulate.
	if body.Regenerate {
		delID := task.DesignProjectID
		if delID == "" {
			delID = projID
		}
		if err := client.DeleteProject(r.Context(), delID); err != nil {
			// Not fatal: continue creating the replacement, but leave a trail.
			s.addComment(task, project, agent, "design regenerate: old OD project delete failed: "+err.Error())
		}
	}

	reqText, err := s.designGateRequirement(project, taskID, task)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	finalPrompt := designGatePrompt(reqText)

	if err := client.CreateProject(r.Context(), projID, name, body.DesignSystemID, finalPrompt); err != nil {
		created := false
		if isODProjectAlreadyExistsErr(err) {
			// Self-healing: if an orphan project already exists in OD (from an interrupted
			// run or prior session), delete it and retry creation once with the fresh prompt.
			if delErr := client.DeleteProject(r.Context(), projID); delErr == nil {
				if retryErr := client.CreateProject(r.Context(), projID, name, body.DesignSystemID, finalPrompt); retryErr == nil {
					created = true
				} else {
					err = retryErr
				}
			}
		}
		if !created {
			s.writeDesignUpstreamError(w, err)
			return
		}
	}
	// Which OD connection this design session uses is part of the audit trail:
	// multi-connection workspaces need to answer "which OD did this talk to".
	odCfg, odCfgErr := s.resolveDesignConnectionForProject(project)
	var odAudit map[string]any
	if odCfgErr == nil {
		odAudit = designConnectionAuditFields(odCfg)
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
	conversationID, err := client.StartRun(r.Context(), projID, finalPrompt, body.DesignSystemID, body.Model, creds)
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
	auditAfter := map[string]any{
		"projectId": projID, "regenerated": task.DesignProjectID != "" && body.Regenerate,
	}
	for k, v := range odAudit {
		auditAfter[k] = v
	}
	s.recordDesignAudit(r, workspaceIDOfProject(project), "design.start", project, taskID, auditAfter)
	s.writeDesignStartResponse(w, project, taskID, projID, body.Regenerate, conversationID)
}

func (s *Server) writeDesignStartResponse(w http.ResponseWriter, project, taskID, projID string, regenerated bool, conversationID string) {
	base := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design", url.PathEscape(project), url.PathEscape(taskID))
	odt := s.signDesignToken(taskID, project)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(designStartResponse{
		ProjectID: projID,
		// Root-shape studio URL: OD's client router only knows native paths,
		// so the iframe points straight at the proxied project page.
		ProxyURL:       studioProxyURL(projID, odt),
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
	if designMockEnabled() {
		s.writeDesignMockStatus(w, project, taskID)
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
	launchBase := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design", url.PathEscape(project), url.PathEscape(taskID))
	odt := s.signDesignToken(taskID, project)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"projectId": projID,
		"runStatus": status,
		"proxyUrl":  studioProxyURL(projID, odt),
		"launchUrl": launchBase + "/launch?" + designTokenQuery + "=" + odt,
	})
}

// writeDesignMockStatus answers design/status in mock mode: the fake run is
// "running" right after a mock start and flips to "succeeded" once
// designMockReadyAfter has elapsed; before any start it reports "none". URLs
// are always re-minted so a reopen/refresh recovers the placeholder session.
func (s *Server) writeDesignMockStatus(w http.ResponseWriter, project, taskID string) {
	status := "none"
	if v, ok := designMockStarts.Load(taskID); ok {
		if time.Since(v.(time.Time)) >= designMockReadyAfter {
			status = "succeeded"
		} else {
			status = "running"
		}
	}
	launchBase := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/design", url.PathEscape(project), url.PathEscape(taskID))
	odt := s.signDesignToken(taskID, project)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"projectId": "mock-" + taskID,
		"runStatus": status,
		"proxyUrl":  studioProxyURL("mock-"+taskID, odt),
		"launchUrl": launchBase + "/launch?" + designTokenQuery + "=" + odt,
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
	if !s.allowDesignRequest(taskID, true) {
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

// studioProxyURL mints the root-shape studio iframe URL: the proxied project
// page with a fresh signed odt bootstrap token.
func studioProxyURL(projID, odt string) string {
	return "/projects/" + projID + "?" + designTokenQuery + "=" + odt
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
	if designMockEnabled() {
		// Mock mode has no OD to redirect to; serve the placeholder canvas so
		// the new-tab flow stays observable.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(designMockCanvasHTML(taskID)))
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

// ---- root-shape design proxy ----

const (
	designWriteRateLimit = 60
	designReadRateLimit  = 600

	// designProjectPrefix marks OD projects minted by multigent: proj_mg_<taskID>.
	designProjectPrefix = "proj_mg_"
)

// The studio iframe previously lived under /api/v1/.../design/proxy/..., but
// OD's Next.js router reads location.pathname and only knows root-shaped
// routes (/projects/<id>/…), so the proxied app always fell back to its home
// page. The design surface is therefore served at OD's native path shape on
// the console origin:
//
//	/projects/proj_mg_<taskID>/…   project pages (+ session cookie)
//	/_next/, /fonts/, /design-systems/, /logo-scan.svg   static assets
//	/api/<anything-but-v1>         OD's own API surface
//
// Every request must present a valid design session credential (odt query
// token or the mg_od_<taskID> cookie); without one the handlers fall back to
// the console SPA / answer 404, so console routes (/projects/<name>/…) are
// never shadowed. The OD project id is derived from the task ID, which makes
// collisions with console project names structurally impossible.

// handleDesignRootProject serves /projects/… when the path addresses a design
// session; it reports false for anything else so the caller can fall through
// to the console SPA.
func (s *Server) handleDesignRootProject(w http.ResponseWriter, r *http.Request) bool {
	seg := strings.TrimPrefix(r.URL.Path, "/projects/")
	if i := strings.Index(seg, "/"); i >= 0 {
		seg = seg[:i]
	}
	if designMockEnabled() {
		// Mock canvas: a standalone placeholder document with no subresources.
		if taskID, ok := strings.CutPrefix(seg, "mock-"); ok && taskID != "" {
			if !s.designRootAuthorize(w, r, taskID) {
				return true
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(designMockCanvasHTML(taskID)))
			return true
		}
		return false
	}
	taskID, ok := strings.CutPrefix(seg, designProjectPrefix)
	if !ok || taskID == "" {
		return false
	}
	if !s.designRootAuthorize(w, r, taskID) {
		return true
	}
	s.designRootProxy(w, r, taskID)
	return true
}

// handleDesignRootAsset serves OD's root-shaped static and API surfaces for
// requests carrying a valid design session; anything else is a plain 404
// (the console never requests these paths).
func (s *Server) handleDesignRootAsset(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.designRootTaskFromCredential(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.designRootAuthorize(w, r, taskID) {
		return
	}
	s.designRootProxy(w, r, taskID)
}

// designRootTaskFromCredential resolves the task behind a root-shape static/
// API request. The project page carries the task id in the path
// (proj_mg_<taskID>) and mints the session cookie; these requests are
// attributed via that cookie (or a still-present odt query token).
func (s *Server) designRootTaskFromCredential(r *http.Request) (string, bool) {
	if tok := strings.TrimSpace(r.URL.Query().Get(designTokenQuery)); tok != "" {
		if claims, ok := s.verifyPreviewTokenAny(tok); ok && claims.TaskID != "" {
			return claims.TaskID, true
		}
	}
	for _, c := range r.Cookies() {
		if !strings.HasPrefix(c.Name, designTokenCookieP) {
			continue
		}
		if claims, ok := s.verifyPreviewTokenAny(c.Value); ok && claims.TaskID != "" {
			return claims.TaskID, true
		}
	}
	return "", false
}

// designRootAuthorize admits a root-shape request for taskID: a valid design
// token (query or cookie) or authenticated console traffic. On failure it has
// already written the response.
func (s *Server) designRootAuthorize(w http.ResponseWriter, r *http.Request, taskID string) bool {
	if tok := strings.TrimSpace(r.URL.Query().Get(designTokenQuery)); tok != "" {
		if _, ok := s.verifyPreviewToken(tok, taskID); ok {
			return true
		}
	}
	if cookie, err := r.Cookie(designTokenCookieP + taskID); err == nil {
		if _, ok := s.verifyPreviewToken(cookie.Value, taskID); ok {
			return true
		}
	}
	// Console-authenticated traffic (tests, server-side callers) — resolves
	// the owning project from the task for the RBAC check.
	taskProject, _, task, err := s.ts.FindTaskByID(taskID)
	if err != nil || task == nil {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "design session required")
		return false
	}
	return s.previewRequestAuthorized(w, r, taskProject, taskID)
}

func (s *Server) designRootProxy(w http.ResponseWriter, r *http.Request, taskID string) {
	odPath := r.URL.Path
	if !designPathAllowed(odPath) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "design proxy path not allowed")
		return
	}
	// Rate-limit only OD API calls, not static assets: a single studio boot
	// fetches dozens of /_next/ chunks, which would exhaust the per-task
	// budget and leave the canvas stuck on its loader (2026-09-02).
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if !designPathIsStatic(odPath) && !s.allowDesignRequest(taskID, write) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "design proxy rate limited")
		return
	}
	// The odt credential must not leak upstream.
	if r.URL.Query().Has(designTokenQuery) {
		q := r.URL.Query()
		q.Del(designTokenQuery)
		r.URL.RawQuery = q.Encode()
	}
	// Session-scoped assets must never be cached under a shared URL shape:
	// a cached response (e.g. a stale 401 or a doubled Content-Type from an
	// earlier deploy) would keep breaking the studio after the fix.
	w.Header().Set("Cache-Control", "no-store")
	s.designProxyPass(w, r, "", taskID, r.Method, odPath, nil)
}

// serveDesignLooseAsset proxies a root-level static file (single path
// segment, static extension) to OD when the request carries a valid design
// session; it reports false for everything else so the caller falls through
// to the console. This covers OD's unenumerable loose root images
// (composer-matrix-loader.svg, …) without shadowing console routes.
func (s *Server) serveDesignLooseAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !isRootLevelStaticAsset(r.URL.Path) {
		return false
	}
	taskID, ok := s.designRootTaskFromCredential(r)
	if !ok {
		return false
	}
	if !s.designRootAuthorize(w, r, taskID) {
		return true
	}
	s.designRootProxy(w, r, taskID)
	return true
}

// isRootLevelStaticAsset matches loose OD static files outside its bundler
// output: root-level "/name.ext" or shallow "/dir/name.ext" under a
// non-console directory (IsStudioLooseAssetPath's server-side twin).
func isRootLevelStaticAsset(path string) bool {
	return IsStudioLooseAssetPath(path)
}

// designPathAllowed whitelists the OD surfaces the studio may touch.
func designPathAllowed(path string) bool {
	for _, d := range designDeniedPrefixes {
		if strings.HasPrefix(path, d) {
			return false
		}
	}
	// Loose assets OD ships at its origin root (composer-matrix-loader.svg,
	// logo-scan.svg, …) — not enumerable, so accept the whole class for
	// credential-gated requests.
	if isRootLevelStaticAsset(path) {
		return true
	}
	for _, a := range designAllowedPrefixes {
		if path == strings.TrimRight(a, "/") || strings.HasPrefix(path, a) {
			return true
		}
	}
	return false
}

// designPathIsStatic reports whether a proxied path is a static asset that
// carries no upstream side effects; those are exempt from the per-task proxy
// rate limit (see handleDesignProxy).
func designPathIsStatic(path string) bool {
	return strings.HasPrefix(path, "/_next/") ||
		strings.HasPrefix(path, "/assets/") ||
		strings.HasSuffix(path, ".js") ||
		strings.HasSuffix(path, ".css") ||
		strings.HasSuffix(path, ".png") ||
		strings.HasSuffix(path, ".svg") ||
		strings.HasSuffix(path, ".ico") ||
		strings.HasSuffix(path, ".woff") ||
		strings.HasSuffix(path, ".woff2") ||
		strings.HasSuffix(path, ".webp") ||
		strings.HasSuffix(path, ".gif") ||
		strings.HasSuffix(path, ".jpg") ||
		strings.HasSuffix(path, ".jpeg") ||
		strings.HasSuffix(path, ".webmanifest")
}

// designProxyPass forwards the request to OD with server-side auth injection.
// When body is non-nil (design/chat) it replaces the incoming body. HTML
// responses are forwarded byte-for-byte (root-shape proxy — no URL surgery)
// and only mint the scoped session cookie.
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
		// Root-shape proxy: the document is forwarded byte-for-byte — OD sees
		// native paths on both sides, so no base/patcher/importmap surgery is
		// needed (and would break OD's client-side router again). The HTML pass
		// only mints the scoped session cookie the subresource requests carry.
		odt := s.signDesignToken(taskID, project)
		cookie := &http.Cookie{
			Name:     designTokenCookieP + taskID,
			Value:    odt,
			Path:     "/",
			MaxAge:   int(designTokenTTL.Seconds()),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		}
		resp.Header.Add("Set-Cookie", cookie.String())
		resp.Header.Set("Cache-Control", "no-store")
		resp.ContentLength = int64(len(raw))
		resp.Header.Set("Content-Length", fmt.Sprint(len(raw)))
		resp.Header.Del("Content-Encoding")
		resp.Body = io.NopCloser(strings.NewReader(string(raw)))
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

// designGateRequirement resolves the requirement text for the design gate.
// For workflow runs parked at design_review, it strictly prioritizes
// approved_requirement (or requirement_draft) from step inputs/outputs, and
// fails closed if neither is present. For non-workflow tasks, it falls back
// to the task's prompt/description.
func (s *Server) designGateRequirement(project, taskID string, task *entity.Task) (string, error) {
	workspaceID, err := s.currentWorkspaceID()
	if err == nil && workspaceID != "" && s.controlDB != nil {
		wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
		run, found, err := wfStore.RunForTask(project, taskID)
		if err == nil && found {
			insts, err := wfStore.ListStepInstances(run.ID)
			if err != nil {
				return "", fmt.Errorf("list workflow step instances: %w", err)
			}
			// 1. Check active step instance (matching run.ActiveStepID or "design_review")
			for i := len(insts) - 1; i >= 0; i-- {
				if insts[i].StepID == run.ActiveStepID || insts[i].StepID == "design_review" {
					if req := strings.TrimSpace(insts[i].InputValues["approved_requirement"]); req != "" {
						return req, nil
					}
					if req := strings.TrimSpace(insts[i].InputValues["requirement_draft"]); req != "" {
						return req, nil
					}
					break
				}
			}
			// 2. Fall back to scanning prior step instances for requirement outputs/inputs
			for i := len(insts) - 1; i >= 0; i-- {
				if req := strings.TrimSpace(insts[i].OutputValues["approved_requirement"]); req != "" {
					return req, nil
				}
				if req := strings.TrimSpace(insts[i].InputValues["approved_requirement"]); req != "" {
					return req, nil
				}
				if req := strings.TrimSpace(insts[i].OutputValues["requirement_draft"]); req != "" {
					return req, nil
				}
				if req := strings.TrimSpace(insts[i].InputValues["requirement_draft"]); req != "" {
					return req, nil
				}
			}
			// In a workflow design gate, requirement cannot be empty (fail closed).
			return "", fmt.Errorf("workflow design gate requires approved_requirement or requirement_draft")
		}
	}

	// Standalone task (not part of a workflow run)
	fallback := strings.TrimSpace(designPendingPrompt(task))
	if fallback == "" {
		return "", fmt.Errorf("task prompt and description are empty")
	}
	return fallback, nil
}

func designGatePrompt(requirement string) string {
	return fmt.Sprintf(`【角色与使命】
你是本次交付的原型设计专家（OpenDesign UI Designer）。
你的唯一职责是：基于下方经过产品与用户确认的真实需求规格，设计并实现高保真、可交互的 UI 原型界面。

【硬性边界 — 严禁越界】
1. 原型沙箱内工作：你所有的界面设计、页面布局和交互逻辑均在当前 OpenDesign 设计工作区内完成。
2. 严禁修改工程代码：严禁触碰、修改或提交目标项目的交付 Git 仓库代码（代码实现由后续研发 Agent 完成）。
3. 严禁编写生产后端：严禁编写生产级后端服务、数据库迁移或独立 API 服务，原型中的数据展示请使用合理的前端 Mock 数据。

【产出要求】
- 完整覆盖下方需求中的核心业务流程、关键页面与交互状态（正常态、空状态、加载态、错误提示等）。
- 产出高保真、视觉规范统一样式的 Web/UI 原型，以便人工审核后冻结快照，作为后续研发的唯一视觉基线。

【执行与落盘行动指南（重要）】
1. 立即落盘原型文件：理清思路后，必须立即使用文件写入工具在当前工作区根目录下创建原型文件（首选 index.html，可内联样式脚本或引入 Tailwind CDN），严禁仅在思考分析或对话文字中输出代码而不落盘！
2. 聚焦完整单页或核心多视图：将核心控制台仪表盘、列表、弹窗与抽屉等完整构建在原型中，配备完备的前端交互与演示 Mock 数据。
3. 控制思考长度：聚焦页面布局与交互实现，避免超长冗余思考，确保单次调用顺利完成文件落盘交付。

【经过确认的需求规格输入】
<requirement>
%s
</requirement>`, strings.TrimSpace(requirement))
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

func isODProjectAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "unique constraint failed: projects.id") ||
		strings.Contains(lower, "already exists") ||
		strings.Contains(lower, "duplicate")
}

// allowDesignRequest caps proxied studio traffic per task per window. Writes
// keep the tight chat-style budget; reads (the studio's boot burst, its ~5s
// status polling and navigation) get 10× headroom — one healthy studio
// session exceeds 60 GETs/min and would otherwise deadlock retrying against
// its own exhausted budget.
func (s *Server) allowDesignRequest(taskID string, write bool) bool {
	limit := designReadRateLimit
	seen := &s.designReadRateSeen
	if write {
		limit = designWriteRateLimit
		seen = &s.designWriteRateSeen
	}
	s.designRateMu.Lock()
	defer s.designRateMu.Unlock()
	if *seen == nil {
		*seen = map[string]*previewChatBucket{}
	}
	now := time.Now()
	bucket, ok := (*seen)[taskID]
	if !ok || now.Sub(bucket.start) >= previewChatWindow {
		if len(*seen) > 1024 {
			for k, v := range *seen {
				if now.Sub(v.start) >= previewChatWindow {
					delete(*seen, k)
				}
			}
		}
		bucket = &previewChatBucket{start: now}
		(*seen)[taskID] = bucket
	}
	bucket.count++
	return bucket.count <= limit
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
