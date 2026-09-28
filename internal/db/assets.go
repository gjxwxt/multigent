package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Project assets: a content-addressed attachment model (see
// outputs/project-assets-minimal-attachment-design-2026-09-28.md).
//
// Three layers, each answering one question and nothing more:
//   - asset_blobs: the bytes. Identity = SHA-256; identical content is stored
//     once per deployment. "Immutable versions" are a free consequence of
//     content addressing, not a implemented feature.
//   - asset_files: the user-visible "one file". Editing = moving current_sha
//     to a new blob; old blobs stay alive as long as something references
//     them. No version objects, no version UI. A file IS the project-level
//     asset — no separate "project attachment" row exists.
//   - asset_attachments: task bindings only (project membership lives on the
//     file row). Deleting a task or an attachment never deletes a blob.
//
// SECURITY INVARIANT: blobs are never directly addressable by clients. Every
// read goes through an asset_files/asset_attachments row and the caller's
// project access check. There is deliberately NO "bind by bare SHA" path —
// asset_attachments.file_id is NOT NULL, so a binding always requires a file
// row that was created by an upload (bytes present) or is otherwise readable
// in the caller's workspace. That closes the bind-by-hash hole (binding
// content you cannot read, then reading it through a run mount).

type AssetBlob struct {
	Sha256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Mime        string `json:"mime"`
	StoragePath string `json:"storagePath"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   string `json:"createdAt"`
}

type AssetFile struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	ProjectID   string `json:"projectId"`
	DisplayName string `json:"displayName"`
	CurrentSha  string `json:"currentSha"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   string `json:"createdAt"`
	ArchivedAt  string `json:"archivedAt,omitempty"`
}

type AssetAttachment struct {
	ID        string `json:"id"`
	FileID    string `json:"fileId"`
	Sha256    string `json:"sha256"`
	ProjectID string `json:"projectId"`
	TaskID    string `json:"taskId"`
	Role      string `json:"role"`
	Required  bool   `json:"required"`
	AddedBy   string `json:"addedBy"`
	CreatedAt string `json:"createdAt"`
}

// AssetAttachmentWithFile is a join row: the attachment plus the fields the
// manifest and UI need from the file and blob layers.
type AssetAttachmentWithFile struct {
	AssetAttachment
	FileWorkspaceID string `json:"fileWorkspaceId"`
	DisplayName     string `json:"displayName"`
	BlobSize        int64  `json:"blobSize"`
	BlobMime        string `json:"blobMime"`
}

// Asset attachment roles. requirement_input = the task's requirement/spec
// material; reference = nice-to-have context; deliverable = an agent output
// published back into the project library (phase 2).
const (
	AssetRoleRequirementInput = "requirement_input"
	AssetRoleReference        = "reference"
	AssetRoleDeliverable      = "deliverable"
)

func ValidAssetRole(role string) bool {
	switch role {
	case AssetRoleRequirementInput, AssetRoleReference, AssetRoleDeliverable:
		return true
	}
	return false
}

func generateAssetID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// UpsertAssetBlob registers a stored blob. The blob bytes themselves are
// managed by the assets blob store; this row is the metadata index. INSERT OR
// IGNORE: identical content uploaded again keeps the first uploader/atime.
func (db *SQLiteStore) UpsertAssetBlob(b AssetBlob) error {
	b.Sha256 = strings.ToLower(strings.TrimSpace(b.Sha256))
	if len(b.Sha256) != 64 || b.Size < 0 || strings.TrimSpace(b.StoragePath) == "" {
		return fmt.Errorf("asset blob requires sha256 (64 hex), size and storage path")
	}
	if b.CreatedAt == "" {
		b.CreatedAt = nowUTC()
	}
	_, err := db.sql.Exec(`INSERT OR IGNORE INTO asset_blobs (sha256, size, mime, storage_path, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		b.Sha256, b.Size, b.Mime, b.StoragePath, b.CreatedBy, b.CreatedAt)
	return err
}

func (db *SQLiteStore) AssetBlob(sha256 string) (*AssetBlob, bool, error) {
	row := db.sql.QueryRow(`SELECT sha256, size, mime, storage_path, created_by, created_at
		FROM asset_blobs WHERE sha256 = ?`, strings.ToLower(strings.TrimSpace(sha256)))
	var b AssetBlob
	if err := row.Scan(&b.Sha256, &b.Size, &b.Mime, &b.StoragePath, &b.CreatedBy, &b.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &b, true, nil
}

// InsertAssetFile creates the logical file row. The id is assigned here so
// callers cannot forge one; the current_sha blob must already exist (FK).
func (db *SQLiteStore) InsertAssetFile(f *AssetFile) error {
	f.WorkspaceID = strings.TrimSpace(f.WorkspaceID)
	f.ProjectID = strings.TrimSpace(f.ProjectID)
	f.DisplayName = strings.TrimSpace(f.DisplayName)
	f.CurrentSha = strings.ToLower(strings.TrimSpace(f.CurrentSha))
	if f.WorkspaceID == "" || f.ProjectID == "" || f.DisplayName == "" || f.CurrentSha == "" {
		return fmt.Errorf("asset file requires workspace, project, display name and current sha")
	}
	if f.ID == "" {
		f.ID = generateAssetID("asf")
	}
	if f.CreatedAt == "" {
		f.CreatedAt = nowUTC()
	}
	_, err := db.sql.Exec(`INSERT INTO asset_files (id, workspace_id, project_id, display_name, current_sha, created_by, created_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.WorkspaceID, f.ProjectID, f.DisplayName, f.CurrentSha, f.CreatedBy, f.CreatedAt, f.ArchivedAt)
	if err != nil {
		f.ID = ""
		return err
	}
	return nil
}

func scanAssetFile(row interface{ Scan(...any) error }) (*AssetFile, error) {
	var f AssetFile
	if err := row.Scan(&f.ID, &f.WorkspaceID, &f.ProjectID, &f.DisplayName, &f.CurrentSha, &f.CreatedBy, &f.CreatedAt, &f.ArchivedAt); err != nil {
		return nil, err
	}
	return &f, nil
}

const assetFileColumns = `id, workspace_id, project_id, display_name, current_sha, created_by, created_at, archived_at`

func (db *SQLiteStore) AssetFile(id string) (*AssetFile, bool, error) {
	row := db.sql.QueryRow(`SELECT `+assetFileColumns+` FROM asset_files WHERE id = ?`, strings.TrimSpace(id))
	f, err := scanAssetFile(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// ListAssetFilesForProject lists the project's files, newest first. Archived
// files are excluded unless includeArchived is set.
func (db *SQLiteStore) ListAssetFilesForProject(workspaceID, projectID string, includeArchived bool) ([]AssetFile, error) {
	q := `SELECT ` + assetFileColumns + ` FROM asset_files WHERE workspace_id = ? AND project_id = ?`
	if !includeArchived {
		q += ` AND archived_at = ''`
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := db.sql.Query(q, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetFile{}
	for rows.Next() {
		f, err := scanAssetFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// RenameAssetFile changes the display name only — blob bytes are immutable.
func (db *SQLiteStore) RenameAssetFile(id, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return fmt.Errorf("display name must not be empty")
	}
	res, err := db.sql.Exec(`UPDATE asset_files SET display_name = ? WHERE id = ?`, displayName, strings.TrimSpace(id))
	return requireAssetRow(res, err)
}

// MoveAssetFilePointer points the file at a new blob (the "edit" operation).
// The old blob stays: existing attachments pin their own sha256.
func (db *SQLiteStore) MoveAssetFilePointer(id, newSha string) error {
	newSha = strings.ToLower(strings.TrimSpace(newSha))
	if len(newSha) != 64 {
		return fmt.Errorf("new sha256 must be 64 hex chars")
	}
	res, err := db.sql.Exec(`UPDATE asset_files SET current_sha = ? WHERE id = ?`, newSha, strings.TrimSpace(id))
	return requireAssetRow(res, err)
}

// ArchiveAssetFile soft-deletes the file. Attachments (and therefore task
// pins) survive; the blob stays referenced.
func (db *SQLiteStore) ArchiveAssetFile(id string) error {
	res, err := db.sql.Exec(`UPDATE asset_files SET archived_at = ? WHERE id = ? AND archived_at = ''`, nowUTC(), strings.TrimSpace(id))
	return requireAssetRow(res, err)
}

// InsertAssetAttachment binds a file to a project (task_id empty) or a task.
// The pinned sha256 defaults to the file's current version when empty.
func (db *SQLiteStore) InsertAssetAttachment(a *AssetAttachment) error {
	a.FileID = strings.TrimSpace(a.FileID)
	a.ProjectID = strings.TrimSpace(a.ProjectID)
	a.TaskID = strings.TrimSpace(a.TaskID)
	a.Sha256 = strings.ToLower(strings.TrimSpace(a.Sha256))
	a.Role = strings.TrimSpace(a.Role)
	if a.FileID == "" || a.ProjectID == "" || a.Sha256 == "" {
		return fmt.Errorf("asset attachment requires file, project and pinned sha256")
	}
	if a.TaskID == "" {
		return fmt.Errorf("asset attachment requires a task id; project-level assets are files without attachments")
	}
	if !ValidAssetRole(a.Role) {
		return fmt.Errorf("asset attachment role must be one of requirement_input, reference, deliverable")
	}
	if a.ID == "" {
		a.ID = generateAssetID("asa")
	}
	if a.CreatedAt == "" {
		a.CreatedAt = nowUTC()
	}
	_, err := db.sql.Exec(`INSERT INTO asset_attachments (id, file_id, sha256, project_id, task_id, role, required, added_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.FileID, a.Sha256, a.ProjectID, a.TaskID, a.Role, boolToInt(a.Required), a.AddedBy, a.CreatedAt)
	if err != nil {
		a.ID = ""
		return err
	}
	return nil
}

const assetAttachmentColumns = `id, file_id, sha256, project_id, task_id, role, required, added_by, created_at`

func scanAssetAttachment(row interface{ Scan(...any) error }) (*AssetAttachment, error) {
	var a AssetAttachment
	var required int
	if err := row.Scan(&a.ID, &a.FileID, &a.Sha256, &a.ProjectID, &a.TaskID, &a.Role, &required, &a.AddedBy, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.Required = required != 0
	return &a, nil
}

func (db *SQLiteStore) AssetAttachment(id string) (*AssetAttachment, bool, error) {
	row := db.sql.QueryRow(`SELECT `+assetAttachmentColumns+` FROM asset_attachments WHERE id = ?`, strings.TrimSpace(id))
	a, err := scanAssetAttachment(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return a, true, nil
}

func (db *SQLiteStore) ListAssetAttachmentsForFile(fileID string) ([]AssetAttachment, error) {
	rows, err := db.sql.Query(`SELECT `+assetAttachmentColumns+` FROM asset_attachments WHERE file_id = ? ORDER BY created_at DESC, id DESC`, strings.TrimSpace(fileID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetAttachment{}
	for rows.Next() {
		a, err := scanAssetAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ListAssetAttachmentsForTask returns the task's bound assets with file/blob
// metadata joined in. This is the single query the run-time stager and the
// task detail UI both consume — one shape, no drift.
func (db *SQLiteStore) ListAssetAttachmentsForTask(taskID string) ([]AssetAttachmentWithFile, error) {
	rows, err := db.sql.Query(`SELECT
		a.id, a.file_id, a.sha256, a.project_id, a.task_id, a.role, a.required, a.added_by, a.created_at,
		f.workspace_id, f.display_name, b.size, b.mime
		FROM asset_attachments a
		JOIN asset_files f ON f.id = a.file_id
		JOIN asset_blobs b ON b.sha256 = a.sha256
		WHERE a.task_id = ?
		ORDER BY a.required DESC, a.created_at ASC, a.id ASC`, strings.TrimSpace(taskID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetAttachmentWithFile{}
	for rows.Next() {
		var w AssetAttachmentWithFile
		var required int
		if err := rows.Scan(&w.ID, &w.FileID, &w.Sha256, &w.ProjectID, &w.TaskID, &w.Role, &required, &w.AddedBy, &w.CreatedAt,
			&w.FileWorkspaceID, &w.DisplayName, &w.BlobSize, &w.BlobMime); err != nil {
			return nil, err
		}
		w.Required = required != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteAssetAttachment unbinds. Files and blobs are untouched.
func (db *SQLiteStore) DeleteAssetAttachment(id string) error {
	res, err := db.sql.Exec(`DELETE FROM asset_attachments WHERE id = ?`, strings.TrimSpace(id))
	return requireAssetRow(res, err)
}

// AssetFileUsage counts attachments per file for a project (list page badge:
// how many tasks bind this file).
func (db *SQLiteStore) AssetFileUsage(projectID string) (map[string]int, error) {
	rows, err := db.sql.Query(`SELECT file_id, COUNT(*) FROM asset_attachments WHERE project_id = ? GROUP BY file_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CountAssetBlobReferences returns how many files point at the blob and how
// many attachments pin it — the unreferenced-blob cleanup rule's input.
func (db *SQLiteStore) CountAssetBlobReferences(sha256 string) (fileRefs, attachmentRefs int, err error) {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM asset_files WHERE current_sha = ?`, sha256).Scan(&fileRefs); err != nil {
		return 0, 0, err
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM asset_attachments WHERE sha256 = ?`, sha256).Scan(&attachmentRefs); err != nil {
		return 0, 0, err
	}
	return fileRefs, attachmentRefs, nil
}

func requireAssetRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("asset row not found")
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
