package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (db *SQLiteStore) UpsertRuntimeNode(node RuntimeNode) error {
	if node.CreatedAt == "" {
		node.CreatedAt = nowUTC()
	}
	if node.UpdatedAt == "" {
		node.UpdatedAt = nowUTC()
	}
	if node.CapabilitiesJSON == "" {
		node.CapabilitiesJSON = "{}"
	}
	if node.PolicyJSON == "" {
		node.PolicyJSON = "{}"
	}
	if node.Status == "" {
		node.Status = "pending"
	}
	_, err := db.sql.Exec(`INSERT INTO runtime_nodes (
	id, workspace_id, name, kind, status, os, arch, hostname, version,
	capabilities_json, policy_json, last_seen_at, last_error, created_by_user_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name = excluded.name,
	kind = excluded.kind,
	status = excluded.status,
	os = excluded.os,
	arch = excluded.arch,
	hostname = excluded.hostname,
	version = excluded.version,
	capabilities_json = excluded.capabilities_json,
	policy_json = excluded.policy_json,
	last_seen_at = excluded.last_seen_at,
	last_error = excluded.last_error,
	updated_at = excluded.updated_at`,
		node.ID, node.WorkspaceID, node.Name, node.Kind, node.Status, node.OS, node.Arch, node.Hostname, node.Version,
		defaultJSONObject(node.CapabilitiesJSON), defaultJSONObject(node.PolicyJSON), node.LastSeenAt, node.LastError,
		node.CreatedByUserID, node.CreatedAt, node.UpdatedAt)
	return err
}

