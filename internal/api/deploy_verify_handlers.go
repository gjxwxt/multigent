package api

// Deploy smoke verification (deploy center phase 3a, M5 deterministic). After
// a deploy succeeds, the commit span is turned into a smoke-check spec
// (method/path/expected status/contains) by the project agent's model, then
// executed BY THE PLATFORM as plain HTTP — no browser, no agent runtime in
// the execution path. The spec is persisted alongside the deploy_request
// (Q7 ruling: replayable regression asset); failures auto-post an issue task.
//
// Host red line: every request targets the SAME host the health probe uses
// (MULTIGENT_DEPLOY_HOST + the project's deploy port). A spec entry may not
// override the host — paths only — so model output can never steer the
// verifier at arbitrary network targets.
//
// Storage: kv_records (table deploy_verification, k1=requestID) — the
// verification state is request-scoped data, and the deploy_requests schema
// stays untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

const (
	// deployVerifyKVTable is the kv_records shard for per-request verification
	// state.
	deployVerifyKVTable = "deploy_verification"
	// deployVerifyHTTPTimeout bounds one smoke request.
	deployVerifyHTTPTimeout = 5 * time.Second
	// deployVerifySpecMax caps how many checks one spec may carry.
	deployVerifySpecMax = 12
	// deployVerifyMaxBodyBytes bounds the request body a check may carry.
	deployVerifyMaxBodyBytes = 4 << 10
	// deployVerifyCreatedBy stamps auto-posted failure tasks.
	deployVerifyCreatedBy = "deploy-verify"
	// deployVerifyModelMaxTokens mirrors the OD short-conversation budget.
	deployVerifyModelMaxTokens = 16384
)

