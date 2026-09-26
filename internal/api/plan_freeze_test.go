package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// --- slice 4 formal-entry fixtures -----------------------------------------

// seedGreenfieldRunTask starts a task on the REAL greenfield template with an
// actor binding for every role (human reviews → admin).
func seedGreenfieldRunTask(t *testing.T, s *Server, workspaceID, taskID, baseCommit string) entity.WorkflowDefinition {
	t.Helper()
	def, ok := workflowstore.DefinitionFromTemplate("greenfield-delivery-pipeline", "en", "GF slice4")
	if !ok {
		t.Fatal("greenfield template missing")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := &entity.Task{
		ID: taskID, Title: "GF delivery " + taskID, Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "sample/pm", CreatedAt: now, UpdatedAt: now,
		BaseCommit: baseCommit, BaseBranch: "main", Vars: map[string]string{},
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	bindings := map[string]entity.WorkflowActorBinding{}
	for _, step := range def.Steps {
		if strings.TrimSpace(step.ActorRole) == "" {
			continue
		}
		if step.Type == "human_review" {
			bindings[step.ActorRole] = entity.WorkflowActorBinding{Type: "human", ID: "admin"}
			continue
		}
		bindings[step.ActorRole] = entity.WorkflowActorBinding{Type: "agent", ID: "pm"}
		for _, branch := range step.Branches {
			if role := strings.TrimSpace(branch.ActorRole); role != "" {
				bindings[role] = entity.WorkflowActorBinding{Type: "agent", ID: "pm"}
			}
		}
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, bindings); err != nil {
		t.Fatal(err)
	}
	return def
}

// samplePlanJSON is the structured plan a scale_gate agent would emit: two
// wave-0 packages plus one dependent package, anchored on the requirement
// snapshot ids.
func samplePlanJSON() string {
	return `{
	  "planId": "plan-slice4",
	  "workPackages": [
	    {"id":"wp-session","title":"Session lifecycle","domain":"frontend session",
	     "acceptanceCriteria":["uc-1"],"agentBinding":"pm","expectedDelivery":["console-web/src/api"]},
	    {"id":"wp-audit","title":"Audit export","domain":"server audit",
	     "acceptanceCriteria":["uc-2"],"agentBinding":"pm","expectedDelivery":["server/audit"]},
	    {"id":"wp-wire","title":"Wire the export client","domain":"integration",
	     "dependsOn":["wp-session"],"acceptanceCriteria":["uc-1","uc-2"],"agentBinding":"pm"}
	  ],
	  "sharedContract": [{"id":"api-skeleton","artifact":"audit export endpoint","path":"server/audit"}]
	}`
}

func requirementItemsJSON() string {
	return `[{"id":"uc-1","text":"session expiry clears local state","source":"requirements.md"},
	         {"id":"uc-2","text":"audit export is capped","source":"requirements.md"}]`
}

// driveToContractReview walks the formal chain up to (not including) the
// contract review approval: requirement_draft → requirement_review →
// design_review → scale_gate → contract_batch.
func driveToContractReview(t *testing.T, s *Server, workspaceID, taskID, planJSON string) {
	t.Helper()
	rec := postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"requirement_draft": "The console must clear local session state on expiry and export audit logs.",
		"open_questions":    "none",
		"requirement_items": requirementItemsJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("requirement_draft completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "requirement approved",
	}); err != nil {
		t.Fatalf("requirement_review approve failed (%d): %v", status, err)
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "design approved with waiver",
		Outputs: map[string]string{"design_waiver_reason": "acceptance run: UI prototype not required"},
	}); err != nil {
		t.Fatalf("design_review approve failed (%d): %v", status, err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"scale_verdict": "batched",
		"delivery_plan": planJSON,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("scale_gate completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = postBranchStepComplete(t, s, workspaceID, taskID, map[string]string{
		"contract_artifacts": "schema + error codes committed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("contract_batch completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func approveContractReview(t *testing.T, s *Server, workspaceID, taskID string) (int, error) {
	t.Helper()
	_, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
		Decision: "approve", Comments: "contract + plan approved",
	})
	return status, err
}

func runForTask(t *testing.T, s *Server, workspaceID, taskID string) entity.WorkflowRun {
	t.Helper()
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask("sample", taskID)
	if err != nil || !found {
		t.Fatalf("run lookup for %s: found=%v err=%v", taskID, found, err)
	}
	return run
}

// TestContractBatchReworkRepairsRequirementAnchors is the S2 hardening batch 2
// regression for the run4 finding (task t-20260926-jy5o8b shape): the freeze
// anchor chain reads requirement_items from contract_batch OUTPUTS, but the
// template declared the field input-only, so the request_changes→contract_batch
// rework loop could never repair a malformed requirement_draft snapshot — the
// step-complete whitelist rejected the field before it could flow to the
// freeze ("workflow output field %q is not defined on step"). The rehearsal
// only escaped via prompt-level workarounds.
//
// The test walks the exact task2 shape: requirement_draft emits a BARE string
// array (malformed), the first freeze attempt is refused (fail-closed), then
// contract_batch re-emits corrected requirement_items as its OUTPUT through
// the rework loop, and the freeze succeeds with the REWORKED anchors.
//
// Boundary (run4 task2 evidence, documented not fixed): the rework loop can
// only repair OUTPUT-shape defects. A rework that omits the field still
// freezes nothing — the anchor chain falls through to the malformed
// requirement_draft output and the freeze refuses again (fail-closed kept).
func TestContractBatchReworkRepairsRequirementAnchors(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-anchor-rework", baseCommit)

	// Walk the chain with a MALFORMED requirement snapshot (bare string
	// array — what the run4 agent actually produced): the task2 prompt shape.
	malformedAnchors := `["uc-1:POST /fingerprints computes SHA-256","uc-2:same fingerprint dedupes"]`
	rec := postBranchStepComplete(t, s, workspaceID, "task-anchor-rework", map[string]string{
		"requirement_draft": "The service must fingerprint content and dedupe registrations.",
		"open_questions":    "none",
		"requirement_items": malformedAnchors,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("requirement_draft completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-anchor-rework", workflowReviewBody{
		Decision: "approve", Comments: "requirement approved",
	}); err != nil {
		t.Fatalf("requirement_review approve failed (%d): %v", status, err)
	}
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-anchor-rework", workflowReviewBody{
		Decision: "approve", Comments: "design approved with waiver",
		Outputs: map[string]string{"design_waiver_reason": "acceptance run: UI prototype not required"},
	}); err != nil {
		t.Fatalf("design_review approve failed (%d): %v", status, err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, "task-anchor-rework", map[string]string{
		"scale_verdict": "batched",
		"delivery_plan": samplePlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("scale_gate completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = postBranchStepComplete(t, s, workspaceID, "task-anchor-rework", map[string]string{
		"contract_artifacts": "schema + error codes committed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("contract_batch completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The rework edge forwards the OUTPUT side (P1-1), so the human review's
	// anchor view is empty until contract_batch re-emits the field. That is
	// fail-closed: approving still refuses the freeze because the anchor
	// chain falls back to the malformed requirement_draft output. (The
	// previous edge forwarded the stale INPUT snapshot instead, letting a
	// reviewer approve anchors the freeze would not consume.)
	if status, err := approveContractReview(t, s, workspaceID, "task-anchor-rework"); err == nil {
		t.Fatalf("approving with an unrepaired malformed anchor snapshot must refuse the freeze (got status %d)", status)
	}

	// Request changes → contract_batch re-emits CORRECTED requirement_items
	// as its OUTPUT (now that the template declares it), then approve → the
	// freeze succeeds and carries the reworked anchors.
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-anchor-rework", workflowReviewBody{
		Decision: "request_changes", Comments: "requirement_items malformed; re-emit as structured output",
	}); err != nil {
		t.Fatalf("contract_review request_changes failed (%d): %v", status, err)
	}
	rec = postBranchStepComplete(t, s, workspaceID, "task-anchor-rework", map[string]string{
		"contract_artifacts": "schema + error codes committed (rework)",
		"requirement_items":  requirementItemsJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reworked contract_batch completion must accept the requirement_items output, got %d: %s", rec.Code, rec.Body.String())
	}

	// The reworked output must flow to the human review through the re-mapped
	// edge (P1-1): the reviewer sees exactly what the freeze will consume.
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-anchor-rework")
	instances, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reviewInput string
	for _, inst := range instances {
		if inst.StepID == "contract_review" {
			reviewInput = inst.InputValues["requirement_items"]
		}
	}
	if !strings.Contains(reviewInput, "session expiry clears local state") {
		t.Fatalf("contract_review must receive the REWORKED requirement_items output, got %q", reviewInput)
	}

	if status, err := approveContractReview(t, s, workspaceID, "task-anchor-rework"); err != nil {
		t.Fatalf("contract_review approve after rework failed (%d): %v", status, err)
	}
	record, _, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil || !ok {
		t.Fatalf("approve after rework must freeze the plan: ok=%v err=%v", ok, err)
	}
	version, vOK := record.Current()
	if !vOK {
		t.Fatalf("frozen record must carry a current version: %+v", record)
	}
	if len(version.Plan.RequirementItems) != 2 || version.Plan.RequirementItems[0].ID != "uc-1" || version.Plan.RequirementItems[0].Text != "session expiry clears local state" {
		t.Fatalf("the frozen plan must carry the REWORKED anchors, got %+v", version.Plan.RequirementItems)
	}
	// The malformed requirement_draft snapshot must not leak into the freeze.
	for _, item := range version.Plan.RequirementItems {
		if strings.HasPrefix(item.Text, "uc-") {
			t.Fatalf("frozen anchors must not come from the malformed bare-array snapshot: %+v", item)
		}
	}
	_ = baseCommit
}

// TestFormalEntryChainFreezesPlanAndDrivesPlannedWaves is the mandated
// end-to-end chain: requirement_review → scale_gate → contract_batch →
// contract_review approve → wave 1 → dependency-unlocked wave 2 → join, all
// through the real template, without seeding any branch instance.
func TestFormalEntryChainFreezesPlanAndDrivesPlannedWaves(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-slice4", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-slice4", samplePlanJSON())

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-slice4")
	if run.ActiveStepID != "contract_review" {
		t.Fatalf("chain must park at contract_review, got %s", run.ActiveStepID)
	}

	// Approve: the plan is frozen INSIDE the same transition that advances the
	// run to the parallel stage.
	if status, err := approveContractReview(t, s, workspaceID, "task-slice4"); err != nil {
		t.Fatalf("contract_review approve failed (%d): %v", status, err)
	}
	record, version, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil || !ok {
		t.Fatalf("approve must freeze the plan atomically: ok=%v err=%v", ok, err)
	}
	if version.Status != workflowstore.PlanStatusFrozen || version.Digest == "" || version.ApprovedBy != "admin" {
		t.Fatalf("frozen version incomplete: %+v", version)
	}
	if version.Approval == nil || version.Approval.StepID != "contract_review" || version.Approval.InstanceID == "" {
		t.Fatalf("frozen version must carry the approving review provenance, got %+v", version.Approval)
	}
	if len(version.Plan.RequirementItems) == 0 || version.Plan.RequirementItems[0].ID != "uc-1" {
		t.Fatalf("the frozen plan must carry the requirement snapshot, got %+v", version.Plan.RequirementItems)
	}

	run = runForTask(t, s, workspaceID, "task-slice4")
	if run.Status != "active" || run.ActiveStepID != "parallel_workstreams" {
		t.Fatalf("run must advance to the parallel stage, got %s@%s", run.Status, run.ActiveStepID)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel_workstreams")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("wave 0 must materialize its two ready work packages, got %d: %+v", len(instances), instances)
	}
	childByWP := map[string]string{}
	for _, inst := range instances {
		if inst.BranchID != "wp-session" && inst.BranchID != "wp-audit" {
			t.Fatalf("unexpected wave-0 branch %q (wave 1 must wait)", inst.BranchID)
		}
		childByWP[inst.BranchID] = inst.ChildTaskID
		task, err := s.ts.GetTask("sample", "pm", inst.ChildTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Vars[workflowPlanIDVar] != "plan-slice4" || task.Vars[workflowPlanWPIDVar] != inst.BranchID {
			t.Fatalf("branch %s task must carry the plan identity: %v", inst.BranchID, task.Vars)
		}
		if task.BaseCommit != baseCommit || task.WorktreeDir == "" {
			t.Fatalf("branch %s must be materialized at the frozen baseline", inst.BranchID)
		}
		if _, ok, err := wfStore.LoadQABaselinePayload("sample", inst.ChildTaskID); err != nil || !ok {
			t.Fatalf("branch %s control-plane QA baseline missing (ok=%v err=%v)", inst.BranchID, ok, err)
		}
	}

	// Wave 1 completes → its dependent unlocks wave 2 through the existing
	// completion path.
	completePlannedBranch(t, s, workspaceID, childByWP["wp-session"], "module_session.go")
	instWire, found := branchInstanceFor(t, wfStore, run.ID, "parallel_workstreams", "wp-wire")
	if !found {
		t.Fatal("wp-wire must be materialized once wp-session completed")
	}
	completePlannedBranch(t, s, workspaceID, childByWP["wp-audit"], "module_audit.go")
	completePlannedBranch(t, s, workspaceID, instWire.ChildTaskID, "module_wire.go")

	run = runForTask(t, s, workspaceID, "task-slice4")
	if run.Status != "active" || run.ActiveStepID != "integration_review" {
		t.Fatalf("the stage must join into integration_review, got %s@%s", run.Status, run.ActiveStepID)
	}
	record, _, _, err = wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Materializations) != 3 {
		t.Fatalf("every work package must be pinned exactly once, got %+v", record.Materializations)
	}
}

func completePlannedBranch(t *testing.T, s *Server, workspaceID, childTaskID, filename string) {
	t.Helper()
	task, err := s.ts.GetTask("sample", "pm", childTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreeDir, filename), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, childTaskID, map[string]string{
		"branch_summary": filename + " delivered",
		"touched_paths":  filename,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("branch %s completion must be 200, got %d: %s", childTaskID, rec.Code, rec.Body.String())
	}
}

// TestPlanFreezeRefusedCasesLeaveNoRecordAndNoAdvance: request_changes, a bad
// JSON plan, a dangling requirement reference and a DAG cycle must each leave
// ZERO frozen records and keep the run parked — the freeze is fail-closed and
// belongs to the approving transition only.
func TestPlanFreezeRefusedCasesLeaveNoRecordAndNoAdvance(t *testing.T) {
	cases := []struct {
		name     string
		planJSON string
		decision string
		want     string
	}{
		{name: "request_changes", decision: "request_changes"},
		{name: "bad json", planJSON: "not-json-at-all", decision: "approve", want: "not parseable"},
		{name: "unknown anchor", planJSON: `{"workPackages":[{"id":"wp-a","title":"A","acceptanceCriteria":["uc-404"],"agentBinding":"pm"}]}`, decision: "approve", want: "unknown requirement item"},
		{name: "dependency cycle", planJSON: `{"workPackages":[{"id":"wp-a","title":"A","dependsOn":["wp-b"],"acceptanceCriteria":["uc-1"],"agentBinding":"pm"},{"id":"wp-b","title":"B","dependsOn":["wp-a"],"acceptanceCriteria":["uc-1"],"agentBinding":"pm"}]}`, decision: "approve", want: "dependency cycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, workspaceID := newBranchJoinHTTPServer(t)
			s.worktreeMgr = gitworktreeManagerForTest()
			_, baseCommit := buildFanoutGitWorkspace(t, s)
			taskID := "task-slice4-" + strings.ReplaceAll(tc.name, " ", "-")
			seedGreenfieldRunTask(t, s, workspaceID, taskID, baseCommit)
			driveToContractReview(t, s, workspaceID, taskID, tc.planJSON)
			wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
			run := runForTask(t, s, workspaceID, taskID)

			_, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", taskID, workflowReviewBody{
				Decision: tc.decision, Comments: tc.name,
			})
			if tc.decision == "approve" {
				if err == nil {
					t.Fatalf("an invalid plan must be refused")
				}
				if status != http.StatusBadRequest && status != http.StatusConflict {
					t.Fatalf("refusal must be 400/409, got %d (%v)", status, err)
				}
				if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("refusal must name the problem (%q), got %v", tc.want, err)
				}
			} else if err != nil {
				t.Fatalf("review submission failed (%d): %v", status, err)
			}

			if _, _, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID); err != nil || ok {
				t.Fatalf("no frozen plan may exist (ok=%v err=%v)", ok, err)
			}
			// No pre-transition side effects on a REFUSED APPROVAL: the review
			// instance must still be the untouched active instance and no
			// review event may exist (request_changes legitimately completes
			// the step, so it is checked separately below).
			reviewInstances := 0
			for _, inst := range mustStepInstances(t, wfStore, run.ID) {
				if inst.StepID != "contract_review" {
					continue
				}
				reviewInstances++
				if tc.decision == "approve" && strings.TrimSpace(inst.OutputValues["decision"]) != "" {
					t.Fatalf("a refused approval must not record a decision: %+v", inst.OutputValues)
				}
			}
			if reviewInstances != 1 {
				t.Fatalf("the review step must keep exactly one instance, got %d", reviewInstances)
			}
			if tc.decision == "approve" {
				for _, ev := range mustStepEvents(t, wfStore, run.ID) {
					if ev.StepID == "contract_review" {
						t.Fatalf("a refused approval must not append a review event: %+v", ev)
					}
				}
			}
			after := runForTask(t, s, workspaceID, taskID)
			if after.ActiveStepID == "parallel_workstreams" {
				t.Fatalf("the run must not reach the parallel stage without a frozen plan")
			}
			if tc.decision == "request_changes" && after.ActiveStepID != "contract_batch" {
				t.Fatalf("request_changes must route back to contract_batch, got %s", after.ActiveStepID)
			}
			if tc.decision == "approve" && after.ActiveStepID != "contract_review" {
				t.Fatalf("a refused approval must leave the run parked at the review, got %s", after.ActiveStepID)
			}
		})
	}
}

