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
}

func NewTaskThreadProjectionService(store controldb.Store, client *http.Client) *TaskThreadProjectionService {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TaskThreadProjectionService{
		store:      store,
		httpClient: client,
	}
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

	// 3. Format Task Root Post
	title := req.TaskTitle
	if title == "" {
		title = req.TaskID
	}
	pipelineInfo := req.PipelineID
	if pipelineInfo == "" {
		pipelineInfo = "standard"
	}
	initiator := req.CreatedBy
	if initiator == "" {
		initiator = "system"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### 📋 [Task] %s\n", title))
	sb.WriteString(fmt.Sprintf("> **Task ID**: `%s` | **Project**: `%s` | **Pipeline**: `%s`\n", req.TaskID, req.ProjectID, pipelineInfo))
	sb.WriteString(fmt.Sprintf("> **Initiator**: `@%s` | **Status**: 🚀 Running\n\n", initiator))
	if req.TaskSummary != "" {
		sb.WriteString("**Summary**:\n")
		sb.WriteString(req.TaskSummary)
		sb.WriteString("\n\n")
	}
	sb.WriteString("---\n*All progress updates, reviews, and handoffs for this task will be logged in this thread.*")

	postID, err := s.createPost(ctx, baseURL, botToken, channelID, "", sb.String())
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

// CloseTaskThread posts a completion message and marks the thread projection as closed.
func (s *TaskThreadProjectionService) CloseTaskThread(ctx context.Context, workspaceID, projectID, taskID, finalSummary string) error {
	active, found, err := s.store.ActiveTaskThreadProjection(workspaceID, taskID, "mattermost")
	if err != nil || !found || active.RootPostID == "" {
		return nil
	}

	baseURL, botToken, _, _, _, err := s.resolveMMTarget(workspaceID, projectID, active.ChannelID)
	if err == nil {
		var sb strings.Builder
		sb.WriteString("### 🏁 Task Delivery Completed\n")
		if finalSummary != "" {
			sb.WriteString(fmt.Sprintf("- **Final Summary**: %s\n", finalSummary))
		}
		sb.WriteString("- **Status**: Closed. All delivery stages finished.")
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
