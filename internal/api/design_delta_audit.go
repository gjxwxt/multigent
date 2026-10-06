package api

// Design-gate delta audit (docs/design-gate-delta-audit-proposal.md, v2.6):
// when the human presses "confirm and flow" on the design gate, the platform
// fetches the OD project's HTML once, runs a model audit against the approved
// requirement (plus differential context from a previous round), and returns
// the difference report for a human adjudication modal. Nothing is written:
// the fetch is read-only, the adjudication + freeze happen in the confirm
// submit (POST workflow/review), which freezes THE AUDIT FETCH's HTML, not a
// re-fetch (same-source freeze; sha256 checked server-side).
//
// Contract red lines (from the proposal):
//   - First-hit zero-write: POST design/audit-delta performs zero DB and zero
//     file writes. Idempotent: re-clicking re-runs the audit from scratch.
//   - Same-source freeze: the audit response carries the fetched HTML + its
//     sha256; the confirm submit must present them back and the server
//     verifies content-hash equality before persisting. Re-fetching at
//     confirm would freeze a different document than the one adjudicated
//     (the OD agent may still be writing between the two moments) and would
//     invalidate every adjudicated element fingerprint.
//   - Benchmark B (OD conversation history) is conditionally available: the
//     odClient has no conversation-read API, so source classification
//     degrades to "uncertain" until an OD read endpoint exists. The audit
//     therefore never claims a source it cannot see and hides the default
//     suggestion when the intent signal is unknown.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

const (
	// designDeltaAuditTimeout bounds the whole first-hit fetch+audit pass.
	designDeltaAuditTimeout = 120 * time.Second
	// designDeltaMaxOutputTokens keeps the audit on the OD short-conversation
	// red line (same cap as the deploy drawer): structured JSON out, never a
	// long thinking chain.
	designDeltaMaxOutputTokens = 16384
	// designDeltaHTMLCap caps the HTML fed to the audit model. Prototypes are
	// single-page artifacts well under this; larger ones get truncated with
	// an explicit notice in the prompt rather than a silent overflow.
	designDeltaHTMLCap = 300 << 10
	// designDeltaMaxDeltas bounds the parsed delta list; an audit answering
	// with more entries than this is treated as a parse failure (fail-closed
	// to the escape hatch rather than rendering a runaway list).
	designDeltaMaxDeltas = 40
	// designDeltaSkipHintThreshold is the adjudication-modal hint threshold:
	// at or above this many skipped items the modal shows the "these will be
	// re-checked at self review" notice (§5.4 skipped observability).
	designDeltaSkipHintThreshold = 5
)

// designDeltaDirection is the delta taxonomy of §5.2. Divergent deltas are
// READ-ONLY in phase 1 (adjudication interaction ships in phase 2); the model
// may still emit them so the human at least sees the inconsistency.
type designDeltaDirection string

const (
	deltaExtra   designDeltaDirection = "prototype_extra"
	deltaMissing designDeltaDirection = "prototype_missing"
	deltaDiverge designDeltaDirection = "both_diverge"
)

// designDeltaItem is one difference entry (§5.2 schema, server-side shape).
type designDeltaItem struct {
	ID                 string           `json:"delta_id"`
	Direction          string           `json:"direction"`
	RequirementSide    *designDeltaSide `json:"requirement_side,omitempty"`
	PrototypeSide      *designDeltaSide `json:"prototype_side,omitempty"`
	DivergeFields      string           `json:"diverge_fields,omitempty"`
	IntentSignal       string           `json:"intent_signal,omitempty"`
	SourceClass        string           `json:"source_class"`
	SourceEvidence     string           `json:"source_evidence,omitempty"`
	SuggestedAction    string           `json:"suggested_action"`
	SuggestedSpec      string           `json:"suggested_spec,omitempty"`
	Confidence         string           `json:"confidence,omitempty"`
	ElementFingerprint string           `json:"element_fingerprint,omitempty"`
}

type designDeltaSide struct {
	Present bool   `json:"present"`
	Reading string `json:"reading"`
	Ref     string `json:"ref,omitempty"`
}

