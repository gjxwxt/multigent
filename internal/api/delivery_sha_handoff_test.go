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
	"github.com/multigent/multigent/internal/tasktemplate"
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
// seedAnchorPromisedRun seeds the same linear fixture but with the qa step
// REQUIRING delivery_sha — the greenfield-promise shape. Used to prove the
// fail-closed behavior for runs that carry the promise without a contract.
func seedAnchorPromisedRun(t *testing.T, s *Server, workspaceID, taskID, baseCommit string, withContract bool) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-delivery-anchored-" + taskID, Name: "Anchored delivery " + taskID, Version: 1, Scope: "workspace", StartStepID: "impl",
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
		ID: taskID, Title: "Anchored delivery " + taskID, Status: entity.TaskStatusInProgress,
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
				// delivery_sha is CONTRACT-VISIBLE but OPTIONAL: this fixture
				// is the pre-promise shape — the anchor is threaded when
				// present, but the definition never REQUIRES it, so runs
				// without a contract keep legacy passthrough semantics. The
				// REQUIRED (promised) shape is covered by the greenfield
				// template tests and TestUnseededAnchorPromiseFailsClosed.
				InputFields:  []entity.WorkflowField{{Name: "pr"}, {Name: "delivery_sha", Optional: true}},
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
	task := seedAnchorPromisedRun(t, s, workspaceID, "task-delivery-guard", base, true)

	// Current step is impl (declares pr): the guard must opt the gate IN.
	declaresPR, qaAnchor, err := s.workflowDeliveryGateShape(workspaceID, "sample", task)
	if err != nil || !declaresPR {
		t.Fatalf("impl step declares pr: got=%v err=%v", declaresPR, err)
	}
	// The fixture definition's qa step REQUIRES delivery_sha — the shape
	// probe must see the anchor promise (this is what makes forged SHAs
	// fail-closed on unseeded tasks).
	if !qaAnchor {
		t.Fatal("definition qa step requires delivery_sha: the shape probe must report the anchor promise")
	}
	// A task without a workflow must error (fail-visible), not silently
	// opt out.
	bare := &entity.Task{ID: "task-no-workflow", Vars: map[string]string{}}
	if _, _, err := s.workflowDeliveryGateShape(workspaceID, "sample", bare); err == nil {
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

// TestUnseededAnchorPromiseFailsClosed (blind-review blocker, reverse
// side): the definition's qa REQUIRES delivery_sha but the task carries NO
// delivery contract (an in-flight run instantiated before auto-seeding
// deployed). A pr-declaring completion with an agent-forged SHA must be
// REJECTED — not waved through with the forgery riding to QA.
func TestUnseededAnchorPromiseFailsClosed(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-unseeded", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	// withContract=false: the in-flight run has no contract var.
	task := seedAnchorPromisedRun(t, s, workspaceID, "task-delivery-unseeded", base, false)

	// A real pushed delivery would satisfy any gate, but there is no
	// contract to run a gate with: the anchor promise is unbacked.
	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "delivered (allegedly)",
		"outputs": map[string]string{
			"pr":           "branch:task/x (commit feedface)",
			"tests_run":    "ok",
			"delivery_sha": "feedfacefeedfacefeedfacefeedfacefeedface",
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("anchor promise without a delivery contract must fail closed with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MULTIGENT_DELIVERY_CONTRACT") {
		t.Fatalf("rejection must name the missing contract seed, got %s", rec.Body.String())
	}
	// Nothing persisted: the run stays at impl, no delivery_sha anywhere.
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if run.ActiveStepID != "impl" {
		t.Fatalf("run must remain at impl after fail-closed rejection, got %q", run.ActiveStepID)
	}
}

// TestLegacyDefinitionsUnaffected: a definition whose qa does NOT require
// delivery_sha (legacy shape) completes a pr-declaring step WITHOUT a
// contract exactly as before — the fail-closed promise is scoped to
// definitions that actually promise the anchor.
func TestLegacyDefinitionsUnaffected(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-legacy-shape", wt)

	// Legacy-shaped definition: qa declares NO delivery_sha input.
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-delivery-legacy-shape", Name: "Legacy shape", Version: 1, Scope: "workspace", StartStepID: "impl",
		Steps: []entity.WorkflowStep{
			{
				ID: "impl", Type: "agent_task", Title: "Implement", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "pr"}},
			},
			{
				ID: "qa", Type: "agent_task", Title: "QA", ActorRole: "qa-agent",
				InputFields:  []entity.WorkflowField{{Name: "pr"}},
				OutputFields: []entity.WorkflowField{{Name: "test_report"}},
			},
		},
		Edges:     []entity.WorkflowEdge{{From: "impl", To: "qa", InputMapping: map[string]string{"pr": "$output.pr"}}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := &entity.Task{
		ID: "task-delivery-legacy-shape", Title: "Legacy shape", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: gitOut(t, wt, "rev-parse", "HEAD"), BaseBranch: "main", BranchName: "task/x",
		Vars: map[string]string{}, // no contract var
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}

	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "legacy delivery",
		"outputs": map[string]string{"pr": "branch:task/x"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy definition without anchor promise must complete ungated, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestTaskCreationSeedsDeliveryContractForAnchorTemplates: the REAL config
// entry point — creating a task with a definition whose qa requires
// delivery_sha auto-seeds the delivery contract on the root task. The
// greenfield template is instantiated as a definition exactly the way
// POST /api/v1/workflows does it.
func TestTaskCreationSeedsDeliveryContractForAnchorTemplates(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Anchor Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Seeded anchor run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("task creation failed: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode task: %v (%s)", err, rec.Body.String())
	}
	// The creation response does not echo vars: read the persisted task back.
	stored, err := s.ts.GetTask("sample", "pm", created.ID)
	if err != nil {
		t.Fatalf("created task %q not found in taskstore: %v", created.ID, err)
	}
	contract := stored.Vars[runner.DeliveryContractVar]
	if !strings.Contains(contract, "requirePush") || !strings.Contains(contract, "true") {
		t.Fatalf("anchor-requiring template must auto-seed the delivery contract on the root task, got vars=%v", stored.Vars)
	}
	// The created task is wired to the greenfield workflow run.
	if _, found, err := wfStore.RunForTask("sample", created.ID); err != nil || !found {
		t.Fatalf("created task must have a workflow run: found=%v err=%v", found, err)
	}
}

// TestRuntimeTaskCreationSeedsDeliveryContract: the runtime agent path
// (POST /api/v1/runtime/tasks/from-template → createRuntimeTaskFromBody)
// must seed the delivery contract exactly like the console path — an
// anchor-requiring definition cannot rely on the agent knowing the var.
func TestRuntimeTaskCreationSeedsDeliveryContract(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "dispatcher", "aw-dispatcher", "pm-dispatcher-sample")

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Runtime Anchor Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	if err := tasktemplate.NewStore(s.controlDB, workspaceID).Save(&entity.TaskTemplate{
		ID:                    "tt-runtime-anchor",
		Name:                  "Runtime anchor",
		Project:               "sample",
		Type:                  string(entity.TaskTypeChore),
		TitleTemplate:         "Deliver it",
		PromptTemplate:        "deliver",
		WorkflowDefinitionID:  def.ID,
		WorkflowActorBindings: map[string]entity.WorkflowActorBinding{},
	}); err != nil {
		t.Fatalf("save template: %v", err)
	}

	principal := runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      "sample",
		Agent:        "dispatcher",
		Capabilities: []string{"task.use"},
	}
	body := `{"templateId":"tt-runtime-anchor","project":"sample"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/from-template", strings.NewReader(body))
	req = runtimePrincipalContext(req, principal)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleRuntimePostTaskFromTemplate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		TaskID string `json:"taskId"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	taskID := created.TaskID
	if taskID == "" {
		taskID = created.ID
	}
	stored, err := s.ts.GetTask("sample", "dispatcher", taskID)
	if err != nil {
		t.Fatalf("stored task %q: %v", taskID, err)
	}
	contract := stored.Vars[runner.DeliveryContractVar]
	if !strings.Contains(contract, "requirePush") || !strings.Contains(contract, "true") {
		t.Fatalf("runtime creation must auto-seed the delivery contract for anchor-requiring definitions, got vars=%v", stored.Vars)
	}
	if _, found, err := wfStore.RunForTask("sample", taskID); err != nil || !found {
		t.Fatalf("runtime task must have a workflow run: found=%v err=%v", found, err)
	}
}

