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
	"github.com/multigent/multigent/internal/entity"
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

// writeMattermostActionError uses the post-action response schema, rather than
// the slash-command schema. Mattermost otherwise accepts the HTTP response but
// silently drops the feedback below the card, which makes an early fail-closed
// rejection look like an inert button.
func writeMattermostActionError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message},
		// Keep text during the transition from slash-command responses so older
		// clients and existing integrations can still surface the same message.
		"text": message,
	})
}

func writeMattermostActionSuccess(w http.ResponseWriter, ephemeralText, statusText string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"update": map[string]any{
			"message": fmt.Sprintf("#### 🔒 人工审核已完结\n> %s", statusText),
			"props": map[string]any{
				"attachments": []any{map[string]any{
					"color": "#10b981",
					"title": "人工审核已完成 (已归档)",
					"text":  statusText,
				}},
			},
		},
		"ephemeral_text":     ephemeralText,
		"skip_slack_parsing": true,
		"text":               ephemeralText,
	})
}

// logMattermostActionCallback is intentionally shape-only diagnostics. Action
// and dialog tokens are bearer material and request bodies can contain review
// text, so neither may be logged.
func logMattermostActionCallback(stage string, payload mattermostActionPayload) {
	action := strings.TrimSpace(payload.Context.Action)
	switch action {
	case "approve", "edit", "reject", "review_approve":
	default:
		action = "unknown"
	}
	log.Printf("[chatops] mattermost action callback stage=%s action=%s user_id_present=%t channel_id_present=%t post_id_present=%t trigger_id_present=%t action_token_present=%t",
		stage, action, strings.TrimSpace(payload.UserID) != "", strings.TrimSpace(payload.ChannelID) != "", strings.TrimSpace(payload.PostID) != "", strings.TrimSpace(payload.TriggerID) != "", strings.TrimSpace(payload.Context.ActionToken) != "")
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

// resolveMattermostConnectionTarget resolves baseUrl, botToken, and hmacSecret directly from a ConnectionID.
func (s *Server) resolveMattermostConnectionTarget(connectionID string) (baseURL, botToken, hmacSecret string, err error) {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return "", "", "", errors.New("missing connection ID")
	}
	conn, found, err := s.controlDB.ConnectionByID(connectionID)
	if err != nil || !found || conn.Status != "active" {
		return "", "", "", fmt.Errorf("connection %s not found or inactive", connectionID)
	}
	secret, found, err := s.controlDB.ConnectionSecret(connectionID)
	if err != nil || !found {
		return "", "", "", fmt.Errorf("connection secret not found for %s: %v", connectionID, err)
	}
	values, err := controldb.OpenConnectionSecret(secret)
	if err != nil {
		return "", "", "", fmt.Errorf("open connection secret for %s: %w", connectionID, err)
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

// resolvePlatformUserForAction resolves a Mattermost sender user ID to a Multigent platform user ID
// strictly within the trusted scope of targetConnectionID (the exact connection, or an active
// connection in the same administrator-attested IM instance).
// If multiple distinct platform users claim the same external ID in this scope, it fails closed
// and logs a security audit event.
func (s *Server) resolvePlatformUserForAction(r *http.Request, workspaceID, targetConnectionID, mmUserID string) (string, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	targetConnectionID = strings.TrimSpace(targetConnectionID)
	mmUserID = strings.TrimSpace(mmUserID)
	if workspaceID == "" || targetConnectionID == "" || mmUserID == "" {
		return "", errors.New("missing required resolution parameters")
	}

	targetConn, found, err := s.controlDB.ConnectionByID(targetConnectionID)
	if err != nil || !found || targetConn.Status != "active" || targetConn.WorkspaceID != workspaceID {
		return "", errors.New("target connection not found or inactive")
	}

	identities, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ExternalUserID: mmUserID,
	})
	if err != nil || len(identities) == 0 {
		return "", nil
	}

	matchedUsers := make(map[string]struct{})
	for _, id := range identities {
		binding, bFound, bErr := s.controlDB.AgentChannelBindingByID(id.ChannelBindingID)
		if bErr != nil || !bFound || binding.Status != "connected" || binding.WorkspaceID != workspaceID {
			continue
		}
		sourceConn, cFound, cErr := s.controlDB.ConnectionByID(binding.ConnectionID)
		if cErr != nil || !cFound || sourceConn.Status != "active" || sourceConn.WorkspaceID != workspaceID {
			continue
		}
		usable, _ := s.connectionsShareDeliveryBoundary(targetConn, sourceConn)
		if usable && strings.TrimSpace(id.UserID) != "" {
			matchedUsers[id.UserID] = struct{}{}
		}
	}

	if len(matchedUsers) > 1 {
		// Ambiguity check: multiple platform users claimed this external identity within the same trusted boundary!
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			Action:       "chatops.identity_resolution_ambiguity_blocked",
			ResourceType: "connection",
			ResourceID:   targetConnectionID,
			Summary:      fmt.Sprintf("Security alert: ambiguous platform users for Mattermost user %s in trusted scope", mmUserID),
			Request:      r,
		})
		return "", errors.New("ambiguous identity in trusted scope")
	}

	for u := range matchedUsers {
		return u, nil
	}
	return "", nil
}

