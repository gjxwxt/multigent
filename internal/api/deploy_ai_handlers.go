package api

// Deploy AI drawer (deploy center phase 2, M4). A deploy-page-only chat panel
// that is deliberately tool-free: unlike the preview Copilot it never touches
// code, worktrees, or the workflow write lock. The model does exactly two
// things per turn — answer in prose, and (when the user reported a problem)
// emit a structured FINDINGS block that the server parses into "findings"
// (symptom / expectation / repro). The user can then check findings and post
// them as real tasks (created_by "deploy-ai") via the standard task pipeline.
//
// Security: both routes live on the token-auth main mux with
// checkProjectAccess; chat additionally rate-limits per project. The model
// call reuses resolveDesignModelProvider (the project agent's bound model
// account), so no new credential surface is introduced. Output tokens are
// hard-capped (deployAIMaxOutputTokens) per the OD short-conversation red
// line.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

const (
	// deployAIDefaultModel keeps the drawer on the OD short-conversation
	// model family (glm-5.3-flash) unless the caller pins another.
	deployAIDefaultModel = odDefaultModel
	// deployAIMaxOutputTokens is the 16k hard cap carried over from the OD
	// design-gate red line: a deploy triage conversation must never run a
	// long thinking chain.
	deployAIMaxOutputTokens = 16384
	// deployAICreatedBy stamps tasks minted from checked findings so the
	// ledger can tell deploy-triage backlog from human-created work.
	deployAICreatedBy = "deploy-ai"
	// deployAIMaxHistoryTurns bounds how much client-side history replays
	// into the next model call.
	deployAIMaxHistoryTurns = 12
)

// deployAIFinding is one structured issue extracted from the conversation.
type deployAIFinding struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Symptom     string `json:"symptom"`
	Expectation string `json:"expectation,omitempty"`
	Repro       string `json:"repro,omitempty"`
	Severity    string `json:"severity,omitempty"`
}

// deployAIChatBody is the request for POST .../deploy/ai/chat. History is
// client-held (the drawer owns persistence in localStorage) so the endpoint
// stays stateless.
type deployAIChatBody struct {
	Message string             `json:"message"`
	History []deployAIMsg      `json:"history,omitempty"`
	Model   string             `json:"model,omitempty"`
	Project string             `json:"project,omitempty"` // optional display context override
}

type deployAIMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// deployAIChatResponse returns both the prose answer and the findings the
// server extracted from this turn's FINDINGS block (empty when none).
type deployAIChatResponse struct {
	Reply    string            `json:"reply"`
	Findings []deployAIFinding `json:"findings"`
	Model    string            `json:"model"`
}

// deployAIContextResponse is the drawer bootstrap: the platform-known facts
// the system prompt is seeded with, plus the model info the UI badges.
type deployAIContextResponse struct {
	Project         string `json:"project"`
	Model           string `json:"model"`
	DeployPort      int    `json:"deployPort,omitempty"`
	LastDeploy      string `json:"lastDeploy,omitempty"`      // "branch@shortsha"
	LastDeployBy    string `json:"lastDeployBy,omitempty"`
	HealthStatus    string `json:"healthStatus,omitempty"`    // up | down | unknown
	DeployConfigured bool  `json:"deployConfigured"`
}