// designDeltaDecision is one human adjudication recorded at confirm time.
type designDeltaDecision struct {
	ID     string `json:"delta_id"`
	Action string `json:"action"` // adopt | drop | skip | rework | waive | note
	Note   string `json:"note,omitempty"`
	// both_diverge pick-side verdict: requirement | prototype | other.
	Side string `json:"side,omitempty"`
	// Fresh authoritative description when side === "other" (neither side
	// taken as-is) — implementation builds against this.
	SideNote string `json:"sideNote,omitempty"`
}

// designDeltaAuditResponse is the first-hit payload: the report, the frozen
// candidate (HTML+hash) for same-source confirmation, and the differential
// context the adjudication modal renders for prior-round continuity.
type designDeltaAuditResponse struct {
	HTML          string                `json:"html"`
	HTMLSHA256    string                `json:"htmlSha256"`
	FetchedAt     string                `json:"fetchedAt"`
	Report        []designDeltaItem     `json:"deltas"`
	SkippedHint   bool                  `json:"skippedHint"`
	PrevDecisions []designDeltaDecision `json:"prevDecisions,omitempty"`
	Rounds        int                   `json:"auditRounds"`
}

// designDeltaAuditRequest is the first-hit body. prevDecisions replays the
// prior adjudications for differential re-audit (adopted/dropped elements are
// matched by fingerprint and auto-carried, never re-adjudicated). hints are
// the human's re-audit reminders: aspects they feel the audit missed. They
// steer WHERE the audit looks next round — never WHAT counts as a difference.
type designDeltaAuditRequest struct {
	Model         string                `json:"model,omitempty"`
	PrevDecisions []designDeltaDecision `json:"prevDecisions,omitempty"`
	PrevHTML      string                `json:"prevHtml,omitempty"`
	Hints         []string              `json:"hints,omitempty"`
}

// ---- first-hit endpoint: POST .../design/audit-delta ----

