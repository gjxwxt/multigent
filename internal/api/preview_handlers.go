package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/preview"
	"github.com/multigent/multigent/internal/sandbox"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

type previewChatSession struct {
	TaskID      string
	Project     string
	Agent       string
	StartedAt   time.Time
	Cancel      context.CancelFunc
	Cmd         *exec.Cmd
	Events      []string
	Subscribers map[chan string]struct{}
	Mu          sync.Mutex
	Done        bool
	Stopped     bool
}

func (s *previewChatSession) addSubscriber() (chan string, []string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	ch := make(chan string, 256)
	if s.Subscribers == nil {
		s.Subscribers = make(map[chan string]struct{})
	}
	history := make([]string, len(s.Events))
	copy(history, s.Events)
	if !s.Done && !s.Stopped {
		s.Subscribers[ch] = struct{}{}
	}
	return ch, history
}

func (s *previewChatSession) removeSubscriber(ch chan string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.Subscribers != nil {
		delete(s.Subscribers, ch)
	}
}

func (s *previewChatSession) broadcast(payload string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Events = append(s.Events, payload)
	for ch := range s.Subscribers {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (s *previewChatSession) finish(stopped bool) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.Done || s.Stopped {
		return
	}
	s.Done = true
	s.Stopped = stopped
	endPayload := `{"type":"done"}`
	if stopped {
		endPayload = `{"type":"stopped"}`
	}
	s.Events = append(s.Events, endPayload)
	for ch := range s.Subscribers {
		select {
		case ch <- endPayload:
		default:
		}
		close(ch)
	}
	s.Subscribers = make(map[chan string]struct{})
}

type previewChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type previewChatBody struct {
	Message string           `json:"message"`
	History []previewChatMsg `json:"history,omitempty"`
}

// buildPreviewEnvSnapshot renders the platform-known environment facts for a
// preview Copilot run. Every Copilot message executes as a fresh
// --no-session process, so without this block the agent re-discovers ports,
// service state, and worktree dirtiness from scratch each round — and stale
// mid-run statements replayed from chat history get treated as facts. The
// snapshot uses only state the platform already holds (preview engine
// instance, git worktree read-only queries); it never probes services.
func (s *Server) buildPreviewEnvSnapshot(project, taskID, worktreeDir string) string {
	var b strings.Builder
	b.WriteString("【环境快照 | 平台注入的当前事实,以此为准】\n")

	if s.previewEngine == nil {
		b.WriteString("- 预览服务: 未启动(从未启动或已被回收;如页面无法访问这是原因)\n")
	} else if inst, ok := s.previewEngine.GetInstance(taskID); ok && inst != nil {
		switch inst.Status {
		case "running":
			kind := string(inst.Type)
			readonly := ""
			if inst.ReadOnly {
				readonly = ", 只读快照"
			}
			b.WriteString(fmt.Sprintf("- 预览服务: 运行中 (%s, 宿主端口 %d, 经 /preview/%s/ 代理%s)\n", kind, inst.Port, taskID, readonly))
		case "starting":
			b.WriteString("- 预览服务: 正在启动(稍等片刻即可访问)\n")
		case "error":
			b.WriteString(fmt.Sprintf("- 预览服务: 启动失败 (%s)\n", firstLine(inst.Error)))
		default:
			b.WriteString("- 预览服务: 已停止\n")
		}
	} else {
		b.WriteString("- 预览服务: 未启动(从未启动或已被回收;如页面无法访问这是原因)\n")
	}

	if _, err := os.Stat(filepath.Join(worktreeDir, ".git")); err == nil {
		b.WriteString(fmt.Sprintf("- 工作区目录: %s\n", worktreeDir))
		if branch, err := gitworktree.NewManager().CheckedOutBranch(worktreeDir); err == nil && strings.TrimSpace(branch) != "" {
			b.WriteString(fmt.Sprintf("- 当前分支: %s\n", branch))
		}
		if out, err := boundedGitOutput(worktreeDir, 5*time.Second, "status", "--porcelain"); err == nil {
			lines := nonEmptyLines(string(out))
			if len(lines) == 0 {
				b.WriteString("- 未提交改动: 无(工作区干净)\n")
			} else {
				limit := len(lines)
				if limit > 5 {
					limit = 5
				}
				b.WriteString(fmt.Sprintf("- 未提交改动: %d 个文件 (%s)\n", len(lines), strings.Join(lines[:limit], ", ")))
			}
		}
	} else {
		b.WriteString("- Git: 未检测到 Git 仓库(非分支任务环境,跳过分支/改动信息)\n")
	}

	b.WriteString("- 注意: 历史对话仅供理解意图,其中的服务状态、改动状态可能已过时;以上快照为当前唯一事实。\n")
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			// porcelain: XY <path>; keep just the path tail.
			out = append(out, fields[len(fields)-1])
		}
	}
	return out
}

