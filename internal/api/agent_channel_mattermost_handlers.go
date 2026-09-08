package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// handleMattermostSlashBind processes the /bind Slash Command from Mattermost.
// Security Invariants & Design:
// - B3 + B4: Routing key is the bind code (code.ChannelBindingID directly identifies the binding).
//   Command Token verifies origin authenticity, but does NOT route.
// - C1 + Q2: Responses are explicitly ephemeral (response_type: "ephemeral").
// - §8 Rule 1: Command Token must match the configured commandToken (or verificationToken). Failure => 401.
// - §8 Rule 9: Plaintext MG- code never stored in audit logs or metadata.
func (s *Server) handleMattermostSlashBind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	command := strings.TrimSpace(r.FormValue("command"))
	switch command {
	case "/bind", "/bind-chat", "/multigent", "/mg", "/mg-bind":
		// valid
	default:
		writeMattermostEphemeral(w, "未知命令。请使用 /bind <绑定码> 或 /multigent bind <绑定码> 进行账号绑定。")
		return
	}

	rawText := strings.TrimSpace(r.FormValue("text"))
	if rawText == "status" {
		s.handleMattermostStatus(w, r)
		return
	}
	if rawText == "help" || (rawText == "" && (command == "/multigent" || command == "/mg")) {
		s.handleMattermostHelp(w, r)
		return
	}

	code := rawText
	if strings.HasPrefix(code, "bind ") {
		code = strings.TrimSpace(strings.TrimPrefix(code, "bind "))
	}
	if code == "" {
		writeMattermostEphemeral(w, "请提供绑定码。用法：/bind <绑定码> 或 /multigent bind <绑定码>（请在 Multigent 控制台对应 Agent 协作渠道中生成）。输入 /multigent help 查看帮助。")
		return
	}

	codeRow, found, err := s.controlDB.AgentChannelBindCodeByCode(code)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		writeMattermostEphemeral(w, "绑定码无效。请在 Multigent 中重新生成绑定码后再试。")
		return
	}

	now := time.Now().UTC()
	if strings.TrimSpace(codeRow.UsedAt) != "" {
		writeMattermostEphemeral(w, "绑定码已使用。请在 Multigent 中重新生成绑定码。")
		return
	}

	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(codeRow.ExpiresAt))
	if err != nil || now.After(expiresAt) {
		writeMattermostEphemeral(w, "绑定码已过期。请在 Multigent 中重新生成绑定码。")
		return
	}

	targetType := strings.TrimSpace(codeRow.TargetType)
	if targetType == "" {
		targetType = "user"
	}

	if command == "/bind" && targetType == "chat" {
		writeMattermostEphemeral(w, "该绑定码为群聊绑定码。请在群聊中使用 /bind-chat 命令。")
		return
	}
	if command == "/bind-chat" && targetType == "user" {
		writeMattermostEphemeral(w, "该绑定码为个人用户绑定码。请在私聊中使用 /bind 命令。")
		return
	}

	// B3 & B4: Lookup binding directly from bind code
	binding, found, err := s.controlDB.AgentChannelBindingByID(codeRow.ChannelBindingID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		writeMattermostEphemeral(w, "绑定失败：未找到对应的协作渠道。")
		return
	}
	if binding.Provider != "mattermost" {
		writeMattermostEphemeral(w, "绑定失败：该绑定码不属于 Mattermost 协作渠道。")
		return
	}

	// Verify Command Token from connection secret
	secret, ok, err := s.controlDB.ConnectionSecret(binding.ConnectionID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !ok {
		writeMattermostEphemeral(w, "绑定失败：协作渠道凭证不存在，请联系管理员重新配置。")
		return
	}
	values, err := openConnectionSecret(secret)
	if err != nil {
		s.serverError(w, err)
		return
	}

	expectedToken := strings.TrimSpace(values["commandToken"])
	if expectedToken == "" {
		expectedToken = strings.TrimSpace(values["verificationToken"])
	}
	if expectedToken == "" {
		writeMattermostEphemeral(w, "绑定失败：协作渠道尚未配置 Slash Command Token，请联系管理员在 Multigent 渠道设置中填入 commandToken 后再试。")
		return
	}

	receivedToken := strings.TrimSpace(r.FormValue("token"))
	if !isMattermostCommandTokenValid(receivedToken, expectedToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	senderUserID := strings.TrimSpace(r.FormValue("user_id"))
	channelID := strings.TrimSpace(r.FormValue("channel_id"))
	userName := strings.TrimSpace(r.FormValue("user_name"))
	if senderUserID == "" {
		writeMattermostEphemeral(w, "绑定失败：无法识别发送者 Mattermost user_id。")
		return
	}

	nowStr := now.Format(time.RFC3339)
	codeHash := shortSensitiveHash(codeRow.Code)

	if targetType == "chat" {
		chatName := strings.TrimSpace(codeRow.TargetName)
		if chatName == "" {
			chatName = strings.TrimSpace(r.FormValue("channel_name"))
		}
		if chatName == "" {
			chatName = channelID
		}
		metaRaw, _ := json.Marshal(map[string]any{
			"source":          "agent_channel_bind_code",
			"project":         binding.ProjectID,
			"agent":           binding.AgentID,
			"provider":        "mattermost",
			"targetType":      "chat",
			"boundAt":         nowStr,
			"chatId":          channelID,
			"externalUserId":  senderUserID,
			"bindCodeHash":    codeHash,
			"channelId":       binding.ID,
			"workspaceId":     binding.WorkspaceID,
		})
		if err := s.controlDB.UpsertAgentChannelTarget(controldb.AgentChannelTarget{
			ID:               newChannelID("cht"),
			WorkspaceID:      binding.WorkspaceID,
			ChannelBindingID: binding.ID,
			Provider:         "mattermost",
			TargetType:       "chat",
			DisplayName:      chatName,
			ExternalUserID:   senderUserID,
			ExternalChatID:   channelID,
			MetadataJSON:     string(metaRaw),
			CreatedBy:        codeRow.UserID,
			CreatedAt:        nowStr,
			UpdatedAt:        nowStr,
			LastActivityAt:   nowStr,
		}); err != nil {
			s.serverError(w, err)
			return
		}

		binding.ExternalChatID = channelID
		binding.LastActivityAt = nowStr
		binding.UpdatedAt = nowStr
		_ = s.controlDB.UpsertAgentChannelBinding(binding)
		_ = s.controlDB.MarkAgentChannelBindCodeUsed(codeRow.Code, nowStr)

		s.auditLog(auditLogInput{
			WorkspaceID:  binding.WorkspaceID,
			ActorType:    "user",
			ActorID:      codeRow.UserID,
			Action:       "agent_channel.chat_bound",
			ResourceType: "agent_channel",
			ResourceID:   binding.ID,
			Summary:      fmt.Sprintf("Bound mattermost chat target %q to %s/%s channel", chatName, binding.ProjectID, binding.AgentID),
			After: map[string]any{
				"provider":       "mattermost",
				"project":        binding.ProjectID,
				"agent":          binding.AgentID,
				"externalUserId": senderUserID,
				"chatId":         channelID,
			},
			Request: r,
		})

		writeMattermostEphemeral(w, fmt.Sprintf("群聊绑定成功！当前群聊已与 %s/%s 绑定。", binding.ProjectID, binding.AgentID))
		return
	}

	// User target binding
	metaRaw, _ := json.Marshal(map[string]any{
		"source":           "agent_channel_bind_code",
		"project":          binding.ProjectID,
		"agent":            binding.AgentID,
		"provider":         "mattermost",
		"boundAt":          nowStr,
		"chatId":           channelID,
		"externalUserId":   senderUserID,
		"externalUsername": userName,
		"bindCodeHash":     codeHash,
		"channelId":        binding.ID,
		"workspaceId":      binding.WorkspaceID,
	})

	if err := s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               newChannelID("uch"),
		WorkspaceID:      binding.WorkspaceID,
		UserID:           codeRow.UserID,
		ChannelBindingID: binding.ID,
		Provider:         "mattermost",
		ExternalUserID:   senderUserID,
		ExternalChatID:   channelID,
		MetadataJSON:     string(metaRaw),
		CreatedBy:        codeRow.UserID,
		CreatedAt:        nowStr,
		UpdatedAt:        nowStr,
	}); err != nil {
		s.serverError(w, err)
		return
	}

	_ = s.controlDB.UpsertExternalIdentity(controldb.ExternalIdentity{
		ID:             newChannelID("ext"),
		WorkspaceID:    binding.WorkspaceID,
		Provider:       "mattermost",
		ExternalUserID: senderUserID,
		UserID:         codeRow.UserID,
		MetadataJSON:   string(metaRaw),
		CreatedBy:      codeRow.UserID,
		CreatedAt:      nowStr,
		UpdatedAt:      nowStr,
	})

	binding.LastActivityAt = nowStr
	binding.UpdatedAt = nowStr
	_ = s.controlDB.UpsertAgentChannelBinding(binding)
	_ = s.controlDB.MarkAgentChannelBindCodeUsed(codeRow.Code, nowStr)

	s.auditLog(auditLogInput{
		WorkspaceID:  binding.WorkspaceID,
		ActorType:    "user",
		ActorID:      codeRow.UserID,
		Action:       "agent_channel.identity_bound",
		ResourceType: "agent_channel",
		ResourceID:   binding.ID,
		Summary:      fmt.Sprintf("Bound mattermost user %s to %s/%s channel", senderUserID, binding.ProjectID, binding.AgentID),
		After: map[string]any{
			"provider":       "mattermost",
			"project":        binding.ProjectID,
			"agent":          binding.AgentID,
			"externalUserId": senderUserID,
			"chatId":         channelID,
		},
		Request: r,
	})

	agentLabel := s.agentChannelDisplayName(binding)
	writeMattermostEphemeral(w, fmt.Sprintf("绑定成功！您已将 Mattermost 账号与 Multigent 关联。之后 %s 可以通过 Mattermost 通知你。", agentLabel))
}