func (s *Server) handleDesignAuditDelta(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	if task.DesignProjectID == "" {
		s.jsonError(w, http.StatusConflict, "no design project for this task; start one first")
		return
	}
	// Write-shape rate limit: a model call per hit, budgeted like design chat.
	if !s.allowDesignRequest(taskID, true) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "too many audit requests, slow down")
		return
	}
	var body designDeltaAuditRequest
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid request body")
		return
	}

	// Requirement text: strict designGateRequirement (approved_requirement →
	// requirement_draft) so the audit基准 matches what the designer saw.
	requirement, err := s.designGateRequirement(project, taskID, task)
	if err != nil {
		s.jsonError(w, http.StatusConflict, err.Error())
		return
	}

	client := s.defaultODClient()
	ctx, cancel := context.WithTimeout(r.Context(), designDeltaAuditTimeout)
	defer cancel()

	files, err := client.ListProjectFiles(ctx, task.DesignProjectID)
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	artifacts := designSnapshotArtifacts(files)
	if len(artifacts) == 0 {
		s.jsonError(w, http.StatusConflict, "no html artifacts in OD project "+task.DesignProjectID)
		return
	}
	entry := artifacts[0]
	raw, err := client.GetProjectFile(ctx, task.DesignProjectID, entry.Path)
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	html := string(raw)
	sum := sha256.Sum256(raw)
	fetchedAt := time.Now().UTC()

	// Benchmark B is LIVE (probe 2026-10-04): OD answers conversation
	// read-back, so human turns feed source classification. Any failure
	// degrades to uncertain (fail-closed, never blocks the audit).
	humanTurns := s.fetchDesignHumanTurns(ctx, client, task.DesignProjectID)

	// Differential context: prior adjudications narrow this round's list.
	prev := designDeltaPrevContextFrom(body)

	audit, err := s.runDesignDeltaAudit(ctx, project, task, requirement, html, humanTurns, prev)
	if err != nil {
		log.Printf("[design-delta] %s/%s: audit failed: %v", project, taskID, err)
		s.writeDesignUpstreamError(w, err)
		return
	}

	skipped := 0
	for _, d := range audit.Deltas {
		if strings.EqualFold(d.SuggestedAction, "skip") {
			skipped++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(designDeltaAuditResponse{
		HTML:          html,
		HTMLSHA256:    hex.EncodeToString(sum[:]),
		FetchedAt:     fetchedAt.Format(time.RFC3339),
		Report:        audit.Deltas,
		SkippedHint:   skipped >= designDeltaSkipHintThreshold,
		PrevDecisions: body.PrevDecisions,
		Rounds:        prev.rounds,
	})
}

// designDeltaPrevContext normalizes the differential payload carried on the
// request: prior decisions grouped into carried items (adopt/drop/waive with
// fingerprints), the human's re-audit reminders, and the round counter.
type designDeltaPrevContext struct {
	rounds    int
	decisions []designDeltaDecision
	hints     []string
}

func designDeltaPrevContextFrom(body designDeltaAuditRequest) designDeltaPrevContext {
	pc := designDeltaPrevContext{decisions: []designDeltaDecision{}}
	for _, d := range body.PrevDecisions {
		action := strings.ToLower(strings.TrimSpace(d.Action))
		if action == "" {
			continue
		}
		pc.decisions = append(pc.decisions, designDeltaDecision{
			ID:       strings.TrimSpace(d.ID),
			Action:   action,
			Note:     strings.TrimSpace(d.Note),
			Side:     strings.TrimSpace(d.Side),
			SideNote: strings.TrimSpace(d.SideNote),
		})
	}
	for _, h := range body.Hints {
		if h = strings.TrimSpace(h); h != "" {
			pc.hints = append(pc.hints, h)
		}
	}
	// Round counter: 1 on a fresh audit; 2+ when prior adjudications are
	// replayed (the differential re-audit is a new round) or the human sent
	// re-audit reminders — an explicit re-audit ask is itself a new round.
	pc.rounds = 1
	if strings.TrimSpace(body.PrevHTML) != "" || len(pc.decisions) > 0 || len(pc.hints) > 0 {
		pc.rounds = 2
	}
	return pc
}

// ---- model audit ----

type designDeltaAuditResult struct {
	Deltas []designDeltaItem
}

// designDeltaTurn is one human turn from the OD conversation read-back.
type designDeltaTurn struct {
	Seq     int
	Content string
}

// fetchDesignHumanTurns reads the OD conversation history and returns the
// HUMAN turns (role=user), excluding the platform-injected gate prompt (the
// first user message — it carries the 【角色与使命】 design brief, not user
// intent). Benchmark B live path: OD answers
// GET /api/projects/{id}/conversations and .../{convId}/messages (probed
// 2026-10-04, OD v0.21.1). Any error returns nil — the audit degrades to
// uncertain sources, never blocks.
func (s *Server) fetchDesignHumanTurns(ctx context.Context, client odClientAPI, projectID string) []designDeltaTurn {
	convs, err := client.ListProjectConversations(ctx, projectID)
	if err != nil || len(convs) == 0 {
		return nil
	}
	// The design session is the conversation carrying the gate prompt: pick
	// the most recently updated conversation in design mode (deterministic).
	best := convs[0]
	for _, c := range convs {
		if c.UpdatedAt > best.UpdatedAt {
			best = c
		}
	}
	msgs, err := client.GetConversationMessages(ctx, projectID, best.ID)
	if err != nil {
		return nil
	}
	turns := make([]designDeltaTurn, 0, len(msgs))
	seq := 0
	for _, m := range msgs {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		seq++
		if seq == 1 && strings.Contains(content, "【角色与使命】") {
			// Platform-injected design brief, not user intent.
			seq--
			continue
		}
		// Cap each turn and the turn list so a chatty session cannot balloon
		// the audit prompt.
		if len(content) > 2000 {
			content = content[:2000]
		}
		turns = append(turns, designDeltaTurn{Seq: len(turns) + 1, Content: content})
		if len(turns) >= 20 {
			break
		}
	}
	return turns
}

// runDesignDeltaAudit performs one model call. It reads only: requirement
// text, fetched HTML, OD conversation human turns, and client-supplied
// differential context.
func (s *Server) runDesignDeltaAudit(ctx context.Context, project string, task *entity.Task, requirement, html string, humanTurns []designDeltaTurn, prev designDeltaPrevContext) (*designDeltaAuditResult, error) {
	agent := taskAgentFromAssignee(task)
	if agent == "" || s.designAgentMeta(project, agent) == nil {
		// The assignee may be a human user (task parked at the gate under a
		// user's name); the audit rides the project's first agent model
		// account instead, same resolution as the deploy drawer.
		agent = s.deployAIProjectAgent(project)
	}
	model := odDefaultModel
	var creds *designModelCreds
	if agent != "" {
		// Configured layers first (agent runtime model / provider default);
		// odDefaultModel only when the project has no configured account.
		model = s.designModelOrDefault(project, agent, "")
		resolved, err := s.resolveDesignModelProvider(project, agent, model)
		if err != nil {
			return nil, err
		}
		creds = resolved
		if resolved.Model != "" {
			model = resolved.Model
		}
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" {
		return nil, fmt.Errorf("no model provider resolvable for audit; bind a model account first")
	}

	prompt := designDeltaAuditPrompt(requirement, html, humanTurns, prev)
	provider := entity.APIProvider{
		Type:    creds.Protocol,
		BaseURL: creds.BaseURL,
		APIKey:  creds.APIKey,
		Model:   model,
	}
	reply, err := invokeDeployAIProviderChat(ctx, provider, prompt, nil, designDeltaUserInstruction(html))
	if err != nil {
		return nil, err
	}
	deltas, err := parseDesignDeltaReport(reply, len(humanTurns) > 0)
	if err != nil {
		return nil, err
	}
	return &designDeltaAuditResult{Deltas: deltas}, nil
}

// designDeltaAuditPrompt builds the audit system prompt. Hard boundaries:
// output JSON only, three-direction taxonomy, no invented evidence. The
// requirement is the authoritative baseline; when the OD conversation
// read-back yielded human turns, source classification may use them
// (user_requested vs agent_initiated) and the intent of a turn beats guesswork;
// without turns the source_class stays uncertain (benchmark B degraded).
func designDeltaAuditPrompt(requirement, html string, humanTurns []designDeltaTurn, prev designDeltaPrevContext) string {
	var b strings.Builder
	b.WriteString("你是交付流水线设计闸门的差异审计员。给你两份输入：经过确认的需求规格，以及设计产出的 HTML 原型。你的唯一职责：找出原型与需求之间的差异，输出结构化 JSON。\n\n")
	b.WriteString("【硬性边界】\n")
	b.WriteString("1. 只输出一个 JSON 数组，不要任何其他文字、注释或代码块标记。\n")
	b.WriteString("2. 需求规格是唯一权威基准。source_class 标注差异元素的来源：若下方【设计会话中人的输入】能证明某元素是人明确提出的要求 → user_requested（并在 source_evidence 引用「输入 #N」原文片段）；无会话输入支持、也不能确定是 Agent 发挥 → uncertain。严禁编造证据。user_requested 且与需求不冲突的元素，建议从宽（suggested_action 倾向 adopt_into_requirement）。\n")
	b.WriteString("3. 差异三分类：prototype_extra（原型有、需求无）、prototype_missing（需求要求、原型未体现）、both_diverge（双侧都有但描述/参数不一致）。参数级对照（数量、阈值、交互方式、状态枚举）发现不一致才算 both_diverge，措辞风格差异不算。\n")
	b.WriteString("4. 判定以行为/功能为准：元素存在但行为不符合需求 → both_diverge；纯文案/样式漂移不算差异。\n")
	b.WriteString("5. 宁缺毋滥：每条差异必须有具体可指认的元素或缺失点。没有差异时输出空数组 []。\n\n")
	b.WriteString("【输出 schema（数组每个元素）】\n")
	b.WriteString(`{"delta_id":"d1","direction":"prototype_extra|prototype_missing|both_diverge","requirement_side":{"present":bool,"reading":"需求侧理解一句话","ref":"需求章节引用"},"prototype_side":{"present":bool,"reading":"原型侧理解一句话","ref":"CSS 选择器或页面区域"},"diverge_fields":"仅 both_diverge：不一致的参数维度","source_class":"uncertain","suggested_action":"adopt_into_requirement|drop_from_prototype|skip|rework_design|waive_this_round|pick_requirement_side|pick_prototype_side|specify_fresh","suggested_spec":"若建议纳入：一句话行为描述草稿","confidence":"high|medium|low","element_fingerprint":"原型侧元素的稳定指纹：可见文本+稳定选择器，如 '配额趋势图|aside .quota-panel'"}`)
	b.WriteString("\n\n")
	b.WriteString("【suggested_action 对应关系】prototype_extra → adopt_into_requirement（看起来是有价值的功能）或 drop_from_prototype（纯演示性内容）；prototype_missing → rework_design（核心流程缺失）或 waive_this_round（边缘项）；both_diverge → pick_requirement_side（需求描述更完整可信）、pick_prototype_side（原型表现更接近用户意图）或 specify_fresh（两侧都不完整，附 suggested_spec 草拟应有行为）。人会在裁决弹窗做最终选边，你的建议只是预选。\n\n")
	if len(prev.hints) > 0 {
		b.WriteString("【人的重审提醒】以下是人在上一轮报告后补充的关注点——他们感觉审计可能漏掉了这些方面。本轮必须逐条重点核查并在结论中体现：\n")
		b.WriteString("- 提醒涉及且确有差异的方面 → 正常输出差异条目（三分类不变）；\n")
		b.WriteString("- 提醒涉及但核对后确无差异的方面 → 不要编造条目；审计报告本身不含说明文字，无差异即不输出；\n")
		b.WriteString("- 提醒只是扩大你的检查范围，不改变判定标准：需求仍是唯一权威基准，宁缺毋滥。\n")
		for i, h := range prev.hints {
			fmt.Fprintf(&b, "提醒 #%d：%s\n", i+1, h)
		}
		b.WriteString("\n")
	}
	if len(prev.decisions) > 0 {
		b.WriteString("【差分复审上下文】以下是上一轮已由人裁决的差异条目。本轮审计中：\n")
		b.WriteString("- 若某条上轮元素在新原型中仍然存在且语义未变，不要重复输出该条（裁决已定，平台会自动沿用）；\n")
		b.WriteString("- 若上轮元素已不存在或语义明显变化，重新输出（delta_id 继续编号）并在 reading 中说明变化；\n")
		b.WriteString("- 上轮裁决「打回补齐」的缺口若已补上，不要输出（视为已闭环）；仍未补上则重新输出。\n")
		b.WriteString("上轮裁决清单（JSON）：\n")
		raw, _ := json.Marshal(prev.decisions)
		b.Write(raw)
		b.WriteString("\n\n")
	}
	if len(humanTurns) > 0 {
		b.WriteString("【设计会话中人的输入】以下是设计会话中用户亲口输入的原文（排除平台注入的设计任务书）。它们是用户真实意图的第一手证据：\n")
		for _, turn := range humanTurns {
			fmt.Fprintf(&b, "输入 #%d：%s\n---\n", turn.Seq, turn.Content)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("【设计会话中人的输入】（不可用：会话回读失败或无用户输入。source_class 一律 uncertain。）\n\n")
	}
	b.WriteString("【需求规格】\n<requirement>\n")
	b.WriteString(strings.TrimSpace(requirement))
	b.WriteString("\n</requirement>\n\n")
	b.WriteString("【HTML 原型】\n<prototype>\n")
	b.WriteString(designDeltaHTMLExcerpt(html))
	b.WriteString("\n</prototype>")
	return b.String()
}

// designDeltaUserInstruction is the user-turn message: the model helpers take
// system+user, the audit keeps all reference material in the system prompt and
// uses the user turn for the task instruction only.
func designDeltaUserInstruction(html string) string {
	truncated := ""
	if len(html) > designDeltaHTMLCap {
		truncated = "（原型较长，已截断——只审计可见部分，缺失判定请谨慎）"
	}
	return "请审计上述原型与需求的差异，直接输出 JSON 数组。" + truncated
}

// designDeltaHTMLExcerpt strips executable script bodies to fit the prompt
// budget. UI-template scripts (type="text/babel" / "text/jsx" — the Babel-
// standalone single-file pattern OD prototypes use) are KEPT verbatim: their
// JSX bodies ARE the page structure and visible copy of every screen behind
// the login, and stripping them blinds the audit (2026-10-05 fu91es incident:
// the whole admin console lived in a text/babel block, the excerpt kept only
// the login CSS, and the audit reported the console as prototype_missing).
// External src-only tags and plain inline JS are removed — that part is
// interaction wiring, not structure.
func designDeltaHTMLExcerpt(html string) string {
	var b strings.Builder
	rest := html
	for {
		start := strings.Index(rest, "<script")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.Index(rest[start:], "</script>")
		if end < 0 {
			b.WriteString(rest)
			break
		}
		openEnd := strings.Index(rest[start:], ">")
		if openEnd < 0 || openEnd > end {
			// Malformed tag; drop the whole block rather than guess.
			rest = rest[start+end+len("</script>"):]
			continue
		}
		openTag := rest[start : start+openEnd+1]
		body := rest[start+openEnd+1 : start+end]
		b.WriteString(rest[:start])
		if isUITemplateScript(openTag) {
			b.WriteString(openTag)
			b.WriteString(body)
			b.WriteString("</script>")
		}
		rest = rest[start+end+len("</script>"):]
	}
	out := b.String()
	if len(out) > designDeltaHTMLCap {
		out = out[:designDeltaHTMLCap]
	}
	return out
}

// isUITemplateScript reports whether a <script …> open tag marks a UI
// template (Babel-standalone JSX) rather than executable wiring.
func isUITemplateScript(openTag string) bool {
	lower := strings.ToLower(openTag)
	if strings.Contains(lower, "src=") {
		return false
	}
	return strings.Contains(lower, `"text/babel"`) || strings.Contains(lower, `"text/jsx"`) ||
		strings.Contains(lower, `'text/babel'`) || strings.Contains(lower, `'text/jsx'`)
}

// parseDesignDeltaReport decodes the model's JSON array, normalizes ids and
// enum values, and fails closed on garbage (the caller turns that into the
// escape-hatch path). A missing array (prose reply) is a failure, not empty.
// hadHumanTurns gates source-class trust: without conversation evidence in
// context, every claimed source degrades to uncertain.
func parseDesignDeltaReport(reply string, hadHumanTurns bool) ([]designDeltaItem, error) {
	text := strings.TrimSpace(reply)
	// Tolerate a fenced block if the model ignored the format rule.
	if i := strings.Index(text, "["); i > 0 && !strings.HasPrefix(text, "[") {
		if j := strings.LastIndex(text, "]"); j > i {
			text = text[i : j+1]
		}
	}
	var deltas []designDeltaItem
	if err := json.Unmarshal([]byte(text), &deltas); err != nil {
		return nil, fmt.Errorf("audit reply is not a JSON delta array")
	}
	if len(deltas) > designDeltaMaxDeltas {
		return nil, fmt.Errorf("audit returned %d deltas, over the cap", len(deltas))
	}
	validDir := map[string]string{
		"prototype_extra": string(deltaExtra), "extra": string(deltaExtra),
		"prototype_missing": string(deltaMissing), "missing": string(deltaMissing),
		"both_diverge": string(deltaDiverge), "diverge": string(deltaDiverge),
	}
	validAction := map[string]bool{
		"adopt_into_requirement": true, "drop_from_prototype": true, "skip": true,
		"rework_design": true, "waive_this_round": true, "pick_requirement_side": true,
		"pick_prototype_side": true, "specify_fresh": true,
	}
	out := make([]designDeltaItem, 0, len(deltas))
	usedIDs := map[string]bool{}
	for i, d := range deltas {
		dir, ok := validDir[strings.ToLower(strings.TrimSpace(d.Direction))]
		if !ok {
			continue
		}
		if !validAction[strings.ToLower(strings.TrimSpace(d.SuggestedAction))] {
			d.SuggestedAction = "skip"
		} else {
			d.SuggestedAction = strings.ToLower(strings.TrimSpace(d.SuggestedAction))
		}
		d.Direction = dir
		// Benchmark B: a claimed user_requested/agent_initiated/wording_drift
		// classification is trusted only when the audit actually had human
		// turns in context; otherwise everything degrades to uncertain.
		claimed := strings.ToLower(strings.TrimSpace(d.SourceClass))
		if !hadHumanTurns {
			claimed = "uncertain"
		}
		switch claimed {
		case "user_requested", "agent_initiated", "wording_drift":
			d.SourceClass = claimed
		default:
			d.SourceClass = "uncertain"
		}
		id := strings.TrimSpace(d.ID)
		if id == "" || usedIDs[id] {
			id = fmt.Sprintf("d%d", i+1)
		}
		usedIDs[id] = true
		d.ID = id
		if d.RequirementSide == nil {
			d.RequirementSide = &designDeltaSide{}
		}
		if d.PrototypeSide == nil {
			d.PrototypeSide = &designDeltaSide{}
		}
		d.RequirementSide.Reading = strings.TrimSpace(d.RequirementSide.Reading)
		d.PrototypeSide.Reading = strings.TrimSpace(d.PrototypeSide.Reading)
		d.SuggestedSpec = strings.TrimSpace(d.SuggestedSpec)
		if d.ElementFingerprint == "" && d.PrototypeSide.Reading != "" {
			d.ElementFingerprint = d.PrototypeSide.Reading
		}
		out = append(out, d)
	}
	// Stable order: extra, diverge, missing — matches the adjudication modal
	// grouping (extras first, diverge read-only, missings last).
	rank := map[string]int{string(deltaExtra): 0, string(deltaDiverge): 1, string(deltaMissing): 2}
	sort.SliceStable(out, func(i, j int) bool {
		return rank[out[i].Direction] < rank[out[j].Direction]
	})
	return out, nil
}

// ---- confirm-side integration ----

// handleDesignRework closes the 续聊式打回 loop (proposal §5.3.1): the human
// flagged missing items as 打回设计补齐 in the adjudication modal, and instead
// of approving, the platform sends ONE completion message back to the SAME OD
// design conversation. The task stays at the design gate; the OD agent amends
// the existing prototype and the next confirm re-runs the audit (prior
// adjudications travel with the modal as differential context).
//
// The platform resolves the conversation and the model creds server-side: the
// frontend holds no conversation id and never sees model credentials. The
// request carries only the rework message text (built client-side from the
// human's actual decisions; the server length-caps and refuses empty).
func (s *Server) handleDesignRework(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	task := s.designTaskGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	if task.DesignProjectID == "" {
		s.jsonError(w, http.StatusConflict, "no design project for this task; start one first")
		return
	}
	// A model call per hit — budgeted like design chat / audit.
	if !s.allowDesignRequest(taskID, true) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "too many rework requests, slow down")
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid request body")
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		s.jsonError(w, http.StatusBadRequest, "message required")
		return
	}
	if len(msg) > 16000 {
		msg = msg[:16000]
	}

	client := s.defaultODClient()
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// Same conversation selection rule as fetchDesignHumanTurns: the most
	// recently updated design-mode conversation carries the gate prompt and
	// every continuation — amending it preserves full context.
	convs, err := client.ListProjectConversations(ctx, task.DesignProjectID)
	if err != nil || len(convs) == 0 {
		s.writeDesignUpstreamError(w, fmt.Errorf("no design conversation to rework on %s: %w", task.DesignProjectID, err))
		return
	}
	best := convs[0]
	for _, c := range convs {
		if c.UpdatedAt > best.UpdatedAt {
			best = c
		}
	}

	// The assignee may be a human user (the task sits at the gate under the
	// reviewer's name, live case fu91es: assignee=admin) — ride the project's
	// first agent model account then, same fallback as the audit itself.
	agent := taskAgentFromAssignee(task)
	if agent == "" || s.designAgentMeta(project, agent) == nil {
		if pa := s.deployAIProjectAgent(project); pa != "" {
			agent = pa
		}
	}
	model := s.designModelOrDefault(project, agent, "")
	creds, err := s.resolveDesignModelProvider(project, agent, model)
	if err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" {
		s.jsonError(w, http.StatusConflict, "no model provider resolvable for design rework; bind a model account first")
		return
	}
	if model == "" {
		model = odDefaultModel
	}

	// Continuation rides the same POST /api/runs surface as the first run;
	// conversationId is what makes OD treat it as a same-session turn.
	payload := map[string]any{
		"projectId":      task.DesignProjectID,
		"agentId":        odAgentID,
		"message":        msg,
		"sessionMode":    "design",
		"conversationId": best.ID,
		"model":          model,
		"byokProvider": odByokProvider{
			Protocol: creds.Protocol,
			APIKey:   creds.APIKey,
			BaseURL:  creds.BaseURL,
			Model:    model,
		},
	}
	var resp struct {
		ConversationID string `json:"conversationId"`
		RunID          string `json:"runId"`
		ID             string `json:"id"`
	}
	if err := s.odProxyJSON(ctx, "POST", "/api/runs", payload, &resp); err != nil {
		s.writeDesignUpstreamError(w, err)
		return
	}
	agentName := agent
	if agentName == "" {
		agentName = "design-gate"
	}
	note := msg
	if len(note) > 300 {
		note = note[:300] + "…"
	}
	s.addComment(task, project, agentName, fmt.Sprintf("design rework sent to OD conversation %s (design review stays open): %s", best.ID, note))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"conversationId": best.ID,
		"runId":          resp.RunID,
	})
}

