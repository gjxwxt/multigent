package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/previewreceipt"
	"github.com/multigent/multigent/internal/sandbox"
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
		return errors.New("agent runner scheduler unavailable")
	}

	// Invariant: Preview Copilot requires an isolated container sandbox; host fallback is forbidden.
	meta, err := r.server.agentMetaForProjectMember(r.workspaceID, r.project, r.agentName)
	if err != nil {
		return fmt.Errorf("load agent metadata: %w", err)
	}
	if meta == nil || meta.Sandbox == nil || meta.Sandbox.Provider == "" || meta.Sandbox.Provider == entity.SandboxNone {
		return errors.New("preview copilot requires an isolated container sandbox; host execution is forbidden")
	}
	if entity.NormaliseModel(meta.Model) == entity.ModelHTTPAgent {
		return errors.New("preview copilot does not support HTTP agents; isolated container sandbox required")
	}

	if meta.Sandbox.Provider != entity.SandboxDocker {
		return fmt.Errorf("preview copilot requires docker sandbox, got %q", meta.Sandbox.Provider)
	}
	if len(meta.Sandbox.Mounts) > 0 {
		return fmt.Errorf("preview copilot isolated run rejects custom mounts: %d mounts configured", len(meta.Sandbox.Mounts))
	}
	if meta.Sandbox.Docker != nil {
		for _, v := range meta.Sandbox.Docker.ExtraVolumes {
			if strings.Contains(v, "docker.sock") {
				return errors.New("preview copilot isolated run strictly forbids Docker socket")
			}
		}
		for _, e := range meta.Sandbox.Docker.ExtraEnv {
			if strings.Contains(e, "docker.sock") {
				return errors.New("preview copilot isolated run strictly forbids Docker socket in environment")
			}
		}
		if len(meta.Sandbox.Docker.ExtraVolumes) > 0 {
			return fmt.Errorf("preview copilot isolated run rejects ExtraVolumes: %v", meta.Sandbox.Docker.ExtraVolumes)
		}
		if len(meta.Sandbox.Docker.CredentialMounts) > 0 {
			return fmt.Errorf("preview copilot isolated run rejects CredentialMounts: %v", meta.Sandbox.Docker.CredentialMounts)
		}
	}

	if err := sandbox.CheckDocker(); err != nil {
		return fmt.Errorf("docker sandbox unavailable: %w", err)
	}

	args := []string{"--dir", r.server.root, "exec", "--project", r.project, "--agent", r.agentName, "--prompt", prompt, "--no-save-session", "--no-session"}
	cmd := exec.CommandContext(ctx, r.server.sched.binPath, args...)
	cmd.Dir = r.server.root
	r.server.configureAgentExecEnv(cmd, r.workspaceID, r.project, r.agentName, r.runtimeURL, map[string]string{
		"MULTIGENT_WORKTREE_DIR":         cloneDir,
		"MULTIGENT_PREVIEW_ISOLATED_RUN": "1",
	})
	setProcGroup(cmd)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("agent execution in clone failed: %w (output: %s)", err, previewreceipt.RedactSecrets(strings.TrimSpace(out.String())))
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

