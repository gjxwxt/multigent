package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// TestManualStartStaleSessionSpawnsWithoutOkPidLie is the H1 closeout API
// regression (run4 finding, stale-side split): with a STALE scheduler
// interaction session (idle beyond the shared two-minute recovery window),
// the /start precheck lets the spawn through and returns ok+pid. The spawned
// `multigent run` acquires the lock as manual_run and — before the H1 fix —
// REFUSED the same stale session the API had admitted, exiting busy right
// after the ok+pid response. The CLI side is covered by
// TestManualRunRecoversStaleSchedulerSession (real acquisition entry); this
// test pins the API side of the same contract:
//   - a stale session must NOT take the attention fallback (no queued status,
//     no fabricated pid) — the start proceeds to the real spawn path;
//   - the ok+pid response must carry the REAL spawned pid (StartManagedCommand
//     actually launched the stub), i.e. the response is honest about spawning;
//   - when the spawn itself fails (stub exits immediately), the heartbeat
//     must not stay pinned to a dead pid in "running" — the manual-run waiter
//     resets it, so a follow-up /start is not falsely refused.
func TestManualStartStaleSessionSpawnsWithoutOkPidLie(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	def := entity.WorkflowDefinition{
		ID: "wf-stale-recovery", Name: "stale recovery", Version: 1, Scope: "workspace", StartStepID: "work",
		Steps: []entity.WorkflowStep{{
			ID: "work", Type: "agent_task", Title: "work", ActorRole: "pm-agent",
		}},
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		t.Fatal(err)
	}
	task := &entity.Task{ID: "task-stale-recovery", Title: "stale recovery", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := wfStore.CompleteAndAdvance("sample", task.ID, "agent died mid-step", "", nil, "failed"); err != nil {
		t.Fatalf("fail the agent step: %v", err)
	}
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID: "aw-pm", WorkspaceID: workspaceID, Name: "pm", DisplayName: "pm",
		Model: "human", Status: "available",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	// A STALE scheduler interaction session (>2min idle): the precheck admits
	// it as a crashed predecessor and the spawn proceeds.
	stale := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	if err := s.controlDB.CreateInteractionSession(controldb.InteractionSession{
		ID: "sess-stale-fixture", WorkspaceID: workspaceID, AgentWorkerID: "aw-pm",
		ProjectID: "sample", AgentID: "pm", SourceKind: "scheduler", SourceChannel: "scheduler",
		ActorType: "system", ActorID: "scheduler", Status: "active",
		LockReason: "running_task", MetadataJSON: "{}",
		CreatedAt: stale, UpdatedAt: stale, LastActivityAt: stale,
	}); err != nil {
		t.Fatal(err)
	}

	// The stub binary exits 0 immediately: it stands in for `multigent run`,
	// whose REAL-lock behavior (recovering the stale session) is covered by
	// the CLI acquisition test. Here the API contract under test is that the
	// stale path reaches the spawn and reports the real pid honestly.
	stub := filepath.Join(t.TempDir(), "stub-multigent-ok")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.sched = newSchedulerManager(t.TempDir())
	s.sched.binPath = stub

	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, "sample", "pm")
	waitHeartbeatClear := func(what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
			if err == nil && hb != nil && hb.PID == 0 && hb.LastWakeupStatus != "running" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("heartbeat must clear after %s, got %+v", what, hb)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task.ID+"/start", "admin", nil)
	req.SetPathValue("name", "sample")
	req.SetPathValue("taskId", task.ID)
	rec := httptest.NewRecorder()
	s.handleStartProjectTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual start over a STALE scheduler session must proceed to spawn, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK           bool   `json:"ok"`
		Status       string `json:"status"`
		PID          int    `json:"pid"`
		RuntimeRunID string `json:"runtimeRunId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status == "queued_via_attention" {
		t.Fatalf("a stale session must not be queued via attention (the CLI recovers it): %s", rec.Body.String())
	}
	if !resp.OK || resp.PID <= 0 || resp.Status != "started" {
		t.Fatalf("stale-path response must be an honest started+pid, got %+v", resp)
	}
	if resp.RuntimeRunID != "" {
		t.Fatalf("local spawn must not fabricate a runtimeRunId, got %q", resp.RuntimeRunID)
	}
	// The stub exited: the managed-command waiter must reap it and clear the
	// heartbeat (PID=0, not running) so the next start is not falsely refused.
	waitHeartbeatClear("the first stub spawn exits")

	// Spawn failure (non-zero exit): the response is still an honest spawn
	// (the pid WAS launched), and the heartbeat must again clear instead of
	// pinning a dead pid in running state.
	stubFail := filepath.Join(t.TempDir(), "stub-multigent-fail")
	if err := os.WriteFile(stubFail, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.sched.binPath = stubFail
	task2 := &entity.Task{ID: "task-stale-recovery-2", Title: "stale recovery 2", Status: entity.TaskStatusInProgress,
		Priority: 2, Assignee: "pm", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.ts.AddTask("sample", "pm", task2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wfStore.StartRun("sample", task2.ID, def.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := wfStore.CompleteAndAdvance("sample", task2.ID, "agent died mid-step", "", nil, "failed"); err != nil {
		t.Fatalf("fail the agent step (2): %v", err)
	}
	req2 := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks/"+task2.ID+"/start", "admin", nil)
	req2.SetPathValue("name", "sample")
	req2.SetPathValue("taskId", task2.ID)
	rec2 := httptest.NewRecorder()
	s.handleStartProjectTask(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second stale-path start must also proceed to spawn, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp2 struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		PID    int    `json:"pid"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode response (2): %v", err)
	}
	if !resp2.OK || resp2.PID <= 0 || resp2.Status != "started" {
		t.Fatalf("failed-spawn response must still be an honest started+pid, got %+v", resp2)
	}
	waitHeartbeatClear("the failing stub spawn exits")
}
