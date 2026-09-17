package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/previewreceipt"
)

type previewDefaultAgentRunner struct {
	server      *Server
	workspaceID string
	project     string
	agentName   string
	runtimeURL  string
}

func (r *previewDefaultAgentRunner) RunAgent(ctx context.Context, cloneDir string, prompt string) error {
	if r.server == nil || r.server.sched == nil || strings.TrimSpace(r.server.sched.binPath) == "" {
		return nil
	}
	args := []string{"--dir", r.server.root, "exec", "--project", r.project, "--agent", r.agentName, "--prompt", prompt, "--no-save-session", "--no-session"}
	cmd := exec.CommandContext(ctx, r.server.sched.binPath, args...)
	cmd.Dir = r.server.root
	r.server.configureAgentExecEnv(cmd, r.workspaceID, r.project, r.agentName, r.runtimeURL, map[string]string{
		"MULTIGENT_WORKTREE_DIR": cloneDir,
	})
	setProcGroup(cmd)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("agent execution in clone failed: %w (output: %s)", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func (s *Server) newPreviewAgentRunner(workspaceID, project, agentName, runtimeURL string) previewreceipt.AgentRunner {
	if s.previewAgentRunnerFunc != nil {
		return s.previewAgentRunnerFunc(workspaceID, project, agentName, runtimeURL)
	}
	return &previewDefaultAgentRunner{
		server:      s,
		workspaceID: workspaceID,
		project:     project,
		agentName:   agentName,
		runtimeURL:  runtimeURL,
	}
}

func (s *Server) buildPreviewChatPrompt(project, taskID, worktreeDir, msg string, history []previewChatMsg) string {
	var promptBuf strings.Builder
	promptBuf.WriteString("【预览界面即时修改】用户在特性分支 (Worktree) 的实时预览环境中提出了代码修改要求。\n\n")
	promptBuf.WriteString(s.buildPreviewEnvSnapshot(project, taskID, worktreeDir))
	promptBuf.WriteString("\n\n以下为此前对话记录(仅供理解意图):\n\n")
	for _, h := range history {
		roleLabel := "用户"
		if h.Role == "assistant" {
			roleLabel = "助手"
		}
		promptBuf.WriteString(fmt.Sprintf("%s: %s\n", roleLabel, h.Content))
	}
	promptBuf.WriteString(fmt.Sprintf("\n用户最新修改需求: %s\n\n", msg))
	promptBuf.WriteString("【重要准则】请严格在当前 Worktree 目录 (/workspace) 内完成代码修改，并确保本地服务热重载正常，严禁切换分支。")
	return promptBuf.String()
}

func (s *Server) executePreviewChatTurn(w http.ResponseWriter, r *http.Request, project, taskID string, task *entity.Task, principal previewPrincipal) {
	workspaceID := s.currentWorkspaceIDValue(r)

	// Invariant: Concurrent write lock. Allow interactive Copilot only when
	// the task is at a human review step or awaiting confirmation.
	if task.Status == entity.TaskStatusInProgress && !s.isTaskAtHumanReviewStep(workspaceID, project, taskID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "当前节点正由智能体后台执行中。待流转至人工审核节点后即可进行代码即时调优。")
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

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	if worktreeDir == "" {
		s.jsonError(w, http.StatusInternalServerError, "worktree directory not found")
		return
	}
	projectGitRoot := gitworktree.ProjectRootForWorktree(worktreeDir)

	_, agentName, _ := s.findTaskInProject(project, taskID)
	promptText := s.buildPreviewChatPrompt(project, taskID, worktreeDir, msg, body.History)

	engine := s.turnEngine(r)
	if engine == nil {
		s.jsonError(w, http.StatusInternalServerError, "turn engine unavailable")
		return
	}

	runner := s.newPreviewAgentRunner(workspaceID, project, agentName, localRuntimeAPIURLForRequest(r))

	receipt, err := engine.ExecuteTurn(r.Context(), previewreceipt.ExecuteTurnParams{
		WorkspaceID:    workspaceID,
		Project:        project,
		ProjectGitRoot: projectGitRoot,
		TaskID:         taskID,
		WorktreeDir:    worktreeDir,
		Prompt:         promptText,
		Actor:          principal.Username,
		Runner:         runner,
		LeaseDuration:  5 * time.Minute,
	})
	if err != nil {
		if errors.Is(err, previewreceipt.ErrConflict) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, err.Error())
			return
		}
		s.jsonErrorCode(w, http.StatusInternalServerError, "turn_execution_failed", err.Error())
		return
	}

	// Add audit comment to task
	_ = s.ts.AddComment(project, agentName, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    taskID,
		Author:    principal.Username,
		Body:      fmt.Sprintf("[Preview Copilot Turn %s] %s", receipt.ID, msg),
		CreatedAt: time.Now().UTC(),
	})

	// Check if client expects SSE stream
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		flusher, ok := w.(http.Flusher)
		if ok {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			flusher.Flush()

			doneMsg := fmt.Sprintf("修改已成功捕获并应用 (Turn %s)。\n\n```diff\n%s\n```", receipt.ID, receipt.DisplayDiff)
			msgPayload, _ := json.Marshal(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": doneMsg},
					},
				},
			})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msgPayload)
			flusher.Flush()

			resPayload, _ := json.Marshal(map[string]any{
				"type":    "result",
				"result":  doneMsg,
				"turnId":  receipt.ID,
				"receipt": receipt,
			})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", resPayload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"done"}`)
			flusher.Flush()
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"taskId":       taskID,
		"turnId":       receipt.ID,
		"status":       receipt.Status,
		"displayDiff":  receipt.DisplayDiff,
		"touchedPaths": receipt.TouchedPaths,
		"receipt":      receipt,
	})
}

