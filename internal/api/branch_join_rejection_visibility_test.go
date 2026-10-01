package api

// B2 regression: a branch child's join re-check (the QA touched_paths gate
// inside CompleteBranchAndMaybeAdvance) that rejects a human-approved
// completion must leave the rejection reason ON THE TASK RECORD, not only in
// the HTTP error the reviewer's client happens to render. The two console /
// ChatOps review entries persisted done_success first and then delegated to
// completeRuntimeWorkflowBranch; a rejection there used to vanish — the task
// looked finished, the branch instance stayed "running", and nothing on the
// record said why the stage never advanced.
//
// The runtime agent path already had this rejection-visibility contract
// (review round 4 P0-1, resumeArchivedBranchJoin / completeRuntimeWorkflow
// BranchHTTP); these tests pin the SAME contract for the two remaining
// review entries, plus the retry lever: a rejected branch child stays
// re-drivable (its recorded declaration replayed by the manual-start
// branch-join resume) once the delivery is corrected on disk.
//
// Fixture contract: the review body carries ONLY the branch step's declared
// outputs (branch_summary/touched_paths). The review entries inject
// body.Decision/body.Comments into the completion outputs, and the branch
// child's step declares neither — an approval decision here would be
// rejected by the output whitelist BEFORE the join gate ever runs (the
// wrong rejection, at the wrong layer). A branch join is driven by the
// terminal step report itself; there is no decision field to approve.