func (s *Server) handleListProjectBranches(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}

	gitRoot := s.resolveProjectGitRoot(project)

	// refresh=1 triggers an explicit user-requested remote fetch so the
	// listing reflects upstream state. Task creation always fetches the
	// target branch itself; this only affects UI freshness. A fetch
	// failure degrades to local refs plus a warning instead of an error.
	refreshed, refreshErr := s.refreshProjectBranches(project, gitRoot, r.URL.Query().Get("refresh") == "1")

	branches := []string{"main"}
	branchInfos := []gitworktree.BranchInfo{{Name: "main"}}
	if s.worktreeMgr != nil {
		if list, err := s.worktreeMgr.ListBranches(gitRoot); err == nil && len(list) > 0 {
			branches = list
		}
		if detailed, err := s.worktreeMgr.ListBranchesDetailed(gitRoot); err == nil && len(detailed) > 0 {
			branchInfos = detailed
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"project":     project,
		"branches":    branches,
		"branchInfos": branchInfos,
		"refreshed":   refreshed,
		"refreshErr":  refreshErr,
	})
}

// handlePostProjectBranchesRefresh fetches remote updates for a project's
// git root and returns the refreshed structured branch listing. Used by the
// task creation dialog's explicit "sync" affordance.
func (s *Server) handlePostProjectBranchesRefresh(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	gitRoot := s.resolveProjectGitRoot(project)
	refreshed, refreshErr := s.refreshProjectBranches(project, gitRoot, true)

	branches := []string{"main"}
	branchInfos := []gitworktree.BranchInfo{{Name: "main"}}
	if s.worktreeMgr != nil {
		if list, err := s.worktreeMgr.ListBranches(gitRoot); err == nil && len(list) > 0 {
			branches = list
		}
		if detailed, err := s.worktreeMgr.ListBranchesDetailed(gitRoot); err == nil && len(detailed) > 0 {
			branchInfos = detailed
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"project":     project,
		"branches":    branches,
		"branchInfos": branchInfos,
		"refreshed":   refreshed,
		"refreshErr":  refreshErr,
	})
}

// refreshProjectBranches optionally fetches origin and reports whether the
// refresh succeeded. A nil worktree manager or missing remote is a no-op
// success; fetch failures are returned as a message, not an error.
func (s *Server) refreshProjectBranches(project, gitRoot string, refresh bool) (bool, string) {
	if !refresh || s.worktreeMgr == nil {
		return false, ""
	}
	if err := s.worktreeMgr.FetchRemoteUpdates(gitRoot); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (s *Server) resolveProjectGitRoot(project string) string {
	projectRoot := s.st.ProjectDir(project)
	wsDir := filepath.Join(projectRoot, "workspace")
	if _, err := os.Stat(filepath.Join(wsDir, ".git")); err == nil {
		return wsDir
	}
	if p, err := s.st.Project(project); err == nil && p != nil && p.Repo != "" {
		if _, err := os.Stat(filepath.Join(p.Repo, ".git")); err == nil {
			return p.Repo
		}
	}
	agentsDir := filepath.Join(projectRoot, "agents")
	if entries, err := os.ReadDir(agentsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				agentGit := filepath.Join(agentsDir, e.Name(), ".git")
				if _, err := os.Stat(agentGit); err == nil {
					return filepath.Join(agentsDir, e.Name())
				}
			}
		}
	}
	return projectRoot
}

func (s *Server) handleGetTaskPreview(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}

	if s.previewEngine == nil {
		s.previewEngine = preview.NewEngine()
	}

	inst, ok := s.previewEngine.GetInstance(taskID)
	if !ok {
		// Detect project type from worktree or project workspace
		worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
		projType := preview.DetectProjectType(worktreeDir)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId":        taskID,
			"project":       project,
			"type":          string(projType),
			"status":        "stopped",
			"url":           s.previewSurfaceURL(taskID),
			"worktreeDir":   worktreeDir,
			"previewToken":        s.signPreviewToken(taskID, project),
			"drawerEnabled":       s.PreviewCopilotDrawerEnabled(),
			"turnReceiptsEnabled": s.PreviewTurnReceiptsEnabled(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		*preview.PreviewInstance
		PreviewToken        string `json:"previewToken,omitempty"`
		DrawerEnabled       bool   `json:"drawerEnabled"`
		TurnReceiptsEnabled bool   `json:"turnReceiptsEnabled"`
	}{
		PreviewInstance:     inst,
		PreviewToken:        s.signPreviewToken(taskID, inst.Project),
		DrawerEnabled:       s.PreviewCopilotDrawerEnabled(),
		TurnReceiptsEnabled: s.PreviewTurnReceiptsEnabled(),
	})
}

// previewSurfaceURL returns the shareable preview URL for a task. When the
// preview origin is configured (§2.0) it points at that origin so the share
// link lands on the isolated surface; otherwise it is the legacy relative
// path, which the origin gate now rejects — the UI communicates the
// misconfiguration instead of silently serving a same-origin preview.
func (s *Server) previewSurfaceURL(taskID string) string {
	if s.previewOrigin != "" {
		return s.previewOrigin + "/preview/" + taskID + "/"
	}
	return fmt.Sprintf("/preview/%s/", taskID)
}

