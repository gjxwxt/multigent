package api

// Delivery preplan (large-requirement optimization plan stage 1, W1):
// a STATELESS draft generator for the CreateTaskDialog batch mode. One LLM
// call over the pasted requirement (or project-library asset content)
// produces a DeliveryPlan DRAFT + scale hint; the draft is returned in the
// response and never persisted here. Persistence happens when the operator
// sends it with the task creation body (vars.delivery_plan_draft) and — the
// authoritative moment — when the contract review approves the run's plan
// (plan_freeze.go). The in-run scale_gate stays the ONLY formal verdict:
// the draft merely accelerates it, exactly like the plan doc's "模型提议、
// 人审拍板、引擎执行" split, moved earlier in time.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/assets"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// preplanMaxInputBytes caps the requirement text accepted in one call. The
// 16k-token output cap (deployAIMaxOutputTokens) bounds the reply side; this
// bounds the prompt side. 512KB comfortably covers a 156KB military SRS.
const preplanMaxInputBytes = 512 * 1024

// preplanMaxOutputTokens is the preplan-specific output budget. The shared
// 16k cap (deployAIMaxOutputTokens) is calibrated for deploy-AI chats; live
// incident 2026-10-03 20:57 (gateway row 330000): a 163KB requirement made
// the model emit a full-length draft that hit the 16k ceiling mid-JSON —
// output truncated, unparseable, "unexpected end of JSON input". 32k is
// accepted by the gateway (live probe 200) and covers a 40-package draft.
const preplanMaxOutputTokens = 32768

// preplanMaxInputAssets bounds how many library files one call may source.
const preplanMaxInputAssets = 4

// preplanCallTimeout bounds one preplan model call. The old 120s deadline was
// calibrated on the bare-model non-streaming path, which averages ~26s and
// hangs under upstream load (live incident 2026-10-03: a 163KB draft hit the
// 120s wall with no upstream bytes). Streaming Fusion-path drafts complete in
// single-digit seconds; 240s only absorbs the slow tail, it is not the fix.
const preplanCallTimeout = 240 * time.Second

// preplanAttemptTimeout is one attempt's slice of preplanCallTimeout. Two
// independent slices (110s + 120s + 2s backoff + slack) keep a byte-less
// first attempt from starving the retry — the shared-budget retry never
// fired in the 21:11 incident because the first attempt burned all 240s.
//
// 2026-10-03 23:01 incident (gateway rows 330011/330012): the deadline itself
// became the killer — the upstream STREAMED continuously (581-726KB of SSE in
// 110s, ~22k tokens of output needs 110-130s for this input size) and both
// attempts were cut at exactly 110s while still producing bytes. A wall-clock
// deadline cannot distinguish "slow but alive" from "hung", so it now only
// arms the OVERALL budget (preplanCallTimeout); liveness is enforced per-read
// by preplanStallTimeout below.
const preplanAttemptTimeout = 110 * time.Second

// preplanStallTimeout is the no-progress watchdog: if no SSE byte arrives for
// this long, the attempt is declared hung and aborted (retrying within the
// overall budget). A healthy stream for a 163KB requirement emits deltas
// continuously for 110-130s (rows 330011/330012: 5-7KB/s of SSE while alive);
// a truly hung upstream emits zero bytes (rows 329996/330003). 120s of
// silence is far beyond any observed healthy gap and still leaves 110s+ for
// a retry inside the 240s budget.
const preplanStallTimeout = 120 * time.Second

type deliveryPreplanRequest struct {
	// Source selects where the requirement text comes from.
	Source string   `json:"source"` // "text" | "assets"
	Text   string   `json:"text,omitempty"`
	FileIDs []string `json:"fileIds,omitempty"`
	// Agent optionally names the project agent whose model routing backs the
	// preplan call. Agents bound to a Fusion/* runtime model ride the
	// gateway's streaming virtual-model path (2026-10-03: 878/878 ok, ~7s avg)
	// instead of the bare-model non-streaming path (17/61 failed, ~26s avg,
	// hangs past the deadline under upstream load).
	Agent string `json:"agent,omitempty"`
}

