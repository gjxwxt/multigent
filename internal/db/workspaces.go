package db

import (
	"database/sql"
	"errors"
	"fmt"
)

func (db *SQLiteStore) UpsertWorkspace(w Workspace) error {
	if w.CreatedAt == "" {
		w.CreatedAt = nowUTC()
	}
	_, err := db.sql.Exec(`INSERT INTO workspaces (
	id, name, slug, description, root, created_by, created_at, updated_at, last_opened_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name = excluded.name,
	slug = excluded.slug,
	description = excluded.description,
	root = excluded.root,
	created_by = CASE WHEN excluded.created_by != '' THEN excluded.created_by ELSE workspaces.created_by END,
	created_at = CASE WHEN excluded.created_at != '' THEN excluded.created_at ELSE workspaces.created_at END,
	updated_at = excluded.updated_at,
	last_opened_at = CASE WHEN excluded.last_opened_at != '' THEN excluded.last_opened_at ELSE workspaces.last_opened_at END`,
		w.ID, w.Name, w.Slug, w.Description, w.Root, w.CreatedBy, w.CreatedAt, w.UpdatedAt, w.LastOpenedAt)
	return err
}

func (db *SQLiteStore) ListWorkspaces() ([]Workspace, error) {
	rows, err := db.sql.Query(`SELECT id, name, slug, description, root, created_by, created_at, updated_at, last_opened_at
FROM workspaces ORDER BY COALESCE(NULLIF(last_opened_at, ''), created_at) DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Root, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.LastOpenedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) WorkspaceByID(id string) (Workspace, bool, error) {
	var w Workspace
	err := db.sql.QueryRow(`SELECT id, name, slug, description, root, created_by, created_at, updated_at, last_opened_at
FROM workspaces WHERE id = ?`, id).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Root, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.LastOpenedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, err
	}
	return w, true, nil
}

func (db *SQLiteStore) MarkWorkspaceOpened(id string) error {
	_, err := db.sql.Exec(`UPDATE workspaces SET last_opened_at = ? WHERE id = ?`, nowUTC(), id)
	return err
}

func (db *SQLiteStore) DeleteWorkspace(id string) error {
	_, err := db.sql.Exec(`DELETE FROM workspaces WHERE id = ?`, id)
	return err
}

func (db *SQLiteStore) UpsertUser(u User) error {
	if u.CreatedAt == "" {
		u.CreatedAt = nowUTC()
	}
	disabled := 0
	if u.Disabled {
		disabled = 1
	}
	_, err := db.sql.Exec(`INSERT INTO users (
	username, email, display_name, role, avatar, phone, bio, password_hash, disabled, created_at, projects_json, linked_agents_json, worker_grants_json
) VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(username) DO UPDATE SET
	email = NULLIF(excluded.email, ''),
	display_name = excluded.display_name,
	role = excluded.role,
	avatar = excluded.avatar,
	phone = excluded.phone,
	bio = excluded.bio,
	password_hash = excluded.password_hash,
	disabled = excluded.disabled,
	projects_json = excluded.projects_json,
	linked_agents_json = excluded.linked_agents_json,
	worker_grants_json = excluded.worker_grants_json`,
		u.Username, u.Email, u.DisplayName, u.Role, u.Avatar, u.Phone, u.Bio, u.PasswordHash, disabled, u.CreatedAt, defaultJSON(u.ProjectsJSON), defaultJSON(u.LinkedJSON), defaultJSON(u.WorkerGrantsJSON))
	return err
}

func (db *SQLiteStore) ListUsers() ([]User, error) {
	rows, err := db.sql.Query(`SELECT username, COALESCE(email, ''), display_name, role, avatar, phone, bio, password_hash, disabled, created_at, projects_json, linked_agents_json, worker_grants_json FROM users ORDER BY username ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) UserByUsername(username string) (User, bool, error) {
	row := db.sql.QueryRow(`SELECT username, COALESCE(email, ''), display_name, role, avatar, phone, bio, password_hash, disabled, created_at, projects_json, linked_agents_json, worker_grants_json FROM users WHERE username = ?`, username)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (db *SQLiteStore) UserByLogin(login string) (User, bool, error) {
	row := db.sql.QueryRow(`SELECT username, COALESCE(email, ''), display_name, role, avatar, phone, bio, password_hash, disabled, created_at, projects_json, linked_agents_json, worker_grants_json FROM users WHERE lower(username) = lower(?) OR lower(email) = lower(?)`, login, login)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (db *SQLiteStore) DeleteUser(username string) error {
	_, err := db.sql.Exec(`DELETE FROM users WHERE username = ?`, username)
	return err
}

func (db *SQLiteStore) UpsertWorkspaceMember(workspaceID, username, role string) error {
	_, err := db.sql.Exec(`INSERT INTO workspace_members (workspace_id, username, role, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(workspace_id, username) DO UPDATE SET role = excluded.role`,
		workspaceID, username, role, nowUTC())
	return err
}

func (db *SQLiteStore) WorkspaceMember(workspaceID, username string) (WorkspaceMember, bool, error) {
	var m WorkspaceMember
	err := db.sql.QueryRow(`SELECT workspace_id, username, role, created_at
FROM workspace_members WHERE workspace_id = ? AND username = ?`, workspaceID, username).
		Scan(&m.WorkspaceID, &m.Username, &m.Role, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceMember{}, false, nil
	}
	if err != nil {
		return WorkspaceMember{}, false, err
	}
	return m, true, nil
}

func (db *SQLiteStore) ListWorkspaceMembers(workspaceID string) ([]WorkspaceMember, error) {
	rows, err := db.sql.Query(`SELECT workspace_id, username, role, created_at
FROM workspace_members WHERE workspace_id = ? ORDER BY created_at ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WorkspaceMember, 0)
	for rows.Next() {
		var m WorkspaceMember
		if err := rows.Scan(&m.WorkspaceID, &m.Username, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) ListWorkspaceMembersForUser(username string) ([]WorkspaceMember, error) {
	rows, err := db.sql.Query(`SELECT workspace_id, username, role, created_at
FROM workspace_members WHERE username = ? ORDER BY created_at ASC`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WorkspaceMember, 0)
	for rows.Next() {
		var m WorkspaceMember
		if err := rows.Scan(&m.WorkspaceID, &m.Username, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) GetSetting(key string) (string, bool, error) {
	var value string
	err := db.sql.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (db *SQLiteStore) SetSetting(key, value string) error {
	_, err := db.sql.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (db *SQLiteStore) CreateInvitation(inv Invitation) error {
	if inv.CreatedAt == "" {
		inv.CreatedAt = nowUTC()
	}
	_, err := db.sql.Exec(`INSERT INTO invitations (token, workspace_id, email, role, display_name, projects_json, linked_agents_json, invited_by, status, created_at, expires_at, accepted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, inv.Token, inv.WorkspaceID, inv.Email, inv.Role, inv.DisplayName, defaultJSON(inv.ProjectsJSON), defaultJSON(inv.LinkedJSON), inv.InvitedBy, inv.Status, inv.CreatedAt, inv.ExpiresAt, inv.AcceptedAt)
	return err
}

func (db *SQLiteStore) InvitationByToken(token string) (Invitation, bool, error) {
	row := db.sql.QueryRow(`SELECT token, workspace_id, email, role, display_name, projects_json, linked_agents_json, invited_by, status, created_at, expires_at, accepted_at FROM invitations WHERE token = ?`, token)
	inv, err := scanInvitation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, false, nil
	}
	if err != nil {
		return Invitation{}, false, err
	}
	return inv, true, nil
}

func (db *SQLiteStore) ListInvitations() ([]Invitation, error) {
	rows, err := db.sql.Query(`SELECT token, workspace_id, email, role, display_name, projects_json, linked_agents_json, invited_by, status, created_at, expires_at, accepted_at FROM invitations ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Invitation, 0)
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (db *SQLiteStore) UpdateInvitation(inv Invitation) error {
	_, err := db.sql.Exec(`UPDATE invitations SET workspace_id = ?, email = ?, role = ?, display_name = ?, projects_json = ?, linked_agents_json = ?, invited_by = ?, status = ?, created_at = ?, expires_at = ?, accepted_at = ? WHERE token = ?`,
		inv.WorkspaceID, inv.Email, inv.Role, inv.DisplayName, defaultJSON(inv.ProjectsJSON), defaultJSON(inv.LinkedJSON), inv.InvitedBy, inv.Status, inv.CreatedAt, inv.ExpiresAt, inv.AcceptedAt, inv.Token)
	return err
}

type userScanner interface {
	Scan(dest ...any) error
}

func scanUser(row userScanner) (User, error) {
	var u User
	var disabled int
	err := row.Scan(&u.Username, &u.Email, &u.DisplayName, &u.Role, &u.Avatar, &u.Phone, &u.Bio, &u.PasswordHash, &disabled, &u.CreatedAt, &u.ProjectsJSON, &u.LinkedJSON, &u.WorkerGrantsJSON)
	u.Disabled = disabled != 0
	return u, err
}

func scanInvitation(row userScanner) (Invitation, error) {
	var inv Invitation
	err := row.Scan(&inv.Token, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.DisplayName, &inv.ProjectsJSON, &inv.LinkedJSON, &inv.InvitedBy, &inv.Status, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt)
	return inv, err
}

func defaultJSON(value string) string {
	if value == "" {
		return "[]"
	}
	return value
}

func (db *SQLiteStore) UpsertRecord(table string, workspaceID string, key []string, payload string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sqlite upsert panic recovered: %v", r)
		}
	}()
	if db == nil || db.sql == nil {
		return fmt.Errorf("database not open")
	}
	k1, k2, k3 := normalizeKey(key)
	_, err = db.sql.Exec(`INSERT INTO kv_records (table_name, workspace_id, k1, k2, k3, payload, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(table_name, workspace_id, k1, k2, k3) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		table, workspaceID, k1, k2, k3, payload, nowUTC())
	return err
}

func (db *SQLiteStore) GetRecord(table string, workspaceID string, key []string) (payload string, found bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sqlite get panic recovered: %v", r)
		}
	}()
	if db == nil || db.sql == nil {
		return "", false, fmt.Errorf("database not open")
	}
	k1, k2, k3 := normalizeKey(key)
	err = db.sql.QueryRow(`SELECT payload FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`, table, workspaceID, k1, k2, k3).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return payload, err == nil, err
}

// UpdateRecordIfPayloadAndRevision is a payload-aware compare-and-swap over a
// kv_records row: it writes newPayload only when the stored payload equals
// expectPayload AND the revision (updated_at) equals expectRevision. The
// workflow transition gate needs the payload witness — a claim gate must
// distinguish "the row I observed" from "a claim marker someone else wrote",
// and a revision-only witness cannot: two claimants observing revision R
// serialize their swaps, and the second sees a DIFFERENT revision (caught),
// but a marker→marker refresh by the marker's owner would otherwise succeed
// against any same-length revision sequence.
func (db *SQLiteStore) UpdateRecordIfPayloadAndRevision(table, workspaceID string, key []string, newPayload, expectPayload, expectRevision string) (swapped bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sqlite cas panic recovered: %v", r)
		}
	}()
	if db == nil || db.sql == nil {
		return false, fmt.Errorf("database not open")
	}
	k1, k2, k3 := normalizeKey(key)
	res, err := db.sql.Exec(`UPDATE kv_records SET payload = ?, updated_at = ?
WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ? AND payload = ? AND updated_at = ?`,
		newPayload, nowUTC(), table, workspaceID, k1, k2, k3, expectPayload, expectRevision)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// UpdateRecordIfRevision is a payload-blind CAS over a kv_records row,
// conditioned only on the revision token returned by RecordRevision (the
// row's updated_at column). A stale caller gets swapped=false and the row is
// left untouched for the winner to proceed. The WHERE clause rides the same
// connection as every other statement (SQLite serializes writers), so between
// the revision read and this write no third-party update can slip through
// unobserved: either the caller's revision is still current and the write
// lands, or it isn't and nothing changes.
func (db *SQLiteStore) UpdateRecordIfRevision(table, workspaceID string, key []string, newPayload, expectRevision string) (swapped bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sqlite cas panic recovered: %v", r)
		}
	}()
	if db == nil || db.sql == nil {
		return false, fmt.Errorf("database not open")
	}
	k1, k2, k3 := normalizeKey(key)
	res, err := db.sql.Exec(`UPDATE kv_records SET payload = ?, updated_at = ?
WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ? AND updated_at = ?`,
		newPayload, nowUTC(), table, workspaceID, k1, k2, k3, expectRevision)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// RecordRevision returns the current revision token (updated_at) of a
