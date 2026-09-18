package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/agentdir"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runner"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// TestRuntimeWorkflowSeam_GreenfieldVNextPromptAndStepDone tests the platform-level
// automated seam across Greenfield vNext:
//  1. runner.BuildTaskPrompt renders the milestone contract, expected inputs, and
//     mga instructions for each step.
//  2. Completion executes via the exact HTTP endpoint invoked by `mga task step done`
//     (POST /api/v1/runtime/tasks/{id}/workflow/step/complete).
//  3. The acceptance test design manifest gate enforces structural validity.
//  4. Developer test_implementation_evidence flows into self_review and qa prompts.
//  5. QA rejection with empty comments losslessly enriches qa_rework_items and
//     review_comments with the manifest's expected_result.
//  6. The reworked implementation prompt preserves the original test spec manifest
//     alongside the structured rework items.
func TestRuntimeWorkflowSeam_GreenfieldVNextPromptAndStepDone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MULTIGENT_DATA_DIR", root)

	dbDir := filepath.Join(root, ".multigent")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir .multigent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "agency.yaml"), []byte("name: SeamTestAgency\n"), 0o644); err != nil {
		t.Fatalf("write agency.yaml: %v", err)
	}

	dbPath := filepath.Join(dbDir, "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	workspaceID := "ws-seam-test"
	if err := db.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Seam Workspace",
		Slug: workspaceID,
		Root: root,
	}); err != nil {
		t.Fatalf("upsert workspace: %v", err)
	}

	st := store.NewDB(root, db)
	ts := taskstore.NewDB(root, db)
	users := newUserStore(db)
	s := &Server{
		root:            root,
		controlDB:       db,
		st:              st,
		ts:              ts,
		users:           users,
		agentDirectory:  agentdir.New(db),
		previewSessions: make(map[string]*previewChatSession),
	}

	repoDir := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=seam", "GIT_AUTHOR_EMAIL=seam@example.com",
		"GIT_COMMITTER_NAME=seam", "GIT_COMMITTER_EMAIL=seam@example.com")
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "server.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "tests"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "tests", ".gitkeep"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	if err := s.st.SaveProject("resproj", &entity.Project{Name: "resproj", Repo: repoDir}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{
		ID:          "t-seam-1",
		Title:       "Token Revocation Endpoint",
		Assignee:    "resproj/pm-agent",
		Status:      entity.TaskStatusInProgress,
		Prompt:      "Implement Token Revocation Endpoint with test spec",
		WorktreeDir: repoDir,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Register workers and project memberships for all agent roles in the pipeline
	for _, w := range []struct {
		id   string
		name string
		role string
	}{
		{"aw-pm", "pm-agent", "pm"},
		{"aw-dev", "developer-agent", "developer"},
		{"aw-reviewer", "reviewer-agent", "reviewer"},
		{"aw-qa", "qa-agent", "qa"},
		{"aw-rel", "release-agent", "release"},
	} {
		if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
			ID:          w.id,
			WorkspaceID: workspaceID,
			Name:        w.name,
			Role:        w.role,
		}); err != nil {
			t.Fatalf("upsert agent worker %s: %v", w.name, err)
		}
		if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
			ID:          "pm-" + w.id,
			WorkspaceID: workspaceID,
			ProjectID:   "resproj",
			MemberType:  "agent_worker",
			MemberID:    w.id,
			Role:        w.role,
		}); err != nil {
			t.Fatalf("upsert membership %s: %v", w.name, err)
		}
		tCopy := *task
		tCopy.Assignee = "resproj/" + w.name
		if err := s.ts.AddTask("resproj", w.name, &tCopy); err != nil {
			t.Fatalf("add task for %s: %v", w.name, err)
		}
	}

	for _, u := range []string{"po-owner", "tech-lead", "qa-lead"} {
		if err := s.users.CreateUser(u, "password", RoleMember, "", "", "", "", ""); err != nil {
			t.Fatalf("create user %s: %v", u, err)
		}
		if err := s.controlDB.UpsertWorkspaceMember(workspaceID, u, WorkspaceRoleMember); err != nil {
			t.Fatalf("member %s: %v", u, err)
		}
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Greenfield Seam Test")
	if !ok {
		t.Fatal("greenfield-delivery-pipeline template missing")
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatalf("save definition: %v", err)
	}

	actorBindings := map[string]entity.WorkflowActorBinding{
		"pm-agent":        {Type: "agent", ID: "pm-agent"},
		"product-owner":   {Type: "human", ID: "po-owner"},
		"developer-agent": {Type: "agent", ID: "developer-agent"},
		"reviewer-agent":  {Type: "agent", ID: "reviewer-agent"},
		"owner-engineer":  {Type: "human", ID: "tech-lead"},
		"qa-agent":        {Type: "agent", ID: "qa-agent"},
		"qa-owner":        {Type: "human", ID: "qa-lead"},
		"release-agent":   {Type: "agent", ID: "release-agent"},
	}

	if _, _, err := wfStore.StartRunWithInput("resproj", task.ID, def.ID, actorBindings, map[string]string{
		"request": "Implement Token Revocation Endpoint",
		"context": "Security requirement: token blacklist with TTL",
	}); err != nil {
		t.Fatalf("start run: %v", err)
	}

	postRuntimeStepComplete := func(agent string, outputs map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"agent":   agent,
			"status":  "success",
			"summary": "step completed by agent",
			"outputs": outputs,
		})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+task.ID+"/workflow/step/complete", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", task.ID)
		req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
			WorkspaceID:  workspaceID,
			Project:      "resproj",
			Agent:        agent,
			Capabilities: []string{"task.use"},
		}))
		rec := httptest.NewRecorder()
		s.handleRuntimeWorkflowStepComplete(rec, req)
		return rec
	}

	advanceDirect := func(stepID, summary string, outputs map[string]string) {
		t.Helper()
		if _, err := wfStore.CompleteAndAdvance("resproj", task.ID, summary, "", outputs, "completed"); err != nil {
			t.Fatalf("advance direct %s: %v", stepID, err)
		}
	}

	refreshTask := func(agent string) *entity.Task {
		t.Helper()
		cur, _, err := s.findTaskInProject("resproj", task.ID)
		if err != nil || cur == nil {
			t.Fatalf("refresh task for agent %s: %v", agent, err)
		}
		_ = s.ts.AddTask("resproj", agent, cur)
		return cur
	}

	// ── Fast-forward requirement_draft, requirement_review, design_review ──
	advanceDirect("requirement_draft", "drafted", map[string]string{
		"requirement_draft": "AC-1: POST /api/v1/auth/revoke revokes token; AC-2: 401 on revoked token",
		"open_questions":    "none",
	})
	advanceDirect("requirement_review", "approved", map[string]string{
		"decision":             "approve",
		"comments":             "Scope approved",
		"approved_requirement": "Approved AC-1: revoke token; AC-2: 401 on revoked token",
	})
	advanceDirect("design_review", "design approved", map[string]string{
		"decision":             "approve",
		"comments":             "headless API change",
		"design_waiver_reason": "headless API service, no frontend UI",
		"design_waived":        "true",
	})

	// ── 1. acceptance_test_design Seam Check ────────────────────────────────
	taskQA := refreshTask("qa-agent")
	atdPrompt := runner.New(s.root, s.ts, s.st).BuildTaskPrompt("resproj", "qa-agent", taskQA)

	if !strings.Contains(atdPrompt, "Current step: Acceptance Test Design (`acceptance_test_design`") {
		t.Fatalf("acceptance_test_design prompt missing current step title, got:\n%s", atdPrompt)
	}
	if !strings.Contains(atdPrompt, "Step actor role: `qa-agent`") {
		t.Fatalf("acceptance_test_design prompt missing actor role, got:\n%s", atdPrompt)
	}
	if !strings.Contains(atdPrompt, "test_spec_manifest") || !strings.Contains(atdPrompt, "test_spec_doc") {
		t.Fatalf("acceptance_test_design prompt missing required output fields, got:\n%s", atdPrompt)
	}
	if !strings.Contains(atdPrompt, "mga task step done --id "+task.ID) {
		t.Fatalf("acceptance_test_design prompt missing mga task step done instruction, got:\n%s", atdPrompt)
	}

	// Gate test: invalid manifest must fail HTTP complete with 400 Bad Request
	recInvalid := postRuntimeStepComplete("qa-agent", map[string]string{
		"test_spec_doc":      "doc",
		"test_spec_manifest": `[{"case_id":"","risk_level":"invalid"}]`,
		"test_spec_summary":  "1 invalid case",
	})
	if recInvalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid manifest must return HTTP 400, got %d: %s", recInvalid.Code, recInvalid.Body.String())
	}

	// Valid manifest completes successfully via HTTP
	validManifest := `[
	 {"case_id":"AUTH-001","ac_id":"AC-1","risk_level":"high","automation_level":"api_integration","execution_type":"auto","expected_result":"revoked token returns 401 unauthorized"},
	 {"case_id":"AUTH-002","ac_id":"AC-2","risk_level":"medium","automation_level":"unit","execution_type":"auto","expected_result":"TTL expiration removes token from blacklist"}
	]`
	recValid := postRuntimeStepComplete("qa-agent", map[string]string{
		"test_spec_doc":      "Full acceptance test specification for token revocation",
		"test_spec_manifest": validManifest,
		"test_spec_summary":  "2 cases: 1 high risk, 1 medium risk",
	})
	if recValid.Code != http.StatusOK {
		t.Fatalf("valid manifest complete failed, code %d: %s", recValid.Code, recValid.Body.String())
	}

	// ── 2. implementation Seam Check ────────────────────────────────────────
	taskDev := refreshTask("developer-agent")
	implPrompt := runner.New(s.root, s.ts, s.st).BuildTaskPrompt("resproj", "developer-agent", taskDev)

	if !strings.Contains(implPrompt, "Current step: Implementation (`implementation`") {
		t.Fatalf("implementation prompt missing step title, got:\n%s", implPrompt)
	}
	if !strings.Contains(implPrompt, "AUTH-001") || !strings.Contains(implPrompt, "revoked token returns 401 unauthorized") {
		t.Fatalf("implementation prompt missing structured test_spec_manifest input, got:\n%s", implPrompt)
	}
	if !strings.Contains(implPrompt, "test_implementation_evidence") {
		t.Fatalf("implementation prompt missing required output field test_implementation_evidence, got:\n%s", implPrompt)
	}

	evidenceJSON := `{"AUTH-001":{"test":"TestRevokedTokenRejection","file":"auth_test.go","result":"pass"},"AUTH-002":{"test":"TestTTLExpiration","file":"auth_test.go","result":"pass"}}`
	recImpl := postRuntimeStepComplete("developer-agent", map[string]string{
		"pr":                           "feat(auth): token revocation endpoint and middleware check",
		"tests_run":                    "go test ./... 2/2 PASS",
		"risks":                        "none",
		"test_implementation_evidence": evidenceJSON,
	})
	if recImpl.Code != http.StatusOK {
		t.Fatalf("implementation step complete failed, code %d: %s", recImpl.Code, recImpl.Body.String())
	}

	// ── 3. self_review Seam Check ───────────────────────────────────────────
	taskReviewer := refreshTask("reviewer-agent")
	reviewerPrompt := runner.New(s.root, s.ts, s.st).BuildTaskPrompt("resproj", "reviewer-agent", taskReviewer)

	if !strings.Contains(reviewerPrompt, "Current step: Agent Self Review (`self_review`") {
		t.Fatalf("self_review prompt missing step title, got:\n%s", reviewerPrompt)
	}
	if !strings.Contains(reviewerPrompt, "TestRevokedTokenRejection") {
		t.Fatalf("self_review prompt missing developer test_implementation_evidence input, got:\n%s", reviewerPrompt)
	}
	if !strings.Contains(reviewerPrompt, "AUTH-001") {
		t.Fatalf("self_review prompt missing test_spec_manifest input, got:\n%s", reviewerPrompt)
	}

	recReview := postRuntimeStepComplete("reviewer-agent", map[string]string{
		"self_review_verdict": "pass",
		"review_comments":     "All 2 cases covered with clean tests",
	})
	if recReview.Code != http.StatusOK {
		t.Fatalf("self_review complete failed, code %d: %s", recReview.Code, recReview.Body.String())
	}

	// ── 4. code_review (Human review approves) ──────────────────────────────
	advanceDirect("code_review", "CR approved", map[string]string{
		"decision":        "approve",
		"comments":        "Code structure is good",
		"approved_change": "approved commit",
	})

	// ── 5. qa Seam Check ────────────────────────────────────────────────────
	taskQA2 := refreshTask("qa-agent")
	qaPrompt := runner.New(s.root, s.ts, s.st).BuildTaskPrompt("resproj", "qa-agent", taskQA2)

	if !strings.Contains(qaPrompt, "Current step: QA Test & Risk Matrix (`qa`") {
		t.Fatalf("qa prompt missing step title, got:\n%s", qaPrompt)
	}
	if !strings.Contains(qaPrompt, "TestRevokedTokenRejection") {
		t.Fatalf("qa prompt missing developer test_implementation_evidence input, got:\n%s", qaPrompt)
	}
	if !strings.Contains(qaPrompt, "risk_coverage_matrix") || !strings.Contains(qaPrompt, "touched_paths") {
		t.Fatalf("qa prompt missing required output fields, got:\n%s", qaPrompt)
	}

	// QA identifies a failure on AUTH-001 and outputs the matrix
	qaMatrix := `[
	 {"item_id":"AUTH-001","risk_level":"high","status":"failed","evidence":"reproduced: revoked token returns 200 instead of 401"},
	 {"item_id":"AUTH-002","risk_level":"medium","status":"passed","evidence":"TTL expiry verified"}
	]`

	// QA Checkpoint Gate negative check: touching a business implementation file is rejected
	if err := os.WriteFile(filepath.Join(repoDir, "server.go"), []byte("package main\n// bad edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	recQABad := postRuntimeStepComplete("qa-agent", map[string]string{
		"risk_coverage_matrix": qaMatrix,
		"test_report":          "test report",
		"touched_paths":        "server.go",
	})
	if recQABad.Code != http.StatusBadRequest {
		t.Fatalf("QA touching business code must be rejected with HTTP 400, got %d: %s", recQABad.Code, recQABad.Body.String())
	}
	runGit("checkout", "server.go")

	// Legitimate QA touched file (probe test) passes
	if err := os.MkdirAll(filepath.Join(repoDir, "tests"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "tests", "revocation_probe_test.go"), []byte("package tests\n"), 0644); err != nil {
		t.Fatal(err)
	}
	recQA := postRuntimeStepComplete("qa-agent", map[string]string{
		"risk_coverage_matrix": qaMatrix,
		"test_report":          "AUTH-001 failed in independent probe",
		"touched_paths":        "tests/revocation_probe_test.go",
	})
	if recQA.Code != http.StatusOK {
		t.Fatalf("qa step complete failed, code %d: %s", recQA.Code, recQA.Body.String())
	}

	// ── 6. qa_signoff Rejection & Lossless Rework Backflow ──────────────────
	// Human reviewer rejects via decision endpoint with empty comments
	qaSignoffOutputs := map[string]string{
		"decision": "request_changes",
		"comments": "",
	}
	runState, ok, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !ok {
		t.Fatal("run not found")
	}

	// Server-side enrichment happens before completion
	enrichQARejectionComments(qaSignoffOutputs, entity.WorkflowStep{ID: "qa_signoff", Type: "human_review"}, runState, wfStore)

	// Verify that comments were automatically enriched with the failed item
	if !strings.Contains(qaSignoffOutputs["comments"], "【QA 准出未通过/阻断项清单】") ||
		!strings.Contains(qaSignoffOutputs["comments"], "AUTH-001") {
		t.Fatalf("comments must contain aggregated failed items, got: %s", qaSignoffOutputs["comments"])
	}

	// Verify structured rework items were created and enriched with expected_result from manifest
	reworkItemsJSON := qaSignoffOutputs["qa_rework_items"]
	if !strings.Contains(reworkItemsJSON, `"item_id":"AUTH-001"`) ||
		!strings.Contains(reworkItemsJSON, "revoked token returns 401 unauthorized") {
		t.Fatalf("qa_rework_items missing manifest expected_result enrichment: %s", reworkItemsJSON)
	}

	// Advance through qa_signoff rework
	if _, err := wfStore.CompleteAndAdvance("resproj", task.ID, "qa rejected", "", qaSignoffOutputs, "completed"); err != nil {
		t.Fatalf("qa_signoff advance: %v", err)
	}

	// ── 7. Reworked implementation Prompt Seam Check ────────────────────────
	taskDevRework := refreshTask("developer-agent")
	reworkPrompt := runner.New(s.root, s.ts, s.st).BuildTaskPrompt("resproj", "developer-agent", taskDevRework)

	if !strings.Contains(reworkPrompt, "Current step: Implementation (`implementation`") {
		t.Fatalf("reworked implementation prompt missing step title, got:\n%s", reworkPrompt)
	}
	// Assert prompt carries BOTH the structured rework items and the original spec manifest
	if !strings.Contains(reworkPrompt, "qa_rework_items") || !strings.Contains(reworkPrompt, "AUTH-001") {
		t.Fatalf("reworked implementation prompt missing qa_rework_items, got:\n%s", reworkPrompt)
	}
	if !strings.Contains(reworkPrompt, "【QA 准出未通过/阻断项清单】") {
		t.Fatalf("reworked implementation prompt missing enriched review_comments, got:\n%s", reworkPrompt)
	}
	if !strings.Contains(reworkPrompt, "test_spec_manifest") || !strings.Contains(reworkPrompt, "revoked token returns 401 unauthorized") {
		t.Fatalf("reworked implementation prompt lost the original test_spec_manifest baseline, got:\n%s", reworkPrompt)
	}

	_ = time.Now()
}