func (db *SQLiteStore) RuntimeNodeByID(workspaceID, id string) (RuntimeNode, bool, error) {
	row := db.sql.QueryRow(`SELECT id, workspace_id, name, kind, status, os, arch, hostname, version,
capabilities_json, policy_json, last_seen_at, last_error, created_by_user_id, created_at, updated_at
FROM runtime_nodes WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	node, err := scanRuntimeNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeNode{}, false, nil
	}
	if err != nil {
		return RuntimeNode{}, false, err
	}
	return node, true, nil
}

func (db *SQLiteStore) ListRuntimeNodes(workspaceID string) ([]RuntimeNode, error) {
	rows, err := db.sql.Query(`SELECT id, workspace_id, name, kind, status, os, arch, hostname, version,
capabilities_json, policy_json, last_seen_at, last_error, created_by_user_id, created_at, updated_at
FROM runtime_nodes WHERE workspace_id = ? ORDER BY name ASC, id ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeNode{}
	for rows.Next() {
		node, err := scanRuntimeNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) DeleteRuntimeNode(workspaceID, id string) error {
	_, err := db.sql.Exec(`DELETE FROM runtime_nodes WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	return err
}

func (db *SQLiteStore) CreateRuntimeNodeToken(token RuntimeNodeToken) error {
	if token.CreatedAt == "" {
		token.CreatedAt = nowUTC()
	}
	if token.ScopesJSON == "" {
		token.ScopesJSON = "[]"
	}
	_, err := db.sql.Exec(`INSERT INTO runtime_node_tokens (
	id, workspace_id, runtime_node_id, token_hash, name, scopes_json, expires_at, revoked_at, created_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.ID, token.WorkspaceID, token.RuntimeNodeID, token.TokenHash, token.Name, defaultJSONArray(token.ScopesJSON),
		token.ExpiresAt, token.RevokedAt, token.CreatedBy, token.CreatedAt)
	return err
}

func (db *SQLiteStore) UpdateRuntimeNodeToken(token RuntimeNodeToken) error {
	_, err := db.sql.Exec(`UPDATE runtime_node_tokens SET
	workspace_id = ?,
	runtime_node_id = ?,
	name = ?,
	scopes_json = ?,
	expires_at = ?,
	revoked_at = ?
WHERE id = ?`,
		token.WorkspaceID, token.RuntimeNodeID, token.Name, defaultJSONArray(token.ScopesJSON),
		token.ExpiresAt, token.RevokedAt, token.ID)
	return err
}

func (db *SQLiteStore) RuntimeNodeTokenByHash(hash string) (RuntimeNodeToken, bool, error) {
	row := db.sql.QueryRow(`SELECT id, workspace_id, runtime_node_id, token_hash, name, scopes_json, expires_at, revoked_at, created_by, created_at
FROM runtime_node_tokens WHERE token_hash = ?`, hash)
	token, err := scanRuntimeNodeToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeNodeToken{}, false, nil
	}
	if err != nil {
		return RuntimeNodeToken{}, false, err
	}
	return token, true, nil
}

func (db *SQLiteStore) RevokeRuntimeNodeToken(id string) error {
	_, err := db.sql.Exec(`UPDATE runtime_node_tokens SET revoked_at = ? WHERE id = ?`, nowUTC(), id)
	return err
}

func (db *SQLiteStore) UpsertRuntimeRun(run RuntimeRun) error {
	if run.CreatedAt == "" {
		run.CreatedAt = nowUTC()
	}
	if run.UpdatedAt == "" {
		run.UpdatedAt = nowUTC()
	}
	if run.SpecJSON == "" {
		run.SpecJSON = "{}"
	}
	if run.ResultJSON == "" {
		run.ResultJSON = "{}"
	}
	if run.Status == "" {
		run.Status = "queued"
	}
	_, err := db.sql.Exec(`INSERT INTO runtime_runs (
	id, workspace_id, agent_worker_id, project_membership_id, project_id, agent_id, task_id, workflow_instance_id, workflow_step_id, fork_session_id, desired_runtime_node_id, runtime_node_id,
	status, priority, spec_json, result_json, lease_expires_at, claimed_at, started_at, finished_at,
	error_code, error_message, created_at, updated_at, run_key, slot_class, lease_generation
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	agent_worker_id = excluded.agent_worker_id,
	project_membership_id = excluded.project_membership_id,
	fork_session_id = excluded.fork_session_id,
	desired_runtime_node_id = excluded.desired_runtime_node_id,
	runtime_node_id = excluded.runtime_node_id,
	status = excluded.status,
	priority = excluded.priority,
	spec_json = excluded.spec_json,
	result_json = excluded.result_json,
	lease_expires_at = excluded.lease_expires_at,
	claimed_at = excluded.claimed_at,
	started_at = excluded.started_at,
	finished_at = excluded.finished_at,
	error_code = excluded.error_code,
	error_message = excluded.error_message,
	updated_at = excluded.updated_at,
	run_key = excluded.run_key,
	slot_class = excluded.slot_class`,
		run.ID, run.WorkspaceID, run.AgentWorkerID, run.ProjectMembershipID, run.ProjectID, run.AgentID, run.TaskID, run.WorkflowInstanceID, run.WorkflowStepID, run.ForkSessionID, run.DesiredRuntimeNodeID, run.RuntimeNodeID,
		run.Status, run.Priority, defaultJSONObject(run.SpecJSON), defaultJSONObject(run.ResultJSON), run.LeaseExpiresAt, run.ClaimedAt,
		run.StartedAt, run.FinishedAt, run.ErrorCode, run.ErrorMessage, run.CreatedAt, run.UpdatedAt,
		run.RunKey, defaultSlotClass(run.SlotClass), run.LeaseGeneration)
	return err
}

// UpsertRuntimeRunIdempotent inserts a run; when the partial unique index on
// (workspace_id, run_key) rejects a second ACTIVE run with the same key it
// returns the already-existing active run instead of an error. Empty RunKey
// rows bypass the unique index entirely (legacy rows, fork sessions, exec
// prompts without a caller key) and behave exactly like UpsertRuntimeRun.
func (db *SQLiteStore) UpsertRuntimeRunIdempotent(run RuntimeRun) (RuntimeRun, bool, error) {
	if strings.TrimSpace(run.RunKey) == "" {
		if err := db.UpsertRuntimeRun(run); err != nil {
			return RuntimeRun{}, false, err
		}
		return run, true, nil
	}
	if err := db.UpsertRuntimeRun(run); err != nil {
		// The unique index only fires on a duplicate ACTIVE (queued/running)
		// row with the same (workspace_id, run_key) — surface the winner.
		if isUniqueConstraintErr(err) {
			existing, found, getErr := db.ActiveRuntimeRunByKey(run.WorkspaceID, run.RunKey)
			if getErr == nil && found {
				return existing, false, nil
			}
		}
		return RuntimeRun{}, false, err
	}
	return run, true, nil
}

// ActiveRuntimeRunByKey returns the single active (queued/running) run for a
// run key, if any.
func (db *SQLiteStore) ActiveRuntimeRunByKey(workspaceID, runKey string) (RuntimeRun, bool, error) {
	runKey = strings.TrimSpace(runKey)
	if runKey == "" {
		return RuntimeRun{}, false, nil
	}
	row := db.sql.QueryRow(runtimeRunSelectSQL()+` WHERE workspace_id = ? AND run_key = ? AND status IN ('queued','running') ORDER BY created_at ASC, id ASC LIMIT 1`, workspaceID, runKey)
	run, err := scanRuntimeRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeRun{}, false, nil
	}
	if err != nil {
		return RuntimeRun{}, false, err
	}
	return run, true, nil
}

// isUniqueConstraintErr reports whether err is a SQLite unique-constraint
// violation (the modernc driver surfaces the message text; there is no typed
// error code exported through database/sql).
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

func defaultSlotClass(slotClass string) string {
	if strings.TrimSpace(slotClass) == "" {
		return "normal"
	}
	return slotClass
}

func (db *SQLiteStore) RuntimeRunByID(workspaceID, id string) (RuntimeRun, bool, error) {
	row := db.sql.QueryRow(runtimeRunSelectSQL()+` WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	run, err := scanRuntimeRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeRun{}, false, nil
	}
	if err != nil {
		return RuntimeRun{}, false, err
	}
	return run, true, nil
}

func (db *SQLiteStore) ListRuntimeRuns(filter RuntimeRunFilter) ([]RuntimeRun, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := runtimeRunSelectSQL() + ` WHERE workspace_id = ?`
	args := []any{filter.WorkspaceID}
	if strings.TrimSpace(filter.RuntimeNodeID) != "" {
		query += ` AND runtime_node_id = ?`
		args = append(args, strings.TrimSpace(filter.RuntimeNodeID))
	}
	if strings.TrimSpace(filter.AgentWorkerID) != "" {
		query += ` AND agent_worker_id = ?`
		args = append(args, strings.TrimSpace(filter.AgentWorkerID))
	}
	if strings.TrimSpace(filter.ProjectMembershipID) != "" {
		query += ` AND project_membership_id = ?`
		args = append(args, strings.TrimSpace(filter.ProjectMembershipID))
	}
	if strings.TrimSpace(filter.ProjectID) != "" {
		query += ` AND project_id = ?`
		args = append(args, strings.TrimSpace(filter.ProjectID))
	}
	if strings.TrimSpace(filter.AgentID) != "" {
		query += ` AND agent_id = ?`
		args = append(args, strings.TrimSpace(filter.AgentID))
	}
	if strings.TrimSpace(filter.TaskID) != "" {
		query += ` AND task_id = ?`
		args = append(args, strings.TrimSpace(filter.TaskID))
	}
	if strings.TrimSpace(filter.ForkSessionID) != "" {
		query += ` AND fork_session_id = ?`
		args = append(args, strings.TrimSpace(filter.ForkSessionID))
	}
	if strings.TrimSpace(filter.Status) != "" {
		query += ` AND status = ?`
		args = append(args, strings.TrimSpace(filter.Status))
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeRun{}
	for rows.Next() {
		run, err := scanRuntimeRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) ClaimRuntimeRun(workspaceID, nodeID string, leaseSeconds int, busyAgents []string) (RuntimeRun, bool, error) {
	if leaseSeconds <= 0 {
		leaseSeconds = 60
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return RuntimeRun{}, false, err
	}
	defer tx.Rollback()

	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	// Slot exclusion is evaluated in the same transaction as the claim so a
	// concurrent claim of the same worker's other run cannot slip through
	// between the occupancy check and the UPDATE (Q0 D1: one slot per Worker;
	// queued runs never occupy a slot — only running + unexpired lease do).
	holdingSlots, err := db.runtimeRunsHoldingSlots(tx, workspaceID, nowTime)
	if err != nil {
		return RuntimeRun{}, false, err
	}
	query := runtimeRunSelectSQL() + ` WHERE workspace_id = ? AND (desired_runtime_node_id = '' OR desired_runtime_node_id = ?) AND (
	status = 'queued'
	OR (status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at != '' AND lease_expires_at < ?)
)
`
	args := []any{workspaceID, nodeID, now}
	if len(busyAgents) > 0 {
		placeholders := make([]string, 0, len(busyAgents))
		for _, agent := range busyAgents {
			agent = strings.TrimSpace(agent)
			if agent == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, agent)
		}
		if len(placeholders) > 0 {
			query += ` AND (
	fork_session_id != ''
	OR
	(CASE WHEN agent_worker_id != '' THEN 'worker/' || agent_worker_id ELSE project_id || '/' || agent_id END) NOT IN (` + strings.Join(placeholders, ",") + `)
)
`
		}
	}
	query += `ORDER BY priority ASC, created_at ASC, id ASC LIMIT 1`
	row := tx.QueryRow(query, args...)
	run, err := scanRuntimeRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeRun{}, false, nil
	}
	if err != nil {
		return RuntimeRun{}, false, err
	}
	if _, occupied := holdingSlots[RuntimeRunWorkerKey(run)]; occupied {
		// The candidate's Worker already holds its slot with a live run.
		// Queued rows behind it must wait; expired-lease rows will be reaped
		// or re-claimed after the holder finishes.
		return RuntimeRun{}, false, nil
	}
	lease := nowTime.Add(time.Duration(leaseSeconds) * time.Second).Format(time.RFC3339)
	// Generation bumps on every claim/takeover. All later renew/finish/reap
	// updates are conditional on (node, generation), so a stale loop from a
	// previous claim can never resurrect the run.
	res, err := tx.Exec(`UPDATE runtime_runs SET status = 'running', runtime_node_id = ?, claimed_at = ?, started_at = ?, lease_expires_at = ?, lease_generation = lease_generation + 1, updated_at = ?
WHERE workspace_id = ? AND id = ? AND (desired_runtime_node_id = '' OR desired_runtime_node_id = ?) AND (
	status = 'queued'
	OR (status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at != '' AND lease_expires_at < ?)
)`, nodeID, now, now, lease, now, workspaceID, run.ID, nodeID, now)
	if err != nil {
		return RuntimeRun{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return RuntimeRun{}, false, err
	}
	if affected == 0 {
		return RuntimeRun{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return RuntimeRun{}, false, err
	}
	run.Status = "running"
	run.RuntimeNodeID = nodeID
	run.ClaimedAt = now
	run.StartedAt = now
	run.LeaseExpiresAt = lease
	run.LeaseGeneration++
	run.UpdatedAt = now
	return run, true, nil
}

// ExtendRuntimeRunLease is the legacy generationless renewal used by the
// Store interface during migration; it renews only when the caller still owns
// the run (same node, still running). New callers must use
// ExtendRuntimeRunLeaseWithGeneration — generation-conditioned renewal is the
// Q0 contract; generationless renewal cannot detect takeover.
func (db *SQLiteStore) ExtendRuntimeRunLease(workspaceID, runID, nodeID string, leaseSeconds int) (RuntimeRun, bool, error) {
	if leaseSeconds <= 0 {
		leaseSeconds = 60
	}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	lease := nowTime.Add(time.Duration(leaseSeconds) * time.Second).Format(time.RFC3339)
	res, err := db.sql.Exec(`UPDATE runtime_runs SET lease_expires_at = ?, updated_at = ?
WHERE workspace_id = ? AND id = ? AND runtime_node_id = ? AND status = 'running'`,
		lease, now, workspaceID, runID, nodeID)
	if err != nil {
		return RuntimeRun{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return RuntimeRun{}, false, err
	}
	if affected == 0 {
		return RuntimeRun{}, false, nil
	}
	run, found, err := db.RuntimeRunByID(workspaceID, runID)
	if err != nil || !found {
		return RuntimeRun{}, false, err
	}
	return run, true, nil
}

// ExtendRuntimeRunLeaseWithGeneration renews the lease only when the caller
// still owns the run: same node AND same lease generation. A mismatch (run
// taken over, finished, or reaped) returns a RuntimeRunLeaseGenerationError —
// stale renewal loops abandon silently. leaseGeneration <= 0 is rejected
// fail-closed: pre-Q0 nodes do not know their generation and are not allowed
// to renew (mixed-version operation is unsupported; deployment requires
// draining before upgrade).
func (db *SQLiteStore) ExtendRuntimeRunLeaseWithGeneration(workspaceID, runID, nodeID string, leaseGeneration, leaseSeconds int) (RuntimeRun, bool, error) {
	return db.extendRuntimeRunLease(workspaceID, runID, nodeID, leaseGeneration, leaseSeconds)
}

func (db *SQLiteStore) extendRuntimeRunLease(workspaceID, runID, nodeID string, leaseGeneration int, leaseSeconds int) (RuntimeRun, bool, error) {
	if leaseGeneration <= 0 {
		return RuntimeRun{}, false, &RuntimeRunLeaseGenerationError{RunID: runID}
	}
	if leaseSeconds <= 0 {
		leaseSeconds = 60
	}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	lease := nowTime.Add(time.Duration(leaseSeconds) * time.Second).Format(time.RFC3339)
	res, err := db.sql.Exec(`UPDATE runtime_runs SET lease_expires_at = ?, updated_at = ?
WHERE workspace_id = ? AND id = ? AND runtime_node_id = ? AND lease_generation = ? AND status = 'running'`,
		lease, now, workspaceID, runID, nodeID, leaseGeneration)
	if err != nil {
		return RuntimeRun{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return RuntimeRun{}, false, err
	}
	if affected == 0 {
		return RuntimeRun{}, false, &RuntimeRunLeaseGenerationError{RunID: runID}
	}
	run, found, err := db.RuntimeRunByID(workspaceID, runID)
	if err != nil || !found {
		return RuntimeRun{}, false, err
	}
	return run, true, nil
}

// FinishRuntimeRun transitions a running run to a terminal status, conditional
// on the caller still owning it (node + generation). On success it returns the
// final run; on ownership loss it returns a RuntimeRunLeaseGenerationError and
// leaves the run untouched. Terminal runs release their run_key and slot.
func (db *SQLiteStore) FinishRuntimeRun(workspaceID, runID, nodeID string, leaseGeneration int, status string, errorCode, errorMessage, resultJSON string) (RuntimeRun, bool, error) {
	if leaseGeneration <= 0 {
		return RuntimeRun{}, false, &RuntimeRunLeaseGenerationError{RunID: runID}
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "succeeded", "failed", "cancelled", "canceled":
	default:
		return RuntimeRun{}, false, nil
	}
	if strings.TrimSpace(resultJSON) == "" {
		resultJSON = "{}"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.sql.Exec(`UPDATE runtime_runs SET status = ?, error_code = ?, error_message = ?, result_json = ?, finished_at = ?, updated_at = ?
WHERE workspace_id = ? AND id = ? AND runtime_node_id = ? AND lease_generation = ? AND status = 'running'`,
		status, strings.TrimSpace(errorCode), strings.TrimSpace(errorMessage), resultJSON, now, now,
		workspaceID, runID, nodeID, leaseGeneration)
	if err != nil {
		return RuntimeRun{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return RuntimeRun{}, false, err
	}
	if affected == 0 {
		return RuntimeRun{}, false, &RuntimeRunLeaseGenerationError{RunID: runID}
	}
	run, found, err := db.RuntimeRunByID(workspaceID, runID)
	if err != nil || !found {
		return RuntimeRun{}, false, err
	}
	return run, true, nil
}

// ReapExpiredRuntimeRun force-fails a running run whose lease has been stale
// for longer than the grace window. The where-clause re-checks the lease
// timestamp so a run renewed (or finished, or taken over) between the SELECT
// and this UPDATE is left alone; ownership passes to nobody — the generation
// bump invalidates every outstanding (node, generation) pair.
func (db *SQLiteStore) ReapExpiredRuntimeRun(workspaceID, runID string, leaseGeneration int, cutoff time.Time) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.sql.Exec(`UPDATE runtime_runs SET status = 'failed', error_code = 'lease_expired', error_message = ?, lease_generation = lease_generation + 1, finished_at = ?, updated_at = ?
WHERE workspace_id = ? AND id = ? AND status = 'running' AND lease_generation = ? AND lease_expires_at IS NOT NULL AND lease_expires_at != '' AND lease_expires_at < ?`,
		"lease expired; run reaped by control plane", now, now, workspaceID, runID, leaseGeneration, cutoff.Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ListExpiredRunningRuns returns running runs whose lease expired before the
// cutoff — the reaper's sole criterion (plan 2.2-1: no node-health join; the
// lease being renewed IS the heartbeat).
func (db *SQLiteStore) ListExpiredRunningRuns(workspaceID string, cutoff time.Time, limit int) ([]RuntimeRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.sql.Query(runtimeRunSelectSQL()+` WHERE workspace_id = ? AND status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at != '' AND lease_expires_at < ? ORDER BY lease_expires_at ASC LIMIT ?`,
		workspaceID, cutoff.Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeRun{}
	for rows.Next() {
		run, err := scanRuntimeRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) CreateRuntimeEvent(event RuntimeEvent) error {
	if event.CreatedAt == "" {
		event.CreatedAt = nowUTC()
	}
	if event.PayloadJSON == "" {
		event.PayloadJSON = "{}"
	}
	_, err := db.sql.Exec(`INSERT INTO runtime_events (
	id, workspace_id, run_id, sequence, type, payload_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.WorkspaceID, event.RunID, event.Sequence, event.Type, defaultJSONObject(event.PayloadJSON), event.CreatedAt)
	return err
}

func (db *SQLiteStore) ListRuntimeEvents(workspaceID, runID string, limit int) ([]RuntimeEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := db.sql.Query(`SELECT id, workspace_id, run_id, sequence, type, payload_json, created_at
FROM runtime_events WHERE workspace_id = ? AND run_id = ? ORDER BY sequence ASC LIMIT ?`, workspaceID, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RuntimeEvent{}
	for rows.Next() {
		var ev RuntimeEvent
		if err := rows.Scan(&ev.ID, &ev.WorkspaceID, &ev.RunID, &ev.Sequence, &ev.Type, &ev.PayloadJSON, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

type runtimeNodeScanner interface {
	Scan(dest ...any) error
}

func scanRuntimeNode(row runtimeNodeScanner) (RuntimeNode, error) {
	var node RuntimeNode
	err := row.Scan(&node.ID, &node.WorkspaceID, &node.Name, &node.Kind, &node.Status, &node.OS, &node.Arch, &node.Hostname, &node.Version,
		&node.CapabilitiesJSON, &node.PolicyJSON, &node.LastSeenAt, &node.LastError, &node.CreatedByUserID, &node.CreatedAt, &node.UpdatedAt)
	return node, err
}

func scanRuntimeNodeToken(row runtimeNodeScanner) (RuntimeNodeToken, error) {
	var token RuntimeNodeToken
	err := row.Scan(&token.ID, &token.WorkspaceID, &token.RuntimeNodeID, &token.TokenHash, &token.Name, &token.ScopesJSON,
		&token.ExpiresAt, &token.RevokedAt, &token.CreatedBy, &token.CreatedAt)
	return token, err
}

func runtimeRunSelectSQL() string {
	return `SELECT id, workspace_id, agent_worker_id, project_membership_id, project_id, agent_id, task_id, workflow_instance_id, workflow_step_id, fork_session_id, desired_runtime_node_id, runtime_node_id,
status, priority, spec_json, result_json, lease_expires_at, claimed_at, started_at, finished_at,
error_code, error_message, created_at, updated_at, run_key, slot_class, lease_generation FROM runtime_runs`
}

func scanRuntimeRun(row runtimeNodeScanner) (RuntimeRun, error) {
	var run RuntimeRun
	err := row.Scan(&run.ID, &run.WorkspaceID, &run.AgentWorkerID, &run.ProjectMembershipID, &run.ProjectID, &run.AgentID, &run.TaskID, &run.WorkflowInstanceID, &run.WorkflowStepID,
		&run.ForkSessionID, &run.DesiredRuntimeNodeID, &run.RuntimeNodeID, &run.Status, &run.Priority, &run.SpecJSON, &run.ResultJSON, &run.LeaseExpiresAt, &run.ClaimedAt,
		&run.StartedAt, &run.FinishedAt, &run.ErrorCode, &run.ErrorMessage, &run.CreatedAt, &run.UpdatedAt, &run.RunKey, &run.SlotClass, &run.LeaseGeneration)
	return run, err
}

func defaultJSONObject(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

func defaultJSONArray(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "[]"
	}
	return raw
}

func addSecondsRFC3339(seconds int) string {
	return time.Now().UTC().Add(time.Duration(seconds) * time.Second).Format(time.RFC3339)
}
