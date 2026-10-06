package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Regression (2026-10-03): preplan hardcoded the bare default model name and
// missed the agent's Fusion/* runtime model, routing the draft call down the
// gateway's non-streaming slow path (17/61 failed, hangs past the deadline)
// while every agent run rode the streaming fast path (878/878 ok). The call
// must resolve the model from the agent binding first.

func seedPreplanAgentWithModel(t *testing.T, s *Server, workspaceID, project, name, workerID, runtimeModel string) {
	t.Helper()
	seedAgentWorkerWithIDForTest(t, s, workspaceID, project, name, workerID, "pm-"+workerID)
	if runtimeModel == "" {
		return
	}
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, workerID)
	if err != nil || !ok {
		t.Fatalf("load worker: ok=%v err=%v", ok, err)
	}
	worker.RuntimeModel = runtimeModel
	if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
		t.Fatalf("update runtime model: %v", err)
	}
}

func TestDeployAIProjectAgentModelPrefersRuntimeModel(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedPreplanAgentWithModel(t, s, workspaceID, "sample", "alpha", "aw-preplan-a", "")
	seedPreplanAgentWithModel(t, s, workspaceID, "sample", "beta", "aw-preplan-b", "Fusion/glm-5.3-flash")

	if got := s.deployAIProjectAgentModel("sample"); got != "Fusion/glm-5.3-flash" {
		t.Fatalf("expected first runtime model, got %q", got)
	}
	if got := s.deployAIProjectAgent("sample"); got != "alpha" {
		t.Fatalf("agent pick wrong: %q", got)
	}
}

func TestDeployAIProjectAgentModelEmptyWithoutBindings(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedPreplanAgentWithModel(t, s, workspaceID, "sample", "alpha", "aw-preplan-c", "")
	if got := s.deployAIProjectAgentModel("sample"); got != "" {
		t.Fatalf("expected empty model, got %q", got)
	}
}

// The handler must pass the requested agent through to model resolution: a
// body naming an agent with a runtime model resolves that model, not the
// bare default. Only the unresolved-provider conflict is asserted — the LLM
// call itself is not exercised in unit tests.
func TestHandleDeliveryPreplanAgentFieldAccepted(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedPreplanAgentWithModel(t, s, workspaceID, "sample", "alpha", "aw-preplan-d", "Fusion/glm-5.3-flash")

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/delivery/preplan", "owner", map[string]any{
		"source": "text", "text": "需求", "agent": "alpha",
	})
	req.SetPathValue("name", "sample")
	s.handleDeliveryPreplan(rec, req)
	// No model provider is bound in this fixture, so the handler must stop at
	// the provider-unresolved conflict (409) — proving it got past parsing and
	// agent resolution with the agent field present. A decode/400 failure here
	// would mean the agent field broke request handling.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("agent field must not break request parsing: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 provider-unresolved without bound account, got %d %s", rec.Code, rec.Body.String())
	}
}

// Regression (2026-10-03 23:01, gateway rows 330011/330012): the old
// wall-clock attempt deadline (110s) killed SLOW-BUT-ALIVE streams — the
// upstream emitted 581-726KB of SSE while streaming and was cut mid-flight,
// twice, because a 22k-token draft for a 163KB input needs 110-130s. The
// watchdog now measures PROGRESS (any SSE byte re-arms the timer), not
// duration: a healthy-but-slow stream must complete; a silent one must be
// aborted quickly with the retryable "流停滞" wording.
func TestPreplanStreamingStallWatchdog(t *testing.T) {
	provider := entity.APIProvider{Type: "anthropic", BaseURL: "placeholder", APIKey: "k", Model: "m"}

	t.Run("stalled stream aborts with retryable wording", func(t *testing.T) {
		// Server accepts the request then goes silent forever — no SSE bytes
		// ever, handler only exits when the client disconnects (watchdog).
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			<-r.Context().Done()
		}))
		defer srv.Close()
		provider.BaseURL = srv.URL
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		start := time.Now()
		_, err := preplanAnthropicChatStreaming(ctx, provider, "sys", "hello")
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected stall error, got nil")
		}
		if !strings.Contains(err.Error(), "流停滞") {
			t.Fatalf("expected stall wording, got: %v", err)
		}
		// Watchdog must abort well before the overall ctx; allow slack.
		if elapsed >= preplanStallTimeout+10*time.Second {
			t.Fatalf("watchdog did not fire in time: %v", elapsed)
		}
		if elapsed < preplanStallTimeout {
			t.Fatalf("watchdog fired too early: %v", elapsed)
		}
	})

	t.Run("slow but alive stream completes", func(t *testing.T) {
		// Server emits one SSE byte every 500ms for ~6s — far slower than any
		// observed healthy stream, but never silent for preplanStallTimeout.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 12; i++ {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
				_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"))
				flusher.Flush()
			}
		}))
		defer srv.Close()
		provider.BaseURL = srv.URL
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := preplanAnthropicChatStreaming(ctx, provider, "sys", "hello")
		if err != nil {
			t.Fatalf("slow-but-alive stream must complete, got: %v", err)
		}
		if out != strings.Repeat("x", 12) {
			t.Fatalf("unexpected output: %q", out)
		}
	})
}

