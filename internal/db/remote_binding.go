package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// VerifiedRemoteBinding is the ONLY record external GitLab operations may
// consume. It is written exclusively by server-side flows that hold an
// independent proof:
//
//   - source "platform-create": the create-repository endpoint persisted the
//     GitLab API response right after creating the repo (P0.6-1);
//   - source "explicit-verify": an administrator ran the read-only verify
//     endpoint, which looked the project up live on the forge.
//
// Nothing in the project PUT path can write or alter it. Every consumer
// (runner binding, APP_PORT variable, remote adopt, ci_ready pipeline
// evidence, MR query/merge) resolves its target from here — never from the
// client-writable Project.RemoteConnection/RemoteProjectID display fields.
type VerifiedRemoteBinding struct {
	WorkspaceID       string `json:"workspaceId"`
	ProjectID         string `json:"projectId"`
	Provider          string `json:"provider"`
	ConnectionID      string `json:"connectionId"`
	RemoteProjectID   string `json:"remoteProjectId"`
	PathWithNamespace string `json:"pathWithNamespace,omitempty"`
	VerifiedAt        string `json:"verifiedAt"`
	Source            string `json:"source"`
	CreatedAt         string `json:"createdAt,omitempty"`
	UpdatedAt         string `json:"updatedAt,omitempty"`
}

// VerifiedRemoteBindingSource values.
const (
	BindingSourcePlatformCreate = "platform-create"
	BindingSourceExplicitVerify = "explicit-verify"
)

// UpsertVerifiedRemoteBinding writes the binding row for (workspace, project).
// One binding per project: a re-verify overwrites the previous row.
func (db *SQLiteStore) UpsertVerifiedRemoteBinding(b VerifiedRemoteBinding) error {
	b.WorkspaceID = strings.TrimSpace(b.WorkspaceID)
	b.ProjectID = strings.TrimSpace(b.ProjectID)
	b.Provider = strings.ToLower(strings.TrimSpace(b.Provider))
	b.ConnectionID = strings.TrimSpace(b.ConnectionID)
	b.RemoteProjectID = strings.TrimSpace(b.RemoteProjectID)
	if b.WorkspaceID == "" || b.ProjectID == "" || b.Provider == "" || b.ConnectionID == "" || b.RemoteProjectID == "" {
		return fmt.Errorf("verified remote binding requires workspace, project, provider, connection and remote project id")
	}
	switch b.Source {
	case BindingSourcePlatformCreate, BindingSourceExplicitVerify:
	default:
		return fmt.Errorf("verified remote binding source must be %q or %q", BindingSourcePlatformCreate, BindingSourceExplicitVerify)
	}
	if b.CreatedAt == "" {
		b.CreatedAt = nowUTC()
	}
	if b.UpdatedAt == "" {
		b.UpdatedAt = b.CreatedAt
	}
	_, err := db.sql.Exec(`INSERT INTO verified_remote_bindings
		(workspace_id, project_id, provider, connection_id, remote_project_id, path_with_namespace, verified_at, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, project_id) DO UPDATE SET
		provider = excluded.provider,
		connection_id = excluded.connection_id,
		remote_project_id = excluded.remote_project_id,
		path_with_namespace = excluded.path_with_namespace,
		verified_at = excluded.verified_at,
		source = excluded.source,
		updated_at = excluded.updated_at`,
		b.WorkspaceID, b.ProjectID, b.Provider, b.ConnectionID, b.RemoteProjectID, b.PathWithNamespace, b.VerifiedAt, b.Source, b.CreatedAt, b.UpdatedAt)
	return err
}

// VerifiedRemoteBindingFor returns the project's verified binding, or
// (nil, false, nil) when the project has none — callers must treat "no
// binding" as fail-closed for every external remote operation.
func (db *SQLiteStore) VerifiedRemoteBindingFor(workspaceID, projectID string) (*VerifiedRemoteBinding, bool, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	projectID = strings.TrimSpace(projectID)
	if workspaceID == "" || projectID == "" {
		return nil, false, nil
	}
	row := db.sql.QueryRow(`SELECT provider, connection_id, remote_project_id, path_with_namespace, verified_at, source
		FROM verified_remote_bindings WHERE workspace_id = ? AND project_id = ?`, workspaceID, projectID)
	var b VerifiedRemoteBinding
	var source string
	b.WorkspaceID = workspaceID
	b.ProjectID = projectID
	if err := row.Scan(&b.Provider, &b.ConnectionID, &b.RemoteProjectID, &b.PathWithNamespace, &b.VerifiedAt, &source); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	b.Source = source
	return &b, true, nil
}

// DeleteVerifiedRemoteBinding removes the project's binding (project delete).
func (db *SQLiteStore) DeleteVerifiedRemoteBinding(workspaceID, projectID string) error {
	_, err := db.sql.Exec(`DELETE FROM verified_remote_bindings WHERE workspace_id = ? AND project_id = ?`,
		strings.TrimSpace(workspaceID), strings.TrimSpace(projectID))
	return err
}
