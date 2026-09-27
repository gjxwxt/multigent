package api

// Delivery SHA hand-off (Route A, option B): the platform's git delivery
// gate — which PROVES remote tip == local HEAD at formal completion — must
// stamp the proven full 40-hex SHA into the completing step's outputs as
// delivery_sha/delivery_branch, always overwriting any agent-provided
// values. The proven SHA is the ONLY sanctioned QA code anchor; free-text
// pr/approved_change must never be the machine anchor.

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

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runner"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// gitOut runs a git command in dir and RETURNS the trimmed output (fails
// the test on error). Complements the asserting-only gitRun helper.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func postLinearStepComplete(t *testing.T, s *Server, workspaceID, taskID, agent string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+taskID+"/workflow/step/complete", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      "sample",
		Agent:        agent,
		Capabilities: []string{"task.use"},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeWorkflowStepComplete(rec, req)
	return rec
}

// seedLinearDeliveryRun seeds a task + run whose CURRENT step is an agent
// step declaring the `pr` output (implementation-like), with the delivery
// contract var on the task. The next step (qa) consumes delivery_sha via
// the edge mapping, mirroring the greenfield implementation→qa chain.
func seedLinearDeliveryRun(t *testing.T, s *Server, workspaceID, taskID, baseCommit string, withContract bool) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-delivery-linear", Name: "Linear delivery", Version: 1, Scope: "workspace", StartStepID: "impl",
		Steps: []entity.WorkflowStep{
			{
				ID: "impl", Type: "agent_task", Title: "Implement", ActorRole: "pm-agent",
				InputFields:  []entity.WorkflowField{{Name: "request"}},
				OutputFields: []entity.WorkflowField{{Name: "pr"}, {Name: "tests_run"}},
			},
			{
				ID: "qa", Type: "agent_task", Title: "QA", ActorRole: "qa-agent",
				InputFields:  []entity.WorkflowField{{Name: "pr"}, {Name: "delivery_sha"}},
				OutputFields: []entity.WorkflowField{{Name: "test_report"}},
			},
		},
		Edges:     []entity.WorkflowEdge{{From: "impl", To: "qa", InputMapping: map[string]string{"pr": "$output.pr", "delivery_sha": "$output.delivery_sha"}}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	if withContract {
		vars[runner.DeliveryContractVar] = `{"requireGitCommit":true,"requirePush":true}`
	}
	task := &entity.Task{
		ID: taskID, Title: "Linear delivery " + taskID, Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main", BranchName: "task/x", Vars: vars,
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", taskID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent":       {Type: "agent", ID: "pm"},
		"qa-agent":       {Type: "agent", ID: "pm"},
		"owner-engineer": {Type: "human", ID: "admin"},
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

func stepOutput(t *testing.T, s *Server, workspaceID, taskID, stepID, key string) (string, bool) {
	t.Helper()
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", taskID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	insts, err := workflowstore.NewStore(s.controlDB, workspaceID).ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range insts {
		if inst.StepID == stepID {
			v, ok := inst.OutputValues[key]
			return v, ok
		}
	}
	t.Fatalf("step instance %q not found for task %s", stepID, taskID)
	return "", false
}

// TestLinearStepDeliveryGateStampsProvenSHA: an implementation completion on
// a contracted task must (a) run the git gate BEFORE persisting, (b) stamp
// the proven 40-hex SHA into outputs, and (c) OVERWRITE an agent-forged
// delivery_sha with the machine value.
func TestLinearStepDeliveryGateStampsProvenSHA(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-linear", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	task := seedLinearDeliveryRun(t, s, workspaceID, "task-delivery-linear", base, true)

	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remote, "init", "--bare", "-b", "main")
	gitRun(t, wt, "remote", "add", "origin", remote)
	gitRun(t, wt, "push", "origin", "main")

	gitRun(t, wt, "checkout", "-b", "task/x")
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc D() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "delivery")
	gitRun(t, wt, "push", "origin", "task/x")
	pushed := gitOut(t, wt, "rev-parse", "HEAD")

	// The agent tries to forge the anchor with a SHORT fake SHA; the gate
	// must overwrite it with the machine-proven value.
	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered",
		"outputs": map[string]string{
			"pr":           "branch:task/x (commit abc1234)",
			"tests_run":    "go test ./...",
			"delivery_sha": "abc1234",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("completion rejected: %d %s", rec.Code, rec.Body.String())
	}

	got, ok := stepOutput(t, s, workspaceID, task.ID, "impl", "delivery_sha")
	if !ok || got != pushed {
		t.Fatalf("delivery_sha must be the proven full SHA %q (present=%v), got %q", pushed, ok, got)
	}
	if got, _ := stepOutput(t, s, workspaceID, task.ID, "impl", "delivery_branch"); got != "task/x" {
		t.Fatalf("delivery_branch must be the proven branch, got %q", got)
	}
}

