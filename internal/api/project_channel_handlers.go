package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
)

type projectChannelProvisionRequest struct {
	Provider        string   `json:"provider"`        // Default: "mattermost"
	InstanceID      string   `json:"instanceId"`      // Optional IMInstance ID
	ConnectionID    string   `json:"connectionId"`    // Optional Connection ID
	Mode            string   `json:"mode"`            // "create" (default) or "link"
	TeamID          string   `json:"teamId"`          // Optional explicit Mattermost Team ID
	ChannelName     string   `json:"channelName"`     // e.g. "proj-6test"
	DisplayName     string   `json:"displayName"`     // e.g. "项目频道 - 6test"
	Visibility      string   `json:"visibility"`      // "private" (default) or "public"
	WorkerIDs       []string `json:"workerIds"`       // Agent Worker IDs to bind
	MemberUsernames []string `json:"memberUsernames"` // Workspace usernames to invite
}

type projectChannelProvisionResponse struct {
	OK             bool     `json:"ok"`
	Status         string   `json:"status"` // "success" | "partial"
	Provider       string   `json:"provider"`
	InstanceID     string   `json:"instanceId"`
	ChannelID      string   `json:"channelId"`
	ChannelName    string   `json:"channelName"`
	DisplayName    string   `json:"displayName"`
	Visibility     string   `json:"visibility"`
	TeamID         string   `json:"teamId"`
	BoundAgents    []string `json:"boundAgents"`
	FailedAgents   []string `json:"failedAgents"`
	FailedBots     []string `json:"failedBots"`
	InvitedMembers []string `json:"invitedMembers"`
	FailedMembers  []string `json:"failedMembers"`
	UnboundMembers []string `json:"unboundMembers"`
	Warning        string   `json:"warning,omitempty"`
}

func (s *Server) handleProvisionProjectChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	projectName := strings.TrimSpace(r.PathValue("name"))
	if projectName == "" {
		s.jsonError(w, http.StatusBadRequest, "project name is required")
		return
	}

	// Security Invariant #1: Channel creation and bot/member provisioning require Project Manager permissions.
	if !s.checkProjectManager(w, r, projectName) {
		return
	}

	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}

	var req projectChannelProvisionRequest
	if err := s.readJSON(w, r, &req); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}

	resp, pErr := s.provisionProjectChannelCore(r.Context(), workspaceID, projectName, req, requestUsername(r))
	if pErr != nil {
		if pErr.Code != "" {
			s.jsonErrorCode(w, pErr.StatusCode, pErr.Code, pErr.Message)
		} else {
			s.jsonError(w, pErr.StatusCode, pErr.Message)
		}
		return
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "agent_channel.provisioned",
		ResourceType: "project",
		ResourceID:   projectName,
		Summary:      fmt.Sprintf("Provisioned Mattermost channel %s (%s) for project %s (mode=%s, status=%s)", resp.DisplayName, resp.ChannelID, projectName, req.Mode, resp.Status),
		After: map[string]any{
			"channelId":      resp.ChannelID,
			"channelName":    resp.ChannelName,
			"displayName":    resp.DisplayName,
			"type":           resp.Visibility,
			"mode":           req.Mode,
			"status":         resp.Status,
			"teamId":         resp.TeamID,
			"instanceId":     resp.InstanceID,
			"boundAgents":    resp.BoundAgents,
			"failedAgents":   resp.FailedAgents,
			"failedBots":     resp.FailedBots,
			"invitedMembers": resp.InvitedMembers,
			"failedMembers":  resp.FailedMembers,
			"unboundMembers": resp.UnboundMembers,
		},
		Request: r,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

type channelProvisionError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *channelProvisionError) Error() string {
	return e.Message
}

