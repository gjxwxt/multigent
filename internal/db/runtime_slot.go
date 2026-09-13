package db

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// Q0 PR-2: Worker-slot occupancy and lease-generation guards.
//
// Slot semantics (plan D1): a run occupies its Worker's execution slot iff it
// is running with an unexpired lease AND its persisted slot_class is not
// "readonly". Queued runs never occupy a slot. slot_class is decided at
// enqueue time and never re-derived.

// SlotClassReadonly marks runs that are exempt from the Worker slot: strict
// read-only fork sessions whose capability set is within the platform-fixed
// read-only category (inspect/log/list classes only). Anything else — and any
// parse failure — is "normal" (fail-closed).
const SlotClassReadonly = "readonly"

// SlotClassNormal is the default slot class: the run occupies the Worker slot.
const SlotClassNormal = "normal"

// NormalizedSlotClass validates a persisted slot_class value. Unknown values
// are treated as normal so a typo can never widen the exemption.
func NormalizedSlotClass(slotClass string) string {
	switch strings.ToLower(strings.TrimSpace(slotClass)) {
	case SlotClassReadonly:
		return SlotClassReadonly
	default:
		return SlotClassNormal
	}
}

// RunOccupiesWorkerSlot reports whether run holds its Worker's single
// execution slot at time now. This is the single source of truth shared by
// claim selection, scheduler-side "is the agent busy" checks, and the reaper.
// Empty lease timestamps (pre-lease legacy rows) are treated as NOT occupying:
// the reaper will expire them and the claim path may take them over.
func RunOccupiesWorkerSlot(run RuntimeRun, now time.Time) bool {
	if NormalizedSlotClass(run.SlotClass) != SlotClassNormal {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(run.Status), "running") {
		return false
	}
	lease := strings.TrimSpace(run.LeaseExpiresAt)
	if lease == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, lease)
	if err != nil {
		return false
	}
	return expiresAt.After(now)
}

// RuntimeRunWorkerKey is the slot identity: agent workers key on "worker/<id>",
// legacy membership rows on "<project>/<agent>". Mirrors the busyAgents key
// format used by runtime nodes.
func RuntimeRunWorkerKey(run RuntimeRun) string {
	if id := strings.TrimSpace(run.AgentWorkerID); id != "" {
		return "worker/" + id
	}
	return strings.TrimSpace(run.ProjectID) + "/" + strings.TrimSpace(run.AgentID)
}

// LeaseExpiredCutoff returns the reaper's lease criterion:
// lease_expires_at < now - grace. The grace period counts from the LEASE
// EXPIRY, not from node disconnection — a healthy node renews every 30s to
// now+90s, so a dead node is typically reaped ≈ lease(90s) + grace(180s) ≈
// 270s after its last renewal. The lease timestamp is the SOLE criterion: the
// renewal loop renews heartbeat and lease in the same tick, so "lease being
// renewed" IS the heartbeat signal; querying node health separately would
// only add a starvation-prone second judge (fail-closed heartbeats would stop
// reaping entirely).
func LeaseExpiredCutoff(grace time.Duration, now time.Time) time.Time {
	return now.Add(-grace)
}

// RuntimeRunLeaseGenerationError is returned by lease-conditioned operations
// when the caller's (node, generation) pair no longer owns the run — the run
// was taken over, finished, or reaped. Callers must abandon silently.
type RuntimeRunLeaseGenerationError struct {
	RunID string
}

func (e *RuntimeRunLeaseGenerationError) Error() string {
	return "runtime run lease generation mismatch: run " + e.RunID + " is no longer owned by the caller"
}

// LeaseGenerationMismatch reports whether err is a lease-generation loss.
func LeaseGenerationMismatch(err error) bool {
	var genErr *RuntimeRunLeaseGenerationError
	return errors.As(err, &genErr)
}

// runtimeRunsHoldingSlots returns the worker keys of all runs currently
// occupying a Worker slot in the workspace. Callers embed this in the claim
// transaction so the slot exclusion is decided from the same snapshot.
func (db *SQLiteStore) runtimeRunsHoldingSlots(tx *sql.Tx, workspaceID string, now time.Time) (map[string]struct{}, error) {
	cutoff := now.Format(time.RFC3339)
	rows, err := tx.Query(`SELECT agent_worker_id, project_id, agent_id, slot_class, lease_expires_at FROM runtime_runs
WHERE workspace_id = ? AND LOWER(status) = 'running' AND slot_class <> ? AND lease_expires_at <> '' AND lease_expires_at > ?`,
		workspaceID, SlotClassReadonly, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var agentWorkerID, projectID, agentID, slotClass, leaseExpiresAt string
		if err := rows.Scan(&agentWorkerID, &projectID, &agentID, &slotClass, &leaseExpiresAt); err != nil {
			return nil, err
		}
		expiresAt, err := time.Parse(time.RFC3339, leaseExpiresAt)
		if err != nil {
			// Unparseable lease: not provably holding a slot; the reaper will
			// expire it. Fail-open here matches RunOccupiesWorkerSlot.
			continue
		}
		if !expiresAt.After(now) {
			continue
		}
		key := RuntimeRunWorkerKey(RuntimeRun{
			AgentWorkerID: agentWorkerID,
			ProjectID:     projectID,
			AgentID:       agentID,
		})
		out[key] = struct{}{}
	}
	return out, rows.Err()
}

// holdingSlotKeys returns the sorted worker keys of the occupied-slot set so
// claim can filter candidates in SQL deterministically.
func holdingSlotKeys(holding map[string]struct{}) []string {
	if len(holding) == 0 {
		return nil
	}
	out := make([]string, 0, len(holding))
	for key := range holding {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// slotKeyPlaceholders renders a valid SQL IN-list for N keys; zero keys yield
// a one-element list containing an impossible value so NOT IN still parses.
func slotKeyPlaceholders(n int) string {
	if n == 0 {
		return "('')"
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}
