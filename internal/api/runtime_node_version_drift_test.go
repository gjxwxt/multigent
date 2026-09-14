package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// captureSlog swaps the default logger for one writing to the returned
// buffer, restoring the previous logger on test cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Version-drift detection: a node heartbeat reporting a build version that
// differs from the console's own must be logged (once per node+version) —
// mixed-version fleets are unsupported and otherwise surface only as
// confusing claim/lease behavior days after a console upgrade. The heartbeat
// itself must still succeed: rejecting it would starve lease renewal on a
// live run.
func TestRuntimeNodeHeartbeatWarnsOnVersionDrift(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	logs := captureSlog(t)
	nowText := time.Now().UTC().Format(time.RFC3339)
	node := controldb.RuntimeNode{
		ID:              "rtn-drift",
		WorkspaceID:     workspaceID,
		Name:            "Drift Node",
		Kind:            "personal_computer",
		Status:          "online",
		Version:         "v0.1.0",
		LastSeenAt:      nowText,
		CreatedByUserID: "admin",
		CreatedAt:       nowText,
		UpdatedAt:       nowText,
	}
	if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
		t.Fatalf("runtime node: %v", err)
	}
	s.SetVersion("v0.2.0")

	driftWarnings := func() int {
		return strings.Count(logs.String(), "runtime node version drift")
	}

	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, workspaceID, "v0.1.0"))
	if got := driftWarnings(); got != 1 {
		t.Fatalf("expected exactly one drift warning, got %d (logs: %s)", got, logs.String())
	}

	// Same version again on the next heartbeat: deduped, no second warning.
	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, workspaceID, "v0.1.0"))
	if got := driftWarnings(); got != 1 {
		t.Fatalf("expected drift warning to be deduped, got %d", got)
	}

	// Node upgrades to the console's version: no warning.
	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, workspaceID, "v0.2.0"))
	if got := driftWarnings(); got != 1 {
		t.Fatalf("matching version must not warn, got %d", got)
	}

	// A later downgrade warns again (reported version changed back).
	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, workspaceID, "v0.1.0"))
	if got := driftWarnings(); got != 2 {
		t.Fatalf("expected re-warn after version change, got %d", got)
	}

	stored, found, err := s.controlDB.RuntimeNodeByID(workspaceID, node.ID)
	if err != nil || !found {
		t.Fatalf("reload node found=%v err=%v", found, err)
	}
	if stored.Version != "v0.1.0" {
		t.Fatalf("node version not persisted: %q", stored.Version)
	}
}

// An empty console version (dev builds before SetVersion) or an empty node
// version must never produce a spurious warning.
func TestRuntimeNodeHeartbeatSkipsDriftCheckWhenVersionsUnknown(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	logs := captureSlog(t)
	nowText := time.Now().UTC().Format(time.RFC3339)
	node := controldb.RuntimeNode{
		ID:              "rtn-nodrift",
		WorkspaceID:     workspaceID,
		Name:            "No-Drift Node",
		Kind:            "personal_computer",
		Status:          "online",
		LastSeenAt:      nowText,
		CreatedByUserID: "admin",
		CreatedAt:       nowText,
		UpdatedAt:       nowText,
	}
	if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
		t.Fatalf("runtime node: %v", err)
	}

	// Console version unset, node reports something: no warning possible.
	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, workspaceID, "v9.9.9"))
	// Console version set, node reports empty: nothing to compare.
	s.SetVersion("v1.0.0")
	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, workspaceID, ""))
	if got := strings.Count(logs.String(), "runtime node version drift"); got != 0 {
		t.Fatalf("unknown versions must not warn, got %d", got)
	}
}

// heartbeatRequest builds a heartbeat call with the node principal injected
// the same way withRuntimeNodeAuth does.
func heartbeatRequest(t *testing.T, nodeID, workspaceID, version string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"status": "online", "version": version})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/heartbeat", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{
		Node: controldb.RuntimeNode{ID: nodeID, WorkspaceID: workspaceID},
	}))
	return req
}

// Concurrent heartbeats from many nodes must not crash the process — the
// drift-dedupe map is written from concurrent HTTP handlers (GPT review
// P0-2). Run under -race: every goroutine writes a distinct node id (map
// growth) and hammers the shared map, which a lost mutex turns into
// `concurrent map writes` deterministically enough for the race detector.
func TestRuntimeNodeHeartbeatConcurrentDriftCheckIsRaceFree(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	s.SetVersion("v1.0.0")

	const nodes = 16
	const rounds = 40
	var wg sync.WaitGroup
	for n := 0; n < nodes; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			nodeID := workspaceIDForDriftNodes + "-drift-" + strconv.Itoa(n)
			for i := 0; i < rounds; i++ {
				version := "v0.1." + strconv.Itoa(i%3)
				s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, nodeID, workspaceIDForDriftNodes, version))
			}
		}(n)
	}
	wg.Wait()
}

// Deleting a node drops its drift entry so a re-registered node starts a
// clean episode and the map cannot grow without bound across node churn.
func TestForgetRuntimeNodeDriftState(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	s.SetVersion("v1.0.0")
	node := controldb.RuntimeNode{ID: "rtn-forget", WorkspaceID: "ws"}
	s.handleRuntimeNodeHeartbeat(httptest.NewRecorder(), heartbeatRequest(t, node.ID, "ws", "v0.9.0"))
	s.driftWarnedMu.Lock()
	_, present := s.driftWarnedNodes[node.ID]
	s.driftWarnedMu.Unlock()
	if !present {
		t.Fatal("drift entry missing after warning")
	}

	s.forgetRuntimeNodeDriftState(node.ID)
	s.driftWarnedMu.Lock()
	_, present = s.driftWarnedNodes[node.ID]
	s.driftWarnedMu.Unlock()
	if present {
		t.Fatal("drift entry survived forgetRuntimeNodeDriftState")
	}
}

const workspaceIDForDriftNodes = "ws"