// Regression (2026-10-03 23:40): the CreateTaskDialog batch mode requests the
// greenfield pipeline by its FIXED id (greenfield-delivery-pipeline), but
// UI instantiation via POST /workflows saves a random wf-* id — a fresh
// deployment 404s "workflow definition not found" on first large-requirement
// task creation. The fixed-id definition must auto-seed at task-creation
// time, idempotently (user-edited copies at a >= version survive untouched).
func TestEnsureGreenfieldDefinition(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	// 1. Seed from nothing.
	if err := workflowstore.EnsureGreenfieldDefinition(wfStore, "zh-CN"); err != nil {
		t.Fatalf("ensure from scratch: %v", err)
	}
	def, ok, err := wfStore.Definition(workflowstore.GreenfieldDeliveryWorkflowID)
	if err != nil || !ok {
		t.Fatalf("definition missing after ensure: ok=%v err=%v", ok, err)
	}
	if def.ID != workflowstore.GreenfieldDeliveryWorkflowID {
		t.Fatalf("seeded under wrong id: %s", def.ID)
	}
	if len(def.Steps) == 0 {
		t.Fatal("seeded definition has no steps")
	}

	// 2. User-edited copy at a higher version survives a re-ensure.
	def.Version += 5
	def.Name = "用户改过的名字"
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatalf("save edited copy: %v", err)
	}
	if err := workflowstore.EnsureGreenfieldDefinition(wfStore, "zh-CN"); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	after, ok, err := wfStore.Definition(workflowstore.GreenfieldDeliveryWorkflowID)
	if err != nil || !ok {
		t.Fatalf("definition missing after re-ensure: ok=%v err=%v", ok, err)
	}
	if after.Name != "用户改过的名字" {
		t.Fatalf("user-edited definition was overwritten: %s", after.Name)
	}
}

// Regression (2026-10-03, round L): deployments that seeded the greenfield
// pipeline while the template was at v1 carry the stacked canvas layout (all
// y=180, several x gaps under one node width). Bumping the built-in to v2
// must upgrade those stored copies in place on the next ensure — without
// touching a copy that has since been edited to an equal-or-higher version.
func TestEnsureGreenfieldDefinitionUpgradesV1Layout(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	if err := workflowstore.EnsureGreenfieldDefinition(wfStore, "en"); err != nil {
		t.Fatalf("ensure from scratch: %v", err)
	}
	def, ok, err := wfStore.Definition(workflowstore.GreenfieldDeliveryWorkflowID)
	if err != nil || !ok {
		t.Fatalf("definition missing: ok=%v err=%v", ok, err)
	}
	// Simulate a stored v1 copy from an older deployment (stacked layout).
	def.Version = 1
	for i := range def.Steps {
		def.Steps[i].Position = entity.WorkflowPosition{X: 500 + i*30, Y: 180}
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatalf("save v1 copy: %v", err)
	}

	if err := workflowstore.EnsureGreenfieldDefinition(wfStore, "en"); err != nil {
		t.Fatalf("re-ensure over v1: %v", err)
	}
	upgraded, ok, err := wfStore.Definition(workflowstore.GreenfieldDeliveryWorkflowID)
	if err != nil || !ok {
		t.Fatalf("definition missing after upgrade: ok=%v err=%v", ok, err)
	}
	if upgraded.Version < 2 {
		t.Fatalf("stored copy still at version %d — old layout survives", upgraded.Version)
	}
	const nodeWidth = 198
	for i := 1; i < len(upgraded.Steps); i++ {
		for j := 0; j < i; j++ {
			gap := upgraded.Steps[i].Position.X - upgraded.Steps[j].Position.X
			if gap < 0 {
				gap = -gap
			}
			if gap < nodeWidth && upgraded.Steps[i].Position.Y == upgraded.Steps[j].Position.Y {
				t.Fatalf("upgraded copy still stacks %q and %q (x gap %d < %d, same y)", upgraded.Steps[j].ID, upgraded.Steps[i].ID, gap, nodeWidth)
			}
		}
	}
}