// TestWeakContractOnAnchorDefinitionFailsClosed (independent-review gap):
// an explicit WEAK contract (both git dimensions false) on an
// anchor-requiring definition previously passed the seeding check (contract
// present), passed the completion gate (no git requirement = no violation),
// stripped the forged SHA, and still advanced to a qa step that REQUIRES
// delivery_sha — an anchorless QA the platform itself let through.
func TestWeakContractOnAnchorDefinitionFailsClosed(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-delivery-weak", wt)
	base := gitOut(t, wt, "rev-parse", "HEAD")
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-delivery-anchored-weak", Name: "Anchored weak contract", Version: 1, Scope: "workspace", StartStepID: "impl",
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
	task := &entity.Task{
		ID: "task-delivery-weak", Title: "Anchored weak contract", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: base, BaseBranch: "main", BranchName: "task/x",
		Vars: map[string]string{runner.DeliveryContractVar: `{"requireGitCommit":false,"requirePush":false}`},
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent":       {Type: "agent", ID: "pm"},
		"qa-agent":       {Type: "agent", ID: "pm"},
		"owner-engineer": {Type: "human", ID: "admin"},
	}); err != nil {
		t.Fatal(err)
	}

	rec := postLinearStepComplete(t, s, workspaceID, task.ID, "pm", map[string]any{
		"status": "success", "summary": "weak contract delivery",
		"outputs": map[string]string{
			"pr":           "branch:task/x (no commit, no push)",
			"tests_run":    "go test ./... 0/0 (no real delivery)",
			"delivery_sha": "feedfacefeedfacefeedfacefeedfacefeedface",
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak contract on an anchor-requiring definition must fail closed with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requireGitCommit") {
		t.Fatalf("rejection must name the strengthened contract, got %s", rec.Body.String())
	}
	// Nothing persisted: the run stays at impl.
	run, found, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup: found=%v err=%v", found, err)
	}
	if run.ActiveStepID != "impl" {
		t.Fatalf("run must remain at impl after weak-contract rejection, got %q", run.ActiveStepID)
	}
}

