package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// A run carries the executing agent's project env and model provider key in its
// spec, so which node gets to fetch it is a credential boundary, not a
// scheduling detail. Two live nodes in one workspace must not be able to reach
// each other's unaddressed work.
func TestRuntimeNodeClaimHoldsUnaddressedRunFromSecondNode(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)

	var nodes []controldb.RuntimeNode
	for _, id := range []string{"rtn-a", "rtn-b"} {
		node := controldb.RuntimeNode{
			ID: id, WorkspaceID: workspaceID, Name: strings.ToUpper(id),
			Kind: "personal_computer", Status: "online",
			LastSeenAt: nowText, CreatedByUserID: "admin",
			CreatedAt: nowText, UpdatedAt: nowText,
		}
		if err := s.controlDB.UpsertRuntimeNode(node); err != nil {
			t.Fatalf("node %s: %v", id, err)
		}
		nodes = append(nodes, node)
	}
	otherNode, rivalNode := nodes[0], nodes[1]

	run := controldb.RuntimeRun{
		ID: "rtrun-unaddressed", WorkspaceID: workspaceID, ProjectID: "sample", AgentID: "pm",
		TaskID: "task-unaddressed", Status: "queued", Priority: 2,
		SpecJSON:   `{"kind":"task"}`,
		ResultJSON: `{}`, CreatedAt: nowText, UpdatedAt: nowText,
	}
	if err := s.controlDB.UpsertRuntimeRun(run); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimAs := func(node controldb.RuntimeNode) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-node/runs/claim", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{Node: node}))
		rec := httptest.NewRecorder()
		s.handleRuntimeNodeClaimRun(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("claim status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode claim: %v", err)
		}
		return out
	}

	for _, node := range nodes {
		if got := claimAs(node); got["run"] != nil {
			t.Fatalf("node %s claimed an unaddressed run: %v", node.ID, got)
		}
	}

	// The parked work must explain itself: silent stalling reads as a dead node.
	notice, _ := claimAs(otherNode)["notice"].(string)
	if !strings.Contains(notice, "not addressed") {
		t.Fatalf("claim gave no reason for parked work: %q", notice)
	}

	// Fetching the spec is the disclosure; it stays closed while unclaimed.
	specReq := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-node/runs/rtrun-unaddressed/spec", nil)
	specReq.SetPathValue("runId", run.ID)
	specReq = specReq.WithContext(context.WithValue(specReq.Context(), ctxRuntimeNodeKey, runtimeNodePrincipal{Node: rivalNode}))
	specRec := httptest.NewRecorder()
	s.handleRuntimeNodeRunSpec(specRec, specReq)
	if specRec.Code == http.StatusOK {
		t.Fatalf("rival node fetched spec of an unclaimed run: %s", specRec.Body.String())
	}
	if body := specRec.Body.String(); strings.Contains(body, "provider") || strings.Contains(body, "apiKey") {
		t.Fatalf("denied spec response still leaks credential material: %s", body)
	}

	// Addressing the run to one node releases exactly that node's copy.
	addressed := run
	addressed.DesiredRuntimeNodeID = otherNode.ID
	if err := s.controlDB.UpsertRuntimeRun(addressed); err != nil {
		t.Fatalf("address run: %v", err)
	}
	if got := claimAs(rivalNode); got["run"] != nil {
		t.Fatalf("node %s claimed a run addressed to %s: %v", rivalNode.ID, otherNode.ID, got)
	}
	claimed, _ := claimAs(otherNode)["run"].(map[string]any)
	if claimed == nil || claimed["id"] != run.ID {
		t.Fatalf("addressed run was not claimed by its node: %v", claimed)
	}
}
