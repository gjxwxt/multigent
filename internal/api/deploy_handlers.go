package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Deploy center handlers (batch 3): REST surface over the deploy_requests
// ledger (internal/db/deploy_requests.go) plus the GitLab pipeline trigger
// and the startup self-healing sweep. Auth posture:
//
//   - every endpoint runs s.checkProjectAccess first;
//   - state-changing endpoints additionally require project operator
//     (s.checkProjectOperator) — the same bar as preview write surfaces;
//   - all routes are registered on the token-authenticated main mux. The
//     ONLY public route is the read-only verify callback
//     (GET /api/v1/deploy-verify, deploy_verify_token.go).
//
// Credential invariant: sensitive vars (keys matching SECRET/TOKEN/PASSWORD/
// KEY) are pushed to GitLab CI/CD variables and stored locally masked as
// "***" — credentials never land in the deploy ledger (AGENTS.md §5.2).

// deploySensitiveVar reports whether a user-supplied deploy variable key
// carries a credential that must not be persisted locally.
func deploySensitiveVar(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	for _, marker := range []string{"SECRET", "TOKEN", "PASSWORD", "KEY"} {
		if strings.Contains(k, marker) {
			return true
		}
	}
	return false
}

// deploySensitiveVarMask is what the ledger stores in place of a secret value.
const deploySensitiveVarMask = "***"

// deployGitLabHostFor resolves the GitLab host for external operations on a
// project. Production resolves through the verified remote binding
// (fail-closed: no binding, no deploy). It is a plain method rather than an
// inline call so tests can swap the GitLab surface without a live forge;
// there is no var seam in production code.
func (s *Server) deployGitLabHostFor(ctx context.Context, project string) (*codehost.GitLabHost, *controldb.VerifiedRemoteBinding, error) {
	return s.verifiedGitLabHost(ctx, project)
}

// deployProjectRow loads the project entity (deploy port, approval default).
func (s *Server) deployProjectRow(w http.ResponseWriter, project string) (*entity.Project, bool) {
	p, err := s.st.Project(project)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return nil, false
		}
		s.serverError(w, err)
		return nil, false
	}
	return p, true
}

// handleGetDeployAggregate serves GET /api/v1/projects/{name}/deploy: the
// one round-trip the console deploy page needs — project, deploy port,
// last finished deploy, in-flight deploy, live health probe of the deployed
// app, and recent pipeline job summaries. Every GitLab-dependent section
// degrades gracefully: a missing binding or an unreachable forge must not
// block the ledger view (branches/pipelines render empty instead).
func (s *Server) handleGetDeployAggregate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	p, ok := s.deployProjectRow(w, project)
	if !ok {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}

	// Ledger reads: newest request (regardless of status) is the display
	// candidate; it is "lastDeployed" only when terminal, "inflight" when
	// not.
	requests, err := s.controlDB.ListDeployRequests(controldb.DeployRequestFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
		Limit:       10,
	})
	if err != nil {
		s.serverError(w, fmt.Errorf("list deploy requests: %w", err))
		return
	}
	var lastDeployed, inflight *controldb.DeployRequest
	for i := range requests {
		status := requests[i].Status
		if isInflightDeployStatus(status) && inflight == nil {
			inflight = &requests[i]
		}
		if isTerminalDeployStatus(status) && lastDeployed == nil {
			lastDeployed = &requests[i]
		}
	}

	health := s.probeDeployedAppHealth(p, lastDeployed)

	// Pipelines: best-effort job summaries; a failure degrades to an empty
	// list (the frontend renders "—" rather than an error banner).
	pipelines := s.deployPipelineSummaries(r.Context(), project, 10)

	deployHostConfigured := strings.TrimSpace(os.Getenv(deployHostEnv)) != ""

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"project":              project,
		"deployPort":           p.DeployPort,
		"deployHostConfigured": deployHostConfigured,
		"lastDeployed":         lastDeployed,
		"inflight":             inflight,
		"health":               health,
		"pipelines":            pipelines,
	})
}