func (s *Server) handlePostTaskPreviewStart(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}

	if s.previewEngine == nil {
		s.previewEngine = preview.NewEngine()
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	readOnly := false
	if task, _, taskErr := s.findTaskInProject(project, taskID); taskErr == nil && task != nil && task.Status.IsTerminal() && strings.TrimSpace(task.CompletionCommit) != "" {
		if s.worktreeMgr == nil {
			s.jsonError(w, http.StatusInternalServerError, "worktree manager is unavailable")
			return
		}
		var err error
		worktreeDir, err = s.worktreeMgr.EnsureSnapshotWorktree(s.resolveProjectGitRoot(project), taskID, task.CompletionCommit)
		if err != nil {
			s.jsonError(w, http.StatusConflict, fmt.Sprintf("rebuild completed task snapshot failed: %v", err))
			return
		}
		readOnly = true
	}
	var inst *preview.PreviewInstance
	var err error
	// Resolve the runtime per project at start time so the preview container
	// matches what this project's agent sandboxes run on (jvm21 projects get a
	// JDK-capable preview without any server-wide override). The task's
	// executing agent is passed along so agent-level profile preferences apply
	// on the same terms as the runner applies them — agent and preview agree.
	runtime, rtErr := s.resolveTaskPreviewRuntime(project, taskID)
	if rtErr != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, rtErr.Error())
		return
	}
	if readOnly {
		inst, err = s.previewEngine.StartSnapshotPreviewWithRuntime(r.Context(), taskID, project, worktreeDir, runtime)
	} else {
		inst, err = s.previewEngine.StartEphemeralPreviewWithRuntime(r.Context(), taskID, project, worktreeDir, runtime)
	}
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, fmt.Sprintf("start preview failed: %v", err))
		return
	}
	// The share URL must land on the isolated preview origin (§2.0). The
	// engine only knows the relative path; rewrite it here where the
	// deployment config lives.
	inst.URL = s.previewSurfaceURL(taskID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		*preview.PreviewInstance
		PreviewToken string `json:"previewToken,omitempty"`
	}{inst, s.signPreviewToken(taskID, project)})
}

type previewFeedbackBody struct {
	Feedback string `json:"feedback"`
	Category string `json:"category,omitempty"`
}

func (s *Server) handlePostTaskPreviewFeedback(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))

	if (project == "" || project == "current") && s.previewEngine != nil {
		if inst, ok := s.previewEngine.GetInstance(taskID); ok && inst.Project != "" {
			project = inst.Project
		}
	}
	_, _, ok := s.previewWritePrincipal(w, r, project, taskID)
	if !ok {
		return
	}
	// Guard 1: Drawer feature flag gate (server-controlled rollout, fail-closed)
	if !s.PreviewCopilotDrawerEnabled() {
		s.jsonErrorCode(w, http.StatusConflict, "feature_disabled", "preview copilot drawer is disabled")
		return
	}

	// Guard 2: Task status gate (fail-closed check on task.Status, regardless of container lifecycle)
	task, _, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status != entity.TaskStatusInProgress {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "task is not in progress; modifying completed or inactive task code is forbidden")
		return
	}

	if !s.allowPreviewChat(taskID) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeConflict, "preview feedback rate limit exceeded; retry shortly")
		return
	}
	if s.previewInstanceReadOnly(taskID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "completed-task snapshot is read-only; create a follow-up task to modify it")
		return
	}

	// Guard 3: Slice B gate — receipts feature flag and isolated clone
	if !s.PreviewTurnReceiptsEnabled() {
		s.jsonErrorCode(w, http.StatusConflict, "feature_disabled", s.PreviewTurnReceiptsDisabledReason())
		return
	}
	s.jsonErrorCode(w, http.StatusConflict, "feature_disabled", "preview feedback code modification is disabled until receipt and isolated clone are implemented")
}

func (s *Server) handlePostTaskPreviewChat(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))

	if (project == "" || project == "current") && s.previewEngine != nil {
		if inst, ok := s.previewEngine.GetInstance(taskID); ok && inst.Project != "" {
			project = inst.Project
		}
	}
	// Task 1.1: write surface — Bearer-only principal, operator + approver
	// gates; share tokens are view-only and get 403 here.
	authReq, principal, ok := s.previewWritePrincipal(w, r, project, taskID)
	if !ok {
		return
	}
	r = authReq
	_ = principal

	// Guard 1: Drawer feature flag gate (server-controlled rollout, fail-closed)
	if !s.PreviewCopilotDrawerEnabled() {
		s.jsonErrorCode(w, http.StatusConflict, "feature_disabled", "preview copilot drawer is disabled")
		return
	}

	// Guard 2: Task status gate (fail-closed check on task.Status, regardless of container lifecycle)
	task, _, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status != entity.TaskStatusInProgress {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "task is not in progress; modifying completed or inactive task code is forbidden")
		return
	}

	if !s.allowPreviewChat(taskID) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeConflict, "preview chat rate limit exceeded; retry shortly")
		return
	}
	if s.previewInstanceReadOnly(taskID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "completed-task snapshot is read-only; create a follow-up task to modify it")
		return
	}

	// Guard 3: Slice B gate — receipts feature flag and isolated clone
	if !s.PreviewTurnReceiptsEnabled() {
		s.jsonErrorCode(w, http.StatusConflict, "feature_disabled", s.PreviewTurnReceiptsDisabledReason())
		return
	}

	s.executePreviewChatTurn(w, r, project, taskID, task, principal)
}

