package api

// Regression tests for fix round S2-2 (review findings P0-2/P1-1/P1-2/P2):
// the join precheck must judge the PARENT task's worktree under the PARENT
// run's frozen branch contract, a re-report after a partial join must be
// idempotent, and the fingerprint must notice chmod/symlink deltas.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// TestPrecheckUsesParentWorktreeNotChild (P0-2): the child task's completion
// must be judged against the ROOT task's worktree (that is what the join
// gate reads via CompleteBranchAndMaybeAdvance(project, rootTaskID, ...)),
// never the child's own (usually nonexistent) worktree.
func TestPrecheckUsesParentWorktreeNotChild(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	wt := newBranchTestWorktree(t)
	// The CHILD worktree is empty/clean; the PARENT worktree holds the
	// undeclared business edit. The old bug resolved the child task's
	// worktree here and passed precheck, then the join failed on the
	// parent's dirty tree after terminal state had been persisted.
	s.worktreeResolveOverride = func(project, taskID string) string {
		if taskID == "task-precheck-root" {
			return wt
		}
		return ""
	}
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-root", wt)
	if _, err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, wt, "server.go", "package main\n\nfunc Bad() {}\n"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID: "wf-branch-single-3", Name: "Single-step branch 3", Version: 1,
		Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID: "start", Type: "agent_task", Title: "Branch work",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary"}, {Name: "touched_paths"},
			},
		}},
		Edges: []entity.WorkflowEdge{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	task.Vars[workflowRootTaskIDVar] = "task-precheck-root"
	if _, _, err := wfStore.StartRun("proj", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
	})
	if err == nil || !strings.Contains(err.Error(), "server.go") {
		t.Fatalf("precheck must judge the PARENT worktree (undeclared server.go edit), got: %v", err)
	}
}

// TestPrecheckRejectsOutputMissingParentContractField (P1-1): the join maps
// outputs through the PARENT branch contract; an output set that satisfies
// the child step's fields but lacks a REQUIRED parent branch field must be
// rejected pre-persistence, not after the child run went terminal.
func TestPrecheckRejectsOutputMissingParentContractField(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	wt := newBranchTestWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-root", wt)
	if _, err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, wt, "server_test.go", "package main\n\nfunc TestX() {}\n"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// PARENT definition: the branch requires three outputs.
	parentDef := &entity.WorkflowDefinition{
		ID: "wf-parent-3", Name: "Parent 3", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{{
			ID: "parallel", Type: "parallel_stage", Title: "Parallel",
			Branches: []entity.WorkflowBranch{{
				ID: "workstream_2", Title: "WS-2",
				OutputFields: []entity.WorkflowField{
					{Name: "branch_summary"},
					{Name: "touched_paths"},
					{Name: "test_report"},
				},
			}},
		}},
		Edges: []entity.WorkflowEdge{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(parentDef); err != nil {
		t.Fatal(err)
	}
	// CHILD definition (generated stub shape): only two outputs.
	childDef := &entity.WorkflowDefinition{
		ID: "wf-branch-single-4", Name: "Single-step branch 4", Version: 1,
		Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID: "start", Type: "agent_task", Title: "Branch work",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary"}, {Name: "touched_paths"},
			},
		}},
		Edges: []entity.WorkflowEdge{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(childDef); err != nil {
		t.Fatal(err)
	}
	// Parent run for the contract lookup, with the branch instance the
	// fan-out handler would have created (contract frozen at run start).
	parentTask := &entity.Task{ID: "task-precheck-root", Title: "root", Status: entity.TaskStatusInProgress}
	parentRun, _, err := wfStore.StartRun("proj", parentTask.ID, parentDef.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wfStore.SaveBranchInstance(&entity.WorkflowBranchInstance{
		RunID: parentRun.ID, StepID: "parallel", BranchID: "workstream_2", Status: "running",
		StartedAt: now, UpdatedAt: now,
		OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}, {Name: "test_report"}},
	}); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	task.Vars[workflowRootTaskIDVar] = parentTask.ID
	task.Vars[workflowRunIDVar] = parentRun.ID
	task.Vars[workflowStepIDVar] = "parallel"
	if _, _, err := wfStore.StartRun("proj", task.ID, childDef.ID, nil); err != nil {
		t.Fatal(err)
	}
	err = s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
		// test_report missing — required by the PARENT contract.
	})
	if err == nil || !strings.Contains(err.Error(), "test_report") {
		t.Fatalf("precheck must enforce the PARENT branch contract (missing test_report), got: %v", err)
	}
}

