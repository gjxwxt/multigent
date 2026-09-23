package api

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// seedDualBranchChildRun is a TWO-branch variant of seedBranchChildRun: the
// parent's parallel step declares ws_a and ws_b, both branch tasks are
// created and stamped with the parent run ID, and BOTH branch instances are
// seeded (running). Returns the ws_b child task (the one whose re-report is
// exercised) plus both child run IDs.
func seedDualBranchChildRun(t *testing.T, s *Server, workspaceID string) (*entity.Task, string, string) {
	t.Helper()
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	parentDef := &entity.WorkflowDefinition{
		ID: "wf-join-parent-dual", Name: "Join parent dual", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{{
			ID: "parallel", Type: "parallel_stage", Title: "Parallel",
			JoinPolicy: "all",
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
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(parentDef); err != nil {
		t.Fatal(err)
	}
	parentRun, _, err := wfStore.StartRun("sample", "task-join-root-dual", parentDef.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	childDef := &entity.WorkflowDefinition{
		ID: "wf-join-child-dual", Name: "Branch child dual", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID: "start", Type: "agent_task", Title: "Branch work", ActorRole: "pm-agent",
			OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(childDef); err != nil {
		t.Fatal(err)
	}

	tasks := map[string]*entity.Task{}
	runIDs := map[string]string{}
	for _, bid := range []string{"ws_a", "ws_b"} {
		branchTask := &entity.Task{
			ID: "task-dual-" + bid, Title: "Branch " + bid, Status: entity.TaskStatusInProgress,
			Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
			Vars: map[string]string{
				workflowRunIDVar:      parentRun.ID,
				workflowStepIDVar:     "parallel",
				workflowBranchIDVar:   bid,
				workflowRootTaskIDVar: "task-join-root-dual",
			},
		}
		run, _, err := wfStore.StartRun("sample", branchTask.ID, childDef.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ts.AddTask("sample", "pm", branchTask); err != nil {
			t.Fatal(err)
		}
		if err := wfStore.SaveBranchInstance(&entity.WorkflowBranchInstance{
			RunID: parentRun.ID, StepID: "parallel", BranchID: bid, Status: "running",
			StartedAt: now, UpdatedAt: now,
			ChildTaskID: branchTask.ID, ChildRunID: run.ID,
			OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
		}); err != nil {
			t.Fatal(err)
		}
		tasks[bid] = branchTask
		runIDs[bid] = run.ID
	}
	return tasks["ws_b"], runIDs["ws_a"], runIDs["ws_b"]
}

// TestDualBranchCrashWindowResumesParent (S2-2.2 review P2 — coverage gap):
// the multi-branch crash window. BOTH branch instances terminal (ws_a
// completed, ws_b completed), the parent run still active@parallel — the
// process died between the second SaveBranchInstance and the join. A re-report
// of the ws_b branch must re-drive the join from the recorded two-branch
// aggregate and advance the parent past the parallel step.
func TestDualBranchCrashWindowResumesParent(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-dual-ws_b", wt)
	if err := workflowstore.NewStore(s.controlDB, workspaceID).CaptureQABaselineRecord("sample", "task-join-root-dual", wt); err != nil {
		t.Fatalf("upload parent qa baseline: %v", err)
	}
	task, childRunA, childRunB := seedDualBranchChildRun(t, s, workspaceID)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRunID := task.Vars[workflowRunIDVar]

	// Crash window: finish BOTH branch instances directly (bypassing the
	// join), simulating a death right after the second SaveBranchInstance.
	for bid, childRun := range map[string]string{"ws_a": childRunA, "ws_b": childRunB} {
		instances, err := wfStore.BranchInstancesForStep(parentRunID, "parallel")
		if err != nil {
			t.Fatal(err)
		}
		for i := range instances {
			if instances[i].BranchID == bid {
				instances[i].Status = "completed"
				instances[i].ChildRunID = childRun
				instances[i].Summary = "delivered " + bid
				instances[i].OutputValues = map[string]string{"branch_summary": "delivered " + bid, "touched_paths": "server_test.go"}
				if err := wfStore.SaveBranchInstance(&instances[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{"branch_summary": "delivered ws_b", "touched_paths": "server_test.go"}
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, outputs)
	if rec.Code != http.StatusOK {
		t.Fatalf("crash-window re-report must succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	parentRun, _, err := wfStore.RunByID("sample", parentRunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("two-branch crash window must resume the join, got %q@%q", parentRun.Status, parentRun.ActiveStepID)
	}

	// Idempotent second report stays 200.
	rec = postBranchStepComplete(t, s, workspaceID, task.ID, outputs)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-join re-report must stay idempotent, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDualBranchConcurrentReportsNoDoubleAdvance (S2-2.2 review P0/P2 —
// TOCTOU probe): two branches reach the crash window, then BOTH child tasks
// re-report CONCURRENTLY. Exactly one of them may drive the join; the other
// must degrade to a zero-write replay (200). The parent must advance exactly
// once — never a double transition, never a 500 on the losing reporter.
func TestDualBranchConcurrentReportsNoDoubleAdvance(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wt := newBranchJoinWorktree(t)
	s.worktreeResolveOverride = func(project, taskID string) string { return wt }
	s.qaBaselineLookupOverride = mustUploadQABaseline(t, s, workspaceID, "sample", "task-dual-ws_b", wt)
	if err := workflowstore.NewStore(s.controlDB, workspaceID).CaptureQABaselineRecord("sample", "task-join-root-dual", wt); err != nil {
		t.Fatalf("upload parent qa baseline: %v", err)
	}
	task, childRunA, childRunB := seedDualBranchChildRun(t, s, workspaceID)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	parentRunID := task.Vars[workflowRunIDVar]

	// Both branch instances terminal behind the join (crash window).
	for bid, childRun := range map[string]string{"ws_a": childRunA, "ws_b": childRunB} {
		instances, err := wfStore.BranchInstancesForStep(parentRunID, "parallel")
		if err != nil {
			t.Fatal(err)
		}
		for i := range instances {
			if instances[i].BranchID == bid {
				instances[i].Status = "completed"
				instances[i].ChildRunID = childRun
				instances[i].Summary = "delivered " + bid
				instances[i].OutputValues = map[string]string{"branch_summary": "delivered " + bid, "touched_paths": "server_test.go"}
				if err := wfStore.SaveBranchInstance(&instances[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// ws_a re-reports through the STORE path (simulating a second node),
	// ws_b re-reports through the HTTP handler; both concurrent.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := wfStore.CompleteBranchAndMaybeAdvance("sample", "task-join-root-dual", parentRunID, "parallel", "ws_a",
			"delivered ws_a", map[string]string{"branch_summary": "delivered ws_a", "touched_paths": "server_test.go"}, "completed")
		if err != nil {
			t.Errorf("ws_a concurrent re-report failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		rec := postBranchStepComplete(t, s, workspaceID, task.ID, map[string]string{"branch_summary": "delivered ws_b", "touched_paths": "server_test.go"})
		codes[0] = rec.Code
		if rec.Code != http.StatusOK {
			t.Errorf("ws_b concurrent re-report got %d: %s", rec.Code, rec.Body.String())
		}
	}()
	wg.Wait()

	// Persisted truth: the parent advanced exactly once, to completed.
	parentRun, _, err := wfStore.RunByID("sample", parentRunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("parent must be completed after concurrent re-reports, got %q@%q", parentRun.Status, parentRun.ActiveStepID)
	}

	// The losing reporter degraded to the replay path: a SECOND report of
	// ws_b afterwards must also stay 200-idempotent.
	rec := postBranchStepComplete(t, s, workspaceID, task.ID, map[string]string{"branch_summary": "delivered ws_b", "touched_paths": "server_test.go"})
	if rec.Code != http.StatusOK {
		t.Fatalf("trailing re-report must stay idempotent, got %d: %s", rec.Code, rec.Body.String())
	}
}