func (s *Server) handleGetTaskPreviewTurns(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	task := s.projectTaskResourceGuard(w, r, project, taskID)
	if task == nil {
		return
	}

	store := s.receiptStore(r)
	if store == nil {
		s.jsonError(w, http.StatusInternalServerError, "receipt store unavailable")
		return
	}

	receipts, err := store.List(r.Context(), project, taskID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if receipts == nil {
		receipts = []*previewreceipt.PreviewReceipt{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"taskId":   taskID,
		"receipts": receipts,
	})
}

func (s *Server) handleGetTaskPreviewTurnDiff(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	turnID := strings.TrimSpace(r.PathValue("turnId"))
	task := s.projectTaskResourceGuard(w, r, project, taskID)
	if task == nil {
		return
	}

	store := s.receiptStore(r)
	if store == nil {
		s.jsonError(w, http.StatusInternalServerError, "receipt store unavailable")
		return
	}

	receipt, err := store.Get(r.Context(), project, taskID, turnID)
	if err != nil {
		if errors.Is(err, previewreceipt.ErrNotFound) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "turn not found")
			return
		}
		s.serverError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"taskId":       taskID,
		"id":           receipt.ID,
		"turnId":       receipt.TurnID,
		"status":       receipt.Status,
		"displayDiff":  receipt.DisplayDiff,
		"touchedPaths": receipt.TouchedPaths,
	})
}