// TestPlanFreezeReworkRetryIsReentrant: a refused approval leaves no partial
// state; the reviewer requests changes, the contract batch re-emits a corrected
// plan (newest carrier wins), and the retried approval freezes exactly one
// version and materializes each work package once.
func TestPlanFreezeReworkRetryIsReentrant(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-slice4-retry", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-slice4-retry", "not-json")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-slice4-retry")

	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-slice4-retry", workflowReviewBody{
		Decision: "approve",
	}); err == nil {
		t.Fatalf("the malformed plan must refuse the approval (status %d)", status)
	}
	if _, _, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID); err != nil || ok {
		t.Fatalf("a refused approval must leave no frozen record (ok=%v err=%v)", ok, err)
	}
	if after := runForTask(t, s, workspaceID, "task-slice4-retry"); after.ActiveStepID != "contract_review" {
		t.Fatalf("the run must stay parked at contract_review, got %s", after.ActiveStepID)
	}

	// request_changes → contract_batch re-emits the corrected plan.
	if _, status, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-slice4-retry", workflowReviewBody{
		Decision: "request_changes", Comments: "plan is malformed; re-emit it",
	}); err != nil {
		t.Fatalf("request_changes failed (%d): %v", status, err)
	}
	rec := postBranchStepComplete(t, s, workspaceID, "task-slice4-retry", map[string]string{
		"contract_artifacts": "schema + error codes committed",
		"delivery_plan":      samplePlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reworked contract_batch completion must be 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if status, err := approveContractReview(t, s, workspaceID, "task-slice4-retry"); err != nil {
		t.Fatalf("retry approval must succeed (%d): %v", status, err)
	}
	record, version, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil || !ok {
		t.Fatalf("retry must freeze the plan: ok=%v err=%v", ok, err)
	}
	if len(record.Versions) != 1 {
		t.Fatalf("the retry must freeze exactly one version, got %d", len(record.Versions))
	}
	if version.Plan.PlanID != "plan-slice4" || len(version.Plan.WorkPackages) != 3 {
		t.Fatalf("the frozen plan must be the corrected one, got %+v", version.Plan)
	}
	instances, err := wfStore.BranchInstancesForStep(run.ID, "parallel_workstreams")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, inst := range instances {
		seen[inst.BranchID]++
	}
	if len(seen) != 2 {
		t.Fatalf("only wave 0 may materialize, got %v", seen)
	}
	for wp, count := range seen {
		if count != 1 {
			t.Fatalf("work package %s materialized %d times", wp, count)
		}
	}
	// Approving the consumed review again must not mint a second version.
	if _, _, err := s.submitTaskWorkflowReview(httptest.NewRequest(http.MethodPost, "/", nil), workspaceID, "sample", "task-slice4-retry", workflowReviewBody{
		Decision: "approve",
	}); err == nil {
		t.Fatal("re-approving a consumed review step must be refused")
	}
	record, _, _, err = wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Versions) != 1 {
		t.Fatalf("a re-approval must not mint a second version, got %d", len(record.Versions))
	}
}

