package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// fixtureDeliveryPlan builds a two-wave plan: wp-a/wp-b run in wave 0 and
// wp-c (depending on wp-a) only becomes ready after wp-a completes.
func fixtureDeliveryPlan(runID string) workflowstore.DeliveryPlan {
	return workflowstore.DeliveryPlan{
		SchemaVersion: workflowstore.DeliveryPlanSchemaVersion,
		PlanID:        "plan-loop-fixture",
		Version:       1,
		RunID:         runID,
		RequirementItems: []workflowstore.PlanRequirementItem{
			{ID: "uc-a", Text: "deliver module A"},
			{ID: "uc-b", Text: "deliver module B"},
			{ID: "uc-c", Text: "deliver module C after A"},
		},
		WorkPackages: []workflowstore.PlanWorkPackage{
			{ID: "wp-a", Title: "Work package A", AcceptanceCriteria: []string{"uc-a"}, AgentBinding: "pm", ExpectedDelivery: []string{"module_a.go"}},
			{ID: "wp-b", Title: "Work package B", AcceptanceCriteria: []string{"uc-b"}, AgentBinding: "pm", ExpectedDelivery: []string{"module_b.go"}},
			{ID: "wp-c", Title: "Work package C", DependsOn: []string{"wp-a"}, AcceptanceCriteria: []string{"uc-c"}, AgentBinding: "pm", ExpectedDelivery: []string{"module_c.go"}},
		},
	}
}

