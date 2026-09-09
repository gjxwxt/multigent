package imbridge

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"github.com/multigent/multigent/internal/workflow"
)

type TaskThreadProjectionService struct {
	store      controldb.Store
	httpClient *http.Client
	debouncer  *LiveCardDebouncer
}

func NewTaskThreadProjectionService(store controldb.Store, client *http.Client) *TaskThreadProjectionService {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	s := &TaskThreadProjectionService{
		store:      store,
		httpClient: client,
	}
	s.debouncer = NewLiveCardDebouncer(1500*time.Millisecond, s.patchLiveCardDirect)
	return s
}

// HTTPClient returns the HTTP client configured for projection service.
func (s *TaskThreadProjectionService) HTTPClient() *http.Client {
	if s != nil && s.httpClient != nil {
		return s.httpClient
	}
	return http.DefaultClient
}

type TaskRootPostRequest struct {
	WorkspaceID string
	ProjectID   string
	TaskID      string
	TaskTitle   string
	TaskSummary string
	PipelineID  string
	CreatedBy   string
	ChannelID   string // Optional: target channel ID. If empty, resolved from project bindings.
	TotalSteps  int
	ConsoleURL  string
}

type StepTransitionPostRequest struct {
	WorkspaceID  string
	ProjectID    string
	TaskID       string
	StepID       string
	StepTitle    string
	StepStatus   string // "running", "completed", "failed", "waiting_approval"
	Assignee     string
	NextAssignee string
	Summary      string
	Outputs      map[string]string
}

type HumanReviewPostRequest struct {
	WorkspaceID     string
	ProjectID       string
	TaskID          string
	StepID          string
	StepTitle       string
	Assignee        string
	Preview         workflow.ReviewResolutionPreview
	CallbackBaseURL string
}

