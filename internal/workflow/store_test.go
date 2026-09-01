package workflow

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func TestNormalizeWorkflowOutputValuesRequiresStructuredOutputs(t *testing.T) {
	step := entity.WorkflowStep{
		Title:        "PM Spec",
		OutputFields: []entity.WorkflowField{{Name: "spec_doc_id", Description: "Spec docID."}},
	}
	_, err := normalizeWorkflowOutputValues(step, nil, "", "plain text", false)
	if err == nil {
		t.Fatal("expected missing structured output error")
	}
	if !strings.Contains(err.Error(), "requires structured outputs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeWorkflowOutputValuesSkipsRequiredCheckForOptionalFields(t *testing.T) {
	step := entity.WorkflowStep{
		Title: "人工代码审核",
		OutputFields: []entity.WorkflowField{
			{Name: "decision", Description: "approve 或 request_changes。"},
			{Name: "approved_change", Description: "人工审核通过的代码产物。", Optional: true},
		},
	}
	values, err := normalizeWorkflowOutputValues(step, map[string]string{"decision": "approve"}, "", "", false)
	if err != nil {
		t.Fatalf("optional field left empty should pass validation: %v", err)
	}
	if _, ok := values["approved_change"]; ok {
		t.Fatalf("optional field should stay absent when empty, got %q", values["approved_change"])
	}
	// A required field missing still fails, even alongside an optional one.
	if _, err := normalizeWorkflowOutputValues(step, map[string]string{"approved_change": "feat/x @ abc1234"}, "", "", false); err == nil {
		t.Fatal("expected required decision error")
	}
}

func TestNormalizeWorkflowOutputValuesValidatesDocIDFields(t *testing.T) {
	step := entity.WorkflowStep{
		Title: "PM Spec",
		OutputFields: []entity.WorkflowField{
			{Name: "spec_doc_id", Description: "Knowledge base document docID."},
			{Name: "summary", Description: "Short summary."},
		},
	}

	_, err := normalizeWorkflowOutputValues(step, map[string]string{
		"spec_doc_id": "I created doc-20260728-abc123",
		"summary":     "done",
	}, "", "", false)
	if err == nil {
		t.Fatal("expected invalid docID error")
	}
	if !strings.Contains(err.Error(), "must be a knowledge docID") {
		t.Fatalf("unexpected error: %v", err)
	}

	values, err := normalizeWorkflowOutputValues(step, map[string]string{
		"spec_doc_id": "doc-20260728-abc123",
		"summary":     "done",
	}, "", "", false)
	if err != nil {
		t.Fatalf("expected valid docID output: %v", err)
	}
	if values["spec_doc_id"] != "doc-20260728-abc123" {
		t.Fatalf("unexpected docID value: %q", values["spec_doc_id"])
	}
}

func TestWorkflowDocIDValueValidAllowsLists(t *testing.T) {
	if !workflowDocIDValueValid("doc-20260728-abc123, doc-20260728-def456") {
		t.Fatal("expected comma-separated docID list to be valid")
	}
	if workflowDocIDValueValid("doc-20260728-abc123 and doc-20260728-def456") {
		t.Fatal("expected prose docID value to be invalid")
	}
}

func TestNormalizeWorkflowOutputValuesAllowsExplicitNoneForDocIDFields(t *testing.T) {
	step := entity.WorkflowStep{
		Title: "Human Review",
		OutputFields: []entity.WorkflowField{
			{Name: "fix_request_doc_id", Description: "If no fix is needed, 不需要则填 none."},
			{Name: "decision", Description: "Decision."},
		},
	}
	values, err := normalizeWorkflowOutputValues(step, map[string]string{
		"fix_request_doc_id": "none",
		"decision":           "approve",
	}, "", "", false)
	if err != nil {
		t.Fatalf("expected explicit none to be accepted: %v", err)
	}
	if values["fix_request_doc_id"] != "none" {
		t.Fatalf("unexpected value: %q", values["fix_request_doc_id"])
	}
}

func TestWorkflowConditionEqDoesNotUseSubstringMatching(t *testing.T) {
	if compareWorkflowValue("不通过", "eq", "通过", nil) {
		t.Fatal("expected exact eq comparison; 不通过 must not match 通过")
	}
	if !compareWorkflowValue("通过", "eq", "通过", nil) {
		t.Fatal("expected exact eq comparison to match identical values")
	}
}

func TestWorkflowConditionInDoesNotUseSubstringMatching(t *testing.T) {
	if compareWorkflowValue("不通过", "in", "", []string{"通过", "approve"}) {
		t.Fatal("expected exact in comparison; 不通过 must not match 通过")
	}
	if !compareWorkflowValue("approve", "in", "", []string{"通过", "approve"}) {
		t.Fatal("expected exact in comparison to match listed value")
	}
}

func TestSoftwareDeliveryTemplateHasPRReviewLoop(t *testing.T) {
	tmpl, ok := Template("agentic-software-delivery", "en")
	if !ok {
		t.Fatal("expected software delivery template")
	}

	steps := make(map[string]entity.WorkflowStep, len(tmpl.Steps))
	for _, step := range tmpl.Steps {
		steps[step.ID] = step
	}
	for _, id := range []string{"implementation", "code_review", "changelog_cleanup", "create_pr", "pr_review", "merge_and_sync", "qa"} {
		if _, ok := steps[id]; !ok {
			t.Fatalf("expected step %q", id)
		}
	}
	if steps["changelog_cleanup"].Type != "agent_task" || steps["create_pr"].Type != "agent_task" || steps["merge_and_sync"].Type != "agent_task" {
		t.Fatal("expected changelog cleanup, create PR, and merge_and_sync to be agent tasks")
	}
	if steps["pr_review"].Type != "human_review" {
		t.Fatal("expected PR review to be a human review gate")
	}

	findEdge := func(from, to string) entity.WorkflowEdge {
		for _, edge := range tmpl.Edges {
			if edge.From == from && edge.To == to {
				return edge
			}
		}
		t.Fatalf("expected edge %s -> %s", from, to)
		return entity.WorkflowEdge{}
	}
	if edge := findEdge("code_review", "changelog_cleanup"); edge.Condition == nil || edge.Condition.Value != "approve" {
		t.Fatal("expected approved code review to enter changelog cleanup")
	}
	if edge := findEdge("pr_review", "merge_and_sync"); edge.Condition == nil || !strings.Contains(edge.Condition.Value, "approve") {
		t.Fatal("expected approved PR review to enter merge_and_sync")
	}
	if edge := findEdge("merge_and_sync", "qa"); !edge.IsDefault {
		t.Fatal("expected merge_and_sync to enter QA by default")
	}
	if edge := findEdge("pr_review", "implementation"); edge.Condition == nil || edge.Condition.Value != "request_changes" {
		t.Fatal("expected requested PR changes to return to implementation")
	}
}

func TestSeededSoftwareDeliveryHasPRReviewLoop(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-seed", Name: "Workspace", Slug: "workspace-seed", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-seed")
	if err := store.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	def, ok, err := store.Definition("software-delivery-v1")
	if err != nil || !ok {
		t.Fatalf("load seeded definition: ok=%v err=%v", ok, err)
	}
	if def.Version < 5 {
		t.Fatalf("expected seeded workflow version >= 5, got %d", def.Version)
	}
	seen := map[string]bool{}
	for _, step := range def.Steps {
		seen[step.ID] = true
	}
	for _, id := range []string{"changelog_cleanup", "create_pr", "pr_review", "merge_and_sync"} {
		if !seen[id] {
			t.Fatalf("expected seeded step %q", id)
		}
	}
	initDef, ok, err := store.Definition(ProjectInitializationWorkflowID)
	if err != nil || !ok {
		t.Fatalf("load initialization workflow: ok=%v err=%v", ok, err)
	}
	if len(initDef.Steps) != 5 || initDef.StartStepID != "prepare" {
		t.Fatalf("unexpected initialization workflow: %#v", initDef)
	}
}