// deployVerifyCheck is one deterministic smoke check.
type deployVerifyCheck struct {
	Name           string            `json:"name"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	ExpectedStatus int               `json:"expectedStatus"`
	// Headers are extra request headers; Host/Authorization are stripped
	// (host red line / credential red line).
	Headers map[string]string `json:"headers,omitempty"`
	// Body is a small JSON/string body for POST/PUT/PATCH checks.
	Body string `json:"body,omitempty"`
	// Contains, when set, must appear in the response body — the only
	// assertion class the deterministic executor makes (substring,
	// case-sensitive).
	Contains string `json:"contains,omitempty"`
}

// deployVerifyResultEntry is one executed check.
type deployVerifyResultEntry struct {
	Name       string `json:"name"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Passed     bool   `json:"passed"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Expected   int    `json:"expected"`
	LatencyMs  int64  `json:"latencyMs"`
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// deployVerifyState is the persisted payload for one deploy request.
type deployVerifyState struct {
	Spec        *deployVerifySpec         `json:"spec,omitempty"`
	Results     []deployVerifyResultEntry `json:"results,omitempty"`
	Passed      *bool                     `json:"passed,omitempty"`
	GeneratedBy string                    `json:"generatedBy,omitempty"`
	GeneratedAt string                    `json:"generatedAt,omitempty"`
	ExecutedAt  string                    `json:"executedAt,omitempty"`
	GenError    string                    `json:"genError,omitempty"`
}

type deployVerifySpec struct {
	Checks []deployVerifyCheck `json:"checks"`
}

func (s *Server) loadDeployVerification(workspaceID, requestID string) *deployVerifyState {
	raw, found, err := s.controlDB.GetRecord(deployVerifyKVTable, workspaceID, []string{requestID})
	if err != nil || !found || strings.TrimSpace(raw) == "" {
		return nil
	}
	var st deployVerifyState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil
	}
	return &st
}

func (s *Server) storeDeployVerification(workspaceID, requestID string, st *deployVerifyState) {
	raw, err := json.Marshal(st)
	if err != nil {
		log.Printf("[deploy-verify] %s: marshal state: %v", requestID, err)
		return
	}
	if err := s.controlDB.UpsertRecord(deployVerifyKVTable, workspaceID, []string{requestID}, string(raw)); err != nil {
		log.Printf("[deploy-verify] %s: persist state: %v", requestID, err)
	}
}

// ---- spec generation (LLM, one stateless call) ----

// buildDeployVerifyPrompt seeds the spec-extraction request. It carries the
// commit span and the strict output contract; the platform validates the
// result regardless of what the model claims.
func buildDeployVerifyPrompt(project string, req *controldb.DeployRequest) string {
	var b strings.Builder
	b.WriteString("你是 Multigent 部署校验器的规格生成器。项目 " + project + " 刚完成一次部署，")
	b.WriteString("请根据本次部署覆盖的变更，产出一组部署后冒烟校验项（纯 HTTP 探测，无浏览器）。\n\n")
	b.WriteString("【本次部署范围】\n")
	b.WriteString("- 分支：" + req.Branch + "\n")
	b.WriteString("- 目标提交：" + shortDeploySHA(req.SHA) + "\n")
	if len(req.CommitSpan) > 0 {
		b.WriteString("- 基线提交：" + shortDeploySHA(req.CommitSpan[0].SHA) + "\n")
	}
	for i, c := range req.CommitSpan {
		if i >= 20 {
			break
		}
		title := strings.TrimSpace(c.Title)
		if title == "" {
			title = shortDeploySHA(c.SHA)
		}
		fmt.Fprintf(&b, "  · %s\n", title)
	}
	b.WriteString(`
【输出格式】只输出一个 JSON 对象，不要任何其它文字或代码围栏：
{"checks": [
  {"name":"健康探针","method":"GET","path":"/api/health","expectedStatus":200},
  {"name":"首页可访问","method":"GET","path":"/","expectedStatus":200,"contains":"<title"}
]}
【硬性规则】
1. path 必须以 / 开头，不允许 http:// 或域名（平台固定打部署机，你无权选择主机）。
2. method 只能是 GET/POST/PUT/HEAD；POST/PUT 需给 body（≤2KB）。
3. expectedStatus 必须是 2xx-4xx 之间的明确数字；不要把预期写成"可能"。
4. 最多 8 项，按重要性排序：健康/存活探测必在第 1 项。
5. contains 是响应体子串断言（可选），选稳定不变的特征串。
6. 不要猜测需要认证的接口（平台不带凭据）。`)
	return b.String()
}

// parseDeployVerifySpec validates the model's JSON into a safe spec. Every
// field is re-validated here: the model output is untrusted input.
func parseDeployVerifySpec(raw string) (*deployVerifySpec, error) {
	raw = strings.TrimSpace(raw)
	// Tolerate a ```json fence the model adds despite instructions.
	if idx := strings.Index(raw, "{"); idx > 0 {
		if end := strings.LastIndex(raw, "}"); end > idx {
			raw = raw[idx : end+1]
		}
	}
	var spec deployVerifySpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, fmt.Errorf("spec json: %w", err)
	}
	if len(spec.Checks) == 0 {
		return nil, fmt.Errorf("spec has no checks")
	}
	if len(spec.Checks) > deployVerifySpecMax {
		spec.Checks = spec.Checks[:deployVerifySpecMax]
	}
	out := make([]deployVerifyCheck, 0, len(spec.Checks))
	for i, c := range spec.Checks {
		c.Name = strings.TrimSpace(c.Name)
		c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
		c.Path = strings.TrimSpace(c.Path)
		if c.Name == "" {
			c.Name = fmt.Sprintf("check-%d", i+1)
		}
		if len(c.Name) > 80 {
			c.Name = c.Name[:80]
		}
		switch c.Method {
		case "GET", "HEAD":
		case "POST", "PUT":
			if len(c.Body) > deployVerifyMaxBodyBytes {
				c.Body = c.Body[:deployVerifyMaxBodyBytes]
			}
		case "":
			c.Method = "GET"
		default:
			return nil, fmt.Errorf("check %q: method %q not allowed", c.Name, c.Method)
		}
		// Host red line: path-only. Absolute URLs, protocol-relative URLs and
		// backslash tricks are all rejected outright.
		if c.Path == "" || !strings.HasPrefix(c.Path, "/") ||
			strings.Contains(c.Path, "://") || strings.HasPrefix(c.Path, "//") {
			return nil, fmt.Errorf("check %q: path must be site-local (start with /)", c.Name)
		}
		if len(c.Path) > 512 {
			return nil, fmt.Errorf("check %q: path too long", c.Name)
		}
		if c.ExpectedStatus < 200 || c.ExpectedStatus > 499 {
			return nil, fmt.Errorf("check %q: expectedStatus %d out of 200-499", c.Name, c.ExpectedStatus)
		}
		if len(c.Headers) > 8 {
			c.Headers = nil
		}
		for k := range c.Headers {
			lk := strings.ToLower(k)
			if lk == "host" || lk == "authorization" || lk == "cookie" {
				delete(c.Headers, k)
			}
		}
		if len(c.Contains) > 256 {
			c.Contains = c.Contains[:256]
		}
		out = append(out, c)
	}
	spec.Checks = out
	return &spec, nil
}