// handleGetDeployBranches serves GET /api/v1/projects/{name}/deploy/branches:
// a thin proxy over the verified binding's GitLab ListBranches so the deploy
// page can pin a SHA. Unverified projects get an empty list, not an error —
// the page treats "no branches" as "unbound".
func (s *Server) handleGetDeployBranches(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	_, ok := s.deployProjectRow(w, project)
	if !ok {
		return
	}
	out := []map[string]any{}
	host, binding, err := s.deployGitLabHostFor(r.Context(), project)
	if err != nil {
		log.Printf("[deploy] %s: branches unavailable: %v", project, err)
	} else if branches, listErr := host.ListBranches(r.Context(), binding.RemoteProjectID); listErr != nil {
		log.Printf("[deploy] %s: list branches failed: %v", project, listErr)
	} else {
		for _, b := range branches {
			out = append(out, map[string]any{
				"name":        b.Name,
				"isDefault":   b.Default,
				"commitId":    b.CommitID,
				"commitTitle": b.CommitTitle,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type createDeployRequest struct {
	Branch           string            `json:"branch"`
	SHA              string            `json:"sha"`
	Vars             map[string]string `json:"vars"`
	ApprovalRequired *bool             `json:"approvalRequired"`
}

// handleCreateDeployRequest serves POST /api/v1/projects/{name}/deploy/requests.
// Operator-gated. Builds the commit span from the last successful deploy,
// splits sensitive vars out to GitLab CI/CD variables (local ledger stores
// only the mask), and either parks the request at pending_approval or moves
// it straight to approved. The partial unique index
// uq_deploy_requests_inflight guarantees at most one in-flight request per
// project — a violation surfaces as 409.
func (s *Server) handleCreateDeployRequest(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	p, ok := s.deployProjectRow(w, project)
	if !ok {
		return
	}
	var body createDeployRequest
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidRequestBody, "invalid request body")
		return
	}
	body.Branch = strings.TrimSpace(body.Branch)
	body.SHA = strings.TrimSpace(body.SHA)
	if body.Branch == "" {
		s.jsonError(w, http.StatusBadRequest, "branch is required")
		return
	}

	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}

	// Resolve the target SHA: an explicit SHA wins; otherwise the branch
	// HEAD from GitLab. Branch resolution is the one GitLab call that is
	// mandatory here — without a SHA the request pins nothing.
	sha := body.SHA
	host, binding, hostErr := s.deployGitLabHostFor(r.Context(), project)
	if hostErr != nil {
		s.jsonErrorCode(w, http.StatusServiceUnavailable, ErrCodeServiceUnavailable,
			"deploy requires a verified remote binding; run the admin remote verify flow first")
		return
	}
	if sha == "" {
		branches, listErr := host.ListBranches(r.Context(), binding.RemoteProjectID)
		if listErr != nil {
			s.jsonErrorCode(w, http.StatusBadGateway, ErrCodeUpstreamError,
				"resolve branch head from gitlab failed")
			return
		}
		for _, b := range branches {
			if b.Name == body.Branch {
				sha = strings.TrimSpace(b.CommitID)
				break
			}
		}
		if sha == "" {
			s.jsonError(w, http.StatusBadRequest, "branch not found on remote: "+body.Branch)
			return
		}
	}

	// Commit span: baseline is the last successful deploy's SHA; without a
	// baseline (first deploy) the span is just the target commit.
	span := []controldb.CommitSpanEntry{{SHA: sha, ShortSHA: shortDeploySHA(sha)}}
	if base, found, baseErr := s.controlDB.LatestSucceededDeployRequest(workspaceID, project); baseErr != nil {
		s.serverError(w, fmt.Errorf("read last successful deploy: %w", baseErr))
		return
	} else if found && base != nil && strings.TrimSpace(base.SHA) != "" && base.SHA != sha {
		span = []controldb.CommitSpanEntry{
			{SHA: base.SHA, ShortSHA: shortDeploySHA(base.SHA)},
			{SHA: sha, ShortSHA: shortDeploySHA(sha)},
		}
	}

	// Split sensitive vars: values go to GitLab CI/CD variables; the ledger
	// keeps only the mask. A failed push fails the request — silently
	// deploying with a missing secret would be worse than not deploying.
	localVars := make(map[string]string, len(body.Vars))
	for key, value := range body.Vars {
		if !deploySensitiveVar(key) {
			localVars[key] = value
			continue
		}
		ciKey := deployVarCIPrefix + key
		if err := host.SetProjectVariable(r.Context(), binding.RemoteProjectID, ciKey, value); err != nil {
			log.Printf("[deploy] %s: push sensitive var %s to gitlab failed: %v", project, ciKey, err)
			s.jsonErrorCode(w, http.StatusBadGateway, ErrCodeUpstreamError,
				"push deploy variable to gitlab failed")
			return
		}
		localVars[key] = deploySensitiveVarMask
	}

	approvalRequired := s.deployApprovalRequired(r, p, body.ApprovalRequired)
	now := nowUTCAPI()
	id := newDeployRequestID()
	status := "approved"
	if approvalRequired {
		status = "pending_approval"
	}
	req := controldb.DeployRequest{
		ID:          id,
		WorkspaceID: workspaceID,
		ProjectID:   project,
		Branch:      body.Branch,
		SHA:         sha,
		Env:         "production",
		Vars:        localVars,
		CommitSpan:  span,
		Approval: map[string]any{
			"required": approvalRequired,
			"state":    status,
		},
		Status:    status,
		CreatedBy: s.currentUserName(r),
		CreatedAt: now,
	}
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		if isDeployInflightConflict(err) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict,
				"another deploy request is already in flight for this project")
			return
		}
		s.serverError(w, fmt.Errorf("insert deploy request: %w", err))
		return
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.create",
		ResourceType: "deploy_request",
		ResourceID:   id,
		Summary:      fmt.Sprintf("deploy request %s for %s@%s (status %s)", id, body.Branch, shortDeploySHA(sha), status),
		After: map[string]any{
			"id": id, "project": project, "branch": body.Branch, "sha": sha, "status": status,
		},
		Request: r,
	})

	if status == "pending_approval" {
		// Batch 4: render the approval card in the project's IM channel.
		s.postDeployApprovalCard(&req)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(req)
}