// TestLinearStepDeliveryGateFailsClosed: a contracted implementation whose
// delivery is NOT pushed must be rejected with 400 BEFORE persistence —
// task stays in_progress, step still active, no delivery_sha anywhere.
func TestLinearStepDeliveryGateFailsClosed(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-unpushed", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	task := seedLinearDeliveryRun(t, s, workspaceID, "task-delivery-unpushed", base, true)

	// Commit locally on task/x but DO NOT push (no origin configured).
	gitRun(t, wt, "checkout", "-b", "task/x")
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc D() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "unpushed delivery")

	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered",
		"outputs": map[string]string{
			"pr":           "branch:task/x (unpushed)",
			"tests_run":    "go test ./...",
			"delivery_sha": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unpushed delivery must fail closed with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery contract unmet") {
		t.Fatalf("rejection must carry the contract violation, got %s", rec.Body.String())
	}
	// Nothing persisted: task still in_progress, step still active, no
	// delivery_sha recorded.
	got, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != entity.TaskStatusInProgress {
		t.Fatalf("task must stay in_progress after gate rejection, got %s", got.Status)
	}
	// The step instance was never terminal-transitioned: the run must still
	// sit at the impl step.
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if run.ActiveStepID != "impl" {
		t.Fatalf("run must remain at the impl step after rejection, got %q", run.ActiveStepID)
	}
}

// TestLinearStepNoContractVarLegacyUnchanged: the same completion without
// the delivery contract var must NOT be gated nor stamped — legacy flows
// keep their exact semantics (agent values pass through untouched).
func TestLinearStepNoContractVarLegacyUnchanged(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-legacy", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	task := seedLinearDeliveryRun(t, s, workspaceID, "task-delivery-legacy", base, false)

	// No delivery at all — nothing committed, nothing pushed — and a forged
	// delivery_sha that must survive untouched because there is no contract
	// to gate on.
	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "no delivery",
		"outputs": map[string]string{
			"pr":           "branch:task/x (not even committed)",
			"tests_run":    "none",
			"delivery_sha": "agent-forged",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy completion must not be gated, got %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := stepOutput(t, s, workspaceID, task.ID, "impl", "delivery_sha"); got != "agent-forged" {
		t.Fatalf("legacy outputs must pass through untouched, got delivery_sha=%q", got)
	}
}

// TestLinearStepNonDeliveryStepNotGated: a contracted task whose CURRENT
// step does NOT declare `pr` (review/QA-style step) must complete without
// the git gate — the declaration-as-opt-in contract.
func TestLinearStepNonDeliveryStepNotGated(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-review", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	task := seedLinearDeliveryRun(t, s, workspaceID, "task-delivery-review", base, true)

	// Complete implementation with a real pushed delivery so the run
	// advances to the qa step (which declares no `pr`).
	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remote, "init", "--bare", "-b", "main")
	gitRun(t, wt, "remote", "add", "origin", remote)
	gitRun(t, wt, "push", "origin", "main")
	gitRun(t, wt, "checkout", "-b", "task/x")
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc D() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "delivery")
	gitRun(t, wt, "push", "origin", "task/x")

	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered",
		"outputs": map[string]string{"pr": "branch:task/x", "tests_run": "ok"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("implementation completion rejected: %d %s", rec.Code, rec.Body.String())
	}

	// The run is now at the qa step (declares no `pr`). The reporting agent
	// holds the task in its own queue (fixture uses one agent), and the
	// completion must succeed even though nothing was committed or pushed
	// for the qa step itself — a non-delivery step is never git-gated.
	rec = postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "qa done",
		"outputs": map[string]string{"test_report": "all green"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("non-delivery step must not be gated, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestLinearGateFeedsQAAnchorThroughEdge: the fresh machine evidence stamped
// at implementation completion must arrive as the qa step's INPUT via the
// edge mapping — the end-to-end hand-off the greenfield template encodes.
func TestLinearGateFeedsQAAnchorThroughEdge(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-edge", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	task := seedLinearDeliveryRun(t, s, workspaceID, "task-delivery-edge", base, true)

	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remote, "init", "--bare", "-b", "main")
	gitRun(t, wt, "remote", "add", "origin", remote)
	gitRun(t, wt, "push", "origin", "main")
	gitRun(t, wt, "checkout", "-b", "task/x")
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc D() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "delivery")
	gitRun(t, wt, "push", "origin", "task/x")
	pushed := gitOut(t, wt, "rev-parse", "HEAD")

	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered",
		"outputs": map[string]string{"pr": "branch:task/x", "tests_run": "ok"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("implementation completion rejected: %d %s", rec.Code, rec.Body.String())
	}

	// The qa step instance must have been created with delivery_sha as an
	// INPUT value resolved from the implementation outputs.
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	insts, err := workflowstore.NewStore(s.controlDB, workspaceID).ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range insts {
		if inst.StepID != "qa" {
			continue
		}
		if got := inst.InputValues["delivery_sha"]; got != pushed {
			t.Fatalf("qa step input delivery_sha must be the proven SHA %q, got %q", pushed, got)
		}
		return
	}
	t.Fatal("qa step instance not found after implementation completion")
}

