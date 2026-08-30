package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	htmllib "html"
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

func (s *Server) handleListProjectBranches(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}

	gitRoot := s.resolveProjectGitRoot(project)
	branches := []string{"main"}
	if s.worktreeMgr != nil {
		if list, err := s.worktreeMgr.ListBranches(gitRoot); err == nil && len(list) > 0 {
			branches = list
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"project":  project,
		"branches": branches,
	})
}

func (s *Server) resolveProjectGitRoot(project string) string {
	projectRoot := s.st.ProjectDir(project)
	wsDir := filepath.Join(projectRoot, "workspace")
	if _, err := os.Stat(filepath.Join(wsDir, ".git")); err == nil {
		return wsDir
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
			"taskId":       taskID,
			"project":      project,
			"type":         string(projType),
			"status":       "stopped",
			"url":          fmt.Sprintf("/preview/%s/", taskID),
			"worktreeDir":  worktreeDir,
			"previewToken": s.signPreviewToken(taskID, project),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		*preview.PreviewInstance
		PreviewToken string `json:"previewToken,omitempty"`
	}{inst, s.signPreviewToken(taskID, inst.Project)})
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
	if readOnly {
		inst, err = s.previewEngine.StartSnapshotPreview(r.Context(), taskID, project, worktreeDir)
	} else {
		inst, err = s.previewEngine.StartEphemeralPreview(r.Context(), taskID, project, worktreeDir)
	}
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, fmt.Sprintf("start preview failed: %v", err))
		return
	}

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
	if !s.previewRequestAuthorized(w, r, project, taskID) {
		return
	}
	if s.previewInstanceReadOnly(taskID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "completed-task snapshot is read-only; create a follow-up task to modify it")
		return
	}

	var body previewFeedbackBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	feedback := strings.TrimSpace(body.Feedback)
	if feedback == "" {
		s.jsonError(w, http.StatusBadRequest, "feedback content is required")
		return
	}

	task, agentName, err := s.findTaskInProject(project, taskID)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}

	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}

	feedbackPrompt := fmt.Sprintf(
		"【预览界面即时修改反馈】用户在特性分支 (Worktree) 的实时预览环境中提出了以下修改要求：\n\n%s\n\n请严格在当前 Worktree 目录 (/workspace) 内完成代码修改，并确保本地服务热重载正常，严禁切换分支。",
		feedback,
	)

	author := "user"
	if cur := s.currentUser(r); cur != nil && strings.TrimSpace(cur.Username) != "" {
		author = cur.Username
	}

	// Append feedback to task comments
	_ = s.ts.AddComment(project, agentName, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    taskID,
		Author:    author,
		Body:      "[Preview Feedback] " + feedback,
		CreatedAt: time.Now().UTC(),
	})

	// Wake up agent in current worktree
	signalID := s.recordTaskAttentionSignal(workspaceID, project, agentName, task, "preview_feedback")
	s.requestTaskAttentionWakeup(workspaceID, project, agentName, task, "preview_feedback", signalID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"taskId":  taskID,
		"prompt":  feedbackPrompt,
		"message": "Feedback submitted to agent",
	})
}

