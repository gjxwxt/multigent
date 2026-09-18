package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/store"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func setupReviewerTaskEnvironment(t *testing.T, locale string) (*controldb.SQLiteStore, *Runner, string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", root)

	dbDir := filepath.Join(root, ".multigent")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir .multigent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "agency.yaml"), []byte("name: ReviewerPromptTest\n"), 0o644); err != nil {
		t.Fatalf("write agency.yaml: %v", err)
	}

	dbPath := filepath.Join(dbDir, "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open controldb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	workspaceID := "ws-reviewer-prompt-test"
	if err := db.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Reviewer Prompt Workspace",
		Slug: workspaceID,
		Root: root,
	}); err != nil {
		t.Fatalf("upsert workspace: %v", err)
	}

	projectName := "audit-project"
	taskID := "task-audit-001"

	for _, worker := range []struct {
		id   string
		name string
		role string
	}{
		{"aw-pm", "pm-agent", "pm"},
		{"aw-dev", "developer-agent", "developer"},
		{"aw-reviewer", "reviewer-agent", "reviewer"},
	} {
		if err := db.UpsertAgentWorker(controldb.AgentWorker{
			ID:            worker.id,
			WorkspaceID:   workspaceID,
			Name:          worker.name,
			Role:          worker.role,
			ProfilePrompt: "Long-term role profile for " + worker.name,
		}); err != nil {
			t.Fatalf("upsert agent worker %s: %v", worker.name, err)
		}
		if err := db.UpsertProjectMembership(controldb.ProjectMembership{
			ID:          "pm-" + worker.id,
			WorkspaceID: workspaceID,
			ProjectID:   projectName,
			MemberType:  "agent_worker",
			MemberID:    worker.id,
			Role:        worker.role,
		}); err != nil {
			t.Fatalf("upsert membership %s: %v", worker.name, err)
		}
	}

	wfStore := workflowstore.NewStore(db, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("unified-delivery-pipeline", locale, "Unified Pipeline ("+locale+")")
	if !ok {
		t.Fatalf("template unified-delivery-pipeline missing for locale %s", locale)
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}

	bindings := map[string]entity.WorkflowActorBinding{
		"pm-agent":        {Type: "agent", ID: "pm-agent"},
		"product-owner":   {Type: "human", ID: "owner"},
		"developer-agent": {Type: "agent", ID: "developer-agent"},
		"reviewer-agent":  {Type: "agent", ID: "reviewer-agent"},
		"owner-engineer":  {Type: "human", ID: "tech-lead"},
		"qa-agent":        {Type: "agent", ID: "qa-agent"},
		"qa-owner":        {Type: "human", ID: "qa-lead"},
		"release-agent":   {Type: "agent", ID: "release-agent"},
	}

	if _, _, err := wfStore.StartRun(projectName, taskID, def.ID, bindings); err != nil {
		t.Fatalf("start run: %v", err)
	}

	// 1. clarify
	_, err = wfStore.CompleteAndAdvance(projectName, taskID, "clarify", "", map[string]string{
		"clarified": "Scope: Add audit logging to user update\nAC-1: Log user ID and changed fields\nAC-2: Mask password fields in audit payload\nNon-goals: No DB schema migration",
	}, "completed")
	if err != nil {
		t.Fatalf("advance clarify: %v", err)
	}

	// 2. clarify_review
	_, err = wfStore.CompleteAndAdvance(projectName, taskID, "approve", "", map[string]string{
		"decision":       "approve",
		"comments":       "Approved scope.",
		"approved_scope": "Scope: Add audit logging to user update\nAC-1: Log user ID and changed fields\nAC-2: Mask password fields in audit payload\nNon-goals: No DB schema migration",
	}, "completed")
	if err != nil {
		t.Fatalf("advance clarify_review: %v", err)
	}

	// 3. implement
	_, err = wfStore.CompleteAndAdvance(projectName, taskID, "implement", "", map[string]string{
		"implementation": "Branch: task/audit-log\nBase SHA: 37f610cb821a\nHEAD SHA: 893b62e691bc\nCommit Message: feat(audit): add audit logging on user update\nTests: 4/4 passing locally",
		"review_rounds":  "1",
	}, "completed")
	if err != nil {
		t.Fatalf("advance implement: %v", err)
	}

	runner := &Runner{
		root:       root,
		agentStore: store.NewDB(root, db),
	}

	return db, runner, projectName, taskID
}