func (s *Server) handleMattermostStatus(w http.ResponseWriter, r *http.Request) {
	mmUserID := strings.TrimSpace(r.FormValue("user_id"))
	if mmUserID == "" {
		writeMattermostEphemeral(w, "无法获取 Mattermost 用户 ID。")
		return
	}

	identities, err := s.controlDB.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
		Provider:       "mattermost",
		ExternalUserID: mmUserID,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}

	if len(identities) == 0 {
		msg := "### ℹ️ Multigent 账号状态 (未绑定)\n" +
			"当前 Mattermost 账号尚未绑定到 Multigent 平台。\n\n" +
			"**快速绑定步骤**：\n" +
			"1. 在浏览器登录 Multigent 平台；\n" +
			"2. 进入 Agent 渠道设置并点击「生成绑定码」（获得 `MG-xxxx`）；\n" +
			"3. 在 Mattermost 中输入 `/multigent bind <绑定码>` 完成持有认证。"
		writeMattermostEphemeral(w, msg)
		return
	}

	identity := identities[0]
	user, userFound, _ := s.controlDB.UserByUsername(identity.UserID)
	roleName := "普通成员 (Member)"
	if userFound && user.Role != "" {
		roleName = user.Role
	}

	msg := fmt.Sprintf("### ℹ️ Multigent 账号协同状态 (已就绪)\n"+
		"> **平台用户名**: `@%s` | **权限角色**: `%s`\n"+
		"> **协同渠道**: `Mattermost` | **出站路由**: `全工作区通行 (D6 Ready)`\n\n"+
		"**已启用特性**: ✅ Dual CAS 防漂移保护 · ✅ Task Thread 实时看板 · ✅ 1-Click / Dialog 审批\n\n"+
		"*💡 如需绑定其他账号，可输入 `/multigent bind <新绑定码>`*", identity.UserID, roleName)
	writeMattermostEphemeral(w, msg)
}

func (s *Server) handleMattermostHelp(w http.ResponseWriter, r *http.Request) {
	msg := "### 🤖 Multigent ChatOps 协同指令指南 (Ambient Workspace)\n\n" +
		"- `/multigent bind <绑定码>`: 绑定您的 Mattermost 账号与 Multigent 开发者身份 (PoP 持据防伪)。\n" +
		"- `/multigent status`: 查看当前账号的绑定状态、权限角色与可用能力。\n" +
		"- `/multigent help`: 查看此帮助手册。\n\n" +
		"*支持快捷别名: `/mg bind <码>`、`/mg status`、`/bind <码>`*"
	writeMattermostEphemeral(w, msg)
}

func writeMattermostEphemeral(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"response_type": "ephemeral",
		"text":          text,
	})
}

func isMattermostCommandTokenValid(receivedToken, expectedTokens string) bool {
	if receivedToken == "" || expectedTokens == "" {
		return false
	}
	parts := strings.FieldsFunc(expectedTokens, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	matched := 0
	for _, part := range parts {
		clean := strings.TrimSpace(part)
		if clean != "" && subtle.ConstantTimeCompare([]byte(receivedToken), []byte(clean)) == 1 {
			matched = 1
		}
	}
	return matched == 1
}

