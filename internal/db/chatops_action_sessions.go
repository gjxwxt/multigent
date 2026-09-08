package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ChatopsActionSession struct {
	ID                   string    `json:"id"`
	WorkspaceID          string    `json:"workspaceId"`
	Project              string    `json:"project"`
	TaskID               string    `json:"taskId"`
	StepID               string    `json:"stepId"`
	ExpectedStateVersion int64     `json:"expectedStateVersion"`
	ReviewSnapshotHash   string    `json:"reviewSnapshotHash"`
	ActionType           string    `json:"actionType"` // approve | override | review_approve | reject
	ActorMMUserID        string    `json:"actorMmUserId"`
	ActorPlatformUserID  string    `json:"actorPlatformUserId"`
	State                string    `json:"state"` // issued | dialog_opening | dialog_opened | processing | completed | canceled | expired | stale | failed
	ActionNonce          string    `json:"actionNonce"`
	TokenHash            string    `json:"tokenHash"`
	ResolutionTraceJSON  string    `json:"resolutionTraceJson,omitempty"`
	ResolvedOutputsJSON  string    `json:"resolvedOutputsJson,omitempty"`
	ExpiresAt            time.Time `json:"expiresAt"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

func generateActionSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (db *SQLiteStore) CreateChatopsActionSession(s *ChatopsActionSession) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in CreateChatopsActionSession: %v", r)
		}
	}()
	if s == nil {
		return errors.New("chatops action session is nil")
	}
	s.ID = strings.TrimSpace(s.ID)
	if s.ID == "" {
		s.ID = "cas-" + generateActionSessionID()
	}
	s.WorkspaceID = strings.TrimSpace(s.WorkspaceID)
	if s.WorkspaceID == "" {
		return errors.New("workspace_id is required")
	}
	s.TaskID = strings.TrimSpace(s.TaskID)
	if s.TaskID == "" {
		return errors.New("task_id is required")
	}
	s.StepID = strings.TrimSpace(s.StepID)
	if s.StepID == "" {
		return errors.New("step_id is required")
	}
	s.ActionNonce = strings.TrimSpace(s.ActionNonce)
	if s.ActionNonce == "" {
		return errors.New("action_nonce is required")
	}
	s.TokenHash = strings.TrimSpace(s.TokenHash)
	if s.TokenHash == "" {
		return errors.New("token_hash is required")
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = now.Add(10 * time.Minute)
	}

	query := `INSERT INTO chatops_action_sessions (
		id, workspace_id, project, task_id, step_id,
		expected_state_version, review_snapshot_hash, action_type,
		actor_mm_user_id, actor_platform_user_id, state,
		action_nonce, token_hash, resolution_trace_json, resolved_outputs_json,
		expires_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err = db.sql.Exec(query,
		s.ID, s.WorkspaceID, s.Project, s.TaskID, s.StepID,
		s.ExpectedStateVersion, s.ReviewSnapshotHash, s.ActionType,
		s.ActorMMUserID, s.ActorPlatformUserID, s.State,
		s.ActionNonce, s.TokenHash,
		s.ResolutionTraceJSON, s.ResolvedOutputsJSON,
		s.ExpiresAt.Format(time.RFC3339Nano),
		s.CreatedAt.Format(time.RFC3339Nano),
		s.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("create chatops action session: %w", err)
	}
	return nil
}

func (db *SQLiteStore) ChatopsActionSessionByID(workspaceID, id string) (session *ChatopsActionSession, found bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ChatopsActionSessionByID: %v", r)
		}
	}()
	query := `SELECT id, workspace_id, project, task_id, step_id,
		expected_state_version, review_snapshot_hash, action_type,
		actor_mm_user_id, actor_platform_user_id, state,
		action_nonce, token_hash, resolution_trace_json, resolved_outputs_json,
		expires_at, created_at, updated_at
		FROM chatops_action_sessions WHERE workspace_id = ? AND id = ?`

	row := db.sql.QueryRow(query, workspaceID, id)
	var s ChatopsActionSession
	var expStr, crtStr, updStr string
	err = row.Scan(
		&s.ID, &s.WorkspaceID, &s.Project, &s.TaskID, &s.StepID,
		&s.ExpectedStateVersion, &s.ReviewSnapshotHash, &s.ActionType,
		&s.ActorMMUserID, &s.ActorPlatformUserID, &s.State,
		&s.ActionNonce, &s.TokenHash,
		&s.ResolutionTraceJSON, &s.ResolvedOutputsJSON,
		&expStr, &crtStr, &updStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query chatops action session by id: %w", err)
	}
	s.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expStr)
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, crtStr)
	s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updStr)
	return &s, true, nil
}

