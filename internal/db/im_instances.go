package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (db *SQLiteStore) UpsertIMInstance(instance IMInstance) error {
	if instance.CreatedAt == "" {
		instance.CreatedAt = nowUTC()
	}
	if instance.UpdatedAt == "" {
		instance.UpdatedAt = nowUTC()
	}
	if instance.Attestation == "" {
		instance.Attestation = "admin_attested"
	}
	_, err := db.sql.Exec(`INSERT INTO im_instances (
id, workspace_id, provider, display_name, attestation, created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
display_name = excluded.display_name,
attestation = excluded.attestation,
updated_at = excluded.updated_at`,
		instance.ID, instance.WorkspaceID, instance.Provider, instance.DisplayName, instance.Attestation,
		instance.CreatedBy, instance.CreatedAt, instance.UpdatedAt)
	return err
}

func (db *SQLiteStore) IMInstanceByID(id string) (IMInstance, bool, error) {
	row := db.sql.QueryRow(`SELECT id, workspace_id, provider, display_name, attestation, created_by, created_at, updated_at
FROM im_instances WHERE id = ?`, strings.TrimSpace(id))
	instance, err := scanIMInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return IMInstance{}, false, nil
	}
	if err != nil {
		return IMInstance{}, false, err
	}
	return instance, true, nil
}

func (db *SQLiteStore) ListIMInstances(filter IMInstanceFilter) ([]IMInstance, error) {
	query := `SELECT id, workspace_id, provider, display_name, attestation, created_by, created_at, updated_at
FROM im_instances WHERE 1=1`
	args := make([]any, 0, 2)
	if workspaceID := strings.TrimSpace(filter.WorkspaceID); workspaceID != "" {
		query += ` AND workspace_id = ?`
		args = append(args, workspaceID)
	}
	if provider := strings.TrimSpace(filter.Provider); provider != "" {
		query += ` AND provider = ?`
		args = append(args, provider)
	}
	query += ` ORDER BY provider ASC, display_name ASC, created_at ASC`
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	instances := make([]IMInstance, 0)
	for rows.Next() {
		instance, err := scanIMInstance(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, rows.Err()
}

func (db *SQLiteStore) SetConnectionIMInstance(workspaceID, instanceID, connectionID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	instanceID = strings.TrimSpace(instanceID)
	connectionID = strings.TrimSpace(connectionID)
	if workspaceID == "" || instanceID == "" || connectionID == "" {
		return errors.New("workspaceID, instanceID, and connectionID are required")
	}
	result, err := db.sql.Exec(`UPDATE connections SET im_instance_id = ?, updated_at = ?
WHERE id = ? AND workspace_id = ?`, instanceID, nowUTC(), connectionID, workspaceID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err == nil && changed != 1 {
		return errors.New("connection not found")
	}
	return nil
}

func (db *SQLiteStore) ClearConnectionIMInstance(workspaceID, instanceID, connectionID string) error {
	result, err := db.sql.Exec(`UPDATE connections SET im_instance_id = '', updated_at = ?
WHERE id = ? AND workspace_id = ? AND im_instance_id = ?`, nowUTC(), strings.TrimSpace(connectionID), strings.TrimSpace(workspaceID), strings.TrimSpace(instanceID))
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err == nil && changed != 1 {
		return errors.New("connection is not attached to this IM instance")
	}
	return nil
}

func (db *SQLiteStore) DeleteIMInstance(workspaceID, id string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	id = strings.TrimSpace(id)
	if workspaceID == "" || id == "" {
		return errors.New("workspaceID and id are required")
	}
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM connections WHERE workspace_id = ? AND im_instance_id = ?`, workspaceID, id).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("IM instance still has %d connection(s)", count)
	}
	result, err := db.sql.Exec(`DELETE FROM im_instances WHERE id = ? AND workspace_id = ?`, id, workspaceID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err == nil && changed != 1 {
		return errors.New("IM instance not found")
	}
	return nil
}

type imInstanceScanner interface {
	Scan(dest ...any) error
}

func scanIMInstance(row imInstanceScanner) (IMInstance, error) {
	var instance IMInstance
	err := row.Scan(&instance.ID, &instance.WorkspaceID, &instance.Provider, &instance.DisplayName,
		&instance.Attestation, &instance.CreatedBy, &instance.CreatedAt, &instance.UpdatedAt)
	return instance, err
}
