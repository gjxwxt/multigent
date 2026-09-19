package db

import (
	"path/filepath"
	"testing"
)

// A run that was never addressed to a node (desired_runtime_node_id == "") used
// to be claimable by every node in the workspace. Because the run spec carries
// the executing agent's project env and model provider key, a second machine
// holding any workspace-issued token could harvest work belonging to projects it
// was never bound to. Ambient claim is therefore only allowed while the
// workspace has at most one non-disabled node; past that, runs must be
// addressed by binding the agent worker to a node.
func TestClaimRuntimeRunAmbientClaimRequiresSingleNode(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	const workspaceID = "ws-scope"
	if err := db.UpsertWorkspace(Workspace{ID: workspaceID, Name: "Scope", Slug: "scope", Root: "/tmp/scope", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}

	enqueue := func(id, project, desired string) {
		t.Helper()
		if err := db.UpsertRuntimeRun(RuntimeRun{
			ID: id, WorkspaceID: workspaceID, ProjectID: project, AgentID: "agent-" + project,
			TaskID: id, Status: "queued", Priority: 1, DesiredRuntimeNodeID: desired,
			SpecJSON: `{"kind":"exec_prompt"}`, ResultJSON: `{}`,
			CreatedAt: nowUTC(), UpdatedAt: nowUTC(),
		}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}

	register := func(nodes ...RuntimeNode) {
		t.Helper()
		for _, node := range nodes {
			node.WorkspaceID = workspaceID
			node.Kind = "personal_computer"
			node.Status = "online"
			node.LastSeenAt = nowUTC()
			node.CreatedByUserID = "admin"
			if err := db.UpsertRuntimeNode(node); err != nil {
				t.Fatalf("node %s: %v", node.ID, err)
			}
		}
	}

	// One node: unaddressed work still runs, so single-node deployments keep
	// working unchanged after this rule lands.
	enqueue("run-ambient-single", "proj-a", "")
	claimed, found, err := db.ClaimRuntimeRun(workspaceID, "node-a", 30, nil)
	if err != nil || !found {
		t.Fatalf("single-node ambient claim found=%v err=%v", found, err)
	}
	if claimed.ID != "run-ambient-single" || claimed.RuntimeNodeID != "node-a" {
		t.Fatalf("unexpected claim: %#v", claimed)
	}

	// Second node joins: nothing unaddressed is claimable by anyone until a
	// worker binding addresses it.
	register(RuntimeNode{ID: "node-a"}, RuntimeNode{ID: "node-b"})
	enqueue("run-ambient-multi", "proj-secret", "")
	for _, nodeID := range []string{"node-a", "node-b"} {
		if run, found, err := db.ClaimRuntimeRun(workspaceID, nodeID, 30, nil); err != nil || found {
			t.Fatalf("node %s claimed unaddressed run in multi-node workspace: run=%#v found=%v err=%v", nodeID, run, found, err)
		}
	}

	// Addressed work is unaffected: only the addressed node gets the run.
	enqueue("run-for-b", "proj-secret", "node-b")
	if run, found, err := db.ClaimRuntimeRun(workspaceID, "node-a", 30, nil); err != nil || found {
		t.Fatalf("node-a claimed a run addressed to node-b: run=%#v found=%v err=%v", run, found, err)
	}
	got, found, err := db.ClaimRuntimeRun(workspaceID, "node-b", 30, nil)
	if err != nil || !found {
		t.Fatalf("node-b claim found=%v err=%v", found, err)
	}
	if got.ID != "run-for-b" || got.RuntimeNodeID != "node-b" {
		t.Fatalf("unexpected claim for node-b: %#v", got)
	}
}

// A disabled node must not count toward the single-node allowance: taking a
// machine out of service cannot be a way to re-open ambient claim for the rest.
func TestClaimRuntimeRunDisabledNodeDoesNotCount(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	const workspaceID = "ws-disabled"
	if err := db.UpsertWorkspace(Workspace{ID: workspaceID, Name: "Disabled", Slug: "disabled", Root: "/tmp/disabled", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	for _, node := range []RuntimeNode{
		{ID: "node-live", WorkspaceID: workspaceID, Name: "Live", Kind: "personal_computer", Status: "online", LastSeenAt: nowUTC(), CreatedByUserID: "admin"},
		{ID: "node-off", WorkspaceID: workspaceID, Name: "Off", Kind: "personal_computer", Status: "disabled", LastSeenAt: nowUTC(), CreatedByUserID: "admin"},
	} {
		if err := db.UpsertRuntimeNode(node); err != nil {
			t.Fatalf("node: %v", err)
		}
	}
	if err := db.UpsertRuntimeRun(RuntimeRun{
		ID: "run-x", WorkspaceID: workspaceID, ProjectID: "proj", AgentID: "agent", TaskID: "task-x",
		Status: "queued", Priority: 1, SpecJSON: `{"kind":"exec_prompt"}`, ResultJSON: `{}`,
		CreatedAt: nowUTC(), UpdatedAt: nowUTC(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, found, err := db.ClaimRuntimeRun(workspaceID, "node-live", 30, nil); err != nil || !found {
		t.Fatalf("ambient claim with one live node should succeed: found=%v err=%v", found, err)
	}
}
