package api

// Regression tests for fix round S2-2 (review findings P0-2/P1-1/P1-2/P2)
// and the S2-2.3 review round (item 2): the join precheck judges the
// BRANCH task's own worktree + capture-time baseline under the PARENT run's
// frozen branch contract, a re-report after a partial join must be
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

// TestPrecheckUsesBranchWorktreeNotParentWorktree (S2-2.3, review round
// item 2): the branch is developed in its OWN worktree against its OWN
// capture-time baseline; the join runs BEFORE any merge, so the parent
// worktree cannot hold the branch's delta yet. The precheck AND the
// authoritative join gate must both measure the CHILD task's surface:
//   - an undeclared change in the branch worktree is rejected;
//   - noise in the PARENT worktree (a different directory) is irrelevant;
//   - the gate keys on the branch task, not the root task.
func TestPrecheckUsesBranchWorktreeNotParentWorktree(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	branchWt := newBranchTestWorktree(t)
	parentWt := newBranchTestWorktree(t)
	// Baselines are per-task, mirroring production: capture at the
	// protected materialization point (BEFORE any edit), keyed on each
	// worktree's OWN task.
	if err := workflowstore.NewStore(s.controlDB, "ws").CaptureQABaselineRecord("proj", "task-precheck-workstream_2", branchWt); err != nil {
		t.Fatalf("capture branch baseline: %v", err)
	}
	if err := workflowstore.NewStore(s.controlDB, "ws").CaptureQABaselineRecord("proj", "task-precheck-root", parentWt); err != nil {
		t.Fatalf("capture parent baseline: %v", err)
	}
	// Two DIFFERENT directories: the branch worktree holds the undeclared
	// business edit; the parent worktree holds an unrelated edit. If the
	// gate measured the parent surface, the verdict would be about the
	// wrong tree.
	if err := writeTestFile(t, branchWt, "server.go", "package main\n\nfunc BranchChange() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, parentWt, "client.go", "package main\n\nfunc ParentNoise() {}\n"); err != nil {
		t.Fatal(err)
	}
	s.worktreeResolveOverride = func(project, taskID string) string {
		if taskID == "task-precheck-workstream_2" {
			return branchWt
		}
		if taskID == "task-precheck-root" {
			return parentWt
		}
		return ""
	}
	// The lookup override over the SAME control DB (no re-capture: the
	// baseline was already persisted above, BEFORE the edit — re-running
	// capture here would fingerprint the dirty tree and launder the edit).
	s.qaBaselineLookupOverride = workflowstore.NewStore(s.controlDB, "ws").QABaselineLookupAdapter()

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
	// Undeclared branch-side change: rejected, and the error is about the
	// BRANCH worktree's server.go (not the parent's client.go).
	err := s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "none",
	})
	if err == nil {
		t.Fatalf("precheck must judge the BRANCH worktree (undeclared server.go edit), got nil")
	}
	if strings.Contains(err.Error(), "client.go") {
		t.Fatalf("precheck must not measure the parent worktree's noise, got: %v", err)
	}

	// Honest declaration of the branch-side business change: PASSES — the
	// parent worktree's unrelated noise must not leak into the verdict.
	err = s.precheckBranchJoinGate("ws", "proj", task, map[string]string{
		"branch_summary": "did things",
		"touched_paths":  "server.go",
	})
	if err != nil {
		t.Fatalf("honest branch delivery must pass regardless of parent worktree noise, got: %v", err)
	}
}