// deployVarCIPrefix namespaces platform-pushed variables in the GitLab CI/CD
// variable store.
const deployVarCIPrefix = "MULTIGENT_DEPLOY_VAR_"

// deployApprovalRequired resolves the approval gate: an explicit request
// flag wins; otherwise the project-level default (entity.Project has no
// dedicated field yet — TODO(batch-4): add ApprovalRequired to entity.Project
// and persist it via the project PUT); otherwise approval is off.
func (s *Server) deployApprovalRequired(r *http.Request, p *entity.Project, explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	// TODO(batch-4): read p.ApprovalRequired once the entity field exists.
	_ = r
	_ = p
	return false
}

// deployHostEnv names the machine that hosts deployed apps; used both for
// the health probe target and the console URL handed to CI.
const deployHostEnv = "MULTIGENT_DEPLOY_HOST"

// consoleURLEnv overrides the console address handed to the CI deploy gate.
const consoleURLEnv = "MULTIGENT_CONSOLE_URL"

// consoleReachableURL builds the console base URL the CI job calls back to.
// MULTIGENT_CONSOLE_URL wins; otherwise scheme http + request Host (the
// server is the console in local deployments).
func (s *Server) consoleReachableURL(r *http.Request) string {
	if raw := strings.TrimSpace(os.Getenv(consoleURLEnv)); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// deployedAppHealthURL builds the health endpoint of the deployed app on the
// deploy host, or ("", false) when the deployment has no probe target.
func deployedAppHealthURL(deployPort int) (string, bool) {
	host := strings.TrimSpace(os.Getenv(deployHostEnv))
	if host == "" || deployPort <= 0 {
		return "", false
	}
	return fmt.Sprintf("http://%s:%d/api/health", host, deployPort), true
}

// probeDeployedAppHealth live-probes the deployed app when a finished
// deployment exists. Unconfigured host → status unknown with reason; probe
// failure is reported honestly (status down) — never an error response and
// never a panic. Timeout is 3s so the aggregate endpoint stays snappy.
func (s *Server) probeDeployedAppHealth(p *entity.Project, lastDeployed *controldb.DeployRequest) map[string]any {
	if lastDeployed == nil {
		return nil
	}
	url, ok := deployedAppHealthURL(p.DeployPort)
	if !ok {
		return map[string]any{"status": "unknown", "reason": "deploy host not configured"}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		return map[string]any{"status": "down", "reason": err.Error()}
	}
	defer resp.Body.Close()
	latencyMs := time.Since(start).Milliseconds()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return map[string]any{"status": "down", "httpStatus": resp.StatusCode, "latencyMs": latencyMs}
	}
	return map[string]any{"status": "up", "latencyMs": latencyMs}
}