// TestPrecheckUsesFrozenInstanceContract (P1-1): when the branch instance
// carries frozen OutputFields (captured at run start), a LATER edit to the
// parent definition must not change the contract the precheck enforces.
func TestPrecheckUsesFrozenInstanceContract(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	wt := newBranchTestWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-root", wt)
	if _, err := gitworktree.CaptureQABaseline(wt); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, wt, "server_test.go", "package main\n\nfunc TestX() {}\n"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	parentDef := &entity.WorkflowDefinition{
		ID: "wf-parent-5", Name: "Parent 5", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{{
			ID: "parallel", Type: "parallel_stage", Title: "Parallel",
			Branches: []entity.WorkflowBranch{{
				ID:           "workstream_2",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
			}},
		}},
		Edges: []entity.WorkflowEdge{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(parentDef); err != nil {
		t.Fatal(err)
	}
	childDef := &entity.WorkflowDefinition{
		ID: "wf-branch-single-5", Name: "Single-step branch 5", Version: 1,
		Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID: "start", Type: "agent_task", Title: "Branch work",
			OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
		}},
		Edges: []entity.WorkflowEdge{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(childDef); err != nil {
		t.Fatal(err)
	}
	parentTask := &entity.Task{ID: "task-precheck-root", Title: "root", Status: entity.TaskStatusInProgress}
	parentRun, _, err := wfStore.StartRun("proj", parentTask.ID, parentDef.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Freeze the contract on the instance as production does: the frozen
	// contract REQUIRES risk_matrix while the parent definition does not —
	// the frozen copy must win.
	if err := wfStore.SaveBranchInstance(&entity.WorkflowBranchInstance{
		RunID: parentRun.ID, StepID: "parallel", BranchID: "workstream_2", Status: "running",
		StartedAt: now, UpdatedAt: now,
		OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}, {Name: "risk_matrix"}},
	}); err != nil {
		t.Fatal(err)
	}
	task := branchTaskWithVars(t, "workstream_2")
	task.Vars[workflowRootTaskIDVar] = parentTask.ID
	task.Vars[workflowRunIDVar] = parentRun.ID
	task.Vars[workflowStepIDVar] = "parallel"
	if _, _, err := wfStore.StartRun("proj", task.ID, childDef.ID, nil); err != nil {
		t.Fatal(err)
	}
	// risk_matrix is missing but required by the FROZEN instance contract.
	err = s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server_test.go",
	})
	if err == nil || !strings.Contains(err.Error(), "risk_matrix") {
		t.Fatalf("precheck must enforce the FROZEN instance contract, got: %v", err)
	}
}

// TestBranchReReportAfterJoinIsIdempotent (P1-2): a branch child whose join
// ALREADY consumed its completion (branch instance terminal) but whose
// runtime re-reports (retry after a network error) must get the recorded
// result back without error and without new writes — never a stale
// rejection that wedges the agent.
func TestBranchReReportAfterJoinIsIdempotent(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-join-child", wt)
	if err := workflowstore.NewStore(s.controlDB, workspaceID).CaptureQABaselineRecord("sample", "task-join-root", wt); err != nil {
		t.Fatalf("upload parent qa baseline: %v", err)
	}
	task, runID := seedBranchChildRun(t, s, workspaceID)
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{"branch_summary": "did things", "touched_paths": "server_test.go"}
	if rec := postBranchStepComplete(t, s, workspaceID, task.ID, outputs); rec.Code != http.StatusOK {
		t.Fatalf("first completion must succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	// The re-report goes through the step/complete endpoint: the task is
	// archived (done_success) and the child run terminal, so the normal
	// step transition would reject; the resumed branch-join path must
	// reproduce the recorded result.
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, outputs)
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent re-report must succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Branch entity.WorkflowBranchInstance `json:"branch"`
		AllDone bool                          `json:"allDone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode re-report payload: %v", err)
	}
	if payload.Branch.BranchID != "ws_b" || payload.Branch.Status != "completed" {
		t.Fatalf("re-report must return the recorded branch result, got %+v", payload.Branch)
	}
	run, _, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = runID
	if run.Status != "completed" {
		t.Fatalf("re-report must not change run state, got %q", run.Status)
	}
}

// TestFingerprintDetectsChmodAndSymlink (P2): the baseline delta must catch
// a content-identical permission flip and a symlink retarget. Built directly
// on gitworktree (the api package has no workflow-store fixtures).
func TestFingerprintDetectsChmodAndSymlink(t *testing.T) {
	wt := newBranchTestWorktree(t)
	script := filepath.Join(wt, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	baseline, err := gitworktree.CaptureQABaseline(wt)
	if err != nil {
		t.Fatal(err)
	}
	// chmod +x with identical content is a real delta.
	if err := os.Chmod(script, 0755); err != nil {
		t.Fatal(err)
	}
	delta, err := gitworktree.QABaselineWorktreeDelta(wt, baseline)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range delta {
		if p == "run.sh" {
			found = true
		}
	}
	if !found {
		t.Fatalf("chmod-only flip must surface as a delta, got %v", delta)
	}
	// Symlink retarget is a real delta too.
	link := filepath.Join(wt, "latest")
	if err := os.Symlink("run.sh", link); err != nil {
		t.Fatal(err)
	}
	baseline2, err := gitworktree.CaptureQABaseline(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("server.go", link); err != nil {
		t.Fatal(err)
	}
	delta2, err := gitworktree.QABaselineWorktreeDelta(wt, baseline2)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, p := range delta2 {
		if p == "latest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("symlink retarget must surface as a delta, got %v", delta2)
	}
}
