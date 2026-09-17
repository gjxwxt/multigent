package preview

import (
	"testing"
)

func TestTurnPreviewInstanceLifecycle(t *testing.T) {
	e := NewEngine()

	taskID := "task-turn-test"
	turnID := "turn-abc12345"

	// Initial check: turn does not exist
	if _, ok := e.GetTurnInstance(taskID, turnID); ok {
		t.Fatal("expected GetTurnInstance to return false initially")
	}

	// Seed instance for test
	seeded := &PreviewInstance{
		TaskID:  "turn:" + taskID + ":" + turnID,
		Project: "test-proj",
		Status:  "running",
		Port:    45678,
	}
	e.SeedInstanceForTest(seeded)

	// Fetch via GetTurnInstance
	inst, ok := e.GetTurnInstance(taskID, turnID)
	if !ok || inst == nil {
		t.Fatal("expected GetTurnInstance to find seeded instance")
	}
	if inst.Port != 45678 || inst.Project != "test-proj" {
		t.Fatalf("unexpected instance attributes: %+v", inst)
	}

	// Also verify that the regular taskID does NOT collide
	if _, ok := e.GetInstance(taskID); ok {
		t.Fatal("expected regular task preview not to collide with turn preview")
	}

	// Stop turn preview
	if err := e.StopTurnPreview(taskID, turnID); err != nil {
		t.Fatalf("StopTurnPreview failed: %v", err)
	}

	// After stop: must not be found
	if _, ok := e.GetTurnInstance(taskID, turnID); ok {
		t.Fatal("expected GetTurnInstance to return false after stop")
	}
}
