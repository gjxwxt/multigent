package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// TestBackfillRestoresPlanConfigOnDeclaredStages: a stored parallel_stage that
// declares plan inputs but lost planMaterialization=frozen (canvas edit,
// pre-25445bcf instantiation) gets the config restored by the startup
// backfill; a pure legacy static stage is untouched; already-fixed
// definitions are idempotent no-ops.
func TestBackfillRestoresPlanConfigOnDeclaredStages(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	declared := &entity.WorkflowDefinition{
		ID: "wf-backfill-declared", Name: "Declared stage", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{
			{ID: "parallel", Type: "parallel_stage", Title: "Declared",
				InputFields: []entity.WorkflowField{{Name: "delivery_plan"}},
				Branches:    []entity.WorkflowBranch{{ID: "ws_a", Title: "WS-1"}}},
		},
	}
	legacy := &entity.WorkflowDefinition{
		ID: "wf-backfill-legacy", Name: "Legacy static", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{
			{ID: "parallel", Type: "parallel_stage", Title: "Legacy",
				Branches: []entity.WorkflowBranch{{ID: "ws_a", Title: "WS-1"}}},
		},
	}
	fixed := &entity.WorkflowDefinition{
		ID: "wf-backfill-fixed", Name: "Already frozen", Version: 1, Scope: "workspace", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{
			{ID: "parallel", Type: "parallel_stage", Title: "Frozen",
				Config:   map[string]string{planMaterializationConfigKey: "frozen"},
				Branches: []entity.WorkflowBranch{{ID: "ws_a", Title: "WS-1"}}},
		},
	}
	for _, def := range []*entity.WorkflowDefinition{declared, legacy, fixed} {
		if err := wfStore.SaveDefinition(def); err != nil {
			t.Fatal(err)
		}
	}

	s.backfillParallelStagePlanConfigs(workspaceID, wfStore)

	for _, tc := range []struct {
		defID string
		want  bool
	}{
		{"wf-backfill-declared", true},
		{"wf-backfill-legacy", false},
		{"wf-backfill-fixed", true},
	} {
		def, found, err := wfStore.Definition(tc.defID)
		if err != nil || !found {
			t.Fatalf("%s lookup: found=%v err=%v", tc.defID, found, err)
		}
		got := def.Steps[0].Config[planMaterializationConfigKey] == "frozen"
		if got != tc.want {
			t.Fatalf("%s: planMaterialization=frozen want=%v got=%v (config=%v)", tc.defID, tc.want, got, def.Steps[0].Config)
		}
	}

	// Idempotency: a second pass must not error and must not change anything.
	s.backfillParallelStagePlanConfigs(workspaceID, wfStore)
	def, _, err := wfStore.Definition("wf-backfill-fixed")
	if err != nil {
		t.Fatal(err)
	}
	if def.Steps[0].Config[planMaterializationConfigKey] != "frozen" {
		t.Fatal("second pass must keep the frozen config")
	}
}

// TestListAllDefinitionsRawSeesProjectScopeDefinitions: ListDefinitions
// filters to the workspace gallery; the backfill sweep must also see
// project-scoped definitions (SaveDefinition defaults Scope to "project"),
// because a hidden definition is still selectable by an active run.
func TestListAllDefinitionsRawSeesProjectScopeDefinitions(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	proj := &entity.WorkflowDefinition{
		ID: "wf-proj-scope", Name: "Project scoped", StartStepID: "parallel",
		Steps: []entity.WorkflowStep{
			{ID: "parallel", Type: "parallel_stage", Title: "P",
				InputFields: []entity.WorkflowField{{Name: "batch_plan"}}},
		},
	}
	if err := wfStore.SaveDefinition(proj); err != nil {
		t.Fatal(err)
	}

	gallery, err := wfStore.ListDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range gallery {
		if def.ID == "wf-proj-scope" {
			t.Fatal("empty-scope definition must NOT appear in the gallery list")
		}
	}
	raw, err := wfStore.ListAllDefinitionsRaw()
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, def := range raw {
		if def.ID == "wf-proj-scope" {
			seen = true
			if len(def.Steps) != 1 || def.Steps[0].ID != "parallel" {
				t.Fatalf("raw list must carry the full definition, got %+v", def)
			}
		}
	}
	if !seen {
		t.Fatal("raw list must include project-scoped definitions")
	}
	_ = workflowstore.PlanWPStateDeferred
}