// deployPipelineSummaries fetches the most recent pipelines with their job
// summaries for the aggregate view. All failures degrade to an empty list.
func (s *Server) deployPipelineSummaries(ctx context.Context, project string, limit int) []map[string]any {
	out := []map[string]any{}
	host, binding, err := s.deployGitLabHostFor(ctx, project)
	if err != nil {
		return out
	}
	pipelines, err := host.ListRecentPipelines(ctx, binding.RemoteProjectID, limit)
	if err != nil {
		log.Printf("[deploy] %s: list recent pipelines failed: %v", project, err)
		return out
	}
	for _, pipe := range pipelines {
		entry := map[string]any{
			"id":     pipe.ID,
			"ref":    pipe.Ref,
			"sha":    pipe.SHA,
			"status": pipe.Status,
			"webUrl": pipe.WebURL,
		}
		if jobs, jobsErr := host.PipelineJobs(ctx, binding.RemoteProjectID, pipe.ID); jobsErr == nil {
			summary := make([]map[string]any, 0, len(jobs))
			for _, j := range jobs {
				summary = append(summary, map[string]any{
					"name":              j.Name,
					"stage":             j.Stage,
					"status":            j.Status,
					"runnerDescription": j.RunnerDescription,
				})
			}
			entry["jobs"] = summary
		}
		out = append(out, entry)
	}
	return out
}

// deployRequestContext resolves project + workspace + the request row for
// {id}-scoped endpoints. Returns ok=false when a response was written.
func (s *Server) deployRequestContext(w http.ResponseWriter, r *http.Request) (project string, workspaceID string, req *controldb.DeployRequest, ok bool) {
	project = r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return "", "", nil, false
	}
	if !s.checkProjectOperator(w, r, project) {
		return "", "", nil, false
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return "", "", nil, false
	}
	id := r.PathValue("id")
	req, found, err := s.controlDB.DeployRequestFor(workspaceID, id)
	if err != nil {
		s.serverError(w, fmt.Errorf("read deploy request: %w", err))
		return "", "", nil, false
	}
	if !found || req == nil || req.ProjectID != project {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "deploy request not found")
		return "", "", nil, false
	}
	return project, workspaceID, req, true
}

