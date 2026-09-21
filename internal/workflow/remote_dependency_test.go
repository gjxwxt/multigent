package workflow

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func templateByIDForTest(t *testing.T, id string) entity.WorkflowTemplate {
	t.Helper()
	for _, tmpl := range Templates("en") {
		if tmpl.ID == id {
			return tmpl
		}
	}
	t.Fatalf("template %q is not registered", id)
	return entity.WorkflowTemplate{}
}

// The marker is what the console renders and what the prompt contract keys off,
// so the set has to be explicit per template — including the templates that
// deliberately declare nothing, which is how a local-only loop stays honest.
func TestRemoteDependentStepsDeclaredPerTemplate(t *testing.T) {
	cases := map[string][]string{
		"unified-delivery-pipeline":    {"create_pr", "merge_sync", "release"},
		"greenfield-delivery-pipeline": {"pr_open_and_merge", "release"},
		"hotfix-deploy-pipeline":       {"verify_and_tag"},
		"tdd-review-loop":              nil,
	}
	for id, want := range cases {
		got := RequiresRemoteStepIDs(templateByIDForTest(t, id).Steps)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s remote-dependent steps = %v, want %v", id, got, want)
		}
	}
}

// A human gate never "fails because of a missing remote" — it waits for a
// person. Marking one would make the console blame the project's binding config
// for something only the reviewer can resolve.
func TestNoHumanReviewStepDeclaresRemoteDependency(t *testing.T) {
	for _, tmpl := range Templates("en") {
		for _, step := range tmpl.Steps {
			if step.Type == "human_review" && StepRequiresRemote(step) {
				t.Fatalf("%s/%s: human_review step declares a remote dependency", tmpl.ID, step.ID)
			}
		}
	}
}

// Declaration-only means a custom workflow that renames `create_pr` to `open_mr`
// is reported as *not declared*, never inferred as remote-dependent. That is the
// intended trade: the console then renders "未声明" and a human resolves it,
// instead of a heuristic silently guessing wrong in either direction.
func TestRenamedStepsAreNotGuessedAsRemoteDependent(t *testing.T) {
	renamed := []entity.WorkflowStep{
		{ID: "open_mr", Type: "agent_task"},
		{ID: "publish_tag", Type: "agent_task"},
		{ID: "create_pull_request", Type: "agent_task"},
	}
	if got := RequiresRemoteStepIDs(renamed); len(got) != 0 {
		t.Fatalf("renamed steps must not be inferred as remote-dependent, got %v", got)
	}
}

func TestStepRequiresRemoteAcceptsOnlyExplicitTrue(t *testing.T) {
	cases := map[string]struct {
		cfg  map[string]string
		want bool
	}{
		"explicit true":         {map[string]string{StepConfigRequiresRemote: "true"}, true},
		"padded and cased":      {map[string]string{StepConfigRequiresRemote: "  TRUE "}, true},
		"explicit false":        {map[string]string{StepConfigRequiresRemote: "false"}, false},
		"truthy-looking string": {map[string]string{StepConfigRequiresRemote: "yes"}, false},
		"absent":                {map[string]string{"color": "violet"}, false},
		"nil config":            {nil, false},
	}
	for name, tc := range cases {
		step := entity.WorkflowStep{ID: "create_pr", Config: tc.cfg}
		if got := StepRequiresRemote(step); got != tc.want {
			t.Fatalf("%s: StepRequiresRemote = %v, want %v", name, got, tc.want)
		}
	}
}

// Marking must survive instantiation: templates are read-only directory entries,
// and only the instantiated definition reaches a task.
func TestRequiresRemoteMarkerSurvivesInstantiation(t *testing.T) {
	def, ok := DefinitionFromTemplate("unified-delivery-pipeline", "zh-CN", "统一交付流水线")
	if !ok {
		t.Fatal("template unified-delivery-pipeline missing")
	}
	if got := strings.Join(RequiresRemoteStepIDs(def.Steps), ","); got != "create_pr,merge_sync,release" {
		t.Fatalf("instantiated definition lost the marker: %q", got)
	}
}

// Task 0.1: every built-in template must declare platform_gate explicitly on
// its gate steps — the registry resolves the marker first, and the legacy
// ID/title fallbacks only exist for pre-marker definitions. A template
// relying on the fallback is a regression.
func TestPlatformGateMarkersDeclaredOnBuiltinTemplates(t *testing.T) {
	cases := map[string]map[string]string{
		"unified-delivery-pipeline": {
			"ci_ready_gate": GateKindCIReady,
			"qa_signoff":    GateKindQASignoff,
		},
	}
	for tmplID, want := range cases {
		tmpl := templateByIDForTest(t, tmplID)
		for stepID, kind := range want {
			found := false
			for _, step := range tmpl.Steps {
				if step.ID != stepID {
					continue
				}
				found = true
				if got := step.Config[GateConfigKey]; got != kind {
					t.Fatalf("%s/%s marker = %q, want %q", tmplID, stepID, got, kind)
				}
				gate, ok := GateForStep(step)
				if !ok || gate.Kind != kind {
					t.Fatalf("%s/%s does not resolve to gate %s", tmplID, stepID, kind)
				}
			}
			if !found {
				t.Fatalf("%s has no step %s", tmplID, stepID)
			}
		}
	}

	// Greenfield declares the design gate via the helper in greenfield.go.
	green := templateByIDForTest(t, "greenfield-delivery-pipeline")
	for _, step := range green.Steps {
		if step.ID == "design_review" {
			if got := step.Config[GateConfigKey]; got != GateKindDesignGate {
				t.Fatalf("greenfield design_review marker = %q, want %q", got, GateKindDesignGate)
			}
			return
		}
	}
	t.Fatal("greenfield-delivery-pipeline has no design_review step")
}

// The project-initialization definition's ci_ready step also carries the marker.
func TestInitTemplateCIReadyMarker(t *testing.T) {
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "ws-gate", Name: "WS", Slug: "ws-gate", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	store := NewStore(controlDB, "ws-gate")
	if err := store.EnsureProjectInitializationDefinition(); err != nil {
		t.Fatalf("ensure init definition: %v", err)
	}
	def, found, err := store.Definition(ProjectInitializationWorkflowID)
	if err != nil || !found {
		t.Fatalf("init definition not persisted: %v (found=%v)", err, found)
	}
	for _, step := range def.Steps {
		if step.ID == "ci_ready" {
			if got := step.Config[GateConfigKey]; got != GateKindCIReady {
				t.Fatalf("init ci_ready marker = %q, want %q", got, GateKindCIReady)
			}
			return
		}
	}
	t.Fatal("init definition has no ci_ready step")
}