// generateDeployVerification runs one LLM call to produce the smoke spec.
// Failures are recorded in state.GenError and never block the deploy ledger:
// verification is best-effort hardening, not a gate.
func (s *Server) generateDeployVerification(ctx context.Context, workspaceID, project string, req *controldb.DeployRequest) *deployVerifyState {
	st := &deployVerifyState{GeneratedAt: nowUTCAPI()}
	st.GenError = "no agent in project"
	agent := s.deployAIProjectAgent(project)
	if agent == "" {
		s.storeDeployVerification(workspaceID, req.ID, st)
		return st
	}
	creds, err := s.resolveDesignModelProvider(project, agent, deployAIDefaultModel)
	if err != nil || creds == nil || creds.BaseURL == "" || creds.APIKey == "" {
		st.GenError = "model provider unresolved"
		s.storeDeployVerification(workspaceID, req.ID, st)
		return st
	}
	st.GeneratedBy = "agent:" + agent + "/" + creds.Model
	provider := entity.APIProvider{
		Type:    creds.Protocol,
		BaseURL: creds.BaseURL,
		APIKey:  creds.APIKey,
		Model:   creds.Model,
	}
	gctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	reply, err := invokeDeployAIProviderChat(gctx, provider, buildDeployVerifyPrompt(project, req), nil,
		"请输出本次部署的冒烟校验规格 JSON。")
	if err != nil {
		st.GenError = "model call failed: " + truncateDeployVerify(err.Error(), 200)
		s.storeDeployVerification(workspaceID, req.ID, st)
		return st
	}
	spec, err := parseDeployVerifySpec(reply)
	if err != nil {
		st.GenError = "spec invalid: " + truncateDeployVerify(err.Error(), 200)
		s.storeDeployVerification(workspaceID, req.ID, st)
		return st
	}
	st.Spec = spec
	st.GenError = ""
	s.storeDeployVerification(workspaceID, req.ID, st)
	return st
}