// handleDeployAIContext serves GET /api/v1/projects/{name}/deploy/ai/context.
func (s *Server) handleDeployAIContext(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	p, ok := s.deployProjectRow(w, project)
	if !ok {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}
	model := deployAIDefaultModel
	if agent := s.deployAIProjectAgent(project); agent != "" {
		if creds, err := s.resolveDesignModelProvider(project, agent, deployAIDefaultModel); err == nil && creds != nil && creds.Model != "" {
			model = creds.Model
		}
	}
	requests, err := s.controlDB.ListDeployRequests(controldb.DeployRequestFilter{
		WorkspaceID: workspaceID,
		ProjectID:   project,
		Status:      "success",
		Limit:       1,
	})
	if err != nil {
		s.serverError(w, fmt.Errorf("list deploy requests: %w", err))
		return
	}
	out := deployAIContextResponse{
		Project:          project,
		Model:            model,
		DeployPort:       p.DeployPort,
		DeployConfigured: strings.TrimSpace(os.Getenv(deployHostEnv)) != "",
	}
	if len(requests) > 0 {
		req := requests[0]
		out.LastDeploy = req.Branch + "@" + shortDeploySHA(req.SHA)
		out.LastDeployBy = req.CreatedBy
	}
	if health := s.probeDeployedAppHealth(p, nil); health != nil {
		if status, ok := health["status"].(string); ok {
			out.HealthStatus = status
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleDeployAIChat serves POST /api/v1/projects/{name}/deploy/ai/chat.
func (s *Server) handleDeployAIChat(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.allowDesignRequest("deploy-ai:"+project, true) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "too many deploy AI messages, slow down")
		return
	}
	var body deployAIChatBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid request body")
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		s.jsonError(w, http.StatusBadRequest, "message required")
		return
	}

	agent := s.deployAIProjectAgent(project)
	model := deployAIDefaultModel
	if strings.TrimSpace(body.Model) != "" {
		model = strings.TrimSpace(body.Model)
	}
	var creds *designModelCreds
	if agent != "" {
		resolved, err := s.resolveDesignModelProvider(project, agent, model)
		if err != nil {
			s.writeDesignUpstreamError(w, err)
			return
		}
		creds = resolved
		if resolved.Model != "" {
			model = resolved.Model
		}
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" {
		s.jsonError(w, http.StatusServiceUnavailable, "no model provider resolvable for this project's agents; bind a model account first")
		return
	}

	prompt := s.buildDeployAISystemPrompt(project)
	history := sanitizeDeployAIHistory(body.History)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	reply, err := s.invokeDeployAIChat(ctx, creds, prompt, history, msg)
	if err != nil {
		log.Printf("[deploy-ai] %s: chat failed: %v", project, err)
		s.writeDesignUpstreamError(w, err)
		return
	}
	text, findings := splitDeployAIFindings(reply)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(deployAIChatResponse{
		Reply:    text,
		Findings: findings,
		Model:    model,
	})
}

// deployAIProjectAgent picks the agent whose model account backs the drawer:
// the project's first listed agent (deterministic alphabetical order), the
// same resolution the design gate's taskAgentFromAssignee ends at when the
// task has no explicit assignee.
func (s *Server) deployAIProjectAgent(project string) string {
	agents, err := s.st.ListAgents(project)
	if err != nil || len(agents) == 0 {
		return ""
	}
	for _, a := range agents {
		if a != nil && strings.TrimSpace(a.Name) != "" {
			return strings.TrimSpace(a.Name)
		}
	}
	return ""
}

// buildDeployAISystemPrompt seeds the role, the hard tool-free boundary, and
// the platform-known deploy facts. It never probes services or runs tools.
func (s *Server) buildDeployAISystemPrompt(project string) string {
	var b strings.Builder
	b.WriteString("你是 Multigent 部署中心的部署助手，帮助用户排查该项目（" + project + "）部署相关的问题。")
	b.WriteString("你只能对话与给出建议：不能执行命令、不能修改代码、不能调用任何工具，也不能声称已经替用户执行了操作。\n")
	b.WriteString("当用户描述了一个疑似问题时，你必须在他每条回答的末尾追加一个 FINDINGS 代码块（```findings ... ```），内为 JSON 数组，")
	b.WriteString("每个元素字段：title（一句话标题）、symptom（现象）、expectation（预期行为，可空）、repro（复现路径，可空）、severity（high/medium/low，可空）。")
	b.WriteString("没有新发现时省略该代码块。代码块之外用简洁中文回答。\n")
	return b.String()
}

// invokeDeployAIChat runs one stateless chat turn against the resolved model
// account. It delegates to the assistant handler's dual-protocol request
// path with a 16k output cap (deployAIMaxOutputTokens): the OD short-
// conversation red line for this drawer.
func (s *Server) invokeDeployAIChat(ctx context.Context, creds *designModelCreds, systemPrompt string, history []deployAIMsg, message string) (string, error) {
	provider := entity.APIProvider{
		Type:    creds.Protocol,
		BaseURL: creds.BaseURL,
		APIKey:  creds.APIKey,
		Model:   creds.Model,
	}
	msgs := make([]assistantChatMsg, 0, len(history)+1)
	for _, h := range history {
		msgs = append(msgs, assistantChatMsg{Role: h.Role, Content: h.Content})
	}
	return invokeDeployAIProviderChat(ctx, provider, systemPrompt, msgs, message)
}

