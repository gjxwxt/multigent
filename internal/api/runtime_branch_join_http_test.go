package api

// End-to-end HTTP regression for fix round S2-1 Fix A: the step/complete
// endpoint (the one every `mga task step done` hits) must leave ZERO
// terminal state behind when the branch-join QA gate rejects a branch
// child's completion — task stays in_progress, child run stays active,
// branch instance stays running — and a corrected re-report must succeed,
// while a duplicate re-report must not double-advance the parent.

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
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func newBranchJoinHTTPServer(t *testing.T) (*Server, string) {
	t.Helper()
	db, err := controldb.Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(t.TempDir(), "workspace")
	s := &Server{root: root, controlDB: db, st: store.NewDB(root, db), ts: taskstore.NewDB(root, db),
		users: newUserStore(db), agentDirectory: agentdir.New(db), previewSessions: make(map[string]*previewChatSession)}
	s.triggers = newTriggerManager(root, "", s.ts, s.controlDB)
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		t.Fatalf("workspace id: %v", err)
	}
	if err := s.controlDB.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Test Workspace",
		Slug: "test-workspace",
		Root: root,
	}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := s.controlDB.UpsertWorkspaceMember(workspaceID, "admin", WorkspaceRoleAdmin); err != nil {
		t.Fatalf("admin member: %v", err)
	}
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	seedSampleAgentsForTest(t, s, workspaceID)
	return s, workspaceID
}

func newBranchJoinWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	for p, c := range map[string]string{"server.go": "package main\n", "server_test.go": "package main\n"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "base")
	// Scaffold noise like a real materialized worktree, inside the baseline.
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitworktree.CaptureQABaseline(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}


// mustUploadQABaseline mirrors the production capture flow for fixtures:
// fingerprint the worktree and persist the authoritative baseline into the
// test control plane, then hand the server a lookup override over the same
// DB (equivalent to s.QABaselineLookupAdapter with the fixture workspace).
func mustUploadQABaseline(t *testing.T, s *Server, workspaceID, project, taskID, wt string) gitworktree.QABaselineLookup {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.CaptureQABaselineRecord(project, taskID, wt); err != nil {
		t.Fatalf("upload qa baseline: %v", err)
	}
	return wfStore.QABaselineLookupAdapter()
}