func TestReviewerPromptContractZhCN(t *testing.T) {
	_, runner, projectName, taskID := setupReviewerTaskEnvironment(t, "zh-CN")

	prompt := runner.BuildTaskPrompt(projectName, "reviewer-agent", &entity.Task{
		ID:     taskID,
		Prompt: "Please execute code review for task.",
	})

	// 1. Verify Workflow Context header and current step identification
	expectedHeaders := []string{
		"## Workflow Context",
		"Current step: Agent 初审 (`agent_self_review`, type: `agent_task`)",
		"- Running agent: `audit-project/reviewer-agent`",
	}
	for _, h := range expectedHeaders {
		if !strings.Contains(prompt, h) {
			t.Errorf("prompt missing header/identity clause: %q", h)
		}
	}

	// 2. Verify Reviewer Independent Baseline Contract (7 Core Clauses) in zh-CN
	clauses := []struct {
		name    string
		snippet string
	}{
		{"Clause 1: Independent Baseline Before Diff", "先读需求澄清与已确认范围，不打开 diff，独立写出预期基线"},
		{"Clause 2: Base SHA and HEAD SHA from implementation", "从 implementation 工件记下 base SHA 与 HEAD SHA，只读该区间的 diff"},
		{"Clause 3: Personal Execution Requirement", "亲自重跑构建、测试与 lint——没有亲手跑过的检查视为 unverified，不能支撑 approve"},
		{"Clause 4: Criterion-by-Criterion and Out-of-Scope Check", "逐条标准对照基线"},
		{"Clause 4b: Out-of-Scope Guard", "检查是否混入范围外改动"},
		{"Clause 5: Single Token Routing Verdict", "self_review_verdict 只写路由结论，必须是单一词元——pass / issues_fixed / escalate"},
		{"Clause 6: Structured Escalation Case File", "escalation_case 是结构化问题清单"},
		{"Clause 7: Reasoning Chain Isolation", "不要读取或信任作者的思考链，只裁决 diff 与其可观察行为"},
		{"Environment Escalation Rule", "环境型故障升级规则：若失败由环境因素"},
	}
	for _, c := range clauses {
		if !strings.Contains(prompt, c.snippet) {
			t.Errorf("prompt missing contract clause [%s]: %q", c.name, c.snippet)
		}
	}

	// 3. Verify Upstream Inputs Passed to Current Step
	upstreamInputs := []string{
		"AC-1: Log user ID and changed fields",
		"AC-2: Mask password fields in audit payload",
		"Base SHA: 37f610cb821a",
		"HEAD SHA: 893b62e691bc",
		"- `review_rounds`:",
	}
	for _, in := range upstreamInputs {
		if !strings.Contains(prompt, in) {
			t.Errorf("prompt missing upstream input data: %q", in)
		}
	}

	// 4. Verify Required Output Fields
	outputFields := []string{
		"- `self_review`: ",
		"- `self_review_verdict`: ",
		"- `escalation_case`: ",
		"- `review_rounds`: ",
	}
	for _, field := range outputFields {
		if !strings.Contains(prompt, field) {
			t.Errorf("prompt missing required output field: %q", field)
		}
	}

	// 5. Verify Agent Worker Profile and Instructions
	if !strings.Contains(prompt, "Long-term role profile for reviewer-agent") {
		t.Errorf("prompt missing agent worker profile")
	}
	if !strings.Contains(prompt, "mga task step done --id "+taskID) {
		t.Errorf("prompt missing task step done instructions")
	}
}

func TestReviewerPromptContractEN(t *testing.T) {
	_, runner, projectName, taskID := setupReviewerTaskEnvironment(t, "en")

	prompt := runner.BuildTaskPrompt(projectName, "reviewer-agent", &entity.Task{
		ID:     taskID,
		Prompt: "Please execute code review for task.",
	})

	// 1. Verify Workflow Context header and current step identification
	expectedHeaders := []string{
		"## Workflow Context",
		"Current step: Agent Self Review (`agent_self_review`, type: `agent_task`)",
		"- Running agent: `audit-project/reviewer-agent`",
	}
	for _, h := range expectedHeaders {
		if !strings.Contains(prompt, h) {
			t.Errorf("prompt missing header/identity clause: %q", h)
		}
	}

	// 2. Verify Reviewer Independent Baseline Contract (7 Core Clauses) in EN
	clauses := []struct {
		name    string
		snippet string
	}{
		{"Clause 1: Independent Baseline Before Diff", "without opening the diff, write your own baseline"},
		{"Clause 2: Base SHA and HEAD SHA from implementation", "Note the base SHA and HEAD SHA from the implementation artifact"},
		{"Clause 3: Personal Execution Requirement", "Re-run build, tests, and lint yourself — a check you did not run is unverified and cannot back an approval"},
		{"Clause 4: Criterion-by-Criterion and Out-of-Scope Check", "compare it against your baseline, criterion by criterion"},
		{"Clause 4b: Out-of-Scope Guard", "Check for changes outside the approved scope"},
		{"Clause 5: Single Token Routing Verdict", "self_review_verdict holds ONLY the routing verdict, exactly one token — pass / issues_fixed / escalate"},
		{"Clause 6: Structured Escalation Case File", "escalation_case is a structured issue list"},
		{"Clause 7: Reasoning Chain Isolation", "do not read or trust the author's reasoning chain, judge only the diff and its observable behavior"},
		{"Environment Escalation Rule", "Environment-failure escalation: when the failure stems from environmental factors"},
	}
	for _, c := range clauses {
		if !strings.Contains(prompt, c.snippet) {
			t.Errorf("prompt missing contract clause [%s]: %q", c.name, c.snippet)
		}
	}

	// 3. Verify Upstream Inputs Passed to Current Step
	upstreamInputs := []string{
		"AC-1: Log user ID and changed fields",
		"AC-2: Mask password fields in audit payload",
		"Base SHA: 37f610cb821a",
		"HEAD SHA: 893b62e691bc",
		"- `review_rounds`:",
	}
	for _, in := range upstreamInputs {
		if !strings.Contains(prompt, in) {
			t.Errorf("prompt missing upstream input data: %q", in)
		}
	}

	// 4. Verify Required Output Fields
	outputFields := []string{
		"- `self_review`: ",
		"- `self_review_verdict`: ",
		"- `escalation_case`: ",
		"- `review_rounds`: ",
	}
	for _, field := range outputFields {
		if !strings.Contains(prompt, field) {
			t.Errorf("prompt missing required output field: %q", field)
		}
	}

	// 5. Verify Agent Worker Profile and Instructions
	if !strings.Contains(prompt, "Long-term role profile for reviewer-agent") {
		t.Errorf("prompt missing agent worker profile")
	}
	if !strings.Contains(prompt, "mga task step done --id "+taskID) {
		t.Errorf("prompt missing task step done instructions")
	}
}