// seedPlanStageParentRun seeds a parent run whose next step is a
// plan-driven parallel stage (no static branches: the branches MUST come from
// the frozen plan) and freezes the plan against that run.
func seedPlanStageParentRun(t *testing.T, s *Server, workspaceID, baseCommit string, plan workflowstore.DeliveryPlan, freeze bool) *entity.Task {
	t.Helper()
	now := time.Now().UTC()
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "wf-plan-parent", Name: "Plan parent", Version: 1, Scope: "workspace", StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID: "start", Type: "agent_task", Title: "Contract", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "branch_summary"}, {Name: "touched_paths"}},
			},
			{
				ID: "parallel", Type: "parallel_stage", Title: "Plan wave stage", JoinPolicy: "all",
				Config: map[string]string{planMaterializationConfigKey: "frozen"},
			},
		},
		Edges:     []entity.WorkflowEdge{{From: "start", To: "parallel"}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	parentTask := &entity.Task{
		ID: "task-plan-root", Title: "Plan root", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "sample/pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main", Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", parentTask); err != nil {
		t.Fatal(err)
	}
	run, _, err := wfStore.StartRun("sample", parentTask.ID, def.ID, map[string]entity.WorkflowActorBinding{
		"pm-agent": {Type: "agent", ID: "pm"},
		"parallel": {Type: "agent", ID: "pm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if freeze {
		plan.RunID = run.ID
		if _, _, err := wfStore.FreezeDeliveryPlan("sample", plan, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	return parentTask
}

func planStageFixtures(t *testing.T) (*Server, string, string) {
	t.Helper()
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	return s, workspaceID, baseCommit
}

func branchInstanceFor(t *testing.T, wfStore *workflowstore.Store, runID, stepID, branchID string) (entity.WorkflowBranchInstance, bool) {
	t.Helper()
	instances, err := wfStore.BranchInstancesForStep(runID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if inst.BranchID == branchID {
			return inst, true
		}
	}
	return entity.WorkflowBranchInstance{}, false
}

func planRunIDFor(t *testing.T, s *Server, workspaceID, taskID string) string {
	t.Helper()
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", taskID)
	if err != nil || !found {
		t.Fatalf("run lookup for %s: found=%v err=%v", taskID, found, err)
	}
	return run.ID
}

// TestPlanStageMaterializesReadyWaveAndUnlocksNextWaveOnCompletion drives the
// real HTTP fan-out on a plan-driven stage: only wave 0 materializes, wave 1
// appears from the EXISTING branch-completion path once its dependency
// completes, and the run only advances after the whole plan is materialized
// and done.
func TestPlanStageMaterializesReadyWaveAndUnlocksNextWaveOnCompletion(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("plan-driven fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	instances, err := wfStore.BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("wave 0 must materialize exactly its two work packages, got %d", len(instances))
	}
	childIDs := map[string]string{}
	for _, inst := range instances {
		if inst.BranchID != "wp-a" && inst.BranchID != "wp-b" {
			t.Fatalf("unexpected branch materialized: %q (wave 1 must not start yet)", inst.BranchID)
		}
		childIDs[inst.BranchID] = inst.ChildTaskID
		task, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Vars[workflowPlanIDVar] != plan.PlanID || task.Vars[workflowPlanWPIDVar] != inst.BranchID {
			t.Fatalf("branch %s task must carry the plan identity, got vars %v", inst.BranchID, task.Vars)
		}
		if task.Vars[workflowPlanVersionVar] != "1" || task.Vars[workflowPlanDigestVar] == "" {
			t.Fatalf("branch %s task must carry version+digest, got vars %v", inst.BranchID, task.Vars)
		}
		if task.IdempotencyKey != "plan/"+runID+"/1/"+inst.BranchID {
			t.Fatalf("branch %s idempotency key = %q", inst.BranchID, task.IdempotencyKey)
		}
		if task.BaseCommit != baseCommit || task.WorktreeDir == "" {
			t.Fatalf("branch %s must be materialized at the frozen baseline: base=%q wt=%q", inst.BranchID, task.BaseCommit, task.WorktreeDir)
		}
		if _, ok, err := wfStore.LoadQABaselinePayload("sample", inst.ChildTaskID); err != nil || !ok {
			t.Fatalf("branch %s control-plane QA baseline missing: ok=%v err=%v", inst.BranchID, ok, err)
		}
	}

	record, version, ok, err := wfStore.LoadFrozenPlanForRun("sample", runID)
	if err != nil || !ok {
		t.Fatalf("frozen plan must load: ok=%v err=%v", ok, err)
	}
	if len(record.Materializations) != 2 {
		t.Fatalf("materializations must be pinned in the plan record, got %+v", record.Materializations)
	}
	for _, m := range record.Materializations {
		if m.DefinitionDigest == "" || m.TaskID == "" || m.WaveIndex != 0 {
			t.Fatalf("materialization entry incomplete: %+v", m)
		}
	}
	if version.Digest == "" {
		t.Fatal("frozen version must carry a digest")
	}

	// Complete wp-a: its completion must unlock wp-c through the existing
	// branch-completion path (no manual re-drive).
	taskA, err := s.ts.GetTask("sample", "pm", childIDs["wp-a"])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskA.WorktreeDir, "module_a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := postBranchStepComplete(t, s, workspaceID, childIDs["wp-a"], map[string]string{
		"branch_summary": "module A done",
		"touched_paths":  "module_a.go",
	}); rec.Code != http.StatusOK {
		t.Fatalf("wp-a completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	instC, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-c")
	if !found {
		t.Fatal("wp-c must be materialized once its dependency completed")
	}
	if instC.ChildTaskID == "" || instC.ChildTaskID == childIDs["wp-a"] {
		t.Fatalf("wp-c must get its own task identity, got %q", instC.ChildTaskID)
	}
	record, _, _, err = wfStore.LoadFrozenPlanForRun("sample", runID)
	if err != nil {
		t.Fatal(err)
	}
	waveC := -1
	for _, m := range record.Materializations {
		if m.WPID == "wp-c" {
			waveC = m.WaveIndex
		}
	}
	if waveC != 1 {
		t.Fatalf("wp-c must be recorded in wave 1, got %d (%+v)", waveC, record.Materializations)
	}
	parentRun, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "active" || parentRun.ActiveStepID != "parallel" {
		t.Fatalf("stage must still be running while wave 1 executes, got %s@%s", parentRun.Status, parentRun.ActiveStepID)
	}

	// Complete the remaining branches: the stage then joins and the run ends.
	remaining := []string{"wp-b", "wp-c"}
	for _, wp := range remaining {
		inst, found := branchInstanceFor(t, wfStore, runID, "parallel", wp)
		if !found {
			t.Fatalf("branch %s must exist before completion", wp)
		}
		task, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(task.WorktreeDir, "module_"+wp+".go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rec := postBranchStepComplete(t, s, workspaceID, inst.ChildTaskID, map[string]string{
			"branch_summary": wp + " done",
			"touched_paths":  "module_" + wp + ".go",
		}); rec.Code != http.StatusOK {
			t.Fatalf("branch %s completion must be 200, got %d: %s", wp, rec.Code, rec.Body.String())
		}
	}
	parentRun, _, err = wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	if parentRun.Status != "completed" {
		t.Fatalf("run must complete after the whole plan is delivered, got %s@%s", parentRun.Status, parentRun.ActiveStepID)
	}
}

// TestPlanStageRedriveKeepsSingleIdentity is the reverse case for re-drive
// duplication: driving the materialization again must reuse the same branch
// tasks and instances (1:1:1:1), never mint a second identity.
func TestPlanStageRedriveKeepsSingleIdentity(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	root := seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	if rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}); rec.Code != http.StatusOK {
		t.Fatalf("first drive must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	before, err := wfStore.BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	idsBefore := map[string]string{}
	for _, inst := range before {
		idsBefore[inst.BranchID] = inst.ChildTaskID
	}

	// Re-drive exactly the way the platform does (same parent, same stage).
	run, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	stageInst, err := s.parallelStageInstance(wfStore, runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	step, found, err := s.parallelStageStepForRun(wfStore, run, "parallel")
	if err != nil || !found {
		t.Fatalf("stage step lookup: found=%v err=%v", found, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.activateParallelWorkflowStep(workspaceID, "sample", "pm", root, workflowstore.TransitionResult{
			Run:      run,
			Current:  stageInst,
			Next:     &step,
			NextInst: &entity.WorkflowStepInstance{RunID: runID, StepID: "parallel", Status: "pending"},
		}, httptest.NewRequest(http.MethodPost, "/", nil)); err != nil {
			t.Fatalf("re-drive %d must be idempotent, got %v", i, err)
		}
	}
	after, err := wfStore.BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("re-drive must not duplicate branch instances: %d -> %d", len(before), len(after))
	}
	for _, inst := range after {
		if idsBefore[inst.BranchID] != inst.ChildTaskID {
			t.Fatalf("branch %s re-drive changed the task identity: %q -> %q", inst.BranchID, idsBefore[inst.BranchID], inst.ChildTaskID)
		}
	}
	record, _, _, err := wfStore.LoadFrozenPlanForRun("sample", runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Materializations) != 2 {
		t.Fatalf("re-drive must not append materializations, got %+v", record.Materializations)
	}
}

// TestPlanStageRefusesWithoutFrozenPlan is the reverse case for "no frozen
// record": a plan-driven stage must refuse (and materialize nothing) rather
// than fall back to static branches.
func TestPlanStageRefusesWithoutFrozenPlan(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, false) // no freeze
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")

	rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("plan-driven stage without a frozen plan must not succeed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no frozen delivery plan") {
		t.Fatalf("refusal must name the missing frozen plan, got %s", rec.Body.String())
	}
	instances, err := workflowstore.NewStore(s.controlDB, workspaceID).BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Fatalf("no branch may be materialized without a frozen plan, got %d", len(instances))
	}
}

// TestPlanStageRefusesTamperedPlan is the reverse case for "one byte changed
// in the stored plan": materialization must fail closed.
func TestPlanStageRefusesTamperedPlan(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	record, _, _, err := wfStore.LoadFrozenPlanForRun("sample", runID)
	if err != nil {
		t.Fatal(err)
	}
	record.Versions[0].Plan.WorkPackages[0].Title = "Work package A (tampered)"
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.controlDB.UpsertRecord("workflow_plans", workspaceID, []string{"sample", runID}, string(payload)); err != nil {
		t.Fatal(err)
	}

	rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("tampered plan must not materialize: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "digest mismatch") {
		t.Fatalf("refusal must name the digest mismatch, got %s", rec.Body.String())
	}
	if instances, err := wfStore.BranchInstancesForStep(runID, "parallel"); err != nil || len(instances) != 0 {
		t.Fatalf("no branch may be materialized from a tampered plan (err=%v)", err)
	}
}

// TestPlanStageBlocksDownstreamOnUpstreamFailure is the reverse case for
// "upstream failed": dependents must be blocked, visibly, and never
// materialized.
func TestPlanStageBlocksDownstreamOnUpstreamFailure(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	root := seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	if rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}); rec.Code != http.StatusOK {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The wp-a branch fails (platform state: instance failed + root blocked is
	// the production shape; here we set the instance and drive the wave hook).
	instA, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-a")
	if !found {
		t.Fatal("wp-a must be materialized in wave 0")
	}
	instA.Status = "failed"
	instA.Summary = "module A could not be delivered"
	if err := wfStore.SaveBranchInstance(&instA); err != nil {
		t.Fatal(err)
	}

	run, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	_, waveErr := s.materializeNextPlanWave(workspaceID, "sample", "pm", root, workflowstore.BranchTransitionResult{
		Branch:  instA,
		AllDone: true,
		Transition: workflowstore.TransitionResult{
			Run:     run,
			Current: entity.WorkflowStepInstance{RunID: runID, StepID: "parallel"},
		},
	}, httptest.NewRequest(http.MethodPost, "/", nil))
	if waveErr == nil {
		t.Fatal("a failed upstream must block the next wave, not materialize it")
	}
	if !strings.Contains(waveErr.Error(), "wp-c") {
		t.Fatalf("the blocked set must be visible in the error, got %v", waveErr)
	}
	if _, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-c"); found {
		t.Fatal("a blocked work package must never be materialized")
	}
	// Visible on the root task as well (operators read the task, not logs).
	persisted, err := s.ts.GetTask("sample", "pm", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persisted.LastError, "wp-c") {
		t.Fatalf("root task must carry the blocked set, got %q", persisted.LastError)
	}
}

