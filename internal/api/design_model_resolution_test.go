package api

import (
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// The design-gate model must resolve through configured layers (agent
// runtime model → bound model account's default) before falling back to the
// built-in constant, so gateway migrations never require a code change.
func TestDesignResolvedModelLayers(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)

	// Layer 3 source: a model account with a configured default model.
	if err := s.controlDB.UpsertModelProvider(workspaceID, controldb.ModelProvider{
		ID:          "prov-test-default",
		WorkspaceID: workspaceID,
		Name:        "gw-test",
		Type:        "anthropic",
		BaseURL:     "http://gw.test/v1",
		Model:       "Fusion/glm-5.3-flash",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// Agent A: runtime model set → wins over the provider default.
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:                    "aw-model-a",
		WorkspaceID:           workspaceID,
		Name:                  "Model A",
		DisplayName:           "Model A",
		Model:                 "claudecode",
		RuntimeModel:          "Fusion/deepseek-v4-flash",
		DefaultModelAccountID: "prov-test-default",
		Status:                "available",
		CreatedAt:             now,
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("seed worker A: %v", err)
	}
	// Agent B: no runtime model → falls through to the provider default.
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:                    "aw-model-b",
		WorkspaceID:           workspaceID,
		Name:                  "Model B",
		DisplayName:           "Model B",
		Model:                 "claudecode",
		DefaultModelAccountID: "prov-test-default",
		Status:                "available",
		CreatedAt:             now,
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("seed worker B: %v", err)
	}
	for _, m := range []struct{ worker, membership string }{
		{"aw-model-a", "pm-model-a"},
		{"aw-model-b", "pm-model-b"},
	} {
		if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
			ID:          m.membership,
			WorkspaceID: workspaceID,
			ProjectID:   "sample",
			MemberType:  "agent_worker",
			MemberID:    m.worker,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("seed membership %s: %v", m.membership, err)
		}
	}

	// Explicit request model wins over everything.
	if got := s.designResolvedModel("sample", "Model A", "custom/model"); got != "custom/model" {
		t.Fatalf("explicit model: got %q", got)
	}
	// Layer 2: agent runtime model.
	if got := s.designResolvedModel("sample", "Model A", ""); got != "Fusion/deepseek-v4-flash" {
		t.Fatalf("runtime model: got %q", got)
	}
	// Layer 3: bound account's configured default.
	if got := s.designResolvedModel("sample", "Model B", ""); got != "Fusion/glm-5.3-flash" {
		t.Fatalf("provider default: got %q", got)
	}
	// No agent at all → empty (caller falls back to odDefaultModel).
	if got := s.designResolvedModel("sample", "nobody", ""); got != "" {
		t.Fatalf("unbound agent: got %q, want empty", got)
	}
	// designModelOrDefault wraps the fallback.
	if got := s.designModelOrDefault("sample", "nobody", ""); got != odDefaultModel {
		t.Fatalf("default fallback: got %q", got)
	}
}