// kv_records row, for use with UpdateRecordIfRevision. A missing row yields
// found=false.
func (db *SQLiteStore) RecordRevision(table, workspaceID string, key []string) (revision string, found bool, err error) {
	if db == nil || db.sql == nil {
		return "", false, fmt.Errorf("database not open")
	}
	k1, k2, k3 := normalizeKey(key)
	err = db.sql.QueryRow(`SELECT updated_at FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`, table, workspaceID, k1, k2, k3).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return revision, err == nil, err
}

func (db *SQLiteStore) ListRecords(table string, workspaceID string, keyPrefix []string) ([]Record, error) {
	if len(keyPrefix) > 3 {
		return nil, fmt.Errorf("record key prefix too long")
	}
	query := `SELECT k1, k2, k3, payload FROM kv_records WHERE table_name = ? AND workspace_id = ?`
	args := []any{table, workspaceID}
	if len(keyPrefix) >= 1 {
		query += ` AND k1 = ?`
		args = append(args, keyPrefix[0])
	}
	if len(keyPrefix) >= 2 {
		query += ` AND k2 = ?`
		args = append(args, keyPrefix[1])
	}
	if len(keyPrefix) >= 3 {
		query += ` AND k3 = ?`
		args = append(args, keyPrefix[2])
	}
	query += ` ORDER BY k1 ASC, k2 ASC, k3 ASC`
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var k1, k2, k3, payload string
		if err := rows.Scan(&k1, &k2, &k3, &payload); err != nil {
			return nil, err
		}
		out = append(out, Record{Key: []string{k1, k2, k3}, Payload: payload})
	}
	return out, rows.Err()
}

func (db *SQLiteStore) DeleteRecord(table string, workspaceID string, key []string) error {
	k1, k2, k3 := normalizeKey(key)
	_, err := db.sql.Exec(`DELETE FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`, table, workspaceID, k1, k2, k3)
	return err
}

func normalizeKey(key []string) (string, string, string) {
	var out [3]string
	for i := 0; i < len(key) && i < 3; i++ {
		out[i] = key[i]
	}
	return out[0], out[1], out[2]
}
