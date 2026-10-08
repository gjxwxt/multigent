package workflow

import (
	"path/filepath"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func TestBrownfieldOnboardingDefinitionStructure(t *testing.T) {
	tempDir := t.TempDir()
	db, err := controldb.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()

	store := NewStore(db, "ws-test")
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws-test", Name: "Test", Slug: "test", Root: tempDir}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := store.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	def, ok, err := store.Definition(BrownfieldOnboardingWorkflowID)
	if err != nil || !ok {
		t.Fatalf("load brownfield definition: ok=%v, err=%v", ok, err)
	}

	if def.ID != BrownfieldOnboardingWorkflowID {
		t.Fatalf("expected definition id %s, got %s", BrownfieldOnboardingWorkflowID, def.ID)
	}
	if def.StartStepID != "preflight" {
		t.Fatalf("expected start step preflight, got %s", def.StartStepID)
	}

	expectedSteps := []string{"preflight", "preflight_review", "readonly_scan", "baseline_pin", "verify_build", "evaluate_readiness", "human_signoff", "materialize_contract"}
	if len(def.Steps) != len(expectedSteps) {
		t.Fatalf("expected %d steps, got %d", len(expectedSteps), len(def.Steps))
	}

	stepMap := make(map[string]entity.WorkflowStep)
	for _, s := range def.Steps {
		stepMap[s.ID] = s
	}

	for _, expectedID := range expectedSteps {
		s, found := stepMap[expectedID]
		if !found {
			t.Fatalf("missing step %s", expectedID)
		}
		if expectedID == "preflight_review" || expectedID == "human_signoff" {
			if s.Type != "human_review" {
				t.Fatalf("expected %s to be human_review, got %s", expectedID, s.Type)
			}
			if s.ActorRole != "owner-engineer" {
				t.Fatalf("expected %s to have actor role owner-engineer, got %s", expectedID, s.ActorRole)
			}
		} else {
			if s.Type != "agent_task" {
				t.Fatalf("expected step %s to be agent_task, got %s", expectedID, s.Type)
			}
			if s.ActorRole != "project-initializer" {
				t.Fatalf("expected step %s to have actor role project-initializer, got %s", expectedID, s.ActorRole)
			}
		}
	}

	// Preflight clarify gate (v2): the human preflight_review step must
	// declare open_questions as an input (activates the per-question cards
	// and the server-side freeze gate) and the approve edge must forward the
	// original request into readonly_scan as onboarding_request.
	declaresOpenQuestions := false
	for _, f := range stepMap["preflight_review"].InputFields {
		if f.Name == "open_questions" {
			declaresOpenQuestions = true
		}
	}
	if !declaresOpenQuestions {
		t.Fatal("preflight_review must declare open_questions as an input (clarify loop contract)")
	}
	approveToScan := false
	for _, e := range def.Edges {
		if e.From == "preflight_review" && e.To == "readonly_scan" && e.Condition != nil &&
			e.Condition.Field == "decision" && e.Condition.Value == "approve" {
			if e.InputMapping["onboarding_request"] != "$input.request" {
				t.Fatalf("preflight approve edge must forward request as onboarding_request, mapping=%v", e.InputMapping)
			}
			approveToScan = true
		}
	}
	if !approveToScan {
		t.Fatal("missing preflight_review -> readonly_scan approve edge")
	}

	// Verify template catalog contains brownfield template
	templates := Templates("zh-CN")
	foundTemplate := false
	for _, tmpl := range templates {
		if tmpl.ID == "brownfield-onboarding-pipeline" {
			foundTemplate = true
			break
		}
	}
	if !foundTemplate {
		t.Fatalf("brownfield-onboarding-pipeline template not found in Templates catalog")
	}
}

func TestBrownfieldOnboardingLifecycle_ApprovalAndRework(t *testing.T) {
	tempDir := t.TempDir()
	db, err := controldb.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()

	store := NewStore(db, "ws-test")
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws-test", Name: "Test", Slug: "test", Root: tempDir}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := store.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	project := "brownfield-proj"
	taskID := "task-bf-001"

	bindings := map[string]entity.WorkflowActorBinding{
		"project-initializer": {Type: "agent", ID: "agent-mira"},
		"owner-engineer":      {Type: "human", ID: "admin"},
	}

	run, steps, err := store.StartRun(project, taskID, BrownfieldOnboardingWorkflowID, bindings)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if len(steps) == 0 || steps[0].StepID != "preflight" {
		t.Fatalf("expected initial step preflight, got %+v", steps)
	}

	// 0. preflight -> preflight_review (agent emits report, questions cleared)
	res, err := store.CompleteAndAdvance(project, taskID, "preflight done", "", map[string]string{
		"preflight_report": `{"repo":"existing","runtimeJson":false,"targetBranch":"main"}`,
		"open_questions":   "[]",
	}, "completed")
	if err != nil {
		t.Fatalf("advance preflight: %v", err)
	}
	if res.Next == nil || res.Next.ID != "preflight_review" {
		t.Fatalf("expected next step preflight_review, got %+v", res.Next)
	}

	// 0b. preflight_review approve -> readonly_scan (request forwarded)
	res, err = store.CompleteAndAdvance(project, taskID, "preflight approved", "", map[string]string{
		"decision": "approve",
		"comments": "parameters confirmed",
	}, "completed")
	if err != nil {
		t.Fatalf("advance preflight_review: %v", err)
	}
	if res.Next == nil || res.Next.ID != "readonly_scan" {
		t.Fatalf("expected next step readonly_scan, got %+v", res.Next)
	}

	// 1. readonly_scan -> baseline_pin
	res, err = store.CompleteAndAdvance(project, taskID, "readonly_scan", "step done", map[string]string{
		"scan_report": `{"techStacks":["node"],"services":[{"name":"frontend","directory":"web","techStack":"node","hasLockfile":true}]}`,
	}, "completed")
	if err != nil {
		t.Fatalf("advance readonly_scan: %v", err)
	}
	if res.Next == nil || res.Next.ID != "baseline_pin" {
		t.Fatalf("expected next step baseline_pin, got %+v", res.Next)
	}

	// 2. baseline_pin -> verify_build
	res, err = store.CompleteAndAdvance(project, taskID, "baseline_pin", "step done", map[string]string{
		"base_commit":     "c1a2b3d4e5f6",
		"baseline_status": "clean, no dirty files",
	}, "completed")
	if err != nil {
		t.Fatalf("advance baseline_pin: %v", err)
	}
	if res.Next == nil || res.Next.ID != "verify_build" {
		t.Fatalf("expected next step verify_build, got %+v", res.Next)
	}

	// 3. verify_build -> evaluate_readiness
	res, err = store.CompleteAndAdvance(project, taskID, "verify_build", "step done", map[string]string{
		"verification_log": "npm ci PASS\nnpm test PASS\nnpm run build PASS",
		"build_exit_code":  "0",
	}, "completed")
	if err != nil {
		t.Fatalf("advance verify_build: %v", err)
	}
	if res.Next == nil || res.Next.ID != "evaluate_readiness" {
		t.Fatalf("expected next step evaluate_readiness, got %+v", res.Next)
	}

	// 4. evaluate_readiness -> human_signoff
	res, err = store.CompleteAndAdvance(project, taskID, "evaluate_readiness", "step done", map[string]string{
		"readiness_report":         `{"status":"ready","issues":[]}`,
		"synthesized_runtime_json": `{"version":1,"frontend":{"directory":"web","command":"npm run dev","port":3000,"healthPath":"/"}}`,
	}, "completed")
	if err != nil {
		t.Fatalf("advance evaluate_readiness: %v", err)
	}
	if res.Next == nil || res.Next.ID != "human_signoff" {
		t.Fatalf("expected next step human_signoff, got %+v", res.Next)
	}

	// 5. Test human_signoff Request Changes -> Loops back to readonly_scan
	resRework, err := store.CompleteAndAdvance(project, taskID, "human_signoff", "request changes", map[string]string{
		"decision": "request_changes",
		"comments": "Please verify backend directory as well",
	}, "completed")
	if err != nil {
		t.Fatalf("advance human_signoff rework: %v", err)
	}
	if resRework.Next == nil || resRework.Next.ID != "readonly_scan" {
		t.Fatalf("expected request_changes to loop back to readonly_scan, got %+v", resRework.Next)
	}

	// Fast-forward back to human_signoff
	_, _ = store.CompleteAndAdvance(project, taskID, "readonly_scan", "done", map[string]string{"scan_report": "{}"}, "completed")
	_, _ = store.CompleteAndAdvance(project, taskID, "baseline_pin", "done", map[string]string{"base_commit": "c1a2b3", "baseline_status": "clean"}, "completed")
	_, _ = store.CompleteAndAdvance(project, taskID, "verify_build", "done", map[string]string{"verification_log": "ok", "build_exit_code": "0"}, "completed")
	_, _ = store.CompleteAndAdvance(project, taskID, "evaluate_readiness", "done", map[string]string{
		"readiness_report":         `{"status":"ready"}`,
		"synthesized_runtime_json": `{"version":1,"frontend":{"directory":"web","command":"npm run dev","port":3000,"healthPath":"/"}}`,
	}, "completed")

	// 6. Test human_signoff Approval -> Proceeds to materialize_contract
	resApproved, err := store.CompleteAndAdvance(project, taskID, "human_signoff", "approved", map[string]string{
		"decision":              "approve",
		"comments":              "Looks great, proceed with onboarding",
		"approved_runtime_json": `{"version":1,"frontend":{"directory":"web","command":"npm run dev","port":3000,"healthPath":"/"}}`,
	}, "completed")
	if err != nil {
		t.Fatalf("advance human_signoff approve: %v", err)
	}
	if resApproved.Next == nil || resApproved.Next.ID != "materialize_contract" {
		t.Fatalf("expected approve to advance to materialize_contract, got %+v", resApproved.Next)
	}

	// 7. materialize_contract -> Completed
	resCompleted, err := store.CompleteAndAdvance(project, taskID, "materialize_contract", "contract materialized in sandbox", map[string]string{
		"ready_commit":      "f9e8d7c6b5a4",
		"onboarding_status": "ready",
	}, "completed")
	if err != nil {
		t.Fatalf("advance materialize_contract: %v", err)
	}
	if !resCompleted.Done || resCompleted.Run.Status != "completed" {
		t.Fatalf("expected run status completed, got done=%v status=%s", resCompleted.Done, resCompleted.Run.Status)
	}

	_ = run
}
