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

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedFanoutGitRemote creates a bare remote and pushes the workspace main so
// push evidence (ls-remote) is exercisable inside the test. Worktrees clone
// nothing — they share the workspace repo's objects, and pushes from a
// worktree go to the same origin remote.
func seedFanoutGitRemote(t *testing.T, s *Server, gitRoot string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run(remote, "init", "--bare", "-b", "main")
	run(gitRoot, "remote", "add", "origin", remote)
	run(gitRoot, "push", "origin", "main")
}

// fanoutBranchChildIDs returns branchID → child task ID for the parent's
// parallel step.
func fanoutBranchChildIDs(t *testing.T, s *Server, workspaceID, parentTaskID string) map[string]string {
	t.Helper()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("sample", parentTaskID)
	if err != nil || !found {
		t.Fatalf("parent run lookup: found=%v err=%v", found, err)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	childIDs := map[string]string{}
	for _, inst := range instances {
		childIDs[inst.BranchID] = inst.ChildTaskID
	}
	return childIDs
}

// gitRun runs a git command in dir with a deterministic identity.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

// Review round 3, item 3 (entry A): the delivery contract inherited by
// fan-out branch children makes a NO-DELIVERY branch completion fail (join
// must not advance), and the same branch WITH a pushed delivery commit must
// complete successfully.
func TestFanoutDeliveryContractBlocksNoDeliveryChild(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutGitRemote(t, s, gitRoot)

	// Parent carries the delivery contract — every branch child inherits it.
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
	if len(childIDs) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(childIDs))
	}

	// Entry check: both children inherited the contract var.
	for branchID, childID := range childIDs {
		task, err := s.ts.GetTask("sample", "pm", childID)
		if err != nil {
			t.Fatal(err)
		}
		if got := task.Vars["MULTIGENT_DELIVERY_CONTRACT"]; got == "" {
			t.Fatalf("branch %s must inherit the delivery contract", branchID)
		}
	}

	// Branch ws_a completes with an UNCOMMITTED change: the touched_paths
	// cross-check passes (the file really changed vs the baseline) but the
	// delivery contract has nothing to measure — no commit beyond the
	// frozen base, no push. The formal completion entry must reject it.
	noDelivery := childIDs["ws_a"]
	noDelTask, err := s.ts.GetTask("sample", "pm", noDelivery)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noDelTask.WorktreeDir, "uncommitted.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, noDelivery, map[string]string{
		"branch_summary": "claims done without any commit",
		"touched_paths":  "uncommitted.go",
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("no-delivery branch must NOT complete successfully: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "delivery contract unmet") {
		t.Fatalf("rejection must name the contract violation, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no commit beyond the frozen base") {
		t.Fatalf("rejection must name the missing commit evidence, got: %s", rec.Body.String())
	}

	// The join must NOT have advanced: the branch instance stays running.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil || !found {
		t.Fatalf("parent run lookup: %v %v", found, err)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.BranchID == "ws_a" && inst.Status == "completed" {
			t.Fatal("no-delivery branch instance must not be completed")
		}
	}

	// Branch ws_b does the honest work: commit + push on ITS OWN branch,
	// inside its own materialized worktree.
	deliver := func(childID string, file, msg string) {
		t.Helper()
		task, err := s.ts.GetTask("sample", "pm", childID)
		if err != nil {
			t.Fatal(err)
		}
		wt := task.WorktreeDir
		if wt == "" {
			t.Fatalf("branch child %s must have a worktree", childID)
		}
		if err := os.WriteFile(filepath.Join(wt, file), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, wt, "add", ".")
		gitRun(t, wt, "commit", "-m", msg)
		gitRun(t, wt, "push", "origin", task.BranchName)
	}
	honestID := childIDs["ws_b"]
	deliver(honestID, "module_ws_b.go", "ws_b delivery")
	rec = postBranchStepComplete(t, s, workspaceID, honestID, map[string]string{
		"branch_summary": "delivered and pushed",
		"touched_paths":  "module_ws_b.go",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("honest branch must complete, got %d: %s", rec.Code, rec.Body.String())
	}

	// The rejecting branch then delivers (its uncommitted.go becomes part
	// of the delivery commit) and completes too — join proceeds.
	deliver(noDelivery, "module_ws_a.go", "ws_a delivery (late)")
	rec = postBranchStepComplete(t, s, workspaceID, noDelivery, map[string]string{
		"branch_summary": "delivered late",
		"touched_paths":  "module_ws_a.go\nuncommitted.go",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("late-delivery branch must complete, got %d: %s", rec.Code, rec.Body.String())
	}

	parentRun, found, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil || !found {
		t.Fatalf("parent run lookup: %v %v", found, err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("parent run must complete once both branches deliver, got %q@%q", parentRun.Status, parentRun.ActiveStepID)
	}
}

// postRuntimeComplete posts to the plain-task runtime completion endpoint
// (handleRuntimeTaskComplete) with an injected runtime principal.
func postRuntimeComplete(t *testing.T, s *Server, workspaceID, taskID string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+taskID+"/complete", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      "sample",
		Agent:        "pm",
		Capabilities: []string{"task.use"},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeTaskComplete(rec, req)
	return rec
}

// Review round 3, item 3 (entry B): plain tasks completed via the runtime
// completion endpoint are gated by the same contract — a success claim
// without the git delivery is flipped to done_failed with the violation on
// the task record.
func TestRuntimeTaskCompleteDeliveryContractGatesPlainTask(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutGitRemote(t, s, gitRoot)

	now := time.Now().UTC()
	task := &entity.Task{
		ID: "task-plain-contract", Title: "Plain contracted task", Status: entity.TaskStatusPending,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main",
		Vars: map[string]string{
			"MULTIGENT_DELIVERY_CONTRACT": `{"requireGitCommit":true,"requirePush":true}`,
		},
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	wtDir, branchName, err2, _ := s.worktreeMgr.EnsureWorktreeAt(gitRoot, task.ID, baseCommit, task.BranchName)
	if err2 != nil {
		t.Fatal(err2)
	}
	task.WorktreeDir = wtDir
	task.BranchName = branchName
	if err := s.ts.PersistTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}

	// Complete claiming success with zero commits — must flip to failure.
	rec := postRuntimeComplete(t, s, workspaceID, task.ID, map[string]string{
		"agent":   "pm",
		"status":  "success",
		"summary": "empty-handed success",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete endpoint must still 200 (status flips server-side), got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("no-delivery success must flip to done_failed, got %q (err=%q)", stored.Status, stored.LastError)
	}
	if !strings.Contains(stored.LastError, "delivery contract unmet") {
		t.Fatalf("task error must carry the violation, got %q", stored.LastError)
	}

	// Now deliver for real (commit + push), then re-complete — must succeed.
	if err := os.WriteFile(filepath.Join(wtDir, "delivery.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wtDir, "add", ".")
	gitRun(t, wtDir, "commit", "-m", "plain task delivery")
	gitRun(t, wtDir, "push", "origin", branchName)

	rec = postRuntimeComplete(t, s, workspaceID, task.ID, map[string]string{
		"agent":   "pm",
		"status":  "success",
		"summary": "delivered",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("honest completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	stored, err = s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != entity.TaskStatusDoneSuccess {
		t.Fatalf("honest delivery must complete successfully, got %q (err=%q)", stored.Status, stored.LastError)
	}
}
