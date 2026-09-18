package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// setupUnifiedWorkflowRun sets up a real SQLite database, workspace, task,
// and instantiates unified-delivery-pipeline for the task.
func setupUnifiedWorkflowRun(t *testing.T, locale string) (*controldb.SQLiteStore, *Store, string, string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "control.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open controldb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatalf("mkdir .multigent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: ReviewerTest\n"), 0o644); err != nil {
		t.Fatalf("write agency.yaml: %v", err)
	}

	workspaceID := "ws-reviewer-test"
	if err := db.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Reviewer Test Workspace",
		Slug: workspaceID,
		Root: root,
	}); err != nil {
		t.Fatalf("upsert workspace: %v", err)
	}

	projectName := "proj-audit"
	taskID := "task-reviewer-slice-001"

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
			ID:          worker.id,
			WorkspaceID: workspaceID,
			Name:        worker.name,
			Role:        worker.role,
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

	wfStore := NewStore(db, workspaceID)
	def, ok := DefinitionFromTemplate("unified-delivery-pipeline", locale, "Unified Pipeline ("+locale+")")
	if !ok {
		t.Fatalf("unified-delivery-pipeline template missing for locale %s", locale)
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

	return db, wfStore, projectName, taskID
}

// driveRunToSelfReview advances through clarify -> clarify_review -> implement.
func driveRunToSelfReview(t *testing.T, wfStore *Store, projectName, taskID string) {
	t.Helper()

	// 1. clarify: produce clear requirement scope and acceptance criteria
	_, err := wfStore.CompleteAndAdvance(projectName, taskID, "clarify", "", map[string]string{
		"clarified": "Scope: Add audit logging to user update\nAC-1: Log user ID and changed fields\nAC-2: Mask password fields in audit payload\nNon-goals: No DB schema migration",
	}, "completed")
	if err != nil {
		t.Fatalf("complete clarify: %v", err)
	}

	// 2. clarify_review: approve scope
	_, err = wfStore.CompleteAndAdvance(projectName, taskID, "approve", "", map[string]string{
		"decision":       "approve",
		"comments":       "Approved as stated. Proceed to implementation.",
		"approved_scope": "Scope: Add audit logging to user update\nAC-1: Log user ID and changed fields\nAC-2: Mask password fields in audit payload\nNon-goals: No DB schema migration",
	}, "completed")
	if err != nil {
		t.Fatalf("complete clarify_review: %v", err)
	}

	// 3. implement: implement code with base SHA and head SHA
	_, err = wfStore.CompleteAndAdvance(projectName, taskID, "implement", "", map[string]string{
		"implementation": "Branch: task/audit-log\nBase SHA: 37f610cb821a\nHEAD SHA: 893b62e691bc\nCommit Message: feat(audit): add audit logging on user update\nTests: 4/4 passing locally",
		"review_rounds":  "1",
	}, "completed")
	if err != nil {
		t.Fatalf("complete implement: %v", err)
	}
}