// handleMattermostActionCallback processes button clicks from Mattermost cards.
func (s *Server) handleMattermostActionCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	stage := "decode"
	var payload mattermostActionPayload
	defer func() { logMattermostActionCallback(stage, payload) }()
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		stage = "decode_invalid"
		writeMattermostActionError(w, "请求解析失败：无效的 JSON 格式。")
		return
	}

	actionToken := strings.TrimSpace(payload.Context.ActionToken)
	if actionToken == "" {
		stage = "action_token_missing"
		writeMattermostActionError(w, "安全校验失败：缺少 action_token。")
		return
	}

	// 1. Peek token without secret to get connectionID for credential lookup
	parts := strings.Split(actionToken, ".")
	if len(parts) != 2 {
		stage = "action_token_shape_invalid"
		writeMattermostActionError(w, "审批卡片已过期或格式错误。请刷新任务 Thread 获取最新卡片。")
		return
	}

	var peek imbridge.ActionTokenPayload
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
		_ = json.Unmarshal(raw, &peek)
	}

	connectionID := strings.TrimSpace(peek.ConnectionID)
	if connectionID == "" {
		stage = "action_token_connection_missing"
		writeMattermostActionError(w, "安全拦截：审批令牌缺少连接标识，无法完成安全验证。")
		return
	}

	baseURL, botToken, hmacSecret, err := s.resolveMattermostConnectionTarget(connectionID)
	if err != nil {
		stage = "connection_resolution_failed"
		writeMattermostActionError(w, "无法获取连接验证密钥，请求已被安全拦截。")
		return
	}

	// 2. Verify action_token HMAC signature & expiration
	tokenData, err := imbridge.VerifyActionToken(hmacSecret, actionToken)
	if err != nil {
		stage = "action_token_verification_failed"
		writeMattermostActionError(w, "审批卡片已超过有效时限（2小时）或签名失效。请刷新任务 Thread 获取最新卡片，或前往 Web 控制台完成审批。")
		return
	}
	if tokenData.ConnectionID != connectionID {
		stage = "action_token_connection_mismatch"
		writeMattermostActionError(w, "安全拦截：审批令牌连接标识被篡改。")
		return
	}
	if payloadAction := strings.TrimSpace(payload.Context.Action); payloadAction == "" || payloadAction != tokenData.Action {
		stage = "action_context_mismatch"
		writeMattermostActionError(w, "安全校验失败：审批动作与卡片上下文不匹配。请刷新任务 Thread 后重试。")
		return
	}

	// 3. Security Boundary: Channel & Projection validation
	if tokenData.ChannelID != "" && payload.ChannelID != "" && tokenData.ChannelID != payload.ChannelID {
		stage = "token_channel_mismatch"
		writeMattermostActionError(w, "安全拦截：请求来源频道与审批令牌不匹配。")
		return
	}

	active, found, err := s.controlDB.ActiveTaskThreadProjection(tokenData.WorkspaceID, tokenData.TaskID, "mattermost")
	if err != nil || !found || active.RootPostID == "" {
		stage = "projection_missing"
		writeMattermostActionError(w, "未找到活跃的任务 Thread 投影或任务已归档。")
		return
	}
	if payload.ChannelID != "" && active.ChannelID != payload.ChannelID {
		stage = "projection_channel_mismatch"
		writeMattermostActionError(w, "安全拦截：请求频道与任务活跃投影不匹配。")
		return
	}

	// 4. Server Verification: Ensure User exists on target Mattermost instance
	validUser, err := s.verifyMattermostUser(r.Context(), baseURL, botToken, payload.UserID)
	if err != nil || !validUser {
		stage = "mattermost_user_verification_failed"
		writeMattermostActionError(w, "无法在 Mattermost 验证您的用户身份，请求已被安全拦截。")
		return
	}

	// 5. Anti-Replay Check: Ensure action token has not already been used.
	// A dialog session can remain dialog_opened when the user closes an expired
	// Mattermost modal without submitting it. In that narrow case, reissue a
	// fresh card after the normal identity/RBAC/CAS checks below.
	expiredDialogReplay := false
	if strings.TrimSpace(tokenData.Nonce) != "" {
		if existing, found, err := s.controlDB.ChatopsActionSessionByNonce(tokenData.WorkspaceID, strings.TrimSpace(tokenData.Nonce)); err == nil && found && existing != nil && existing.State != "failed" {
			if (tokenData.Action == "edit" || tokenData.Action == "review_approve") &&
				(existing.State == "dialog_opened" || existing.State == "dialog_opening") &&
				!existing.ExpiresAt.IsZero() && !time.Now().UTC().Before(existing.ExpiresAt) {
				expiredDialogReplay = true
				_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, existing.ID, "expired")
			} else {
				stage = "action_replay_blocked"
				writeMattermostActionError(w, "该审批操作已被处理或正在处理中，请勿重复操作。")
				return
			}
		}
	}

	// 6. User Identity Mapping & Dynamic RBAC check
	platformUserID, err := s.resolvePlatformUserForAction(r, tokenData.WorkspaceID, tokenData.ConnectionID, payload.UserID)
	if err != nil {
		stage = "identity_resolution_failed"
		writeMattermostActionError(w, "身份验证异常：您的账号在此可信范围内存在歧义或连接异常，已被安全拦截。")
		return
	}
	if platformUserID == "" {
		stage = "identity_unbound"
		writeMattermostActionError(w, "您的 Mattermost 账号未与 Multigent 平台关联。请先在控制台或私聊中使用 /bind 命令完成绑定。")
		return
	}

	if err := s.validateWorkflowDecisionReviewer(tokenData.WorkspaceID, tokenData.ProjectID, tokenData.TaskID, platformUserID); err != nil {
		stage = "reviewer_authorization_failed"
		if errors.Is(err, errWorkflowDecisionReviewerForbidden) {
			writeMattermostActionError(w, fmt.Sprintf("权限不足：您没有项目 %s 的审批权限。", tokenData.ProjectID))
		} else {
			writeMattermostActionError(w, fmt.Sprintf("审批不可用：%v", err))
		}
		return
	}

	// 6. Fetch Task and Domain Preview
	task, _, err := s.findTaskInProject(tokenData.ProjectID, tokenData.TaskID)
	if err != nil || task == nil {
		stage = "task_missing"
		writeMattermostActionError(w, "未找到关联任务或任务已被归档。")
		return
	}

	wfStore := workflow.NewStore(s.controlDB, tokenData.WorkspaceID)
	preview, err := wfStore.GetReviewResolutionPreview(tokenData.ProjectID, task, tokenData.StepID)
	if err != nil {
		stage = "review_preview_failed"
		writeMattermostActionError(w, "无法获取当前审核步骤状态："+err.Error())
		return
	}

	// 7. Dual CAS check (StateVersion + SnapshotHash)
	if err := workflow.ValidateReviewCAS(preview.ExpectedStateVersion, tokenData.ExpectedStateVersion, preview.ReviewSnapshotHash, tokenData.ReviewSnapshotHash); err != nil {
		log.Printf("[chatops] mattermost action callback CAS values project=%s task=%s step=%s current_version=%d token_version=%d current_hash=%s token_hash=%s", tokenData.ProjectID, tokenData.TaskID, tokenData.StepID, preview.ExpectedStateVersion, tokenData.ExpectedStateVersion, shortSensitiveHash(preview.ReviewSnapshotHash), shortSensitiveHash(tokenData.ReviewSnapshotHash))
		msg := "### ⚠️ 审批标的已发生更新 (409 Conflict: Protected)\n" +
			"> **保护机制触发**: 系统检测到您查看卡片后，上游产物或工作流状态已发生变更（版本/指纹防漂移）。\n" +
			"> **安全说明**: 原卡片已为您保护性失效，防止误批旧代码。\n\n" +
			"💡 **系统将自动补发当前版本的待审卡片，请使用新卡片操作；也可前往 Multigent 控制台完成审批。**"
		stage = "review_cas_stale"
		s.reissueCurrentMattermostReviewCard(r, actionToken, *tokenData, payload.UserID, task, preview, platformUserID)
		writeMattermostActionError(w, msg)
		return
	}
	if expiredDialogReplay {
		if err := s.postCurrentMattermostReviewCard(r, *tokenData, task, preview); err != nil {
			stage = "expired_dialog_refresh_failed"
			writeMattermostActionError(w, "原审批弹窗已过期，补发当前审批卡片失败："+err.Error())
			return
		}
		stage = "expired_dialog_reissued"
		writeMattermostActionSuccess(w, "♻️ 原审批弹窗已过期，系统已补发当前版本审批卡片。", "请使用 Thread 中最新的审批卡片。")
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
			stage = "approve_session_create_failed"
			writeMattermostActionError(w, "该审批操作已被处理或正在处理中，请勿重复操作。")
			return
		}
		claimed, err := s.controlDB.ClaimChatopsActionSessionForProcessing(tokenData.WorkspaceID, session.ID)
		if err != nil || !claimed {
			stage = "approve_session_claim_failed"
			writeMattermostActionError(w, "该审批操作已被处理或正在处理中，请勿重复操作。")
			return
		}

		snap, err := workflow.ResolveApprovalOutputs(preview, "approved", map[string]string{}, platformUserID)
		if err != nil {
			_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, session.ID, "failed")
			stage = "approve_output_resolution_failed"
			writeMattermostActionError(w, "参数决议失败："+err.Error())
			return
		}

		_, status, err := s.submitTaskWorkflowReview(r, tokenData.WorkspaceID, tokenData.ProjectID, tokenData.TaskID, workflowReviewBody{
			Decision: "approved",
			Comments: snap.ResolvedOutputs["comments"],
			Outputs:  snap.ResolvedOutputs,
		})
		if err != nil {
			_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, session.ID, "failed")
			stage = "approve_workflow_submit_failed"
			writeMattermostActionError(w, fmt.Sprintf("推进工作流失败 (%d)：%v", status, err))
			return
		}

		traceJSON, _ := json.Marshal(snap.ResolutionTrace)
		outputsJSON, _ := json.Marshal(snap.ResolvedOutputs)
		_ = s.controlDB.CompleteChatopsActionSession(tokenData.WorkspaceID, session.ID, string(traceJSON), string(outputsJSON))

		// Update card in Mattermost: remove action buttons and mark approved
		statusText := fmt.Sprintf("由已验证审批人 %s 于 %s 批准通过 (v%d)", platformUserID, time.Now().Format("15:04"), preview.ExpectedStateVersion)
		if s.threadProjections != nil {
			_ = s.threadProjections.RemoveCardActionsAndSetStatus(r.Context(), tokenData.WorkspaceID, tokenData.ProjectID, payload.ChannelID, payload.PostID, statusText)
		}

		stage = "approve_completed"
		writeMattermostActionSuccess(w, "✅ 审批已成功提交并推进工作流！", statusText)
		return
	}

	// Dialog Action: Reject, Review & Approve, or Edit
	if payload.TriggerID == "" {
		stage = "dialog_trigger_missing"
		writeMattermostActionError(w, "无法打开交互弹窗：缺少 trigger_id。请在 Mattermost 客户端直接点击按钮。")
		return
	}

	callbackBaseURL := os.Getenv("CHATOPS_CALLBACK_BASE_URL")
	if callbackBaseURL == "" {
		callbackBaseURL = os.Getenv("MULTIGENT_PUBLIC_URL")
	}
	if callbackBaseURL == "" {
		callbackBaseURL = os.Getenv("MULTIGENT_CONSOLE_URL")
	}
	if strings.TrimSpace(callbackBaseURL) == "" {
		stage = "dialog_callback_url_missing"
		writeMattermostActionError(w, "系统未配置 CHATOPS_CALLBACK_BASE_URL，无法打开审批弹窗。")
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
		stage = "dialog_session_create_failed"
		writeMattermostActionError(w, "该审批动作已被点击或正在处理中，请勿重复操作。")
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
		stage = "dialog_token_sign_failed"
		writeMattermostActionError(w, "生成对话框签名失败。")
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
		if tokenData.Action == "edit" {
			title = fmt.Sprintf("查看并微调: %s", preview.StepTitle)
			submitLabel = "确认并批准"
		} else {
			title = fmt.Sprintf("审批确认: %s", preview.StepTitle)
			submitLabel = "确认批准"
		}

		summaryParts := make([]string, 0, len(preview.Parameters)+1)
		if len(preview.Parameters) == 0 {
			summaryParts = append(summaryParts, "当前审核没有待决或可微调参数；确认后会按当前快照批准。")
		} else {
			for _, p := range preview.Parameters {
				value := strings.TrimSpace(p.CandidateValue)
				if value == "" {
					value = "(未提供)"
				}
				summaryParts = append(summaryParts, fmt.Sprintf("%s：%s", p.Label, value))
			}
		}

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
			"display_name": "审核摘要与附言 (可选)",
			"placeholder":  "输入备注或批示...",
			"help_text":    strings.Join(summaryParts, "\n"),
			"optional":     true,
		})
		if imbridge.IsDesignConfirmationPreview(preview) {
			elements = append(elements, map[string]any{
				"type":         "textarea",
				"name":         "design_waiver_reason",
				"display_name": "设计豁免理由（无 OpenDesign 项目 ID 时必填）",
				"placeholder":  "仅在没有可验证的 OpenDesign 设计凭证时填写，并说明为什么可以豁免...",
				"help_text":    "有 approved_design_project_id 时优先冻结设计快照；只有明确填写本理由，才允许走豁免路径。",
				"optional":     true,
			})
		}
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
		stage = "dialog_request_build_failed"
		writeMattermostActionError(w, "构建对话框请求失败: "+err.Error())
		return
	}
	dialogReq.Header.Set("Authorization", "Bearer "+botToken)
	dialogReq.Header.Set("Content-Type", "application/json")

	resp, err := s.getChatopsHTTPClient().Do(dialogReq)
	if err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, sessionID, "failed")
		stage = "dialog_open_request_failed"
		writeMattermostActionError(w, "调用 Mattermost 弹窗接口失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, sessionID, "failed")
		log.Printf("[chatops] open dialog error (%d): %s", resp.StatusCode, string(respBody))
		stage = "dialog_open_rejected"
		writeMattermostActionError(w, fmt.Sprintf("Mattermost 拒绝打开弹窗 (%d)。", resp.StatusCode))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
	stage = "dialog_opened"
}

