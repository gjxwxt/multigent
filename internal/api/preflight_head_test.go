package api

import (
	"testing"

	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// drivePastPreflightHead completes the greenfield v7 head pair (preflight
// agent step + preflight_review human clarify gate) with everything cleared,
// leaving the run at requirement_draft. Tests that only exercise the
// pre-existing spine call this before completing requirement_draft.
func drivePastPreflightHead(t *testing.T, wfStore *workflowstore.Store, project, taskID string) {
	t.Helper()
	if _, err := wfStore.CompleteAndAdvance(project, taskID, "preflighted", "", map[string]string{
		"preflight_report": `{"repo":"initialized","env":"ready","request":"startable"}`,
		"open_questions":   "[]",
	}, "completed"); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, err := wfStore.CompleteAndAdvance(project, taskID, "preflight ok", "", map[string]string{
		"decision": "approve",
		"comments": "environment ready",
	}, "completed"); err != nil {
		t.Fatalf("preflight review: %v", err)
	}
}