func (s *Server) handlePostTaskPreviewChat(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))

	if (project == "" || project == "current") && s.previewEngine != nil {
		if inst, ok := s.previewEngine.GetInstance(taskID); ok && inst.Project != "" {
			project = inst.Project
		}
	}
	if !s.previewRequestAuthorized(w, r, project, taskID) {
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

	var body previewChatBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		s.jsonError(w, http.StatusBadRequest, "message content is required")
		return
	}

	task, agentName, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}

	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.jsonError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// 1. Branch Mutex check
	s.previewMu.Lock()
	if existing, busy := s.previewSessions[taskID]; busy && existing != nil && !existing.Done && !existing.Stopped {
		s.previewMu.Unlock()
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "Agent 正在修改当前分支代码，请稍候或点击中止")
		return
	}

	// Detached background context: page refresh will NOT kill the agent!
	ctx, cancel := context.WithCancel(context.Background())
	session := &previewChatSession{
		TaskID:      taskID,
		Project:     project,
		Agent:       agentName,
		StartedAt:   time.Now(),
		Cancel:      cancel,
		Subscribers: make(map[chan string]struct{}),
	}
	s.previewSessions[taskID] = session
	s.previewMu.Unlock()

	// 2. Record user comment
	author := "user"
	if cur := s.currentUser(r); cur != nil && strings.TrimSpace(cur.Username) != "" {
		author = cur.Username
	}
	_ = s.ts.AddComment(project, agentName, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    taskID,
		Author:    author,
		Body:      "[Preview Chat] " + msg,
		CreatedAt: time.Now().UTC(),
	})

	// 3. Build Prompt with conversation history
	var promptBuf strings.Builder
	promptBuf.WriteString("【预览界面即时修改】用户在特性分支 (Worktree) 的实时预览环境中提出了代码修改要求：\n\n")
	for _, h := range body.History {
		roleLabel := "用户"
		if h.Role == "assistant" {
			roleLabel = "助手"
		}
		promptBuf.WriteString(fmt.Sprintf("%s: %s\n", roleLabel, h.Content))
	}
	promptBuf.WriteString(fmt.Sprintf("\n用户最新修改需求: %s\n\n", msg))
	promptBuf.WriteString("【重要准则】请严格在当前 Worktree 目录 (/workspace) 内完成代码修改，并确保本地服务热重载正常，严禁切换分支。")
	promptText := promptBuf.String()

	// 4. Start execution command in background goroutine
	args := []string{"--dir", s.root, "exec", "--project", project, "--agent", agentName, "--prompt", promptText, "--no-save-session", "--no-session"}
	cmd := exec.CommandContext(ctx, s.sched.binPath, args...)
	cmd.Dir = s.root
	runID := "preview-exec-" + time.Now().UTC().Format("20060102-150405")
	runtimeToken := s.issueAgentRuntimeToken(runtimeAgentTokenPayload{
		WorkspaceID:  workspaceID,
		Project:      project,
		Agent:        agentName,
		RunID:        runID,
		Capabilities: defaultRuntimeCapabilities(),
	}, 2*time.Hour)
	cmd.Env = append(os.Environ(),
		"MULTIGENT_API_URL="+localRuntimeAPIURLForRequest(r),
		"MULTIGENT_AGENT_TOKEN="+runtimeToken,
		"MULTIGENT_RUN_ID="+runID,
		"MULTIGENT_WORKSPACE_ID="+workspaceID,
		"MULTIGENT_WORKTREE_DIR="+worktreeDir,
	)
	setProcGroup(cmd)
	session.Cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		session.finish(true)
		s.serverError(w, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		session.finish(true)
		s.serverError(w, err)
		return
	}

	if err := cmd.Start(); err != nil {
		session.finish(true)
		s.serverError(w, err)
		return
	}

	agentModel := entity.AgentModel("")
	if meta, err := s.agentMetaForProjectMember(workspaceID, project, agentName); err == nil && meta != nil {
		agentModel = meta.Model
	}

	go func() {
		lines := make(chan string, 64)
		var wg sync.WaitGroup
		scan := func(src io.Reader) {
			defer wg.Done()
			scanner := bufio.NewScanner(src)
			scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
			for scanner.Scan() {
				line := strings.TrimRight(scanner.Text(), "\r")
				if line != "" {
					lines <- line
				}
			}
		}
		wg.Add(2)
		go scan(stdout)
		go scan(stderr)
		go func() {
			wg.Wait()
			close(lines)
		}()

		for line := range lines {
			payload := chatSSEPayload(line, agentModel)
			session.broadcast(payload)
		}

		_ = cmd.Wait()
		session.finish(ctx.Err() != nil)
	}()

	// Subscribe current HTTP request to the live stream
	subCh, history := session.addSubscriber()
	defer session.removeSubscriber(subCh)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	for _, p := range history {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", p); err != nil {
			return
		}
		flusher.Flush()
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

func (s *Server) handleGetTaskPreviewLive(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if !s.previewRequestAuthorized(w, r, r.PathValue("name"), taskID) {
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
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if !s.previewRequestAuthorized(w, r, r.PathValue("name"), taskID) {
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"stopped": exists,
		"taskId":  taskID,
	})
}

