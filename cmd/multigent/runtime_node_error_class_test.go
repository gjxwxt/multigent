package main

// Regression coverage for the GPT-review P1: the worker loop treated EVERY
// error from runtimeNodeLoopOnce as a console outage — a run whose agent
// failed (already reported to the console via /fail) put the worker into
// backoff, logged a bogus "reconnected after outage" line, and fired a
// duplicate re-register. Execution results are business outcomes, not
// outages. Also: outage state must be node-level — with concurrency > 1 a
// single outage must produce ONE reconnect log and ONE re-register, not N.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runtimeexec"
)

// agentMetaForExecuteRunTest builds an agent meta whose run command is a
// shell one-liner that fails deterministically, so the executor fails
// without spawning any model CLI.
func agentMetaForExecuteRunTest(runCommand string) entity.AgentMeta {
	return entity.AgentMeta{
		Name:       "agent",
		Project:    "proj",
		Model:      entity.ModelClaudeCode,
		RunCommand: runCommand,
	}
}

func TestReportedExecutionErrorClassification(t *testing.T) {
	wrapped := wrapRuntimeNodeReportedError(errors.New("agent run failed: model offline"))
	if !isRuntimeNodeReportedExecutionError(wrapped) {
		t.Fatal("wrapped reported error must classify as reported-execution")
	}
	if isRuntimeNodeReportedExecutionError(errors.New("connection refused")) {
		t.Fatal("raw transport error must NOT classify as reported-execution")
	}
	if isRuntimeNodeReportedExecutionError(nil) {
		t.Fatal("nil must not classify")
	}
	// Unwrapping preserves the original message for logs.
	if !strings.Contains(wrapped.Error(), "agent run failed: model offline") {
		t.Fatalf("wrapped error lost the original message: %v", wrapped)
	}
}

func TestTransportErrorClassification(t *testing.T) {
	if !isRuntimeNodeTransportError(errors.New("dial tcp 127.0.0.1:1: connect: connection refused")) {
		t.Fatal("connection refused must classify as transport")
	}
	if !isRuntimeNodeTransportError(errors.New("Get \"http://x\": context deadline exceeded")) {
		t.Fatal("deadline exceeded must classify as transport")
	}
	// A decoded console response (even a 5xx error body) proves the control
	// plane is up — not a transport error.
	if isRuntimeNodeTransportError(errors.New("runtime node API /x returned HTTP 503: down")) {
		t.Fatal("HTTP-status errors must NOT classify as transport (console is up)")
	}
	if isRuntimeNodeTransportError(nil) {
		t.Fatal("nil must not classify")
	}
}

// An agent run that fails AFTER the console accepted the failure report must
// come back classified as a reported execution error — not as an outage.
func TestRuntimeNodeExecuteRunClassifiesReportedFailure(t *testing.T) {
	var failReports atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runtime-node/runs/run-1/spec", func(w http.ResponseWriter, r *http.Request) {
		spec := runtimeexec.Spec{
			Kind:        runtimeexec.KindExecPrompt,
			WorkspaceID: "ws-test",
			ProjectID:   "proj",
			AgentID:     "agent",
			Prompt:      "trigger an executor failure",
			Agent:       agentMetaForExecuteRunTest("exit 7"),
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"id": "run-1", "status": "running"}, "spec": spec})
	})
	mux.HandleFunc("/api/v1/runtime-node/runs/run-1/fail", func(w http.ResponseWriter, r *http.Request) {
		failReports.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := runtimeNodeConfig{ServerURL: server.URL, Token: "test-token"}
	run := runtimeNodeRun{ID: "run-1", LeaseGeneration: 1}
	err := runtimeNodeExecuteRun(cfg, run, 1, false)
	if err == nil {
		t.Fatal("executor failure must return an error")
	}
	if failReports.Load() != 1 {
		t.Fatalf("failure must be reported to the console exactly once, got %d", failReports.Load())
	}
	if !isRuntimeNodeReportedExecutionError(err) {
		t.Fatalf("console-acknowledged run failure must classify as reported-execution, got: %v", err)
	}
}

// If the failure report itself is rejected (console unreachable), the error
// must NOT be classified as reported-execution — it is an outage.
func TestRuntimeNodeExecuteRunRejectedFailureReportIsOutage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runtime-node/runs/run-2/spec", func(w http.ResponseWriter, r *http.Request) {
		spec := runtimeexec.Spec{
			Kind:        runtimeexec.KindExecPrompt,
			WorkspaceID: "ws-test",
			ProjectID:   "proj",
			AgentID:     "agent",
			Prompt:      "trigger an executor failure",
			Agent:       agentMetaForExecuteRunTest("exit 7"),
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"id": "run-2", "status": "running"}, "spec": spec})
	})
	mux.HandleFunc("/api/v1/runtime-node/runs/run-2/fail", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := runtimeNodeConfig{ServerURL: server.URL, Token: "test-token"}
	run := runtimeNodeRun{ID: "run-2", LeaseGeneration: 1}
	err := runtimeNodeExecuteRun(cfg, run, 1, false)
	if err == nil {
		t.Fatal("rejected failure report must return an error")
	}
	if isRuntimeNodeReportedExecutionError(err) {
		t.Fatalf("console-rejected failure must stay an outage error, got: %v", err)
	}
}

// Node-level outage state: with concurrency 3, one outage must yield exactly
// one re-register (the per-worker state of the old code fired N).
func TestRuntimeNodeRunWorkersConcurrentSingleReregister(t *testing.T) {
	var registrations atomic.Int64
	var heartbeats atomic.Int64
	consoleUp := atomic.Bool{}
	consoleUp.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runtime-node/register", func(w http.ResponseWriter, r *http.Request) {
		registrations.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"node": map[string]any{"id": "rtn_test"}, "status": "registered"})
	})
	mux.HandleFunc("/api/v1/runtime-node/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !consoleUp.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		heartbeats.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "serverTime": time.Now().UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("/api/v1/runtime-node/runs/claim", func(w http.ResponseWriter, r *http.Request) {
		if !consoleUp.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": nil, "retryAfterMs": 0})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	restoreCaps := swapRuntimeNodeCapabilitiesProbe(func() map[string]any { return map[string]any{"os": "test"} })
	defer restoreCaps()

	cfg := runtimeNodeConfig{ServerURL: server.URL, Token: "test-token"}
	poll := 20 * time.Millisecond
	go func() {
		_ = runtimeNodeRunWorkers(cfg, 3, poll, newRuntimeNodeCoordinator(), false)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for heartbeats.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if heartbeats.Load() < 3 {
		t.Fatalf("workers never reached healthy heartbeat phase: registrations=%d heartbeats=%d", registrations.Load(), heartbeats.Load())
	}

	consoleUp.Store(false)
	time.Sleep(60 * time.Millisecond)
	consoleUp.Store(true)

	// The worker loop never registers at startup (the start command does
	// that), so every register observed here is a post-outage re-register.
	deadline = time.Now().Add(5 * time.Second)
	for registrations.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let duplicate re-registers (if buggy) surface
	if got := registrations.Load(); got != 1 {
		t.Fatalf("one outage with 3 workers must re-register exactly once, got %d", got)
	}
}