// EnsureTaskRootPost ensures a task has an active root post thread in Mattermost.
// If an active projection already exists, its root_post_id is returned immediately.
func (s *TaskThreadProjectionService) EnsureTaskRootPost(ctx context.Context, req TaskRootPostRequest) (string, error) {
	if req.WorkspaceID == "" || req.TaskID == "" || req.ProjectID == "" {
		return "", fmt.Errorf("workspaceID, projectID, and taskID are required")
	}

	// 1. Check existing active projection
	active, found, err := s.store.ActiveTaskThreadProjection(req.WorkspaceID, req.TaskID, "mattermost")
	if err != nil {
		log.Printf("[task-thread-proj] check active projection error: %v", err)
	} else if found && active.RootPostID != "" {
		return active.RootPostID, nil
	}

	// 2. Resolve credentials and channel
	baseURL, botToken, channelID, _, _, err := s.resolveMMTarget(req.WorkspaceID, req.ProjectID, req.ChannelID)
	if err != nil {
		return "", fmt.Errorf("resolve mattermost target: %w", err)
	}
	if channelID == "" {
		return "", fmt.Errorf("no target channel configured for project %s", req.ProjectID)
	}

	totalSteps := req.TotalSteps
	if totalSteps <= 0 {
		totalSteps = 1
	}

	// 3. Format Task Root Post as Live Task Card
	content := FormatLiveCardContent(LiveCardUpdateRequest{
		WorkspaceID:   req.WorkspaceID,
		ProjectID:     req.ProjectID,
		TaskID:        req.TaskID,
		TaskTitle:     req.TaskTitle,
		PipelineID:    req.PipelineID,
		Initiator:     req.CreatedBy,
		CurrentStepID: "init",
		CurrentStep:   "任务已启动，正在准备执行环境...",
		StepStatus:    "running",
		StepIndex:     0,
		TotalSteps:    totalSteps,
		ConsoleURL:    req.ConsoleURL,
	})

	postID, err := s.createPost(ctx, baseURL, botToken, channelID, "", content)
	if err != nil {
		return "", fmt.Errorf("create task root post: %w", err)
	}

	// 4. Save TaskThreadProjection
	projID := generateProjectionID()
	now := time.Now().UTC().Format(time.RFC3339)
	proj := controldb.TaskThreadProjection{
		ID:          projID,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		TaskID:      req.TaskID,
		Provider:    "mattermost",
		ChannelID:   channelID,
		RootPostID:  postID,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.UpsertTaskThreadProjection(proj); err != nil {
		log.Printf("[task-thread-proj] warning: failed to persist TaskThreadProjection: %v", err)
	}

	return postID, nil
}

// PostStepTransition posts a step transition update into the task's thread.
func (s *TaskThreadProjectionService) PostStepTransition(ctx context.Context, req StepTransitionPostRequest) error {
	if req.WorkspaceID == "" || req.TaskID == "" {
		return fmt.Errorf("workspaceID and taskID are required")
	}

	active, found, err := s.store.ActiveTaskThreadProjection(req.WorkspaceID, req.TaskID, "mattermost")
	if err != nil {
		return fmt.Errorf("query active projection: %w", err)
	}
	if !found || active.RootPostID == "" {
		// No active thread projection for this task; ignore gracefully
		return nil
	}

	baseURL, botToken, _, _, _, err := s.resolveMMTarget(req.WorkspaceID, req.ProjectID, active.ChannelID)
	if err != nil {
		return fmt.Errorf("resolve credentials: %w", err)
	}

	var statusEmoji string
	switch strings.ToLower(req.StepStatus) {
	case "completed", "success":
		statusEmoji = "✅"
	case "failed", "error":
		statusEmoji = "❌"
	case "waiting_approval", "in_review", "review":
		statusEmoji = "🚨"
	default:
		statusEmoji = "🔄"
	}

	var sb strings.Builder
	stepTitle := req.StepTitle
	if stepTitle == "" {
		stepTitle = req.StepID
	}
	sb.WriteString(fmt.Sprintf("#### %s Step: %s (`%s`)\n", statusEmoji, stepTitle, req.StepID))
	sb.WriteString(fmt.Sprintf("- **Status**: `%s`\n", req.StepStatus))
	if req.Assignee != "" {
		sb.WriteString(fmt.Sprintf("- **Assignee**: `%s`\n", req.Assignee))
	}
	if req.NextAssignee != "" {
		sb.WriteString(fmt.Sprintf("- **Next Assignee**: `@%s`\n", req.NextAssignee))
	}
	if req.Summary != "" {
		sb.WriteString(fmt.Sprintf("- **Summary**: %s\n", req.Summary))
	}

	if len(req.Outputs) > 0 {
		sb.WriteString("- **Key Outputs**:\n")
		for k, v := range req.Outputs {
			if strings.TrimSpace(v) != "" {
				sb.WriteString(fmt.Sprintf("  - **%s**: %s\n", k, v))
			}
		}
	}

	_, err = s.createPost(ctx, baseURL, botToken, active.ChannelID, active.RootPostID, sb.String())
	if err != nil {
		return fmt.Errorf("post step transition reply: %w", err)
	}
	return nil
}

// PostHumanReviewCard posts an interactive human review gate card into the task thread.
func (s *TaskThreadProjectionService) PostHumanReviewCard(ctx context.Context, req HumanReviewPostRequest) (string, error) {
	if req.WorkspaceID == "" || req.TaskID == "" {
		return "", fmt.Errorf("workspaceID and taskID are required")
	}

	active, found, err := s.store.ActiveTaskThreadProjection(req.WorkspaceID, req.TaskID, "mattermost")
	if err != nil {
		return "", fmt.Errorf("query active projection: %w", err)
	}
	if !found || active.RootPostID == "" {
		return "", nil
	}

	baseURL, botToken, _, connID, hmacSecret, err := s.resolveMMTarget(req.WorkspaceID, req.ProjectID, active.ChannelID)
	if err != nil {
		return "", fmt.Errorf("resolve credentials: %w", err)
	}

	callbackURL := req.CallbackBaseURL
	if callbackURL == "" {
		callbackURL = os.Getenv("MULTIGENT_CONSOLE_URL")
	}
	if callbackURL == "" {
		callbackURL = os.Getenv("MULTIGENT_PUBLIC_URL")
	}
	if callbackURL == "" {
		callbackURL = os.Getenv("CHATOPS_CALLBACK_BASE_URL")
	}
	if strings.TrimSpace(callbackURL) == "" {
		return "", errors.New("CHATOPS_CALLBACK_BASE_URL is required to generate interactive chatops action cards")
	}

	attachment := FormatHumanReviewAttachment(req.WorkspaceID, req.ProjectID, req.TaskID, active.ChannelID, connID, req.Preview, hmacSecret, callbackURL)
	props := map[string]any{
		"attachments": []any{attachment},
	}

	message := fmt.Sprintf("#### ⚠️ 等待人工审核: %s", req.StepTitle)
	postID, err := s.createPostWithProps(ctx, baseURL, botToken, active.ChannelID, active.RootPostID, message, props)
	if err != nil {
		return "", fmt.Errorf("post human review card: %w", err)
	}
	return postID, nil
}

// FormatHumanReviewAttachment builds a Mattermost attachment with Interactive Buttons based on workflow.ReviewResolutionPreview.
func FormatHumanReviewAttachment(workspaceID, projectID, taskID, channelID, connectionID string, preview workflow.ReviewResolutionPreview, signingSecret, callbackBaseURL string) map[string]any {
	callbackBaseURL = strings.TrimRight(callbackBaseURL, "/")

	fields := make([]map[string]any, 0, len(preview.Parameters))
	for _, p := range preview.Parameters {
		var title, val string
		short := true
		switch p.Kind {
		case workflow.KindEvidence:
			title = fmt.Sprintf("🔒 %s (客观事实)", p.Label)
			val = p.CandidateValue
		case workflow.KindInheritedContract:
			title = fmt.Sprintf("📄 %s (继承契约)", p.Label)
			val = p.CandidateValue
			short = false
		case workflow.KindHumanDecision:
			title = fmt.Sprintf("❓ %s (待决策)", p.Label)
			val = "*(点击下方按钮在弹窗中选择/填写)*"
		default:
			title = p.Label
			val = p.CandidateValue
		}
		if val == "" {
			val = "(未提供)"
		}
		fields = append(fields, map[string]any{
			"title": title,
			"value": val,
			"short": short,
		})
	}

	// Build action buttons according to UXMode
	// Token TTL: 2 hours (protected by CAS & dynamic RBAC)
	exp := time.Now().UTC().Add(2 * time.Hour).Unix()
	actionURL := callbackBaseURL + "/api/v1/im/mattermost/actions"

	actions := make([]map[string]any, 0, 3)

	createBtn := func(id, name, style, actionVerb string) map[string]any {
		nonce := generateNonce()
		tok, _ := SignActionToken(signingSecret, ActionTokenPayload{
			WorkspaceID:          workspaceID,
			ProjectID:            projectID,
			TaskID:               taskID,
			StepID:               preview.StepID,
			Action:               actionVerb,
			ChannelID:            channelID,
			ConnectionID:         connectionID,
			ExpectedStateVersion: preview.ExpectedStateVersion,
			ReviewSnapshotHash:   preview.ReviewSnapshotHash,
			Nonce:                nonce,
			ExpiresAt:            exp,
		})
		return map[string]any{
			"id":    id,
			"name":  name,
			"type":  "button",
			"style": style,
			"integration": map[string]any{
				"url": actionURL,
				"context": map[string]any{
					"action_token": tok,
					"action":       actionVerb,
				},
			},
		}
	}

	switch preview.UXMode {
	case "can_override":
		actions = append(actions,
			createBtn("act-approve", "✅ 批准通过 (Approve)", "success", "approve"),
			createBtn("act-edit", "✏️ 查看并微调... (Review / Edit)", "primary", "edit"),
			createBtn("act-reject", "❌ 打回修改 (Reject)", "danger", "reject"),
		)
	case "decision_required":
		actions = append(actions,
			createBtn("act-review-approve", "📋 填写参数并审批... (Review & Approve)", "primary", "review_approve"),
			createBtn("act-reject", "❌ 打回修改 (Reject)", "danger", "reject"),
		)
	default: // "zero_input"
		actions = append(actions,
			createBtn("act-approve", "✅ 批准通过 (Approve)", "success", "approve"),
			createBtn("act-reject", "❌ 打回修改 (Reject)", "danger", "reject"),
		)
	}

	return map[string]any{
		"color":   "#f59e0b",
		"title":   fmt.Sprintf("审核闸门: %s", preview.StepTitle),
		"text":    "请仔细核对上述参数与证据。选择对应的审批操作：",
		"fields":  fields,
		"actions": actions,
	}
}

// RemoveCardActionsAndSetStatus updates the post props via PATCH to remove interactive actions and show completed status.
func (s *TaskThreadProjectionService) RemoveCardActionsAndSetStatus(ctx context.Context, workspaceID, projectID, channelID, postID, statusText string) error {
	baseURL, botToken, _, _, _, err := s.resolveMMTarget(workspaceID, projectID, channelID)
	if err != nil {
		return fmt.Errorf("resolve credentials: %w", err)
	}

	patchPayload := map[string]any{
		"message": fmt.Sprintf("#### 🔒 人工审核已完结\n> %s", statusText),
		"props": map[string]any{
			"attachments": []any{
				map[string]any{
					"color": "#10b981",
					"title": "人工审核已完成 (已归档)",
					"text":  statusText,
				},
			},
		},
	}

	baseURL = strings.TrimRight(baseURL, "/")
	bodyBytes, err := json.Marshal(patchPayload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/api/v4/posts/"+postID+"/patch", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mattermost patch post (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// UpdateTaskRootPostLiveCard queues a Live Card update (throttled by 1.5s in-memory debouncer).
func (s *TaskThreadProjectionService) UpdateTaskRootPostLiveCard(ctx context.Context, req LiveCardUpdateRequest) error {
	if s == nil || s.debouncer == nil {
		return nil
	}
	return s.debouncer.Schedule(ctx, req)
}

func (s *TaskThreadProjectionService) patchLiveCardDirect(ctx context.Context, req LiveCardUpdateRequest) error {
	if req.WorkspaceID == "" || req.TaskID == "" {
		return fmt.Errorf("workspaceID and taskID are required")
	}
	active, found, err := s.store.ActiveTaskThreadProjection(req.WorkspaceID, req.TaskID, "mattermost")
	if err != nil {
		return fmt.Errorf("query active projection: %w", err)
	}
	if !found || active.RootPostID == "" {
		return nil
	}

	baseURL, botToken, _, _, _, err := s.resolveMMTarget(req.WorkspaceID, req.ProjectID, active.ChannelID)
	if err != nil {
		return fmt.Errorf("resolve credentials: %w", err)
	}

	content := FormatLiveCardContent(req)
	patchPayload := map[string]any{
		"message": content,
	}

	baseURL = strings.TrimRight(baseURL, "/")
	bodyBytes, err := json.Marshal(patchPayload)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/api/v4/posts/"+active.RootPostID+"/patch", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+botToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mattermost patch live card (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// CloseTaskThread posts a completion message and marks the thread projection as closed.
func (s *TaskThreadProjectionService) CloseTaskThread(ctx context.Context, workspaceID, projectID, taskID, finalSummary string, totalSteps int, consoleURL string) error {
	active, found, err := s.store.ActiveTaskThreadProjection(workspaceID, taskID, "mattermost")
	if err != nil || !found || active.RootPostID == "" {
		return nil
	}

	if totalSteps <= 0 {
		totalSteps = 1
	}

	// Update Live Task Card to final completed state immediately
	_ = s.patchLiveCardDirect(ctx, LiveCardUpdateRequest{
		WorkspaceID:    workspaceID,
		ProjectID:      projectID,
		TaskID:         taskID,
		StepStatus:     "completed",
		CurrentStep:    "全部交付阶段完成",
		CurrentStepID:  "done",
		StepIndex:      totalSteps,
		TotalSteps:     totalSteps,
		QualitySummary: "✓ 全流程顺利完结 | 已归档",
		ConsoleURL:     consoleURL,
		ForceImmediate: true,
	})

	baseURL, botToken, _, _, _, err := s.resolveMMTarget(workspaceID, projectID, active.ChannelID)
	if err == nil {
		var sb strings.Builder
		sb.WriteString("### 🏁 任务交付已完结 (Task Delivery Completed)\n")
		if finalSummary != "" {
			sb.WriteString(fmt.Sprintf("- **最终摘要**: %s\n", finalSummary))
		}
		sb.WriteString("- **状态**: 流程顺利闭环，已归档。")
		_, _ = s.createPost(ctx, baseURL, botToken, active.ChannelID, active.RootPostID, sb.String())
	}

	return s.store.CloseTaskThreadProjection(workspaceID, taskID, "mattermost")
}

func (s *TaskThreadProjectionService) createPost(ctx context.Context, baseURL, botToken, channelID, rootID, message string) (string, error) {
	return s.createPostWithProps(ctx, baseURL, botToken, channelID, rootID, message, nil)
}

func (s *TaskThreadProjectionService) createPostWithProps(ctx context.Context, baseURL, botToken, channelID, rootID, message string, props map[string]any) (string, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	payload := map[string]any{
		"channel_id": channelID,
		"message":    message,
	}
	if rootID != "" {
		payload["root_id"] = rootID
	}
	if props != nil {
		payload["props"] = props
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v4/posts", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("mattermost create post (%d): %s", resp.StatusCode, string(b))
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("decode create post response: %w", err)
	}
	return created.ID, nil
}

func (s *TaskThreadProjectionService) resolveMMTarget(workspaceID, projectID, preferredChannelID string) (baseURL, botToken, channelID, connectionID, hmacSecret string, err error) {
	bindings, err := s.store.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		Provider:    "mattermost",
		Status:      "connected",
	})
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("list bindings: %w", err)
	}

	// Fallback to any connected mattermost binding in workspace if none for this project
	if len(bindings) == 0 {
		bindings, err = s.store.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
			WorkspaceID: workspaceID,
			Provider:    "mattermost",
			Status:      "connected",
		})
		if err != nil {
			return "", "", "", "", "", fmt.Errorf("list workspace bindings: %w", err)
		}
	}

	if len(bindings) == 0 {
		return "", "", "", "", "", fmt.Errorf("no connected mattermost binding found")
	}

	b := bindings[0]
	connectionID = b.ConnectionID
	secret, found, err := s.store.ConnectionSecret(b.ConnectionID)
	if err != nil || !found {
		return "", "", "", "", "", fmt.Errorf("connection secret not found for %s: %v", b.ConnectionID, err)
	}
	values, err := controldb.OpenConnectionSecret(secret)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("open connection secret for %s: %w", b.ConnectionID, err)
	}

	baseURL = values["baseUrl"]
	botToken = values["botToken"]
	hmacSecret = strings.TrimSpace(values["bridgeHmacSecret"])
	if hmacSecret == "" {
		hmacSecret = botToken // fallback to botToken as signing key
	}

	channelID = preferredChannelID
	if channelID == "" {
		if link, found, _ := s.store.GetProjectChannelLink(workspaceID, projectID, "mattermost", ""); found && link.ChannelID != "" {
			channelID = link.ChannelID
		}
	}
	if channelID == "" {
		channelID = b.ExternalChatID
	}

	return baseURL, botToken, channelID, connectionID, hmacSecret, nil
}

func generateProjectionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ttp-" + hex.EncodeToString(b)
}

func generateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