func truncateDeployVerify(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- deterministic execution (pure HTTP, platform-side) ----

// deployedVerifyBaseURL returns the smoke-target base URL. It reuses the
// health probe's host red line: MULTIGENT_DEPLOY_HOST + the project port.
func deployedVerifyBaseURL(deployPort int) (string, bool) {
	host := strings.TrimSpace(os.Getenv(deployHostEnv))
	if host == "" || deployPort <= 0 {
		return "", false
	}
	return fmt.Sprintf("http://%s:%d", host, deployPort), true
}

// runDeployVerification executes every spec check with the platform's own
// HTTP client. Deterministic: no agent, no browser, no user input.
func (s *Server) runDeployVerification(ctx context.Context, project string, p *entity.Project, req *controldb.DeployRequest, st *deployVerifyState) *deployVerifyState {
	if st == nil || st.Spec == nil || len(st.Spec.Checks) == 0 {
		return st
	}
	base, ok := deployedVerifyBaseURL(p.DeployPort)
	results := make([]deployVerifyResultEntry, 0, len(st.Spec.Checks))
	if !ok {
		for _, c := range st.Spec.Checks {
			results = append(results, deployVerifyResultEntry{
				Name: c.Name, Method: c.Method, Path: c.Path,
				Expected: c.ExpectedStatus, Passed: false,
				Error: "deploy host not configured (" + deployHostEnv + ")",
			})
		}
		failed := false
		st.Results, st.Passed, st.ExecutedAt = results, &failed, nowUTCAPI()
		s.storeDeployVerification(req.WorkspaceID, req.ID, st)
		return st
	}
	client := &http.Client{Timeout: deployVerifyHTTPTimeout}
	for _, c := range st.Spec.Checks {
		entry := s.executeDeployCheck(ctx, client, base, c)
		results = append(results, entry)
	}
	st.Results = results
	st.ExecutedAt = nowUTCAPI()
	passed := true
	for _, r := range results {
		if !r.Passed {
			passed = false
			break
		}
	}
	st.Passed = &passed
	s.storeDeployVerification(req.WorkspaceID, req.ID, st)
	return st
}

func (s *Server) executeDeployCheck(ctx context.Context, client *http.Client, baseURL string, c deployVerifyCheck) deployVerifyResultEntry {
	entry := deployVerifyResultEntry{
		Name: c.Name, Method: c.Method, Path: c.Path, Expected: c.ExpectedStatus,
	}
	var bodyReader io.Reader
	if c.Body != "" {
		bodyReader = strings.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, baseURL+c.Path, bodyReader)
	if err != nil {
		entry.Error = "build request: " + err.Error()
		return entry
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := client.Do(req)
	entry.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		entry.Error = "request failed: " + err.Error()
		return entry
	}
	defer resp.Body.Close()
	entry.HTTPStatus = resp.StatusCode
	raw, _ := readLimitedDeployBody(resp, 64<<10)
	body := string(raw)
	if resp.StatusCode != c.ExpectedStatus {
		entry.Detail = truncateDeployVerify(strings.TrimSpace(body), 300)
		return entry
	}
	if c.Contains != "" && !strings.Contains(body, c.Contains) {
		return entry
	}
	entry.Passed = true
	return entry
}

func readLimitedDeployBody(resp *http.Response, max int64) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 4096)
	for int64(len(buf)) < max {
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			return buf, nil // EOF or transient read error: use what we got
		}
	}
	return buf, nil
}

// ---- deploy-success hook ----

// maybeStartDeployVerification is called from finishDeployWatch on success.
// Best-effort: spec generation runs in a goroutine, all failures are logged
// and stored in the state — never propagated to the ledger transition.
func (s *Server) maybeStartDeployVerification(workspaceID, project string, req *controldb.DeployRequest) {
	if req == nil || strings.TrimSpace(os.Getenv(deployHostEnv)) == "" {
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[deploy-verify] %s panicked: %v", req.ID, rec)
			}
		}()
		ctx := context.Background()
		p, err := s.st.Project(project)
		if err != nil || p == nil {
			log.Printf("[deploy-verify] %s: project %s unreadable: %v", req.ID, project, err)
			return
		}
		st := s.generateDeployVerification(ctx, workspaceID, project, req)
		if st.Spec == nil {
			log.Printf("[deploy-verify] %s: spec generation failed: %s", req.ID, st.GenError)
			return
		}
		st = s.runDeployVerification(ctx, project, p, req, st)
		log.Printf("[deploy-verify] %s: executed %d checks, passed=%v", req.ID, len(st.Results), st.Passed != nil && *st.Passed)
		if st.Passed != nil && !*st.Passed {
			s.postDeployVerifyFailureTask(project, req, st)
		}
	}()
}

