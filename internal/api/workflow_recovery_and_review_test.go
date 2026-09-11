package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func TestRecoverActiveWorkflowRunsFiltersHumanReviewAndCompleted(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	now := time.Now().UTC()

	// Task 1: on human review step
	taskHuman := &entity.Task{
		ID:        "t-human-review",
		Title:     "Human review task",
		Priority:  2,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", taskHuman); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// Task 2: on agent step (should be resumed)
	taskAgent := &entity.Task{
		ID:        "t-agent-step",
		Title:     "Agent step task",
		Priority:  2,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", taskAgent); err != nil {
		t.Fatalf("add task: %v", err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	defHuman := &entity.WorkflowDefinition{
		ID:          "wf-human-test",
		Name:        "Human Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "review",
		Steps: []entity.WorkflowStep{{
			ID:       "review",
			Type:     "human_review",
			Title:    "Review",
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(defHuman); err != nil {
		t.Fatalf("save human def: %v", err)
	}
	if _, _, err := wfStore.StartRun("sample", taskHuman.ID, defHuman.ID, nil); err != nil {
		t.Fatalf("start human run: %v", err)
	}

	defAgent := &entity.WorkflowDefinition{
		ID:          "wf-agent-test",
		Name:        "Agent Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "qa",
		Steps: []entity.WorkflowStep{{
			ID:       "qa",
			Type:     "agent_task",
			Title:    "QA Step",
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(defAgent); err != nil {
		t.Fatalf("save agent def: %v", err)
	}
	if _, _, err := wfStore.StartRun("sample", taskAgent.ID, defAgent.ID, nil); err != nil {
		t.Fatalf("start agent run: %v", err)
	}

	resumed := s.recoverActiveWorkflowRunsWithDelay(0)
	if resumed != 1 {
		t.Fatalf("expected exactly 1 task to be resumed, got %d", resumed)
	}
}

func TestIsTaskAtHumanReviewStep(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	now := time.Now().UTC()

	taskHuman := &entity.Task{
		ID:        "t-hr-check",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.ts.AddTask("sample", "pm", taskHuman)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID:          "wf-hr-check",
		Name:        "HR Check",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "rev",
		Steps: []entity.WorkflowStep{
			{ID: "rev", Type: "human_review", Title: "Rev"},
			{ID: "dev", Type: "agent_task", Title: "Dev"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = wfStore.SaveDefinition(def)
	_, _, _ = wfStore.StartRun("sample", taskHuman.ID, def.ID, nil)

	if !s.isTaskAtHumanReviewStep(workspaceID, "sample", taskHuman.ID) {
		t.Fatal("expected isTaskAtHumanReviewStep to return true for human_review step")
	}

	// Advance to dev step
	_, _ = wfStore.CompleteAndAdvance("sample", taskHuman.ID, "ok", "", map[string]string{"decision": "approve"}, "completed")
	if s.isTaskAtHumanReviewStep(workspaceID, "sample", taskHuman.ID) {
		t.Fatal("expected isTaskAtHumanReviewStep to return false for agent_task step")
	}
}

func TestCommitAndPushReviewChanges(t *testing.T) {
	tmpDir := t.TempDir()
	gitInit := exec.Command("git", "init", "-b", "main")
	gitInit.Dir = tmpDir
	if err := gitInit.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	gitConfigEmail := exec.Command("git", "config", "user.email", "test@example.com")
	gitConfigEmail.Dir = tmpDir
	_ = gitConfigEmail.Run()
	gitConfigName := exec.Command("git", "config", "user.name", "Test Runner")
	gitConfigName.Dir = tmpDir
	_ = gitConfigName.Run()

	// Initial commit
	readme := filepath.Join(tmpDir, "README.md")
	_ = os.WriteFile(readme, []byte("# Test"), 0644)
	gitAdd := exec.Command("git", "add", "README.md")
	gitAdd.Dir = tmpDir
	_ = gitAdd.Run()
	gitCommit := exec.Command("git", "commit", "-m", "initial commit")
	gitCommit.Dir = tmpDir
	_ = gitCommit.Run()

	s, _ := newConnectionGrantPolicyServer(t)
	task := &entity.Task{
		ID:          "t-preview-fix",
		BranchName:  "main",
		WorktreeDir: tmpDir,
	}

	// 1. Clean workspace -> nothing committed
	s.commitAndPushReviewChanges("sample", "pm", task)
	logCmd := exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = tmpDir
	out, _ := logCmd.Output()
	if !strings.Contains(string(out), "initial commit") {
		t.Fatalf("expected no new commit on clean workspace, got %s", string(out))
	}

	// 2. Modified workspace from Preview Copilot -> automatically committed
	_ = os.WriteFile(filepath.Join(tmpDir, "copilot_fix.txt"), []byte("reviewed and fixed"), 0644)
	s.commitAndPushReviewChanges("sample", "pm", task)

	logCmd = exec.Command("git", "log", "-n", "1", "--oneline")
	logCmd.Dir = tmpDir
	out, _ = logCmd.Output()
	if !strings.Contains(string(out), "user in-context preview feedback fixes") {
		t.Fatalf("expected commit created with review message, got %s", string(out))
	}
}

func TestRecoverablePendingAttentionWakeupTargetsExcludesActiveWorkflowTasks(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedTaskAttentionWorker(t, s, workspaceID, "sample", "pm", true)
	now := time.Now().UTC()

	task := &entity.Task{
		ID:        "t-active-workflow-task",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.ts.AddTask("sample", "pm", task)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID:          "wf-active-test",
		Name:        "Active Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "step1",
		Steps: []entity.WorkflowStep{{
			ID:       "step1",
			Type:     "agent_task",
			Title:    "Step 1",
			Position: entity.WorkflowPosition{X: 0, Y: 0},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("save def: %v", err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, nil); err != nil {
		t.Fatalf("start run: %v", err)
	}

	// Add an attention signal referencing this active workflow task
	if err := s.controlDB.UpsertAttentionSignal(controldb.AttentionSignal{
		ID:            "sig-active-workflow-task",
		WorkspaceID:   workspaceID,
		AgentWorkerID: "aw-pm",
		DedupeKey:     "test:active-wf-task",
		SourceKind:    "task",
		SourceID:      task.ID,
		Reason:        "task_assigned",
		Summary:       "Task assigned signal",
		Status:        "pending",
		RefsJSON:      fmt.Sprintf(`{"project":"sample","agent":"pm","taskId":%q}`, task.ID),
		CreatedAt:     now.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("upsert attention: %v", err)
	}

	targets, err := s.recoverablePendingAttentionWakeupTargets(100)
	if err != nil {
		t.Fatalf("recoverable targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected active workflow task signal to be excluded from wakeup recovery targets, got: %+v", targets)
	}
}

func TestEnrichQARejectionComments(t *testing.T) {
	if !isRejectionDecision("reject") || !isRejectionDecision("request_changes") || !isRejectionDecision("rework") {
		t.Fatal("expected isRejectionDecision to be true for rejection synonyms")
	}
	if isRejectionDecision("approve") || isRejectionDecision("pass") {
		t.Fatal("expected isRejectionDecision to be false for approvals")
	}

	// Case 1: Outputs has matrix with failed/blocked items, empty comments
	matrixJSON := `[
		{"item_id":"AUTH-01","risk_level":"high","acceptance_criteria":"API 签名认证校验","status":"failed","evidence":"401 Unauthorized expected, got 200"},
		{"item_id":"PERF-02","risk_level":"critical","acceptance_criteria":"高并发探针","status":"blocked"},
		{"item_id":"UI-03","risk_level":"low","acceptance_criteria":"暗黑模式切换","status":"passed"}
	]`
	outputs := map[string]string{
		"decision":             "request_changes",
		"risk_coverage_matrix": matrixJSON,
	}
	currentStep := entity.WorkflowStep{
		ID:    "qa_signoff",
		Title: "QA 准出签核",
	}

	enrichQARejectionComments(outputs, currentStep, entity.WorkflowRun{}, nil)

	comments := outputs["comments"]
	if !strings.Contains(comments, "【QA 准出未通过/阻断项清单】") {
		t.Fatalf("expected header in comments, got: %s", comments)
	}
	if !strings.Contains(comments, "[HIGH] AUTH-01") || !strings.Contains(comments, "401 Unauthorized") {
		t.Fatalf("expected AUTH-01 in comments, got: %s", comments)
	}
	if !strings.Contains(comments, "[CRITICAL] PERF-02") {
		t.Fatalf("expected PERF-02 in comments, got: %s", comments)
	}
	if strings.Contains(comments, "UI-03") {
		t.Fatalf("passed item UI-03 should not be in rejection comments, got: %s", comments)
	}

	// Case 2: User provided existing comments, should append block
	outputs2 := map[string]string{
		"comments":             "请重点排查鉴权模块。",
		"risk_coverage_matrix": matrixJSON,
	}
	enrichQARejectionComments(outputs2, currentStep, entity.WorkflowRun{}, nil)
	if !strings.HasPrefix(outputs2["comments"], "请重点排查鉴权模块。\n\n【QA 准出未通过/阻断项清单】") {
		t.Fatalf("expected prepended custom comments, got: %s", outputs2["comments"])
	}

	// Case 3: Empty matrix, fallback comments
	outputs3 := map[string]string{
		"risk_coverage_matrix": `[{"item_id":"UI-01","risk_level":"low","status":"passed"}]`,
	}
	enrichQARejectionComments(outputs3, currentStep, entity.WorkflowRun{}, nil)
	if !strings.Contains(outputs3["comments"], "QA 准出审核未通过") {
		t.Fatalf("expected default fallback comment, got: %s", outputs3["comments"])
	}
}

func TestQARejectionWorkflow_EndToEnd(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)
	now := time.Now().UTC()

	taskID := "t-qa-e2e"
	task := &entity.Task{
		ID:        taskID,
		Title:     "QA E2E Task",
		Priority:  2,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID:          "wf-qa-e2e-def",
		Name:        "QA E2E Workflow",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "qa_signoff",
		Steps: []entity.WorkflowStep{
			{
				ID:    "qa_signoff",
				Type:  "human_review",
				Title: "QA 准出",
				OutputFields: []entity.WorkflowField{
					{Name: "decision"},
					{Name: "comments"},
					{Name: "risk_coverage_matrix"},
				},
			},
			{
				ID:    "implement",
				Type:  "agent_task",
				Title: "研发实现",
				InputFields: []entity.WorkflowField{
					{Name: "review_comments"},
				},
			},
		},
		Edges: []entity.WorkflowEdge{
			{
				ID:   "e-qa-rework",
				From: "qa_signoff",
				To:   "implement",
				Condition: &entity.WorkflowEdgeCondition{
					Field:    "decision",
					Operator: "eq",
					Value:    "request_changes",
				},
				InputMapping: map[string]string{
					"review_comments": "$output.comments",
				},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("save def: %v", err)
	}

	run, _, err := wfStore.StartRun("sample", taskID, def.ID, nil)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	matrixJSON := `[
		{"item_id":"SEC-01","risk_level":"critical","acceptance_criteria":"OAuth2 token verification","status":"failed","evidence":"403 returned"},
		{"item_id":"UI-01","risk_level":"low","acceptance_criteria":"Dashboard button color","status":"passed"}
	]`

	// Populate step instance input for qa_signoff
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, inst := range instances {
		if inst.StepID == "qa_signoff" {
			inst.InputValues = map[string]string{
				"risk_coverage_matrix": matrixJSON,
			}
			if err := wfStore.SaveStepInstance(&inst); err != nil {
				t.Fatalf("save step instance: %v", err)
			}
		}
	}

	// Submit review with empty comments and request_changes
	req, _ := http.NewRequest("POST", "/test", nil)
	body := workflowReviewBody{
		Decision: "request_changes",
		Comments: "", // intentionally empty
	}

	_, status, err := s.submitTaskWorkflowReview(req, workspaceID, "sample", taskID, body)
	if err != nil {
		t.Fatalf("submitTaskWorkflowReview error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}

	// Verify that the run transitioned to 'implement'
	runAfter, found, err := wfStore.RunForTask("sample", taskID)
	if err != nil || !found {
		t.Fatalf("run not found after review: %v", err)
	}
	if runAfter.ActiveStepID != "implement" {
		t.Fatalf("expected active step 'implement', got %q", runAfter.ActiveStepID)
	}

	// Verify implement step instance received review_comments containing the formatted failed checklist
	instancesAfter, err := wfStore.ListStepInstances(runAfter.ID)
	if err != nil {
		t.Fatalf("list instances after: %v", err)
	}
	var implementInst *entity.WorkflowStepInstance
	for i := range instancesAfter {
		if instancesAfter[i].StepID == "implement" {
			implementInst = &instancesAfter[i]
			break
		}
	}
	if implementInst == nil {
		t.Fatalf("expected step instance for 'implement'")
	}

	receivedComments := implementInst.InputValues["review_comments"]
	if !strings.Contains(receivedComments, "【QA 准出未通过/阻断项清单】") {
		t.Fatalf("expected header in received comments, got: %s", receivedComments)
	}
	if !strings.Contains(receivedComments, "[CRITICAL] SEC-01: OAuth2 token verification") {
		t.Fatalf("expected failed item in received comments, got: %s", receivedComments)
	}
	if strings.Contains(receivedComments, "UI-01") {
		t.Fatalf("passed item UI-01 should not be in rework comments, got: %s", receivedComments)
	}
}