type deliveryPreplanResponse struct {
	ScaleHint string `json:"scaleHint"` // "linear" | "batched"
	Reason    string `json:"reason,omitempty"`
	// Plan is a DRAFT (never persisted by this endpoint). It may fail deep
	// validation; the plan editor fixes it up before task creation.
	Plan *workflowstore.DeliveryPlan `json:"plan,omitempty"`
	// PlanError carries the parse/validation failure so the UI can offer
	// manual editing instead of a dead end.
	PlanError string `json:"planError,omitempty"`
	Model     string `json:"model,omitempty"`
}

// handleDeliveryPreplan serves POST /api/v1/projects/{name}/delivery/preplan.
// Read-natured (nothing persisted), so project access suffices; the LLM call
// is rate-limited per project via allowDesignRequest.
func (s *Server) handleDeliveryPreplan(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	var body deliveryPreplanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, preplanMaxInputBytes+(1<<20))).Decode(&body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	source := strings.ToLower(strings.TrimSpace(body.Source))
	if source == "" {
		if strings.TrimSpace(body.Text) != "" {
			source = "text"
		} else if len(body.FileIDs) > 0 {
			source = "assets"
		}
	}
	if source != "text" && source != "assets" {
		s.jsonError(w, http.StatusBadRequest, "source must be \"text\" or \"assets\"")
		return
	}

	requirement, err := s.preplanRequirementText(project, source, body)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(requirement) == "" {
		s.jsonError(w, http.StatusBadRequest, "requirement text is empty")
		return
	}

	if !s.allowDesignRequest("preplan:"+project, true) {
		s.jsonError(w, http.StatusTooManyRequests, "preplan rate limit exceeded, retry later")
		return
	}

	agent := strings.TrimSpace(body.Agent)
	if agent == "" {
		agent = s.deployAIProjectAgent(project)
	}
	if agent == "" {
		s.jsonError(w, http.StatusConflict, "project has no agent to run the preplan model")
		return
	}
	// Model priority: the named agent's runtime model (rides the same
	// Fusion/* fast path its runs use) > deployAIProjectAgentModel (first
	// agent with a runtime model) > deployAIDefaultModel (bare glm name).
	preplanModel := deployAIDefaultModel
	if meta := s.designAgentMeta(project, agent); meta != nil && strings.TrimSpace(meta.RuntimeModel) != "" {
		preplanModel = strings.TrimSpace(meta.RuntimeModel)
	} else if m := s.deployAIProjectAgentModel(project); m != "" {
		preplanModel = m
	}
	creds, err := s.resolveDesignModelProvider(project, agent, preplanModel)
	if err != nil || creds == nil || creds.BaseURL == "" || creds.APIKey == "" {
		s.jsonError(w, http.StatusConflict, "model provider unresolved for project agent")
		return
	}

	provider := entity.APIProvider{
		Type:    creds.Protocol,
		BaseURL: creds.BaseURL,
		APIKey:  creds.APIKey,
		Model:   creds.Model,
	}
	gctx, cancel := context.WithTimeout(r.Context(), preplanCallTimeout)
	defer cancel()
	// Two INDEPENDENT attempt deadlines inside the 240s budget. Live incident
	// 21:11 (gateway row 330003): the first attempt hung byte-less for the
	// full 240s, so the shared-budget retry never got its turn — a hung
	// upstream must burn only its own slice, leaving room for a fresh
	// connection. The 12:57 companion row (330000) proves the same 163KB
	// input CAN complete given a healthy upstream (238s, output hit the old
	// 16k ceiling). Only hung/empty streams retry; auth/parse failures would
	// fail identically.
	//
	// 2026-10-03 23:01 (rows 330011/330012) taught the second half: a
	// wall-clock attempt deadline also kills SLOW-BUT-ALIVE streams (both
	// attempts cut at 110s while the upstream was actively emitting 5-7KB/s).
	// So the per-attempt ctx now carries only the OVERALL budget; liveness is
	// enforced inside the stream loop by the preplanStallTimeout watchdog —
	// any SSE byte resets it, silence for 120s aborts the attempt.
	var reply string
	var callErr error
	for attempt := 1; attempt <= 2; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(gctx, preplanCallTimeout)
		reply, callErr = preplanAnthropicChatStreaming(attemptCtx, provider,
			buildDeliveryPreplanPrompt(), requirement)
		attemptCancel()
		if callErr == nil {
			err = nil
			break
		}
		if attempt == 1 && gctx.Err() == nil &&
			(strings.Contains(callErr.Error(), "读取模型响应失败") ||
				strings.Contains(callErr.Error(), "模型服务没有返回内容") ||
				strings.Contains(callErr.Error(), "流停滞")) {
			select {
			case <-time.After(2 * time.Second):
			case <-gctx.Done():
			}
			continue
		}
		err = callErr
		break
	}
	if err != nil {
		s.jsonError(w, http.StatusBadGateway, "preplan model call failed: "+truncateDeployVerify(err.Error(), 200))
		return
	}

	resp := parseDeliveryPreplanReply(reply)
	resp.Model = creds.Model
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// preplanAnthropicChatStreaming is the preplan call transport. It forces
// stream:true and accumulates the SSE deltas: live incident 2026-10-03 showed
// the same gateway model completes in ~5s streamed vs 26-64s (or an
// indefinite hang) non-streamed — the upstream buffers the whole completion
// before answering a stream:false request, while streamed bytes flow as they
// are produced. Also strips any Expect header: the ccr gateway's undici fetch
// rejects Expect: 100-continue with UND_ERR_NOT_SUPPORTED (502) at any body
// size. Mirrors deployAIAnthropicChat's request shape otherwise.
func preplanAnthropicChatStreaming(ctx context.Context, provider entity.APIProvider, systemPrompt, message string) (string, error) {
	model := strings.TrimSpace(provider.Model)
	if model == "" {
		return "", fmt.Errorf("preplan 模型账号缺少默认模型")
	}
	apiKey := strings.TrimSpace(provider.APIKey)
	if apiKey == "" {
		return "", fmt.Errorf("preplan 模型账号缺少 API Key")
	}
	endpoint := assistantAnthropicEndpoint(provider.BaseURL)
	reqBody := map[string]any{
		"model":      model,
		"system":     systemPrompt,
		"messages":   []assistantLLMMessage{{Role: "user", Content: message}},
		"max_tokens": preplanMaxOutputTokens,
		"stream":     true,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("构造模型请求失败：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("构造模型请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	// The gateway re-sends upstream headers verbatim via undici, which does
	// not support Expect; drop it defensively regardless of client default.
	req.Header.Del("Expect")

	// Stall watchdog: scanner.Scan() blocks with no way to bound a silent
	// connection via the request deadline alone. The request rides stallCtx
	// (a child of the attempt ctx, so the deadline still cascades), and a
	// timer re-armed on every upstream byte aborts the read when the stream
	// goes quiet for preplanStallTimeout — live rows 330011/330012 show a
	// healthy 163KB stream emits bytes continuously (5-7KB/s) while a hung
	// one emits zero (rows 329996/330003), so "no byte for 120s" separates
	// the two failure shapes cleanly.
	stallCtx, stallCancel := context.WithCancel(ctx)
	defer stallCancel()
	stallTimer := time.AfterFunc(preplanStallTimeout, stallCancel)
	defer stallTimer.Stop()
	req = req.WithContext(stallCtx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求模型服务失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("模型服务返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}

	var text strings.Builder
	var deltas int
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Any byte from upstream proves liveness; re-arm the watchdog.
		stallTimer.Reset(preplanStallTimeout)
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		deltas++
		if event.Error != nil {
			return "", fmt.Errorf("模型服务错误：%s", event.Error.Message)
		}
		if event.Type == "content_block_delta" && event.Delta != nil && event.Delta.Type == "text_delta" {
			text.WriteString(event.Delta.Text)
		}
	}
	if err := scanner.Err(); err != nil {
		// Distinguish watchdog aborts (context.Canceled from stallCancel)
		// from ordinary read failures — the retry gate matches on wording.
		if stallCtx.Err() != nil && ctx.Err() == nil {
			return "", fmt.Errorf("流停滞：上游 %s 内没有任何字节（已收到 %d 个事件）", preplanStallTimeout, deltas)
		}
		return "", fmt.Errorf("读取模型响应失败：%w", err)
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", fmt.Errorf("模型服务没有返回内容（流式事件 %d 个）", deltas)
	}
	return out, nil
}

// preplanRequirementText assembles the requirement payload. For assets it
// reads the blob content server-side (access was already checked above);
// oversized assets are truncated, never silently dropped.
func (s *Server) preplanRequirementText(project, source string, body deliveryPreplanRequest) (string, error) {
	if source == "text" {
		text := strings.TrimSpace(body.Text)
		if len(text) > preplanMaxInputBytes {
			text = text[:preplanMaxInputBytes]
		}
		return text, nil
	}
	ids := body.FileIDs
	if len(ids) == 0 {
		return "", fmt.Errorf("fileIds required for source=assets")
	}
	if len(ids) > preplanMaxInputAssets {
		ids = ids[:preplanMaxInputAssets]
	}
	bs, err := assets.NewBlobStore()
	if err != nil {
		return "", fmt.Errorf("asset store unavailable")
	}
	var b strings.Builder
	for _, id := range ids {
		f, ok, ferr := s.controlDB.AssetFile(strings.TrimSpace(id))
		if ferr != nil || !ok || f.ProjectID != project {
			return "", fmt.Errorf("asset %s not found in project", id)
		}
		if f.ArchivedAt != "" {
			return "", fmt.Errorf("asset %s is archived", id)
		}
		if berr := bs.Verify(f.CurrentSha); berr != nil {
			return "", fmt.Errorf("asset %s content verification failed", id)
		}
		data, rerr := os.ReadFile(bs.Path(f.CurrentSha))
		if rerr != nil {
			return "", fmt.Errorf("asset %s read failed", id)
		}
		if len(data) > preplanMaxInputBytes {
			data = data[:preplanMaxInputBytes]
		}
		b.WriteString("---\n# " + f.DisplayName + "\n")
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// buildDeliveryPreplanPrompt is the system prompt for the single preplan
// call. Output contract: one JSON object with scaleHint + a DeliveryPlan
// draft matching the frozen-plan schema (workflow.DeliveryPlan).
func buildDeliveryPreplanPrompt() string {
	return `你是大需求交付的预拆分助手。用户会给你一份需求文本（可能是一份数千行的军标式 SRS）。你的任务：产出一份**交付计划草案**（draft），供操作者在任务创建前编辑——你不是裁决者，正式裁决由流程内的 scale_gate 步骤做出，人审在 contract_review 完成。

输出**恰好一个 JSON 对象**（可以带 json 围栏），schema 如下：
{
  "scaleHint": "linear" | "batched",
  "reason": "一句话判定理由（内聚域/规模依据）",
  "plan": {
    "schemaVersion": 1,
    "planId": "preplan-<小写短横线slug>",
    "version": 1,
    "requirementItems": [{"id": "req-1", "text": "需求锚点原文摘要", "source": "章节号或原文位置"}],
    "sharedContract": [{"id": "contract-1", "artifact": "DDL/错误码/API骨架/技术栈骨架", "path": "预计落库路径（可空）"}],
    "workPackages": [{
      "id": "wp-1",
      "title": "工作包标题",
      "domain": "内聚域声明（为什么这些活放在一起）",
      "details": "给实现者的补充说明（可含验收标准骨架）",
      "dependsOn": ["wp-0"],
      "acceptanceCriteria": ["req-1"],
      "agentBinding": "",
      "expectedDelivery": ["api/", "db/"],
      "contractRefs": ["contract-1"]
    }],
    "integrationPolicy": "接缝集成策略（一句话）",
    "qaPolicy": "QA 策略（一句话）"
  }
}

硬规则：
- scaleHint=linear 时 plan 可以为 null，但 reason 必须说明为何不需要拆分。
- batched 时 workPackages ≥2；每个包必须满足：id 仅小写字母数字/-/_；acceptanceCriteria 非空，每项引用 requirementItems 的 id，或以 "infra:" 前缀标记基础设施包；dependsOn 只引用存在的包 id 且不得自引用或成环。
- agentBinding 留空字符串——由操作者在编辑器中选择具体 agent。
- 契约先行（contract-first）：所有跨包共享的 DDL/错误码/API 骨架必须列为 sharedContract 条目，并在首个工作包（通常 wp-0/w-1）中实现，其余包通过 contractRefs 依赖。
- requirementItems 从需求原文逐条提取（id 用 req-N），是后续每个包验收标准的锚点；不要编造原文没有的需求。
- 只输出 JSON，不要输出其他解释文字。`
}

// parseDeliveryPreplanReply tolerates a ```json fence, then parses. Deep
// validation runs but its failure is reported (PlanError) instead of 500 —
// the draft is editable, and the operator may fix what the model got wrong.
func parseDeliveryPreplanReply(raw string) *deliveryPreplanResponse {
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "{"); idx > 0 {
		if end := strings.LastIndex(raw, "}"); end > idx {
			raw = raw[idx : end+1]
		}
	}
	var out struct {
		ScaleHint string                      `json:"scaleHint"`
		Reason    string                      `json:"reason"`
		Plan      *workflowstore.DeliveryPlan `json:"plan"`
	}
	resp := &deliveryPreplanResponse{ScaleHint: "linear"}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// A draft truncated mid-JSON means the model hit the output ceiling
		// (live incident: 163KB requirement, 16384-token cap). Say that
		// instead of leaking a bare parser error.
		msg := "preplan json: " + truncateDeployVerify(err.Error(), 200)
		if strings.Contains(err.Error(), "unexpected end of JSON input") {
			msg = "preplan 输出在 JSON 完成前被截断（模型达到输出 token 上限）。请精简需求原文后重试，或将需求分批提交拆分"
		}
		resp.PlanError = msg
		return resp
	}
	resp.ScaleHint = strings.ToLower(strings.TrimSpace(out.ScaleHint))
	if resp.ScaleHint != "linear" && resp.ScaleHint != "batched" {
		resp.ScaleHint = "linear"
	}
	resp.Reason = strings.TrimSpace(out.Reason)
	if out.Plan == nil {
		return resp
	}
	plan := *out.Plan
	// Normalize the identity fields the draft may have missed; keep the
	// editor in charge of agent bindings (the model is told to leave them
	// empty, but strip whatever it invented anyway).
	plan.SchemaVersion = workflowstore.DeliveryPlanSchemaVersion
	if strings.TrimSpace(plan.PlanID) == "" {
		plan.PlanID = "preplan-draft"
	}
	if plan.Version < 1 {
		plan.Version = 1
	}
	for i := range plan.WorkPackages {
		plan.WorkPackages[i].AgentBinding = ""
		// ValidateDeliveryPlan refuses a package with neither agentBinding
		// nor actorRole (a materialization that cannot resolve an actor
		// deadlocks the stage). Drafts leave the agent choice to the plan
		// editor, so carry the role placeholder to stay internally valid;
		// the editor replaces it with a concrete binding before the plan is
		// ever frozen.
		if strings.TrimSpace(plan.WorkPackages[i].ActorRole) == "" {
			plan.WorkPackages[i].ActorRole = "developer-agent"
		}
	}
	plan.RunID = ""
	plan.ResolvedAtBaseCommit = ""
	if err := workflowstore.ValidateDeliveryPlan(plan); err != nil {
		resp.Plan = &plan
		resp.PlanError = truncateDeployVerify(err.Error(), 300)
		return resp
	}
	resp.Plan = &plan
	return resp
}