// TestDeliveryPlanViewExposesMachinePointers checks the acceptance-table data
// plane: approval provenance, per-work-package pointers (task vars, worktree,
// QA baseline, branch status) and the recorded human sign-offs.
func TestDeliveryPlanViewExposesMachinePointers(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-slice4-table", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-slice4-table", samplePlanJSON())
	if status, err := approveContractReview(t, s, workspaceID, "task-slice4-table"); err != nil {
		t.Fatalf("approve failed (%d): %v", status, err)
	}

	view, status, err := s.deliveryPlanView("sample", "task-slice4-table")
	if err != nil || status != http.StatusOK {
		t.Fatalf("delivery plan view failed (%d): %v", status, err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"planId":"plan-slice4"`, `"approvedBy":"admin"`, `"wp-session"`, `"wp-audit"`,
		`"qaBaselinePresent":true`, `"worktreeDir"`, `"branchStatus":"running"`,
		`"stepId":"contract_review"`, `"decision":"approve"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("acceptance data plane must expose %s; body: %s", want, body)
		}
	}
	rows, ok := view["materializations"].([]planAcceptanceRow)
	if !ok || len(rows) != 2 {
		t.Fatalf("wave-0 rows expected, got %T len=%d", view["materializations"], len(rows))
	}
	for _, row := range rows {
		if row.TaskVars[workflowPlanVersionVar] == "" || row.ChildRunID == "" {
			t.Fatalf("row %s must carry plan vars + child run: %+v", row.WPID, row)
		}
	}
}

