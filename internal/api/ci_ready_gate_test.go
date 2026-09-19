package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/ciready"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func TestClassifyCIReadyGateAttributesEachFailureToAnOwner(t *testing.T) {
	tests := []struct {
		name        string
		response    ciReadyResponse
		wantBlocked bool
		wantCause   string
		wantRetry   bool
		wantHuman   bool
	}{
		{
			name:        "ready report advances",
			response:    ciReadyResponse{Report: ciready.Report{Overall: ciready.OverallReady}},
			wantBlocked: false,
		},
		{
			name: "repository defect outranks a pending pipeline",
			response: ciReadyResponse{Report: ciready.Report{
				Overall: ciready.OverallNotReady,
				Checks: []ciready.Check{
					{Name: "lockfile", Status: ciready.StatusFail},
					{Name: "pipeline_evidence", Status: ciready.StatusFail},
				},
			}, Pipeline: &ciReadyPipelineEvidence{ID: 7, Status: "running"}},
			wantBlocked: true,
			wantCause:   gateCauseRepairable,
		},
		{
			name: "pipeline still running is a wait, not a defect",
			response: ciReadyResponse{Report: ciready.Report{
				Overall: ciready.OverallNotReady,
				Checks:  []ciready.Check{{Name: "pipeline_evidence", Status: ciready.StatusFail}},
			}, Pipeline: &ciReadyPipelineEvidence{ID: 7, Status: "running"}},
			wantBlocked: true,
			wantCause:   gateCauseWaitingCI,
			wantRetry:   true,
		},
		{
			name: "no pipeline observed for HEAD yet is also a wait",
			response: ciReadyResponse{Report: ciready.Report{
				Overall: ciready.OverallNotReady,
				Checks:  []ciready.Check{{Name: "pipeline_evidence", Status: ciready.StatusFail}},
			}, PipelineError: errNoPipelineForSHA.Error()},
			wantBlocked: true,
			wantCause:   gateCauseWaitingCI,
			wantRetry:   true,
		},
		{
			name: "pipeline ran and failed belongs to the developer",
			response: ciReadyResponse{Report: ciready.Report{
				Overall: ciready.OverallNotReady,
				Checks:  []ciready.Check{{Name: "pipeline_evidence", Status: ciready.StatusFail}},
			}, Pipeline: &ciReadyPipelineEvidence{ID: 7, Status: "failed"}},
			wantBlocked: true,
			wantCause:   gateCauseCIFailed,
		},
		{
			name: "unbound remote under required semantics is an environment problem",
			response: ciReadyResponse{Report: ciready.Report{
				Overall: ciready.OverallNotReady,
				Checks:  []ciready.Check{{Name: "pipeline_evidence", Status: ciready.StatusFail}},
			}, PipelineError: errRemoteRequired.Error()},
			wantBlocked: true,
			wantCause:   gateCauseEnvironment,
			wantHuman:   true,
		},
		{
			name: "not_ready with nothing failing still blocks",
			response: ciReadyResponse{Report: ciready.Report{
				Overall: ciready.OverallNotReady,
			}},
			wantBlocked: true,
			wantCause:   gateCauseRepairable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCIReadyGate(tc.response)
			if got.Blocked != tc.wantBlocked {
				t.Fatalf("blocked=%v want %v (%#v)", got.Blocked, tc.wantBlocked, got)
			}
			if !tc.wantBlocked {
				return
			}
			if got.Cause != tc.wantCause {
				t.Fatalf("cause=%q want %q (%#v)", got.Cause, tc.wantCause, got)
			}
			if got.NeedsHuman != tc.wantHuman {
				t.Fatalf("needsHuman=%v want %v (%#v)", got.NeedsHuman, tc.wantHuman, got)
			}
			if ciReadyGateIsRetryable(got) != tc.wantRetry {
				t.Fatalf("retryable=%v want %v (%#v)", ciReadyGateIsRetryable(got), tc.wantRetry, got)
			}
			// A waiting verdict must never read as a request for a person.
			if got.Cause == gateCauseWaitingCI && got.NeedsHuman {
				t.Fatalf("waiting verdict must not page a human: %#v", got)
			}
			if msg := ciReadyGateMessage(got); msg == "" {
				t.Fatal("held gate must tell the agent what to do next")
			}
		})
	}
}

