package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
	"github.com/multigent/multigent/internal/workflow"
)

func newChatopsID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type mattermostActionContext struct {
	ActionToken string `json:"action_token"`
	Action      string `json:"action"`
}

type mattermostActionPayload struct {
	UserID    string                  `json:"user_id"`
	UserName  string                  `json:"user_name"`
	ChannelID string                  `json:"channel_id"`
	PostID    string                  `json:"post_id"`
	TriggerID string                  `json:"trigger_id"`
	Context   mattermostActionContext `json:"context"`
}

type mattermostDialogPayload struct {
	Type       string            `json:"type"` // "dialog_submission"
	CallbackID string            `json:"callback_id"`
	State      string            `json:"state"` // dialog_token
	UserID     string            `json:"user_id"`
	ChannelID  string            `json:"channel_id"`
	Submission map[string]string `json:"submission"`
	Cancelled  bool              `json:"cancelled"`
}

// resolveMattermostTarget resolves baseUrl, botToken, and hmacSecret for ChatOps operations.
func (s *Server) resolveMattermostTarget(workspaceID, projectID string) (baseURL, botToken, hmacSecret string, err error) {
	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Provider:    "mattermost",
		Status:      "connected",
	})
	if err != nil {
		return "", "", "", fmt.Errorf("list bindings: %w", err)
	}
	if len(bindings) == 0 {
		bindings, err = s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
			WorkspaceID: workspaceID,
			Provider:    "mattermost",
			Status:      "connected",
		})
		if err != nil {
			return "", "", "", fmt.Errorf("list workspace bindings: %w", err)
		}
	}
	if len(bindings) == 0 {
		return "", "", "", fmt.Errorf("no connected mattermost binding found")
	}

	b := bindings[0]
	secret, found, err := s.controlDB.ConnectionSecret(b.ConnectionID)
	if err != nil || !found {
		return "", "", "", fmt.Errorf("connection secret not found for %s: %v", b.ConnectionID, err)
	}
	values, err := controldb.OpenConnectionSecret(secret)
	if err != nil {
		return "", "", "", fmt.Errorf("open connection secret for %s: %w", b.ConnectionID, err)
	}

	baseURL = values["baseUrl"]
	botToken = values["botToken"]
	hmacSecret = strings.TrimSpace(values["bridgeHmacSecret"])
	if hmacSecret == "" {
		hmacSecret = botToken
	}
	return baseURL, botToken, hmacSecret, nil
}

