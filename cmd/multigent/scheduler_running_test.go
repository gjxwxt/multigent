package main

import (
	"os"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

// selfPID is a pid that always exists and that this test process may
// signal(0): our own. PID 1 cannot be signalled by unprivileged users on all
// platforms, so fixture pids that must count as "alive" use this helper.
func selfPID() int {
	return os.Getpid()
}

func runningHeartbeat(pid int, started time.Time) *entity.HeartbeatConfig {
	return &entity.HeartbeatConfig{
		PID:              pid,
		LastWakeupStatus: "running",
		RunStartedAt:     &started,
	}
}

// Regression (VM production, 2026-09-29): a resident systemd scheduler owns
// hb.PID for the daemon's whole uptime, so the old PID+Signal(0)-only check
// permanently refused every manual/autostart wakeup with "already running".
// A live scheduler process that is idle must not count as "running".
func TestIsAlreadyRunningResidentSchedulerIdle(t *testing.T) {
	hb := runningHeartbeat(selfPID(), time.Now().Add(-time.Hour))
	hb.RunStartedAt = nil // no in-flight evidence recorded
	// A "running" status with a live PID but no fresh run-start timestamp is a
	// stale flag, not an executing cycle.
	if isAlreadyRunning(hb) {
		t.Fatal("stale running flag without in-flight evidence must not block wakeup")
	}
}

func TestIsAlreadyRunningFreshEvidenceBlocks(t *testing.T) {
	now := time.Now()
	hb := runningHeartbeat(selfPID(), now.Add(-time.Minute))
	if !isAlreadyRunningAt(now, hb) {
		t.Fatal("a cycle with fresh in-flight evidence must report busy")
	}
}

func TestIsAlreadyRunningEvidenceOutsideWindowExpires(t *testing.T) {
	now := time.Now()
	hb := runningHeartbeat(selfPID(), now.Add(-2*time.Hour)) // default window 30m + 15m grace
	if isAlreadyRunningAt(now, hb) {
		t.Fatal("in-flight evidence past the liveness window must expire")
	}
}

func TestIsAlreadyRunningWindowFollowsMaxCycleDuration(t *testing.T) {
	now := time.Now()
	hb := runningHeartbeat(selfPID(), now.Add(-50*time.Minute))
	hb.MaxCycleDuration = "2h"
	if !isAlreadyRunningAt(now, hb) {
		t.Fatal("cycle within its own max_cycle_duration window must report busy")
	}
	hb.MaxCycleDuration = "10m"
	if isAlreadyRunningAt(now, hb) {
		t.Fatal("cycle beyond max_cycle_duration + grace must expire")
	}
}

// Heartbeats written before RunStartedAt existed carry only LastWakeup; the
// fallback must keep genuine in-flight cycles busy while eventually expiring.
func TestIsAlreadyRunningFallsBackToLastWakeup(t *testing.T) {
	now := time.Now()
	hb := runningHeartbeat(selfPID(), now.Add(-time.Minute))
	hb.RunStartedAt = nil
	hb.LastWakeup = &now
	if !isAlreadyRunningAt(now, hb) {
		t.Fatal("legacy heartbeat with fresh LastWakeup must report busy")
	}
	stale := now.Add(-2 * time.Hour)
	hb.LastWakeup = &stale
	if isAlreadyRunningAt(now, hb) {
		t.Fatal("legacy heartbeat with stale LastWakeup must not block")
	}
}

func TestIsAlreadyRunningNonRunningStatusNeverBlocks(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	for _, status := range []string{"done", "failed", "interrupted", ""} {
		hb := &entity.HeartbeatConfig{PID: selfPID(), LastWakeupStatus: status, RunStartedAt: &started}
		if isAlreadyRunningAt(now, hb) {
			t.Fatalf("status %q must never report busy", status)
		}
	}
}

func TestIsAlreadyRunningDeadPIDDoesNotBlock(t *testing.T) {
	// Find a PID that is (almost certainly) not ours and hope it exited; use a
	// pid from a finished subprocess to stay deterministic.
	hb := runningHeartbeat(-1, time.Now().Add(-time.Minute))
	if isAlreadyRunning(hb) {
		t.Fatal("invalid pid must not report busy")
	}
}

func TestClearHeartbeatCycleRunningResetsEvidence(t *testing.T) {
	hb := runningHeartbeat(1234, time.Now())
	clearHeartbeatCycleRunning(hb)
	if hb.PID != 0 || hb.RunStartedAt != nil || hb.LastWakeupStatus != "running" {
		t.Fatalf("clear must drop pid and start stamp but keep the caller's status handling: %+v", hb)
	}
}

func TestMarkHeartbeatCycleRunningStampsEvidence(t *testing.T) {
	now := time.Now()
	hb := &entity.HeartbeatConfig{}
	markHeartbeatCycleRunning(hb, now)
	if hb.PID != os.Getpid() {
		t.Fatalf("pid = %d, want current process pid %d", hb.PID, os.Getpid())
	}
	if hb.LastWakeupStatus != "running" || hb.RunStartedAt == nil || hb.LastWakeup == nil {
		t.Fatalf("running stamp incomplete: %+v", hb)
	}
	if !hb.RunStartedAt.Equal(now.UTC()) || !hb.LastWakeup.Equal(now.UTC()) {
		t.Fatalf("timestamps must be normalised to UTC: %+v", hb.RunStartedAt)
	}
}
