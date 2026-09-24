package api

// Regression tests for the S2 round 7 fan-out defects found by the real run on
// 2026-09-24 (D-F: the worktree reaper retired delivery worktrees whose join
// was still parked; D-G: no entry point could re-drive that join afterwards).
//
// Reverse validation: with the reaper guard removed, T1 reclaims the parked
// branch's worktree (and asserts the sweep must NOT); with the manual-start
// re-drive removed, T2 falls through to the "task is already finished" refusal
// and the join never resolves.

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
	"github.com/multigent/multigent/internal/gitworktree"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedFanoutProjectRepo materializes the production shape for ONE branch child:
// a real project repo with a frozen base commit, the platform-materialized
// delivery worktree (capture-time baseline persisted to the control plane), and
// a committed deliverable on the task branch.
func seedFanoutProjectRepo(t *testing.T, s *Server, workspaceID, project, taskID string) (gitRoot, wtDir, baseCommit, deliveryCommit string) {
	t.Helper()
	if s.worktreeMgr == nil {
		s.worktreeMgr = gitworktree.NewManager()
	}
	// Production layout: the project git root is <projectDir>/workspace, and
	// the resolver must be able to see it BEFORE materialization runs.
	gitRoot = filepath.Join(s.st.ProjectDir(project), "workspace")
	if err := os.MkdirAll(gitRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v (%s): %v (%s)", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(gitRoot, "init", "-b", "main")
	git(gitRoot, "config", "user.email", "t@t")
	git(gitRoot, "config", "user.name", "t")
	for name, content := range map[string]string{
		"server.go":      "package main\n",
		"server_test.go": "package main\n",
		".mcp.json":      "{}",
	} {
		if err := os.WriteFile(filepath.Join(gitRoot, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(gitRoot, "add", ".")
	git(gitRoot, "commit", "-m", "base")
	baseCommit = git(gitRoot, "rev-parse", "HEAD")
	if got := s.resolveProjectGitRoot(project); got != gitRoot {
		t.Fatalf("fixture git root mismatch: resolver=%q want=%q", got, gitRoot)
	}

	// Platform materialization path (same call the fan-out uses), including the
	// capture-time baseline that becomes the authoritative control-plane record.
	dir, branch, err, capture := s.worktreeMgr.EnsureWorktreeAt(gitRoot, taskID, baseCommit, "feature/wf-test-"+taskID)
	if err != nil {
		t.Fatalf("materialize worktree: %v", err)
	}
	if capture.Baseline.Entries == nil {
		t.Fatalf("materialization must produce a baseline capture")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.CaptureQABaselineRecord(project, taskID, dir); err != nil {
		t.Fatalf("persist qa baseline: %v", err)
	}
	wtDir = dir

	// The agent's deliverable lands on the task branch, inside the worktree.
	if err := os.WriteFile(filepath.Join(wtDir, "server.go"), []byte("package main\n\nfunc Delivered() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(wtDir, "add", "server.go")
	git(wtDir, "commit", "-m", "deliver")
	deliveryCommit = git(wtDir, "rev-parse", "HEAD")
	if branch == "" {
		t.Fatalf("materialization must report the branch")
	}
	return gitRoot, wtDir, baseCommit, deliveryCommit
}

func branchChildTaskForRedrive(t *testing.T, s *Server, workspaceID, gitRoot, wtDir, baseCommit string, recordDeclaration bool) *entity.Task {
	t.Helper()
	task, childRunID := seedBranchChildRun(t, s, workspaceID)
	// The production fan-out stamps the frozen base + the run-scoped branch on
	// the child task; the fixture seeds the vars, this fills the delivery
	// coordinates (worktree path as materialized by the platform, not guessed).
	task.BaseCommit = baseCommit
	task.BaseBranch = "main"
	task.BranchName = "feature/wf-test-" + task.ID
	task.WorktreeDir = wtDir
	if err := s.ts.UpdateTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if !recordDeclaration {
		return task
	}
	// Replay the child run's recorded declaration exactly as the real run left
	// it: the agent's own step output, persisted before the (rejected) join.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.ListStepInstances(childRunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) == 0 {
		t.Fatalf("child run must have a step instance to record outputs on")
	}
	inst := instances[0]
	inst.Status = "completed"
	inst.OutputValues = map[string]string{
		"branch_summary": "delivered the branch work",
		"touched_paths":  "server.go",
	}
	inst.UpdatedAt = time.Now().UTC()
	if err := wfStore.SaveStepInstance(&inst); err != nil {
		t.Fatal(err)
	}
	return task
}

// T1 — D-F: a terminal branch child whose join is still parked keeps its
// delivery worktree across the hourly sweep; once the join resolves, the sweep
// reclaims it.
func TestWorktreeReaperKeepsParkedBranchJoinWorktree(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	gitRoot, wtDir, baseCommit, _ := seedFanoutProjectRepo(t, s, workspaceID, "sample", "task-join-child")
	task := branchChildTaskForRedrive(t, s, workspaceID, gitRoot, wtDir, baseCommit, true)
	// Terminal, exactly like the archived branch children of the real run.
	task.Status = entity.TaskStatusDoneSuccess
	if err := s.ts.UpdateTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("delivery worktree must exist before the sweep: %v", err)
	}

	// The join is parked (branch instance running, parent run still on the
	// stage): the sweep must keep the evidence tree.
	s.sweepTerminalTaskWorktrees(context.Background())
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("parked branch join must keep its delivery worktree, got %v", err)
	}

	// The agent's recorded completion (the production report path) resolves the
	// join; the branch is no longer parked, so the next sweep reclaims it.
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, map[string]string{
		"branch_summary": "delivered the branch work",
		"touched_paths":  "server.go",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("recorded completion must be accepted: %d %s", rec.Code, rec.Body.String())
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRun, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.ActiveStepID == "parallel" {
		t.Fatalf("join must have moved the parent run off the stage, still %q", parentRun.ActiveStepID)
	}
	s.sweepTerminalTaskWorktrees(context.Background())
	if _, err := os.Stat(wtDir); err == nil {
		t.Fatalf("resolved branch join must release its worktree to the reaper")
	}
}

// T2 — D-G: after the reaper retired both evidence trees of the real fan-out,
// manual start is the only lever left. It must restore the tree from the
// recorded branch, keep the ORIGINAL capture-time baseline as the measurement
// origin, replay the agent's recorded declaration, and resolve the join.
func TestManualStartRedrivesParkedBranchJoinAfterWorktreeLoss(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	gitRoot, wtDir, baseCommit, deliveryCommit := seedFanoutProjectRepo(t, s, workspaceID, "sample", "task-join-child")
	task := branchChildTaskForRedrive(t, s, workspaceID, gitRoot, wtDir, baseCommit, true)

	// Simulate the reaper: the platform's own retire sequence removes the
	// worktree while leaving the delivered branch ref in the repo.
	if err := s.worktreeMgr.CleanupWorktree(gitRoot, task.ID); err != nil {
		t.Fatalf("retire worktree: %v", err)
	}
	if _, err := os.Stat(wtDir); err == nil {
		t.Fatalf("worktree must be gone to reproduce the incident")
	}
	// mark terminal AFTER the retirement, like the archived real branches.
	task.Status = entity.TaskStatusDoneSuccess
	task.LastError = "delivery contract unmet: git delivery evidence unreadable (git ls-remote: exit status 128)"
	if err := s.ts.UpdateTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual start must re-drive the parked branch join: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	if body["status"] != "branch_join_resumed" {
		t.Fatalf("expected branch_join_resumed, got %s", rec.Body.String())
	}

	// 1. The evidence tree is back, checked out at the DELIVERED commit.
	head := exec.Command("git", "-C", wtDir, "rev-parse", "HEAD^{commit}")
	out, err := head.CombinedOutput()
	if err != nil {
		t.Fatalf("read restored worktree head: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != deliveryCommit {
		t.Fatalf("restored worktree must be at the delivery %s, got %s", deliveryCommit, got)
	}
	// 2. The baseline mirror is the ORIGINAL control-plane record, byte for byte.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	payload, found, err := wfStore.LoadQABaselinePayload("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("control-plane baseline must survive the restore (found=%v err=%v)", found, err)
	}
	mirror, err := os.ReadFile(gitworktree.QABaselinePath(wtDir))
	if err != nil {
		t.Fatalf("restored worktree must carry the recovery mirror: %v", err)
	}
	if string(mirror) != payload {
		t.Fatalf("restored mirror must equal the authoritative record (measurement origin must not move)")
	}
	if _, err := os.Stat(gitworktree.QABaselineManifestPath(wtDir)); err != nil {
		t.Fatalf("restored worktree must carry the capture manifest: %v", err)
	}
	// 3. The join resolved: branch instance completed, parent advanced off the
	// stage, and the stale rejection reason is cleared.
	branches, err := wfStore.BranchInstancesForStep(task.Vars[workflowRunIDVar], "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Status != "completed" {
		t.Fatalf("branch instance must be completed after the re-drive, got %+v", branches)
	}
	parentRun, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.ActiveStepID == "parallel" {
		t.Fatalf("parent run must leave the stage after the re-driven join")
	}
	child, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.LastError != "" {
		t.Fatalf("a resolved join must clear the stale rejection reason, got %q", child.LastError)
	}
}

// T3 — D-F residue: a terminal (or cancelled) run can still carry step
// instances from an earlier incarnation (the wfr-07249bes leftovers). Those
// must NOT keep a worktree alive forever.
func TestWorktreeReaperReclaimsTerminalRunResidue(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	gitRoot, wtDir, baseCommit, _ := seedFanoutProjectRepo(t, s, workspaceID, "sample", "task-join-child")
	task := branchChildTaskForRedrive(t, s, workspaceID, gitRoot, wtDir, baseCommit, true)
	task.Status = entity.TaskStatusDoneSuccess
	if err := s.ts.UpdateTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	// Residue shape: run terminal, stale ActiveStepID left pointing at the stage.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "cancelled"
	run.ActiveStepID = "parallel"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	s.sweepTerminalTaskWorktrees(context.Background())
	if _, err := os.Stat(wtDir); err == nil {
		t.Fatalf("a terminal run's residue must not keep a worktree alive")
	}
}

// T4 — D-F resolution: a branch the workflow already resolved as failed keeps
// no claim on its worktree even while the stage waits for its siblings.
func TestWorktreeReaperReclaimsFailedBranchWorktree(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	gitRoot, wtDir, baseCommit, _ := seedFanoutProjectRepo(t, s, workspaceID, "sample", "task-join-child")
	task := branchChildTaskForRedrive(t, s, workspaceID, gitRoot, wtDir, baseCommit, true)
	task.Status = entity.TaskStatusDoneSuccess
	if err := s.ts.UpdateTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.BranchInstancesForStep(task.Vars[workflowRunIDVar], "parallel")
	if err != nil || len(instances) != 1 {
		t.Fatalf("fixture must have exactly one branch instance (err=%v n=%d)", err, len(instances))
	}
	instances[0].Status = "failed"
	if err := wfStore.SaveBranchInstance(&instances[0]); err != nil {
		t.Fatal(err)
	}
	s.sweepTerminalTaskWorktrees(context.Background())
	if _, err := os.Stat(wtDir); err == nil {
		t.Fatalf("a failed branch must not hold its worktree through the sweep")
	}
}

// T5 — D-G guard rail: without the agent's own recorded declaration the
// re-drive must refuse instead of letting an empty declaration reach the gate.
func TestManualStartRefusesRedriveWithoutRecordedDeclaration(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	gitRoot, wtDir, baseCommit, _ := seedFanoutProjectRepo(t, s, workspaceID, "sample", "task-join-child")
	task := branchChildTaskForRedrive(t, s, workspaceID, gitRoot, wtDir, baseCommit, false)
	if err := s.worktreeMgr.CleanupWorktree(gitRoot, task.ID); err != nil {
		t.Fatalf("retire worktree: %v", err)
	}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-drive without a recorded declaration must be refused: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no recorded step declaration") {
		t.Fatalf("refusal must name the missing declaration, got %s", rec.Body.String())
	}
}
