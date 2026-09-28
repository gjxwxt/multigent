package main

import (
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// TestManualRunRecoversStaleSchedulerSession is the H1 closeout regression:
// the /start precheck admits a STALE scheduler interaction (idle beyond the
// shared two-minute window) and falls through to spawning `multigent run
// --task`. The spawned CLI acquires the interaction lock as sourceKind
// manual_run — which previously NEVER recovered a stale scheduler session
// (ShouldRecoverStaleInteraction required a scheduler requester), so the
// spawn exited "agent is busy in scheduler session" right after the API had
// already reported ok+pid. The fix admits manual_run requesters for the same
// stale-scheduler shape; this test exercises the REAL CLI acquisition entry
// against a live control DB with a stale session in it.
func TestManualRunRecoversStaleSchedulerSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	db := openTestWorkspaceControlDB(t, root)
	defer db.Close()
	if err := db.UpsertWorkspace(controldb.Workspace{
		ID: "ws-one", Name: "One", Slug: "one", Root: root,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	stale := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	if err := db.CreateInteractionSession(controldb.InteractionSession{
		ID: "sess-stale-scheduler", WorkspaceID: "ws-one",
		ProjectID: "project", AgentID: "pm", SourceKind: "scheduler", SourceChannel: "scheduler",
		ActorType: "system", ActorID: "scheduler", Status: "active",
		LockReason: "running_task", MetadataJSON: "{}",
		CreatedAt: stale, UpdatedAt: stale, LastActivityAt: stale,
	}); err != nil {
		t.Fatal(err)
	}
	lease, busy, err := acquireCLIInteraction(root, "project", "pm", "manual_run", "cli", "cli", "running_task")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if busy || lease == nil {
		t.Fatalf("manual_run must recover a stale scheduler session: busy=%v lease=%v", busy, lease != nil)
	}
	defer lease.Release()
	// The recovered predecessor must be marked failed (crashed-predecessor
	// semantics), not left active behind the new session.
	row, found, err := db.InteractionSessionByID("sess-stale-scheduler")
	if err != nil {
		t.Fatalf("lookup predecessor: %v", err)
	}
	if !found {
		t.Fatalf("predecessor session vanished instead of being marked failed")
	}
	if row.Status != "failed" {
		t.Fatalf("recovered predecessor must be marked failed, got %q", row.Status)
	}
	// A second manual_run acquirer must now see the NEW live session as busy
	// (the fix must not turn the lock into a no-op).
	second, busy, err := acquireCLIInteraction(root, "project", "pm", "manual_run", "cli", "cli", "running_task")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if !busy || second == nil {
		t.Fatalf("second manual_run acquire must be busy against the fresh session: busy=%v", busy)
	}
	if second.session.ID != lease.session.ID {
		t.Fatalf("busy result must reference the live blocking session %s, got %s", lease.session.ID, second.session.ID)
	}
}
