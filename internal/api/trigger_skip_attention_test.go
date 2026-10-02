package api

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// TestSkippedTriggerLandsAttentionOnAgentTasks: a Fire that resolves to a
// silent skip (agent worker gone / paused / trigger unconfigured) must leave
// a system comment on the waiting workflow task — the operator sees WHY
// nothing is happening instead of staring at a parked task.
func TestSkippedTriggerLandsAttentionOnAgentTasks(t *testing.T) {
	// The plan-stage fixture carries a real workflow run on a task assigned
	// to sample/pm — exactly the shape a skipped dispatch strands.
	s, workspaceID, root, _, _ := seedBackfillFixture(t)

	if s.triggers == nil {
		t.Fatal("fixture must have a trigger manager")
	}
	// Direct callback invocation with an agent spelling that matches nothing
	// (the binding-resolution failure shape).
	s.onSkippedTriggerAttention("sample", "no-such-agent", "no heartbeat or worker schedule for this agent")

	comments, err := s.ts.ListComments("sample", "pm", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range comments {
		if c.Author == "system" && strings.Contains(c.Body, "派发被跳过") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a skipped dispatch must surface a system comment on the waiting task, got %d comments", len(comments))
	}
	_ = workspaceID
}

// TestSkippedTriggerFullFirePath: the whole chain — Fire on an agent with no
// resolvable worker lands the system comment on the waiting task through the
// injected callback.
func TestSkippedTriggerFullFirePath(t *testing.T) {
	s, _, root, _, _ := seedBackfillFixture(t)
	// Fire for an agent whose worker cannot resolve: the silent-skip path
	// fires the injected callback, which lands the comment on the parked
	// workflow task.
	s.triggers.Fire("sample", "ghost-agent", entity.TriggerOnTask, "workflow step dispatch")
	comments, err := s.ts.ListComments("sample", "pm", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range comments {
		if c.Author == "system" && strings.Contains(c.Body, "派发被跳过") {
			return
		}
	}
	t.Fatalf("the full Fire path must surface the skip on the waiting task, got %d comments", len(comments))
}

// TestSkippedTriggerCallbackNilSafe: a trigger manager without the callback
// (unit fixtures, other embedders) skips silently as before — no panic.
func TestSkippedTriggerCallbackNilSafe(t *testing.T) {
	s, _ := newBranchJoinHTTPServer(t)
	if s.triggers == nil {
		t.Fatal("fixture must have a trigger manager")
	}
	s.triggers.onSkippedTrigger = nil
	// Must not panic.
	s.triggers.Fire("sample", "no-such-agent", entity.TriggerOnTask, "workflow step dispatch")
}
