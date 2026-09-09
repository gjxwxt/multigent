package db

import (
	"database/sql"
	"errors"
	"strings"
)

func (db *SQLiteStore) UpsertProjectChannelLink(l ProjectChannelLink) error {
	if l.CreatedAt == "" {
		l.CreatedAt = nowUTC()
	}
	if l.UpdatedAt == "" {
		l.UpdatedAt = nowUTC()
	}
	if l.Status == "" {
		l.Status = "active"
	}
	if l.MetadataJSON == "" {
		l.MetadataJSON = "{}"
	}
	if l.Visibility == "" {
		l.Visibility = "private"
	}

	_, err := db.sql.Exec(`INSERT INTO project_channel_links (
	id, workspace_id, project_id, provider, im_instance_id, team_id,
	channel_id, channel_name, display_name, visibility, status, metadata_json,
	created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, project_id, provider, im_instance_id) DO UPDATE SET
	team_id = excluded.team_id,
	channel_id = excluded.channel_id,
	channel_name = excluded.channel_name,
	display_name = excluded.display_name,
	visibility = excluded.visibility,
	status = excluded.status,
	metadata_json = excluded.metadata_json,
	updated_at = excluded.updated_at
ON CONFLICT(id) DO UPDATE SET
	workspace_id = excluded.workspace_id,
	project_id = excluded.project_id,
	provider = excluded.provider,
	im_instance_id = excluded.im_instance_id,
	team_id = excluded.team_id,
	channel_id = excluded.channel_id,
	channel_name = excluded.channel_name,
	display_name = excluded.display_name,
	visibility = excluded.visibility,
	status = excluded.status,
	metadata_json = excluded.metadata_json,
	updated_at = excluded.updated_at`,
		l.ID, l.WorkspaceID, l.ProjectID, l.Provider, l.IMInstanceID, l.TeamID,
		l.ChannelID, l.ChannelName, l.DisplayName, l.Visibility, l.Status, l.MetadataJSON,
		l.CreatedBy, l.CreatedAt, l.UpdatedAt)
	return err
}

func (db *SQLiteStore) GetProjectChannelLink(workspaceID, projectID, provider, imInstanceID string) (ProjectChannelLink, bool, error) {
	query := `SELECT id, workspace_id, project_id, provider, im_instance_id, team_id,
channel_id, channel_name, display_name, visibility, status, metadata_json,
created_by, created_at, updated_at
FROM project_channel_links
WHERE workspace_id = ? AND project_id = ? AND provider = ?`
	args := []any{workspaceID, projectID, provider}

	if strings.TrimSpace(imInstanceID) != "" {
		query += " AND im_instance_id = ?"
		args = append(args, strings.TrimSpace(imInstanceID))
	}
	query += " ORDER BY created_at DESC LIMIT 1"

	var l ProjectChannelLink
	err := db.sql.QueryRow(query, args...).Scan(
		&l.ID, &l.WorkspaceID, &l.ProjectID, &l.Provider, &l.IMInstanceID, &l.TeamID,
		&l.ChannelID, &l.ChannelName, &l.DisplayName, &l.Visibility, &l.Status, &l.MetadataJSON,
		&l.CreatedBy, &l.CreatedAt, &l.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectChannelLink{}, false, nil
		}
		return ProjectChannelLink{}, false, err
	}
	return l, true, nil
}

func (db *SQLiteStore) ListProjectChannelLinks(workspaceID, projectID string) ([]ProjectChannelLink, error) {
	rows, err := db.sql.Query(`SELECT id, workspace_id, project_id, provider, im_instance_id, team_id,
channel_id, channel_name, display_name, visibility, status, metadata_json,
created_by, created_at, updated_at
FROM project_channel_links
WHERE workspace_id = ? AND project_id = ?
ORDER BY created_at ASC`, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []ProjectChannelLink
	for rows.Next() {
		var l ProjectChannelLink
		if err := rows.Scan(
			&l.ID, &l.WorkspaceID, &l.ProjectID, &l.Provider, &l.IMInstanceID, &l.TeamID,
			&l.ChannelID, &l.ChannelName, &l.DisplayName, &l.Visibility, &l.Status, &l.MetadataJSON,
			&l.CreatedBy, &l.CreatedAt, &l.UpdatedAt,
		); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func (db *SQLiteStore) DeleteProjectChannelLinks(workspaceID, projectID string) error {
	trimmedProjectID := strings.TrimSpace(projectID)
	if trimmedProjectID == "" {
		return nil
	}
	trimmedWorkspaceID := strings.TrimSpace(workspaceID)
	if trimmedWorkspaceID == "" {
		_, err := db.sql.Exec(`DELETE FROM project_channel_links WHERE project_id = ?`, trimmedProjectID)
		return err
	}
	_, err := db.sql.Exec(`DELETE FROM project_channel_links WHERE workspace_id = ? AND project_id = ?`, trimmedWorkspaceID, trimmedProjectID)
	return err
}