func TestProjectInitializationFailureStaysRetryable(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-init", Name: "Workspace", Slug: "workspace-init", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-init")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID: ProjectInitializationWorkflowID, Name: "Project Initialization", Version: 1,
		Scope: "workspace", StartStepID: "prepare",
		Steps: []entity.WorkflowStep{
			{ID: "prepare", Type: "agent_task", Title: "Prepare", ActorRole: "project-initializer", OutputFields: []entity.WorkflowField{{Name: "result"}}},
			{ID: "verify", Type: "agent_task", Title: "Verify", ActorRole: "project-initializer", OutputFields: []entity.WorkflowField{{Name: "result"}}},
		},
		Edges:     []entity.WorkflowEdge{{ID: "next", From: "prepare", To: "verify", IsDefault: true}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := store.StartRun("project", "task-init", def.ID, map[string]entity.WorkflowActorBinding{
		"project-initializer": {Type: "agent", ID: "lina"},
	}); err != nil {
		t.Fatalf("start run: %v", err)
	}

	failed, err := store.CompleteAndAdvance("project", "task-init", "network unavailable", "", nil, "failed")
	if err != nil {
		t.Fatalf("complete failed step: %v", err)
	}
	if failed.Done || failed.Next == nil || failed.Run.Status != "failed" || failed.Run.ActiveStepID != "prepare" {
		t.Fatalf("failure should remain retryable at prepare: %#v", failed)
	}
	steps, err := store.ListStepInstances(failed.Run.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	prepare, ok := workflowStepInstanceByIDForTest(steps, "prepare")
	if !ok || prepare.Status != "failed" {
		t.Fatalf("expected failed prepare step, got %#v", prepare)
	}
}

func TestWorkflowActorBindingPrefersStepIDOverRole(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-step-binding",
		Name:        "Step Binding Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "draft",
		Steps: []entity.WorkflowStep{
			{
				ID:           "draft",
				Type:         "agent_task",
				Title:        "Draft",
				ActorRole:    "worker",
				OutputFields: []entity.WorkflowField{{Name: "draft_doc_id", Description: "Draft docID."}},
			},
			{
				ID:        "review",
				Type:      "agent_task",
				Title:     "Review",
				ActorRole: "worker",
			},
		},
		Edges:     []entity.WorkflowEdge{{ID: "e1", From: "draft", To: "review"}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		"worker": {Type: "agent", ID: "fallback-agent"},
		"draft":  {Type: "agent", ID: "analyst"},
		"review": {Type: "agent", ID: "reviewer"},
	}
	run, steps, err := store.StartRun("project", "task-1", def.ID, bindings)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.ActorBindings["draft"].ID != "analyst" {
		t.Fatalf("expected run to preserve step binding, got %#v", run.ActorBindings["draft"])
	}
	start, ok := workflowStepInstanceByIDForTest(steps, "draft")
	if !ok {
		t.Fatal("missing draft step instance")
	}
	if start.ActorID != "analyst" {
		t.Fatalf("expected draft actor analyst, got %q", start.ActorID)
	}

	transition, err := store.CompleteAndAdvance("project", "task-1", "draft done", "", map[string]string{"draft_doc_id": "doc-20260730-abc123"}, "completed")
	if err != nil {
		t.Fatalf("complete and advance: %v", err)
	}
	if transition.NextInst == nil {
		t.Fatal("expected next instance")
	}
	if transition.NextInst.Status != "pending" {
		t.Fatalf("expected next agent step to remain pending until a runtime starts, got %q", transition.NextInst.Status)
	}
	if transition.NextInst.ActorID != "reviewer" {
		t.Fatalf("expected review actor reviewer, got %q", transition.NextInst.ActorID)
	}
}

func TestWorkflowHumanReviewWithoutActorRoleUsesSingleHumanBinding(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-human-fallback",
		Name:        "Human Fallback Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "draft",
		Steps: []entity.WorkflowStep{
			{
				ID:           "draft",
				Type:         "agent_task",
				Title:        "Draft",
				ActorRole:    "developer",
				OutputFields: []entity.WorkflowField{{Name: "needs_review", Description: "yes or no."}},
			},
			{
				ID:    "technical_design_review",
				Type:  "human_review",
				Title: "Technical Design Review",
			},
		},
		Edges:     []entity.WorkflowEdge{{ID: "e1", From: "draft", To: "technical_design_review", Condition: &entity.WorkflowEdgeCondition{Field: "needs_review", Operator: "eq", Value: "yes"}}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	_, steps, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"developer":  {Type: "agent", ID: "dev"},
		"maintainer": {Type: "human", ID: "admin"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	review, ok := workflowStepInstanceByIDForTest(steps, "technical_design_review")
	if !ok {
		t.Fatal("missing review step instance")
	}
	if review.ActorType != "human" || review.ActorID != "admin" {
		t.Fatalf("expected review fallback actor admin, got type=%q id=%q", review.ActorType, review.ActorID)
	}

	transition, err := store.CompleteAndAdvance("project", "task-1", "draft done", "", map[string]string{"needs_review": "yes"}, "completed")
	if err != nil {
		t.Fatalf("complete and advance: %v", err)
	}
	if transition.NextInst == nil {
		t.Fatal("expected next instance")
	}
	if transition.NextInst.ActorType != "human" || transition.NextInst.ActorID != "admin" {
		t.Fatalf("expected next review actor admin, got type=%q id=%q", transition.NextInst.ActorType, transition.NextInst.ActorID)
	}
}

func TestCompleteAndAdvanceErrorsWhenOutgoingEdgesDoNotMatch(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-route-mismatch",
		Name:        "Route Mismatch Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "qa",
		Steps: []entity.WorkflowStep{
			{
				ID:           "qa",
				Type:         "agent_task",
				Title:        "QA Review",
				ActorRole:    "qa",
				OutputFields: []entity.WorkflowField{{Name: "review_decision", Description: "approve or request_changes."}},
			},
			{
				ID:        "human",
				Type:      "human_review",
				Title:     "Human Merge",
				ActorRole: "maintainer",
			},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "approve", From: "qa", To: "human", Condition: &entity.WorkflowEdgeCondition{Field: "review_decision", Operator: "eq", Value: "approve"}},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	if _, _, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"qa":         {Type: "agent", ID: "qa-agent"},
		"maintainer": {Type: "human", ID: "owner"},
	}); err != nil {
		t.Fatalf("start run: %v", err)
	}

	_, err = store.CompleteAndAdvance("project", "task-1", "review done", "", map[string]string{"review_decision": "comment"}, "completed")
	if err == nil {
		t.Fatal("expected route mismatch error")
	}
	if !strings.Contains(err.Error(), "did not match any outgoing route") {
		t.Fatalf("unexpected error: %v", err)
	}
	run, found, err := store.RunForTask("project", "task-1")
	if err != nil || !found {
		t.Fatalf("load run: found=%v err=%v", found, err)
	}
	if run.Status != "active" || run.ActiveStepID != "qa" {
		t.Fatalf("expected run to stay on qa, got status=%q active=%q", run.Status, run.ActiveStepID)
	}
}

func TestWorkflowRunUsesDefinitionSnapshotAfterDefinitionChanges(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-snapshot",
		Name:        "Snapshot Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "draft",
		Steps: []entity.WorkflowStep{
			{
				ID:           "draft",
				Type:         "agent_task",
				Title:        "Draft",
				ActorRole:    "writer",
				OutputFields: []entity.WorkflowField{{Name: "decision", Description: "next route"}},
			},
			{ID: "review", Type: "human_review", Title: "Review", ActorRole: "reviewer"},
		},
		Edges:     []entity.WorkflowEdge{{ID: "approve", From: "draft", To: "review", Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "approve"}}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	run, _, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"writer":   {Type: "agent", ID: "writer-agent"},
		"reviewer": {Type: "human", ID: "owner"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.DefinitionSnapshot == nil {
		t.Fatal("expected workflow run to store a definition snapshot")
	}

	def.Edges[0].Condition.Value = "renamed-after-task-start"
	def.Steps[1].Title = "Edited Review"
	def.Version = 2
	def.UpdatedAt = now.Add(time.Minute)
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("update definition: %v", err)
	}

	transition, err := store.CompleteAndAdvance("project", "task-1", "draft done", "", map[string]string{"decision": "approve"}, "completed")
	if err != nil {
		t.Fatalf("complete with original snapshot route: %v", err)
	}
	if transition.Next == nil || transition.Next.ID != "review" {
		t.Fatalf("expected transition to original review step, got %#v", transition.Next)
	}
	if transition.Next.Title != "Review" {
		t.Fatalf("expected snapshot step title, got %q", transition.Next.Title)
	}
}

func TestWorkflowRunAnnotatesCurrentAssigneeMembership(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := controlDB.UpsertAgentWorker(db.AgentWorker{
		ID:          "aw-dev",
		WorkspaceID: "workspace-1",
		Name:        "dev",
		DisplayName: "Dev",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if err := controlDB.UpsertProjectMembership(db.ProjectMembership{
		ID:          "pm-dev",
		WorkspaceID: "workspace-1",
		ProjectID:   "project",
		MemberType:  "agent_worker",
		MemberID:    "aw-dev",
		Role:        "developer",
		Title:       "developer",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("membership: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	def := &entity.WorkflowDefinition{
		ID:          "wf-assignee",
		Name:        "Assignee Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "draft",
		Steps: []entity.WorkflowStep{
			{ID: "draft", Type: "agent_task", Title: "Draft", ActorRole: "developer", OutputFields: []entity.WorkflowField{{Name: "decision", Description: "Decision."}}},
			{ID: "review", Type: "human_review", Title: "Review", ActorRole: "owner"},
		},
		Edges:     []entity.WorkflowEdge{{ID: "to-review", From: "draft", To: "review", Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "approve"}}},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	run, _, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"developer": {Type: "agent", ID: "developer"},
		"owner":     {Type: "human", ID: "owner"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.CurrentAssigneeType != "agent_worker" || run.CurrentAssigneeID != "aw-dev" || run.CurrentAssigneeMembershipID != "pm-dev" {
		t.Fatalf("unexpected current assignee on start: %#v", run)
	}
	transition, err := store.CompleteAndAdvance("project", "task-1", "done", "", map[string]string{"decision": "approve"}, "completed")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if transition.Run.ActiveStepID != "review" || transition.Run.CurrentAssigneeType != "user" || transition.Run.CurrentAssigneeID != "owner" || transition.Run.CurrentAssigneeMembershipID != "" {
		t.Fatalf("unexpected current assignee after advance: %#v", transition.Run)
	}
}

func workflowStepInstanceByIDForTest(steps []entity.WorkflowStepInstance, stepID string) (entity.WorkflowStepInstance, bool) {
	for _, step := range steps {
		if step.StepID == stepID {
			return step, true
		}
	}
	return entity.WorkflowStepInstance{}, false
}

func TestCompleteBranchAndMaybeAdvanceWaitsForAllBranches(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-parallel",
		Name:        "Parallel Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{
			{
				ID:           "start",
				Type:         "agent_task",
				Title:        "Start",
				ActorRole:    "pm",
				OutputFields: []entity.WorkflowField{{Name: "spec_doc_id", Description: "Spec docID."}},
				Position:     entity.WorkflowPosition{X: 0, Y: 0},
			},
			{
				ID:       "parallel",
				Type:     "parallel_stage",
				Title:    "Parallel Stage",
				Position: entity.WorkflowPosition{X: 240, Y: 0},
				Branches: []entity.WorkflowBranch{
					{
						ID:           "frontend",
						Title:        "Frontend Spec",
						ActorRole:    "frontend",
						InputFields:  []entity.WorkflowField{{Name: "spec_doc_id", Description: "Spec docID."}},
						OutputFields: []entity.WorkflowField{{Name: "frontend_doc_id", Description: "Frontend docID."}},
					},
					{
						ID:           "backend",
						Title:        "Backend Spec",
						ActorRole:    "backend",
						InputFields:  []entity.WorkflowField{{Name: "spec_doc_id", Description: "Spec docID."}},
						OutputFields: []entity.WorkflowField{{Name: "backend_doc_id", Description: "Backend docID."}},
					},
				},
			},
			{
				ID:        "review",
				Type:      "human_review",
				Title:     "Review",
				ActorRole: "reviewer",
				Position:  entity.WorkflowPosition{X: 480, Y: 0},
			},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "e1", From: "start", To: "parallel"},
			{ID: "e2", From: "parallel", To: "review"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	bindings := map[string]entity.WorkflowActorBinding{
		"pm":       {Type: "agent", ID: "pm"},
		"frontend": {Type: "agent", ID: "frontend"},
		"backend":  {Type: "agent", ID: "backend"},
		"reviewer": {Type: "human", ID: "owner"},
	}
	run, _, err := store.StartRun("project", "task-1", def.ID, bindings)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	transition, err := store.CompleteAndAdvance("project", "task-1", "spec ready", "", map[string]string{"spec_doc_id": "doc-20260730-abc123"}, "completed")
	if err != nil {
		t.Fatalf("complete start: %v", err)
	}
	if transition.Next == nil || transition.Next.ID != "parallel" {
		t.Fatalf("expected transition to parallel stage, got %#v", transition.Next)
	}
	parent := transition.NextInst
	for _, branch := range transition.Next.Branches {
		inst := &entity.WorkflowBranchInstance{
			RunID:       run.ID,
			StepID:      transition.Next.ID,
			BranchID:    branch.ID,
			Status:      "running",
			ActorType:   "agent",
			ActorID:     bindings[branch.ActorRole].ID,
			ChildTaskID: entity.NewTaskID(),
			StartedAt:   now,
			UpdatedAt:   now,
			InputValues: buildBranchInputValues(*parent, branch),
		}
		if err := store.SaveBranchInstance(inst); err != nil {
			t.Fatalf("save branch instance: %v", err)
		}
	}
	first, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", run.ID, "parallel", "frontend", "frontend done", map[string]string{"frontend_doc_id": "doc-20260730-front1"}, "completed")
	if err != nil {
		t.Fatalf("complete first branch: %v", err)
	}
	if first.AllDone {
		t.Fatal("expected first branch completion to wait for the remaining branch")
	}
	second, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", run.ID, "parallel", "backend", "backend done", map[string]string{"backend_doc_id": "doc-20260730-back12"}, "completed")
	if err != nil {
		t.Fatalf("complete second branch: %v", err)
	}
	if !second.AllDone {
		t.Fatal("expected all branches done after second completion")
	}
	if second.Transition.Next == nil || second.Transition.Next.ID != "review" {
		t.Fatalf("expected transition to review, got %#v", second.Transition.Next)
	}
	if second.Transition.Run.ActiveStepID != "review" {
		t.Fatalf("expected active step review, got %q", second.Transition.Run.ActiveStepID)
	}
}

func TestCompleteBranchAndMaybeAdvanceAnyJoinSkipsRemainingBranches(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-parallel-any",
		Name:        "Parallel Any Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "parallel",
		Steps: []entity.WorkflowStep{
			{
				ID:         "parallel",
				Type:       "parallel_stage",
				Title:      "Parallel Stage",
				JoinPolicy: "any",
				Position:   entity.WorkflowPosition{X: 0, Y: 0},
				Branches: []entity.WorkflowBranch{
					{ID: "path_a", Title: "Path A", ActorRole: "agent_a", OutputFields: []entity.WorkflowField{{Name: "path_a_doc_id", Description: "docID"}}},
					{ID: "path_b", Title: "Path B", ActorRole: "agent_b", OutputFields: []entity.WorkflowField{{Name: "path_b_doc_id", Description: "docID"}}},
				},
			},
			{ID: "done", Type: "human_review", Title: "Done", ActorRole: "reviewer", Position: entity.WorkflowPosition{X: 240, Y: 0}},
		},
		Edges: []entity.WorkflowEdge{{ID: "e1", From: "parallel", To: "done"}},
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	run, _, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"agent_a":  {Type: "agent", ID: "agent-a"},
		"agent_b":  {Type: "agent", ID: "agent-b"},
		"reviewer": {Type: "human", ID: "owner"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	for _, branch := range def.Steps[0].Branches {
		if err := store.SaveBranchInstance(&entity.WorkflowBranchInstance{
			RunID:       run.ID,
			StepID:      "parallel",
			BranchID:    branch.ID,
			Status:      "running",
			ChildTaskID: entity.NewTaskID(),
			StartedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("save branch instance: %v", err)
		}
	}
	result, err := store.CompleteBranchAndMaybeAdvance("project", "task-1", run.ID, "parallel", "path_a", "path a done", map[string]string{"path_a_doc_id": "doc-20260730-aa11"}, "completed")
	if err != nil {
		t.Fatalf("complete branch: %v", err)
	}
	if !result.AllDone {
		t.Fatal("expected any join to advance after first completed branch")
	}
	branches, err := store.BranchInstancesForStep(run.ID, "parallel")
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	statuses := map[string]string{}
	for _, branch := range branches {
		statuses[branch.BranchID] = branch.Status
	}
	if statuses["path_b"] != "skipped" {
		t.Fatalf("expected remaining branch to be skipped, got %q", statuses["path_b"])
	}
}

func TestCancelRunForTaskCancelsActiveRunAndStep(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "workspace-1", Name: "Workspace", Slug: "workspace", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	store := NewStore(controlDB, "workspace-1")
	now := time.Now().UTC()
	def := &entity.WorkflowDefinition{
		ID:          "wf-cancel",
		Name:        "Cancel Test",
		Version:     1,
		Scope:       "workspace",
		StartStepID: "draft",
		Steps: []entity.WorkflowStep{{
			ID:        "draft",
			Type:      "agent_task",
			Title:     "Draft",
			ActorRole: "worker",
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	run, _, err := store.StartRun("project", "task-1", def.ID, map[string]entity.WorkflowActorBinding{
		"worker": {Type: "agent", ID: "agent-a"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	cancelled, changed, err := store.CancelRunForTask("project", "task-1", "task cancelled")
	if err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if !changed {
		t.Fatal("expected run to change")
	}
	if cancelled.Status != "cancelled" || cancelled.ActiveStepID != "" {
		t.Fatalf("unexpected cancelled run: %+v", cancelled)
	}
	steps, err := store.ListStepInstances(run.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != "cancelled" || steps[0].Summary != "task cancelled" {
		t.Fatalf("unexpected step instances: %+v", steps)
	}
}

func TestUnifiedDeliveryPipelineTemplateStructure(t *testing.T) {
	tmpl, ok := Template("unified-delivery-pipeline", "zh-CN")
	if !ok {
		t.Fatal("template not registered")
	}
	if tmpl.Name != "统一交付流水线" {
		t.Fatalf("zh-CN localization missing: %q", tmpl.Name)
	}

	stepIDs := map[string]bool{}
	hasHuman := 0
	for _, s := range tmpl.Steps {
		if stepIDs[s.ID] {
			t.Fatalf("duplicate step id %q", s.ID)
		}
		stepIDs[s.ID] = true
		if s.Type == "human_review" {
			hasHuman++
		}
	}
	for _, want := range []string{"clarify", "clarify_review", "implement", "agent_self_review", "code_review", "changelog", "create_pr", "pr_review", "merge_sync", "qa", "qa_signoff", "release"} {
		if !stepIDs[want] {
			t.Fatalf("missing step %q", want)
		}
	}
	if hasHuman != 4 {
		t.Fatalf("expected 4 human_review steps (scope, code, pr, qa), got %d", hasHuman)
	}

	// Agent self-review recycle loop: rework edge back to implement carries
	// the round counter, and an escalation edge reaches human review.
	edges := map[string]entity.WorkflowEdge{}
	for _, e := range tmpl.Edges {
		edges[e.ID] = e
	}
	rework, ok := edges["e-self-rework"]
	if !ok || rework.To != "implement" {
		t.Fatalf("self-review rework edge missing or wrong target: %+v", rework)
	}
	if rework.InputMapping["review_rounds"] == "" {
		t.Fatal("rework edge must carry review_rounds back to implement")
	}
	esc, ok := edges["e-self-escalate"]
	if !ok || esc.To != "code_review" || esc.Condition == nil || esc.Condition.Value != "escalate" {
		t.Fatalf("escalation edge missing or wrong: %+v", esc)
	}
	// PR rework must preserve the PR reference and comments.
	prRework := edges["e-pr-rework"]
	if prRework.InputMapping["previous_pr"] != "$input.pr_url" || prRework.InputMapping["review_comments"] != "$output.comments" {
		t.Fatalf("pr rework edge must carry previous_pr and comments: %+v", prRework.InputMapping)
	}
	// Terminal release must receive the QA-approved candidate.
	rel := edges["e-qa-approved"]
	if rel.InputMapping["release_candidate"] != "$output.release_candidate" {
		t.Fatalf("qa approval must pass release_candidate: %+v", rel.InputMapping)
	}
}

func TestHotfixDeployPipelineTemplateStructure(t *testing.T) {
	tmpl, ok := Template("hotfix-deploy-pipeline", "zh-CN")
	if !ok {
		t.Fatal("template not registered")
	}
	if tmpl.Name != "紧急修复与部署" {
		t.Fatalf("zh-CN localization missing: %q", tmpl.Name)
	}

	stepIDs := map[string]bool{}
	human := 0
	for _, s := range tmpl.Steps {
		if stepIDs[s.ID] {
			t.Fatalf("duplicate step id %q", s.ID)
		}
		stepIDs[s.ID] = true
		if s.Type == "human_review" {
			human++
		}
	}
	for _, want := range []string{"triage", "hotfix_review", "implement_fix", "fix_review", "verify_and_tag", "confirm"} {
		if !stepIDs[want] {
			t.Fatalf("missing step %q", want)
		}
	}
	if human != 3 {
		t.Fatalf("expected 3 human_review steps (plan review, fix effect review, deploy confirm), got %d", human)
	}
	// The plan gate must be approvable without the reviewer authoring a
	// plan: approved_fix is optional and backfilled from the triage
	// diagnosis (same auto-reference contract as the unified pipeline).
	for _, s := range tmpl.Steps {
		if s.ID != "hotfix_review" {
			continue
		}
		for _, f := range s.OutputFields {
			if f.Name == "approved_fix" && !f.Optional {
				t.Fatal("hotfix_review.approved_fix must be optional (auto-referenced from diagnosis)")
			}
		}
	}

	edges := map[string]entity.WorkflowEdge{}
	for _, e := range tmpl.Edges {
		edges[e.ID] = e
	}
	// Deterministic baseline: the approval edge must hand implement_fix the
	// frozen base_commit from triage diagnosis, never a moving branch ref.
	approve, ok := edges["e-fix-approved"]
	if !ok || approve.To != "implement_fix" || approve.Condition == nil || approve.Condition.Value != "approve" {
		t.Fatalf("approve edge missing or wrong: %+v", approve)
	}
	if approve.InputMapping["base_commit"] != "$input.diagnosis.base_commit" {
		t.Fatalf("approve edge must carry frozen base_commit from diagnosis: %+v", approve.InputMapping)
	}
	// Plan-rejection recycles into triage with the prior diagnosis preserved.
	reject := edges["e-fix-rejected"]
	if reject.To != "triage" || reject.Condition == nil || reject.Condition.Value != "request_changes" {
		t.Fatalf("plan-rejection edge missing or wrong: %+v", reject)
	}
	if reject.InputMapping["previous_diagnosis"] != "$input.diagnosis" {
		t.Fatalf("plan-rejection edge must preserve prior diagnosis: %+v", reject.InputMapping)
	}
	// Deploy confirmation is dual-rework: fix-quality issues return to
	// implement_fix (the earliest code-producing step, matching the unified
	// template's rework convention); wrong-diagnosis issues escalate to triage.
	fixRework := edges["e-confirm-done"]
	if fixRework.To != "implement_fix" || fixRework.Condition == nil || fixRework.Condition.Value != "request_changes" {
		t.Fatalf("fix-rework edge missing or wrong: %+v", fixRework)
	}
	retriage := edges["e-confirm-retriage"]
	if retriage.To != "triage" || retriage.Condition == nil || retriage.Condition.Value != "escalate" {
		t.Fatalf("retriage edge missing or wrong: %+v", retriage)
	}
	// Fix-effect gate sits between coding and tagging: approval must carry
	// fix_artifact into verify_and_tag (a bare edge copies only the SOURCE
	// step's outputs, and fix_review produces no fix_artifact); rejection
	// returns to implement_fix with the artifact preserved for rework.
	reviewTag := edges["e-review-tag"]
	if reviewTag.To != "verify_and_tag" || reviewTag.Condition == nil || reviewTag.Condition.Value != "approve" {
		t.Fatalf("fix-review approve edge missing or wrong: %+v", reviewTag)
	}
	if reviewTag.InputMapping["fix_artifact"] != "$input.fix_artifact" {
		t.Fatalf("fix-review approval must pass fix_artifact through to verify_and_tag: %+v", reviewTag.InputMapping)
	}
	reviewRework := edges["e-review-rework"]
	if reviewRework.To != "implement_fix" || reviewRework.Condition == nil || reviewRework.Condition.Value != "request_changes" {
		t.Fatalf("fix-review rework edge missing or wrong: %+v", reviewRework)
	}
	// Fix output must include a preview field so the human gate can inspect
	// the effect before anything is tagged.
	for _, s := range tmpl.Steps {
		if s.ID != "implement_fix" {
			continue
		}
		hasPreview := false
		for _, f := range s.OutputFields {
			if f.Name == "preview" {
				hasPreview = true
			}
		}
		if !hasPreview {
			t.Fatal("implement_fix must expose a preview output for the fix-effect gate")
		}
	}
	// Gate actors: product-owner owns both human gates, distinct agents for
	// triage vs fix so the queues do not serialize on one identity.
	if tmpl.Steps[0].ActorRole != "triage-agent" {
		t.Fatalf("triage must run on triage-agent, got %q", tmpl.Steps[0].ActorRole)
	}
	for _, s := range tmpl.Steps {
		if s.Type == "human_review" && s.ActorRole != "product-owner" {
			t.Fatalf("human gate %q must be product-owner, got %q", s.ID, s.ActorRole)
		}
	}
}
