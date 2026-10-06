package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/taskstore"
)

// Regression for the 2026-10-03 heartbeat flapping incident: the scheduler
// loop holds its hb copy across a 30s+ sleep. A pause/disable saved by the
// UI/API inside that window must survive the loop's next save instead of
// being silently reverted by the loop's stale snapshot.
func TestSaveSchedulerHeartbeatMergesConcurrentConfigChange(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", "")
	t.Setenv("MULTIGENT_DATA_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: nowText}); err != nil {
		t.Fatal(err)
	}
	scheduleRaw, _ := json.Marshal(entity.HeartbeatConfig{Enabled: true, Interval: "30s", SessionID: "sess-live"})
	if err := db.UpsertAgentWorker(controldb.AgentWorker{
		ID:           "aw-flap",
		WorkspaceID:  "ws",
		Name:         "flapper",
		DisplayName:  "flapper",
		ScheduleJSON: string(scheduleRaw),
		CreatedAt:    nowText,
		UpdatedAt:    nowText,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertProjectMembership(controldb.ProjectMembership{
		ID:          "pm-flap",
		WorkspaceID: "ws",
		ProjectID:   "proj",
		MemberType:  "agent_worker",
		MemberID:    "aw-flap",
		Title:       "flapper",
		Role:        "developer",
		CreatedAt:   nowText,
		UpdatedAt:   nowText,
	}); err != nil {
		t.Fatal(err)
	}
	ts := taskstore.NewDB(root, db)

	// The loop's stale in-memory copy: enabled, mid-cycle.
	loopHB := &entity.HeartbeatConfig{Enabled: true, Interval: "30s", SessionID: "sess-live"}

	// Concurrent admin pause lands in the DB while the loop sleeps.
	paused, _ := json.Marshal(entity.HeartbeatConfig{Enabled: false, Paused: true, Interval: "30s", SessionID: "sess-live"})
	w, ok, err := db.AgentWorkerByID("ws", "aw-flap")
	if err != nil || !ok {
		t.Fatalf("load worker: %v ok=%v", err, ok)
	}
	w.ScheduleJSON = string(paused)
	if err := db.UpsertAgentWorker(w); err != nil {
		t.Fatal(err)
	}

	// The loop saves its stale snapshot (e.g. NextWakeupAt update).
	next := now.Add(30 * time.Second).UTC()
	loopHB.NextWakeupAt = &next
	if err := saveSchedulerHeartbeat(root, "proj", "flapper", ts, loopHB); err != nil {
		t.Fatalf("save: %v", err)
	}

	after, ok, err := db.AgentWorkerByID("ws", "aw-flap")
	if err != nil || !ok {
		t.Fatalf("reload worker: %v ok=%v", err, ok)
	}
	var got entity.HeartbeatConfig
	if err := json.Unmarshal([]byte(after.ScheduleJSON), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatalf("Enabled reverted to true by loop save; want merged false")
	}
	if !got.Paused {
		t.Fatalf("Paused reverted to false by loop save; want merged true")
	}
	if got.NextWakeupAt == nil || !got.NextWakeupAt.Equal(next) {
		t.Fatalf("NextWakeupAt lost by merge: got %v, want %v", got.NextWakeupAt, next)
	}
	if got.SessionID != "sess-live" {
		t.Fatalf("SessionID clobbered: got %q", got.SessionID)
	}
}