// reissueCurrentMattermostReviewCard repairs the UX after a stale card click.
// The old action nonce is recorded before posting so repeated clicks on the same
// stale card cannot create an unbounded stream of replacement cards. If posting
// fails, the session is marked failed and a later retry may repair it.
func (s *Server) reissueCurrentMattermostReviewCard(r *http.Request, actionToken string, tokenData imbridge.ActionTokenPayload, mmUserID string, task *entity.Task, preview workflow.ReviewResolutionPreview, platformUserID string) {
	if s == nil || s.controlDB == nil || s.threadProjections == nil || task == nil {
		return
	}
	nonce := strings.TrimSpace(tokenData.Nonce)
	if nonce == "" {
		return
	}
	session := &controldb.ChatopsActionSession{
		ID:                   "cas-refresh-" + newChatopsID(),
		WorkspaceID:          tokenData.WorkspaceID,
		Project:              tokenData.ProjectID,
		TaskID:               tokenData.TaskID,
		StepID:               tokenData.StepID,
		ExpectedStateVersion: preview.ExpectedStateVersion,
		ReviewSnapshotHash:   preview.ReviewSnapshotHash,
		ActionType:           "stale_refresh",
		ActorMMUserID:        strings.TrimSpace(mmUserID),
		ActorPlatformUserID:  platformUserID,
		State:                "stale",
		ActionNonce:          nonce,
		TokenHash:            imbridge.ComputeTokenHash(actionToken),
		ExpiresAt:            time.Now().UTC().Add(10 * time.Minute),
	}
	if err := s.controlDB.CreateChatopsActionSession(session); err != nil {
		log.Printf("[chatops] stale review card refresh already claimed or could not be recorded project=%s task=%s step=%s: %v", tokenData.ProjectID, tokenData.TaskID, tokenData.StepID, err)
		return
	}

	if err := s.postCurrentMattermostReviewCard(r, tokenData, task, preview); err != nil {
		_ = s.controlDB.UpdateChatopsActionSessionState(tokenData.WorkspaceID, session.ID, "failed")
		log.Printf("[chatops] reissue current review card failed for %s/%s: %v", tokenData.ProjectID, tokenData.TaskID, err)
	}
}

