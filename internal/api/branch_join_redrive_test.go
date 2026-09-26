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

	controldb "github.com/multigent/multigent/internal/db"
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

// T6 — D-J: a console restart mid-run leaves the workflow run FAILED, and a
// failed run refuses re-entry forever. Manual start must re-activate exactly
// the failed step and dispatch the agent again.
func TestManualStartReactivatesFailedWorkflowRunOnItsStep(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedFanoutParentRun(t, s, workspaceID, baseCommit)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, _, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	// Advance to the fan-out stage, then fail the run exactly like a killed
	// in-flight agent does (the platform's own failure path).
	if _, err := wfStore.CompleteAndAdvance("sample", "task-fanout-root", "contract frozen", "", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}, "completed"); err != nil {
		t.Fatalf("advance to parallel: %v", err)
	}
	run, _, err = wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	parallelStep := run.ActiveStepID
	if parallelStep == "" {
		t.Fatalf("fixture must park the run on the parallel stage")
	}
	if _, err := wfStore.CompleteAndAdvance("sample", "task-fanout-root", "agent died with the console", "", nil, "failed"); err != nil {
		t.Fatalf("fail the stage: %v", err)
	}
	run, _, err = wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	// The parallel stage is not an agent step: reactivation must refuse it
	// rather than burn a run on a workflow-owned stage.
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/task-fanout-root/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", "task-fanout-root")
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not a task start") {
		t.Fatalf("reactivating a non-agent step must be refused with the workflow reason: %d %s", rec.Code, rec.Body.String())
	}
	if run, _, _ = wfStore.RunForTask("sample", "task-fanout-root"); strings.TrimSpace(run.Status) != "failed" {
		t.Fatalf("a refused reactivation must not touch the run, got %q", run.Status)
	}

	// The agent-owned failure is the shape the lever answers: reactivate and
	// dispatch. Drive the store directly to build it (the fixture's parallel
	// stage has no agent step to fail on).
	agentStepDef := entity.WorkflowDefinition{
		ID: "wf-reactivate-child", Name: "child", Version: 1, Scope: "workspace", StartStepID: "work",
		Steps: []entity.WorkflowStep{{
			ID: "work", Type: "agent_task", Title: "work", ActorRole: "pm-agent",
		}},
	}
	if err := wfStore.SaveDefinition(&agentStepDef); err != nil {
		t.Fatal(err)
	}
	childTask := &entity.Task{ID: "task-reactivate", Title: "reactivate", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.ts.AddTask("sample", "pm", childTask); err != nil {
		t.Fatal(err)
	}
	childRun, childInstances, err := wfStore.StartRun("sample", childTask.ID, agentStepDef.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wfStore.CompleteAndAdvance("sample", childTask.ID, "died with the console", "", nil, "failed"); err != nil {
		t.Fatalf("fail the agent step: %v", err)
	}
	req2 := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+childTask.ID+"/start", "admin", nil)
	req2.SetPathValue("name", "sample")
	req2.SetPathValue("taskId", childTask.ID)
	rec2 := httptest.NewRecorder()
	s.handleStartProjectTask(rec2, req2)
	// The dispatch itself may be refused by runtime readiness in this fixture;
	// the RUN state is what this lever owns and must be restored either way.
	run2, _, err := wfStore.RunForTask("sample", childTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(run2.Status) != "active" || strings.TrimSpace(run2.ActiveStepID) != "work" {
		t.Fatalf("failed agent step must be re-activated by manual start, got %q@%q (resp %d %s)",
			run2.Status, run2.ActiveStepID, rec2.Code, rec2.Body.String())
	}
	instances, err := wfStore.ListStepInstances(childRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundPending := false
	for _, inst := range instances {
		if inst.StepID == "work" && inst.Status == "pending" {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatalf("the re-activated step instance must be pending again, got %+v", instances)
	}
	_ = childInstances
}

// TestManualStartBusySchedulerSessionQueuesViaAttention is the S2 hardening
// batch 2 regression for the run4 finding (qa recovery needed TWO /start
// calls). The manual-start lever re-activates the failed run and then spawns
// `multigent run --task` fire-and-forget; a live scheduler interaction
// session holds the agent's CLI lock, so the spawned process exited
// immediately ("agent is busy in scheduler session from scheduler") AFTER the
// API had already reported ok+pid — the manual start silently did nothing
// (managed-manual-task log, 2026-09-26T07:03:17Z). The fix checks the same
// interaction lock on the API side and falls back to the platform's own
// busy-path semantics: record an attention signal and fire the task trigger
// so the session-holding scheduler cycle picks the task up itself, with an
// honest queued_via_attention response.
func TestManualStartBusySchedulerSessionQueuesViaAttention(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	// A single agent_task step is the shape the lever owns.
	def := entity.WorkflowDefinition{
		ID: "wf-busy-recovery", Name: "busy recovery", Version: 1, Scope: "workspace", StartStepID: "work",
		Steps: []entity.WorkflowStep{{
			ID: "work", Type: "agent_task", Title: "work", ActorRole: "pm-agent",
		}},
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	task := &entity.Task{ID: "task-busy-recovery", Title: "busy recovery", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := wfStore.CompleteAndAdvance("sample", task.ID, "agent died mid-step", "", nil, "failed"); err != nil {
		t.Fatalf("fail the agent step: %v", err)
	}

	// The fixture worker runs on the "human" model so the sandbox/readiness
	// ladder is a no-op and the test isolates exactly the interaction-lock
	// behavior (the run4 agent had a configured runtime; the collision is
	// orthogonal to readiness).
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID: "aw-pm", WorkspaceID: workspaceID, Name: "pm", DisplayName: "pm",
		Model: "human", Status: "available",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	// Enable attention on the pm membership so the fallback can record a
	// signal (the fixture seeder leaves it disabled).
	membership, ok, err := s.controlDB.ProjectMembershipByID(workspaceID, "pm-sample-pm")
	if err != nil || !ok {
		t.Fatalf("membership lookup: ok=%v err=%v", ok, err)
	}
	membership.AttentionEnabled = true
	membership.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.controlDB.UpsertProjectMembership(membership); err != nil {
		t.Fatal(err)
	}

	// A LIVE scheduler interaction session on the agent: exactly what the
	// first run4 /start collided with (heartbeat PID was 0, so the existing
	// ladders passed and the doomed spawn slipped through).
	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.controlDB.CreateInteractionSession(controldb.InteractionSession{
		ID: "sess-busy-fixture", WorkspaceID: workspaceID, AgentWorkerID: "aw-pm",
		ProjectID: "sample", AgentID: "pm", SourceKind: "scheduler", SourceChannel: "scheduler",
		ActorType: "system", ActorID: "scheduler", Status: "active",
		LockReason: "running_task", MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Stub the scheduler spawn with a binary that exits busy immediately —
	// the shape the doomed run4 spawn had. With the fix in place the API must
	// never reach this spawn; without the busy check (reverse validation) the
	// spawn succeeds and the API returns the ok+pid lie this regression
	// forbids.
	stub := filepath.Join(t.TempDir(), "stub-multigent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.sched = newSchedulerManager(t.TempDir())
	s.sched.binPath = stub

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual start under a live scheduler session must queue via attention, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "queued_via_attention" {
		t.Fatalf("response must be honest about the attention hand-off, got status %q body %s", resp.Status, rec.Body.String())
	}
	// The run must STILL be re-activated (the lever's own contract is intact).
	run, _, err := wfStore.RunForTask("sample", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "active" || run.ActiveStepID != "work" {
		t.Fatalf("failed agent step must still be re-activated by manual start, got %q@%q", run.Status, run.ActiveStepID)
	}
	// The attention hand-off must be durable: a pending task_assigned signal
	// for this agent/task exists (the session-holding cycle polls these).
	signals, err := s.controlDB.ListAttentionSignals(controldb.AttentionSignalFilter{
		WorkspaceID: workspaceID,
		Statuses:    []string{"pending"},
		Limit:       50,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundSignal := false
	for _, sig := range signals {
		if sig.SourceKind == "task" && sig.SourceID == task.ID && strings.Contains(strings.ToLower(sig.Reason), "task_assigned") {
			foundSignal = true
		}
	}
	if !foundSignal {
		t.Fatalf("manual start under a live scheduler session must record a pending attention signal for the hand-off, got %+v", signals)
	}

	// A STALE scheduler session (idle beyond the shared 2-minute recovery
	// window) must NOT take the attention fallback: the spawn path is allowed
	// through exactly like the CLI's own stale recovery would.
	stale := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	row, _, err := s.controlDB.ActiveInteractionSessionForWorker(workspaceID, "aw-pm")
	if err != nil {
		t.Fatal(err)
	}
	row.UpdatedAt = stale
	row.LastActivityAt = stale
	if err := s.controlDB.UpdateInteractionSession(row); err != nil {
		t.Fatal(err)
	}
	// Mark the first signal handled so the second /start records a fresh one.
	for _, sig := range signals {
		if sig.SourceKind == "task" && sig.SourceID == task.ID {
			_ = s.controlDB.MarkAttentionSignalStatus(workspaceID, sig.ID, "handled")
		}
	}
	req2 := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req2.SetPathValue("name", "sample")
	req2.SetPathValue("taskId", task.ID)
	rec2 := httptest.NewRecorder()
	s.handleStartProjectTask(rec2, req2)
	if rec2.Code == http.StatusConflict && strings.Contains(rec2.Body.String(), "busy in a scheduler session") {
		t.Fatalf("a stale scheduler session must not block the manual start: %d %s", rec2.Code, rec2.Body.String())
	}
}
