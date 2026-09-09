package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestWorkflowActorBindingForStepPrefersStepIDAndFallsBackToRole(t *testing.T) {
	step := entity.WorkflowStep{ID: "code_review", ActorRole: "owner-engineer", Type: "human_review"}

	stepBinding, ok := workflowActorBindingForStep(map[string]entity.WorkflowActorBinding{
		"code_review":    {Type: "human", ID: "admin"},
		"owner-engineer": {Type: "agent", ID: "Mira"},
	}, step)
	if !ok || stepBinding.Type != "human" || stepBinding.ID != "admin" {
		t.Fatalf("expected step-specific human binding, got %#v (ok=%v)", stepBinding, ok)
	}

	roleBinding, ok := workflowActorBindingForStep(map[string]entity.WorkflowActorBinding{
		"owner-engineer": {Type: "agent", ID: "Mira"},
	}, step)
	if !ok || roleBinding.Type != "agent" || roleBinding.ID != "Mira" {
		t.Fatalf("expected legacy role binding fallback, got %#v (ok=%v)", roleBinding, ok)
	}
}

func TestWorkflowStartActorUsesStepSpecificBinding(t *testing.T) {
	def := entity.WorkflowDefinition{
		StartStepID: "requirement_review",
		Steps: []entity.WorkflowStep{{
			ID:        "requirement_review",
			Type:      "human_review",
			ActorRole: "product-owner",
		}},
	}
	step, inst, ok := workflowStartActor(def, map[string]entity.WorkflowActorBinding{
		"requirement_review": {Type: "human", ID: "alex"},
		"product-owner":      {Type: "human", ID: "admin"},
	})
	if !ok || step == nil || inst == nil {
		t.Fatal("expected workflow start actor")
	}
	if inst.ActorType != "human" || inst.ActorID != "alex" {
		t.Fatalf("expected step-specific reviewer alex, got type=%q id=%q", inst.ActorType, inst.ActorID)
	}
}