func (s *Server) provisionProjectChannelCore(ctx context.Context, workspaceID, projectName string, req projectChannelProvisionRequest, actorUsername string) (*projectChannelProvisionResponse, *channelProvisionError) {
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = "mattermost"
	}
	if provider != "mattermost" {
		return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: fmt.Sprintf("unsupported channel provisioning provider %q", provider)}
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "create"
	}
	if mode != "create" && mode != "link" {
		return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: fmt.Sprintf("invalid channel mode %q: must be 'create' or 'link'", req.Mode)}
	}

	// 1. Resolve active Mattermost connection strictly scoped to instance / workspace
	activeConns, err := s.controlDB.ListConnections(controldb.ConnectionFilter{
		WorkspaceID: workspaceID,
		Provider:    provider,
		Status:      "active",
	})
	if err != nil {
		return nil, &channelProvisionError{StatusCode: http.StatusInternalServerError, Message: err.Error()}
	}

	var targetConn controldb.Connection
	var foundConn bool

	if connID := strings.TrimSpace(req.ConnectionID); connID != "" {
		conn, found, err := s.controlDB.ConnectionByID(connID)
		if err != nil {
			return nil, &channelProvisionError{StatusCode: http.StatusInternalServerError, Message: err.Error()}
		}
		if !found {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "specified connection not found"}
		}
		if conn.WorkspaceID != workspaceID || conn.Provider != provider {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "specified connection does not belong to workspace or provider mismatch"}
		}
		if conn.Status != "active" {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "specified connection is not active"}
		}
		if instID := strings.TrimSpace(req.InstanceID); instID != "" && conn.IMInstanceID != instID {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "specified connection does not match requested instanceId"}
		}
		targetConn = conn
		foundConn = true
	}

	if !foundConn && strings.TrimSpace(req.InstanceID) != "" {
		for _, c := range activeConns {
			if c.IMInstanceID == strings.TrimSpace(req.InstanceID) {
				targetConn = c
				foundConn = true
				break
			}
		}
		if !foundConn {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: fmt.Sprintf("no active connection found for instance %q", req.InstanceID)}
		}
	}

	if !foundConn {
		// If there are multiple different IMInstanceIDs and none specified, fail-closed for multi-instance safety
		distinctInstances := make(map[string]bool)
		for _, c := range activeConns {
			if c.IMInstanceID != "" {
				distinctInstances[c.IMInstanceID] = true
			}
		}
		if len(distinctInstances) > 1 {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "multiple IM instances found in workspace; please specify instanceId"}
		}
		if len(activeConns) > 0 {
			targetConn = activeConns[0]
			foundConn = true
		}
	}

	if !foundConn {
		return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "no active Mattermost connection found in workspace"}
	}

	imInstanceID := targetConn.IMInstanceID

	// 2. Open credentials
	secret, found, err := s.controlDB.ConnectionSecret(targetConn.ID)
	if err != nil {
		return nil, &channelProvisionError{StatusCode: http.StatusInternalServerError, Message: err.Error()}
	}
	if !found {
		return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "connection credentials secret not found"}
	}

	secValues, err := openConnectionSecret(secret)
	if err != nil {
		return nil, &channelProvisionError{StatusCode: http.StatusInternalServerError, Message: err.Error()}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(secValues["baseUrl"]), "/")
	botToken := strings.TrimSpace(secValues["botToken"])
	if baseURL == "" || botToken == "" {
		return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "Mattermost server URL or bot token not configured"}
	}

	client := imbridge.NewMattermostClient(baseURL, botToken, nil)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	// 3. Resolve Mattermost team deterministically (no arbitrary teams[0] blind-guess)
	teamID := strings.TrimSpace(req.TeamID)
	if teamID == "" && targetConn.ProfileJSON != "" {
		var prof map[string]any
		if err := json.Unmarshal([]byte(targetConn.ProfileJSON), &prof); err == nil {
			if tid, ok := prof["teamId"].(string); ok && tid != "" {
				teamID = tid
			} else if tid, ok := prof["defaultTeamId"].(string); ok && tid != "" {
				teamID = tid
			}
		}
	}

	if teamID == "" {
		teams, err := client.GetMyTeams(ctx)
		if err != nil {
			return nil, &channelProvisionError{StatusCode: http.StatusBadGateway, Message: fmt.Sprintf("failed to get Mattermost teams: %v", err)}
		}
		if len(teams) == 0 {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "Mattermost bot user has not joined any team"}
		}
		if len(teams) == 1 {
			teamID = teams[0].ID
		} else {
			return nil, &channelProvisionError{StatusCode: http.StatusBadRequest, Message: "Bot belongs to multiple Mattermost teams; please specify teamId"}
		}
	}

	// 4. Resolve Channel according to mode (Strict Create vs Link)
	cleanName := imbridge.SanitizeMattermostChannelName(req.ChannelName)
	if cleanName == "" {
		cleanName = imbridge.SanitizeMattermostChannelName("proj-" + projectName)
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = "#" + cleanName
	}

	channelType := "P"
	if strings.EqualFold(req.Visibility, "public") {
		channelType = "O"
	}

	var mmChan *imbridge.MattermostChannel

	if mode == "link" {
		// Strictly find existing channel. If not found, return 404. DO NOT create!
		existing, err := client.GetChannelByName(ctx, teamID, cleanName)
		if err != nil {
			if errors.Is(err, imbridge.ErrChannelNotFound) {
				return nil, &channelProvisionError{StatusCode: http.StatusNotFound, Code: "channel_not_found", Message: fmt.Sprintf("要关联的 Mattermost 频道 %q 不存在，请检查频道名称或切换为自动创建频道", cleanName)}
			}
			return nil, &channelProvisionError{StatusCode: http.StatusBadGateway, Message: fmt.Sprintf("failed to get Mattermost channel %q: %v", cleanName, err)}
		}
		mmChan = existing
	} else {
		// Create channel
		created, err := client.CreateChannel(ctx, imbridge.MattermostChannel{
			TeamID:      teamID,
			Name:        cleanName,
			DisplayName: displayName,
			Type:        channelType,
			Purpose:     fmt.Sprintf("Multigent project collaboration channel for %s", projectName),
		})
		if err != nil {
			if errors.Is(err, imbridge.ErrChannelAlreadyExists) {
				// Allow idempotent retry only if THIS project already linked to this exact channel
				if existingLink, found, _ := s.controlDB.GetProjectChannelLink(workspaceID, projectName, provider, imInstanceID); found {
					if existing, getErr := client.GetChannelByName(ctx, teamID, cleanName); getErr == nil && existing.ID == existingLink.ChannelID {
						mmChan = existing
					}
				}
				if mmChan == nil {
					return nil, &channelProvisionError{StatusCode: http.StatusConflict, Code: "channel_already_exists", Message: fmt.Sprintf("Mattermost 频道 %q 已存在。如需复用已有频道请切换为“关联已有频道(link)”，或更改新频道名称。", cleanName)}
				}
			} else {
				return nil, &channelProvisionError{StatusCode: http.StatusBadGateway, Message: fmt.Sprintf("failed to create Mattermost channel: %v", err)}
			}
		} else {
			mmChan = created
		}
	}

	// 5. Invite all active agent bots in the workspace/instance to the channel
	failedBots := make([]string, 0)
	failedBotIDs := make(map[string]string)

	for _, ic := range activeConns {
		if imInstanceID != "" && ic.IMInstanceID != imInstanceID {
			continue
		}
		var botID string
		if ic.ProfileJSON != "" {
			var prof map[string]any
			if err := json.Unmarshal([]byte(ic.ProfileJSON), &prof); err == nil {
				if bid, ok := prof["botId"].(string); ok && bid != "" {
					botID = bid
				} else if aid, ok := prof["appId"].(string); ok && aid != "" {
					botID = aid
				}
			}
		}
		if botID == "" {
			if sec, sFound, _ := s.controlDB.ConnectionSecret(ic.ID); sFound {
				if vals, err := openConnectionSecret(sec); err == nil {
					botID = vals["appId"]
				}
			}
		}
		if botID != "" {
			_ = client.AddUserToTeam(ctx, teamID, botID)
			if err := client.AddUserToChannel(ctx, mmChan.ID, botID); err != nil {
				log.Printf("[project-channel] failed to add bot %s (%s) to channel %s: %v", ic.ConnectionName, botID, mmChan.ID, err)
				failedBots = append(failedBots, botID)
				failedBotIDs[botID] = err.Error()
			}
		}
	}

	// 6. Invite bound workspace members to the channel strictly scoped to IM instance
	memberUsernames := req.MemberUsernames
	if len(memberUsernames) == 0 {
		if memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
			WorkspaceID: workspaceID,
			ProjectID:   projectName,
			MemberType:  "user",
		}); err == nil {
			for _, m := range memberships {
				if u := strings.TrimSpace(m.MemberID); u != "" {
					memberUsernames = append(memberUsernames, u)
				}
			}
		}
	}

	invitedMembers := make([]string, 0, len(memberUsernames))
	failedMembers := make([]string, 0)
	unboundMembers := make([]string, 0)

	for _, username := range memberUsernames {
		username = strings.TrimSpace(username)
		if username == "" {
			continue
		}

		externalUserID, _ := s.controlDB.UserExternalIDForInstance(workspaceID, username, provider, imInstanceID)
		if externalUserID == "" && imInstanceID == "" {
			exts, err := s.controlDB.ListExternalIdentities(controldb.ExternalIdentityFilter{
				WorkspaceID: workspaceID,
				UserID:      username,
				Provider:    provider,
			})
			if err == nil && len(exts) > 0 {
				externalUserID = exts[0].ExternalUserID
			}
		}

		if externalUserID != "" {
			_ = client.AddUserToTeam(ctx, teamID, externalUserID)
			if err := client.AddUserToChannel(ctx, mmChan.ID, externalUserID); err != nil {
				log.Printf("[project-channel] warning: failed to add member %s (%s) to channel %s: %v", username, externalUserID, mmChan.ID, err)
				failedMembers = append(failedMembers, username)
			} else {
				invitedMembers = append(invitedMembers, username)
			}
		} else {
			unboundMembers = append(unboundMembers, username)
		}
	}

	// 7. Persist first-class ProjectChannelLink in DB
	nowStr := time.Now().UTC().Format(time.RFC3339)
	linkID := newChannelID("pcl")
	if existingLink, found, _ := s.controlDB.GetProjectChannelLink(workspaceID, projectName, provider, imInstanceID); found {
		linkID = existingLink.ID
	}

	link := controldb.ProjectChannelLink{
		ID:           linkID,
		WorkspaceID:  workspaceID,
		ProjectID:    projectName,
		Provider:     provider,
		IMInstanceID: imInstanceID,
		TeamID:       teamID,
		ChannelID:    mmChan.ID,
		ChannelName:  mmChan.Name,
		DisplayName:  mmChan.DisplayName,
		Visibility:   req.Visibility,
		Status:       "active",
		CreatedBy:    actorUsername,
		CreatedAt:    nowStr,
		UpdatedAt:    nowStr,
	}
	if err := s.controlDB.UpsertProjectChannelLink(link); err != nil {
		log.Printf("[project-channel] failed to upsert project channel link for %s: %v", projectName, err)
	}

	// 8. Resolve agents to bind
	type agentEntry struct {
		Name     string
		WorkerID string
	}
	agentMap := make(map[string]agentEntry)

	for _, workerID := range req.WorkerIDs {
		workerID = strings.TrimSpace(workerID)
		if workerID == "" {
			continue
		}
		agentName := workerID
		if w, found, err := s.controlDB.AgentWorkerByID(workspaceID, workerID); err == nil && found && strings.TrimSpace(w.Name) != "" {
			agentName = strings.TrimSpace(w.Name)
		}
		agentMap[agentName] = agentEntry{Name: agentName, WorkerID: workerID}
	}

	if memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectName,
	}); err == nil {
		for _, m := range memberships {
			title := strings.TrimSpace(m.Title)
			memberID := strings.TrimSpace(m.MemberID)
			agentName := title
			if agentName == "" {
				agentName = memberID
			}
			if agentName != "" {
				agentMap[agentName] = agentEntry{Name: agentName, WorkerID: memberID}
			}
		}
	}

	if len(agentMap) == 0 {
		agentMap["Mira"] = agentEntry{Name: "Mira"}
		agentMap["Lina"] = agentEntry{Name: "Lina"}
	}

	// 9. Persist AgentChannelBinding and AgentChannelTarget scoped strictly to THIS project
	boundAgents := make([]string, 0, len(agentMap))
	failedAgents := make([]string, 0)
	skippedAgents := make([]string, 0)

	// Pre-assign connections 1:1 to agents to prevent DEFECT-C3 duplicate bindings
	assignedConnByAgent := make(map[string]controldb.Connection)
	usedConnIDs := make(map[string]string) // connID -> agentName

	// Pass 1: Named matches take highest priority
	for agentName := range agentMap {
		lowerAgent := strings.ToLower(agentName)
		for _, ic := range activeConns {
			if imInstanceID != "" && ic.IMInstanceID != imInstanceID {
				continue
			}
			if _, used := usedConnIDs[ic.ID]; used {
				continue
			}
			if strings.Contains(strings.ToLower(ic.ConnectionName), lowerAgent) || strings.Contains(strings.ToLower(ic.ProfileJSON), lowerAgent) {
				assignedConnByAgent[agentName] = ic
				usedConnIDs[ic.ID] = agentName
				break
			}
		}
	}

	// Pass 2: For agents without named match, allocate targetConn or an unused activeConn (at most one per agent)
	for agentName := range agentMap {
		if _, has := assignedConnByAgent[agentName]; has {
			continue
		}
		if targetConn.ID != "" {
			if _, used := usedConnIDs[targetConn.ID]; !used {
				assignedConnByAgent[agentName] = targetConn
				usedConnIDs[targetConn.ID] = agentName
				continue
			}
		}
		for _, ic := range activeConns {
			if imInstanceID != "" && ic.IMInstanceID != imInstanceID {
				continue
			}
			if _, used := usedConnIDs[ic.ID]; !used {
				assignedConnByAgent[agentName] = ic
				usedConnIDs[ic.ID] = agentName
				break
			}
		}
	}

	for agentName, entry := range agentMap {
		agentConn, ok := assignedConnByAgent[agentName]
		if !ok || agentConn.ID == "" {
			log.Printf("[project-channel] skipping channel binding for %s/%s: no dedicated bot connection available in %s; skipping to avoid DEFECT-C3 routing conflict", projectName, agentName, provider)
			skippedAgents = append(skippedAgents, agentName)
			// If an existing binding exists with a duplicated connection in this project, mark it unbound
			if oldBindings, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
				WorkspaceID: workspaceID,
				ProjectID:   projectName,
				AgentID:     agentName,
				Provider:    provider,
			}); len(oldBindings) > 0 {
				for _, ob := range oldBindings {
					if ob.Status == "connected" {
						ob.Status = "unbound"
						_ = s.controlDB.UpsertAgentChannelBinding(ob)
					}
				}
			}
			continue
		}

		var agentBotID string
		if agentConn.ProfileJSON != "" {
			var prof map[string]any
			if err := json.Unmarshal([]byte(agentConn.ProfileJSON), &prof); err == nil {
				if bid, ok := prof["botId"].(string); ok && bid != "" {
					agentBotID = bid
				} else if aid, ok := prof["appId"].(string); ok && aid != "" {
					agentBotID = aid
				}
			}
		}

		// Strictly find existing binding FOR THIS PROJECT ONLY (no cross-project worker ID collision!)
		existingBindings, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
			WorkspaceID: workspaceID,
			ProjectID:   projectName,
			AgentID:     agentName,
			Provider:    provider,
		})

		bindingID := newChannelID("chan")
		createdAt := nowStr
		if len(existingBindings) > 0 {
			bindingID = existingBindings[0].ID
			createdAt = existingBindings[0].CreatedAt
		}

		botFailed := false
		if agentBotID != "" {
			if _, failed := failedBotIDs[agentBotID]; failed {
				botFailed = true
			}
		}

		bindingStatus := "connected"
		if botFailed {
			bindingStatus = "error"
		}

		metaMap := map[string]any{
			"appId": agentBotID,
		}
		if len(existingBindings) > 0 && strings.TrimSpace(existingBindings[0].MetadataJSON) != "" {
			_ = json.Unmarshal([]byte(existingBindings[0].MetadataJSON), &metaMap)
			metaMap["appId"] = agentBotID
		}
		metaBytes, _ := json.Marshal(metaMap)

		binding := controldb.AgentChannelBinding{
			ID:             bindingID,
			WorkspaceID:    workspaceID,
			ProjectID:      projectName,
			AgentID:        agentName,
			AgentWorkerID:  entry.WorkerID,
			Provider:       provider,
			ConnectionID:   agentConn.ID,
			ExternalBotID:  agentBotID,
			ExternalChatID: mmChan.ID,
			Status:         bindingStatus,
			MetadataJSON:   string(metaBytes),
			CreatedBy:      actorUsername,
			CreatedAt:      createdAt,
			UpdatedAt:      nowStr,
			LastActivityAt: nowStr,
		}
		if err := s.controlDB.UpsertAgentChannelBinding(binding); err != nil {
			log.Printf("[project-channel] failed to upsert binding for %s/%s: %v", projectName, agentName, err)
			continue
		}

		if botFailed {
			failedAgents = append(failedAgents, agentName)
			// Do NOT create/update active AgentChannelTarget if bot failed to join channel!
			continue
		}

		// Find target for this binding and channel
		targets, _ := s.controlDB.ListAgentChannelTargets(controldb.AgentChannelTargetFilter{
			WorkspaceID:      workspaceID,
			ChannelBindingID: binding.ID,
		})
		targetID := newChannelID("cht")
		for _, t := range targets {
			if t.ExternalChatID == mmChan.ID {
				targetID = t.ID
				break
			}
		}

		target := controldb.AgentChannelTarget{
			ID:               targetID,
			WorkspaceID:      workspaceID,
			ChannelBindingID: binding.ID,
			Provider:         provider,
			TargetType:       "chat",
			DisplayName:      mmChan.DisplayName,
			ExternalChatID:   mmChan.ID,
			CreatedBy:        actorUsername,
			CreatedAt:        createdAt,
			UpdatedAt:        nowStr,
			LastActivityAt:   nowStr,
		}
		if err := s.controlDB.UpsertAgentChannelTarget(target); err != nil {
			log.Printf("[project-channel] failed to upsert target for %s/%s: %v", projectName, agentName, err)
		}

		boundAgents = append(boundAgents, agentName)
	}

	// 10. Notify bridges
	go s.refreshAgentIMBridges()

	// 11. Audit log
	statusStr := "success"
	var warnings []string
	if len(failedMembers) > 0 {
		warnings = append(warnings, fmt.Sprintf("部分成员未能成功拉入频道: %s", strings.Join(failedMembers, ", ")))
	}
	if len(failedAgents) > 0 {
		warnings = append(warnings, fmt.Sprintf("部分 Agent Bot 未能加入频道: %s", strings.Join(failedAgents, ", ")))
	}
	if len(skippedAgents) > 0 {
		warnings = append(warnings, fmt.Sprintf("部分 Agent（%s）未配置独立的 %s Bot 连接已跳过绑定", strings.Join(skippedAgents, ", "), provider))
	}
	if len(unboundMembers) > 0 {
		warnings = append(warnings, fmt.Sprintf("部分成员未绑定 %s 账号: %s", provider, strings.Join(unboundMembers, ", ")))
	}
	var warningMsg string
	if len(failedMembers) > 0 || len(failedAgents) > 0 {
		statusStr = "partial"
	}
	if len(warnings) > 0 {
		warningMsg = strings.Join(warnings, "；")
	}

	return &projectChannelProvisionResponse{
		OK:             true,
		Status:         statusStr,
		Provider:       provider,
		InstanceID:     imInstanceID,
		ChannelID:      mmChan.ID,
		ChannelName:    mmChan.Name,
		DisplayName:    mmChan.DisplayName,
		Visibility:     req.Visibility,
		TeamID:         teamID,
		BoundAgents:    boundAgents,
		FailedAgents:   failedAgents,
		FailedBots:     failedBots,
		InvitedMembers: invitedMembers,
		FailedMembers:  failedMembers,
		UnboundMembers: unboundMembers,
		Warning:        warningMsg,
	}, nil
}