// TestTriggerReviewPathFreezesThroughTheSameEntry: a plan-freezing review that
// arrives through the trigger/ChatOps callback must use the same freeze entry
// as the in-console review — a malformed plan aborts (no record, run parked),
// a valid one freezes atomically with the advance.
func TestTriggerReviewPathFreezesThroughTheSameEntry(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-slice4-trigger", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-slice4-trigger", "not-json")
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-slice4-trigger")
	record := workflowNotificationRecord{
		ID: "n-1", WorkspaceID: workspaceID, Project: "sample", TaskID: "task-slice4-trigger",
		StepID: "contract_review", RecipientUserID: "admin",
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	if _, err := s.submitWorkflowReviewFromTrigger(workspaceID, record, workflowTriggerCallbackBody{Decision: "approve", Outputs: map[string]string{}}, req); err == nil {
		t.Fatal("a malformed plan must refuse a trigger-path approval")
	}
	if _, _, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID); err != nil || ok {
		t.Fatalf("a refused trigger approval must leave no record (ok=%v err=%v)", ok, err)
	}
	if after := runForTask(t, s, workspaceID, "task-slice4-trigger"); after.ActiveStepID != "contract_review" {
		t.Fatalf("the run must stay parked, got %s", after.ActiveStepID)
	}
	// The reviewer requests changes, the contract batch re-emits a valid plan,
	// then the approval arrives through the trigger callback.
	if _, err := s.submitWorkflowReviewFromTrigger(workspaceID, record, workflowTriggerCallbackBody{Decision: "request_changes", Comments: "fix the plan", Outputs: map[string]string{}}, req); err != nil {
		t.Fatalf("trigger request_changes failed: %v", err)
	}
	if after := runForTask(t, s, workspaceID, "task-slice4-trigger"); after.ActiveStepID != "contract_batch" {
		t.Fatalf("request_changes must route back to contract_batch, got %s", after.ActiveStepID)
	}
	rec := postBranchStepComplete(t, s, workspaceID, "task-slice4-trigger", map[string]string{
		"contract_artifacts": "schema + error codes committed",
		"delivery_plan":      samplePlanJSON(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reworked contract_batch must complete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := s.submitWorkflowReviewFromTrigger(workspaceID, record, workflowTriggerCallbackBody{Decision: "approve", Comments: "ok via chatops", Outputs: map[string]string{}}, req); err != nil {
		t.Fatalf("trigger-path approval failed: %v", err)
	}
	_, version, ok, err := wfStore.LoadFrozenPlanForRun("sample", run.ID)
	if err != nil || !ok {
		t.Fatalf("the trigger approval must freeze the plan: ok=%v err=%v", ok, err)
	}
	if version.ApprovedBy != "admin" || version.Approval == nil || version.Approval.StepID != "contract_review" {
		t.Fatalf("trigger freeze provenance incomplete: %+v", version)
	}
	if after := runForTask(t, s, workspaceID, "task-slice4-trigger"); after.ActiveStepID != "parallel_workstreams" {
		t.Fatalf("the trigger approval must advance the run, got %s", after.ActiveStepID)
	}
}

func mustStepInstances(t *testing.T, wfStore *workflowstore.Store, runID string) []entity.WorkflowStepInstance {
	t.Helper()
	instances, err := wfStore.ListStepInstances(runID)
	if err != nil {
		t.Fatal(err)
	}
	return instances
}

func mustStepEvents(t *testing.T, wfStore *workflowstore.Store, runID string) []entity.WorkflowStepEvent {
	t.Helper()
	events, err := wfStore.ListStepEvents(runID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// TestPlanFreezeExtrasAreDeterministicForOneApproval: the extras closure reads
// ONLY the plan record inside the transition — the plan text/digest handed to
// it is fixed per approval, so a second preparation of the same approval
// yields byte-identical record content (no timestamp-driven digest drift).
func TestPlanFreezeExtrasAreDeterministicForOneApproval(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	s.worktreeMgr = gitworktreeManagerForTest()
	_, baseCommit := buildFanoutGitWorkspace(t, s)
	seedGreenfieldRunTask(t, s, workspaceID, "task-slice4-determinism", baseCommit)
	driveToContractReview(t, s, workspaceID, "task-slice4-determinism", samplePlanJSON())
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run := runForTask(t, s, workspaceID, "task-slice4-determinism")

	planA, err := s.buildDeliveryPlanForFreeze(wfStore, run)
	if err != nil {
		t.Fatal(err)
	}
	planB, err := s.buildDeliveryPlanForFreeze(wfStore, run)
	if err != nil {
		t.Fatal(err)
	}
	digestA, err := workflowstore.PlanDigest(planA)
	if err != nil {
		t.Fatal(err)
	}
	digestB, err := workflowstore.PlanDigest(planB)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("repeated plan assembly for one approval must be digest-stable: %s vs %s", digestA, digestB)
	}
	if _, status, err := s.preparePlanFreezeForReview(wfStore, "sample", run, planFreezeStepForTest(t, wfStore, run), "admin", "ok"); err != nil {
		t.Fatalf("preparing the freeze twice must not fail (%d): %v", status, err)
	}
	if _, status, err := s.preparePlanFreezeForReview(wfStore, "sample", run, planFreezeStepForTest(t, wfStore, run), "admin", "ok"); err != nil {
		t.Fatalf("second preparation must not fail (%d): %v", status, err)
	}
}

func planFreezeStepForTest(t *testing.T, wfStore *workflowstore.Store, run entity.WorkflowRun) entity.WorkflowStep {
	t.Helper()
	def, ok, err := wfStore.RunDefinition(run)
	if err != nil || !ok {
		t.Fatalf("definition: ok=%v err=%v", ok, err)
	}
	for _, step := range def.Steps {
		if step.ID == run.ActiveStepID {
			return step
		}
	}
	t.Fatalf("active step %s missing from the definition", run.ActiveStepID)
	return entity.WorkflowStep{}
}

// TestStepOutputCandidatesOrdersByRecencyAndSkipsBlanks pins the small
// candidate-selection contract the freeze depends on.
func TestStepOutputCandidatesOrdersByRecencyAndSkipsBlanks(t *testing.T) {
	outputs := map[string]map[string]string{
		"scale_gate":     {"delivery_plan": "old"},
		"contract_batch": {"delivery_plan": "new"},
	}
	got := stepOutputCandidates(outputs, []string{"contract_batch", "scale_gate"},
		[]string{"scale_gate", "contract_batch"}, []string{"delivery_plan"})
	if len(got) != 2 || got[0] != "new" || got[1] != "old" {
		t.Fatalf("recency order must win, got %v", got)
	}
	got = stepOutputCandidates(outputs, []string{"contract_batch", "scale_gate"},
		[]string{"scale_gate", "contract_batch"}, []string{"batch_plan", "delivery_plan"})
	if len(got) != 2 || got[0] != "new" {
		t.Fatalf("key aliases must be consulted, got %v", got)
	}
	if got := stepOutputCandidates(nil, nil, nil, nil); len(got) != 0 {
		t.Fatalf("empty inputs must yield no candidates, got %v", got)
	}
}
