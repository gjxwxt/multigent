package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/preview"
)

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
			"taskId":      taskID,
			"project":     project,
			"type":        string(projType),
			"status":      "stopped",
			"url":         fmt.Sprintf("/preview/%s/", taskID),
			"worktreeDir": worktreeDir,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(inst)
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
	inst, err := s.previewEngine.StartEphemeralPreview(r.Context(), taskID, project, worktreeDir)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, fmt.Sprintf("start preview failed: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(inst)
}

func (s *Server) handlePostTaskPreviewStop(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}

	if s.previewEngine != nil {
		_ = s.previewEngine.StopEphemeralPreview(taskID)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
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

		html := rewriteHTML(string(bodyBytes), taskID, inst.Project)

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

func rewriteHTML(html, taskID, projectName string) string {
	previewPrefix := fmt.Sprintf("/preview/%s/", taskID)

	// Interceptor script to handle dynamic fetches and XMLHttpRequest
	patchScript := fmt.Sprintf(`<script>
(function(){
  var prefix = %q;
  function patchUrl(u) {
    if (typeof u === 'string' && u.startsWith('/') && !u.startsWith('/preview/') && !u.startsWith('/_multigent_preview/') && !u.startsWith('/api/v1/')) {
      return prefix + u.slice(1);
    }
    return u;
  }
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
})();
</script>`, previewPrefix)

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
		taskID, projectName,
	)

	if strings.Contains(html, "</body>") {
		html = strings.Replace(html, "</body>", widgetTag+"</body>", 1)
	} else {
		html += widgetTag
	}

	return html
}

func (s *Server) resolveTaskWorktreeDir(project, taskID string) string {
	task, _, err := s.findTaskInProject(project, taskID)
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

	// Fallback to project workspace directory
	wsDir := filepath.Join(s.st.ProjectDir(project), "workspace")
	if _, err := os.Stat(wsDir); err == nil {
		return wsDir
	}
	return s.st.ProjectDir(project)
}