// handleApproveDeployRequest serves POST .../deploy/requests/{id}/approve:
// CAS pending_approval → approved, then triggers the pipeline.
func (s *Server) handleApproveDeployRequest(w http.ResponseWriter, r *http.Request) {
	project, workspaceID, req, ok := s.deployRequestContext(w, r)
	if !ok {
		return
	}
	if req.Status != "pending_approval" {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "deploy request is not pending approval")
		return
	}
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "pending_approval", "approved"); err != nil {
		s.serverError(w, fmt.Errorf("approve deploy request: %w", err))
		return
	}
	approved, _, err := s.controlDB.DeployRequestFor(workspaceID, req.ID)
	if err != nil || approved == nil {
		approved = req
		approved.Status = "approved"
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.approve",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
		Summary:      "deploy request " + req.ID + " approved",
		Request:      r,
	})
	// Approval implies the pipeline fires: reuse the trigger flow verbatim
	// (CAS already moved the row to approved, so the CAS inside the trigger
	// handler cannot run here — call the shared trigger body directly).
	s.triggerDeployPipelineNow(w, r, project, workspaceID, approved)
}

// triggerDeployPipelineNow is the shared trigger body: mints the CI gate
// token, fires the GitLab pipeline, records the pipeline id, and starts the
// bounded watcher. Written so both the explicit trigger endpoint (after its
// own CAS) and the approve endpoint (after its CAS) share one code path.
func (s *Server) triggerDeployPipelineNow(w http.ResponseWriter, r *http.Request, project, workspaceID string, req *controldb.DeployRequest) {
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "approved", "deploying"); err != nil {
		s.serverError(w, fmt.Errorf("mark deploy request deploying: %w", err))
		return
	}
	req.Status = "deploying"
	req.StartedAt = nowUTCAPI()

	// Token + variables for the CI gate. The verify token is bound to the
	// exact (project, SHA) and expires in 30 minutes — runner queue margin.
	token, err := s.signDeployVerifyToken(project, req.SHA, req.ID, deployTokenTTL)
	if err != nil {
		_, _ = s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "deploying", "failed")
		s.serverError(w, fmt.Errorf("sign deploy verify token: %w", err))
		return
	}
	variables := map[string]string{
		"MULTIGENT_DEPLOY":       "1",
		"MULTIGENT_DEPLOY_TOKEN": token,
		"MULTIGENT_CONSOLE_URL":  s.consoleReachableURL(r),
		// The gate callback must identify the platform project. CI_PROJECT_NAME
		// is the repo slug and only coincides with the platform name for
		// platform-created repos; passing the name explicitly keeps brownfield
		// bindings (platform project ≠ repo slug) working.
		"MULTIGENT_PROJECT_NAME": project,
	}

	host, binding, err := s.deployGitLabHostFor(r.Context(), project)
	if err != nil {
		_, _ = s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "deploying", "failed")
		s.jsonErrorCode(w, http.StatusServiceUnavailable, ErrCodeServiceUnavailable,
			"deploy requires a verified remote binding; run the admin remote verify flow first")
		return
	}
	pipeline, err := host.TriggerPipeline(r.Context(), binding.RemoteProjectID, req.Branch, variables)
	if err != nil {
		_, _ = s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "deploying", "failed")
		log.Printf("[deploy] %s: trigger pipeline for %s failed: %v", project, req.ID, err)
		s.jsonErrorCode(w, http.StatusBadGateway, ErrCodeUpstreamError, "trigger gitlab pipeline failed")
		return
	}
	if err := s.controlDB.SetDeployRequestPipeline(workspaceID, req.ID, pipeline.ID); err != nil {
		log.Printf("[deploy] %s: record pipeline %d for %s failed: %v", project, pipeline.ID, req.ID, err)
	}
	req.PipelineID = pipeline.ID

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.trigger",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
		Summary:      fmt.Sprintf("deploy request %s triggered gitlab pipeline %d", req.ID, pipeline.ID),
		Request:      r,
	})

	go s.watchDeployPipeline(req.ID, workspaceID, project, binding.RemoteProjectID, pipeline.ID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(req)
}

