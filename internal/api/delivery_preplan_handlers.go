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
	"context"
	"encoding/json"
	"fmt"
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

// preplanMaxInputAssets bounds how many library files one call may source.
const preplanMaxInputAssets = 4

type deliveryPreplanRequest struct {
	// Source selects where the requirement text comes from.
	Source string   `json:"source"` // "text" | "assets"
	Text   string   `json:"text,omitempty"`
	FileIDs []string `json:"fileIds,omitempty"`
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

	agent := s.deployAIProjectAgent(project)
	if agent == "" {
		s.jsonError(w, http.StatusConflict, "project has no agent to run the preplan model")
		return
	}
	creds, err := s.resolveDesignModelProvider(project, agent, deployAIDefaultModel)
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
	gctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	reply, err := invokeDeployAIProviderChat(gctx, provider,
		buildDeliveryPreplanPrompt(), nil, requirement)
	if err != nil {
		s.jsonError(w, http.StatusBadGateway, "preplan model call failed: "+truncateDeployVerify(err.Error(), 200))
		return
	}

	resp := parseDeliveryPreplanReply(reply)
	resp.Model = creds.Model
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
		resp.PlanError = "preplan json: " + truncateDeployVerify(err.Error(), 200)
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