func (s *Server) getChatopsHTTPClient() *http.Client {
	if s.threadProjections != nil && s.threadProjections.HTTPClient() != nil {
		return s.threadProjections.HTTPClient()
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (s *Server) verifyMattermostUser(ctx context.Context, baseURL, botToken, mmUserID string) (bool, error) {
	if strings.TrimSpace(mmUserID) == "" {
		return false, errors.New("empty mattermost user id")
	}
	reqURL := fmt.Sprintf("%s/api/v4/users/%s", strings.TrimRight(baseURL, "/"), mmUserID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, err := s.getChatopsHTTPClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("verify mattermost user request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	return false, nil
}

// resolvePlatformUser maps a Mattermost sender user ID to a Multigent platform user ID.
func (s *Server) resolvePlatformUser(workspaceID, mmUserID, mmUserName string) string {
	identities, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ExternalUserID: mmUserID,
	})
	if err == nil && len(identities) > 0 {
		return identities[0].UserID
	}
	// Check external_identities as well (per-workspace user identity mapping)
	exts, err := s.controlDB.ListExternalIdentities(controldb.ExternalIdentityFilter{
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ExternalUserID: mmUserID,
	})
	if err == nil && len(exts) > 0 {
		return exts[0].UserID
	}
	// Fallback to match username in workspace users store if available
	if mmUserName != "" && s.users != nil {
		if u := s.users.GetUser(mmUserName); u != nil && u.Username != "" {
			return u.Username
		}
	}
	return ""
}

// handleMattermostActionCallback processes button clicks from Mattermost cards.
func (s *Server) handleMattermostActionCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var payload mattermostActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeMattermostEphemeral(w, "请求解析失败：无效的 JSON 格式。")
		return
	}

	actionToken := strings.TrimSpace(payload.Context.ActionToken)
	if actionToken == "" {
		writeMattermostEphemeral(w, "安全校验失败：缺少 action_token。")
		return
	}

	// 1. Peek token without secret to get workspaceID & projectID for credential lookup
	parts := strings.Split(actionToken, ".")
	if len(parts) != 2 {
		writeMattermostEphemeral(w, "审批卡片已过期或格式错误。请刷新任务 Thread 获取最新卡片。")
		return
	}

	workspaceID, _ := s.currentWorkspaceID()
	if workspaceID == "" {
		workspaceID = "default"
	}
	projectID := ""
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
		var peek imbridge.ActionTokenPayload
		if err := json.Unmarshal(raw, &peek); err == nil {
			if strings.TrimSpace(peek.WorkspaceID) != "" {
				workspaceID = strings.TrimSpace(peek.WorkspaceID)
			}
			projectID = strings.TrimSpace(peek.ProjectID)
		}
	}

	baseURL, botToken, hmacSecret, err := s.resolveMattermostTarget(workspaceID, projectID)
	if err != nil {
		log.Printf("[chatops] resolve mattermost target error: %v", err)
		writeMattermostEphemeral(w, "无法获取 Mattermost 配置，请检查多渠道绑定。")
		return
	}

	// 2. Verify action_token HMAC signature & expiration
	tokenData, err := imbridge.VerifyActionToken(hmacSecret, actionToken)
	if err != nil {
		writeMattermostEphemeral(w, "审批卡片已超过有效时限（2小时）或签名失效。请刷新任务 Thread 获取最新卡片，或前往 Web 控制台完成审批。")
		return
	}

	// 3. Security Boundary: Channel & Projection validation
	if tokenData.ChannelID != "" && payload.ChannelID != "" && tokenData.ChannelID != payload.ChannelID {
		writeMattermostEphemeral(w, "安全拦截：请求来源频道与审批令牌不匹配。")
		return
	}

	active, found, err := s.controlDB.ActiveTaskThreadProjection(tokenData.WorkspaceID, tokenData.TaskID, "mattermost")
	if err != nil || !found || active.RootPostID == "" {
		writeMattermostEphemeral(w, "未找到活跃的任务 Thread 投影或任务已归档。")
		return
	}
	if payload.ChannelID != "" && active.ChannelID != payload.ChannelID {
		writeMattermostEphemeral(w, "安全拦截：请求频道与任务活跃投影不匹配。")
		return
	}

	// 4. Server Verification: Ensure User exists on target Mattermost instance
	validUser, err := s.verifyMattermostUser(r.Context(), baseURL, botToken, payload.UserID)
	if err != nil || !validUser {
		writeMattermostEphemeral(w, "无法在 Mattermost 验证您的用户身份，请求已被安全拦截。")
		return
	}

	// 5. Anti-Replay Check: Ensure action token has not already been used
	if strings.TrimSpace(tokenData.Nonce) != "" {
		if existing, found, err := s.controlDB.ChatopsActionSessionByNonce(tokenData.WorkspaceID, strings.TrimSpace(tokenData.Nonce)); err == nil && found && existing != nil {
			writeMattermostEphemeral(w, "该审批操作已被处理或正在处理中，请勿重复操作。")
			return
		}
	}

	// 6. User Identity Mapping & Dynamic RBAC check
	platformUserID := s.resolvePlatformUser(tokenData.WorkspaceID, payload.UserID, payload.UserName)
	if platformUserID == "" {
		writeMattermostEphemeral(w, "您的 Mattermost 账号未与 Multigent 平台关联。请先在控制台或私聊中使用 /bind 命令完成绑定。")
		return
	}

	if err := s.validateWorkflowDecisionReviewer(tokenData.WorkspaceID, tokenData.ProjectID, tokenData.TaskID, platformUserID); err != nil {
		if errors.Is(err, errWorkflowDecisionReviewerForbidden) {
			writeMattermostEphemeral(w, fmt.Sprintf("权限不足：您没有项目 %s 的审批权限。", tokenData.ProjectID))
		} else {
			writeMattermostEphemeral(w, fmt.Sprintf("审批不可用：%v", err))
		}
		return
	}

	// 6. Fetch Task and Domain Preview
	task, _, err := s.findTaskInProject(tokenData.ProjectID, tokenData.TaskID)
	if err != nil || task == nil {
		writeMattermostEphemeral(w, "未找到关联任务或任务已被归档。")
		return
	}

	wfStore := workflow.NewStore(s.controlDB, tokenData.WorkspaceID)
	preview, err := wfStore.GetReviewResolutionPreview(tokenData.ProjectID, task, tokenData.StepID)
	if err != nil {
		writeMattermostEphemeral(w, "无法获取当前审核步骤状态："+err.Error())
		return
	}

	// 7. Dual CAS check (StateVersion + SnapshotHash)
	if err := workflow.ValidateReviewCAS(preview.ExpectedStateVersion, tokenData.ExpectedStateVersion, preview.ReviewSnapshotHash, tokenData.ReviewSnapshotHash); err != nil {
		msg := "### ⚠️ 审批标的已发生更新 (409 Conflict: Protected)\n" +
			"> **保护机制触发**: 系统检测到您查看卡片后，上游产物或工作流状态已发生变更（版本/指纹防漂移）。\n" +
			"> **安全说明**: 原卡片已为您保护性失效，防止误批旧代码。\n\n" +
			"💡 **请查看 Thread 中最新推送的待审卡片，或前往 Multigent 控制台完成审批。**"
		writeMattermostEphemeral(w, msg)
		return
	}

	// 8. Branch: Direct Approve (Zero-input) vs Dialog Opening (Decision-input / Edit / Reject)
	if tokenData.Action == "approve" {
		tokHash := imbridge.ComputeTokenHash(actionToken)
		sessionID := "cas-" + imbridge.GenerateNonce()
		session := &controldb.ChatopsActionSession{
			ID:                   sessionID,
			WorkspaceID:          tokenData.WorkspaceID,
			Project:              tokenData.ProjectID,
			TaskID:               tokenData.TaskID,
			StepID:               tokenData.StepID,
			ExpectedStateVersion: preview.ExpectedStateVersion,
			ReviewSnapshotHash:   preview.ReviewSnapshotHash,
			ActionType:           "approve",
			ActorMMUserID:        payload.UserID,
			ActorPlatformUserID:  platformUserID,
			State:                "issued",
			ActionNonce:          tokenData.Nonce,
			TokenHash:            tokHash,
			ExpiresAt:            time.Now().UTC().Add(10 * time.Minute),
		}
		if err := s.controlDB.CreateChatopsActionSession(session); err != nil {
			log.Printf("[chatops] create 1-click approve session error: %v", err)
			writeMattermostEphemeral(w, "该审批操作已被处理或正在处理中，请勿重复操作。")
			return
		}
		claimed, err := s.controlDB.ClaimChatopsActionSessionForProcessing(tokenData.WorkspaceID, session.ID)
		if err != nil || !claimed {
			writeMattermostEphemeral(w, "该审批操作已被处理或正在处理中，请勿重复操作。")
			return
		}

		snap, err := workflow.ResolveApprovalOutputs(preview, "approved", map[string]string{}, platformUserID)
		if err != nil {
			_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, session.ID, "failed")
			writeMattermostEphemeral(w, "参数决议失败："+err.Error())
			return
		}

		_, status, err := s.submitTaskWorkflowReview(r, tokenData.WorkspaceID, tokenData.ProjectID, tokenData.TaskID, workflowReviewBody{
			Decision: "approved",
			Comments: snap.ResolvedOutputs["comments"],
			Outputs:  snap.ResolvedOutputs,
		})
		if err != nil {
			_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, session.ID, "failed")
			writeMattermostEphemeral(w, fmt.Sprintf("推进工作流失败 (%d)：%v", status, err))
			return
		}

		traceJSON, _ := json.Marshal(snap.ResolutionTrace)
		outputsJSON, _ := json.Marshal(snap.ResolvedOutputs)
		_ = s.controlDB.CompleteChatopsActionSession(tokenData.WorkspaceID, session.ID, string(traceJSON), string(outputsJSON))

		// Update card in Mattermost: remove action buttons and mark approved
		statusText := fmt.Sprintf("由 @%s 于 %s 批准通过 (v%d)", payload.UserName, time.Now().Format("15:04"), preview.ExpectedStateVersion)
		_ = s.threadProjections.RemoveCardActionsAndSetStatus(r.Context(), tokenData.WorkspaceID, tokenData.ProjectID, payload.ChannelID, payload.PostID, statusText)

		writeMattermostEphemeral(w, "✅ 审批已成功提交并推进工作流！")
		return
	}

	// Dialog Action: Reject, Review & Approve, or Edit
	if payload.TriggerID == "" {
		writeMattermostEphemeral(w, "无法打开交互弹窗：缺少 trigger_id。请在 Mattermost 客户端直接点击按钮。")
		return
	}

	callbackBaseURL := os.Getenv("CHATOPS_CALLBACK_BASE_URL")
	if strings.TrimSpace(callbackBaseURL) == "" {
		writeMattermostEphemeral(w, "系统未配置 CHATOPS_CALLBACK_BASE_URL，无法打开审批弹窗。")
		return
	}

	// Create Action Session
	sessionID := "cas-" + newChatopsID()
	session := &controldb.ChatopsActionSession{
		ID:                   sessionID,
		WorkspaceID:          tokenData.WorkspaceID,
		Project:              tokenData.ProjectID,
		TaskID:               tokenData.TaskID,
		StepID:               tokenData.StepID,
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActionType:           tokenData.Action,
		ActorMMUserID:        payload.UserID,
		ActorPlatformUserID:  platformUserID,
		State:                "dialog_opened",
		ActionNonce:          tokenData.Nonce,
		TokenHash:            imbridge.ComputeTokenHash(actionToken),
		ExpiresAt:            time.Now().UTC().Add(10 * time.Minute),
	}
	if err := s.controlDB.CreateChatopsActionSession(session); err != nil {
		log.Printf("[chatops] create action session error: %v", err)
		writeMattermostEphemeral(w, "该审批动作已被点击或正在处理中，请勿重复操作。")
		return
	}

	dialogToken, err := imbridge.SignDialogToken(hmacSecret, imbridge.DialogTokenPayload{
		WorkspaceID:          tokenData.WorkspaceID,
		ProjectID:            tokenData.ProjectID,
		TaskID:               tokenData.TaskID,
		StepID:               tokenData.StepID,
		Action:               tokenData.Action,
		ChannelID:            tokenData.ChannelID,
		ConnectionID:         tokenData.ConnectionID,
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActorMMUserID:        payload.UserID,
		SessionID:            sessionID,
		PostID:               payload.PostID,
		Nonce:                newChatopsID(),
		ExpiresAt:            time.Now().UTC().Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		writeMattermostEphemeral(w, "生成对话框签名失败。")
		return
	}

	// Build Dialog Schema
	var title, submitLabel string
	elements := make([]map[string]any, 0)

	if tokenData.Action == "reject" {
		title = fmt.Sprintf("打回修改: %s", preview.StepTitle)
		submitLabel = "确认打回"
		elements = append(elements, map[string]any{
			"type":         "textarea",
			"name":         "comments",
			"display_name": "打回修改意见 (必填)",
			"placeholder":  "请清晰列出具体的修改要求或原因...",
			"optional":     false,
		})
	} else {
		title = fmt.Sprintf("审批确认: %s", preview.StepTitle)
		submitLabel = "确认批准"

		for _, p := range preview.Parameters {
			switch p.Kind {
			case workflow.KindHumanDecision:
				elements = append(elements, map[string]any{
					"type":         "text",
					"name":         p.Key,
					"display_name": p.Label,
					"default":      p.CandidateValue,
					"placeholder":  "请输入决策参数值...",
					"optional":     !p.Required,
				})
			case workflow.KindInheritedContract:
				if tokenData.Action == "edit" {
					elements = append(elements, map[string]any{
						"type":         "textarea",
						"name":         p.Key,
						"display_name": p.Label + " (继承契约微调)",
						"default":      p.CandidateValue,
						"optional":     true,
					})
				}
			}
		}

		elements = append(elements, map[string]any{
			"type":         "text",
			"name":         "comments",
			"display_name": "审批附言 (可选)",
			"placeholder":  "输入备注或批示...",
			"optional":     true,
		})
	}

	// Call Mattermost /api/v4/actions/dialogs/open
	dialogPayload := map[string]any{
		"trigger_id": payload.TriggerID,
		"url":        callbackBaseURL + "/api/v1/im/mattermost/dialog-submit",
		"dialog": map[string]any{
			"callback_id":  "chatops-dialog-" + sessionID,
			"title":        title,
			"elements":     elements,
			"submit_label": submitLabel,
			"state":        dialogToken,
		},
	}

	dialogBodyBytes, _ := json.Marshal(dialogPayload)
	dialogReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v4/actions/dialogs/open", bytes.NewReader(dialogBodyBytes))
	if err != nil {
		writeMattermostEphemeral(w, "构建对话框请求失败: "+err.Error())
		return
	}
	dialogReq.Header.Set("Authorization", "Bearer "+botToken)
	dialogReq.Header.Set("Content-Type", "application/json")

	resp, err := s.getChatopsHTTPClient().Do(dialogReq)
	if err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, sessionID, "failed")
		writeMattermostEphemeral(w, "调用 Mattermost 弹窗接口失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, sessionID, "failed")
		log.Printf("[chatops] open dialog error (%d): %s", resp.StatusCode, string(respBody))
		writeMattermostEphemeral(w, fmt.Sprintf("Mattermost 拒绝打开弹窗 (%d)。", resp.StatusCode))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

// handleMattermostDialogSubmit processes modal submissions from Mattermost dialogs.
func (s *Server) handleMattermostDialogSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var payload mattermostDialogPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeMattermostDialogError(w, "comments", "提交数据解析失败。")
		return
	}

	if payload.Cancelled {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}

	dialogToken := strings.TrimSpace(payload.State)
	if dialogToken == "" {
		writeMattermostDialogError(w, "comments", "缺少 dialog_token。")
		return
	}

	parts := strings.Split(dialogToken, ".")
	if len(parts) != 2 {
		writeMattermostDialogError(w, "comments", "对话框签名格式错误。")
		return
	}

	workspaceID, _ := s.currentWorkspaceID()
	if workspaceID == "" {
		workspaceID = "default"
	}
	projectID := ""
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
		var peek imbridge.DialogTokenPayload
		if err := json.Unmarshal(raw, &peek); err == nil {
			if strings.TrimSpace(peek.WorkspaceID) != "" {
				workspaceID = strings.TrimSpace(peek.WorkspaceID)
			}
			projectID = strings.TrimSpace(peek.ProjectID)
		}
	}

	baseURL, botToken, hmacSecret, err := s.resolveMattermostTarget(workspaceID, projectID)
	if err != nil {
		writeMattermostDialogError(w, "comments", "无法获取 Mattermost 验证密钥。")
		return
	}

	tokenData, err := imbridge.VerifyDialogToken(hmacSecret, dialogToken)
	if err != nil {
		writeMattermostDialogError(w, "comments", "对话框签名失效或已超时（10分钟），请重新在卡片中发起。")
		return
	}

	// 1. Verify actor binding
	if payload.UserID != tokenData.ActorMMUserID {
		writeMattermostDialogError(w, "comments", "安全防线拦截：提交者与打开弹窗的用户身份不一致。")
		return
	}

	// 2. Channel & Projection validation
	if tokenData.ChannelID != "" && payload.ChannelID != "" && tokenData.ChannelID != payload.ChannelID {
		writeMattermostDialogError(w, "comments", "安全拦截：请求来源频道与弹窗令牌不匹配。")
		return
	}

	active, found, err := s.controlDB.ActiveTaskThreadProjection(tokenData.WorkspaceID, tokenData.TaskID, "mattermost")
	if err != nil || !found || active.RootPostID == "" {
		writeMattermostDialogError(w, "comments", "未找到活跃的任务 Thread 投影或任务已归档。")
		return
	}
	if payload.ChannelID != "" && active.ChannelID != payload.ChannelID {
		writeMattermostDialogError(w, "comments", "安全拦截：请求频道与任务活跃投影不匹配。")
		return
	}

	// 3. Server Verification: Ensure User exists on target Mattermost instance
	validUser, err := s.verifyMattermostUser(r.Context(), baseURL, botToken, payload.UserID)
	if err != nil || !validUser {
		writeMattermostDialogError(w, "comments", "无法在 Mattermost 验证您的用户身份，请求已被安全拦截。")
		return
	}

	// 4. User Identity Mapping & Dynamic RBAC check
	platformUserID := s.resolvePlatformUser(tokenData.WorkspaceID, payload.UserID, "")
	if platformUserID == "" {
		writeMattermostDialogError(w, "comments", "未关联 Multigent 平台账号。")
		return
	}
	if err := s.validateWorkflowDecisionReviewer(tokenData.WorkspaceID, tokenData.ProjectID, tokenData.TaskID, platformUserID); err != nil {
		writeMattermostDialogError(w, "comments", fmt.Sprintf("权限不足：您没有项目 %s 的审批权限。", tokenData.ProjectID))
		return
	}

	// 5. Atomic claim: dialog_opened -> processing
	claimed, err := s.controlDB.ClaimChatopsActionSessionForProcessing(tokenData.WorkspaceID, tokenData.SessionID)
	if err != nil || !claimed {
		writeMattermostDialogError(w, "comments", "该审批会话已被提交或正在处理中，请勿重复操作。")
		return
	}

	task, _, err := s.findTaskInProject(tokenData.ProjectID, tokenData.TaskID)
	if err != nil || task == nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, tokenData.SessionID, "failed")
		writeMattermostDialogError(w, "comments", "未找到关联任务。")
		return
	}

	wfStore := workflow.NewStore(s.controlDB, tokenData.WorkspaceID)
	preview, err := wfStore.GetReviewResolutionPreview(tokenData.ProjectID, task, tokenData.StepID)
	if err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, tokenData.SessionID, "failed")
		writeMattermostDialogError(w, "comments", "获取工作流步骤失败: "+err.Error())
		return
	}

	// Dual CAS check
	if err := workflow.ValidateReviewCAS(preview.ExpectedStateVersion, tokenData.ExpectedStateVersion, preview.ReviewSnapshotHash, tokenData.ReviewSnapshotHash); err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, tokenData.SessionID, "stale")
		writeMattermostDialogError(w, "comments", "审批内容或上游状态已发生变更 (409 Conflict)，请关闭并在卡片中刷新查看。")
		return
	}

	var decision string
	if tokenData.Action == "reject" {
		decision = "rejected"
	} else {
		decision = "approved"
	}

	snap, err := workflow.ResolveApprovalOutputs(preview, decision, payload.Submission, platformUserID)
	if err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, tokenData.SessionID, "failed")
		writeMattermostDialogError(w, "comments", err.Error())
		return
	}

	_, status, err := s.submitTaskWorkflowReview(r, tokenData.WorkspaceID, tokenData.ProjectID, tokenData.TaskID, workflowReviewBody{
		Decision: decision,
		Comments: snap.ResolvedOutputs["comments"],
		Outputs:  snap.ResolvedOutputs,
	})
	if err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, tokenData.SessionID, "failed")
		writeMattermostDialogError(w, "comments", fmt.Sprintf("工作流推进失败 (%d): %v", status, err))
		return
	}

	// Final success: mark completed with resolution trace and outputs
	traceJSON, _ := json.Marshal(snap.ResolutionTrace)
	outputsJSON, _ := json.Marshal(snap.ResolvedOutputs)
	_ = s.controlDB.CompleteChatopsActionSession(tokenData.WorkspaceID, tokenData.SessionID, string(traceJSON), string(outputsJSON))

	// Update card in Mattermost: remove action buttons and mark approved/rejected
	if s.threadProjections != nil && tokenData.PostID != "" && payload.ChannelID != "" {
		var statusText string
		if decision == "approved" {
			statusText = fmt.Sprintf("由 @%s 于 %s 审核通过 (v%d)", payload.UserID, time.Now().Format("15:04"), preview.ExpectedStateVersion)
		} else {
			statusText = fmt.Sprintf("由 @%s 于 %s 打回修改 (v%d)", payload.UserID, time.Now().Format("15:04"), preview.ExpectedStateVersion)
		}
		_ = s.threadProjections.RemoveCardActionsAndSetStatus(r.Context(), tokenData.WorkspaceID, tokenData.ProjectID, payload.ChannelID, tokenData.PostID, statusText)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

func writeMattermostDialogError(w http.ResponseWriter, fieldName, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": map[string]string{
			fieldName: msg,
		},
	})
}