func (s *Server) buildPreviewChatPrompt(project, taskID, worktreeDir, msg string, history []previewChatMsg, profileGuidance string) string {
	var promptBuf strings.Builder
	promptBuf.WriteString("【预览界面即时修改】用户在特性分支 (Worktree) 的实时预览环境中提出了代码修改要求。\n\n")
	promptBuf.WriteString(s.buildPreviewEnvSnapshot(project, taskID, worktreeDir))
	if strings.TrimSpace(profileGuidance) != "" {
		promptBuf.WriteString("\n\n" + strings.TrimSpace(profileGuidance))
	}
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
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			ActorType:    "user",
			ActorID:      principal.Username,
			Action:       "preview_turn.execution_rejected",
			ResourceType: "preview_turn",
			ResourceID:   taskID,
			Summary:      fmt.Sprintf("preview turn execution rejected for task %s: not at human review step", taskID),
			After: map[string]any{
				"project":   project,
				"taskId":    taskID,
				"errorCode": ErrCodeConflict,
			},
			Request: r,
		})
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

	var profileGuidance string
	if strings.TrimSpace(body.Profile) != "" {
		guidance, err := ResolveSkillProfileGuidance(s.st, body.Profile)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, fmt.Sprintf("invalid skill profile: %v", err))
			return
		}
		profileGuidance = guidance
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	if worktreeDir == "" {
		s.jsonError(w, http.StatusInternalServerError, "worktree directory not found")
		return
	}
	projectGitRoot := gitworktree.ProjectRootForWorktree(worktreeDir)

	_, agentName, _ := s.findTaskInProject(project, taskID)
	promptText := s.buildPreviewChatPrompt(project, taskID, worktreeDir, msg, body.History, profileGuidance)

	engine := s.turnEngine(r)
	if engine == nil {
		s.jsonError(w, http.StatusInternalServerError, "turn engine unavailable")
		return
	}

	runner := s.newPreviewAgentRunner(workspaceID, project, agentName, localRuntimeAPIURLForRequest(r))

	execCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	session := &previewChatSession{
		TaskID:    taskID,
		Project:   project,
		Agent:     agentName,
		StartedAt: time.Now().UTC(),
		Cancel:    cancel,
	}
	s.previewMu.Lock()
	if s.previewSessions == nil {
		s.previewSessions = make(map[string]*previewChatSession)
	}
	s.previewSessions[taskID] = session
	s.previewMu.Unlock()

	defer func() {
		s.previewMu.Lock()
		if s.previewSessions != nil && s.previewSessions[taskID] == session {
			delete(s.previewSessions, taskID)
		}
		session.finish(false)
		s.previewMu.Unlock()
	}()

	receipt, err := engine.ExecuteTurn(execCtx, previewreceipt.ExecuteTurnParams{
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
		errCode := "turn_execution_failed"
		httpStatus := http.StatusInternalServerError
		if errors.Is(err, previewreceipt.ErrConflict) {
			errCode = ErrCodeConflict
			httpStatus = http.StatusConflict
		}
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			ActorType:    "user",
			ActorID:      principal.Username,
			Action:       "preview_turn.execution_rejected",
			ResourceType: "preview_turn",
			ResourceID:   taskID,
			Summary:      fmt.Sprintf("preview turn execution failed for task %s: %s", taskID, errCode),
			After: map[string]any{
				"project":   project,
				"taskId":    taskID,
				"errorCode": errCode,
			},
			Request: r,
		})
		s.jsonErrorCode(w, httpStatus, errCode, previewreceipt.RedactSecrets(err.Error()))
		return
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		ActorType:    "user",
		ActorID:      principal.Username,
		Action:       "preview_turn.executed",
		ResourceType: "preview_turn",
		ResourceID:   receipt.ID,
		Summary:      fmt.Sprintf("preview turn %s executed for task %s", receipt.ID, taskID),
		After: map[string]any{
			"project":   project,
			"taskId":    taskID,
			"turnId":    receipt.TurnID,
			"receiptId": receipt.ID,
			"status":    receipt.Status,
		},
		Request: r,
	})

	// Add audit comment to task
	_ = s.ts.AddComment(project, agentName, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    taskID,
		Author:    principal.Username,
		Body:      previewreceipt.RedactSecrets(fmt.Sprintf("[Preview Copilot Turn %s] %s", receipt.ID, msg)),
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

	if !s.PreviewTurnReceiptsEnabledForProject(project) {
		s.jsonErrorCode(w, http.StatusConflict, "feature_disabled", s.PreviewTurnReceiptsDisabledReasonForProject(project))
		return
	}

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
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			ActorType:    "user",
			ActorID:      principal.Username,
			Action:       "preview_turn.rollback_rejected",
			ResourceType: "preview_turn",
			ResourceID:   turnID,
			Summary:      fmt.Sprintf("preview turn rollback rejected for turn %s in task %s: not at human review step", turnID, taskID),
			After: map[string]any{
				"project":   project,
				"taskId":    taskID,
				"turnId":    turnID,
				"errorCode": ErrCodeConflict,
			},
			Request: r,
		})
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
				s.auditLog(auditLogInput{
					WorkspaceID:  workspaceID,
					ActorType:    "user",
					ActorID:      principal.Username,
					Action:       "preview_turn.rollback_rejected",
					ResourceType: "preview_turn",
					ResourceID:   turnID,
					Summary:      fmt.Sprintf("preview turn rollback rejected for turn %s in task %s: changes are committing to git", turnID, taskID),
					After: map[string]any{
						"project":   project,
						"taskId":    taskID,
						"turnId":    turnID,
						"errorCode": ErrCodeConflict,
					},
					Request: r,
				})
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
		errCode := "rollback_failed"
		httpStatus := http.StatusInternalServerError
		if errors.Is(err, previewreceipt.ErrNotFound) {
			errCode = ErrCodeNotFound
			httpStatus = http.StatusNotFound
		} else if errors.Is(err, previewreceipt.ErrConflict) {
			errCode = ErrCodeConflict
			httpStatus = http.StatusConflict
		}
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			ActorType:    "user",
			ActorID:      principal.Username,
			Action:       "preview_turn.rollback_rejected",
			ResourceType: "preview_turn",
			ResourceID:   turnID,
			Summary:      fmt.Sprintf("preview turn rollback failed for turn %s in task %s: %s", turnID, taskID, errCode),
			After: map[string]any{
				"project":   project,
				"taskId":    taskID,
				"turnId":    turnID,
				"errorCode": errCode,
			},
			Request: r,
		})
		if errors.Is(err, previewreceipt.ErrNotFound) {
			s.jsonErrorCode(w, httpStatus, errCode, "turn receipt not found")
			return
		}
		if errors.Is(err, previewreceipt.ErrConflict) {
			s.jsonErrorCode(w, httpStatus, errCode, fmt.Sprintf("rollback conflict: %v", err))
			return
		}
		s.serverError(w, err)
		return
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		ActorType:    "user",
		ActorID:      principal.Username,
		Action:       "preview_turn.rolled_back",
		ResourceType: "preview_turn",
		ResourceID:   turnID,
		Summary:      fmt.Sprintf("preview turn %s rolled back for task %s", turnID, taskID),
		After: map[string]any{
			"project": project,
			"taskId":  taskID,
			"turnId":  turnID,
			"status":  previewreceipt.StatusRolledBack,
		},
		Request: r,
	})

	_ = s.ts.AddComment(project, agentName, &entity.TaskComment{
		ID:        entity.NewCommentID(),
		TaskID:    taskID,
		Author:    principal.Username,
		Body:      previewreceipt.RedactSecrets(fmt.Sprintf("[Preview Copilot] Rolled back turn %s", turnID)),
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

	if !s.enablePreviewTurnReceipts {
		s.jsonErrorCode(w, http.StatusForbidden, "feature_disabled", "preview turn receipts are disabled")
		return
	}

	if s.previewEngine == nil {
		s.jsonError(w, http.StatusInternalServerError, "preview engine not initialized")
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
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "turn receipt not found")
			return
		}
		s.serverError(w, err)
		return
	}

	if receipt.Status != previewreceipt.StatusCaptured {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, fmt.Sprintf("turn %s in status %s cannot be previewed (must be captured)", turnID, receipt.Status))
		return
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	if worktreeDir == "" {
		s.jsonError(w, http.StatusInternalServerError, "worktree directory not found")
		return
	}
	projectGitRoot := gitworktree.ProjectRootForWorktree(worktreeDir)
	snapshotDir := previewreceipt.TurnSnapshotDir(projectGitRoot, taskID, receipt.TurnID)

	// Invariant: Turn preview must mount dedicated turn snapshot, NEVER the main worktreeDir
	if snapshotDir == "" || snapshotDir == worktreeDir {
		s.jsonError(w, http.StatusInternalServerError, "invalid turn snapshot directory")
		return
	}

	if fi, err := os.Stat(snapshotDir); err != nil || !fi.IsDir() {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "turn preview snapshot directory not found")
		return
	}

	runtime, err := s.resolveTaskPreviewRuntime(project, taskID)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to resolve preview runtime: "+err.Error())
		return
	}

	inst, err := s.previewEngine.StartTurnPreviewWithRuntime(r.Context(), taskID, turnID, project, snapshotDir, runtime)
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
