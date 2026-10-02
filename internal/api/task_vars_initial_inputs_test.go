package api

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func greenfieldLikeDef() entity.WorkflowDefinition {
	return entity.WorkflowDefinition{
		ID:          "wf-test",
		StartStepID: "requirement_draft",
		Steps: []entity.WorkflowStep{
			{
				ID: "requirement_draft",
				InputFields: []entity.WorkflowField{
					{Name: "request"},
					{Name: "context"},
					{Name: "delivery_plan_draft", Optional: true},
				},
			},
		},
	}
}

func TestMergeTaskVarsIntoInitialInputsAcceptsDeclaredFields(t *testing.T) {
	def := greenfieldLikeDef()
	vars := map[string]string{
		"delivery_plan_draft":           `{"planId":"p1"}`,
		"MULTIGENT_WAKEUP_WORKTREE_DIR": "/etc", // reserved, must not flow
		"not_declared":                  "x",    // undeclared, must not flow
	}
	got := mergeTaskVarsIntoInitialInputs(nil, def, vars)
	if got["delivery_plan_draft"] != `{"planId":"p1"}` {
		t.Fatalf("declared var must pass through, got %v", got)
	}
	if _, ok := got["MULTIGENT_WAKEUP_WORKTREE_DIR"]; ok {
		t.Fatal("reserved var must be dropped")
	}
	if _, ok := got["not_declared"]; ok {
		t.Fatal("undeclared var must be dropped")
	}
}

func TestMergeTaskVarsPromptParsedWins(t *testing.T) {
	def := greenfieldLikeDef()
	initial := map[string]string{"delivery_plan_draft": `{"planId":"from-prompt"}`}
	got := mergeTaskVarsIntoInitialInputs(initial, def, map[string]string{
		"delivery_plan_draft": `{"planId":"from-vars"}`,
	})
	if !strings.Contains(got["delivery_plan_draft"], "from-prompt") {
		t.Fatalf("prompt-parsed value must win, got %v", got)
	}
}

func TestMergeTaskVarsNoStartStepIsNoop(t *testing.T) {
	def := entity.WorkflowDefinition{ID: "wf-x", StartStepID: "missing"}
	got := mergeTaskVarsIntoInitialInputs(nil, def, map[string]string{"delivery_plan_draft": "v"})
	if got != nil {
		t.Fatalf("no start step must stay nil, got %v", got)
	}
}
