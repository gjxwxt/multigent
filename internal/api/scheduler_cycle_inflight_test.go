package api

import (
	"os"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

// selfPID is a pid that always exists and that this test process may
// signal(0): our own. PID 1 cannot be signalled by unprivileged users on all
// platforms, so fixture pids that must count as "alive" use this helper.
// Same fixture strategy as cmd/multigent/scheduler_running_test.go.
func selfPID() int {
	return os.Getpid()
}

// The t-20261003-thh6tn dispatch deadlock: a resident scheduler's PID is alive
// for the daemon's whole uptime, so busy checks keyed on PID liveness alone
// permanently refused every wakeup once LastWakeupStatus stuck at "running".
// schedulerCycleInFlight must treat the run start timestamp — not the PID —
// as the in-flight evidence, and a "running" flag without fresh evidence must
// read as stale, not busy. Mirrors the CLI-side contract in
// cmd/multigent/scheduler_running_test.go — keep the two in sync.
func TestSchedulerCycleInFlightStaleFlagDoesNotBlock(t *testing.T) {
	hb := &entity.HeartbeatConfig{
		PID:              selfPID(),
		LastWakeupStatus: "running",
		RunStartedAt:     nil, // no in-flight evidence recorded
	}
	if schedulerCycleInFlight(hb) {
		t.Fatal("stale running flag without in-flight evidence must not block wakeup")
	}
}

func TestSchedulerCycleInFlightFreshEvidenceBlocks(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	hb := &entity.HeartbeatConfig{
		PID:              selfPID(),
		LastWakeupStatus: "running",
		RunStartedAt:     &started,
	}
	if !schedulerCycleInFlight(hb) {
		t.Fatal("a cycle with fresh in-flight evidence must report busy")
	}
}

func TestSchedulerCycleInFlightEvidenceOutsideWindowExpires(t *testing.T) {
	started := time.Now().Add(-2 * time.Hour) // default window 30m + 15m grace
	hb := &entity.HeartbeatConfig{
		PID:              selfPID(),
		LastWakeupStatus: "running",
		RunStartedAt:     &started,
	}
	if schedulerCycleInFlight(hb) {
		t.Fatal("in-flight evidence past the liveness window must expire")
	}
}

func TestSchedulerCycleInFlightWindowFollowsMaxCycleDuration(t *testing.T) {
	started := time.Now().Add(-50 * time.Minute)
	hb := &entity.HeartbeatConfig{
		PID:              selfPID(),
		LastWakeupStatus: "running",
		RunStartedAt:     &started,
		MaxCycleDuration: "2h",
	}
	if !schedulerCycleInFlight(hb) {
		t.Fatal("in-flight evidence within the MaxCycleDuration-extended window must report busy")
	}
}

func TestSchedulerCycleInFlightLastWakeupFallback(t *testing.T) {
	// Heartbeats written before RunStartedAt existed carry only LastWakeup;
	// the fallback must supply the in-flight evidence.
	started := time.Now().Add(-time.Minute)
	hb := &entity.HeartbeatConfig{
		PID:              selfPID(),
		LastWakeupStatus: "running",
		LastWakeup:       &started,
	}
	if !schedulerCycleInFlight(hb) {
		t.Fatal("fresh LastWakeup evidence must report busy when RunStartedAt is absent")
	}
}

func TestSchedulerCycleInFlightTerminalAndAbsentStates(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	if schedulerCycleInFlight(nil) {
		t.Fatal("nil heartbeat must not report busy")
	}
	if schedulerCycleInFlight(&entity.HeartbeatConfig{LastWakeupStatus: "running", RunStartedAt: &started}) {
		t.Fatal("zero PID must not report busy (no process to signal)")
	}
	if schedulerCycleInFlight(&entity.HeartbeatConfig{PID: selfPID(), LastWakeupStatus: "done", RunStartedAt: &started}) {
		t.Fatal("done status must not report busy regardless of evidence freshness")
	}
}