func TestReviewerContractRoutingPass(t *testing.T) {
	_, wfStore, projectName, taskID := setupUnifiedWorkflowRun(t, "zh-CN")
	driveRunToSelfReview(t, wfStore, projectName, taskID)

	reviewReport := `
### 独立预期基线 (Expected Baseline)
- AC-1: 用户更新操作应触发 audit_events 写入，记录操作者 ID 与变更字段。
- AC-2: 密码字段（password/hash）必须在审计 JSON 中显示为 [REDACTED]。
- 测试切面: 需包含密码脱敏单测与正常字段记录单测。

### Diff 对照与验证 (Comparison & Verification)
- Base SHA 37f610cb821a 到 HEAD SHA 893b62e691bc 变更比对：
  - user_service.go:214: 正确触发 AuditService.Record
  - audit_helper.go:45: 正确过滤 password 键
- 实测执行证据: 运行 go test -v ./internal/audit 通过 (4/4 PASS)。无 unverified 项。
- 范围检查: 无无关文件改动。
`
	res, err := wfStore.CompleteAndAdvance(projectName, taskID, "pass", "", map[string]string{
		"self_review":         strings.TrimSpace(reviewReport),
		"self_review_verdict": "pass",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("CompleteAndAdvance on pass: %v", err)
	}

	if res.Next == nil || res.Next.ID != "ci_ready_gate" {
		t.Fatalf("expected next step ci_ready_gate, got %+v", res.Next)
	}
	run, _, _ := wfStore.RunForTask(projectName, taskID)
	if run.ActiveStepID != "ci_ready_gate" {
		t.Fatalf("expected active step ci_ready_gate, got %s", run.ActiveStepID)
	}
}

func TestReviewerContractRoutingIssuesFixedRework(t *testing.T) {
	_, wfStore, projectName, taskID := setupUnifiedWorkflowRun(t, "zh-CN")
	driveRunToSelfReview(t, wfStore, projectName, taskID)

	reworkReport := `
### 独立预期基线 (Expected Baseline)
- AC-2: 密码字段必须被脱敏。

### Diff 对照与验证 (Comparison & Verification)
- 检查 user_service.go:218：直接将 updateReq 序列化，包含 raw_password 明文！违反 AC-2 安全契约。
- 最小修复建议: 在写入 audit 前执行 redactSensitiveFields。
`
	res, err := wfStore.CompleteAndAdvance(projectName, taskID, "rework", "", map[string]string{
		"self_review":         strings.TrimSpace(reworkReport),
		"self_review_verdict": "issues_fixed",
		"review_rounds":       "1",
	}, "completed")
	if err != nil {
		t.Fatalf("CompleteAndAdvance on rework: %v", err)
	}

	if res.Next == nil || res.Next.ID != "implement" {
		t.Fatalf("expected rework edge to implement, got %+v", res.Next)
	}
	run, _, _ := wfStore.RunForTask(projectName, taskID)
	if run.ActiveStepID != "implement" {
		t.Fatalf("expected active step implement, got %s", run.ActiveStepID)
	}

	// Verify input values into the new implement step instance
	if res.NextInst == nil || res.NextInst.StepID != "implement" {
		t.Fatalf("expected NextInst to be implement, got %+v", res.NextInst)
	}
	if !strings.Contains(res.NextInst.InputValues["review_comments"], "违反 AC-2 安全契约") {
		t.Fatalf("expected review_comments forwarded to implement, got %v", res.NextInst.InputValues)
	}
	if res.NextInst.InputValues["review_rounds"] != "2" {
		t.Fatalf("expected review_rounds 2, got %v", res.NextInst.InputValues["review_rounds"])
	}
}

func TestReviewerContractRoutingEscalateRoundCap(t *testing.T) {
	_, wfStore, projectName, taskID := setupUnifiedWorkflowRun(t, "zh-CN")
	driveRunToSelfReview(t, wfStore, projectName, taskID)

	escalationCase := `
[
  {
    "contract_violated": "AC-2 Password Redaction",
    "file_line": "user_service.go:218",
    "impact": "Plaintext password leaks into audit table",
    "minimal_fix": "Add sanitizer.Mask(req)",
    "missing_verification": "Unit test TestUserUpdateAuditMasking"
  }
]
`
	res, err := wfStore.CompleteAndAdvance(projectName, taskID, "escalate", "", map[string]string{
		"self_review":         "3 rounds reached; escalating unresolved contract violations.",
		"self_review_verdict": "escalate",
		"escalation_case":     strings.TrimSpace(escalationCase),
		"review_rounds":       "3",
	}, "completed")
	if err != nil {
		t.Fatalf("CompleteAndAdvance on escalate: %v", err)
	}

	if res.Next == nil || res.Next.ID != "code_review" {
		t.Fatalf("expected escalate edge to code_review, got %+v", res.Next)
	}
	run, _, _ := wfStore.RunForTask(projectName, taskID)
	if run.ActiveStepID != "code_review" {
		t.Fatalf("expected active step code_review, got %s", run.ActiveStepID)
	}

	// Verify escalation_case is passed to code_review
	if res.NextInst == nil || res.NextInst.StepID != "code_review" {
		t.Fatalf("expected NextInst to be code_review, got %+v", res.NextInst)
	}
	if !strings.Contains(res.NextInst.InputValues["escalation_case"], "AC-2 Password Redaction") {
		t.Fatalf("expected escalation_case forwarded to code_review, got %v", res.NextInst.InputValues)
	}
}

func TestReviewerContractDefensiveRejectionOnInvalidVerdict(t *testing.T) {
	_, wfStore, projectName, taskID := setupUnifiedWorkflowRun(t, "zh-CN")
	driveRunToSelfReview(t, wfStore, projectName, taskID)

	// Attempt to complete with an invalid verdict token (e.g. natural language "LGTM" or "approve")
	_, err := wfStore.CompleteAndAdvance(projectName, taskID, "approve", "", map[string]string{
		"self_review":         "Looks good to me!",
		"self_review_verdict": "approved", // Invalid token! Valid are: pass / issues_fixed / escalate
		"review_rounds":       "1",
	}, "completed")
	if err == nil {
		t.Fatal("expected error on invalid self_review_verdict, got nil")
	}

	// Run must remain on agent_self_review (fail-closed)
	run, _, _ := wfStore.RunForTask(projectName, taskID)
	if run.ActiveStepID != "agent_self_review" {
		t.Fatalf("fail-closed broken: run moved to %s on invalid verdict", run.ActiveStepID)
	}
}