func (s *Server) handlePostTaskPreviewTurnRollback(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	turnID := strings.TrimSpace(r.PathValue("turnId"))

	authReq, principal, ok := s.previewWritePrincipal(w, r, project, taskID)
	if !ok {
		return
	}
	r = authReq

	task, agentName, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if task.Status != entity.TaskStatusInProgress {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "task is not in progress; modifying completed or inactive task code is forbidden")
		return
	}
	if s.previewInstanceReadOnly(taskID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "completed-task snapshot is read-only; rollback forbidden")
		return
	}

	workspaceID := s.currentWorkspaceIDValue(r)
	if !s.isTaskAtHumanReviewStep(workspaceID, project, taskID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "当前节点正由智能体后台执行中。待流转至人工审核节点后即可进行回滚调优。")
		return
	}

	store := s.receiptStore(r)
	if store == nil {
		s.jsonError(w, http.StatusInternalServerError, "receipt store unavailable")
		return
	}

	receipts, err := store.List(r.Context(), project, taskID)
	if err == nil {
		for _, rec := range receipts {
			if rec.Status == previewreceipt.StatusCommitting {
				s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "cannot rollback turn while changes are committing to git")
				return
			}
		}
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	if worktreeDir == "" {
		s.jsonError(w, http.StatusInternalServerError, "worktree directory not found")
		return
	}
	projectGitRoot := gitworktree.ProjectRootForWorktree(worktreeDir)

	engine := s.turnEngine(r)
	if engine == nil {
		s.jsonError(w, http.StatusInternalServerError, "turn engine unavailable")
		return
	}

	err = engine.RollbackTurn(r.Context(), previewreceipt.RollbackTurnParams{
		Project:        project,
		ProjectGitRoot: projectGitRoot,
		TaskID:         taskID,
		TurnID:         turnID,
		WorktreeDir:    worktreeDir,
		Actor:          principal.Username,
	})
	if err != nil {
		if errors.Is(err, previewreceipt.ErrNotFound) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "turn receipt not found")
			return
		}
		if errors.Is(err, previewreceipt.ErrConflict) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, fmt.Sprintf("rollback conflict: %v", err))
			return
		}
		s.serverError(w, err)
		return
	}

	_ = s.ts.AddComment(project, agentName, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    taskID,
		Author:    principal.Username,
		Body:      fmt.Sprintf("[Preview Copilot] Rolled back turn %s", turnID),
		CreatedAt: time.Now().UTC(),
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"taskId": taskID,
		"turnId": turnID,
		"status": previewreceipt.StatusRolledBack,
	})
}

func (s *Server) handlePostTaskPreviewTurnPreviewStart(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	turnID := strings.TrimSpace(r.PathValue("turnId"))

	authReq, _, ok := s.previewWritePrincipal(w, r, project, taskID)
	if !ok {
		return
	}
	r = authReq

	if s.previewEngine == nil {
		s.jsonError(w, http.StatusInternalServerError, "preview engine not initialized")
		return
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	if worktreeDir == "" {
		s.jsonError(w, http.StatusInternalServerError, "worktree directory not found")
		return
	}

	runtime, err := s.resolveTaskPreviewRuntime(project, taskID)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to resolve preview runtime: "+err.Error())
		return
	}

	inst, err := s.previewEngine.StartTurnPreviewWithRuntime(r.Context(), taskID, turnID, project, worktreeDir, runtime)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to start turn preview: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"taskId":   taskID,
		"turnId":   turnID,
		"instance": inst,
	})
}

func (s *Server) handlePostTaskPreviewTurnPreviewStop(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	turnID := strings.TrimSpace(r.PathValue("turnId"))

	authReq, _, ok := s.previewWritePrincipal(w, r, project, taskID)
	if !ok {
		return
	}
	r = authReq

	if s.previewEngine == nil {
		s.jsonError(w, http.StatusInternalServerError, "preview engine not initialized")
		return
	}

	err := s.previewEngine.StopTurnPreview(taskID, turnID)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to stop turn preview: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"taskId":  taskID,
		"turnId":  turnID,
		"stopped": true,
	})
}

func (s *Server) handleGetTaskPreviewTurnPreviewStatus(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	turnID := strings.TrimSpace(r.PathValue("turnId"))

	if !s.checkProjectAccess(w, r, project) {
		return
	}

	if s.previewEngine == nil {
		s.jsonError(w, http.StatusInternalServerError, "preview engine not initialized")
		return
	}

	inst, ok := s.previewEngine.GetTurnInstance(taskID, turnID)
	if !ok || inst == nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "turn preview instance not running")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"taskId":   taskID,
		"turnId":   turnID,
		"instance": inst,
	})
}
