package db

import (
	"database/sql"
	"errors"
	"fmt"
)

type TaskThreadProjection struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspace_id"`
	ProjectID    string `json:"project_id"`
	TaskID       string `json:"task_id"`
	Provider     string `json:"provider"`
	ChannelID    string `json:"channel_id"`
	RootPostID   string `json:"root_post_id"`
	Status       string `json:"status"` // "active" | "closed" | "archived"
	MetadataJSON string `json:"metadata_json"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type TaskThreadProjectionFilter struct {
	WorkspaceID string
	ProjectID   string
	TaskID      string
	Provider    string
	ChannelID   string
	RootPostID  string
	Status      string
	Limit       int
}

func (db *SQLiteStore) UpsertTaskThreadProjection(p TaskThreadProjection) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in UpsertTaskThreadProjection: %v", r)
		}
	}()
	if p.CreatedAt == "" {
		p.CreatedAt = nowUTC()
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = nowUTC()
	}
	if p.Status == "" {
		p.Status = "active"
	}
	if p.MetadataJSON == "" {
		p.MetadataJSON = "{}"
	}

	_, err = db.sql.Exec(`INSERT INTO task_thread_projections (
	id, workspace_id, project_id, task_id, provider, channel_id, root_post_id,
	status, metadata_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	project_id = excluded.project_id,
	task_id = excluded.task_id,
	provider = excluded.provider,
	channel_id = excluded.channel_id,
	root_post_id = excluded.root_post_id,
	status = excluded.status,
	metadata_json = excluded.metadata_json,
	updated_at = excluded.updated_at`,
		p.ID, p.WorkspaceID, p.ProjectID, p.TaskID, p.Provider, p.ChannelID, p.RootPostID,
		p.Status, p.MetadataJSON, p.CreatedAt, p.UpdatedAt)
	return err
}

func (db *SQLiteStore) ActiveTaskThreadProjection(workspaceID, taskID, provider string) (proj TaskThreadProjection, found bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ActiveTaskThreadProjection: %v", r)
		}
	}()
	row := db.sql.QueryRow(`SELECT id, workspace_id, project_id, task_id, provider, channel_id, root_post_id,
status, metadata_json, created_at, updated_at
FROM task_thread_projections
WHERE workspace_id = ? AND task_id = ? AND provider = ? AND status = 'active'`,
		workspaceID, taskID, provider)
	p, err := scanTaskThreadProjection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskThreadProjection{}, false, nil
	}
	if err != nil {
		return TaskThreadProjection{}, false, err
	}
	return p, true, nil
}

func (db *SQLiteStore) TaskThreadProjectionByRoot(provider, channelID, rootPostID string) (proj TaskThreadProjection, found bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in TaskThreadProjectionByRoot: %v", r)
		}
	}()
	row := db.sql.QueryRow(`SELECT id, workspace_id, project_id, task_id, provider, channel_id, root_post_id,
status, metadata_json, created_at, updated_at
FROM task_thread_projections
WHERE provider = ? AND channel_id = ? AND root_post_id = ? AND status = 'active'`,
		provider, channelID, rootPostID)
	p, err := scanTaskThreadProjection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskThreadProjection{}, false, nil
	}
	if err != nil {
		return TaskThreadProjection{}, false, err
	}
	return p, true, nil
}

func (db *SQLiteStore) CloseTaskThreadProjection(workspaceID, taskID, provider string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in CloseTaskThreadProjection: %v", r)
		}
	}()
	now := nowUTC()
	_, err = db.sql.Exec(`UPDATE task_thread_projections
SET status = 'closed', updated_at = ?
WHERE workspace_id = ? AND task_id = ? AND provider = ? AND status = 'active'`,
		now, workspaceID, taskID, provider)
	return err
}

func (db *SQLiteStore) ListTaskThreadProjections(filter TaskThreadProjectionFilter) (list []TaskThreadProjection, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in ListTaskThreadProjections: %v", r)
		}
	}()
	query := `SELECT id, workspace_id, project_id, task_id, provider, channel_id, root_post_id,
status, metadata_json, created_at, updated_at
FROM task_thread_projections WHERE 1=1`
	var args []any
	if filter.WorkspaceID != "" {
		query += " AND workspace_id = ?"
		args = append(args, filter.WorkspaceID)
	}
	if filter.ProjectID != "" {
		query += " AND project_id = ?"
		args = append(args, filter.ProjectID)
	}
	if filter.TaskID != "" {
		query += " AND task_id = ?"
		args = append(args, filter.TaskID)
	}
	if filter.Provider != "" {
		query += " AND provider = ?"
		args = append(args, filter.Provider)
	}
	if filter.ChannelID != "" {
		query += " AND channel_id = ?"
		args = append(args, filter.ChannelID)
	}
	if filter.RootPostID != "" {
		query += " AND root_post_id = ?"
		args = append(args, filter.RootPostID)
	}
	if filter.Status != "" {
		query += " AND status = ?"
		args = append(args, filter.Status)
	}
	query += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TaskThreadProjection
	for rows.Next() {
		p, err := scanTaskThreadProjection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

type taskThreadProjectionScanner interface {
	Scan(dest ...any) error
}

func scanTaskThreadProjection(s taskThreadProjectionScanner) (TaskThreadProjection, error) {
	var p TaskThreadProjection
	err := s.Scan(
		&p.ID,
		&p.WorkspaceID,
		&p.ProjectID,
		&p.TaskID,
		&p.Provider,
		&p.ChannelID,
		&p.RootPostID,
		&p.Status,
		&p.MetadataJSON,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	return p, err
}