func (s *Server) handleGetTaskPreviewStatus(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if !s.previewRequestAuthorized(w, r, r.PathValue("name"), taskID) {
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
		"taskId":    taskID,
		"busy":      busy,
		"agent":     agent,
		"startedAt": startedAt,
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

	if s.previewEngine == nil {
		s.previewEngine = preview.NewEngine()
	}

	inst, ok := s.previewEngine.GetInstance(taskID)
	if !ok || inst.Status != "running" || inst.Port <= 0 {
		http.Error(w, fmt.Sprintf("Preview environment for task %q is not running. Please launch it from the task review panel.", taskID), http.StatusServiceUnavailable)
		return
	}

	previewToken := previewRequestToken(r, taskID)
	if _, tokOK := s.verifyPreviewToken(previewToken, taskID); !tokOK {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeUnauthorized, "preview token required")
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

		html := rewriteHTML(string(bodyBytes), taskID, inst.Project, previewToken)

		// Persist the validated token as a path-scoped cookie so subsequent
		// sub-resource requests (which never propagate ?pvt=) still
		// authenticate, while remaining invisible to other origins.
		previewCookie := &http.Cookie{
			Name:     "mg_pvt_" + taskID,
			Value:    previewToken,
			Path:     fmt.Sprintf("/preview/%s/", taskID),
			MaxAge:   int(previewTokenTTL.Seconds()),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		}
		resp.Header.Add("Set-Cookie", previewCookie.String())

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
		return nil
	}

	proxy.ServeHTTP(w, r)
}

var htmlAttrRe = regexp.MustCompile(`(?i)\b(href|src|action)\s*=\s*(["'])/([^"']*)(["'])`)

func rewriteHTML(html, taskID, projectName, previewToken string) string {
	previewPrefix := fmt.Sprintf("/preview/%s/", taskID)

	// Interceptor script to handle dynamic fetches, XMLHttpRequest, WebSocket, and SPA History Navigation
	patchScript := fmt.Sprintf(`<base href=%q><script>
(function(){
  var prefix = %q;
  window.__MG_PREVIEW_TASK_ID__ = %q;
  window.__MG_PREVIEW_PROJECT__ = %q;
  window.__MG_PREVIEW_TOKEN__ = %q;
  window.__MG_PREVIEW_BASE__ = prefix.replace(/\/$/, '');
  var controlPrefix = '/api/v1/projects/' + encodeURIComponent(%q) + '/tasks/' + encodeURIComponent(%q) + '/preview/';
  try {
    sessionStorage.setItem('__mg_preview_task_id', %q);
    sessionStorage.setItem('__mg_preview_project', %q);
  } catch(e) {}

  function patchUrl(u) {
    if (typeof u === 'string' && u.startsWith(controlPrefix)) {
      return u;
    }
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
      } else if (input && typeof input.url === 'string') {
        input = new Request(patchUrl(input.url), input);
      }
      return origFetch.call(this, input, init);
    };
  }
  var origOpen = XMLHttpRequest.prototype.open;
  if (origOpen) {
    XMLHttpRequest.prototype.open = function(method, url) {
      var args = Array.prototype.slice.call(arguments);
      args[1] = patchUrl(url);
      return origOpen.apply(this, args);
    };
  }
  var origWS = window.WebSocket;
  if (origWS) {
    window.WebSocket = function(url, protocols) {
      if (typeof url === 'string') {
        try {
          var parsed = new URL(url);
          if (parsed.pathname === '/' || !parsed.pathname.startsWith('/preview/')) {
            parsed.pathname = prefix + (parsed.pathname.startsWith('/') ? parsed.pathname.slice(1) : parsed.pathname);
            url = parsed.toString();
          }
        } catch(e) {}
      }
      return protocols !== undefined ? new origWS(url, protocols) : new origWS(url);
    };
    window.WebSocket.prototype = origWS.prototype;
  }
})();
</script>`, previewPrefix, previewPrefix, taskID, projectName, previewToken, projectName, taskID, taskID, projectName)

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

	if strings.Contains(html, "<head>") {
		html = strings.Replace(html, "<head>", "<head>"+patchScript, 1)
	} else {
		html = patchScript + html
	}

	widgetTag := fmt.Sprintf(
		`<script src="/_multigent_preview/feedback.js" data-task-id="%s" data-project="%s"></script>`,
		htmllib.EscapeString(taskID), htmllib.EscapeString(projectName),
	)

	if strings.Contains(html, "</body>") {
		html = strings.Replace(html, "</body>", widgetTag+"</body>", 1)
	} else {
		html += widgetTag
	}

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