func (db *SQLiteStore) ChatopsActionSessionByNonce(workspaceID, nonce string) (session *ChatopsActionSession, found bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ChatopsActionSessionByNonce: %v", r)
		}
	}()
	query := `SELECT id, workspace_id, project, task_id, step_id,
		expected_state_version, review_snapshot_hash, action_type,
		actor_mm_user_id, actor_platform_user_id, state,
		action_nonce, token_hash, resolution_trace_json, resolved_outputs_json,
		expires_at, created_at, updated_at
		FROM chatops_action_sessions WHERE workspace_id = ? AND action_nonce = ?`

	row := db.sql.QueryRow(query, workspaceID, nonce)
	var s ChatopsActionSession
	var expStr, crtStr, updStr string
	err = row.Scan(
		&s.ID, &s.WorkspaceID, &s.Project, &s.TaskID, &s.StepID,
		&s.ExpectedStateVersion, &s.ReviewSnapshotHash, &s.ActionType,
		&s.ActorMMUserID, &s.ActorPlatformUserID, &s.State,
		&s.ActionNonce, &s.TokenHash,
		&s.ResolutionTraceJSON, &s.ResolvedOutputsJSON,
		&expStr, &crtStr, &updStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query chatops action session by nonce: %w", err)
	}
	s.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expStr)
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, crtStr)
	s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updStr)
	return &s, true, nil
}

// ClaimChatopsActionSessionForProcessing atomically changes state from issued or dialog_opened to processing.
func (db *SQLiteStore) ClaimChatopsActionSessionForProcessing(workspaceID, id string) (claimed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ClaimChatopsActionSessionForProcessing: %v", r)
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE chatops_action_sessions 
		SET state = 'processing', updated_at = ? 
		WHERE workspace_id = ? AND id = ? AND state IN ('issued', 'dialog_opened')`

	res, err := db.sql.Exec(query, now, workspaceID, id)
	if err != nil {
		return false, fmt.Errorf("claim chatops action session: %w", err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// CompleteChatopsActionSession marks the session as completed and persists resolution trace and outputs.
func (db *SQLiteStore) CompleteChatopsActionSession(workspaceID, id, resolutionTraceJSON, resolvedOutputsJSON string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in CompleteChatopsActionSession: %v", r)
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE chatops_action_sessions 
		SET state = 'completed', resolution_trace_json = ?, resolved_outputs_json = ?, updated_at = ? 
		WHERE workspace_id = ? AND id = ? AND state = 'processing'`

	res, err := db.sql.Exec(query, resolutionTraceJSON, resolvedOutputsJSON, now, workspaceID, id)
	if err != nil {
		return fmt.Errorf("complete chatops action session: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return errors.New("chatops action session not in processing state or not found")
	}
	return nil
}

// UpdateChatopsActionSessionState transitions the session to a new state.
func (db *SQLiteStore) UpdateChatopsActionSessionState(workspaceID, id, state string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in UpdateChatopsActionSessionState: %v", r)
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE chatops_action_sessions 
		SET state = ?, updated_at = ? 
		WHERE workspace_id = ? AND id = ?`

	res, err := db.sql.Exec(query, state, now, workspaceID, id)
	if err != nil {
		return fmt.Errorf("update chatops action session state: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return errors.New("chatops action session not found")
	}
	return nil
}

// ExpireStaleChatopsActionSessions marks expired pending sessions as 'expired'.
func (db *SQLiteStore) ExpireStaleChatopsActionSessions(workspaceID string, now time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ExpireStaleChatopsActionSessions: %v", r)
		}
	}()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	query := `UPDATE chatops_action_sessions 
		SET state = 'expired', updated_at = ? 
		WHERE workspace_id = ? AND expires_at < ? AND state IN ('issued', 'dialog_opening', 'dialog_opened')`

	_, err = db.sql.Exec(query, nowStr, workspaceID, nowStr)
	if err != nil {
		return fmt.Errorf("expire stale chatops action sessions: %w", err)
	}
	return nil
}