func (s *Server) postCurrentMattermostReviewCard(r *http.Request, tokenData imbridge.ActionTokenPayload, task *entity.Task, preview workflow.ReviewResolutionPreview) error {
	if s == nil || s.threadProjections == nil || task == nil {
		return errors.New("review card projection unavailable")
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.threadProjections.PostHumanReviewCard(refreshCtx, imbridge.HumanReviewPostRequest{
		WorkspaceID:     tokenData.WorkspaceID,
		ProjectID:       tokenData.ProjectID,
		TaskID:          tokenData.TaskID,
		StepID:          tokenData.StepID,
		StepTitle:       preview.StepTitle,
		Assignee:        strings.TrimSpace(task.Assignee),
		Preview:         preview,
		CallbackBaseURL: s.consoleBaseURL(),
	})
	return err
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

	var peek imbridge.DialogTokenPayload
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
		_ = json.Unmarshal(raw, &peek)
	}

	connectionID := strings.TrimSpace(peek.ConnectionID)
	if connectionID == "" {
		writeMattermostDialogError(w, "comments", "安全拦截：对话框令牌缺少连接标识。")
		return
	}

	baseURL, botToken, hmacSecret, err := s.resolveMattermostConnectionTarget(connectionID)
	if err != nil {
		writeMattermostDialogError(w, "comments", "无法获取 Mattermost 验证密钥。")
		return
	}

	tokenData, err := imbridge.VerifyDialogToken(hmacSecret, dialogToken)
	if err != nil {
		writeMattermostDialogError(w, "comments", "对话框签名失效或已超时（10分钟），请重新在卡片中发起。")
		return
	}
	if tokenData.ConnectionID != connectionID {
		writeMattermostDialogError(w, "comments", "安全拦截：对话框令牌连接标识被篡改。")
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
	platformUserID, err := s.resolvePlatformUserForAction(r, tokenData.WorkspaceID, tokenData.ConnectionID, payload.UserID)
	if err != nil {
		writeMattermostDialogError(w, "comments", "身份验证异常：账号在此可信范围内存在歧义或连接异常，已被安全拦截。")
		return
	}
	if platformUserID == "" {
		writeMattermostDialogError(w, "comments", "未关联 Multigent 平台账号。请先在控制台或私聊中使用 /bind 命令完成绑定。")
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
	// design_waiver_reason is an audited design-gate control field. Some older
	// persisted workflow definitions do not expose it in their output preview,
	// but the ChatOps dialog still needs to be able to submit the explicit
	// fail-closed waiver. Accept it only for a design confirmation preview.
	if decision == "approved" && imbridge.IsDesignConfirmationPreview(preview) {
		if waiver := strings.TrimSpace(payload.Submission["design_waiver_reason"]); waiver != "" {
			if len(waiver) > 4000 {
				writeMattermostDialogError(w, "design_waiver_reason", "设计豁免理由不能超过 4000 个字符。")
				return
			}
			snap.ResolvedOutputs["design_waiver_reason"] = waiver
			snap.ResolutionTrace["design_waiver_reason"] = workflow.ResolutionTraceItem{
				Mode:   "human_input",
				Source: "dialog.design_waiver_reason",
				Actor:  platformUserID,
			}
		}
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
