package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// D-M regression (hooks-relay rehearsal, run 3): the fan-out aborted before
// materializing any branch because the baseline resolver fetched origin/main
// on the host (no credentials by design) and fell back to a local origin/main
// that a freshly created project never had. The baseline must come from
// immutable, locally known SHAs instead.
//
// The fixture has no remote and no origin/* refs at all, so any fetch-based
// resolution fails: materializing here proves the local path was taken.
func TestFanoutMaterializesForFreshProjectWithoutRemote(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, headCommit := buildFanoutGitWorkspace(t, s)
	// The exact D-M shape: a remote is configured (so the resolver tries to
	// fetch) but it is unreachable without credentials and the workspace has
	// never fetched, so no local origin/main exists either.
	unreachable := "http://127.0.0.1:1/hooks-relay.git"
	if out, err := exec.Command("git", "-C", gitRoot, "remote", "add", "origin", unreachable).CombinedOutput(); err != nil {
		t.Fatalf("remote add: %v (%s)", err, out)
	}
	if err := exec.Command("git", "-C", gitRoot, "rev-parse", "--verify", "origin/main").Run(); err == nil {
		t.Fatalf("fixture must not have a local origin/main ref")
	}
	// Parent carries neither a BaseCommit nor a CompletionCommit: the
	// resolver must fall through to the workspace HEAD.
	task := seedFanoutParentRun(t, s, workspaceID, "")
	if strings.TrimSpace(task.BaseCommit) != "" {
		t.Fatalf("fixture base commit must be empty, got %q", task.BaseCommit)
	}

	rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("fan-out for a remote-less project must succeed: %d %s", rec.Code, rec.Body.String())
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, _, err := wfStore.RunForTask("sample", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel")
	if err != nil || len(instances) != 2 {
		t.Fatalf("branch instances: %v %d, want 2", err, len(instances))
	}
	parentTask, err := s.ts.GetTask("sample", "pm", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	pinned := parentTask.Vars[workflowFanoutBaseCommitVar]
	if pinned != headCommit {
		t.Fatalf("pinned base %q != workspace HEAD %q (the baseline must be the local immutable commit)", pinned, headCommit)
	}
}

// The fan-out must keep preferring the parent task's completion snapshot over
// the workspace HEAD, so a re-drive after the workspace moved still branches
// from the snapshot.
func TestFanoutPrefersCompletionSnapshotOverWorkspaceHead(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, firstCommit := buildFanoutGitWorkspace(t, s)
	task := seedFanoutParentRun(t, s, workspaceID, "")
	task.CompletionCommit = firstCommit
	if err := s.ts.PersistTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	// Move the workspace HEAD forward: the snapshot must win.
	moveFanoutCommit(t, gitRoot, "workspace moved after the snapshot")

	rec := postBranchStepComplete(t, s, workspaceID, "task-fanout-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("fan-out must succeed: %d %s", rec.Code, rec.Body.String())
	}
	parentTask, err := s.ts.GetTask("sample", "pm", "task-fanout-root")
	if err != nil {
		t.Fatal(err)
	}
	if pinned := parentTask.Vars[workflowFanoutBaseCommitVar]; pinned != firstCommit {
		t.Fatalf("pinned base %q != completion snapshot %q", pinned, firstCommit)
	}
}

func moveFanoutCommit(t *testing.T, gitRoot, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(gitRoot, "moved.txt"), []byte(message+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", message}, {"rev-parse", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = gitRoot
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+gitRoot)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		if args[0] == "rev-parse" {
			return trimGitOut(string(out))
		}
	}
	return ""
}

// Project sync must go through the platform git layer instead of the removed
// agent-context `sync` CLI (which always failed with `unknown flag: --project`).
// A file remote needs no credentials, so it exercises the success path; a
// project without a remote must report a git error, never a flag error.
func TestProjectSyncUsesPlatformGitLayer(t *testing.T) {
	s, _ := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, _ := buildFanoutGitWorkspace(t, s)

	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v (%s)", err, out)
	}
	for _, args := range [][]string{{"remote", "add", "origin", bare}, {"push", "origin", "main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = gitRoot
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+gitRoot)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	rec := postProjectSyncForTest(t, s, "sample", "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("sync with a file remote must succeed: %d %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["ok"] != true {
		t.Fatalf("ok=false: %v", payload)
	}
	if strings.TrimSpace(payload["remoteRefCommit"].(string)) == "" {
		t.Fatalf("remote ref commit missing: %v", payload)
	}
	if strings.Contains(rec.Body.String(), "unknown flag") {
		t.Fatalf("sync must not fail on CLI flags: %s", rec.Body.String())
	}

	// Remove the remote: the handler must fail with a git-layer error.
	cmd := exec.Command("git", "remote", "remove", "origin")
	cmd.Dir = gitRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote remove: %v (%s)", err, out)
	}
	rec = postProjectSyncForTest(t, s, "sample", "admin")
	if rec.Code == http.StatusOK {
		t.Fatalf("sync without any remote must not report success: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "unknown flag") {
		t.Fatalf("sync failure must come from the git layer, not CLI flags: %s", rec.Body.String())
	}
}

func postProjectSyncForTest(t *testing.T, s *Server, project, user string) *httptest.ResponseRecorder {
	t.Helper()
	if err := s.controlDB.UpsertWorkspaceMember(s.workspaceIDForTest(t), user, WorkspaceRoleAdmin); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/sync", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", project)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, user))
	rec := httptest.NewRecorder()
	s.handlePostProjectSync(rec, req)
	return rec
}