func (s *Server) handleGetTaskPreviewLive(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	// Task 1.1: live SSE forwards raw agent session output (paths, commands,
	// potential credential echoes) — share tokens are excluded, real users
	// only. The principal is still bound for downstream audit.
	_, _, ok := s.previewWritePrincipal(w, r, r.PathValue("name"), taskID)
	if !ok {
		return
	}
	s.previewMu.Lock()
	session, exists := s.previewSessions[taskID]
	s.previewMu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.jsonError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	if !exists || session == nil {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"idle"}`)
		flusher.Flush()
		return
	}

	subCh, history := session.addSubscriber()
	defer session.removeSubscriber(subCh)

	for _, p := range history {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", p); err != nil {
			return
		}
		flusher.Flush()
	}

	if session.Done || session.Stopped {
		return
	}

	for {
		select {
		case p, ok := <-subCh:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", p); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handlePostTaskPreviewStop(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	// Task 1.1: stop kills a running Copilot session — a real principal with
	// operator rights only; share tokens are view-only (403 here).
	if _, _, ok := s.previewWritePrincipal(w, r, project, taskID); !ok {
		return
	}
	s.previewMu.Lock()
	session, exists := s.previewSessions[taskID]
	if exists && session != nil {
		session.Cancel()
		if session.Cmd != nil && session.Cmd.Process != nil {
			killProcessGroup(session.Cmd.Process.Pid)
		}
		session.finish(true)
	}
	s.previewMu.Unlock()

	// Also cancel any active in-flight preview turn receipt
	engine := s.turnEngine(r)
	if engine != nil {
		_ = engine.CancelActiveTurn(r.Context(), project, taskID, "stopped by operator")
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"stopped": exists,
		"taskId":  taskID,
	})
}

func (s *Server) handleGetTaskPreviewStatus(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	// Round-11 P0 fix: login-required AND project read access — the response
	// (busy/agent/startedAt) is task metadata, so any authenticated user
	// without project membership must be denied. Share tokens get 401 until
	// the leakage audit concludes; members of other projects get 403.
	authReq, _, ok := s.previewLoginPrincipal(w, r, taskID)
	if !ok {
		return
	}
	if !s.authorizePreviewProject(w, authReq, project) {
		return
	}
	s.previewMu.Lock()
	session, exists := s.previewSessions[taskID]
	var agent string
	var startedAt *time.Time
	busy := false
	if exists && session != nil && !session.Done && !session.Stopped {
		busy = true
		agent = session.Agent
		startedAt = &session.StartedAt
	}
	s.previewMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"taskId":              taskID,
		"busy":                busy,
		"agent":               agent,
		"startedAt":           startedAt,
		"turnReceiptsEnabled": s.PreviewTurnReceiptsEnabled(),
	})
}

// handleTaskPreviewProxy reverse-proxies HTTP requests for /preview/{taskId}/...
func (s *Server) handleTaskPreviewProxy(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/preview/") {
		http.NotFound(w, r)
		return
	}

	trimmed := strings.TrimPrefix(path, "/preview/")
	parts := strings.SplitN(trimmed, "/", 2)
	taskID := parts[0]
	subpath := "/"
	if len(parts) > 1 {
		subpath = "/" + parts[1]
	}

	instanceKey := taskID
	turnID := ""
	effectiveTaskPath := taskID
	if strings.HasPrefix(subpath, "/turn/") {
		turnParts := strings.SplitN(strings.TrimPrefix(subpath, "/turn/"), "/", 2)
		if turnParts[0] != "" {
			turnID = turnParts[0]
			instanceKey = "turn:" + taskID + ":" + turnID
			effectiveTaskPath = taskID + "/turn/" + turnID
			subpath = "/"
			if len(turnParts) > 1 {
				subpath = "/" + turnParts[1]
			}
		}
	}

	if s.previewEngine == nil {
		s.previewEngine = preview.NewEngine()
	}

	// Token gate BEFORE the instance check: authorization failures must not
	// depend on runtime state, and legacy no-Cap tokens get a fast, uniform
	// 403 (round-13: breaking migration — pre-Cap share links are dead).
	previewToken := previewRequestToken(r, taskID)
	claims, tokOK := s.verifyPreviewToken(previewToken, taskID)
	if !tokOK {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "preview token required")
		return
	}
	// Round-11: the view capability minted into tokens is enforced on the
	// proxy read path. Tokens without any Cap (pre-Task-1.1 legacy share
	// links) are view-less and rejected — fail-closed for old tokens.
	if !previewTokenHasCapability(claims, previewCapabilityView) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeForbidden,
			"preview share token lacks view capability; request a fresh link")
		return
	}
	// pvt-carrying GETs were already exchanged to the HttpOnly cookie by the
	// origin-routing wrapper before reaching this handler (§2.0.3) — no
	// document request ever renders with the token in location.search.

	inst, ok := s.previewEngine.GetInstance(instanceKey)
	if !ok || inst.Status != "running" || inst.Port <= 0 {
		if turnID != "" {
			http.Error(w, fmt.Sprintf("Preview environment for turn %q is not running.", turnID), http.StatusServiceUnavailable)
		} else {
			http.Error(w, fmt.Sprintf("Preview environment for task %q is not running. Please launch it from the task review panel.", taskID), http.StatusServiceUnavailable)
		}
		return
	}

	targetURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", inst.Port))
	if err != nil {
		http.Error(w, "invalid preview target URL", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = subpath
		req.Host = targetURL.Host
	}

	// Modify response to inject the feedback widget and rewrite root-relative asset URLs into HTML pages
	proxy.ModifyResponse = func(resp *http.Response) error {
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "text/html") {
			return nil
		}

		var reader io.Reader = resp.Body
		isGzip := strings.Contains(resp.Header.Get("Content-Encoding"), "gzip")
		if isGzip {
			gzReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil
			}
			defer gzReader.Close()
			reader = gzReader
		}

		bodyBytes, err := io.ReadAll(reader)
		if err != nil {
			return nil
		}
		_ = resp.Body.Close()

		consoleOrigin := ""
		if s.PreviewCopilotDrawerEnabled() {
			consoleOrigin = s.consoleOrigin
		}
		html := rewriteHTML(string(bodyBytes), effectiveTaskPath, inst.Project, consoleOrigin)

		newBodyBytes := []byte(html)
		if isGzip {
			var buf bytes.Buffer
			gzWriter := gzip.NewWriter(&buf)
			_, _ = gzWriter.Write(newBodyBytes)
			_ = gzWriter.Close()
			newBodyBytes = buf.Bytes()
		}

		resp.Body = io.NopCloser(bytes.NewReader(newBodyBytes))
		resp.ContentLength = int64(len(newBodyBytes))
		resp.Header.Set("Content-Length", strconv.Itoa(len(newBodyBytes)))
		resp.Header.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		resp.Header.Set("Pragma", "no-cache")
		resp.Header.Del("ETag")
		return nil
	}

	proxy.ServeHTTP(w, r)
}

var (
	htmlAttrRe  = regexp.MustCompile(`(?i)\b(href|src|action)\s*=\s*(["'])/([^"']*)(["'])`)
	esmImportRe = regexp.MustCompile(`(?m)\b(from\s*|import\s*)(["'])/([^"']*)(["'])`)
)

func rewriteHTML(html, taskID, projectName string, consoleOrigins ...string) string {
	previewPrefix := fmt.Sprintf("/preview/%s/", taskID)
	consoleOrigin := ""
	if len(consoleOrigins) > 0 {
		consoleOrigin = strings.TrimSpace(consoleOrigins[0])
	}

	// Interceptor script to handle dynamic fetches, XMLHttpRequest, WebSocket,
	// and SPA History Navigation. §2.0.4: no credential is ever injected into
	// the shared preview document — no __MG_PREVIEW_TOKEN__, no Copilot
	// widget. The previewed app has zero ties to console identity.
	patchScript := fmt.Sprintf(`<meta name="referrer" content="same-origin"><base href=%q><script>
(function(){
  var prefix = %q;
  window.__MG_PREVIEW_TASK_ID__ = %q;
  window.__MG_PREVIEW_PROJECT__ = %q;
  window.__MG_PREVIEW_BASE__ = prefix.replace(/\/$/, '');
  try {
    sessionStorage.setItem('__mg_preview_task_id', %q);
    sessionStorage.setItem('__mg_preview_project', %q);
  } catch(e) {}

  function patchUrl(u) {
    if (typeof u === 'string' && u.startsWith('/') && !u.startsWith('/preview/') && !u.startsWith('/_multigent_preview/')) {
      return prefix + u.slice(1);
    }
    return u;
  }

  // Patch history.pushState and replaceState for SPA React Router
  var origPushState = window.history.pushState;
  if (origPushState) {
    window.history.pushState = function(state, unused, url) {
      if (url) {
        url = patchUrl(url);
      }
      return origPushState.call(this, state, unused, url);
    };
  }
  var origReplaceState = window.history.replaceState;
  if (origReplaceState) {
    window.history.replaceState = function(state, unused, url) {
      if (url) {
        url = patchUrl(url);
      }
      return origReplaceState.call(this, state, unused, url);
    };
  }

  // Intercept click on links to keep them inside preview subpath
  document.addEventListener('click', function(e) {
    var a = e.target && e.target.closest ? e.target.closest('a') : null;
    if (!a) return;
    var href = a.getAttribute('href');
    if (href && href.startsWith('/') && !href.startsWith('/preview/') && !href.startsWith('/_multigent_preview/')) {
      e.preventDefault();
      var newHref = prefix + href.slice(1);
      a.setAttribute('href', newHref);
      window.location.href = newHref;
    }
  }, true);

  var origFetch = window.fetch;
  if (origFetch) {
    window.fetch = function(input, init) {
      if (typeof input === 'string') {
        input = patchUrl(input);
      } else if (input instanceof Request) {
        var newUrl = patchUrl(input.url);
        if (newUrl !== input.url) {
          input = new Request(newUrl, input);
        }
      }
      return origFetch.call(this, input, init);
    };
  }

  var origOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(method, url, async, user, password) {
    if (typeof url === 'string') {
      url = patchUrl(url);
    }
    return origOpen.call(this, method, url, async !== false, user, password);
  };

  var origWebSocket = window.WebSocket;
  if (origWebSocket) {
    window.WebSocket = function(url, protocols) {
      if (typeof url === 'string' && (url.startsWith('ws://') || url.startsWith('wss://'))) {
        try {
          var parsed = new URL(url);
          if (parsed.pathname && !parsed.pathname.startsWith('/preview/') && !parsed.pathname.startsWith('/_multigent_preview/')) {
            parsed.pathname = prefix.replace(/\/$/, '') + parsed.pathname;
            url = parsed.toString();
          }
        } catch(err) {}
      }
      return protocols ? new origWebSocket(url, protocols) : new origWebSocket(url);
    };
  }
})();
</script>`, previewPrefix, previewPrefix, taskID, projectName, taskID, projectName)

	// Rewrite static HTML attributes: href="/...", src="/...", action="/..."
	html = htmlAttrRe.ReplaceAllStringFunc(html, func(match string) string {
		sub := htmlAttrRe.FindStringSubmatch(match)
		if len(sub) < 5 {
			return match
		}
		attr := sub[1]
		quote1 := sub[2]
		path := sub[3]
		quote2 := sub[4]

		if strings.HasPrefix(path, "preview/") || strings.HasPrefix(path, "_multigent_preview/") || strings.HasPrefix(path, "/") {
			return match
		}
		return fmt.Sprintf("%s=%s%s%s%s", attr, quote1, previewPrefix, path, quote2)
	})

	// Rewrite inline ES module imports: from "/...", import "/..."
	html = esmImportRe.ReplaceAllStringFunc(html, func(match string) string {
		sub := esmImportRe.FindStringSubmatch(match)
		if len(sub) < 5 {
			return match
		}
		prefix := sub[1]
		quote1 := sub[2]
		path := sub[3]
		quote2 := sub[4]

		if strings.HasPrefix(path, "preview/") || strings.HasPrefix(path, "_multigent_preview/") || strings.HasPrefix(path, "/") {
			return match
		}
		return fmt.Sprintf("%s%s%s%s%s", prefix, quote1, previewPrefix, path, quote2)
	})

	inspectorScript := fmt.Sprintf(`<script>
(function() {
  var isInspectorActive = false;
  var inspectorOverlay = null;
  var inspectorBadge = null;
  var expectedConsoleOrigin = %q;

  function ensureInspectorElements() {
    var parent = document.body || document.documentElement;
    if (!parent) return;

    if (!document.getElementById('__mg_inspector_styles__')) {
      var style = document.createElement('style');
      style.id = '__mg_inspector_styles__';
      style.textContent = '' +
        '.mg-inspecting, .mg-inspecting * { cursor: crosshair !important; }' +
        '.mg-inspector-overlay { position: fixed !important; pointer-events: none !important; border: 2px solid #0284c7 !important; background: rgba(2, 132, 199, 0.18) !important; border-radius: 4px !important; z-index: 2147483647 !important; margin: 0 !important; padding: 0 !important; display: none; box-sizing: border-box !important; transition: all 0.05s ease-out; box-shadow: 0 0 0 1px rgba(255, 255, 255, 0.8), 0 4px 12px rgba(2, 132, 199, 0.25) !important; }' +
        '.mg-inspector-badge { position: absolute !important; top: -24px !important; left: 0 !important; background: #0284c7 !important; color: #ffffff !important; font-size: 11px !important; font-weight: 500 !important; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace !important; padding: 2px 7px !important; border-radius: 4px !important; white-space: nowrap !important; pointer-events: none !important; box-shadow: 0 2px 6px rgba(0,0,0,0.3) !important; line-height: 14px !important; }';
      (document.head || parent).appendChild(style);
    }

    if (!inspectorOverlay) {
      inspectorOverlay = document.createElement('div');
      inspectorOverlay.className = 'mg-inspector-overlay';
      inspectorBadge = document.createElement('div');
      inspectorBadge.className = 'mg-inspector-badge';
      inspectorOverlay.appendChild(inspectorBadge);
      parent.appendChild(inspectorOverlay);
    } else if (inspectorOverlay.parentElement !== parent) {
      parent.appendChild(inspectorOverlay);
    }
  }

  function getCssSelector(el) {
    var path = [];
    while (el && el.nodeType === 1 && el !== document.body && el !== document.documentElement) {
      var tag = (el.tagName || el.nodeName || '').toLowerCase();
      if (!/^[a-z][a-z0-9-]{0,31}$/.test(tag)) {
        return '';
      }
      var seg = tag;
      var parent = el.parentElement;
      if (parent) {
        var siblings = Array.prototype.filter.call(parent.children, function(e) { return e.tagName === el.tagName; });
        if (siblings.length > 1) {
          var index = Array.prototype.indexOf.call(siblings, el) + 1;
          seg += ':nth-of-type(' + index + ')';
        }
      }
      path.unshift(seg);
      el = parent;
      if (path.length >= 15) break;
    }
    return path.join(' > ');
  }

  function onInspectorMouseMove(e) {
    if (!isInspectorActive) return;
    ensureInspectorElements();
    var el = document.elementFromPoint(e.clientX, e.clientY);
    if (!el || el === document.body || el === document.documentElement || (el.closest && el.closest('.mg-inspector-overlay'))) {
      if (inspectorOverlay) inspectorOverlay.style.display = 'none';
      return;
    }

    var rect = el.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) {
      if (inspectorOverlay) inspectorOverlay.style.display = 'none';
      return;
    }

    inspectorOverlay.style.display = 'block';
    inspectorOverlay.style.top = rect.top + 'px';
    inspectorOverlay.style.left = rect.left + 'px';
    inspectorOverlay.style.width = rect.width + 'px';
    inspectorOverlay.style.height = rect.height + 'px';

    var tagStr = el.tagName.toLowerCase();
    if (el.id && /^[A-Za-z][A-Za-z0-9_:-]{0,63}$/.test(el.id)) {
      tagStr += '#' + el.id;
    } else if (el.className && typeof el.className === 'string' && el.className.trim()) {
      var safeBadgeClasses = el.className.trim().split(/\s+/)
        .filter(function(c) { return /^[a-zA-Z0-9_\-:]+$/.test(c); })
        .slice(0, 2);
      if (safeBadgeClasses.length) tagStr += '.' + safeBadgeClasses.join('.');
    }
    inspectorBadge.textContent = '<' + tagStr + '> ' + Math.round(rect.width) + '×' + Math.round(rect.height);

    if (rect.top < 28) {
      inspectorBadge.style.top = 'auto';
      inspectorBadge.style.bottom = '-24px';
    } else {
      inspectorBadge.style.top = '-24px';
      inspectorBadge.style.bottom = 'auto';
    }
  }

  function extractElementContext(el) {
    var tag = (el.tagName || '').toLowerCase();
    if (!/^[a-z][a-z0-9-]{0,31}$/.test(tag)) {
      return null;
    }
    var id = '';
    if (el.id && /^[A-Za-z][A-Za-z0-9_:-]{0,63}$/.test(el.id)) {
      id = el.id;
    }
    var role = '';
    if (el.getAttribute && el.getAttribute('role')) {
      var rVal = (el.getAttribute('role') || '').trim();
      if (/^[a-zA-Z0-9_\-]{1,32}$/.test(rVal)) role = rVal;
    }

    var type = '';
    if (el.getAttribute && el.getAttribute('type')) {
      var tVal = (el.getAttribute('type') || '').trim();
      if (/^[a-zA-Z0-9_\-]{1,32}$/.test(tVal)) type = tVal;
    }

    var testId = '';
    if (el.getAttribute && el.getAttribute('data-testid')) {
      var tidVal = (el.getAttribute('data-testid') || '').trim();
      if (/^[A-Za-z][A-Za-z0-9_:-]{0,63}$/.test(tidVal)) testId = tidVal;
    }

    var selector = getCssSelector(el);

    // SECURITY (Phase 0): Absolutely zero input values, placeholders, text snippets, hrefs, raw markup, or parent trees.
    // DOM contract P1: send pure tag name for both tag and tagName; parent independently constructs display labels.
    return {
      tag: tag,
      tagName: tag,
      id: id,
      role: role,
      type: type,
      testId: testId,
      selector: selector
    };
  }

  function notifyHost(msg) {
    try {
      if (!expectedConsoleOrigin) return;
      if (window.parent && window.parent !== window) {
        window.parent.postMessage(msg, expectedConsoleOrigin);
      }
    } catch(e) {}
  }

  function onInspectorClick(e) {
    if (!isInspectorActive) return;
    var el = document.elementFromPoint(e.clientX, e.clientY);
    if (!el) return;

    e.preventDefault();
    e.stopPropagation();

    if (el !== document.body && el !== document.documentElement && (!el.closest || !el.closest('.mg-inspector-overlay'))) {
      var targetCtx = extractElementContext(el);
      if (targetCtx) {
        notifyHost({ type: 'MG_DOM_SELECTED', target: targetCtx });
      }
      stopInspector(false);
    }
  }

  function startInspector() {
    try {
      ensureInspectorElements();
    } catch(err) {
      console.warn('[Multigent Inspector] Error ensuring elements:', err);
    }
    isInspectorActive = true;
    document.documentElement.classList.add('mg-inspecting');
    try {
      if (document.body) document.body.style.setProperty('cursor', 'crosshair', 'important');
      if (document.documentElement) document.documentElement.style.setProperty('cursor', 'crosshair', 'important');
    } catch(e) {}
    document.addEventListener('mousemove', onInspectorMouseMove, true);
    document.addEventListener('click', onInspectorClick, true);
    notifyHost({ type: 'MG_INSPECTOR_ACTIVE' });
    console.log('[Multigent Inspector] Activated successfully');
  }

  function stopInspector(notify) {
    isInspectorActive = false;
    document.documentElement.classList.remove('mg-inspecting');
    try {
      if (document.body) document.body.style.removeProperty('cursor');
      if (document.documentElement) document.documentElement.style.removeProperty('cursor');
    } catch(e) {}
    if (inspectorOverlay) inspectorOverlay.style.display = 'none';
    document.removeEventListener('mousemove', onInspectorMouseMove, true);
    document.removeEventListener('click', onInspectorClick, true);
    notifyHost({ type: 'MG_INSPECTOR_INACTIVE' });
    if (notify) {
      notifyHost({ type: 'MG_DOM_CANCELLED' });
    }
    console.log('[Multigent Inspector] Deactivated');
  }

  window.addEventListener('message', function(e) {
    if (!e || !e.data || typeof e.data !== 'object') return;
    if (e.source !== window.parent) return;
    if (!expectedConsoleOrigin || e.origin !== expectedConsoleOrigin) return;
    if (e.data.type === 'MG_START_INSPECTOR') {
      startInspector();
    } else if (e.data.type === 'MG_STOP_INSPECTOR') {
      stopInspector(false);
    } else if (e.data.type === 'MG_PING_INSPECTOR') {
      notifyHost({ type: 'MG_PONG_INSPECTOR', active: isInspectorActive });
    }
  });

  window.addEventListener('keydown', function(e) {
    if (isInspectorActive && e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      stopInspector(true);
    }
  }, true);

  // Announce mount to host window immediately, and on DOMContentLoaded / load
  notifyHost({ type: 'MG_INSPECTOR_MOUNTED' });
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function() {
      notifyHost({ type: 'MG_INSPECTOR_MOUNTED' });
    });
  }
  window.addEventListener('load', function() {
    notifyHost({ type: 'MG_INSPECTOR_MOUNTED' });
  });
  console.log('[Multigent Inspector] Script mounted and listening');
})();
</script>`, consoleOrigin)

	fullScript := patchScript
	if consoleOrigin != "" {
		fullScript += inspectorScript
	}

	if strings.Contains(html, "<head>") {
		html = strings.Replace(html, "<head>", "<head>"+fullScript, 1)
	} else {
		html = fullScript + html
	}

	// §2.0.4: the Copilot widget is never injected into shared preview
	// documents — its UI lives in the authenticated console parent surface,
	// and control endpoints no longer accept preview tokens (Task 1.1).

	return html
}

func (s *Server) resolveTaskWorktreeDir(project, taskID string) string {
	task, agent, err := s.findTaskInProject(project, taskID)
	if err == nil && task != nil && strings.TrimSpace(task.WorktreeDir) != "" {
		if _, err := os.Stat(task.WorktreeDir); err == nil {
			return task.WorktreeDir
		}
	}

	// Check standard worktree path
	stdWt := gitworktree.WorktreeDir(s.st.ProjectDir(project), taskID)
	if _, err := os.Stat(stdWt); err == nil {
		return stdWt
	}

	// Check agent workspace path
	if task != nil && agent != "" {
		agentDir := filepath.Join(s.st.ProjectDir(project), "agents", agent)
		if _, err := os.Stat(agentDir); err == nil {
			return agentDir
		}
	}

	// Check project repo if configured
	if p, err := s.st.Project(project); err == nil && p != nil && p.Repo != "" {
		if _, err := os.Stat(p.Repo); err == nil {
			return p.Repo
		}
	}

	// Fallback to project workspace directory
	wsDir := filepath.Join(s.st.ProjectDir(project), "workspace")
	if _, err := os.Stat(wsDir); err == nil {
		return wsDir
	}
	return s.st.ProjectDir(project)
}

func (s *Server) previewInstanceReadOnly(taskID string) bool {
	if s == nil || s.previewEngine == nil {
		return false
	}
	inst, ok := s.previewEngine.GetInstance(strings.TrimSpace(taskID))
	return ok && inst != nil && inst.ReadOnly
}

func (s *Server) isTaskAtHumanReviewStep(workspaceID, project, taskID string) bool {
	if s == nil || s.controlDB == nil {
		return false
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, runFound, err := wfStore.RunForTask(project, taskID)
	if err != nil || !runFound || run.Status != "active" || strings.TrimSpace(run.ActiveStepID) == "" {
		return false
	}
	def, defFound, err := wfStore.RunDefinition(run)
	if err != nil || !defFound {
		return false
	}
	for _, step := range def.Steps {
		if step.ID == run.ActiveStepID {
			return step.Type == "human_review"
		}
	}
	return false
}

// resolveTaskPreviewRuntime resolves the managed runtime (profile + image) for
// a task's preview through sandbox.ResolveRuntime — the same authority chain
// the runner applies to agent sandboxes: an explicitly pinned agent image
// wins; otherwise the project-declared runtime profile is authoritative
// (templates/admins write it); an agent-level profile preference only applies
// in projects that declare none. Previews and agent sandboxes therefore
// always run the same image family for the same work.
func (s *Server) resolveTaskPreviewRuntime(project, taskID string) (preview.RuntimeSelection, error) {
	selection := preview.RuntimeSelection{}
	projectProfile := ""
	if s != nil && s.st != nil && strings.TrimSpace(project) != "" {
		if p, err := s.st.Project(project); err == nil && p != nil {
			projectProfile = p.RuntimeProfile
		}
		// A missing project here must not block preview startup with a confusing
		// error: resolution proceeds on the server default and the task-level
		// access checks already guard project existence.
	}
	agentProfile, agentImage := s.taskExecutingAgentRuntime(project, taskID)
	sel, err := sandbox.ResolveRuntime(sandbox.RuntimeRequest{
		ExplicitImage:  agentImage,
		AgentProfile:   agentProfile,
		ProjectProfile: projectProfile,
	})
	if err != nil {
		return selection, err
	}
	return preview.RuntimeSelection{Profile: sel.Profile, ImageRef: sel.ImageRef}, nil
}

// taskExecutingAgentRuntime extracts the executing agent's runtime preferences
// (its sandbox profile and explicit image, if any) so preview resolution can
// mirror the runner's decision for the same task. Best-effort: tasks without
// an agent assignee (or unloadable metas) resolve on project defaults.
func (s *Server) taskExecutingAgentRuntime(project, taskID string) (agentProfile, agentImage string) {
	task, _, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		return "", ""
	}
	if strings.TrimSpace(task.AssigneeType) != "" && task.AssigneeType != "agent_worker" {
		return "", ""
	}
	assignee := strings.TrimSpace(task.Assignee) // "<project>/<agent>"
	_, agent, ok := strings.Cut(assignee, "/")
	if !ok || strings.TrimSpace(agent) == "" {
		return "", ""
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return "", ""
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, strings.TrimSpace(agent))
	if err != nil || meta == nil || meta.Sandbox == nil {
		return "", ""
	}
	if meta.Sandbox.Docker != nil {
		agentProfile = meta.Sandbox.Docker.Profile
	}
	agentImage = strings.TrimSpace(meta.Sandbox.Image)
	if agentImage == "" && meta.Sandbox.Docker != nil {
		agentImage = strings.TrimSpace(meta.Sandbox.Docker.Image)
	}
	return agentProfile, agentImage
}