// odProxyJSON is the non-proxied OD call path used by the rework endpoint: it
// needs the response BODY (run id), not a pass-through stream. Auth injection
// and output redaction ride odDo's shared path via a fresh client.
func (s *Server) odProxyJSON(ctx context.Context, method, path string, body map[string]any, out any) error {
	var c *odClient
	if dc, ok := s.defaultODClient().(*odClient); ok {
		c = dc
	} else {
		c = &odClient{s: s}
	}
	return c.s.odDo(ctx, method, path, body, out)
}

// designDeltaConfirmPayload is the adjudication bundle the frontend attaches
// to the workflow/review outputs at confirm time (same-source freeze).
type designDeltaConfirmPayload struct {
	HTML       string                `json:"html"`
	HTMLSHA256 string                `json:"htmlSha256"`
	FetchedAt  string                `json:"fetchedAt"`
	Deltas     []designDeltaItem     `json:"deltas,omitempty"`
	Decisions  []designDeltaDecision `json:"decisions"`
	// Provenance of the report being confirmed: audit round count and the
	// human's re-audit reminders that shaped it. Kept so a report produced
	// under reminders is auditable as such — the process, not just the last
	// round's output, survives into the frozen record.
	Hints  []string `json:"hints,omitempty"`
	Rounds int      `json:"auditRounds,omitempty"`
}

