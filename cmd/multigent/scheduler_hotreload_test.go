package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/taskstore"
)

func newSchedulerHotReloadEnv(t *testing.T) (string, taskstore.Store, *controldb.SQLiteStore) {
	t.Helper()
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
	t.Cleanup(func() { db.Close() })
	nowText := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: nowText}); err != nil {
		t.Fatal(err)
	}
	ts := taskstore.NewDB(root, db)
	return root, ts, db
}

func upsertHeartbeatWorker(t *testing.T, db *controldb.SQLiteStore, workerID, name string, projects ...string) {
	t.Helper()
	nowText := time.Now().UTC().Format(time.RFC3339)
	scheduleRaw, _ := json.Marshal(entity.HeartbeatConfig{Enabled: true, Interval: "30m"})
	if err := db.UpsertAgentWorker(controldb.AgentWorker{
		ID:           workerID,
		WorkspaceID:  "ws",
		Name:         name,
		DisplayName:  name,
		ScheduleJSON: string(scheduleRaw),
		CreatedAt:    nowText,
		UpdatedAt:    nowText,
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		if err := db.UpsertProjectMembership(controldb.ProjectMembership{
			ID:               "pm-" + workerID + "-" + project,
			WorkspaceID:      "ws",
			ProjectID:        project,
			MemberType:       "agent_worker",
			MemberID:         workerID,
			Role:             name,
			Title:            name,
			AutoPickTasks:    true,
			AttentionEnabled: true,
			CreatedAt:        nowText,
			UpdatedAt:        nowText,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Defect #9 residual: a resident CLI scheduler keeps heartbeat loops for the
// workers that had heartbeat enabled at startup, and each loop ran on the
// startup membership snapshot. A worker added to a new project while the
// scheduler was running never got scheduled there until restart. The loop
// must re-scan memberships and pick up the new project.
func TestRefreshHeartbeatMembershipsPicksUpNewProject(t *testing.T) {
	root, _, db := newSchedulerHotReloadEnv(t)
	upsertHeartbeatWorker(t, db, "aw-hotreload", "mira", "alpha")

	current := []schedulerAgentKey{{project: "alpha", agent: "mira"}}
	refreshed, changed := refreshHeartbeatMemberships(root, "alpha", "mira", current)
	if changed {
		t.Fatalf("expected no change before membership is added, got %#v", refreshed)
	}

	if err := db.UpsertProjectMembership(controldb.ProjectMembership{
		ID:               "pm-hotreload-gamma",
		WorkspaceID:      "ws",
		ProjectID:        "gamma",
		MemberType:       "agent_worker",
		MemberID:         "aw-hotreload",
		Role:             "mira",
		Title:            "mira",
		AutoPickTasks:    true,
		AttentionEnabled: true,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	refreshed, changed = refreshHeartbeatMemberships(root, "alpha", "mira", current)
	if !changed {
		t.Fatalf("expected membership change after new project membership")
	}
	if len(refreshed) != 2 {
		t.Fatalf("expected alpha+gamma memberships, got %#v", refreshed)
	}
	if refreshed[0] != (schedulerAgentKey{project: "alpha", agent: "mira"}) {
		t.Fatalf("expected the loop anchors to stay first, got %#v", refreshed)
	}
	foundGamma := false
	for _, key := range refreshed {
		if key == (schedulerAgentKey{project: "gamma", agent: "mira"}) {
			foundGamma = true
		}
	}
	if !foundGamma {
		t.Fatalf("expected gamma membership in refreshed set, got %#v", refreshed)
	}
}

// The discovery loop must spawn a heartbeat loop for a worker whose heartbeat
// was enabled after the scheduler started — without a restart.
func TestHeartbeatDiscoveryLoopStartsLoopForNewWorker(t *testing.T) {
	root, ts, db := newSchedulerHotReloadEnv(t)
	// No heartbeat-enabled worker at startup: the discovery loop should idle.
	registry := newSchedulerWorkerRegistry(nil)
	projects, err := ts.ListProjects()
	if err != nil {
		t.Fatal(err)
	}

	// Registry claim semantics: unknown worker is claimable, then held.
	if !registry.registerIfNew("aw-late") {
		t.Fatalf("expected first claim of aw-late to succeed")
	}
	if registry.registerIfNew("aw-late") {
		t.Fatalf("expected second claim of aw-late to be rejected")
	}
	registry.release("aw-late")
	if !registry.registerIfNew("aw-late") {
		t.Fatalf("expected claim after release to succeed")
	}
	registry.release("aw-late")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkspaceHeartbeatDiscoveryLoop(ctx, root, ts, nil, "", registry)
	time.Sleep(50 * time.Millisecond)

	// Enable heartbeat on a brand-new worker while the loop is running.
	upsertHeartbeatWorker(t, db, "aw-late", "late-agent", "alpha")
	time.Sleep(1500 * time.Millisecond)
	cancel()

	// The loop must have claimed and spawned it: a second discovery pass must
	// not re-claim the worker (registerIfNew returns false).
	targets, _, _ := collectAgentWorkerSchedulerTargets(root, projects, "", ts)
	found := false
	for _, target := range targets {
		if target.workerID == "aw-late" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected aw-late among heartbeat targets, got %#v", targets)
	}
}
