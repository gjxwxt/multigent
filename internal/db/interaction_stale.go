package db

import (
	"strings"
	"time"
)

// ShouldRecoverStaleInteraction reports whether a new interaction acquisition
// (scheduler or manual task run) may take over an existing active session of
// the same shape. Both the control-plane API and the CLI (`multigent run`)
// must answer this identically — a split decision lets the API start an agent
// run the CLI then refuses (the run4 finding: /start returned ok while the
// spawned process exited with "agent is busy in scheduler session"), or the
// reverse. The rule: an active scheduler running_task session idle for more
// than two minutes is treated as a crashed predecessor and may be recovered
// by any same-ladder acquirer — another scheduler cycle OR a manual task run
// (both go through the identical heartbeat-PID + interaction-lock ladder the
// API's manual-start precheck models). Restricting recovery to scheduler
// requesters only re-created the run4 split on the STALE side: the API
// precheck admitted the stale session, the spawned manual_run refused it and
// exited busy after the ok+pid response had already lied.
func ShouldRecoverStaleInteraction(active InteractionSession, sourceKind, reason string) bool {
	requester := strings.TrimSpace(sourceKind)
	if requester != "scheduler" && requester != "manual_run" {
		return false
	}
	if strings.TrimSpace(reason) != "running_task" {
		return false
	}
	if strings.TrimSpace(active.SourceKind) != "scheduler" || strings.TrimSpace(active.LockReason) != "running_task" {
		return false
	}
	lastRaw := strings.TrimSpace(active.LastActivityAt)
	if lastRaw == "" {
		lastRaw = strings.TrimSpace(active.UpdatedAt)
	}
	last, err := time.Parse(time.RFC3339, lastRaw)
	if err != nil {
		return false
	}
	return time.Since(last) > 2*time.Minute
}