import (
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

// branchJoinReviewOutputs builds the review body's structured outputs exactly
// as the branch child's step contract declares them, with the recorded
// (deliberately wrong) declaration the join gate must reject.
func branchJoinReviewOutputs(branchSummary, touchedPaths string) map[string]string {
	return map[string]string{
		"branch_summary": branchSummary,
		"touched_paths":  touchedPaths,
	}
}

// seedParkedJoinChild seeds a branch child whose join is PARKED with a
// recorded declaration the worktree cannot back: server.go was really
// edited+committed, but the child run's step outputs declare not_touched.md.
// Returns the child task (with WorktreeDir/BaseCommit/BranchName stamped) and
// the branch instance's child run ID.
func seedParkedJoinChild(t *testing.T, s *Server, workspaceID, declaredPath string) *entity.Task {
	t.Helper()
	s.worktreeMgr = gitworktreeManagerForTest()
	// seedFanoutProjectRepo already commits the branch's real deliverable
	// (server.go) on the task branch inside the platform-materialized
	// worktree.
	gitRoot, wtDir, baseCommit, _ := seedFanoutProjectRepo(t, s, workspaceID, "sample", "task-join-child")
	task := branchChildTaskForRedrive(t, s, workspaceID, gitRoot, wtDir, baseCommit, false)

	// Record the declaration on the child run's step instance exactly what the
	// agent's step report persists before the join runs.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.BranchInstancesForStep(task.Vars[workflowRunIDVar], task.Vars[workflowStepIDVar])
	if err != nil || len(instances) != 1 {
		t.Fatalf("branch instance lookup: %v (n=%d)", err, len(instances))
	}
	childRunID := instances[0].ChildRunID
	stepInstances, err := wfStore.ListStepInstances(childRunID)
	if err != nil || len(stepInstances) == 0 {
		t.Fatalf("child step instances: %v (n=%d)", err, len(stepInstances))
	}
	inst := stepInstances[0]
	inst.Status = "completed"
	inst.OutputValues = map[string]string{
		"branch_summary": "did things",
		"touched_paths":  declaredPath,
	}
	inst.UpdatedAt = time.Now().UTC()
	if err := wfStore.SaveStepInstance(&inst); err != nil {
		t.Fatal(err)
	}
	return task
}

func gitCommitAll(t *testing.T, wtDir, message string) {
	t.Helper()
	cmd := exec.Command("git", "-C", wtDir, "add", "-A")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+wtDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	cmd = exec.Command("git", "-C", wtDir, "commit", "-m", message)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+wtDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
}

// TestConsoleReviewJoinRejectionWritesLastError: approving a branch step from
// the console review must not swallow a join re-check rejection. The 400
// reaches the reviewer's client, but the reason must ALSO land on the task
// record with the branch instance still parked (retryable shape).
func TestConsoleReviewJoinRejectionWritesLastError(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task := seedParkedJoinChild(t, s, workspaceID, "not_touched.md")

	_, status, err := s.submitTaskWorkflowReview(
		httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", task.ID,
		workflowReviewBody{Outputs: branchJoinReviewOutputs("did things", "server.go\nnot_touched.md")},
	)
	if err == nil {
		t.Fatal("a join-rejected branch approval must surface an error to the reviewer")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("review status=%d, want 400", status)
	}

	// The rejection reason must be VISIBLE on the task record.
	fresh, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(fresh.LastError) == "" {
		t.Fatal("join re-check rejection must write the reason to the task record (LastError), not only to the HTTP response")
	}
	if !strings.Contains(fresh.LastError, "not_touched.md") {
		t.Fatalf("LastError must carry the deterministic gate reason, got %q", fresh.LastError)
	}
	// The worktree must survive: the corrected re-drive measures it again.
	if _, err := os.Stat(task.WorktreeDir); err != nil {
		t.Fatalf("rejected branch worktree must survive for retry: %v", err)
	}

	// Retry lever: align the worktree with the recorded declaration and
	// re-drive through the manual-start branch-join resume — the SAME path an
	// operator uses after this rejection. It must now resolve the join.
	if err := os.WriteFile(filepath.Join(task.WorktreeDir, "not_touched.md"), []byte("aligned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, task.WorktreeDir, "align declaration")
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual start must re-drive the rejected join after correction: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "branch_join_resumed") {
		t.Fatalf("expected branch_join_resumed, got %s", rec.Body.String())
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRun, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.ActiveStepID == "parallel" {
		t.Fatal("the re-driven join must move the parent run off the parallel stage")
	}
}

// TestTriggerReviewJoinRejectionWritesLastError: the same rejection arriving
// through a ChatOps/trigger callback (the reviewer never opens the console)
// must likewise leave the reason on the task record and keep the join parked.
// The card pins to the CHILD run (the branch child task's own attached run —
// what RunForTask resolves for the reviewed task), exactly what the
// production mint does.
func TestTriggerReviewJoinRejectionWritesLastError(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task := seedParkedJoinChild(t, s, workspaceID, "not_touched.md")

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	childRun, found, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("child run lookup: found=%v err=%v", found, err)
	}
	record := workflowNotificationRecord{
		ID: "n-join-reject", WorkspaceID: workspaceID, Project: "sample", TaskID: task.ID,
		WorkflowRunID: childRun.ID, StepID: childRun.ActiveStepID, RecipientUserID: "admin",
		WorkflowRunUpdatedAt: childRun.UpdatedAt,
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	_, err = s.submitWorkflowReviewFromTrigger(workspaceID, record, workflowTriggerCallbackBody{
		Outputs: branchJoinReviewOutputs("did things", "server.go\nnot_touched.md"),
	}, req)
	if err == nil {
		t.Fatal("a join-rejected branch approval through the trigger path must surface an error")
	}

	fresh, err := s.ts.GetTask("sample", "pm", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(fresh.LastError) == "" {
		t.Fatal("trigger-path join rejection must write the reason to the task record (LastError)")
	}
	if !strings.Contains(fresh.LastError, "not_touched.md") {
		t.Fatalf("LastError must carry the deterministic gate reason, got %q", fresh.LastError)
	}
	// The branch instance stays running — the stage is still waiting, and the
	// recorded declaration remains replayable for a corrected re-drive.
	instances, err := wfStore.BranchInstancesForStep(task.Vars[workflowRunIDVar], task.Vars[workflowStepIDVar])
	if err != nil || len(instances) != 1 || instances[0].Status != "running" {
		t.Fatalf("branch instance must stay parked after the rejected approval, got %+v (err=%v)", instances, err)
	}
}

// TestTriggerReviewJoinSuccessPathUnchanged: an honest branch approval through
// the trigger callback still advances the parent (the fix must not disturb
// the join success path). The card pins to the CHILD run, as above.
func TestTriggerReviewJoinSuccessPathUnchanged(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	task := seedParkedJoinChild(t, s, workspaceID, "server.go")

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	childRun, found, err := wfStore.RunForTask("sample", task.ID)
	if err != nil || !found {
		t.Fatalf("child run lookup: found=%v err=%v", found, err)
	}
	record := workflowNotificationRecord{
		ID: "n-join-ok", WorkspaceID: workspaceID, Project: "sample", TaskID: task.ID,
		WorkflowRunID: childRun.ID, StepID: childRun.ActiveStepID, RecipientUserID: "admin",
		WorkflowRunUpdatedAt: childRun.UpdatedAt,
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// The parent run is parked at the parallel stage, so the trigger review
	// approves the branch child's terminal step and the join drives from it.
	_, err = s.submitWorkflowReviewFromTrigger(workspaceID, record, workflowTriggerCallbackBody{
		Outputs: branchJoinReviewOutputs("did things", "server.go"),
	}, req)
	if err != nil {
		t.Fatalf("honest branch approval must advance the join, got: %v", err)
	}
	parentRun, _, err := wfStore.RunByID("sample", task.Vars[workflowRunIDVar])
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.ActiveStepID == "parallel" {
		t.Fatal("honest approval must move the parent run off the parallel stage")
	}
}