// invokeDeployAIProviderChat mirrors invokeAssistantModel but (a) takes the
// system prompt explicitly and (b) raises the Anthropic max_tokens cap from
// the assistant's 2048 to deployAIMaxOutputTokens per the OD budget line.
func invokeDeployAIProviderChat(ctx context.Context, provider entity.APIProvider, systemPrompt string, history []assistantChatMsg, message string) (string, error) {
	messages := make([]assistantLLMMessage, 0, len(history)+1)
	for _, item := range history {
		role := strings.TrimSpace(item.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		messages = append(messages, assistantLLMMessage{Role: role, Content: content})
	}
	messages = append(messages, assistantLLMMessage{Role: "user", Content: message})
	switch strings.ToLower(strings.TrimSpace(provider.Type)) {
	case "anthropic":
		return deployAIAnthropicChat(ctx, provider, systemPrompt, messages)
	case "openai", "cursor", "custom", "":
		return assistantOpenAIChat(ctx, provider, systemPrompt, messages)
	default:
		return "", fmt.Errorf("deploy AI 暂不支持该模型账号类型：%s", provider.Type)
	}
}

// deployAIAnthropicChat is assistantAnthropicChat with the deploy drawer's
// 16k output cap; the assistant copy keeps its own 2048 budget.
func deployAIAnthropicChat(ctx context.Context, provider entity.APIProvider, systemPrompt string, messages []assistantLLMMessage) (string, error) {
	model := strings.TrimSpace(provider.Model)
	if model == "" {
		model = strings.TrimSpace(provider.Env["ANTHROPIC_MODEL"])
	}
	if model == "" {
		return "", fmt.Errorf("deploy AI 模型账号 %q 缺少默认模型", provider.Name)
	}
	apiKey := strings.TrimSpace(provider.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(provider.Env["ANTHROPIC_API_KEY"])
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(provider.Env["ANTHROPIC_AUTH_TOKEN"])
	}
	if apiKey == "" {
		return "", fmt.Errorf("deploy AI 模型账号 %q 缺少 API Key", provider.Name)
	}
	endpoint := assistantAnthropicEndpoint(provider.BaseURL)
	reqBody := map[string]any{
		"model":      model,
		"system":     systemPrompt,
		"messages":   messages,
		"max_tokens": deployAIMaxOutputTokens,
		"stream":     false,
	}
	var respBody struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type,omitempty"`
		} `json:"error,omitempty"`
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	if err := assistantPostJSON(ctx, endpoint, apiKey, headers, reqBody, &respBody); err != nil {
		return "", err
	}
	if respBody.Error != nil {
		return "", fmt.Errorf("模型服务错误：%s", respBody.Error.Message)
	}
	var parts []string
	for _, block := range respBody.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("模型服务没有返回内容")
	}
	return strings.Join(parts, "\n"), nil
}

// sanitizeDeployAIHistory keeps only plain user/assistant turns and trims to
// the last N so a bloated localStorage payload cannot balloon the prompt.
func sanitizeDeployAIHistory(history []deployAIMsg) []deployAIMsg {
	out := make([]deployAIMsg, 0, len(history))
	for _, h := range history {
		role := strings.TrimSpace(h.Role)
		content := strings.TrimSpace(h.Content)
		if (role != "user" && role != "assistant") || content == "" {
			continue
		}
		out = append(out, deployAIMsg{Role: role, Content: content})
	}
	if len(out) > deployAIMaxHistoryTurns {
		out = out[len(out)-deployAIMaxHistoryTurns:]
	}
	return out
}

// findingsPrefix markers the model is instructed to emit.
const deployAIFindingsFence = "```findings"

// splitDeployAIFindings peels the trailing ```findings JSON block off the
// reply. Parse failures never fail the turn — the prose still reaches the
// user, the findings list just stays empty (fail-open for prose, the task
// pipeline is the only path that must stay strict).
func splitDeployAIFindings(reply string) (string, []deployAIFinding) {
	idx := strings.LastIndex(reply, deployAIFindingsFence)
	if idx < 0 {
		return strings.TrimSpace(reply), nil
	}
	prose := strings.TrimSpace(reply[:idx])
	block := reply[idx:]
	end := strings.LastIndex(block, "```")
	if end < 0 {
		return prose, nil
	}
	inner := strings.TrimPrefix(strings.TrimSpace(block[:end]), deployAIFindingsFence)
	inner = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(inner), "json"))
	var findings []deployAIFinding
	if err := json.Unmarshal([]byte(inner), &findings); err != nil {
		return prose, nil
	}
	cleaned := make([]deployAIFinding, 0, len(findings))
	for i, f := range findings {
		f.Title = strings.TrimSpace(f.Title)
		f.Symptom = strings.TrimSpace(f.Symptom)
		if f.Title == "" && f.Symptom == "" {
			continue
		}
		if f.Title == "" {
			f.Title = firstDeployAISentence(f.Symptom)
		}
		f.Symptom = strings.TrimSpace(f.Symptom)
		f.Expectation = strings.TrimSpace(f.Expectation)
		f.Repro = strings.TrimSpace(f.Repro)
		f.Severity = strings.TrimSpace(f.Severity)
		f.ID = fmt.Sprintf("f%d", i+1)
		cleaned = append(cleaned, f)
	}
	if len(cleaned) == 0 {
		return prose, nil
	}
	return prose, cleaned
}

func firstDeployAISentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "。.；;，,"); i > 0 {
		return s[:i]
	}
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// deployAITaskBody is the request for POST .../deploy/ai/tasks: the checked
// findings plus the conversation digest they came from.
type deployAITaskBody struct {
	Agent            string            `json:"agent"`
	Title            string            `json:"title"`
	Findings         []deployAIFinding `json:"findings"`
	ConversationSummary string         `json:"conversationSummary,omitempty"`
	Severity         string            `json:"severity,omitempty"`
}

// handleDeployAITasks serves POST /api/v1/projects/{name}/deploy/ai/tasks.
// It funnels into the standard task-creation pipeline (RBAC, worktree,
// workflow selection all inherited) with createdBy pinned to "deploy-ai".
func (s *Server) handleDeployAITasks(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	var body deployAITaskBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid request body")
		return
	}
	if len(body.Findings) == 0 {
		s.jsonError(w, http.StatusBadRequest, "at least one finding is required")
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		if len(body.Findings) == 1 {
			title = firstDeployAISentence(body.Findings[0].Title)
		} else {
			title = fmt.Sprintf("部署问题（%d 项发现）", len(body.Findings))
		}
	}
	// Severity: an explicit body-level severity wins; otherwise the highest
	// severity across the checked findings drives type/priority mapping.
	severity := strings.TrimSpace(body.Severity)
	if severity == "" {
		severity = highestDeployAISeverity(body.Findings)
	}
	taskType := entity.TaskTypeBug
	if strings.EqualFold(severity, "low") {
		taskType = entity.TaskTypeChore
	}
	agent := strings.TrimSpace(body.Agent)
	if agent == "" {
		agent = s.deployAIProjectAgent(project)
	}
	if agent == "" {
		s.jsonError(w, http.StatusServiceUnavailable, "no agent in this project; hire one before creating tasks")
		return
	}

	var b strings.Builder
	b.WriteString("来源：部署 AI 抽屉（created_by deploy-ai）。请勿当作已验证结论，先复核再修。\n\n")
	if summary := strings.TrimSpace(body.ConversationSummary); summary != "" {
		b.WriteString("【对话摘要】\n" + summary + "\n\n")
	}
	b.WriteString("【发现项清单】\n")
	for i, f := range body.Findings {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f.Title)
		if f.Symptom != "" {
			b.WriteString("   现象：" + f.Symptom + "\n")
		}
		if f.Expectation != "" {
			b.WriteString("   预期：" + f.Expectation + "\n")
		}
		if f.Repro != "" {
			b.WriteString("   复现：" + f.Repro + "\n")
		}
		if f.Severity != "" {
			b.WriteString("   严重度：" + f.Severity + "\n")
		}
	}

	// createProjectTaskFromBody writes the 201 body itself
	// ({"id","project","agent"}); it must stay the last writer here.
	s.createProjectTaskFromBody(w, r, project, postTaskBody{
		Agent:       agent,
		Title:       title,
		Description: "部署 AI 抽屉发现项转任务",
		Prompt:      b.String(),
		Type:        string(taskType),
		Priority:    deployAIPriority(severity),
		Assignee:    project + "/" + agent,
		CreatedBy:   deployAICreatedBy,
		Labels:      []string{"deploy-ai"},
	})
}

func deployAIPriority(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "high":
		return 2
	case "medium":
		return 1
	default:
		return 0
	}
}

// highestDeployAISeverity picks the highest severity across findings
// (high > medium > low); unknown/empty ranks lowest.
func highestDeployAISeverity(findings []deployAIFinding) string {
	rank := map[string]int{"low": 1, "medium": 2, "high": 3}
	best, bestRank := "", 0
	for _, f := range findings {
		if r := rank[strings.ToLower(strings.TrimSpace(f.Severity))]; r > bestRank {
			best, bestRank = f.Severity, r
		}
	}
	return best
}