func seedBranchChildRun(t *testing.T, s *Server, workspaceID string) (*entity.Task, string) {
	t.Helper()
	now := time.Now().UTC()
	branchTask := &entity.Task{
		ID: "task-join-child", Title: "WS-2 branch", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		Vars: map[string]string{
			workflowRunIDVar:      "", // parent run ID, stamped below before persist
			workflowStepIDVar:     "parallel",
			workflowBranchIDVar:   "ws_b",
			workflowRootTaskIDVar: "task-join-root",
		},
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-join-child", Name: "Branch child", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID: "start", Type: "agent_task", Title: "Branch work", ActorRole: "pm-agent",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary"}, {Name: "touched_paths"},
			},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	run, _, err := wfStore.StartRun("sample", branchTask.ID, def.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// S2-2 P0-2/P1-1: the precheck + join now resolve the PARENT run
	// (task-join-root) for the worktree key and the branch contract. Seed
	// the parent run + branch instance the production fan-out creates.
	parentDef := &entity.WorkflowDefinition{
		ID: "wf-join-parent", Name: "Join parent", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{{
			ID: "parallel", Type: "parallel_stage", Title: "Parallel",
			Branches: []entity.WorkflowBranch{{
				ID: "ws_b", Title: "WS-2",
				OutputFields: []entity.WorkflowField{
					{Name: "branch_summary"}, {Name: "touched_paths"},
				},
			}},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(parentDef); err != nil {
		t.Fatal(err)
	}
	parentRun, _, err := wfStore.StartRun("sample", "task-join-root", parentDef.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// S2-2 fixture semantics: production fan-out stamps the PARENT run ID
	// on branch tasks (activateParallelWorkflowStep Vars), so the join's
	// RunForTask(project, rootTaskID).ID == runID guard holds. The child
	// run ID is only recorded on the branch instance (ChildRunID).
	branchTask.Vars[workflowRunIDVar] = parentRun.ID
	if err := s.ts.AddTask("sample", "pm", branchTask); err != nil {
		t.Fatal(err)
	}
	if err := wfStore.SaveBranchInstance(&entity.WorkflowBranchInstance{
		RunID: parentRun.ID, StepID: "parallel", BranchID: "ws_b", Status: "running",
		StartedAt: now, UpdatedAt: now,
		ChildTaskID: branchTask.ID, ChildRunID: run.ID,
		OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
	}); err != nil {
		t.Fatal(err)
	}
	return branchTask, run.ID
}

func postBranchStepComplete(t *testing.T, s *Server, workspaceID, taskID string, outputs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"agent":   "pm",
		"status":  "success",
		"summary": "branch done",
		"outputs": outputs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+taskID+"/workflow/step/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      "sample",
		Agent:        "pm",
		Capabilities: []string{"task.use"},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeWorkflowStepComplete(rec, req)
	return rec
}

// TestBranchJoinRejectionLeavesAllStateRetryable: the S2 dogfood failure
// mode, replayed over HTTP. A gate-rejected branch completion must return
// 400 with task still in_progress, child run still active at its step, and
// (below, in the parent fixture) the branch instance still running.
func TestBranchJoinRejectionLeavesAllStateRetryable(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-join-child", wt)
	// S2-2 P0-2: precheck + join both run under the PARENT task key now.
	if err := workflowstore.NewStore(s.controlDB, workspaceID).CaptureQABaselineRecord("sample", "task-join-root", wt); err != nil {
		t.Fatalf("upload parent qa baseline: %v", err)
	}
	task, _ := seedBranchChildRun(t, s, workspaceID)

	// The task's real deliverable (a test-file edit) plus an UNDECLARED
	// business edit — the lying completion from the S2 dogfood.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc Bad() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rejected completion must be 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Zero terminal state: task in_progress.
	fresh, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != entity.TaskStatusInProgress {
		t.Fatalf("rejected task must stay in_progress, got %q", fresh.Status)
	}
	// Child run still active on its step.
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if run.Status != "active" || run.ActiveStepID != "start" {
		t.Fatalf("child run must stay active@start, got %q@%q", run.Status, run.ActiveStepID)
	}

	// Corrected re-report: the agent REVERTS the business edit (a QA
	// checkpoint can never declare server.go — whitelist) and re-reports
	// honestly.
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, task.ID, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("corrected completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	fresh, err = s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("corrected task must be done_success, got %q", fresh.Status)
	}
	run, _, err = workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("child run must be completed after correction, got %q", run.Status)
	}
}

// TestBranchJoinDuplicateReportCannotDoubleAdvance: after the child run is
// terminal, a duplicate step/complete must not re-run the join or advance
// anything — S2-2 (P1-2) it returns the RECORDED result idempotently (200,
// zero writes: identical branch timestamps, no parent re-advance) instead of
// a stale rejection that would wedge a retrying agent.
func TestBranchJoinDuplicateReportCannotDoubleAdvance(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-join-child", wt)
	// S2-2 P0-2: precheck + join both run under the PARENT task key now.
	if err := workflowstore.NewStore(s.controlDB, workspaceID).CaptureQABaselineRecord("sample", "task-join-root", wt); err != nil {
		t.Fatalf("upload parent qa baseline: %v", err)
	}
	task, _ := seedBranchChildRun(t, s, workspaceID)

	// Real delta: one test-file edit, honestly declared.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{"branch_summary": "did things", "touched_paths": "server_test.go"}
	if rec := postBranchStepComplete(t, s, workspaceID, task.ID, outputs); rec.Code != http.StatusOK {
		t.Fatalf("first completion must succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	// Snapshot the recorded branch + runs after the first (consumed)
	// completion, so the duplicate can be proven write-free.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	branchAfterFirst, err := wfStore.BranchInstancesForStep(task.Vars[workflowRunIDVar], "parallel")
	if err != nil {
		t.Fatal(err)
	}
	parentRunAfterFirst, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}

	// Duplicate: idempotent re-report returns the recorded result, never a
	// second join or a stale rejection.
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, outputs)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate completion must return the recorded result idempotently, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Branch  entity.WorkflowBranchInstance `json:"branch"`
		AllDone bool                          `json:"allDone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode duplicate payload: %v", err)
	}
	if payload.Branch.BranchID != "ws_b" || payload.Branch.Status != "completed" {
		t.Fatalf("duplicate must return the recorded branch result, got %+v", payload.Branch)
	}
	if len(branchAfterFirst) != 1 || !payload.Branch.FinishedAt.Equal(branchAfterFirst[0].FinishedAt) {
		t.Fatalf("duplicate must return the recorded (not rewritten) branch result: recorded finishedAt=%v, returned=%v", branchAfterFirst[0].FinishedAt, payload.Branch.FinishedAt)
	}

	stored, err := wfStore.BranchInstancesForStep(task.Vars[workflowRunIDVar], "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Status != "completed" || !stored[0].FinishedAt.Equal(branchAfterFirst[0].FinishedAt) {
		t.Fatalf("duplicate must leave the stored branch instance untouched: first finishedAt=%v, after duplicate=%v", branchAfterFirst[0].FinishedAt, stored[0].FinishedAt)
	}
	childRunAfterDuplicate, _, err := wfStore.RunForTask("sample", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childRunAfterDuplicate.Status != "completed" || childRunAfterDuplicate.ActiveStepID != "" {
		t.Fatalf("duplicate must not change child run state, got %q@%q", childRunAfterDuplicate.Status, childRunAfterDuplicate.ActiveStepID)
	}
	parentRunAfterDuplicate, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}
	if parentRunAfterDuplicate.Status != parentRunAfterFirst.Status || parentRunAfterDuplicate.ActiveStepID != parentRunAfterFirst.ActiveStepID || parentRunAfterDuplicate.UpdatedAt != parentRunAfterFirst.UpdatedAt {
		t.Fatalf("duplicate must not re-advance the parent run: first %q@%q(%s) vs duplicate %q@%q(%s)",
			parentRunAfterFirst.Status, parentRunAfterFirst.ActiveStepID, parentRunAfterFirst.UpdatedAt,
			parentRunAfterDuplicate.Status, parentRunAfterDuplicate.ActiveStepID, parentRunAfterDuplicate.UpdatedAt)
	}
}

// TestBranchJoinPrecheckDoesNotTouchLinearTasks: a linear workflow task
// (no branch vars) with a touched_paths-less completion is untouched by
// the precheck — the CI-ready gate test suite covers the rest of the
// linear surface; this is the branch-var absence short-circuit.
func TestBranchJoinPrecheckDoesNotTouchLinearTasks(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return "" }
	now := time.Now().UTC()
	task := &entity.Task{ID: "task-linear-x", Title: "linear", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-linear-x", Name: "Linear", Version: 1, Scope: "workspace", StartStepID: "only",
		Steps:     []entity.WorkflowStep{{ID: "only", Type: "agent_task", Title: "Only", ActorRole: "pm-agent"}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("linear completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
