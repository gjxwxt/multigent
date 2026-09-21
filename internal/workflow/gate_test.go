package workflow

import (
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Characterization tests for the platform-gate registry (gate.go). They lock
// the CURRENT resolution behavior — explicit marker, legacy config flag, and
// legacy ID/title fallbacks — so the registry stays behavior-identical to the
// scattered matchers it replaced (reduction plan Task 0.1). The legacy
// fallbacks are deletion-gated on the Task 0.5 deprecation policy, not on
// convenience: when they go, these cases flip to expecting no gate.

func TestGateForStepExplicitMarker(t *testing.T) {
	cases := []struct {
		kind string
		want PlatformGate
	}{
		{GateKindCIReady, PlatformGate{Kind: GateKindCIReady}},
		{GateKindQASignoff, PlatformGate{Kind: GateKindQASignoff}},
		{GateKindDesignGate, PlatformGate{Kind: GateKindDesignGate}},
	}
	for _, tc := range cases {
		step := entity.WorkflowStep{ID: "anything_at_all", Config: map[string]string{GateConfigKey: tc.kind}}
		got, ok := GateForStep(step)
		if !ok || got != tc.want {
			t.Fatalf("GateForStep(marker=%s) = %+v,%v want %+v,true", tc.kind, got, ok, tc.want)
		}
	}
}

func TestGateForStepMarkerCaseSensitivity(t *testing.T) {
	// Original consumer compared the marker value case-sensitively; the
	// registry must not widen it (reviewer finding P1: CI_READY would
	// previously NOT match, and must still not match).
	step := entity.WorkflowStep{ID: "x", Config: map[string]string{GateConfigKey: "CI_READY"}}
	if _, ok := GateForStep(step); ok {
		t.Fatalf("uppercase marker must not resolve (exact-match contract)")
	}
	padded := entity.WorkflowStep{ID: "x", Config: map[string]string{GateConfigKey: " ci_ready "}}
	got, ok := GateForStep(padded)
	if !ok || got.Kind != GateKindCIReady {
		t.Fatalf("trimmed marker should resolve, got %+v,%v", got, ok)
	}
}

func TestGateForStepUnknownMarkerYieldsNoGate(t *testing.T) {
	step := entity.WorkflowStep{ID: "x", Config: map[string]string{GateConfigKey: "mystery_gate"}}
	if _, ok := GateForStep(step); ok {
		t.Fatalf("unknown marker must not resolve to any gate")
	}
}

func TestGateForStepLegacyFallbacks(t *testing.T) {
	cases := []struct {
		name string
		step entity.WorkflowStep
		want string
	}{
		{name: "legacy ci_ready id", step: entity.WorkflowStep{ID: "ci_ready"}, want: GateKindCIReady},
		{name: "legacy ci_ready_gate id", step: entity.WorkflowStep{ID: "ci_ready_gate"}, want: GateKindCIReady},
		{name: "legacy qa_signoff exact", step: entity.WorkflowStep{ID: "qa_signoff"}, want: GateKindQASignoff},
		{name: "legacy qa_signoff contains", step: entity.WorkflowStep{ID: "qa_signoff_v2"}, want: GateKindQASignoff},
		{name: "legacy design_review contains", step: entity.WorkflowStep{ID: "greenfield_design_review"}, want: GateKindDesignGate},
		{name: "legacy designGate flag", step: entity.WorkflowStep{ID: "anything", Config: map[string]string{"designGate": "true"}}, want: GateKindDesignGate},
		{name: "legacy designGate flag case-insensitive", step: entity.WorkflowStep{ID: "x", Config: map[string]string{"designGate": "TRUE"}}, want: GateKindDesignGate},
		{name: "marker wins over conflicting legacy id", step: entity.WorkflowStep{ID: "qa_signoff", Config: map[string]string{GateConfigKey: "ci_ready"}}, want: GateKindCIReady},
		{name: "legacy ci_ready id is case-sensitive exact", step: entity.WorkflowStep{ID: "CI_Ready"}, want: ""},
		{name: "legacy qa id lowercases like the original handler matcher", step: entity.WorkflowStep{ID: "QA_Signoff"}, want: GateKindQASignoff},
		{name: "plain qa id is not a gate", step: entity.WorkflowStep{ID: "qa"}, want: ""},
		{name: "renamed design id without marker is not a gate", step: entity.WorkflowStep{ID: "design_signoff"}, want: ""},
		{name: "no config no id", step: entity.WorkflowStep{}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := GateForStep(tc.step)
			if tc.want == "" {
				if ok {
					t.Fatalf("expected no gate, got %+v", got)
				}
				return
			}
			if !ok || got.Kind != tc.want {
				t.Fatalf("GateForStep(%+v) = %+v,%v want kind %s", tc.step, got, ok, tc.want)
			}
		})
	}
}