// seedGateRun attaches a task to a run parked on a single agent step, mirroring
// how a real pipeline parks mid-delivery.
func seedGateRun(t *testing.T, s *Server, workspaceID, stepID string, config map[string]string) *entity.Task {
	t.Helper()
	seedSampleAgentsForTest(t, s, workspaceID)

	repo := t.TempDir()
	if err := os.WriteFile(repo+"/README.md", []byte("# gate fixture\n"), 0o644); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	project, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	project.Repo = repo
	if err := s.st.SaveProject("sample", project); err != nil {
		t.Fatalf("save project: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{
		ID: "task-ci-gate-1", Title: "Gate me", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := &entity.WorkflowDefinition{
		ID: "gate-pipeline-test", Name: "Gate Pipeline", Version: 1,
		Scope: "workspace", StartStepID: stepID,
		Steps: []entity.WorkflowStep{
			{ID: stepID, Type: "agent_task", Title: "Gate", ActorRole: "pm-agent", Config: config,
				OutputFields: []entity.WorkflowField{{Name: "ci_ready_report"}}},
			{ID: "after_gate", Type: "agent_task", Title: "After", ActorRole: "pm-agent",
				OutputFields: []entity.WorkflowField{{Name: "done"}}},
		},
		Edges:     []entity.WorkflowEdge{{ID: "e-out", From: stepID, To: "after_gate", IsDefault: true}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := wfStore.SaveDefinition(def); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	if _, _, err := wfStore.StartRunWithInput("sample", task.ID, def.ID, nil, map[string]string{}); err != nil {
		t.Fatalf("StartRunWithInput: %v", err)
	}
	return task
}

func TestCIReadyGateHoldsStepOnUnreadyRepository(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	task := seedGateRun(t, s, workspaceID, "ci_ready_gate", nil)

	decision, isGate, err := s.ciReadyGateDecisionForTask(context.Background(), workspaceID, "sample", task)
	if err != nil {
		t.Fatalf("gate decision: %v", err)
	}
	if !isGate {
		t.Fatal("ci_ready_gate must be recognised as a platform gate")
	}
	if !decision.Blocked {
		t.Fatalf("a repository that fails the deterministic checks must not be allowed through: %#v", decision)
	}
	if decision.Cause != gateCauseRepairable || len(decision.FailedCheckIds) == 0 {
		t.Fatalf("want a named, developer-owned failure, got %#v", decision)
	}
	if decision.StepID != "ci_ready_gate" {
		t.Fatalf("decision must name the held step, got %q", decision.StepID)
	}
}

// The gate runs on the path every agent step completion takes. If it recognized
// ordinary steps, a single mis-evaluation would freeze the whole platform.
func TestCIReadyGateLeavesOrdinaryStepsAlone(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	task := seedGateRun(t, s, workspaceID, "implement", nil)

	decision, isGate, err := s.ciReadyGateDecisionForTask(context.Background(), workspaceID, "sample", task)
	if err != nil {
		t.Fatalf("gate decision: %v", err)
	}
	if isGate {
		t.Fatalf("a normal agent step must not be gated: %#v", decision)
	}
	if decision.Blocked {
		t.Fatalf("non-gate step reported blocked: %#v", decision)
	}
}

// Recognition is declared on the step, so a project can name its gate anything.
func TestCIReadyGateRecognizesDeclaredStepConfig(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	task := seedGateRun(t, s, workspaceID, "quality_checkpoint", map[string]string{platformGateConfigKey: ciReadyGateKind})

	_, isGate, err := s.ciReadyGateDecisionForTask(context.Background(), workspaceID, "sample", task)
	if err != nil {
		t.Fatalf("gate decision: %v", err)
	}
	if !isGate {
		t.Fatal("config-declared gate steps must be enforced regardless of their ID")
	}
}

// The gate must judge, never repair: seeding a CI baseline from here would put
// the platform process inside a repository working tree.
func TestCIReadyGateDoesNotWriteToTheRepository(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	task := seedGateRun(t, s, workspaceID, "ci_ready_gate", nil)

	project, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	before := listFiles(t, project.Repo)

	if _, _, err := s.ciReadyGateDecisionForTask(context.Background(), workspaceID, "sample", task); err != nil {
		t.Fatalf("gate decision: %v", err)
	}

	after := listFiles(t, project.Repo)
	if len(after) != len(before) {
		t.Fatalf("gate created files in the repository: before=%v after=%v", before, after)
	}
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// postRuntimeStepComplete drives the endpoint every agent's `mga task step done`
// hits, so these two tests are the wiring proof: the gate must hold the step it
// is supposed to hold, and must stay out of the way of every other step.
func postRuntimeStepComplete(t *testing.T, s *Server, workspaceID, project, agent, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"agent":   agent,
		"status":  "success",
		"summary": "ci is ready, trust me",
		"outputs": map[string]string{"ci_ready_report": "ready"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime/tasks/"+taskID+"/workflow/step/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", taskID)
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeAgentKey, runtimeAgentPrincipal{
		WorkspaceID:  workspaceID,
		Project:      project,
		Agent:        agent,
		Capabilities: []string{"task.use"},
	}))
	rec := httptest.NewRecorder()
	s.handleRuntimeWorkflowStepComplete(rec, req)
	return rec
}

func activeGateStep(t *testing.T, s *Server, workspaceID, project, taskID string) string {
	t.Helper()
	run, found, err := workflowstore.NewStore(s.controlDB, workspaceID).RunForTask(project, taskID)
	if err != nil || !found {
		t.Fatalf("run lookup found=%v err=%v", found, err)
	}
	return run.ActiveStepID
}

func TestCIReadyGateHoldsStepCompletionOverHTTP(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	task := seedGateRun(t, s, workspaceID, "ci_ready_gate", nil)

	rec := postRuntimeStepComplete(t, s, workspaceID, "sample", "pm", task.ID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unready repository must not close the gate step: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), gateCauseRepairable) {
		t.Fatalf("response must name the owner of the failure: %s", rec.Body.String())
	}
	if step := activeGateStep(t, s, workspaceID, "sample", task.ID); step != "ci_ready_gate" {
		t.Fatalf("a held gate must leave the run parked on the gate, got %s", step)
	}
}

// The gate sits on the path every agent step completion takes. If it fired on
// ordinary steps, one bad evaluation would freeze delivery platform-wide.
func TestCIReadyGateDoesNotBlockOrdinaryStepCompletion(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	task := seedGateRun(t, s, workspaceID, "implement", nil)

	rec := postRuntimeStepComplete(t, s, workspaceID, "sample", "pm", task.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("a non-gate step must complete: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if step := activeGateStep(t, s, workspaceID, "sample", task.ID); step != "after_gate" {
		t.Fatalf("expected the run to advance past the ordinary step, got %s", step)
	}
}