// TestBranchCompletionStampsProvenSHA: the fan-out branch path stamps the
// proven pair too — first-wave evidence for the aggregated parallel outputs,
// and a forged agent delivery_sha is overwritten by the machine value.
func TestBranchCompletionStampsProvenSHA(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutGitRemote(t, s, gitRoot)

	parentTask := seedFanoutParentRun(t, s, workspaceID, baseCommit)
	parentTask.Vars["MULTIGENT_DELIVERY_CONTRACT"] = `{"requireGitCommit":true,"requirePush":true}`
	if err := s.ts.PersistTask("sample", "pm", parentTask); err != nil {
		t.Fatal(err)
	}

	// Parent start completion fans out the branches.
	rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("parent start completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	childIDs := fanoutBranchChildIDs(t, s, workspaceID, "task-fanout-root")
	honestID := childIDs["ws_b"]
	honestTask, err := s.ts.GetTask("sample", "pm", honestID)
	if err != nil {
		t.Fatal(err)
	}

	// Honest delivery: commit + push on the child's own branch.
	if err := os.WriteFile(filepath.Join(honestTask.WorktreeDir, "module_ws_b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, honestTask.WorktreeDir, "add", ".")
	gitRun(t, honestTask.WorktreeDir, "commit", "-m", "ws_b delivery")
	gitRun(t, honestTask.WorktreeDir, "push", "origin", honestTask.BranchName)
	pushed := gitRunOut(t, honestTask.WorktreeDir, "rev-parse", "HEAD")

	// The agent forges a short delivery_sha; the branch gate must overwrite
	// it with the proven full SHA.
	rec = postBranchStepComplete(t, s, workspaceID, honestID, map[string]string{
		"branch_summary": "delivered and pushed",
		"touched_paths":  "module_ws_b.go",
		"delivery_sha":   "forged",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("honest branch must complete, got %d: %s", rec.Code, rec.Body.String())
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil || !found {
		t.Fatalf("parent run lookup: found=%v err=%v", found, err)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.ChildTaskID != honestID {
			continue
		}
		if got := inst.OutputValues["delivery_sha"]; got != pushed {
			t.Fatalf("branch instance delivery_sha must be the proven SHA %q, got %q", pushed, got)
		}
		if got := inst.OutputValues["delivery_branch"]; got != honestTask.BranchName {
			t.Fatalf("branch instance delivery_branch must be %q, got %q", honestTask.BranchName, got)
		}
		return
	}
	t.Fatalf("branch instance for child %s not found", honestID)
}

// gitRunOut runs git and RETURNS the trimmed output (gitRun only asserts).
func gitRunOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitOut(t, dir, args...)
}

// TestWorkflowStepDeclaresOutputGuard (reverse validation, opt-in guard):
// the declaration lookup itself — a `pr`-declaring step is gated, a
// review/QA step is not. Breaking the lookup (e.g. matching any field)
// would wrongly git-gate spec/review/QA steps on contracted tasks.
func TestWorkflowStepDeclaresOutputGuard(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-guard", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	task := seedLinearDeliveryRun(t, s, workspaceID, "task-delivery-guard", base, true)

	// Current step is impl (declares pr): the guard must opt the gate IN.
	got, err := s.workflowStepDeclaresOutput(workspaceID, "sample", task, "pr")
	if err != nil || !got {
		t.Fatalf("impl step declares pr: got=%v err=%v", got, err)
	}
	// An output the step does not declare must not opt in.
	got, err = s.workflowStepDeclaresOutput(workspaceID, "sample", task, "test_report")
	if err != nil || got {
		t.Fatalf("impl step does not declare test_report: got=%v err=%v", got, err)
	}
	// A task without a workflow must error (fail-visible), not silently
	// opt out.
	bare := &entity.Task{ID: "task-no-workflow", Vars: map[string]string{}}
	if _, err := s.workflowStepDeclaresOutput(workspaceID, "sample", bare, "pr"); err == nil {
		t.Fatal("a task without a workflow must surface an error, not silently skip the gate")
	}
}

// TestReworkReprovesFreshSHA (anti-staleness end-to-end): after a rework
// round, the QA-facing anchor must be the SECOND delivery's proven SHA —
// the first delivery's evidence must not survive anywhere on the path to
// qa. Fixes the blind-review P2: no direct regression guarded the
// title-level freshness guarantee.
func TestReworkReprovesFreshSHA(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-rework", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")

	// Three-step linear mirror of the greenfield chain:
	// impl -> self_review (rework loop back to impl) -> qa.
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-delivery-rework", Name: "Rework delivery", Version: 1, Scope: "workspace", StartStepID: "impl",
		Steps: []entity.WorkflowStep{
			{
				ID: "impl", Type: "agent_task", Title: "Implement", ActorRole: "pm-agent",
				InputFields:  []entity.WorkflowField{{Name: "request"}, {Name: "review_comments"}},
				OutputFields: []entity.WorkflowField{{Name: "pr"}, {Name: "self_verdict"}},
			},
			{
				ID: "review", Type: "agent_task", Title: "Self review", ActorRole: "qa-agent",
				InputFields:  []entity.WorkflowField{{Name: "pr"}, {Name: "delivery_sha"}},
				OutputFields: []entity.WorkflowField{{Name: "verdict"}, {Name: "review_comments"}},
			},
			{
				ID: "qa", Type: "agent_task", Title: "QA", ActorRole: "owner-engineer",
				InputFields:  []entity.WorkflowField{{Name: "pr"}, {Name: "delivery_sha"}},
				OutputFields: []entity.WorkflowField{{Name: "test_report"}},
			},
		},
		Edges: []entity.WorkflowEdge{
			{From: "impl", To: "review", InputMapping: map[string]string{"pr": "$output.pr", "delivery_sha": "$output.delivery_sha"}},
			// Rework edge: deliberately NO delivery_sha mapping (stale anchor).
			{From: "review", To: "impl", Condition: &entity.WorkflowEdgeCondition{Field: "verdict", Operator: "eq", Value: "rework"}, InputMapping: map[string]string{"review_comments": "$output.review_comments", "pr": "$input.pr"}},
			{From: "review", To: "qa", InputMapping: map[string]string{"pr": "$input.pr", "delivery_sha": "$input.delivery_sha"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := &entity.Task{
		ID: "task-delivery-rework", Title: "Rework delivery", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: base, BaseBranch: "main", BranchName: "task/x",
		Vars: map[string]string{runner.DeliveryContractVar: `{"requireGitCommit":true,"requirePush":true}`},
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent":       {Type: "agent", ID: "pm"},
		"qa-agent":       {Type: "agent", ID: "pm"},
		"owner-engineer": {Type: "agent", ID: "pm"},
	}); err != nil {
		t.Fatal(err)
	}

	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remote, "init", "--bare", "-b", "main")
	gitRun(t, wt, "remote", "add", "origin", remote)
	gitRun(t, wt, "push", "origin", "main")
	gitRun(t, wt, "checkout", "-b", "task/x")

	deliver := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\n// "+msg+"\nfunc D() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, wt, "add", ".")
		gitRun(t, wt, "commit", "-m", msg)
		gitRun(t, wt, "push", "origin", "task/x")
		return gitOut(t, wt, "rev-parse", "HEAD")
	}

	// Delivery #1 and its completion — the first (soon stale) anchor.
	first := deliver("delivery one")
	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered one",
		"outputs": map[string]string{"pr": "branch:task/x", "self_verdict": "done"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("impl completion #1 rejected: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := stepOutput(t, s, workspaceID, task.ID, "impl", "delivery_sha"); got != first {
		t.Fatalf("impl #1 evidence must be the first SHA %q, got %q", first, got)
	}

	// Self review demands rework — the stale first anchor must NOT ride
	// back to impl.
	rec = postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "rework needed",
		"outputs": map[string]string{"verdict": "rework", "review_comments": "fix it"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("review rework completion rejected: %d %s", rec.Code, rec.Body.String())
	}

	// Delivery #2: a NEW commit, pushed. The impl gate re-runs and the
	// second proven SHA must overwrite the first on the path to qa.
	second := deliver("delivery two (rework)")
	rec = postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered two",
		"outputs": map[string]string{"pr": "branch:task/x", "self_verdict": "done"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("impl completion #2 rejected: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := stepOutput(t, s, workspaceID, task.ID, "impl", "delivery_sha"); got != second {
		t.Fatalf("impl #2 evidence must be the FRESH SHA %q, got %q (stale %q must be gone)", second, got, first)
	}

	// Self review passes this time; the threaded anchor into qa must be the
	// SECOND SHA, never the first.
	rec = postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "review pass",
		"outputs": map[string]string{"verdict": "pass", "review_comments": "ok"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("review pass completion rejected: %d %s", rec.Code, rec.Body.String())
	}
	run, found, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	insts, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range insts {
		if inst.StepID != "qa" {
			continue
		}
		if got := inst.InputValues["delivery_sha"]; got != second {
			t.Fatalf("qa anchor must be the fresh SHA %q after rework, got %q (stale %q leaked)", second, got, first)
		}
		return
	}
	t.Fatal("qa step instance not found after rework loop")
}