type projectChannelListItem struct {
	Link     controldb.ProjectChannelLink    `json:"link"`
	Bindings []controldb.AgentChannelBinding `json:"bindings"`
	HasError bool                            `json:"hasError"`
}

type listProjectChannelsResponse struct {
	OK       bool                     `json:"ok"`
	Channels []projectChannelListItem `json:"channels"`
}

func (s *Server) handleListProjectChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	projectName := strings.TrimSpace(r.PathValue("name"))
	if projectName == "" {
		s.jsonError(w, http.StatusBadRequest, "project name is required")
		return
	}

	if !s.checkProjectAccess(w, r, projectName) {
		return
	}

	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}

	links, err := s.controlDB.ListProjectChannelLinks(workspaceID, projectName)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to list project channel links: "+err.Error())
		return
	}

	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectName,
	})
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to list agent channel bindings: "+err.Error())
		return
	}

	bindingsByChatID := make(map[string][]controldb.AgentChannelBinding)
	for _, b := range bindings {
		bindingsByChatID[b.ExternalChatID] = append(bindingsByChatID[b.ExternalChatID], b)
	}

	items := make([]projectChannelListItem, 0, len(links))
	for _, link := range links {
		chanBindings := bindingsByChatID[link.ChannelID]
		hasErr := false
		for _, b := range chanBindings {
			if b.Status == "error" {
				hasErr = true
				break
			}
		}
		items = append(items, projectChannelListItem{
			Link:     link,
			Bindings: chanBindings,
			HasError: hasErr,
		})
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(listProjectChannelsResponse{
		OK:       true,
		Channels: items,
	})
}

