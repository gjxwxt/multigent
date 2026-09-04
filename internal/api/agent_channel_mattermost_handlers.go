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
	if command != "/bind" && command != "/bind-chat" {
		writeMattermostEphemeral(w, "未知命令。请使用 /bind <绑定码> 进行账号绑定。")
		return
	}

	code := strings.TrimSpace(r.FormValue("text"))
	if code == "" {
		writeMattermostEphemeral(w, "请提供绑定码。用法：/bind <绑定码>（请在 Multigent 控制台对应 Agent 协作渠道中生成）。")
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
	if subtle.ConstantTimeCompare([]byte(receivedToken), []byte(expectedToken)) != 1 {
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

func writeMattermostEphemeral(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"response_type": "ephemeral",
		"text":          text,
	})
}
