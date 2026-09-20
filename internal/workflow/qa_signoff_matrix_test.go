package workflow

import (
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// The qa_signoff risk-coverage gate validates risk_coverage_matrix before an
// approval routes forward. The matrix must therefore have a legal path in every
// template that contains a qa_signoff step: QA's output contract declares it,
// the qa→qa_signoff edge maps it, and the sign-off input contract accepts it.
// A gate without a production path is the exact "silent no-op" shape the
// platform keeps tearing out.
func TestQASignoffTemplatesCarryRiskMatrixPath(t *testing.T) {
	for _, tmpl := range Templates("en") {
		hasSignoff := false
		var qaStep, signoffStep *entity.WorkflowStep
		for i := range tmpl.Steps {
			step := &tmpl.Steps[i]
			if step.ID == "qa" {
				qaStep = step
			}
			if strings.Contains(strings.ToLower(step.ID), "qa_signoff") {
				hasSignoff = true
				signoffStep = step
			}
		}
		if !hasSignoff {
			continue
		}
		if qaStep == nil || signoffStep == nil {
			t.Fatalf("%s: qa_signoff present but qa steps missing", tmpl.ID)
		}
		if !fieldDeclared(qaStep.OutputFields, "risk_coverage_matrix") {
			t.Fatalf("%s: qa step must declare risk_coverage_matrix as an output", tmpl.ID)
		}
		if !fieldDeclared(signoffStep.InputFields, "risk_coverage_matrix") {
			t.Fatalf("%s: qa_signoff step must declare risk_coverage_matrix as an input", tmpl.ID)
		}
		mapped := false
		for _, e := range tmpl.Edges {
			if e.From == "qa" && strings.Contains(strings.ToLower(e.To), "qa_signoff") && e.InputMapping["risk_coverage_matrix"] == "$output.risk_coverage_matrix" {
				mapped = true
			}
		}
		if !mapped {
			t.Fatalf("%s: qa→qa_signoff edge must map risk_coverage_matrix", tmpl.ID)
		}
	}
}

func fieldDeclared(fields []entity.WorkflowField, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// Definitions instantiated before the matrix contract do not declare the field,
// yet the gate still demands it on approval. The whitelist exemption lets the
// reviewer supply it at sign-off; without it such runs deadlock (gate requires
// what the whitelist rejects). The exemption must be rejected for steps that
// are not qa_signoff.
func TestLegacyQASignoffAcceptsRiskCoverageMatrixOutput(t *testing.T) {
	controlDB, err := controldb.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer controlDB.Close()
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "ws-matrix", Name: "WS", Slug: "ws-matrix", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	store := NewStore(controlDB, "ws-matrix")
	now := time.Now().UTC()

	// Deliberately legacy-shaped: qa_signoff without risk_coverage_matrix in
	// either its input or output contract.
	def := &entity.WorkflowDefinition{
		ID: "legacy-qa-pipeline", Name: "Legacy QA Pipeline", Version: 1,
		Scope: "workspace", StartStepID: "qa",
		Steps: []entity.WorkflowStep{
			{ID: "qa", Type: "agent_task", Title: "QA", ActorRole: "qa-agent", OutputFields: []entity.WorkflowField{{Name: "test_report"}}},
			{ID: "qa_signoff", Type: "human_review", Title: "QA Sign-off", ActorRole: "qa-owner", ReviewPolicy: "manual",
				InputFields:  []entity.WorkflowField{{Name: "test_report"}},
				OutputFields: []entity.WorkflowField{{Name: "decision"}, {Name: "comments"}}},
			{ID: "release", Type: "agent_task", Title: "Release", ActorRole: "release-agent",
				InputFields: []entity.WorkflowField{{Name: "release_candidate"}}, OutputFields: []entity.WorkflowField{{Name: "git_tag"}}},
		},
		Edges: []entity.WorkflowEdge{
			{ID: "e1", From: "qa", To: "qa_signoff", IsDefault: true, InputMapping: map[string]string{"test_report": "$output.test_report"}},
			{ID: "e2", From: "qa_signoff", To: "release", Condition: &entity.WorkflowEdgeCondition{Field: "decision", Operator: "eq", Value: "approve"}, InputMapping: map[string]string{"release_candidate": "$output.release_candidate"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	// QA completes with its legacy contract (test_report only).
	if _, _, err := store.StartRun("proj-qa", "task-qa-1", def.ID, map[string]entity.WorkflowActorBinding{
		"qa-agent": {Type: "agent", ID: "mira"},
		"qa-owner": {Type: "human", ID: "admin"},
	}); err != nil {
		t.Fatalf("start run: %v", err)
	}
	if _, err := store.CompleteAndAdvance("proj-qa", "task-qa-1", "qa done", "", map[string]string{"test_report": "all green"}, "completed"); err != nil {
		t.Fatalf("qa complete: %v", err)
	}

	// The human supplies the matrix at the sign-off step — the narrow path the
	// whitelist exemption opens for legacy definitions.
	matrix := `[{"item_id":"AC1","acceptance_criteria":"typecheck","risk_level":"low","status":"passed","evidence":"exit 0"}]`
	transition, err := store.CompleteAndAdvance("proj-qa", "task-qa-1", "approved", "", map[string]string{"decision": "approve", "comments": "ok", "risk_coverage_matrix": matrix}, "completed")
	if err != nil {
		t.Fatalf("legacy qa_signoff must accept risk_coverage_matrix output: %v", err)
	}
	if transition.Done || transition.Next == nil || transition.Next.ID != "release" {
		t.Fatalf("approval should route to release, got next=%#v done=%v", transition.Next, transition.Done)
	}

	// Non-qa_signoff steps keep the strict whitelist: an undeclared output is
	// still rejected there.
	if _, _, err := store.StartRun("proj-qa", "task-qa-2", def.ID, map[string]entity.WorkflowActorBinding{
		"qa-agent": {Type: "agent", ID: "mira"},
		"qa-owner": {Type: "human", ID: "admin"},
	}); err != nil {
		t.Fatalf("start second run: %v", err)
	}
	if _, err := store.CompleteAndAdvance("proj-qa", "task-qa-2", "qa done", "", map[string]string{"test_report": "green", "risk_coverage_matrix": matrix}, "completed"); err == nil {
		t.Fatalf("qa (non-signoff) step must still reject undeclared risk_coverage_matrix output")
	}
}