// handleRejectDeployRequest serves POST .../deploy/requests/{id}/reject:
// CAS pending_approval → rejected (terminal). The IM card archival is
// batch 4's concern.
func (s *Server) handleRejectDeployRequest(w http.ResponseWriter, r *http.Request) {
	_, workspaceID, req, ok := s.deployRequestContext(w, r)
	if !ok {
		return
	}
	moved, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "pending_approval", "rejected")
	if err != nil {
		s.serverError(w, fmt.Errorf("reject deploy request: %w", err))
		return
	}
	if !moved {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "deploy request is not pending approval")
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.reject",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
		Summary:      "deploy request " + req.ID + " rejected",
		Request:      r,
	})
	req.Status = "rejected"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(req)
}

// handleCancelDeployRequest serves POST .../deploy/requests/{id}/cancel:
// pending_approval/approved → cancelled.
func (s *Server) handleCancelDeployRequest(w http.ResponseWriter, r *http.Request) {
	_, workspaceID, req, ok := s.deployRequestContext(w, r)
	if !ok {
		return
	}
	moved, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "pending_approval", "cancelled")
	if err != nil {
		s.serverError(w, fmt.Errorf("cancel deploy request: %w", err))
		return
	}
	if !moved {
		moved, err = s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "approved", "cancelled")
		if err != nil {
			s.serverError(w, fmt.Errorf("cancel deploy request: %w", err))
			return
		}
	}
	if !moved {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "deploy request can no longer be cancelled")
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.cancel",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
		Summary:      "deploy request " + req.ID + " cancelled",
		Request:      r,
	})
	req.Status = "cancelled"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(req)
}

// handleTriggerDeployRequest serves POST .../deploy/requests/{id}/trigger:
// CAS approved → deploying, mints the CI gate token, fires the GitLab
// pipeline, and spawns a bounded watcher goroutine that reconciles the
// ledger with the pipeline's terminal state. 202 because completion is
// asynchronous by design.
func (s *Server) handleTriggerDeployRequest(w http.ResponseWriter, r *http.Request) {
	project, workspaceID, req, ok := s.deployRequestContext(w, r)
	if !ok {
		return
	}
	if req.Status != "approved" {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "deploy request is not approved")
		return
	}
	s.triggerDeployPipelineNow(w, r, project, workspaceID, req)
}

// watchDeployPipeline polls the pipeline until terminal state (bounded at
// 30 minutes) and reconciles the ledger: success → live health probe +
// status success + health snapshot; failed/canceled → status failed. All
// errors are logged, never propagated — the goroutine owns no response.
// The SHA is re-read from the ledger each round so the watcher survives a
// pipeline recorded after the handler returned.
func (s *Server) watchDeployPipeline(requestID, workspaceID, project, remoteProjectID string, pipelineID int64) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[deploy] watch pipeline for %s panicked: %v", requestID, rec)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	host, _, err := s.deployGitLabHostFor(ctx, project)
	if err != nil {
		log.Printf("[deploy] %s: watch pipeline for %s: resolve host failed: %v", project, requestID, err)
		_, _ = s.controlDB.UpdateDeployRequestStatus(workspaceID, requestID, "deploying", "failed")
		return
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[deploy] %s: watch pipeline for %s timed out; leaving status as-is", project, requestID)
			return
		case <-ticker.C:
		}
		pipelines, err := host.PipelinesForSHA(ctx, remoteProjectID, s.latestDeploySHA(requestID, workspaceID))
		if err != nil {
			log.Printf("[deploy] %s: watch pipeline for %s: list pipelines failed: %v", project, requestID, err)
			continue
		}
		var status string
		for _, pipe := range pipelines {
			if pipe.ID == pipelineID {
				status = strings.TrimSpace(pipe.Status)
				break
			}
		}
		if !isPipelineTerminal(status) {
			continue
		}
		s.finishDeployWatch(ctx, host, requestID, workspaceID, project, status)
		return
	}
}

