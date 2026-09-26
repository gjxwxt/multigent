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
// reverse. The rule: two same-shape scheduler sessions, the older one idle
// for more than two minutes, is treated as a crashed predecessor.
func ShouldRecoverStaleInteraction(active InteractionSession, sourceKind, reason string) bool {
	if strings.TrimSpace(sourceKind) != "scheduler" || strings.TrimSpace(reason) != "running_task" {
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