// TestWeakContractRejectedAtCreation (HTTP, console path): an explicit weak
// contract on an anchor-requiring definition is rejected with 400 and ZERO
// task/run written — no silently-strengthened or waived contract.
func TestWeakContractRejectedAtCreation(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Weak Contract Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Weak contract run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
		Vars:                 map[string]string{runner.DeliveryContractVar: `{"requireGitCommit":false,"requirePush":false}`},
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weak contract must be rejected at creation with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requireGitCommit") {
		t.Fatalf("rejection must name the required contract dimensions, got %s", rec.Body.String())
	}
	// Zero writes: the rejection fires before AddTask, so no pm task with
	// this title may exist (ListTasks is per-agent).
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestStrongContractAcceptedAtCreation (HTTP, console path): an explicit
// contract that CAN produce the anchor (both dimensions true) is accepted
// verbatim — no rewrite, no second-guessing.
func TestStrongContractAcceptedAtCreation(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Strong Contract Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	explicit := `{"requireGitCommit":true,"requirePush":true,"requireModelActivity":true}`
	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Strong contract run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
		Vars:                 map[string]string{runner.DeliveryContractVar: explicit},
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("strong contract must be accepted at creation, got %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	stored, err := s.ts.GetTask("sample", "pm", created.ID)
	if err != nil {
		t.Fatalf("stored task: %v", err)
	}
	if stored.Vars[runner.DeliveryContractVar] != explicit {
		t.Fatalf("explicit strong contract must be preserved verbatim, got %q", stored.Vars[runner.DeliveryContractVar])
	}
	if _, found, err := wfStore.RunForTask("sample", created.ID); err != nil || !found {
		t.Fatalf("created task must have a workflow run: found=%v err=%v", found, err)
	}
}

// TestBranchWeakContractOnAnchorDefinitionFailsClosed (HTTP, real fan-out
// materialization): the parent definition promises the machine anchor to a
// required qa input, an in-flight parent carries a WEAK contract which the
// branch child inherits, and the agent forges a delivery_sha. The branch
// gate must resolve the PARENT run (not the child's single-step run) and
// reject — zero state written: the branch instance stays un-completed and
// no proven/forged outputs land.
func TestBranchWeakContractOnAnchorDefinitionFailsClosed(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutGitRemote(t, s, gitRoot)

	// Custom parent definition: same fan-out shape as the shared fixture,
	// PLUS a qa step requiring delivery_sha — the structural anchor promise
	// the guard keys on. The qa step is never reached here; it only needs
	// to exist so the parent definition promises the anchor.
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	anchorDef := &entity.WorkflowDefinition{
		ID: "wf-fanout-anchor", Name: "Fanout anchor parent", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID: "start", Type: "agent_task", Title: "Contract", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
			},
			{
				ID: "parallel", Type: "parallel_stage", Title: "Parallel", JoinPolicy: "all",
				Branches: []entity.WorkflowBranch{
					{
						ID: "ws_a", Title: "WS-1",
						OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
					},
					{
						ID: "ws_b", Title: "WS-2",
						OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
					},
				},
			},
			{
				ID: "qa", Type: "agent_task", Title: "QA", ActorRole: "qa-agent",
				InputFields:  []entity.WorkflowField{{Name: "delivery_sha"}},
				OutputFields: []entity.WorkflowField{{Name: "test_report"}},
			},
		},
		Edges: []entity.WorkflowEdge{
			{From: "start", To: "parallel"},
			{From: "parallel", To: "qa", InputMapping: map[string]string{"delivery_sha": "$output.delivery_sha"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(anchorDef); err != nil {
		t.Fatal(err)
	}
	parentTask := &entity.Task{
		ID: "task-fanout-anchor", Title: "Fanout anchor root", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main",
		// In-flight grandfather shape: created before creation-side
		// rejection, carrying a weak contract that the fan-out will copy
		// verbatim onto every branch child.
		Vars: map[string]string{"MULTIGENT_DELIVERY_CONTRACT": `{"requireGitCommit":false,"requirePush":false}`},
	}
	if err := s.ts.AddTask("sample", "pm", parentTask); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", parentTask.ID, anchorDef.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent": {Type: "agent", ID: "pm"},
		"ws_a":     {Type: "agent", ID: "pm"},
		"ws_b":     {Type: "agent", ID: "pm"},
		"parallel": {Type: "agent", ID: "pm"},
	}); err != nil {
		t.Fatal(err)
	}

	rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-anchor", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("parent start completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	childIDs := fanoutBranchChildIDs(t, s, workspaceID, "task-fanout-anchor")
	childID := childIDs["ws_b"]
	childTask, err := s.ts.GetTask("sample", "pm", childID)
	if err != nil {
		t.Fatal(err)
	}
	// The child inherited the weak contract — this is the exact shape the
	// guard must catch even though the child's OWN single-step run def
	// never promises the anchor.
	if got := childTask.Vars["MULTIGENT_DELIVERY_CONTRACT"]; !strings.Contains(got, "false") {
		t.Fatalf("fixture precondition: child must inherit the weak contract, got %q", got)
	}

	// Touch a real file in the child worktree so the touched_paths
	// cross-check passes and we reach the DELIVERY gate (the layer under
	// test) instead of being rejected upstream by the paths check.
	if err := os.WriteFile(filepath.Join(childTask.WorktreeDir, "module_ws_b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, childTask.WorktreeDir, "add", ".")
	gitRun(t, childTask.WorktreeDir, "commit", "-m", "ws_b unpushed delivery")
	rec = postBranchStepComplete(t, s, workspaceID, childID, map[string]string{
		"branch_summary": "weak contract, forged anchor",
		"touched_paths":  "module_ws_b.go",
		"delivery_sha":   "feedfacefeedfacefeedfacefeedfacefeedface",
	})
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusInternalServerError {
		t.Fatalf("weak contract on an anchor-promise definition must fail closed, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requireGitCommit") && !strings.Contains(rec.Body.String(), "parent run") {
		t.Fatalf("rejection must name the contract dimensions or the parent-run resolution, got %s", rec.Body.String())
	}
	// Zero state written: the branch instance for this child must NOT be
	// completed, and no delivery_sha output may exist anywhere.
	parentRun, found, err := wfStore.RunForTask("sample", "task-fanout-anchor")
	if err != nil || !found {
		t.Fatalf("parent run lookup: found=%v err=%v", found, err)
	}
	instances, err := wfStore.BranchInstancesForStep(parentRun.ID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.ChildTaskID != childID {
			continue
		}
		if inst.Status == "completed" || inst.Status == "success" {
			t.Fatalf("branch instance must not be completed after rejection, got %q", inst.Status)
		}
		if got := inst.OutputValues["delivery_sha"]; got != "" {
			t.Fatalf("no delivery_sha output may be written after rejection, got %q", got)
		}
		return
	}
	// Instance not found at all is also acceptable (completion never
	// persisted); falling through here means exactly that.
}

// TestBranchStrongContractStampsProvenSHAOnAnchorDefinition (HTTP): the
// same fan-out shape with a STRONG contract completes the branch, and the
// gate stamps the proven pair — the positive control for the guard above.
func TestBranchStrongContractStampsProvenSHAOnAnchorDefinition(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutGitRemote(t, s, gitRoot)

	parentTask := seedFanoutParentRun(t, s, workspaceID, baseCommit)
	parentTask.Vars["MULTIGENT_DELIVERY_CONTRACT"] = `{"requireGitCommit":true,"requirePush":true}`
	if err := s.ts.PersistTask("sample", "pm", parentTask); err != nil {
		t.Fatal(err)
	}

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
	if err := os.WriteFile(filepath.Join(honestTask.WorktreeDir, "module_ws_b.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, honestTask.WorktreeDir, "add", ".")
	gitRun(t, honestTask.WorktreeDir, "commit", "-m", "ws_b delivery")
	gitRun(t, honestTask.WorktreeDir, "push", "origin", honestTask.BranchName)
	pushed := gitRunOut(t, honestTask.WorktreeDir, "rev-parse", "HEAD")

	rec = postBranchStepComplete(t, s, workspaceID, honestID, map[string]string{
		"branch_summary": "delivered and pushed",
		"touched_paths":  "module_ws_b.go",
		"delivery_sha":   "forged",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("honest branch must complete under a strong contract, got %d: %s", rec.Code, rec.Body.String())
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRun, found, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil || !found {
		t.Fatalf("parent run lookup: found=%v err=%v", found, err)
	}
	instances, err := wfStore.BranchInstancesForStep(parentRun.ID, "parallel")
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
		return
	}
	t.Fatalf("branch instance for child %s not found", honestID)
}

// TestRuntimeFromTemplateCannotSmuggleWeakContract (HTTP, runtime path):
// the runtime WORKFLOW-creating entry is POST /api/v1/runtime/tasks/
// from-template → createRuntimeTaskFromBody. Plain runtime POST /tasks has
// no workflowDefinitionId field at all (it cannot start a run), so the
// from-template path is the one the weak-contract guard must hold on. A
// caller cannot inject MULTIGENT_DELIVERY_CONTRACT through template inputs:
// the run must either be seeded with the full default contract or rejected
// — never silently weak. This test drives the real handler with an
// anchor-requiring template and asserts the seeded (strong) contract on
// the stored task, pinning the entry against drift.
func TestRuntimeFromTemplateCannotSmuggleWeakContract(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "sample", "dispatcher", "aw-dispatcher", "pm-dispatcher-sample")

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Runtime Anchor Pipeline 2")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	if err := tasktemplate.NewStore(s.controlDB, workspaceID).Save(&entity.TaskTemplate{
		ID:                    "tt-runtime-anchor-2",
		Name:                  "Runtime anchor 2",
		Project:               "sample",
		Type:                  string(entity.TaskTypeChore),
		TitleTemplate:         "Runtime anchor run",
		PromptTemplate:        "deliver {{thing}}",
		WorkflowDefinitionID:  def.ID,
		WorkflowActorBindings: map[string]entity.WorkflowActorBinding{},
		Variables: []entity.TaskTemplateVariable{
			{Name: "thing", Required: true},
			// A template input trying to smuggle a weak contract var: the
			// inputs feed prompt rendering, NOT task.Vars, so the guard's
			// auto-seed (contract var empty) must win.
			{Name: "MULTIGENT_DELIVERY_CONTRACT", Required: false, Default: `{"requireGitCommit":false,"requirePush":false}`},
		},
	}); err != nil {
		t.Fatalf("save template: %v", err)
	}

	principal := runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      "sample",
		Agent:        "dispatcher",
		Capabilities: []string{"task.use"},
	}
	body := `{"templateId":"tt-runtime-anchor-2","project":"sample","inputs":{"thing":"it","MULTIGENT_DELIVERY_CONTRACT":"{\"requireGitCommit\":false,\"requirePush\":false}"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/from-template", strings.NewReader(body))
	req = runtimePrincipalContext(req, principal)
	rec := httptest.NewRecorder()
	s.handleRuntimePostTaskFromTemplate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("runtime from-template creation failed: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		TaskID string `json:"taskId"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	taskID := created.TaskID
	if taskID == "" {
		taskID = created.ID
	}
	stored, err := s.ts.GetTask("sample", "dispatcher", taskID)
	if err != nil {
		t.Fatalf("stored task %q: %v", taskID, err)
	}
	// The template input must NOT have landed in task.Vars as a weak
	// contract; the auto-seeded full contract must be present.
	contract := stored.Vars["MULTIGENT_DELIVERY_CONTRACT"]
	if !strings.Contains(contract, "requireGitCommit") || !strings.Contains(contract, "true") {
		t.Fatalf("anchor-requiring template must carry the full seeded contract, got vars=%v", stored.Vars)
	}
	if _, found, err := wfStore.RunForTask("sample", taskID); err != nil || !found {
		t.Fatalf("created task must have a workflow run: found=%v err=%v", found, err)
	}
}

// TestAnchorRequiredDefinitionWithoutPRProducerFailsClosed (fact-check of
// the earlier "structurally unreachable" claim): a definition whose qa
// REQUIRES delivery_sha but whose ONLY producing step does NOT declare a
// pr output. The declaration-as-opt-in gate never fires (no pr output),
// the edge still maps $output.delivery_sha (absent), and — before the
// creation-side structural check — the run would reach an anchorless
// required-input qa.
func TestAnchorRequiredDefinitionWithoutPRProducerFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	// Definition with the structural flaw: impl declares NO pr output.
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Broken Producer Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	for i := range def.Steps {
		if def.Steps[i].ID == "implementation" {
			filtered := def.Steps[i].OutputFields[:0]
			for _, f := range def.Steps[i].OutputFields {
				if f.Name != "pr" {
					filtered = append(filtered, f)
				}
			}
			def.Steps[i].OutputFields = filtered
		}
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Broken producer run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an anchor-requiring definition with NO pr-declaring producer must be rejected at creation with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	// Zero writes: no task with this title.
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestAnchorRequiredWithoutDataPathFailsClosed ("graph path, no data
// path"): the edge topology connects a pr-declaring producer to the
// anchor-requiring step, but the edge maps ONLY pr — no
// $output/$input.delivery_sha anywhere. buildNextInputValues forwards only
// mapped keys, so QA would receive nothing despite node connectivity. The
// creation-side structural check must reject this definition.
func TestAnchorRequiredWithoutDataPathFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "No Data Path Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// Strip delivery_sha/delivery_branch from every edge mapping (keep the
	// pr mapping): connectivity intact, data path severed.
	for i := range def.Edges {
		m := def.Edges[i].InputMapping
		if m == nil {
			continue
		}
		delete(m, "delivery_sha")
		delete(m, "delivery_branch")
	}
	// The middle steps keep their optional delivery_sha INPUT declarations
	// (a cosmetic contract without a feed) — the data-path check must not
	// be fooled by declarations alone.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "No data path run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a connected-but-unfed anchor promise must be rejected at creation with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestBranchMissingOrWeakContractOnAnchorDefinitionFailsClosed (HTTP,
// table-driven): on an anchor-promise parent definition, a branch child
// must fail closed when it carries NO contract (the normalize whitelist
// would otherwise persist the forged delivery_sha into the branch instance)
// or a WEAK inherited contract (no git dimension can produce the anchor).
// Legacy shapes without the anchor promise are covered by the existing
// fan-out tests and stay untouched.
func TestBranchMissingOrWeakContractOnAnchorDefinitionFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		contract string // parent contract var; empty = missing
		wantMsg  string
	}{
		{name: "missing contract", contract: "", wantMsg: "no MULTIGENT_DELIVERY_CONTRACT"},
		{name: "weak contract", contract: `{"requireGitCommit":false,"requirePush":false}`, wantMsg: "requireGitCommit"},
		{name: "commit-only contract", contract: `{"requireGitCommit":true,"requirePush":false}`, wantMsg: "requireGitCommit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, workspaceID := newBranchJoinHTTPServer(t)
			s.worktreeMgr = gitworktreeManagerForTest()
			gitRoot, baseCommit := buildFanoutGitWorkspace(t, s)
			seedFanoutGitRemote(t, s, gitRoot)

			now := time.Now().UTC()
			wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
			anchorDef := &entity.WorkflowDefinition{
				ID: "wf-fanout-anchor-" + strings.ReplaceAll(tc.name, " ", "-"), Name: "Fanout anchor parent " + tc.name, Version: 1, Scope: "workspace", StartStepID: "start",
				Steps: []entity.WorkflowStep{
					{
						ID: "start", Type: "agent_task", Title: "Contract", ActorRole: "pm-agent",
						OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
					},
					{
						ID: "parallel", Type: "parallel_stage", Title: "Parallel", JoinPolicy: "all",
						Branches: []entity.WorkflowBranch{
							{
								ID: "ws_a", Title: "WS-1",
								OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
							},
							{
								ID: "ws_b", Title: "WS-2",
								OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
							},
						},
					},
					{
						ID: "qa", Type: "agent_task", Title: "QA", ActorRole: "qa-agent",
						InputFields:  []entity.WorkflowField{{Name: "delivery_sha"}},
						OutputFields: []entity.WorkflowField{{Name: "test_report"}},
					},
				},
				Edges: []entity.WorkflowEdge{
					{From: "start", To: "parallel"},
					{From: "parallel", To: "qa", InputMapping: map[string]string{"delivery_sha": "$output.delivery_sha"}},
				},
				CreatedAt: now, UpdatedAt: now,
			}
			if err := wfStore.SaveDefinition(anchorDef); err != nil {
				t.Fatal(err)
			}
			vars := map[string]string{}
			if tc.contract != "" {
				vars["MULTIGENT_DELIVERY_CONTRACT"] = tc.contract
			}
			parentTask := &entity.Task{
				ID: "task-fanout-anchor", Title: "Fanout anchor root", Status: entity.TaskStatusInProgress,
				Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
				BaseCommit: baseCommit, BaseBranch: "main",
				Vars: vars, // in-flight grandfather shape: whatever the parent carries, the branch inherits
			}
			if err := s.ts.AddTask("sample", "pm", parentTask); err != nil {
				t.Fatal(err)
			}
			if _, _, err := wfStore.StartRun("sample", parentTask.ID, anchorDef.ID, map[string]entity.WorkflowActorBinding{
				"pm-agent": {Type: "agent", ID: "pm"},
				"ws_a":     {Type: "agent", ID: "pm"},
				"ws_b":     {Type: "agent", ID: "pm"},
				"parallel": {Type: "agent", ID: "pm"},
			}); err != nil {
				t.Fatal(err)
			}

			rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-anchor", map[string]string{
				"branch_summary": "contract frozen",
				"touched_paths":  "contract.md",
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("parent start completion must be 200, got %d: %s", rec.Code, rec.Body.String())
			}
			childIDs := fanoutBranchChildIDs(t, s, workspaceID, "task-fanout-anchor")
			childID := childIDs["ws_b"]

			// Real change so the touched_paths cross-check passes and we
			// reach the DELIVERY gate layer under test.
			childTask, err := s.ts.GetTask("sample", "pm", childID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(childTask.WorktreeDir, "module_ws_b.go"), []byte("package main\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRun(t, childTask.WorktreeDir, "add", ".")
			gitRun(t, childTask.WorktreeDir, "commit", "-m", "ws_b unpushed delivery")

			rec = postBranchStepComplete(t, s, workspaceID, childID, map[string]string{
				"branch_summary": "forged anchor, no proven delivery",
				"touched_paths":  "module_ws_b.go",
				"delivery_sha":   "feedfacefeedfacefeedfacefeedfacefeedface",
			})
			if rec.Code == http.StatusOK {
				t.Fatalf("[%s] anchor promise without a producible contract must fail closed, got 200: %s", tc.name, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Fatalf("[%s] rejection must name %q, got %s", tc.name, tc.wantMsg, rec.Body.String())
			}
			// Zero state written: the branch instance must not be completed
			// and no delivery_sha output may exist.
			parentRun, found, err := wfStore.RunForTask("sample", "task-fanout-anchor")
			if err != nil || !found {
				t.Fatalf("parent run lookup: found=%v err=%v", found, err)
			}
			instances, err := wfStore.BranchInstancesForStep(parentRun.ID, "parallel")
			if err != nil {
				t.Fatal(err)
			}
			for _, inst := range instances {
				if inst.ChildTaskID != childID {
					continue
				}
				if inst.Status == "completed" || inst.Status == "success" {
					t.Fatalf("[%s] branch instance must not be completed after rejection, got %q", tc.name, inst.Status)
				}
				if got := inst.OutputValues["delivery_sha"]; got != "" {
					t.Fatalf("[%s] no delivery_sha output may be written after rejection, got %q", tc.name, got)
				}
				return
			}
		})
	}
}

// TestAnchorRequiredViaPRlessRelayFailsClosed ("relay forgery"): a pr-less
// relay step between the producer and qa maps $output.delivery_sha on BOTH
// hops. The relay never declares pr, so its completion is never gate-stamped
// and its delivery_sha output is agent-forgeable (normalize whitelist); the
// anchor would ride agent-chosen values straight into the required qa input.
// The data-path check must only honor $output.delivery_sha when the SOURCE
// step declares pr.
func TestAnchorRequiredViaPRlessRelayFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Relay Forgery Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// Insert a pr-less relay between implementation and self_review: the
	// impl→relay hop maps $output.delivery_sha (impl DOES declare pr — this
	// part is honest and stamps the relay's INPUT), the relay→self_review
	// hop maps $output.delivery_sha from the RELAY (forgeable: relay never
	// declares pr, so its completion is never gate-stamped). The rest of
	// the chain (self_review → code_review → qa) is untouched, so with the
	// relay hop honored the data path is complete — exactly the shape the
	// guard must refuse.
	relay := entity.WorkflowStep{
		ID: "relay", Type: "agent_task", Title: "Relay", ActorRole: "pm-agent",
		InputFields:  []entity.WorkflowField{{Name: "delivery_sha", Optional: true}},
		OutputFields: []entity.WorkflowField{{Name: "relay_note"}},
	}
	def.Steps = append(def.Steps, relay)
	edges := make([]entity.WorkflowEdge, 0, len(def.Edges)+2)
	for _, e := range def.Edges {
		if e.From == "implementation" && e.To == "self_review" {
			// impl → relay: honest hop (impl is a pr-declaring producer),
			// then relay → self_review: forgeable hop (relay has NO pr).
			edges = append(edges, entity.WorkflowEdge{
				From: "implementation", To: "relay",
				InputMapping: map[string]string{"delivery_sha": "$output.delivery_sha"},
			})
			edges = append(edges, entity.WorkflowEdge{
				From: "relay", To: "self_review",
				InputMapping: map[string]string{
					"delivery_sha":    "$output.delivery_sha",
					"delivery_branch": "$output.delivery_sha",
				},
			})
			continue
		}
		edges = append(edges, e)
	}
	def.Edges = edges
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Relay forgery run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a pr-less relay claiming $output.delivery_sha must be rejected at creation with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestAnchorRequiredViaWrongMappingKeyFailsClosed (HTTP): the mapping VALUE
// names delivery_sha but the KEY does not —
// {"approved_change":"$input.delivery_sha"}. buildNextInputValues writes the
// resolved value into the KEY's slot, so the target step's delivery_sha
// input stays empty no matter what flows. A value-only check would bless
// this edge and the creation gate would accept an anchor promise that can
// never be fed. Must reject with zero task/run writes.
func TestAnchorRequiredViaWrongMappingKeyFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Wrong Key Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// Flip ONLY the key of the edge that feeds qa its anchor: the value
	// still says $input.delivery_sha, but it lands in approved_change —
	// qa's required delivery_sha input is starved while every hop still
	// "carries" the expression textually.
	found := false
	for i := range def.Edges {
		e := &def.Edges[i]
		if e.From == "code_review" && e.To == "qa" {
			if m := e.InputMapping; m != nil {
				if v, ok := m["delivery_sha"]; ok {
					delete(m, "delivery_sha")
					m["approved_change"] = v
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("fixture must find the code_review-to-qa delivery_sha mapping")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Wrong key run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a value-only delivery_sha mapping must be rejected at creation with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestAnchorRequiredViaDisconnectedProducerFailsClosed (HTTP): the
// pr-declaring producer is NOT reachable from def.StartStepID — the run
// can never execute it, so its (honestly stamped) delivery_sha can never
// flow. Node-level data-path checks seeded from ALL pr-declaring steps
// would bless the shape; producibility must be proven from the steps the
// run actually starts from. Must reject with zero task writes.
func TestAnchorRequiredViaDisconnectedProducerFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Disconnected Producer Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// Reroute the anchor feed: instead of implementation (on the live path
	// from requirement_draft), make a DISCONNECTED pr-declaring island the
	// only feeder of qa's delivery_sha. The island is linkable by edges
	// (definitions are stored as-is) but nothing on the start-reachable
	// path ever completes it.
	island := entity.WorkflowStep{
		ID: "island_impl", Type: "agent_task", Title: "Island Impl", ActorRole: "qa-agent",
		OutputFields: []entity.WorkflowField{{Name: "pr"}, {Name: "delivery_sha"}},
	}
	def.Steps = append(def.Steps, island)
	edges := make([]entity.WorkflowEdge, 0, len(def.Edges))
	for _, e := range def.Edges {
		// Sever the live feed into qa (drop delivery_sha from every mapping
		// that lands on the live path to qa).
		if e.To == "qa" || e.To == "code_review" || e.To == "self_review" {
			if m := e.InputMapping; m != nil {
				delete(m, "delivery_sha")
			}
		}
		edges = append(edges, e)
	}
	// The disconnected island feeds qa directly with an honest, pr-backed
	// $output.delivery_sha mapping.
	edges = append(edges, entity.WorkflowEdge{
		From: "island_impl", To: "qa",
		InputMapping: map[string]string{"delivery_sha": "$output.delivery_sha"},
	})
	def.Edges = edges
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Disconnected producer run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an anchor promise fed only by a start-unreachable producer must be rejected at creation with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestAnchorRequiredViaBareProducerInputExprFailsClosed (blind-review probe
// A): a pr-declaring producer maps "$input.delivery_sha" (not "$output.")
// straight to the required qa input. The gate stamps the producer's OUTPUT
// on completion; its INPUT slot holds nothing, so the expression resolves
// to an empty value at runtime and qa silently receives no anchor. The
// walk must only trust "$input" passthrough from steps that verifiably
// RECEIVED the anchor. Must reject with zero task writes.
func TestAnchorRequiredViaBareProducerInputExprFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Bare Producer Input Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// Reroute: implementation feeds qa DIRECTLY with the wrong expression
	// form ($input instead of $output). The producer has never received
	// the anchor as an input, so this edge reads an empty value.
	edges := make([]entity.WorkflowEdge, 0, len(def.Edges)+1)
	for _, e := range def.Edges {
		if e.To == "qa" || e.To == "code_review" || e.To == "self_review" {
			if m := e.InputMapping; m != nil {
				delete(m, "delivery_sha")
			}
		}
		edges = append(edges, e)
	}
	edges = append(edges, entity.WorkflowEdge{
		From: "implementation", To: "qa",
		InputMapping: map[string]string{"delivery_sha": "$input.delivery_sha"},
	})
	def.Edges = edges
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Bare producer input run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a producer seeding $input.delivery_sha it never received must be rejected with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestAnchorRequiredViaSelfDeclaringQAFailsClosed (blind-review probe B):
// the qa step itself declares a pr output, which makes it a walk seed —
// but nothing ever feeds its required delivery_sha input (no incoming
// delivery_sha mapping at all). Seed status alone must never satisfy the
// promise: success requires an ARRIVAL via a carrying edge. Must reject
// with zero task writes.
func TestAnchorRequiredViaSelfDeclaringQAFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Self Declaring QA Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// Give qa a pr output (making it a seed) and strip every incoming
	// delivery_sha mapping so nothing can arrive at its required input.
	for i := range def.Steps {
		if def.Steps[i].ID == "qa" {
			def.Steps[i].OutputFields = append(def.Steps[i].OutputFields, entity.WorkflowField{Name: "pr"})
		}
	}
	for i := range def.Edges {
		if def.Edges[i].To == "qa" {
			if m := def.Edges[i].InputMapping; m != nil {
				delete(m, "delivery_sha")
			}
		}
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Self declaring qa run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a self-declaring qa with no anchor arrival must be rejected with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}

// TestAnchorRequiredViaUnmappedFallbackEdgeAccepted (blind-review probe C,
// positive control): a pr-declaring producer feeds the required qa input
// through an edge with NO InputMapping — buildNextInputValues' same-name
// fallback forwards the producer's gate-stamped delivery_sha output. The
// check must recognize this legitimate shape and NOT reject (creation
// succeeds with the full auto-seeded contract).
func TestAnchorRequiredViaUnmappedFallbackEdgeAccepted(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Fallback Edge Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// implementation feeds qa DIRECTLY with an UNMAPPED edge: the fallback
	// forwards delivery_sha from the producer's stamped output. The old
	// code_review-to-qa edge is REMOVED entirely (not merely stripped): a
	// surviving start-reachable edge into the required input that does not
	// carry the anchor is itself a selectable unverified route and must
	// (and does) fail the universal rule.
	edges := make([]entity.WorkflowEdge, 0, len(def.Edges)+1)
	for _, e := range def.Edges {
		if e.From == "code_review" && e.To == "qa" {
			continue
		}
		if e.To == "qa" || e.To == "code_review" || e.To == "self_review" {
			if m := e.InputMapping; m != nil {
				delete(m, "delivery_sha")
			}
		}
		edges = append(edges, e)
	}
	edges = append(edges, entity.WorkflowEdge{From: "implementation", To: "qa"})
	def.Edges = edges
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Fallback edge run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a producer-fed unmapped fallback edge must be accepted with 201, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestAnchorRequiredViaSelectableUnverifiedRouteFailsClosed (acceptance
// challenge): a pr-declaring producer with TWO selectable outgoing routes —
// the good conditional-free fallback into the anchored chain, and a
// conditional express route through a pr-less relay whose $output.delivery_sha
// mapping feeds the SAME required qa input. chooseNextEdge takes the
// conditional edge whenever the agent outputs decision=express, and nothing
// at runtime stamps the relay (no pr means no gate; the normalize whitelist
// admits its delivery_sha output), so qa would receive an agent-chosen SHA
// on the express route. A "some anchored path exists" check returns true
// here; the promise only holds when EVERY selectable route into the
// required input verifiably carries the anchor. Must reject with zero task
// writes.
func TestAnchorRequiredViaSelectableUnverifiedRouteFailsClosed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedSampleAgentsForTest(t, s, workspaceID)

	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "Express Relay Pipeline")
	if !ok {
		t.Fatal("greenfield template must instantiate")
	}
	// impl gains a decision output so the conditional express route is
	// actually selectable by chooseNextEdge (conditional edges win over
	// the nil fallback; decision=express takes the relay, anything else
	// falls back to self_review).
	for i := range def.Steps {
		if def.Steps[i].ID == "implementation" {
			def.Steps[i].OutputFields = append(def.Steps[i].OutputFields, entity.WorkflowField{Name: "decision"})
		}
	}
	relay := entity.WorkflowStep{
		ID: "express_relay", Type: "agent_task", Title: "Express Relay", ActorRole: "developer-agent",
		InputFields:  []entity.WorkflowField{{Name: "delivery_sha", Optional: true}},
		OutputFields: []entity.WorkflowField{{Name: "relay_note"}}, // NO pr output
	}
	def.Steps = append(def.Steps, relay)
	def.Edges = append(def.Edges,
		// Selectable bad route: impl --decision=express--> relay --> qa.
		entity.WorkflowEdge{
			From: "implementation", To: "express_relay",
			Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "express"},
		},
		entity.WorkflowEdge{
			From: "express_relay", To: "qa",
			InputMapping: map[string]string{"delivery_sha": "$output.delivery_sha"},
		},
	)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}

	taskBody := postTaskBody{
		Agent:                "pm",
		Title:                "Express relay run",
		Prompt:               "deliver something",
		WorkflowDefinitionID: def.ID,
	}
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", taskBody)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a selectable unverified route into the required anchor input must be rejected with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery_sha") {
		t.Fatalf("rejection must name the anchor promise, got %s", rec.Body.String())
	}
	tasks, listErr := s.ts.ListTasks("sample", "pm")
	if listErr != nil {
		t.Fatalf("list tasks: %v", listErr)
	}
	for _, tk := range tasks {
		if tk != nil && tk.Title == taskBody.Title {
			t.Fatalf("rejected creation must not persist a task, found %q", tk.ID)
		}
	}
}