// finishDeployWatch applies the terminal transition for a watched pipeline.
func (s *Server) finishDeployWatch(ctx context.Context, host *codehost.GitLabHost, requestID, workspaceID, project, status string) {
	if status == "success" {
		p, err := s.st.Project(project)
		if err != nil {
			log.Printf("[deploy] %s: read project after pipeline success: %v", project, err)
		}
		health := map[string]any{"status": "unknown", "reason": "deploy host not configured"}
		if p != nil {
			if url, ok := deployedAppHealthURL(p.DeployPort); ok {
				client := &http.Client{Timeout: 3 * time.Second}
				start := time.Now()
				resp, probeErr := client.Get(url)
				if probeErr != nil {
					health = map[string]any{"status": "down", "reason": probeErr.Error()}
				} else {
					latency := time.Since(start).Milliseconds()
					defer resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						health = map[string]any{"status": "up", "latencyMs": latency}
					} else {
						health = map[string]any{"status": "down", "httpStatus": resp.StatusCode, "latencyMs": latency}
					}
				}
			}
		}
		if err := s.controlDB.SetDeployRequestHealth(workspaceID, requestID, health); err != nil {
			log.Printf("[deploy] %s: write health snapshot for %s failed: %v", project, requestID, err)
		}
		moved, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, requestID, "deploying", "success")
		if err != nil || !moved {
			log.Printf("[deploy] %s: CAS deploying→success for %s failed (moved=%v err=%v)", project, requestID, moved, err)
		}
		return
	}
	// failed / canceled / skipped: any non-success terminal state fails the
	// request.
	moved, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, requestID, "deploying", "failed")
	if err != nil || !moved {
		log.Printf("[deploy] %s: CAS deploying→failed for %s failed (moved=%v err=%v)", project, requestID, moved, err)
	}
}

// recoverActiveDeployRequests is the startup self-healing sweep (mirrors
// recoverActiveWorkflowRuns): 3s after boot, every request stuck in
// 'deploying' is reconciled against GitLab — terminal pipeline drives the
// CAS, an unknown pipeline older than 2h fails the request, anything else
// keeps deploying (a running pipeline is still live). All errors are logged,
// never panicked.
func (s *Server) recoverActiveDeployRequests() {
	if s == nil || s.controlDB == nil {
		return
	}
	time.Sleep(3 * time.Second)

	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[deploy-recovery] panicked: %v", rec)
		}
	}()
	active, err := s.controlDB.ListDeployingDeployRequests()
	if err != nil {
		log.Printf("[deploy-recovery] list deploying requests: %v", err)
		return
	}
	if len(active) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, req := range active {
		s.recoverSingleDeployRequest(ctx, req)
	}
}

// recoverSingleDeployRequest reconciles one 'deploying' request against
// GitLab. Panics are contained per-request so one bad row cannot abort the
// sweep.
func (s *Server) recoverSingleDeployRequest(ctx context.Context, req controldb.DeployRequest) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[deploy-recovery] request %s panicked: %v", req.ID, rec)
		}
	}()
	project := req.ProjectID
	host, binding, err := s.deployGitLabHostFor(ctx, project)
	if err != nil {
		log.Printf("[deploy-recovery] %s: resolve gitlab host for %s failed: %v", project, req.ID, err)
		return
	}
	status, found := s.lookupDeployPipelineStatus(ctx, host, binding.RemoteProjectID, req)
	if !found {
		if deployRequestAge(req.CreatedAt) > 2*time.Hour {
			log.Printf("[deploy-recovery] %s: no pipeline found for %s after 2h, failing", project, req.ID)
			s.finishDeployWatch(ctx, host, req.ID, req.WorkspaceID, project, "failed")
		}
		return
	}
	if !isPipelineTerminal(status) {
		return
	}
	log.Printf("[deploy-recovery] %s: request %s pipeline terminal (%s)", project, req.ID, status)
	s.finishDeployWatch(ctx, host, req.ID, req.WorkspaceID, project, status)
}

