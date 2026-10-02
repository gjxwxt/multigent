package api

import (
	"testing"

	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func TestParseDeliveryPreplanReplyValidBatched(t *testing.T) {
	raw := "前置说明\n```json\n" + `{
		"scaleHint": "batched",
		"reason": "三个内聚域",
		"plan": {
			"schemaVersion": 1,
			"planId": "preplan-demo",
			"version": 1,
			"requirementItems": [{"id": "req-1", "text": "注册", "source": "§3.1"}],
			"workPackages": [
				{"id": "wp-0", "title": "契约", "acceptanceCriteria": ["infra:contract"], "agentBinding": "a"},
				{"id": "wp-1", "title": "域A", "acceptanceCriteria": ["req-1"], "agentBinding": "a", "dependsOn": ["wp-0"]}
			]
		}
	}` + "\n```"
	resp := parseDeliveryPreplanReply(raw)
	if resp.PlanError != "" {
		t.Fatalf("unexpected plan error: %s", resp.PlanError)
	}
	if resp.ScaleHint != "batched" {
		t.Fatalf("scaleHint = %q, want batched", resp.ScaleHint)
	}
	if resp.Plan == nil || len(resp.Plan.WorkPackages) != 2 {
		t.Fatalf("plan/workpackages missing")
	}
	// Identity normalization: schemaVersion pinned, runID cleared.
	if resp.Plan.SchemaVersion != workflowstore.DeliveryPlanSchemaVersion {
		t.Fatalf("schemaVersion = %d", resp.Plan.SchemaVersion)
	}
	if resp.Plan.RunID != "" {
		t.Fatalf("RunID must be cleared on drafts, got %q", resp.Plan.RunID)
	}
	// AgentBinding stripped — the editor owns it.
	for _, wp := range resp.Plan.WorkPackages {
		if wp.AgentBinding != "" {
			t.Fatalf("wp %s: AgentBinding must be stripped on drafts", wp.ID)
		}
	}
}

func TestParseDeliveryPreplanReplyInvalidPlanStillReturned(t *testing.T) {
	raw := `{"scaleHint": "batched", "plan": {"schemaVersion": 1, "planId": "p", "version": 1, "workPackages": [{"id": "wp-1", "title": "x"}]}}`
	resp := parseDeliveryPreplanReply(raw)
	if resp.Plan == nil {
		t.Fatal("plan must still be returned for editing")
	}
	if resp.PlanError == "" {
		t.Fatal("empty acceptanceCriteria must surface PlanError")
	}
}

func TestParseDeliveryPreplanReplyGarbageFailsOpen(t *testing.T) {
	resp := parseDeliveryPreplanReply("这不是 JSON")
	if resp.ScaleHint != "linear" {
		t.Fatalf("default scaleHint must be linear, got %q", resp.ScaleHint)
	}
	if resp.Plan != nil || resp.PlanError == "" {
		t.Fatalf("garbage reply: plan=%v err=%q", resp.Plan, resp.PlanError)
	}
}

func TestParseDeliveryPreplanReplyHintFallback(t *testing.T) {
	resp := parseDeliveryPreplanReply(`{"scaleHint": "HUGE", "reason": "x"}`)
	if resp.ScaleHint != "linear" {
		t.Fatalf("unknown hint must fall back to linear, got %q", resp.ScaleHint)
	}
}