// ParseDesignDeltaConfirmPayload decodes and validates the client-supplied
// design_delta_decisions output. The HTML and its hash MUST hash-match (the
// client is first-party; the check guards transport damage and stale modal
// state, not adversarial substitution). Returns ("", nil) when absent.
func ParseDesignDeltaConfirmPayload(raw string) (*designDeltaConfirmPayload, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var p designDeltaConfirmPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("design_delta_decisions is not valid JSON: %w", err)
	}
	if strings.TrimSpace(p.HTML) == "" {
		return nil, fmt.Errorf("design_delta_decisions missing html (same-source freeze required)")
	}
	sum := sha256.Sum256([]byte(p.HTML))
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimSpace(p.HTMLSHA256), got) {
		return nil, fmt.Errorf("design_delta_decisions html hash mismatch: audited document and submitted document differ; re-run the audit")
	}
	if len(p.Decisions) == 0 {
		return nil, fmt.Errorf("design_delta_decisions missing decisions")
	}
	for _, d := range p.Decisions {
		switch strings.ToLower(strings.TrimSpace(d.Action)) {
		case "adopt", "drop", "skip", "note":
		default:
			return nil, fmt.Errorf("design_delta_decisions has invalid action %q on %s", d.Action, d.ID)
		}
	}
	for i, h := range p.Hints {
		p.Hints[i] = strings.TrimSpace(h)
		if len(p.Hints[i]) > 4000 {
			p.Hints[i] = p.Hints[i][:4000]
		}
	}
	return &p, nil
}