// TestConcurrentPlanStageActivationMaterializesEachWorkPackageOnce drives the
// formal materialization entry from several goroutines at once (the shape two
// racing branch completions produce): the plan-record claim must admit exactly
// one materialization per work package, so no work package ends up with two
// branch instances / tasks, and every derived branch stays resolvable for its
// report path without any shared record being written.
func TestConcurrentPlanStageActivationMaterializesEachWorkPackageOnce(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	root := seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	stageInst, err := s.parallelStageInstance(wfStore, runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	step, found, err := s.parallelStageStepForRun(wfStore, run, "parallel")
	if err != nil || !found {
		t.Fatalf("stage step lookup: found=%v err=%v", found, err)
	}

	const workers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker owns its task copy: activateParallelWorkflowStep
			// mutates the parent task struct it is handed.
			local := *root
			local.Vars = map[string]string{}
			for k, v := range root.Vars {
				local.Vars[k] = v
			}
			<-start
			errs <- s.activateParallelWorkflowStep(workspaceID, "sample", "pm", &local, workflowstore.TransitionResult{
				Run:      run,
				Current:  stageInst,
				Next:     &step,
				NextInst: &entity.WorkflowStepInstance{RunID: runID, StepID: "parallel", Status: "pending"},
			}, httptest.NewRequest(http.MethodPost, "/", nil))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent activation must be idempotent, got %v", err)
		}
	}

	instances, err := wfStore.BranchInstancesForStep(runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("each ready work package must be materialized exactly once, got %d instances: %+v", len(instances), instances)
	}
	seenBranch := map[string]string{}
	seenTask := map[string]bool{}
	seenWorktree := map[string]bool{}
	for _, inst := range instances {
		if prev, dup := seenBranch[inst.BranchID]; dup {
			t.Fatalf("work package %s materialized twice (instances %s and %s)", inst.BranchID, prev, inst.ID)
		}
		seenBranch[inst.BranchID] = inst.ID
		if inst.ChildTaskID == "" || seenTask[inst.ChildTaskID] {
			t.Fatalf("work package %s must own a distinct child task, got %q", inst.BranchID, inst.ChildTaskID)
		}
		seenTask[inst.ChildTaskID] = true
		task, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if seenWorktree[task.WorktreeDir] {
			t.Fatalf("work packages must not share a worktree, %s shared", task.WorktreeDir)
		}
		seenWorktree[task.WorktreeDir] = true
		// The report path resolves the derived branch from the frozen plan —
		// no snapshot write is involved, so nothing can be lost between
		// concurrent materializations.
		def, ok, err := wfStore.ResolveStageBranch("sample", runID, step, inst.BranchID)
		if err != nil || !ok {
			t.Fatalf("branch %s must resolve for its report path: ok=%v err=%v", inst.BranchID, ok, err)
		}
		if len(def.OutputFields) == 0 {
			t.Fatalf("resolved branch %s must carry the stage field contract", inst.BranchID)
		}
	}
	count, err := wfStore.PlanMaterializationCount("sample", runID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("plan record must pin exactly 2 materializations, got %d", count)
	}
}