// postDeployVerifyFailureTask auto-posts one issue task summarizing the
// failed checks (created_by deploy-verify) so the failure always lands in the
// project backlog even if nobody is watching the ledger.
func (s *Server) postDeployVerifyFailureTask(project string, req *controldb.DeployRequest, st *deployVerifyState) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[deploy-verify] %s: failure task panicked: %v", req.ID, rec)
		}
	}()
	agent := s.deployAIProjectAgent(project)
	if agent == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "部署 %s（%s@%s）部署后冒烟校验失败，共 %d 项：\n\n", req.ID, req.Branch, shortDeploySHA(req.SHA), len(st.Results))
	failed := 0
	for _, r := range st.Results {
		if r.Passed {
			continue
		}
		failed++
		fmt.Fprintf(&b, "%d. [%s] %s %s\n", failed, r.Name, r.Method, r.Path)
		if r.HTTPStatus != 0 {
			fmt.Fprintf(&b, "   实际 HTTP %d（期望 %d）\n", r.HTTPStatus, r.Expected)
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "   错误：%s\n", r.Error)
		}
	}
	if failed == 0 {
		return
	}
	prompt := "来源：部署后自动校验（created_by deploy-verify）。以下是平台确定性 HTTP 校验的失败清单，请复核并修复。\n\n" + b.String()
	task := &entity.Task{
		ID:          entity.NewTaskID(),
		Title:       fmt.Sprintf("部署 %s 冒烟校验失败（%d 项）", shortDeploySHA(req.SHA), failed),
		Description: "部署后自动校验失败转任务",
		Type:        entity.TaskTypeBug,
		Priority:    2,
		Assignee:    project + "/" + agent,
		CreatedBy:   deployVerifyCreatedBy,
		Status:      entity.TaskStatusPending,
		Prompt:      prompt,
		Labels:      []string{"deploy-verify"},
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := s.ts.AddTask(project, agent, task); err != nil {
		log.Printf("[deploy-verify] %s: failure task add failed: %v", req.ID, err)
		return
	}
	log.Printf("[deploy-verify] %s: failure task %s posted to %s/%s", req.ID, task.ID, project, agent)
}

// ---- HTTP surface ----

// handleGetDeployVerification serves GET .../deploy/requests/{id}/verification.
func (s *Server) handleGetDeployVerification(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}
	req, found, err := s.controlDB.DeployRequestFor(workspaceID, r.PathValue("id"))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || req.ProjectID != project {
		s.jsonError(w, http.StatusNotFound, "deploy request not found")
		return
	}
	st := s.loadDeployVerification(workspaceID, req.ID)
	w.Header().Set("Content-Type", "application/json")
	if st == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"exists": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"exists": true, "verification": st})
}

// handleRunDeployVerification serves POST .../deploy/requests/{id}/verification/run
// (operator-gated replay: executes the stored spec again and returns results).
func (s *Server) handleRunDeployVerification(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}
	req, found, err := s.controlDB.DeployRequestFor(workspaceID, r.PathValue("id"))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || req.ProjectID != project {
		s.jsonError(w, http.StatusNotFound, "deploy request not found")
		return
	}
	p, ok := s.deployProjectRow(w, project)
	if !ok {
		return
	}
	st := s.loadDeployVerification(workspaceID, req.ID)
	if st == nil || st.Spec == nil {
		s.jsonError(w, http.StatusConflict, "no verification spec stored for this request")
		return
	}
	st = s.runDeployVerification(r.Context(), project, p, req, st)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"exists": true, "verification": st})
}

// handleGenerateDeployVerification serves POST .../deploy/requests/{id}/verification/generate
// (operator-gated regeneration: one LLM call → validated spec → stored).
func (s *Server) handleGenerateDeployVerification(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || workspaceID == "" {
		s.serverError(w, fmt.Errorf("resolve workspace: %w", err))
		return
	}
	req, found, err := s.controlDB.DeployRequestFor(workspaceID, r.PathValue("id"))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || req.ProjectID != project {
		s.jsonError(w, http.StatusNotFound, "deploy request not found")
		return
	}
	if !s.allowDesignRequest("deploy-verify:"+req.ID, true) {
		s.jsonErrorCode(w, http.StatusTooManyRequests, ErrCodeServiceUnavailable, "too many verification generations, slow down")
		return
	}
	st := s.generateDeployVerification(r.Context(), workspaceID, project, req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"exists": true, "verification": st})
}
