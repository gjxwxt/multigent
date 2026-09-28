package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// c1SeedQASignoffRunNoSnapshot seeds a qa_signoff run whose stored run record
// carries NO definition snapshot and whose definition record is then deleted:
// the run is found, but its definition cannot be resolved. This is the
// deterministic "found run but unresolvable definition" state.
func c1SeedQASignoffRunNoSnapshot(t *testing.T) (*Server, string, *entity.Task, *workflowstore.Store, entity.WorkflowRun) {
	t.Helper()
	s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, false)
	// Strip the definition snapshot from the persisted run record, then delete
	// the definition itself. RunForTask still finds the run; RunDefinition
	// must now report not-found.
	run.DefinitionSnapshot = nil
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}
	if err := wfStore.DeleteDefinition(run.DefinitionID); err != nil {
		t.Fatal(err)
	}
	if _, defFound, err := wfStore.RunDefinition(run); err != nil || defFound {
		t.Fatalf("fixture must produce an unresolvable definition: found=%v err=%v", defFound, err)
	}
	return s, workspaceID, task, wfStore, run
}

// c1AssertRefusedBeforeAnyWrite pins the shared zero-write contract for a
// refused runtime qa_signoff approval: run parked, task not terminal.
func c1AssertRefusedBeforeAnyWrite(t *testing.T, wfStore *workflowstore.Store, s *Server, workspaceID string, task *entity.Task, activeStepID string) {
	t.Helper()
	parked, found, err := wfStore.RunForTask("resproj", task.ID)
	if err != nil || !found {
		t.Fatalf("run lookup after rejection: found=%v err=%v", found, err)
	}
	if parked.ActiveStepID != activeStepID || parked.Status != "active" {
		t.Fatalf("refused approval must leave the run parked, got %s@%s (want active@%s)", parked.Status, parked.ActiveStepID, activeStepID)
	}
	after, err := s.ts.GetTask("resproj", "agent", task.ID)
	if err != nil {
		t.Fatalf("task lookup: %v", err)
	}
	if after.Status == entity.TaskStatusDoneSuccess || after.Status == entity.TaskStatusDoneFailed {
		t.Fatalf("refused approval must not write a terminal task state, got %s", after.Status)
	}
}

// TestC1RuntimeGateFailsClosedOnUnresolvableDefinition pins the END-TO-END
// HTTP contract: a runtime qa_signoff approval whose run exists but whose
// definition cannot be resolved must be refused before any transition/persist
// with a diagnosable message naming the resolution failure — never a silent
// gate skip. The first line of defense is the delivery-gate shape check
// (workflowDeliveryGateShape, 500); the C1 gate itself must also refuse if it
// is ever reached in that state (see the direct-call test below).
func TestC1RuntimeGateFailsClosedOnUnresolvableDefinition(t *testing.T) {
	s, workspaceID, task, wfStore, run := c1SeedQASignoffRunNoSnapshot(t)

	rec := postC1RuntimeStepComplete(t, s, workspaceID, "resproj", task.ID, "agent", map[string]any{
		"summary": "approving without a resolvable definition",
		"status":  "completed",
		"outputs": map[string]string{"decision": "approve", "comments": "no matrix, no definition"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unresolvable definition must fail the runtime entry closed with 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workflow definition") || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("rejection must name the unresolvable workflow definition, got: %s", rec.Body.String())
	}
	c1AssertRefusedBeforeAnyWrite(t, wfStore, s, workspaceID, task, "qa_signoff")
	_ = run
}

// TestC1RuntimeGateFailsClosedOnUnresolvableActiveStep: a run whose stored
// ActiveStepID is not present in its (resolvable) definition is corrupted
// state — the runtime entry must refuse with a diagnosable error instead of
// silently skipping the gate.
func TestC1RuntimeGateFailsClosedOnUnresolvableActiveStep(t *testing.T) {
	s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, false)

	run.ActiveStepID = "step-that-does-not-exist"
	if err := wfStore.SaveRun(&run); err != nil {
		t.Fatal(err)
	}

	rec := postC1RuntimeStepComplete(t, s, workspaceID, "resproj", task.ID, "agent", map[string]any{
		"summary": "approving a corrupted active step",
		"status":  "completed",
		"outputs": map[string]string{"decision": "approve", "comments": "no matrix"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unresolvable active step must fail the runtime entry closed with 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workflow step") || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("rejection must name the unresolvable active step, got: %s", rec.Body.String())
	}
	c1AssertRefusedBeforeAnyWrite(t, wfStore, s, workspaceID, task, "step-that-does-not-exist")
}

// TestC1GateDirectCallRefusesUnresolvableRunState exercises the C1 gate's own
// resolution (completeRuntimeWorkflowStep without the handler's earlier
// delivery-gate shape check): a found run with an unresolvable definition must
// surface a diagnosable error BEFORE CompleteAndAdvance — the gate may not
// silently skip when the run exists. This is the test that goes red if the
// helper reverts to swallow-and-skip.
func TestC1GateDirectCallRefusesUnresolvableRunState(t *testing.T) {
	t.Run("unresolvable definition", func(t *testing.T) {
		s, workspaceID, task, wfStore, _ := c1SeedQASignoffRunNoSnapshot(t)

		_, transitioned, err := s.completeRuntimeWorkflowStep(workspaceID, "resproj", task, map[string]string{"decision": "approve"}, "completed")
		if err == nil {
			t.Fatal("the C1 gate must return a diagnosable error for a found run with an unresolvable definition")
		}
		if !strings.Contains(err.Error(), "workflow definition") {
			t.Fatalf("error must name the resolution failure, got: %v", err)
		}
		if transitioned {
			t.Fatal("no transition may be reported when the gate cannot resolve the run state")
		}
		c1AssertRefusedBeforeAnyWrite(t, wfStore, s, workspaceID, task, "qa_signoff")
	})

	t.Run("unresolvable active step", func(t *testing.T) {
		s, workspaceID, task, wfStore, run := c1SeedQASignoffRun(t, false)
		run.ActiveStepID = "step-that-does-not-exist"
		if err := wfStore.SaveRun(&run); err != nil {
			t.Fatal(err)
		}

		_, transitioned, err := s.completeRuntimeWorkflowStep(workspaceID, "resproj", task, map[string]string{"decision": "approve"}, "completed")
		if err == nil {
			t.Fatal("the C1 gate must return a diagnosable error for a found run with an unresolvable active step")
		}
		if !strings.Contains(err.Error(), "active step") {
			t.Fatalf("error must name the resolution failure, got: %v", err)
		}
		if transitioned {
			t.Fatal("no transition may be reported when the gate cannot resolve the run state")
		}
		c1AssertRefusedBeforeAnyWrite(t, wfStore, s, workspaceID, task, "step-that-does-not-exist")
	})

	t.Run("task without any workflow run keeps the legacy skip", func(t *testing.T) {
		// A task with no run at all is the legacy shape: the precheck inside
		// completeRuntimeWorkflowStep reports not-transitioned with no error
		// and nothing is written. The gate must not invent a failure here.
		s, workspaceID, task := seedDesignTask(t, entity.TaskStatusInProgress)
		_, transitioned, err := s.completeRuntimeWorkflowStep(workspaceID, "resproj", task, map[string]string{"decision": "approve"}, "completed")
		if err != nil {
			t.Fatalf("legacy no-run shape must not error at the gate, got: %v", err)
		}
		if transitioned {
			t.Fatal("a task with no workflow run must not report a transition")
		}
	})
}