func (s *Server) workspaceIDForTest(t *testing.T) string {
	t.Helper()
	id, err := s.currentWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// P1 (D-M review): the frozen plan must pin the pushed contract baseline the
// run recorded, instead of leaving the fan-out to fall back to a workspace
// HEAD that can predate the contract work. The plan then names an immutable
// SHA that the digest covers.
func TestPlanFreezePinsPushedContractBaseline(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	gitRoot, scaffoldCommit := buildFanoutGitWorkspace(t, s)
	// The contract work lands in the workspace and is pushed: this is the SHA
	// the agent reported in contract_artifacts.
	contractCommit := moveFanoutCommit(t, gitRoot, "contract baseline pushed by the sandbox")
	if contractCommit == scaffoldCommit {
		t.Fatalf("fixture contract commit must differ from the scaffold")
	}
	seedGreenfieldRunTask(t, s, workspaceID, "task-dm-base", "")
	driveToContractReviewWithContractArtifacts(t, s, workspaceID, "task-dm-base", samplePlanJSON(), contractCommit)

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-dm-base")
	if run.ActiveStepID != "contract_review" {
		t.Fatalf("chain must park at contract_review, got %s", run.ActiveStepID)
	}
	if status, err := approveContractReview(t, s, workspaceID, "task-dm-base"); err != nil {
		t.Fatalf("contract_review approve failed (%d): %v", status, err)
	}
	_, version, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil || !ok {
		t.Fatalf("approve must freeze the plan: ok=%v err=%v", ok, err)
	}
	if version.Plan.ResolvedAtBaseCommit != contractCommit {
		t.Fatalf("frozen plan base %q != pushed contract baseline %q", version.Plan.ResolvedAtBaseCommit, contractCommit)
	}
	// And the materialized branches must be based on that pinned SHA, not on
	// whatever the workspace HEAD happens to be.
	run = runForTask(t, s, workspaceID, "task-dm-base")
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel_workstreams")
	if err != nil || len(instances) == 0 {
		t.Fatalf("wave 0 instances: %v %d", err, len(instances))
	}
	for _, inst := range instances {
		task, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.BaseCommit != contractCommit {
			t.Fatalf("branch %s base %q != frozen plan base %q", inst.BranchID, task.BaseCommit, contractCommit)
		}
	}
}

// A malformed baseline claim must refuse the freeze: silently ignoring it would
// let the next candidate re-baseline the derived tasks.
func TestPlanFreezeRefusesMalformedBaselineClaim(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-dm-bad", "")
	driveToContractReviewWithContractArtifacts(t, s, workspaceID, "task-dm-bad", samplePlanJSON(), "c84b3a5")

	if status, err := approveContractReview(t, s, workspaceID, "task-dm-bad"); err == nil {
		t.Fatalf("approve with a short baseline claim must fail (status %d)", status)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-dm-bad")
	if _, _, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID); err != nil || ok {
		t.Fatalf("no plan may be frozen after a refused baseline claim (ok=%v err=%v)", ok, err)
	}
}

// driveToContractReviewWithContractArtifacts is driveToContractReview with the
// contract_artifacts output carrying a real pushed baseline.
func driveToContractReviewWithContractArtifacts(t *testing.T, s *Server, workspaceID, taskID, planJSON, baselineCommit string) {
	t.Helper()
	rec := postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"requirement_draft": "The console must clear local session state on expiry and export audit logs.",
		"open_questions":    "none",
		"requirement_items": requirementItemsJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("requirement_draft completion: %d %s", rec.Code, rec.Body.String())
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "requirement approved",
	}); err != nil {
		t.Fatalf("requirement_review approve (%d): %v", status, err)
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "design approved with waiver",
		Outputs: map[string]string{"design_waiver_reason": "D-M regression fixture: server-only delivery"},
	}); err != nil {
		t.Fatalf("design_review approve (%d): %v", status, err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"scale_verdict": "batched",
		"delivery_plan": planJSON,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("scale_gate completion: %d %s", rec.Code, rec.Body.String())
	}
	rec = postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"contract_artifacts": fmt.Sprintf(`{"baseline_commit":%q,"repo":"root/sample","branch":"main"}`, baselineCommit),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("contract_batch completion: %d %s", rec.Code, rec.Body.String())
	}
}