// persistDesignDeltaOutputs serializes the adjudication bundle into the design
// gate step outputs, next to the snapshot fields. Called from the confirm
// path AFTER captureDesignGateSnapshot bookkeeping — the frozen bundle on disk
// is authoritative for downstream agents; the outputs carry the auditable
// delta record (decisions are the deliverable, so serialization failure is a
// hard error, not best-effort).
func (s *Server) persistDesignDeltaOutputs(outputs map[string]string, payload *designDeltaConfirmPayload) error {
	if payload == nil {
		return nil
	}
	decisionsJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal design delta record: %w", err)
	}
	outputs["design_delta_decisions"] = string(decisionsJSON)
	return nil
}

// auditEntryHash hashes the inline snapshot HTML for the same-source check.
func auditEntryHash(inline string) string {
	sum := sha256.Sum256([]byte(inline))
	return hex.EncodeToString(sum[:])
}

// designDeltaActionSummary renders a compact human-readable action tally for
// the audit comment trail.
func designDeltaActionSummary(decisions []designDeltaDecision) string {
	tally := map[string]int{}
	order := []string{}
	for _, d := range decisions {
		a := strings.ToLower(strings.TrimSpace(d.Action))
		if a == "" {
			continue
		}
		if _, ok := tally[a]; !ok {
			order = append(order, a)
		}
		tally[a]++
	}
	parts := make([]string, 0, len(order))
	for _, a := range order {
		parts = append(parts, fmt.Sprintf("%s×%d", a, tally[a]))
	}
	return strings.Join(parts, ", ")
}
