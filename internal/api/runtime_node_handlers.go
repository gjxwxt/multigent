package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runner"
	"github.com/multigent/multigent/internal/runtimeexec"
	"github.com/multigent/multigent/internal/sandbox"
	"github.com/multigent/multigent/internal/store"
)

const ctxRuntimeNodeKey contextKey = "runtime-node"
const runtimeNodeOnlineWindow = 2 * time.Minute

type runtimeNodePrincipal struct {
	Token controldb.RuntimeNodeToken
	Node  controldb.RuntimeNode
}

type createRuntimeNodeTokenRequest struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	ExpiresIn     string `json:"expiresIn"`
	RuntimeNodeID string `json:"runtimeNodeId"`
}

type runtimeNodeRegisterRequest struct {
	OS               string         `json:"os"`
	Arch             string         `json:"arch"`
	Hostname         string         `json:"hostname"`
	Version          string         `json:"version"`
	Capabilities     map[string]any `json:"capabilities"`
	CapabilitiesHash string         `json:"capabilitiesHash"`
	LastError        string         `json:"lastError"`
}

type runtimeNodeHeartbeatRequest struct {
	Status       string         `json:"status"`
	OS           string         `json:"os"`
	Arch         string         `json:"arch"`
	Hostname     string         `json:"hostname"`
	Version      string         `json:"version"`
	Capabilities map[string]any `json:"capabilities"`
	LastError    string         `json:"lastError"`
}

type runtimeRunClaimRequest struct {
	Capacity         int      `json:"capacity"`
	CapabilitiesHash string   `json:"capabilitiesHash"`
	BusyAgents       []string `json:"busyAgents,omitempty"`
}

type runtimeRunEventRequest struct {
	Sequence int64          `json:"sequence"`
	Type     string         `json:"type"`
	Payload  map[string]any `json:"payload"`
}

type runtimeRunLeaseRequest struct {
	LeaseSeconds    int `json:"leaseSeconds"`
	LeaseGeneration int `json:"leaseGeneration"`
}

type runtimeRunFinishRequest struct {
	Result          map[string]any `json:"result"`
	ErrorCode       string         `json:"errorCode"`
	ErrorMessage    string         `json:"errorMessage"`
	LeaseGeneration int            `json:"leaseGeneration"`
}

const runtimeWorkflowStepNotCompletedError = "workflow step was not completed by the agent; use `mga task step done --id <id>` with every required output field, or `mga task step done --id <id> --status failed --error <reason>` if the step cannot be completed"

type createRuntimeExecRunRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"sessionId"`
}

func (s *Server) handleRuntimeNodes(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !s.canAdminWorkspace(r, workspaceID) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeWorkspaceAdminRequired, "workspace admin access required")
		return
	}
	nodes, err := s.controlDB.ListRuntimeNodes(workspaceID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	resp := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		resp = append(resp, runtimeNodeResponse(node))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"nodes": resp})
}

func (s *Server) hasOnlineRuntimeNode(workspaceID string) bool {
	if s == nil || s.controlDB == nil || strings.TrimSpace(workspaceID) == "" {
		return false
	}
	nodes, err := s.controlDB.ListRuntimeNodes(workspaceID)
	if err != nil {
		return false
	}
	for _, node := range nodes {
		if runtimeNodeIsOnline(node) {
			return true
		}
	}
	return false
}

func (s *Server) assignedRuntimeNode(workspaceID string, meta *entity.AgentMeta) (controldb.RuntimeNode, bool) {
	if s == nil || s.controlDB == nil || meta == nil || strings.TrimSpace(workspaceID) == "" {
		return controldb.RuntimeNode{}, false
	}
	nodeID := strings.TrimSpace(meta.RuntimeNodeID)
	if nodeID == "" {
		return controldb.RuntimeNode{}, false
	}
	node, found, err := s.controlDB.RuntimeNodeByID(workspaceID, nodeID)
	if err != nil || !found || !runtimeNodeIsOnline(node) {
		return controldb.RuntimeNode{}, false
	}
	return node, true
}

func (s *Server) usesAssignedRuntimeNode(workspaceID string, meta *entity.AgentMeta) bool {
	_, ok := s.assignedRuntimeNode(workspaceID, meta)
	return ok
}

func (s *Server) runtimeReadinessForExecution(workspaceID string, meta *entity.AgentMeta) runtimeReadinessResponse {
	return s.runtimeReadinessForRuntimeNode(workspaceID, meta, runtimeReadinessOptions{
		ProbeRuntime:   true,
		CheckContainer: true,
		AgentDir:       s.st.AgentDir(meta.Project, meta.Name),
	})
}

func (s *Server) runtimeReadinessForProjectList(workspaceID string, meta *entity.AgentMeta) runtimeReadinessResponse {
	return s.runtimeReadinessForRuntimeNode(workspaceID, meta, runtimeReadinessOptions{
		ProbeRuntime:   false,
		CheckContainer: false,
	})
}

func (s *Server) runtimeReadinessForRuntimeNode(workspaceID string, meta *entity.AgentMeta, opts runtimeReadinessOptions) runtimeReadinessResponse {
	requireNode := runtimeNodeRequired()
	nodeID := ""
	if meta != nil {
		nodeID = strings.TrimSpace(meta.RuntimeNodeID)
	}
	if requireNode && nodeID == "" {
		return runtimeNodeBlockingReadiness("Runtime node is required before this agent can run.", "This workspace is configured for customer-provided Runtime Nodes. Add a Runtime Node in Settings, then bind this agent to that node.")
	}
	readiness := buildRuntimeReadinessWithOptions(meta, opts)
	if nodeID == "" {
		return readiness
	}
	node, found, err := s.controlDB.RuntimeNodeByID(workspaceID, nodeID)
	if err != nil || !found || !runtimeNodeIsOnline(node) {
		statusDetail := "Assigned runtime node is not online."
		if found {
			statusDetail = fmt.Sprintf("Assigned runtime node %q is %s.", firstNonEmpty(node.Name, node.ID), runtimeNodeEffectiveStatus(node))
			if strings.TrimSpace(node.LastError) != "" {
				statusDetail += " Last error: " + strings.TrimSpace(node.LastError)
			}
		}
		if requireNode {
			return runtimeNodeBlockingReadiness("Assigned runtime node is not ready.", statusDetail)
		}
		checks := append([]setupCheck(nil), readiness.Checks...)
		checks = append(checks, runtimeNodeBlockingCheck(statusDetail))
		return runtimeReadinessResponse{Ready: false, Blocking: true, Summary: "Assigned runtime node is not ready.", Checks: checks}
	}
	provider := entity.SandboxDocker
	if meta.Sandbox != nil {
		provider = meta.Sandbox.Provider
	}
	if provider == entity.SandboxNone && entity.NormaliseModel(meta.Model) == entity.ModelClaudeCode && runtimeNodeDirectRunsAsRoot(node) {
		checks := append([]setupCheck(nil), readiness.Checks...)
		checks = append(checks, setupCheck{
			Key:      "runtime_node_claudecode_root",
			Label:    "Runtime Node",
			Status:   "error",
			Detail:   "Assigned runtime node is running as root. Claude Code refuses bypassPermissions under root/sudo privileges for direct host execution.",
			Action:   "Restart the Runtime Node as a normal user, or switch this agent to Docker sandbox execution.",
			Blocking: true,
		})
		return runtimeReadinessResponse{
			Ready:    false,
			Blocking: true,
			Summary:  "Assigned Runtime Node cannot run Claude Code direct execution as root.",
			Checks:   checks,
		}
	}
	if !readiness.Blocking {
		return readiness
	}
	filtered := readiness
	filtered.Checks = append([]setupCheck(nil), readiness.Checks...)
	for i := range filtered.Checks {
		switch filtered.Checks[i].Key {
		case "sandbox", "cli", "docker", "runtime_image", "runtime_container":
			if filtered.Checks[i].Status == "error" {
				filtered.Checks[i].Status = "warning"
				filtered.Checks[i].Detail = firstNonEmpty(filtered.Checks[i].Detail, "Local runtime is not ready, but this agent is assigned to an online Runtime Node.")
				filtered.Checks[i].Blocking = false
			}
		}
	}
	blocking := false
	warnings := 0
	for _, check := range filtered.Checks {
		if check.Blocking || check.Status == "error" {
			blocking = true
		}
		if check.Status == "warning" {
			warnings++
		}
	}
	filtered.Blocking = blocking
	filtered.Ready = !blocking
	if blocking {
		filtered.Summary = "Runtime is not ready. Resolve blocking checks before running this agent."
	} else if warnings > 0 {
		filtered.Summary = "Assigned Runtime Node is available. Local runtime preparation warnings will not block this run."
	} else {
		filtered.Summary = "Runtime is ready."
	}
	return filtered
}

func runtimeNodeBlockingReadiness(summary, detail string) runtimeReadinessResponse {
	return runtimeReadinessResponse{
		Ready:    false,
		Blocking: true,
		Summary:  summary,
		Checks:   []setupCheck{runtimeNodeBlockingCheck(detail)},
	}
}

func runtimeNodeBlockingCheck(detail string) setupCheck {
	return setupCheck{
		Key:      "runtime_node",
		Label:    "Runtime Node",
		Status:   "error",
		Detail:   detail,
		Action:   "Open Settings → Runtime Nodes, add a node, run the join command on your machine, then select it in this agent's advanced settings.",
		Blocking: true,
	}
}

func runtimeNodeRequired() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("MULTIGENT_REQUIRE_RUNTIME_NODE")))
	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func runtimeNodeDirectRunsAsRoot(node controldb.RuntimeNode) bool {
	var caps map[string]any
	if err := json.Unmarshal([]byte(node.CapabilitiesJSON), &caps); err != nil {
		return false
	}
	direct, ok := caps["direct"].(map[string]any)
	if !ok {
		return false
	}
	isRoot, _ := direct["isRoot"].(bool)
	return isRoot
}

func decodeRuntimeRunResult(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return out
	}
	for k, v := range generic {
		switch typed := v.(type) {
		case string:
			out[k] = typed
		case float64, bool:
			out[k] = strings.TrimSpace(strings.Trim(fmt.Sprint(typed), "\""))
		default:
			if body, err := json.Marshal(typed); err == nil {
				out[k] = string(body)
			}
		}
	}
	return out
}

func (s *Server) handleCreateRuntimeExecRun(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	agent := strings.TrimSpace(r.PathValue("agent"))
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.agentExistsInProject(project, agent) {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAgentNotFound, "agent not found")
		return
	}
	if !s.canOperateAgent(r, project, agent) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeAgentOperatorRequired, "agent operator access required")
		return
	}
	var body createRuntimeExecRunRequest
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	message := strings.TrimSpace(body.Message)
	if message == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "message is required")
		return
	}
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !s.usesAssignedRuntimeNode(workspaceID, meta) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeRuntimeNotReady, "agent is not assigned to an online runtime node")
		return
	}
	run, err := s.enqueueRuntimeExecRun(workspaceID, project, agent, message, strings.TrimSpace(body.SessionID), externalServerURL(r), requestUsername(r))
	if err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"run": runtimeRunResponse(run)})
}

func (s *Server) enqueueRuntimeExecRun(workspaceID, project, agent, prompt, sessionID, serverURL, actor string) (controldb.RuntimeRun, error) {
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil {
		return controldb.RuntimeRun{}, err
	}
	workerID, membershipID := s.agentWorkerContextForProjectAgent(workspaceID, project, agent)
	runID := newRuntimeID("rtrun")
	token := s.issueAgentRuntimeToken(runtimeAgentTokenPayload{
		Type:                "agent_runtime",
		WorkspaceID:         workspaceID,
		Project:             project,
		Agent:               agent,
		AgentWorkerID:       workerID,
		ProjectMembershipID: membershipID,
		RunID:               runID,
		Capabilities:        defaultRuntimeCapabilities(),
	}, 6*time.Hour)
	spec := runtimeexec.Spec{
		Kind:        runtimeexec.KindExecPrompt,
		WorkspaceID: workspaceID,
		ProjectID:   project,
		AgentID:     agent,
		SessionID:   sessionID,
		Prompt:      prompt,
		Agent:       *meta,
		ProviderEnv: s.runtimeProviderEnvForAgent(workspaceID, project, agent, meta),
		RuntimeControlEnv: map[string]string{
			"MULTIGENT_API_URL":               strings.TrimRight(serverURL, "/"),
			"MULTIGENT_AGENT_TOKEN":           token,
			"MULTIGENT_RUN_ID":                runID,
			"MULTIGENT_WORKSPACE_ID":          workspaceID,
			"MULTIGENT_AGENT_WORKER_ID":       workerID,
			"MULTIGENT_PROJECT_MEMBERSHIP_ID": membershipID,
		},
	}
	specBody, err := json.Marshal(spec)
	if err != nil {
		return controldb.RuntimeRun{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	run := controldb.RuntimeRun{
		ID:                   runID,
		WorkspaceID:          workspaceID,
		AgentWorkerID:        workerID,
		ProjectMembershipID:  membershipID,
		ProjectID:            project,
		AgentID:              agent,
		DesiredRuntimeNodeID: strings.TrimSpace(meta.RuntimeNodeID),
		Status:               "queued",
		Priority:             2,
		SpecJSON:             string(specBody),
		ResultJSON:           "{}",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		return controldb.RuntimeRun{}, err
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "runtime_run.enqueue",
		ResourceType: "runtime_run",
		ResourceID:   run.ID,
		Summary:      "Runtime run queued",
		After: map[string]any{
			"project": project,
			"agent":   agent,
			"kind":    spec.Kind,
			"actor":   actor,
		},
	})
	return run, nil
}

func (s *Server) markForkSessionRunQueued(workspaceID, forkSessionID, workerID, runID string, task *entity.Task, projectID, membershipID string) {
	if s == nil || s.controlDB == nil || strings.TrimSpace(forkSessionID) == "" {
		return
	}
	session, found, err := s.controlDB.AgentSessionByID(workspaceID, forkSessionID)
	if err != nil || !found {
		return
	}
	if strings.TrimSpace(workerID) != "" && strings.TrimSpace(session.AgentWorkerID) != strings.TrimSpace(workerID) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	session.Status = "running"
	session.LastRunID = strings.TrimSpace(runID)
	if task != nil {
		if strings.TrimSpace(session.TaskID) == "" {
			session.TaskID = strings.TrimSpace(task.ID)
		}
		if strings.TrimSpace(session.WorkflowInstanceID) == "" {
			session.WorkflowInstanceID = strings.TrimSpace(task.Vars[workflowRunIDVar])
		}
	}
	if strings.TrimSpace(session.ProjectID) == "" {
		session.ProjectID = strings.TrimSpace(projectID)
	}
	if strings.TrimSpace(session.ProjectMembershipID) == "" {
		session.ProjectMembershipID = strings.TrimSpace(membershipID)
	}
	session.UpdatedAt = now
	session.LastActivityAt = now
	_ = s.controlDB.UpsertAgentSession(session)
}

func (s *Server) enqueueRuntimeTaskRun(workspaceID, project, agent string, task *entity.Task, sessionID, serverURL, actor string) (controldb.RuntimeRun, error) {
	if task == nil {
		return controldb.RuntimeRun{}, fmt.Errorf("task is required")
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil {
		return controldb.RuntimeRun{}, err
	}
	workerID, membershipID := s.agentWorkerContextForProjectAgent(workspaceID, project, agent)
	forkSessionID := runtimeForkSessionIDFromTask(task)
	runID := newRuntimeID("rtrun")
	token := s.issueAgentRuntimeToken(runtimeAgentTokenPayload{
		Type:                "agent_runtime",
		WorkspaceID:         workspaceID,
		Project:             project,
		Agent:               agent,
		AgentWorkerID:       workerID,
		ProjectMembershipID: membershipID,
		RunID:               runID,
		Capabilities:        defaultRuntimeCapabilities(),
	}, 6*time.Hour)
	preparedPrompt := runner.New(s.root, s.ts, s.st).BuildTaskPrompt(project, agent, task)
	runtimeControlEnv := map[string]string{
		"MULTIGENT_API_URL":               strings.TrimRight(serverURL, "/"),
		"MULTIGENT_AGENT_TOKEN":           token,
		"MULTIGENT_RUN_ID":                runID,
		"MULTIGENT_TASK_ID":               task.ID,
		"MULTIGENT_WORKSPACE_ID":          workspaceID,
		"MULTIGENT_AGENT_WORKER_ID":       workerID,
		"MULTIGENT_PROJECT_MEMBERSHIP_ID": membershipID,
	}
	if forkSessionID != "" {
		runtimeControlEnv["MULTIGENT_FORK_SESSION_ID"] = forkSessionID
	}
	for k, v := range runtimeTaskControlEnv(task) {
		runtimeControlEnv[k] = v
	}
	spec := runtimeexec.Spec{
		Kind:              runtimeexec.KindTask,
		WorkspaceID:       workspaceID,
		ProjectID:         project,
		AgentID:           agent,
		TaskID:            task.ID,
		ForkSessionID:     forkSessionID,
		SessionID:         sessionID,
		Prompt:            preparedPrompt,
		Agent:             *meta,
		ProviderEnv:       s.runtimeProviderEnvForAgent(workspaceID, project, agent, meta),
		RuntimeControlEnv: runtimeControlEnv,
	}
	specBody, err := json.Marshal(spec)
	if err != nil {
		return controldb.RuntimeRun{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	runKey, keyErr := s.runtimeRunKeyForTask(workspaceID, project, agent, task)
	if keyErr != nil {
		return controldb.RuntimeRun{}, keyErr
	}
	run := controldb.RuntimeRun{
		ID:                   runID,
		WorkspaceID:          workspaceID,
		AgentWorkerID:        workerID,
		ProjectMembershipID:  membershipID,
		ProjectID:            project,
		AgentID:              agent,
		TaskID:               task.ID,
		ForkSessionID:        forkSessionID,
		DesiredRuntimeNodeID: strings.TrimSpace(meta.RuntimeNodeID),
		// Q1 stamp-race contract: task runs land as 'preparing' (unclaimable)
		// and are promoted to 'queued' only after the task token stamp
		// succeeds below. This run's own token is issued here and does not
		// depend on the task fence, so the initial status is safe.
		Status:     "preparing",
		Priority:   task.Priority,
		SpecJSON:   string(specBody),
		ResultJSON: "{}",
		CreatedAt:  now,
		UpdatedAt:  now,
		RunKey:     runKey,
	}
	// Idempotent enqueue: a concurrent dispatch of the same intent (double
	// click, scheduler tick racing a manual start, recovery scan racing a
	// trigger) returns the already-queued run instead of a duplicate.
	stored, inserted, err := s.controlDB.UpsertRuntimeRunIdempotent(run)
	if err != nil {
		return controldb.RuntimeRun{}, err
	}
	if !inserted {
		// Converged on an existing active (preparing/queued/running) run for
		// this intent — return it unchanged. Its own enqueue owns the stamp
		// and promote; stamping again here would race that enqueue's token
		// clear-and-restamp cycle for no benefit.
		return stored, nil
	}
	run = stored
	// Q1 stamp-race contract: the run was inserted as 'preparing' — visible
	// to dedupe (ActiveRuntimeRunByKey includes preparing) but invisible to
	// ClaimRuntimeRun (which selects status='queued' only). Stamp the task
	// NOW, while no node can claim the run; the stamp is the run's dispatch
	// ticket (Q0 D5 + GPT 收口 6-1). Only after the stamp succeeds does the
	// promote below make the run claimable, so the old window — queued run
	// claimed by a node before its token landed, executing work whose finish
	// the fence would drop — is closed by construction.
	if err := s.setTaskActiveRuntimeRun(project, agent, task.ID, run.ID); err != nil {
		slog.Warn("runtime task token stamp failed; failing the preparing run so it never dispatches", "run", run.ID, "task", task.ID, "error", err)
		if failed, _, ferr := s.controlDB.FailQueuedRuntimeRun(workspaceID, run.ID, "token_stamp_failed", "task execution token stamp failed; run never dispatched"); ferr == nil && failed.ID != "" {
			run = failed
		} else if ferr != nil {
			run = failed
		} else {
			slog.Warn("runtime run stamp-failure cleanup failed; stale-token sweep will reconcile", "run", run.ID, "error", ferr)
		}
		return run, fmt.Errorf("queue task run failed: %w", err)
	}
	_, promoted, perr := s.controlDB.PromotePreparingRuntimeRun(workspaceID, run.ID)
	if perr != nil && isPromoteKeyCollision(perr) {
		// Another active run with the same run_key already exists (the
		// unique index rejected the promote) — the textbook idempotent-join
		// case: our preparing row is redundant. Converge on the winner and
		// retire ours so the key stays with the surviving run.
		// The winner lookup must exclude our own row IN SQL: created_at has
		// second precision (time.RFC3339) and ids are random hex, so a
		// same-second duplicate enqueue ties on created_at and the random
		// tie-break can order OUR preparing row first — a caller-side filter
		// would spin forever while the real winner sits behind it.
		if winner, found, gerr := s.controlDB.OtherActiveRuntimeRunByKey(workspaceID, run.RunKey, run.ID); gerr == nil && found {
			slog.Info("runtime run promote collided with an existing active run for the same intent; joining it", "run", run.ID, "winner", winner.ID, "task", task.ID)
			// The stamp above moved this task's token to OUR run — point it
			// back at the winner BEFORE retiring ours, or the fence would
			// drop the winner's finish.
			if restampErr := s.setTaskActiveRuntimeRun(project, agent, task.ID, winner.ID); restampErr != nil {
				// Fail-closed: without a correct token the winner's finish
				// would be dropped by the fence, so retire our duplicate and
				// surface the failure — the caller may re-enqueue cleanly.
				_, _, _ = s.controlDB.FailQueuedRuntimeRun(workspaceID, winner.ID, "token_repoint_failed", "enqueue join could not restamp the task token to the surviving run")
				return winner, fmt.Errorf("queue task run failed: token repoint to winning run: %w", restampErr)
			}
			// Retire the duplicate outright: a preparing row was never
			// claimable, so deleting it keeps one row per dispatch intent
			// (a failed row would linger in listings and look like real
			// dispatch history).
			if _, derr := s.controlDB.DeleteUnclaimedRuntimeRun(workspaceID, run.ID); derr != nil {
				slog.Warn("superseded preparing run delete failed; it stays failed-and-unclaimable", "run", run.ID, "error", derr)
				_, _, _ = s.controlDB.FailQueuedRuntimeRun(workspaceID, run.ID, "superseded", "another active run for the same intent already exists")
			}
			return winner, nil
		}
	}
	if perr != nil || !promoted {
		// The stamp landed but the promote failed — the run is stranded in
		// preparing, where it is harmless (unclaimable, invisible to nodes).
		// Fail it so the run_key frees up for the next dispatch attempt.
		if perr != nil {
			slog.Warn("runtime run promote failed after stamp; failing the preparing run", "run", run.ID, "task", task.ID, "error", perr)
			if failed, _, ferr := s.controlDB.FailQueuedRuntimeRun(workspaceID, run.ID, "promote_failed", "run promote to queued failed after token stamp"); ferr == nil && failed.ID != "" {
				run = failed
			}
			return run, fmt.Errorf("queue task run failed: %w", perr)
		}
		return run, fmt.Errorf("queue task run failed: run %s no longer preparing (status=%s)", run.ID, run.Status)
	}
	run.Status = "queued"
	s.markForkSessionRunQueued(workspaceID, forkSessionID, workerID, run.ID, task, project, membershipID)
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "runtime_run.enqueue",
		ResourceType: "runtime_run",
		ResourceID:   run.ID,
		Summary:      "Runtime task run queued",
		After: map[string]any{
			"project": project,
			"agent":   agent,
			"taskId":  task.ID,
			"session": forkSessionID,
			"actor":   actor,
		},
	})
	return run, nil
}

// isPromoteKeyCollision reports whether err is the partial unique index
// (workspace_id, run_key) rejecting a promote because another ACTIVE run
// with the same key already exists — the idempotent-join case, not a fault.
func isPromoteKeyCollision(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

func runtimeForkSessionIDFromTask(task *entity.Task) string {
	if task == nil || len(task.Vars) == 0 {
		return ""
	}
	for _, key := range []string{"MULTIGENT_FORK_SESSION_ID", "fork_session_id", "forkSessionId"} {
		if value := strings.TrimSpace(task.Vars[key]); value != "" {
			return value
		}
	}
	return ""
}

// runtimeTaskControlEnv forwards scheduler-controlled task vars into the run
// spec. The wakeup worktree keys remount the run sandbox onto a host
// directory, so they are only honoured on genuine wakeup tasks — even if a
// vars map slipped past sanitizeTaskVars, a normal task can never carry them.
func runtimeTaskControlEnv(task *entity.Task) map[string]string {
	if task == nil || len(task.Vars) == 0 {
		return nil
	}
	isWakeup := strings.EqualFold(string(task.Type), "wakeup")
	out := map[string]string{}
	for k, v := range task.Vars {
		k = strings.TrimSpace(k)
		if !isRuntimeTaskControlEnvKey(k) || strings.TrimSpace(v) == "" {
			continue
		}
		if !isWakeup && (k == "MULTIGENT_WAKEUP_WORKTREE_DIR" || k == "MULTIGENT_WAKEUP_BRANCH") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isRuntimeTaskControlEnvKey(key string) bool {
	switch key {
	case "MULTIGENT_DELEGATION_TOKEN", "MULTIGENT_DELEGATION_EXPIRES_AT", "MULTIGENT_DELEGATION_INTERACTION_ID", "MULTIGENT_DELEGATION_TOKENS_JSON", "MULTIGENT_DELEGATION_EXPIRES_AT_JSON", "MULTIGENT_FORK_SESSION_ID", "MULTIGENT_WAKEUP_WORKTREE_DIR", "MULTIGENT_WAKEUP_BRANCH":
		return true
	default:
		return false
	}
}

func (s *Server) runtimeProviderEnvForAgent(workspaceID, project, agent string, meta *entity.AgentMeta) map[string]string {
	if meta == nil {
		return nil
	}
	env := map[string]string{
		"MULTIGENT":         "1",
		"MULTIGENT_PROJECT": meta.Project,
		"MULTIGENT_AGENT":   meta.Name,
		"MULTIGENT_TEAM":    meta.Team,
		"MULTIGENT_ROLE":    meta.Role,
		"MULTIGENT_MODEL":   string(meta.Model),
	}
	workerTargets := []string{}
	if workerID := s.agentWorkerIDForProjectAgent(workspaceID, project, agent); strings.TrimSpace(workerID) != "" {
		workerTargets = append(workerTargets, connectionAgentWorkerTargetID(workerID))
	}
	if wsEnv, err := store.NewEnvVarStore(s.root).ResolveEnvForAgentTargets(project, agent, workerTargets); err == nil {
		for k, v := range wsEnv {
			env[k] = v
		}
	}
	if strings.TrimSpace(meta.Provider) != "" {
		if provEnv, err := store.NewProviderStoreWithDB(s.root, s.controlDB).ResolveEnvForModel(meta.Provider, meta.Model); err == nil {
			for k, v := range provEnv {
				env[k] = v
			}
		}
	}
	for k, v := range meta.Env {
		env[k] = v
	}
	for k, v := range runtimeModelEnvForSpec(meta.Model, meta.RuntimeModel) {
		env[k] = v
	}
	return env
}

func runtimeModelEnvForSpec(model entity.AgentModel, runtimeModel string) map[string]string {
	runtimeModel = strings.TrimSpace(runtimeModel)
	if runtimeModel == "" {
		return nil
	}
	switch entity.NormaliseModel(model) {
	case entity.ModelClaudeCode:
		return map[string]string{"ANTHROPIC_MODEL": runtimeModel, "CLAUDE_MODEL": runtimeModel}
	case entity.ModelCodex:
		return map[string]string{"OPENAI_MODEL": runtimeModel, "CODEX_MODEL": runtimeModel}
	case entity.ModelGemini:
		return map[string]string{"GEMINI_MODEL": runtimeModel, "GOOGLE_MODEL": runtimeModel}
	case entity.ModelCursor:
		return map[string]string{"CURSOR_MODEL": runtimeModel}
	case entity.ModelOpenCode:
		return map[string]string{"OPENAI_MODEL": runtimeModel}
	default:
		return map[string]string{"MULTIGENT_RUNTIME_MODEL": runtimeModel}
	}
}

func (s *Server) handleCreateRuntimeNodeJoinToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !s.canAdminWorkspace(r, workspaceID) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeWorkspaceAdminRequired, "workspace admin access required")
		return
	}
	var body createRuntimeNodeTokenRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	now := time.Now().UTC()
	nodeID := strings.TrimSpace(body.RuntimeNodeID)
	var node controldb.RuntimeNode
	if nodeID != "" {
		foundNode, found, err := s.controlDB.RuntimeNodeByID(workspaceID, nodeID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if !found {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime node not found")
			return
		}
		node = foundNode
		if name := strings.TrimSpace(body.Name); name != "" && name != node.Name {
			node.Name = name
			node.UpdatedAt = now.Format(time.RFC3339)
			if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
				s.serverError(w, err)
				return
			}
		}
	} else {
		if !s.checkRuntimeNodeEntitlement(w, r, workspaceID, 1) {
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = "Runtime Node"
		}
		kind := strings.TrimSpace(body.Kind)
		if kind == "" {
			kind = "personal_computer"
		}
		node = controldb.RuntimeNode{
			ID:               newRuntimeID("rtn"),
			WorkspaceID:      workspaceID,
			Name:             name,
			Kind:             kind,
			Status:           "pending",
			CapabilitiesJSON: "{}",
			PolicyJSON:       "{}",
			CreatedByUserID:  requestUsername(r),
			CreatedAt:        now.Format(time.RFC3339),
			UpdatedAt:        now.Format(time.RFC3339),
		}
		if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
			s.serverError(w, err)
			return
		}
	}
	rawToken := "mgrt_" + randomRuntimeHex(32)
	ttl := parseRuntimeTTL(body.ExpiresIn, 30*time.Minute)
	token := controldb.RuntimeNodeToken{
		ID:            newRuntimeID("rtok"),
		WorkspaceID:   workspaceID,
		RuntimeNodeID: node.ID,
		TokenHash:     hashRuntimeToken(rawToken),
		Name:          "join",
		ScopesJSON:    `["runtime.join","runtime.heartbeat","runtime.claim"]`,
		ExpiresAt:     now.Add(ttl).Format(time.RFC3339),
		CreatedBy:     requestUsername(r),
		CreatedAt:     now.Format(time.RFC3339),
	}
	if err := s.controlDB.CreateRuntimeNodeToken(token); err != nil {
		s.serverError(w, err)
		return
	}
	serverURL := externalServerURL(r)
	installCommand := "multigent runtime join --server " + serverURL + " --token " + rawToken
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "runtime_node.join_token.create",
		ResourceType: "runtime_node",
		ResourceID:   node.ID,
		Summary:      "Runtime node join token created",
		After: map[string]any{
			"name":      node.Name,
			"kind":      node.Kind,
			"expiresAt": token.ExpiresAt,
		},
		Request: r,
	})
	_ = json.NewEncoder(w).Encode(map[string]any{
		"runtimeNode":    runtimeNodeResponse(node),
		"runtimeNodeId":  node.ID,
		"joinToken":      rawToken,
		"expiresAt":      token.ExpiresAt,
		"serverUrl":      serverURL,
		"installCommand": installCommand,
	})
}

func (s *Server) handleRuntimeNode(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !s.canAdminWorkspace(r, workspaceID) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeWorkspaceAdminRequired, "workspace admin access required")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	node, found, err := s.controlDB.RuntimeNodeByID(workspaceID, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime node not found")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"node": runtimeNodeResponse(node)})
}

func (s *Server) handleDisableRuntimeNode(w http.ResponseWriter, r *http.Request) {
	s.setRuntimeNodeStatus(w, r, "disabled")
}

func (s *Server) handleEnableRuntimeNode(w http.ResponseWriter, r *http.Request) {
	s.setRuntimeNodeStatus(w, r, "pending")
}

func (s *Server) setRuntimeNodeStatus(w http.ResponseWriter, r *http.Request, status string) {
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !s.canAdminWorkspace(r, workspaceID) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeWorkspaceAdminRequired, "workspace admin access required")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	node, found, err := s.controlDB.RuntimeNodeByID(workspaceID, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime node not found")
		return
	}
	node.Status = status
	node.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"node": runtimeNodeResponse(node)})
}

func (s *Server) handleDeleteRuntimeNode(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if !s.canAdminWorkspace(r, workspaceID) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeWorkspaceAdminRequired, "workspace admin access required")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if err := s.controlDB.DeleteRuntimeNode(workspaceID, id); err != nil {
		s.serverError(w, err)
		return
	}
	s.forgetRuntimeNodeDriftState(id)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleRuntimeNodeRegister(w http.ResponseWriter, r *http.Request) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	var body runtimeNodeRegisterRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	node := principal.Node
	now := time.Now().UTC().Format(time.RFC3339)
	reportedOS := firstNonEmpty(strings.TrimSpace(body.OS), runtime.GOOS)
	reportedArch := firstNonEmpty(strings.TrimSpace(body.Arch), runtime.GOARCH)
	reportedHostname := strings.TrimSpace(body.Hostname)
	if existing, found := s.findExistingRuntimeNodeByFingerprint(node.WorkspaceID, node.ID, reportedOS, reportedArch, reportedHostname); found {
		principal.Token.RuntimeNodeID = existing.ID
		principal.Token.ExpiresAt = ""
		principal.Token.Name = "runtime"
		if err := s.controlDB.UpdateRuntimeNodeToken(principal.Token); err != nil {
			s.serverError(w, err)
			return
		}
		if err := s.controlDB.DeleteRuntimeNode(node.WorkspaceID, node.ID); err != nil {
			s.serverError(w, err)
			return
		}
		node = existing
	}
	if node.Status != "disabled" {
		node.Status = "registered"
	}
	node.OS = reportedOS
	node.Arch = reportedArch
	node.Hostname = reportedHostname
	node.Version = strings.TrimSpace(body.Version)
	node.CapabilitiesJSON = marshalRuntimeObject(body.Capabilities)
	node.LastSeenAt = now
	node.LastError = strings.TrimSpace(body.LastError)
	node.UpdatedAt = now
	if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
		s.serverError(w, err)
		return
	}
	if principal.Token.ExpiresAt != "" || principal.Token.Name != "runtime" {
		principal.Token.RuntimeNodeID = node.ID
		principal.Token.ExpiresAt = ""
		principal.Token.Name = "runtime"
		if err := s.controlDB.UpdateRuntimeNodeToken(principal.Token); err != nil {
			s.serverError(w, err)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"node": runtimeNodeResponse(node), "status": "registered"})
}

func (s *Server) findExistingRuntimeNodeByFingerprint(workspaceID, currentID, osName, archName, hostname string) (controldb.RuntimeNode, bool) {
	workspaceID = strings.TrimSpace(workspaceID)
	currentID = strings.TrimSpace(currentID)
	hostname = strings.TrimSpace(hostname)
	if workspaceID == "" || hostname == "" {
		return controldb.RuntimeNode{}, false
	}
	nodes, err := s.controlDB.ListRuntimeNodes(workspaceID)
	if err != nil {
		return controldb.RuntimeNode{}, false
	}
	for _, node := range nodes {
		if node.ID == currentID || node.Status == "disabled" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(node.Hostname), hostname) &&
			strings.EqualFold(strings.TrimSpace(node.OS), strings.TrimSpace(osName)) &&
			strings.EqualFold(strings.TrimSpace(node.Arch), strings.TrimSpace(archName)) {
			return node, true
		}
	}
	return controldb.RuntimeNode{}, false
}

func (s *Server) handleRuntimeNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	var body runtimeNodeHeartbeatRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	node := principal.Node
	now := time.Now().UTC().Format(time.RFC3339)
	if node.Status != "disabled" {
		node.Status = firstNonEmpty(strings.TrimSpace(body.Status), "online")
	}
	if strings.TrimSpace(body.OS) != "" {
		node.OS = strings.TrimSpace(body.OS)
	}
	if strings.TrimSpace(body.Arch) != "" {
		node.Arch = strings.TrimSpace(body.Arch)
	}
	if strings.TrimSpace(body.Hostname) != "" {
		node.Hostname = strings.TrimSpace(body.Hostname)
	}
	if strings.TrimSpace(body.Version) != "" {
		node.Version = strings.TrimSpace(body.Version)
	}
	s.warnRuntimeNodeVersionDrift(node)
	if body.Capabilities != nil {
		node.CapabilitiesJSON = marshalRuntimeObject(body.Capabilities)
	}
	node.LastSeenAt = now
	node.LastError = strings.TrimSpace(body.LastError)
	node.UpdatedAt = now
	if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "node": runtimeNodeResponse(node), "serverTime": now})
}

func (s *Server) handleRuntimeNodeClaimRun(w http.ResponseWriter, r *http.Request) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	var body runtimeRunClaimRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	if principal.Node.Status == "disabled" {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden, "runtime node is disabled")
		return
	}
	for attempts := 0; attempts < 10; attempts++ {
		run, found, err := s.controlDB.ClaimRuntimeRun(principal.Node.WorkspaceID, principal.Node.ID, 90, body.BusyAgents)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if !found {
			_ = json.NewEncoder(w).Encode(map[string]any{"run": nil, "retryAfterMs": 3000})
			return
		}
		if s.finishClaimedRunIfTaskAlreadyTerminal(principal.Node.WorkspaceID, principal.Node.ID, &run) {
			continue
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": runtimeRunResponse(run), "retryAfterMs": 0})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"run": nil, "retryAfterMs": 3000})
}

func (s *Server) finishClaimedRunIfTaskAlreadyTerminal(workspaceID, nodeID string, run *controldb.RuntimeRun) bool {
	if s == nil || s.controlDB == nil || s.ts == nil || run == nil || strings.TrimSpace(run.TaskID) == "" {
		return false
	}
	task, err := s.ts.GetTask(run.ProjectID, run.AgentID, run.TaskID)
	if err != nil || task == nil || !task.Status.IsTerminal() {
		return false
	}
	now := time.Now().UTC().Format(time.RFC3339)
	run.Status = "succeeded"
	if task.Status == entity.TaskStatusDoneFailed {
		run.Status = "failed"
	}
	run.RuntimeNodeID = nodeID
	run.ResultJSON = marshalRuntimeObject(map[string]any{
		"status":  string(task.Status),
		"summary": strings.TrimSpace(task.Summary),
		"skipped": true,
		"reason":  "task_already_terminal",
	})
	run.ErrorCode = ""
	run.ErrorMessage = ""
	run.FinishedAt = now
	run.UpdatedAt = now
	if err := s.controlDB.UpsertRuntimeRun(*run); err != nil {
		slog.Warn("runtime terminal task run cleanup failed", "run", run.ID, "task", run.TaskID, "error", err)
		return false
	}
	return true
}

func (s *Server) handleRuntimeNodeRunSpec(w http.ResponseWriter, r *http.Request) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	run, found, err := s.controlDB.RuntimeRunByID(principal.Node.WorkspaceID, runID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || run.RuntimeNodeID != principal.Node.ID {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime run not found")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"run": runtimeRunResponse(run), "spec": json.RawMessage(defaultRawJSON(run.SpecJSON))})
}

func (s *Server) handleRuntimeNodeRunEvent(w http.ResponseWriter, r *http.Request) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	run, found, err := s.controlDB.RuntimeRunByID(principal.Node.WorkspaceID, runID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || run.RuntimeNodeID != principal.Node.ID {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime run not found")
		return
	}
	var body runtimeRunEventRequest
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	if body.Sequence <= 0 {
		body.Sequence = time.Now().UTC().UnixNano()
	}
	if strings.TrimSpace(body.Type) == "" {
		body.Type = "system"
	}
	event := controldb.RuntimeEvent{
		ID:          newRuntimeID("rtev"),
		WorkspaceID: principal.Node.WorkspaceID,
		RunID:       run.ID,
		Sequence:    body.Sequence,
		Type:        strings.TrimSpace(body.Type),
		PayloadJSON: marshalRuntimeObject(body.Payload),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.controlDB.CreateRuntimeEvent(event); err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleRuntimeNodeRunLease(w http.ResponseWriter, r *http.Request) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	var body runtimeRunLeaseRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	leaseSeconds := body.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = 90
	}
	// Q0: renewal is generation-conditioned. A node that does not send its
	// generation (pre-Q0 build) is rejected fail-closed — mixed-version
	// operation is unsupported; deployment requires drain → stop nodes →
	// upgrade console → upgrade nodes.
	if body.LeaseGeneration <= 0 {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "leaseGeneration is required: upgrade the runtime node (generation-conditional leases reject pre-Q0 nodes)")
		return
	}
	run, found, err := s.controlDB.ExtendRuntimeRunLeaseWithGeneration(principal.Node.WorkspaceID, runID, principal.Node.ID, body.LeaseGeneration, leaseSeconds)
	if err != nil {
		if controldb.LeaseGenerationMismatch(err) {
			// Run taken over, finished, or reaped: the node must abandon
			// silently. 409 so the lease loop can distinguish it from a
			// transport error and stop renewing.
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "runtime run lease lost: run is no longer owned by this claim")
			return
		}
		s.serverError(w, err)
		return
	}
	if !found {
		existing, existingFound, existingErr := s.controlDB.RuntimeRunByID(principal.Node.WorkspaceID, runID)
		if existingErr != nil {
			s.serverError(w, existingErr)
			return
		}
		if existingFound && existing.RuntimeNodeID == principal.Node.ID && existing.Status == "cancelled" {
			_ = json.NewEncoder(w).Encode(map[string]any{"run": runtimeRunResponse(existing), "cancelled": true})
			return
		}
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime run not found")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"run": runtimeRunResponse(run), "leaseExpiresAt": run.LeaseExpiresAt})
}

func (s *Server) handleRuntimeNodeRunComplete(w http.ResponseWriter, r *http.Request) {
	s.finishRuntimeNodeRun(w, r, "succeeded")
}

func (s *Server) handleRuntimeNodeRunFail(w http.ResponseWriter, r *http.Request) {
	s.finishRuntimeNodeRun(w, r, "failed")
}

func (s *Server) finishRuntimeNodeRun(w http.ResponseWriter, r *http.Request, status string) {
	principal, ok := runtimeNodeFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	run, found, err := s.controlDB.RuntimeRunByID(principal.Node.WorkspaceID, runID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || run.RuntimeNodeID != principal.Node.ID {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime run not found")
		return
	}
	var body runtimeRunFinishRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	// Q0: finish is generation-conditioned. A pre-Q0 node (no generation) or
	// a stale loop whose run was taken over/reaped is rejected; the run state
	// is left exactly as the current owner wrote it.
	if body.LeaseGeneration <= 0 {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "leaseGeneration is required: upgrade the runtime node (generation-conditional finish rejects pre-Q0 nodes)")
		return
	}
	resultJSON := marshalRuntimeObject(body.Result)
	finished, found, err := s.controlDB.FinishRuntimeRun(principal.Node.WorkspaceID, runID, principal.Node.ID, body.LeaseGeneration, status, body.ErrorCode, body.ErrorMessage, resultJSON)
	if err != nil {
		if controldb.LeaseGenerationMismatch(err) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "runtime run finish rejected: run is no longer owned by this claim")
			return
		}
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "runtime run not found")
		return
	}
	run = finished
	s.finalizeRuntimeForkSessionAndHeartbeat(&run, body)
	// GPT 收口 1+2: the task transition goes through the SAME unified
	// fenced critical section as the reaper — read → fence-check
	// (ActiveRuntimeRunID == runID) → mutate → persist → release token, all
	// under one token-mutex hold. No unconditional finalize-then-clear.
	s.applyFencedFinishTransition(&run, body)
	if isSuccessfulRuntimeStatus(run.Status) {
		s.markTaskAttentionSignalsForRun(run, "handled")
		s.markAttentionSignalsForWakeupRun(run)
	}
	// A step completed DURING this run (mga task step done) may have advanced
	// the workflow to the next agent step; that mid-run dispatch was silently
	// suppressed by hasActiveRuntimeRun because THIS run was still active.
	// Now that the run finished, re-attempt the dispatch so the pipeline does
	// not stall until the next poller tick (which would run the task locally
	// instead of on the node).
	s.dispatchWorkflowFollowupAfterRun(&run, r)
	s.requestPendingAttentionWakeupAfterRun(run)
	_ = json.NewEncoder(w).Encode(map[string]any{"run": runtimeRunResponse(run)})
}

// finalizeRuntimeForkSessionAndHeartbeat is the run-side (unfenced) part of a
// finish: fork-session projection and the agent heartbeat record. These are
// keyed to the run/session, not to the task fence, so they stay outside the
// task critical section. Task-state mutation happens exclusively inside
// applyFencedFinishTransition.
func (s *Server) finalizeRuntimeForkSessionAndHeartbeat(run *controldb.RuntimeRun, body runtimeRunFinishRequest) {
	if s == nil || run == nil {
		return
	}
	if strings.TrimSpace(run.ForkSessionID) != "" {
		s.finalizeRuntimeForkSessionRun(run, body)
	}
	if s.ts == nil || strings.TrimSpace(run.TaskID) == "" {
		return
	}
	now := time.Now().UTC()
	if hb, err := s.runtimeRunHeartbeat(run); err == nil && hb != nil {
		if run.Status == "failed" {
			hb.LastWakeupStatus = "failed"
		} else {
			hb.LastWakeupStatus = "done"
		}
		if sid, _ := body.Result["sessionId"].(string); strings.TrimSpace(sid) != "" {
			hb.SessionID = strings.TrimSpace(sid)
			hb.SessionStartedAt = &now
		}
		_ = s.saveRuntimeRunHeartbeat(run, hb)
	}
}

// applyFencedFinishTransition moves the task of a finished run through the
// unified fencedTaskTransition critical section (GPT 收口 1+2): fenced on
// ActiveRuntimeRunID; the mutation (state + ArchivedAt) and the token release
// land in ONE store write owned by the helper, so a write failure persists
// nothing and keeps the fence for the sweep replay. The mutation is a pure
// in-memory decision — it never writes the task store itself and performs NO
// delivery side effects: snapshot/push/worktree cleanup run only after the
// transition's persist succeeded (GPT 收口 6-2, post-commit), otherwise a
// persist failure would strand a deleted worktree with no recorded
// completion commit.
func (s *Server) applyFencedFinishTransition(run *controldb.RuntimeRun, body runtimeRunFinishRequest) {
	if s == nil || s.ts == nil || run == nil || strings.TrimSpace(run.TaskID) == "" {
		return
	}
	committedSuccess := false
	s.fencedTaskTransition(run.WorkspaceID, run.ProjectID, run.AgentID, run.TaskID, run.ID, func(task *entity.Task) fenceDecision {
		now := time.Now().UTC()
		if task.Status.IsTerminal() {
			return fenceDecisionSkip
		}
		if s.runtimeTaskHasWorkflow(run.WorkspaceID, run.ProjectID, run.TaskID) {
			if task.Status == entity.TaskStatusInProgress || task.Status == entity.TaskStatusPending {
				// A step completed mid-run (`mga task step done`) advances the
				// workflow to the next step while THIS run is still active; the
				// dispatch lands the task pending on the next agent. When the
				// run then finishes, the active step instance is pending but
				// fresh — that is a normal handoff, not an abandoned step: keep
				// the task as the dispatch placed it (release the fence only)
				// and let dispatchWorkflowFollowupAfterRun re-drive the run.
				if s.workflowAdvancedDuringRun(run) {
					return fenceDecisionSkip
				}
				msg := firstNonEmpty(strings.TrimSpace(body.ErrorMessage), strings.TrimSpace(body.ErrorCode), "runtime run failed")
				errorCode := firstNonEmpty(strings.TrimSpace(body.ErrorCode), "runtime_run_failed")
				if run.Status != "failed" {
					msg = runtimeWorkflowStepNotCompletedError
					errorCode = "workflow_step_not_completed"
				}
				prev := task.Status
				task.Status = entity.TaskStatusDoneFailed
				task.LastError = msg
				task.ArchivedAt = &now
				task.UpdatedAt = now
				entity.ApplyStatusTimestamps(task, prev, now)
				run.Status = "failed"
				run.ErrorCode = errorCode
				run.ErrorMessage = msg
				if body.Result == nil {
					body.Result = map[string]any{}
				}
				body.Result["error"] = msg
				run.ResultJSON = marshalRuntimeObject(body.Result)
			}
			return fenceDecisionApply
		}
		if task.Status != entity.TaskStatusInProgress && task.Status != entity.TaskStatusPending {
			return fenceDecisionSkip
		}
		prev := task.Status
		if run.Status == "failed" {
			// Q0 D4: infra failures (closed server-controlled error-code set)
			// back off or block the task instead of archiving it as
			// done_failed; business failures still land on done_failed.
			if isRuntimeInfraFailureCode(body.ErrorCode) {
				task.LastError = firstNonEmpty(strings.TrimSpace(body.ErrorMessage), strings.TrimSpace(body.ErrorCode), "runtime run failed")
				if !applyInfraFailureBackoffMutation(task, body.ErrorCode) {
					return fenceDecisionSkip
				}
				return fenceDecisionApply
			}
			task.Status = entity.TaskStatusDoneFailed
			task.LastError = firstNonEmpty(strings.TrimSpace(body.ErrorMessage), strings.TrimSpace(body.ErrorCode), "runtime run failed")
		} else {
			task.Status = entity.TaskStatusDoneSuccess
			resetInfraFailureStreak(task)
			if summary, _ := body.Result["summary"].(string); strings.TrimSpace(summary) != "" {
				task.Summary = strings.TrimSpace(summary)
			}
			committedSuccess = true
		}
		task.UpdatedAt = now
		entity.ApplyStatusTimestamps(task, prev, now)
		if task.Status.IsTerminal() {
			task.ArchivedAt = &now
		}
		return fenceDecisionApply
	})
	// Delivery side effects (GPT 收口 6-2): ONLY after the transition's persist
	// succeeded, against the PERSISTED task. Snapshot failure deliberately
	// keeps the worktree alive and skips the destructive cleanup (documented
	// captureTaskCompletionSnapshot contract).
	if committedSuccess {
		s.runTaskDeliveryPostCommit(run)
	}
	// Notification hook (Q0 PR-3): when the fenced transition parked the task
	// in blocked, the human-owner notify (comment/IM/audit) fires AFTER the
	// fence released — it is a side effect, not task state, and must not hold
	// the token mutex.
	s.notifyInfraBlockedOwner(run.WorkspaceID, run.ProjectID, run.AgentID, run.TaskID, body.ErrorCode)
}

// runTaskDeliveryPostCommit performs the delivery side effects of a successful
// task completion against the PERSISTED task (re-read, not the in-memory
// mutation): git snapshot → remote sync → worktree/preview cleanup. Snapshot
// failure records the error on the persisted task and skips the destructive
// steps so uncommitted work is never destroyed without a captured commit.
func (s *Server) runTaskDeliveryPostCommit(run *controldb.RuntimeRun) {
	if s == nil || s.ts == nil {
		return
	}
	task, err := s.ts.GetTask(run.ProjectID, run.AgentID, run.TaskID)
	if err != nil || task == nil || task.Status != entity.TaskStatusDoneSuccess {
		return
	}
	if err := s.captureTaskCompletionSnapshot(task); err != nil {
		slog.Warn("task completion snapshot failed; worktree kept for retry", "task", task.ID, "error", err)
		_ = s.ts.PersistTask(run.ProjectID, run.AgentID, task)
		return
	}
	s.syncTaskCompletionRemote(run.ProjectID, task)
	_ = s.ts.PersistTask(run.ProjectID, run.AgentID, task)
	s.cleanupTaskDeliveryArtifacts(run.ProjectID, task.ID)
}

func runtimeRunKind(run *controldb.RuntimeRun) string {
	if run == nil {
		return ""
	}
	var spec struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal([]byte(defaultRawJSON(run.SpecJSON)), &spec)
	return strings.TrimSpace(spec.Kind)
}

func (s *Server) finalizeRuntimeForkSessionRun(run *controldb.RuntimeRun, body runtimeRunFinishRequest) {
	if s == nil || s.controlDB == nil || run == nil || strings.TrimSpace(run.ForkSessionID) == "" {
		return
	}
	session, found, err := s.controlDB.AgentSessionByID(run.WorkspaceID, run.ForkSessionID)
	if err != nil || !found {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	before := session
	switch strings.ToLower(strings.TrimSpace(run.Status)) {
	case "failed", "cancelled", "canceled":
		if strings.ToLower(strings.TrimSpace(run.Status)) == "failed" {
			session.Status = "failed"
		} else {
			session.Status = "stopped"
		}
	default:
		session.Status = "done"
	}
	if summary, _ := body.Result["summary"].(string); strings.TrimSpace(summary) != "" {
		session.ResultSummary = strings.TrimSpace(summary)
	} else if strings.TrimSpace(body.ErrorMessage) != "" {
		session.ResultSummary = strings.TrimSpace(body.ErrorMessage)
	} else if strings.TrimSpace(body.ErrorCode) != "" {
		session.ResultSummary = strings.TrimSpace(body.ErrorCode)
	}
	if sid, _ := body.Result["sessionId"].(string); strings.TrimSpace(sid) != "" {
		session.RuntimeSessionID = strings.TrimSpace(sid)
	}
	session.LastRunID = run.ID
	session.UpdatedAt = now
	session.LastActivityAt = now
	session.CompletedAt = now
	if err := s.controlDB.UpsertAgentSession(session); err != nil {
		slog.Warn("runtime fork session finalize failed", "session", session.ID, "run", run.ID, "error", err)
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  run.WorkspaceID,
		ActorType:    "agent",
		ActorID:      firstNonEmpty(strings.TrimSpace(run.ProjectID)+"/"+strings.TrimSpace(run.AgentID), strings.TrimSpace(run.AgentWorkerID)),
		Action:       "agent_session.finish",
		ResourceType: "agent_session",
		ResourceID:   session.ID,
		Summary:      "Fork session runtime run finished",
		Before:       runtimeSessionResponse(before),
		After:        runtimeSessionResponse(session),
	})
}

func (s *Server) runtimeRunHeartbeat(run *controldb.RuntimeRun) (*entity.HeartbeatConfig, error) {
	if s == nil || s.ts == nil || run == nil {
		return nil, nil
	}
	if strings.TrimSpace(run.AgentWorkerID) == "" || s.controlDB == nil {
		return nil, fmt.Errorf("runtime run %s has no agent worker identity", run.ID)
	}
	worker, ok, err := s.controlDB.AgentWorkerByID(run.WorkspaceID, run.AgentWorkerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("agent worker %s not found", run.AgentWorkerID)
	}
	hb := &entity.HeartbeatConfig{}
	if raw := strings.TrimSpace(worker.ScheduleJSON); raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), hb); err != nil {
			return nil, err
		}
	}
	return hb, nil
}

func (s *Server) saveRuntimeRunHeartbeat(run *controldb.RuntimeRun, hb *entity.HeartbeatConfig) error {
	if s == nil || s.ts == nil || run == nil || hb == nil {
		return nil
	}
	if strings.TrimSpace(run.AgentWorkerID) == "" || s.controlDB == nil {
		return fmt.Errorf("runtime run %s has no agent worker identity", run.ID)
	}
	worker, ok, err := s.controlDB.AgentWorkerByID(run.WorkspaceID, run.AgentWorkerID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("agent worker %s not found", run.AgentWorkerID)
	}
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	worker.ScheduleJSON = string(body)
	worker.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.controlDB.UpsertAgentWorker(worker)
}

func (s *Server) cancelRuntimeRunsForAgent(workspaceID, project, agent, taskID, reason string) (int, error) {
	if s == nil || s.controlDB == nil {
		return 0, nil
	}
	total := 0
	for _, status := range []string{"queued", "running"} {
		runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{
			WorkspaceID: workspaceID,
			ProjectID:   project,
			AgentID:     agent,
			TaskID:      taskID,
			Status:      status,
			Limit:       500,
		})
		if err != nil {
			return total, err
		}
		for _, run := range runs {
			run.Status = "cancelled"
			run.ErrorCode = "cancelled"
			run.ErrorMessage = firstNonEmpty(strings.TrimSpace(reason), "runtime run cancelled")
			now := time.Now().UTC().Format(time.RFC3339)
			run.FinishedAt = now
			run.UpdatedAt = now
			if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

func (s *Server) withRuntimeNodeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node token required")
			return
		}
		record, found, err := s.controlDB.RuntimeNodeTokenByHash(hashRuntimeToken(token))
		if err != nil {
			s.serverError(w, err)
			return
		}
		if !found || strings.TrimSpace(record.RevokedAt) != "" || runtimeTokenExpired(record.ExpiresAt) {
			s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid or expired runtime node token")
			return
		}
		node, found, err := s.controlDB.RuntimeNodeByID(record.WorkspaceID, record.RuntimeNodeID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if !found {
			s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "runtime node not found")
			return
		}
		ctx := context.WithValue(r.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{Token: record, Node: node})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func runtimeNodeFromRequest(r *http.Request) (runtimeNodePrincipal, bool) {
	principal, ok := r.Context().Value(ctxRuntimeNodeKey).(runtimeNodePrincipal)
	return principal, ok
}

// warnRuntimeNodeVersionDrift logs once per node when a heartbeat reports a
// build version different from the console's own. Mixed-version fleets are
// unsupported (lease-generation renewals fail closed for pre-Q0 nodes), and a
// node left behind after a console upgrade otherwise surfaces only as
// confusing claim/lease behavior days later. Warn-only by design: a node
// actively running a job must never have its heartbeat rejected mid-flight —
// that would starve lease renewal and get the live run reaped.
// Heartbeats arrive concurrently (one per node, plus claim/lease traffic on
// the same server), so every access to driftWarnedNodes holds driftWarnedMu.
func (s *Server) warnRuntimeNodeVersionDrift(node controldb.RuntimeNode) {
	nodeVersion := strings.TrimSpace(node.Version)
	if nodeVersion == "" || s.version == "" {
		return
	}
	s.driftWarnedMu.Lock()
	defer s.driftWarnedMu.Unlock()
	if s.driftWarnedNodes == nil {
		s.driftWarnedNodes = map[string]string{}
	}
	if nodeVersion == s.version {
		// Back in alignment: end the drift episode so a future mismatch
		// warns again instead of being silenced by the stale entry.
		delete(s.driftWarnedNodes, node.ID)
		return
	}
	if s.driftWarnedNodes[node.ID] == nodeVersion {
		return
	}
	s.driftWarnedNodes[node.ID] = nodeVersion
	slog.Warn("runtime node version drift: console and node run different builds (mixed-version operation is unsupported; upgrade the node to match)",
		"node_id", node.ID,
		"node_name", node.Name,
		"hostname", node.Hostname,
		"node_version", nodeVersion,
		"console_version", s.version,
	)
}

// forgetRuntimeNodeDriftState drops the node's drift-dedupe entry — called
// when the node row is deleted so a re-registered node with the same id (or
// a fresh node after fleet cleanup) starts a clean episode, and so the map
// cannot grow without bound across node churn (GPT review Q4 follow-up).
func (s *Server) forgetRuntimeNodeDriftState(nodeID string) {
	if s == nil || strings.TrimSpace(nodeID) == "" {
		return
	}
	s.driftWarnedMu.Lock()
	defer s.driftWarnedMu.Unlock()
	delete(s.driftWarnedNodes, nodeID)
}

func runtimeNodeResponse(node controldb.RuntimeNode) map[string]any {
	return map[string]any{
		"id":               node.ID,
		"workspaceId":      node.WorkspaceID,
		"name":             node.Name,
		"kind":             node.Kind,
		"status":           runtimeNodeEffectiveStatus(node),
		"storedStatus":     node.Status,
		"os":               node.OS,
		"arch":             node.Arch,
		"hostname":         node.Hostname,
		"version":          node.Version,
		"capabilitiesJson": node.CapabilitiesJSON,
		"policyJson":       node.PolicyJSON,
		"lastSeenAt":       node.LastSeenAt,
		"lastError":        node.LastError,
		"createdByUserId":  node.CreatedByUserID,
		"createdAt":        node.CreatedAt,
		"updatedAt":        node.UpdatedAt,
	}
}

func runtimeNodeEffectiveStatus(node controldb.RuntimeNode) string {
	status := strings.TrimSpace(node.Status)
	if status == "" {
		status = "pending"
	}
	if status != "online" {
		return status
	}
	if !runtimeNodeSeenRecently(node.LastSeenAt) {
		return "offline"
	}
	return "online"
}

func runtimeNodeIsOnline(node controldb.RuntimeNode) bool {
	return runtimeNodeEffectiveStatus(node) == "online"
}

func runtimeNodeSeenRecently(lastSeenAt string) bool {
	lastSeenAt = strings.TrimSpace(lastSeenAt)
	if lastSeenAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastSeenAt)
	if err != nil {
		return false
	}
	return time.Since(t) <= runtimeNodeOnlineWindow
}

func runtimeRunResponse(run controldb.RuntimeRun) map[string]any {
	return map[string]any{
		"id":                   run.ID,
		"workspaceId":          run.WorkspaceID,
		"agentWorkerId":        run.AgentWorkerID,
		"projectMembershipId":  run.ProjectMembershipID,
		"projectId":            run.ProjectID,
		"agentId":              run.AgentID,
		"taskId":               run.TaskID,
		"workflowInstanceId":   run.WorkflowInstanceID,
		"workflowStepId":       run.WorkflowStepID,
		"forkSessionId":        run.ForkSessionID,
		"desiredRuntimeNodeId": run.DesiredRuntimeNodeID,
		"runtimeNodeId":        run.RuntimeNodeID,
		"status":               run.Status,
		"priority":             run.Priority,
		"leaseExpiresAt":       run.LeaseExpiresAt,
		"leaseGeneration":      run.LeaseGeneration,
		"slotClass":            controldb.NormalizedSlotClass(run.SlotClass),
		"claimedAt":            run.ClaimedAt,
		"startedAt":            run.StartedAt,
		"finishedAt":           run.FinishedAt,
		"errorCode":            run.ErrorCode,
		"errorMessage":         run.ErrorMessage,
		"createdAt":            run.CreatedAt,
		"updatedAt":            run.UpdatedAt,
	}
}

func runtimeTokenExpired(expiresAt string) bool {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return true
	}
	return time.Now().UTC().After(t)
}

func hashRuntimeToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newRuntimeID(prefix string) string {
	return prefix + "_" + randomRuntimeHex(8)
}

func randomRuntimeHex(n int) string {
	if n <= 0 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
	}
	return hex.EncodeToString(b)
}

func parseRuntimeTTL(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

func externalServerURL(r *http.Request) string {
	// Background dispatch paths (poller/trigger/followup) have no request;
	// an empty URL means the run spec carries no control-plane address, which
	// nodes tolerate (they use their own configured console URL).
	if r == nil {
		return ""
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	prefix := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if prefix != "" {
		prefix = "/" + strings.Trim(prefix, "/")
		if prefix == "/" || strings.Contains(prefix, "..") {
			prefix = ""
		}
	}
	return strings.TrimRight(proto+"://"+host+prefix, "/")
}

func marshalRuntimeObject(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func defaultRawJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

func detectLocalRuntimeCapabilities() map[string]any {
	caps := sandbox.DetectCapabilities()
	return map[string]any{
		"os":   runtime.GOOS,
		"arch": runtime.GOARCH,
		"sandbox": map[string]any{
			"docker": map[string]any{
				"available": caps.Docker.Available,
				"reason":    caps.Docker.Reason,
			},
			"e2b": map[string]any{
				"available": caps.E2B.Available,
				"reason":    caps.E2B.Reason,
			},
		},
	}
}