// lookupDeployPipelineStatus finds the terminal status of the recorded
// pipeline: pipelines for the request's SHA, matched by pipeline id.
func (s *Server) lookupDeployPipelineStatus(ctx context.Context, host *codehost.GitLabHost, remoteProjectID string, req controldb.DeployRequest) (status string, found bool) {
	if req.PipelineID <= 0 || strings.TrimSpace(req.SHA) == "" {
		return "", false
	}
	pipelines, err := host.PipelinesForSHA(ctx, remoteProjectID, req.SHA)
	if err != nil {
		log.Printf("[deploy-recovery] %s: list pipelines for sha %s failed: %v", req.ProjectID, req.SHA, err)
		return "", false
	}
	for _, pipe := range pipelines {
		if pipe.ID == req.PipelineID {
			return strings.TrimSpace(pipe.Status), true
		}
	}
	return "", false
}

// deployRequestAge parses an RFC3339 created_at; unparseable timestamps read
// as zero age (never auto-fail on malformed data).
func deployRequestAge(createdAt string) time.Duration {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
	if err != nil {
		return 0
	}
	return time.Since(t)
}

func isInflightDeployStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "pending_approval", "approved", "deploying":
		return true
	}
	return false
}

func isTerminalDeployStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "success", "failed", "cancelled", "rejected":
		return true
	}
	return false
}

func shortDeploySHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// latestDeploySHA re-reads the request's SHA for pipeline lookups (the
// watcher closure only carries the id; the SHA lives in the ledger).
func (s *Server) latestDeploySHA(requestID, workspaceID string) string {
	req, _, err := s.controlDB.DeployRequestFor(workspaceID, requestID)
	if err != nil || req == nil {
		return ""
	}
	return req.SHA
}

// handleGetDeployPreviewState serves GET .../deploy/preview-state: the
// lightweight {lastDeployed, inflight} pair used by surfaces that only need
// deployment state (e.g. preview banners), without health probes or
// pipeline fan-out.
func (s *Server) handleGetDeployPreviewState(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}
	requests, err := s.controlDB.ListDeployRequests(controldb.DeployRequestFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
		Limit:       10,
	})
	if err != nil {
		s.serverError(w, fmt.Errorf("list deploy requests: %w", err))
		return
	}
	var lastDeployed, inflight *controldb.DeployRequest
	for i := range requests {
		status := requests[i].Status
		if isInflightDeployStatus(status) && inflight == nil {
			inflight = &requests[i]
		}
		if isTerminalDeployStatus(status) && lastDeployed == nil {
			lastDeployed = &requests[i]
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"lastDeployed": lastDeployed,
		"inflight":     inflight,
	})
}

// handleListDeployRequests serves GET .../deploy/requests: the deploy ledger
// rows the console page renders (newest first). Read-only, project access
// gated; the 50-row cap bounds the table without pagination plumbing.
func (s *Server) handleListDeployRequests(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}
	requests, err := s.controlDB.ListDeployRequests(controldb.DeployRequestFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
		Limit:       50,
	})
	if err != nil {
		s.serverError(w, fmt.Errorf("list deploy requests: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"requests": requests})
}

// postDeployApprovalCard posts the pending-approval card into the project's
// Mattermost channel (batch 4). Call-and-forget: card failures are logged and
// never block the deploy request lifecycle.
func (s *Server) postDeployApprovalCard(req *controldb.DeployRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.PostDeployApprovalCard(ctx, req); err != nil {
		log.Printf("[deploy] %s: post approval card failed: %v", req.ID, err)
	}
}

// currentUserName is the audit/created_by actor for deploy requests.
func (s *Server) currentUserName(r *http.Request) string {
	if cur := s.currentUser(r); cur != nil && strings.TrimSpace(cur.Username) != "" {
		return cur.Username
	}
	return "system"
}

func newDeployRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dep-%d", time.Now().UnixNano())
	}
	return "dep-" + hex.EncodeToString(b[:])
}

func nowUTCAPI() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// isDeployInflightConflict reports whether an InsertDeployRequest failure is
// the partial unique index uq_deploy_requests_inflight rejecting a second
// concurrent in-flight request for the same project.
func isDeployInflightConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") || strings.Contains(msg, "constraint failed: unique")
}