// TestPrecheckTwoBranchesIsolateWorktreesAndBaselines (S2-2.3 review
// closing, P1): THREE directories — the parent worktree plus one worktree
// PER BRANCH — each with its own capture-time baseline. Branch A's gate
// must judge only A's worktree/baseline: A's undeclared edit is rejected
// by name, B's edit and the parent's noise never leak into A's verdict,
// and an honest A declaration passes while B remains undelivered.
func TestPrecheckTwoBranchesIsolateWorktreesAndBaselines(t *testing.T) {
	s, wfStore := newPrecheckServer(t)
	parentWt := newBranchTestWorktree(t)
	wtA := newBranchTestWorktree(t)
	wtB := newBranchTestWorktree(t)
	// Per-task baselines captured at materialization, BEFORE any edit.
	if err := workflowstore.NewStore(s.controlDB, "ws").CaptureQABaselineRecord("proj", "task-precheck-workstream_1", wtA); err != nil {
		t.Fatalf("capture ws_a baseline: %v", err)
	}
	if err := workflowstore.NewStore(s.controlDB, "ws").CaptureQABaselineRecord("proj", "task-precheck-workstream_2", wtB); err != nil {
		t.Fatalf("capture ws_b baseline: %v", err)
	}
	if err := workflowstore.NewStore(s.controlDB, "ws").CaptureQABaselineRecord("proj", "task-precheck-root", parentWt); err != nil {
		t.Fatalf("capture parent baseline: %v", err)
	}
	// Each surface gets its OWN edit: A edits module_a.go, B edits
	// module_b.go, the parent tree carries unrelated client.go noise.
	if err := writeTestFile(t, wtA, "module_a.go", "package main\n\nfunc WorkA() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, wtB, "module_b.go", "package main\n\nfunc WorkB() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(t, parentWt, "client.go", "package main\n\nfunc ParentNoise() {}\n"); err != nil {
		t.Fatal(err)
	}
	s.worktreeResolveOverride = func(project, taskID string) string {
		switch taskID {
		case "task-precheck-workstream_1":
			return wtA
		case "task-precheck-workstream_2":
			return wtB
		case "task-precheck-root":
			return parentWt
		}
		return ""
	}
	// The lookup adapter reads the SAME control DB the baselines were
	// persisted into (no re-capture — re-running capture after the edits
	// would fingerprint the dirty trees and launder the edits).
	s.qaBaselineLookupOverride = workflowstore.NewStore(s.controlDB, "ws").QABaselineLookupAdapter()

	now := time.Now().UTC()
	parentDef := &entity.WorkflowDefinition{
		ID: "wf-parent-iso", Name: "Parent ISO", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{{
			ID: "parallel", Type: "parallel_stage", Title: "Parallel",
			Branches: []entity.WorkflowBranch{
				{ID: "workstream_1", Title: "WS-A", OutputFields: []entity.WorkflowField{
					{Name: "branch_summary"}, {Name: "touched_paths"},
				}},
				{ID: "workstream_2", Title: "WS-B", OutputFields: []entity.WorkflowField{
					{Name: "branch_summary"}, {Name: "touched_paths"},
				}},
			},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(parentDef); err != nil {
		t.Fatal(err)
	}
	childDef := &entity.WorkflowDefinition{
		ID: "wf-branch-single-iso", Name: "Single-step branch ISO", Version: 1,
		Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID: "start", Type: "agent_task", Title: "Branch work",
			OutputFields: []entity.WorkflowField{
				{Name: "branch_summary"}, {Name: "touched_paths"},
			},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(childDef); err != nil {
		t.Fatal(err)
	}
	parentTask := &entity.Task{ID: "task-precheck-root", Title: "root", Status: entity.TaskStatusInProgress}
	if _, _, err := wfStore.StartRun("proj", parentTask.ID, parentDef.ID, nil); err != nil {
		t.Fatal(err)
	}

	taskA := branchTaskWithVars(t, "workstream_1")
	taskA.Vars[workflowRootTaskIDVar] = "task-precheck-root"
	if _, _, err := wfStore.StartRun("proj", taskA.ID, childDef.ID, nil); err != nil {
		t.Fatal(err)
	}
	taskB := branchTaskWithVars(t, "workstream_2")
	taskB.Vars[workflowRootTaskIDVar] = "task-precheck-root"
	if _, _, err := wfStore.StartRun("proj", taskB.ID, childDef.ID, nil); err != nil {
		t.Fatal(err)
	}

	// Branch A lies ("none") with module_a.go dirty: rejected BY NAME, and
	// the verdict never mentions B's module_b.go or the parent's client.go.
	err := s.precheckBranchJoinGate("ws", "proj", taskA, map[string]string{
		"branch_summary": "ws-a did things",
		"touched_paths":  "none",
	})
	if err == nil || !strings.Contains(err.Error(), "module_a.go") {
		t.Fatalf("branch A precheck must reject its own undeclared module_a.go, got: %v", err)
	}
	if strings.Contains(err.Error(), "module_b.go") || strings.Contains(err.Error(), "client.go") {
		t.Fatalf("branch A verdict must not leak branch B's or the parent's edits, got: %v", err)
	}

	// Branch B lies the same way: symmetric isolation.
	err = s.precheckBranchJoinGate("ws", "proj", taskB, map[string]string{
		"branch_summary": "ws-b did things",
		"touched_paths":  "none",
	})
	if err == nil || !strings.Contains(err.Error(), "module_b.go") {
		t.Fatalf("branch B precheck must reject its own undeclared module_b.go, got: %v", err)
	}
	if strings.Contains(err.Error(), "module_a.go") || strings.Contains(err.Error(), "client.go") {
		t.Fatalf("branch B verdict must not leak branch A's or the parent's edits, got: %v", err)
	}

	// Honest deliveries: each branch declares its own file and passes
	// independently — A's pass must not consult B's worktree, and B's pass
	// must not consult A's.
	if err := s.precheckBranchJoinGate("ws", "proj", taskA, map[string]string{
		"branch_summary": "ws-a did things",
		"touched_paths":  "module_a.go",
	}); err != nil {
		t.Fatalf("branch A honest delivery must pass, got: %v", err)
	}
	if err := s.precheckBranchJoinGate("ws", "proj", taskB, map[string]string{
		"branch_summary": "ws-b did things",
		"touched_paths":  "module_b.go",
	}); err != nil {
		t.Fatalf("branch B honest delivery must pass, got: %v", err)
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
	// S2-2.3: the gate measures the BRANCH task; give both the root and the
	// branch task a baseline (same worktree in this fixture — the delta is
	// empty until the edit below).
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-root", wt)
	mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-workstream_2", wt)
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
	// S2-2.3: the gate measures the BRANCH task; give both the root and the
	// branch task a baseline (same worktree in this fixture — the delta is
	// empty until the edit below).
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-root", wt)
	mustUploadQABaseline(t, s, "ws", "proj", "task-precheck-workstream_2", wt)
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
	// S2-2.3: the gate measures the BRANCH task (task-join-child) — its
	// worktree and its own capture-time baseline. The parent's baseline is
	// no longer consulted by the join gate.
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-join-child", wt)
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
		Branch  entity.WorkflowBranchInstance `json:"branch"`
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