// TestPlanStageStaleAllDoneDoesNotAdvancePastRunningWave is the state-machine
// regression for the interleaving GPT flagged: a branch completion whose
// AllDone was computed BEFORE a concurrent completion materialized the next
// wave must not advance the parent run past that running wave.
func TestPlanStageStaleAllDoneDoesNotAdvancePastRunningWave(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	if rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}); rec.Code != http.StatusOK {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	instA, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-a")
	if !found {
		t.Fatal("wp-a must be materialized in wave 0")
	}
	taskA, err := s.ts.GetTask("sample", "pm", instA.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskA.WorktreeDir, "module_a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := postBranchStepComplete(t, s, workspaceID, instA.ChildTaskID, map[string]string{
		"branch_summary": "module A done",
		"touched_paths":  "module_a.go",
	}); rec.Code != http.StatusOK {
		t.Fatalf("wp-a completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-c"); !found {
		t.Fatal("wp-c must be materialized after its dependency completed")
	}

	// Simulate the racing sibling: its completion transaction recorded wp-b as
	// completed while its BranchTransitionResult (computed a moment earlier,
	// before wp-c existed) says AllDone=true / join done.
	instB, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-b")
	if !found {
		t.Fatal("wp-b must be materialized in wave 0")
	}
	instB.Status = "completed"
	instB.Summary = "module B done"
	if err := wfStore.SaveBranchInstance(&instB); err != nil {
		t.Fatal(err)
	}
	run, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	stageInst, err := s.parallelStageInstance(wfStore, runID, "parallel")
	if err != nil {
		t.Fatal(err)
	}
	stale := workflowstore.BranchTransitionResult{
		Branch:  instB,
		AllDone: true,
		Transition: workflowstore.TransitionResult{
			Run:     run,
			Current: stageInst,
			Done:    true, // the join this stale result observed would have finished the run
		},
	}
	if err := s.advanceParentAfterBranchCompletion(workspaceID, "sample", stale,
		httptest.NewRequest(http.MethodPost, "/", nil)); err != nil {
		t.Fatalf("stale completion must be a no-op, got %v", err)
	}
	after, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" || after.ActiveStepID != "parallel" {
		t.Fatalf("stale AllDone must not advance the run past a running wave, got %s@%s", after.Status, after.ActiveStepID)
	}
	persisted, err := s.ts.GetTask("sample", "pm", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status == entity.TaskStatusDoneSuccess {
		t.Fatalf("stale AllDone must not finish the parent task, got %s", persisted.Status)
	}
}