func TestIsPullRequestReviewStep(t *testing.T) {
	cases := []struct {
		name string
		step entity.WorkflowStep
		want bool
	}{
		{name: "exact id pr_review", step: entity.WorkflowStep{ID: "pr_review"}, want: true},
		{name: "exact id mr_review", step: entity.WorkflowStep{ID: "mr_review"}, want: true},
		{name: "exact id merge_and_sync", step: entity.WorkflowStep{ID: "merge_and_sync"}, want: true},
		{name: "exact id push_to_gitlab", step: entity.WorkflowStep{ID: "push_to_gitlab"}, want: true},
		{name: "id contains pr_review suffix", step: entity.WorkflowStep{ID: "auto_pr_review_v2"}, want: true},
		{name: "title substring pull request", step: entity.WorkflowStep{ID: "totally_unrelated", Title: "Open a Pull Request for review"}, want: true},
		{name: "title substring merge request", step: entity.WorkflowStep{ID: "totally_unrelated", Title: "创建 Merge Request"}, want: true},
		{name: "title case insensitive", step: entity.WorkflowStep{ID: "x", Title: "PULL REQUEST gate"}, want: true},
		// The known quirk: id still matches after a title rename...
		{name: "renamed title still matches via id", step: entity.WorkflowStep{ID: "pr_review", Title: "代码合入与远端同步"}, want: true},
		// ...but a fully renamed step escapes detection. Locked here so any
		// future tightening of this matcher is a deliberate, visible change.
		{name: "unrelated id and renamed title", step: entity.WorkflowStep{ID: "deliver", Title: "代码合入与远端同步"}, want: false},
		{name: "create_pr id is NOT a review step", step: entity.WorkflowStep{ID: "create_pr"}, want: false},
		{name: "empty step", step: entity.WorkflowStep{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPullRequestReviewStep(tc.step); got != tc.want {
				t.Fatalf("IsPullRequestReviewStep(%+v) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}

func TestGateHelpers(t *testing.T) {
	ciReady := entity.WorkflowStep{ID: "ci_ready_gate"}
	if !IsCIReadyGateStep(ciReady) {
		t.Fatalf("IsCIReadyGateStep legacy id should match")
	}
	if IsQASignoffStep(ciReady) || IsDesignGateStep(ciReady) {
		t.Fatalf("ci_ready step must not match other gates")
	}
	qa := entity.WorkflowStep{ID: "qa_signoff"}
	if !IsQASignoffStep(qa) {
		t.Fatalf("IsQASignoffStep legacy id should match")
	}
	design := entity.WorkflowStep{ID: "design_review"}
	if !IsDesignGateStep(design) {
		t.Fatalf("IsDesignGateStep legacy id should match")
	}
	// qa_signoff must not be caught by the design_review substring rule.
	if IsDesignGateStep(qa) {
		t.Fatalf("qa_signoff must not match design gate")
	}
}

func TestDeliveryForStepAndStrictMatcher(t *testing.T) {
	marked := entity.WorkflowStep{ID: "whatever", Config: map[string]string{StepConfigPlatformDelivery: DeliveryKindPullRequestReview}}
	if !IsPullRequestReviewStepStrict(marked) {
		t.Fatalf("explicit marker should match strictly")
	}
	if !PullRequestReviewStepMatches(marked, DefinitionVersionV2) {
		t.Fatalf("explicit marker must match even under V2 definitions")
	}
	otherKind := entity.WorkflowStep{ID: "x", Config: map[string]string{StepConfigPlatformDelivery: "deploy"}}
	if IsPullRequestReviewStepStrict(otherKind) {
		t.Fatalf("non-pr_review delivery kind must not match")
	}
	// Legacy shape hit, no marker: matches pre-V2 definitions…
	legacy := entity.WorkflowStep{ID: "merge_and_sync"}
	if !PullRequestReviewStepMatches(legacy, 1) {
		t.Fatalf("legacy id shape must match under v1 definitions")
	}
	// …but NOT under V2 (hard deprecation line — the whole point).
	if PullRequestReviewStepMatches(legacy, DefinitionVersionV2) {
		t.Fatalf("legacy id shape must NOT match under V2 definitions")
	}
	titleOnly := entity.WorkflowStep{ID: "ship_it", Title: "Create or Update Pull Request"}
	if PullRequestReviewStepMatches(titleOnly, DefinitionVersionV2) {
		t.Fatalf("legacy title guess must NOT match under V2 definitions")
	}
	blank := entity.WorkflowStep{ID: "plain", Title: "Do Work"}
	if PullRequestReviewStepMatches(blank, 1) || PullRequestReviewStepMatches(blank, DefinitionVersionV2) {
		t.Fatalf("plain step must never match")
	}
}

func TestBuiltinTemplatesDeclareDeliveryMarkers(t *testing.T) {
	// Task 0.5 guard: every builtin template step that the legacy matcher
	// hits MUST carry the explicit platform_delivery marker, so marker-first
	// resolution and legacy fallback stay behaviorally identical for
	// instantiated definitions. (Found via enumeration: software-delivery v1
	// def + agentic-software-delivery + unified templates; the zh unified
	// merge_sync previously escaped the title guess entirely — exactly the
	// fragility this closes.)
	for _, locale := range []string{"en", "zh"} {
		for _, tmpl := range Templates(locale) {
			for _, step := range tmpl.Steps {
				if IsPullRequestReviewStep(step) && !IsPullRequestReviewStepStrict(step) {
					t.Errorf("template %s (%s) step %s relies on legacy shape matching without an explicit %s marker", tmpl.ID, locale, step.ID, StepConfigPlatformDelivery)
				}
			}
		}
	}
	if def, ok := seededSoftwareDeliveryV1Definition(t); ok {
		for _, step := range def.Steps {
			if IsPullRequestReviewStep(step) && !IsPullRequestReviewStepStrict(step) {
				t.Errorf("seeded software-delivery-v1 step %s relies on legacy matching without explicit marker", step.ID)
			}
		}
	}
}

// seededSoftwareDeliveryV1Definition seeds the builtin software-delivery-v1
// definition (the inline SeedDefaults variant) in a temp store.
func seededSoftwareDeliveryV1Definition(t *testing.T) (entity.WorkflowDefinition, bool) {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "ws-gate-markers", Name: "Workspace", Slug: "ws-gate-markers", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	store := NewStore(controlDB, "ws-gate-markers")
	if err := store.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	def, found, err := store.Definition("software-delivery-v1")
	if err != nil {
		t.Fatalf("definition lookup: %v", err)
	}
	return def, found
}