// TestPlanStageUnrelatedCompletionStillAdvancesAfterPlanDone keeps the guard
// honest: once the LAST work package completes, the stage must still join and
// advance — the authoritative recheck may not wedge a legitimately finished
// plan-driven stage.
func TestPlanStageUnrelatedCompletionStillAdvancesAfterPlanDone(t *testing.T) {
	s, workspaceID, baseCommit := planStageFixtures(t)
	plan := fixtureDeliveryPlan("")
	seedPlanStageParentRun(t, s, workspaceID, baseCommit, plan, true)
	runID := planRunIDFor(t, s, workspaceID, "task-plan-root")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	if rec := postBranchStepComplete(t, s, workspaceID, "task-plan-root", map[string]string{
		"branch_summary": "contract frozen",
		"touched_paths":  "contract.md",
	}); rec.Code != http.StatusOK {
		t.Fatalf("fan-out must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Complete wp-a (unlocks wp-c), then wp-b, then wp-c — the last one must
	// join and finish the run.
	for _, wp := range []string{"wp-a", "wp-b"} {
		inst, found := branchInstanceFor(t, wfStore, runID, "parallel", wp)
		if !found {
			t.Fatalf("branch %s must exist", wp)
		}
		task, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(task.WorktreeDir, "module_"+wp+".go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rec := postBranchStepComplete(t, s, workspaceID, inst.ChildTaskID, map[string]string{
			"branch_summary": wp + " done",
			"touched_paths":  "module_" + wp + ".go",
		}); rec.Code != http.StatusOK {
			t.Fatalf("branch %s completion must be 200, got %d: %s", wp, rec.Code, rec.Body.String())
		}
	}
	instC, found := branchInstanceFor(t, wfStore, runID, "parallel", "wp-c")
	if !found {
		t.Fatal("wp-c must be materialized once its dependency completed")
	}
	taskC, err := s.ts.GetTask("sample", "pm", instC.ChildTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskC.WorktreeDir, "module_c.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := postBranchStepComplete(t, s, workspaceID, instC.ChildTaskID, map[string]string{
		"branch_summary": "module C done",
		"touched_paths":  "module_c.go",
	}); rec.Code != http.StatusOK {
		t.Fatalf("wp-c completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	after, _, err := wfStore.RunForTask("sample", "task-plan-root")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "completed" {
		t.Fatalf("a fully delivered plan must still finish the run, got %s@%s", after.Status, after.ActiveStepID)
	}
}
