package api

import (
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/multigent/multigent/internal/assets"
	controldb "github.com/multigent/multigent/internal/db"
)

// Project asset endpoints. Every route below sits behind withTokenAuth and
// checkProjectAccess (server.go registers them on the authenticated mux) —
// AGENTS.md security red line #1.
//
// SECURITY INVARIANT (bind-by-hash closure): a binding always references an
// asset_files row that lives in the caller's workspace AND this project.
// There is no endpoint that accepts a bare sha256 as a binding source, so a
// user can never attach content they cannot already read here.

func writeAssetJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// POST /api/v1/projects/{name}/assets — multipart upload; every uploaded file
// becomes a project library file (upload once, reuse everywhere).
func (s *Server) handleProjectAssetUpload(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if _, err := s.st.Project(project); err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		s.jsonError(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	bs, err := assets.NewBlobStore()
	if err != nil {
		s.serverError(w, err)
		return
	}
	createdBy := "system"
	if cur := s.currentUser(r); cur != nil && strings.TrimSpace(cur.Username) != "" {
		createdBy = cur.Username
	}
	uploaded := make([]controldb.AssetFile, 0)
	for _, fh := range r.MultipartForm.File["file"] {
		src, err := fh.Open()
		if err != nil {
			continue
		}
		sha, size, err := bs.Put(src)
		src.Close()
		if err != nil {
			log.Printf("[assets] upload store failed for %s: %v", fh.Filename, err)
			s.jsonError(w, http.StatusInternalServerError, "store blob: "+err.Error())
			return
		}
		uploadMime := fh.Header.Get("Content-Type")
		if uploadMime == "" || uploadMime == "application/octet-stream" {
			uploadMime = mime.TypeByExtension(filepath.Ext(fh.Filename))
		}
		if err := s.controlDB.UpsertAssetBlob(controldb.AssetBlob{
			Sha256: sha, Size: size, Mime: uploadMime,
			StoragePath: sha[:2] + "/" + sha, CreatedBy: createdBy,
		}); err != nil {
			s.serverError(w, err)
			return
		}
		f := &controldb.AssetFile{
			WorkspaceID: workspaceID,
			ProjectID:   project,
			DisplayName: assets.SanitizeDisplayName(fh.Filename),
			CurrentSha:  sha,
			CreatedBy:   createdBy,
		}
		if err := s.controlDB.InsertAssetFile(f); err != nil {
			s.serverError(w, err)
			return
		}
		uploaded = append(uploaded, *f)
	}
	if len(uploaded) == 0 {
		s.jsonError(w, http.StatusBadRequest, "no file field in multipart form")
		return
	}
	writeAssetJSON(w, http.StatusOK, uploaded)
}

// GET /api/v1/projects/{name}/assets — project library listing with usage
// counts and version metadata (the "project assets" tab data source).
func (s *Server) handleListProjectAssets(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}
	includeArchived := r.URL.Query().Get("includeArchived") == "1"
	files, err := s.controlDB.ListAssetFilesForProject(workspaceID, project, includeArchived)
	if err != nil {
		s.serverError(w, err)
		return
	}
	usage, err := s.controlDB.AssetFileUsage(project)
	if err != nil {
		s.serverError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(files))
	for _, f := range files {
		row := map[string]any{
			"id":          f.ID,
			"displayName": f.DisplayName,
			"currentSha":  f.CurrentSha,
			"createdBy":   f.CreatedBy,
			"createdAt":   f.CreatedAt,
			"archivedAt":  f.ArchivedAt,
			"usageCount":  usage[f.ID],
		}
		blob, ok, err := s.controlDB.AssetBlob(f.CurrentSha)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if ok {
			row["size"] = blob.Size
			row["mime"] = blob.Mime
		}
		rows = append(rows, row)
	}
	writeAssetJSON(w, http.StatusOK, rows)
}

// GET /api/v1/projects/{name}/assets/{fileId} — detail: file + pinned blob +
// all bindings (project-visible trace of which tasks use it).
func (s *Server) handleProjectAssetDetail(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	f, ok := s.projectAssetFile(w, r, project)
	if !ok {
		return
	}
	atts, err := s.controlDB.ListAssetAttachmentsForFile(f.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	resp := map[string]any{"file": f, "attachments": atts}
	blob, ok, err := s.controlDB.AssetBlob(f.CurrentSha)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if ok {
		resp["blob"] = blob
	}
	writeAssetJSON(w, http.StatusOK, resp)
}

// GET /api/v1/projects/{name}/assets/{fileId}/download — serves the pinned
// blob bytes after re-verifying the hash (corruption must fail loudly, never
// deliver silent wrong bytes).
func (s *Server) handleProjectAssetDownload(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	f, ok := s.projectAssetFile(w, r, project)
	if !ok {
		return
	}
	bs, err := assets.NewBlobStore()
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := bs.Verify(f.CurrentSha); err != nil {
		log.Printf("[assets] download verify failed for %s: %v", f.ID, err)
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	contentType := "application/octet-stream"
	if blob, ok, err := s.controlDB.AssetBlob(f.CurrentSha); err != nil {
		s.serverError(w, err)
		return
	} else if ok && strings.TrimSpace(blob.Mime) != "" {
		contentType = blob.Mime
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(f.DisplayName))
	http.ServeFile(w, r, bs.Path(f.CurrentSha))
}

// PATCH /api/v1/projects/{name}/assets/{fileId} — rename display name (blob
// bytes are immutable; renaming never touches content).
func (s *Server) handleProjectAssetRename(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	f, ok := s.projectAssetFile(w, r, project)
	if !ok {
		return
	}
	var body struct {
		DisplayName string `json:"displayName"`
	}
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.controlDB.RenameAssetFile(f.ID, body.DisplayName); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _, err := s.controlDB.AssetFile(f.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeAssetJSON(w, http.StatusOK, updated)
}

// DELETE /api/v1/projects/{name}/assets/{fileId} — soft delete (archive).
// Bindings and blobs survive; the file leaves the default library listing.
func (s *Server) handleProjectAssetArchive(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	f, ok := s.projectAssetFile(w, r, project)
	if !ok {
		return
	}
	if err := s.controlDB.ArchiveAssetFile(f.ID); err != nil {
		s.serverError(w, err)
		return
	}
	writeAssetJSON(w, http.StatusOK, map[string]string{"status": "archived", "id": f.ID})
}

// GET /api/v1/projects/{name}/tasks/{taskId}/assets — the task's bound assets
// with each file's current version, so the UI can surface version drift
// (attachment sha != fileCurrentSha means a newer version exists).
func (s *Server) handleListTaskAssets(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	taskID := r.PathValue("taskId")
	if _, _, err := s.findTaskInProject(project, taskID); err != nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "task not found")
		return
	}
	atts, err := s.controlDB.ListAssetAttachmentsForTask(taskID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(atts))
	for _, att := range atts {
		currentSha := ""
		if f, ok, err := s.controlDB.AssetFile(att.FileID); err != nil {
			s.serverError(w, err)
			return
		} else if ok {
			currentSha = f.CurrentSha
		}
		rows = append(rows, map[string]any{
			"id":             att.ID,
			"fileId":         att.FileID,
			"displayName":    att.DisplayName,
			"role":           att.Role,
			"required":       att.Required,
			"sha256":         att.Sha256,
			"fileCurrentSha": currentSha,
			"size":           att.BlobSize,
			"mime":           att.BlobMime,
			"addedBy":        att.AddedBy,
			"createdAt":      att.CreatedAt,
		})
	}
	writeAssetJSON(w, http.StatusOK, rows)
}

// POST /api/v1/projects/{name}/tasks/{taskId}/assets — bind a project library
// file to the task, pinning the file's current version. sha256 may be passed
// explicitly but must equal the file's current version: v1 has no version
// history table, so pinning anything else would be a lie.
func (s *Server) handleBindTaskAsset(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	taskID := r.PathValue("taskId")
	if _, _, err := s.findTaskInProject(project, taskID); err != nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "task not found")
		return
	}
	var body struct {
		FileID   string `json:"fileId"`
		Sha256   string `json:"sha256"`
		Role     string `json:"role"`
		Required bool   `json:"required"`
	}
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}
	f, ok, err := s.controlDB.AssetFile(strings.TrimSpace(body.FileID))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !ok {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAssetNotFound, "asset file not found")
		return
	}
	// The bind-by-hash closure: the file must be visible in THIS project.
	if f.WorkspaceID != workspaceID || f.ProjectID != project {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeAssetNotAccessible, "asset file belongs to another project or workspace")
		return
	}
	if f.ArchivedAt != "" {
		s.jsonError(w, http.StatusBadRequest, "asset file is archived; restore it before binding")
		return
	}
	if sha := strings.ToLower(strings.TrimSpace(body.Sha256)); sha != "" && sha != f.CurrentSha {
		s.jsonError(w, http.StatusBadRequest, "sha256 does not match the file's current version")
		return
	}
	role := strings.TrimSpace(body.Role)
	if role == "" {
		role = controldb.AssetRoleReference
	}
	existing, err := s.controlDB.ListAssetAttachmentsForTask(taskID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(existing) >= assets.MaxTaskAttachments {
		s.jsonError(w, http.StatusBadRequest, "task already has the maximum number of bound assets; unbind one first")
		return
	}
	addedBy := "system"
	if cur := s.currentUser(r); cur != nil && strings.TrimSpace(cur.Username) != "" {
		addedBy = cur.Username
	}
	a := &controldb.AssetAttachment{
		FileID:    f.ID,
		Sha256:    f.CurrentSha,
		ProjectID: project,
		TaskID:    taskID,
		Role:      role,
		Required:  body.Required,
		AddedBy:   addedBy,
	}
	if err := s.controlDB.InsertAssetAttachment(a); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAssetJSON(w, http.StatusOK, map[string]any{
		"attachment":     a,
		"displayName":    f.DisplayName,
		"fileCurrentSha": f.CurrentSha,
	})
}

// DELETE /api/v1/projects/{name}/tasks/{taskId}/assets/{attachmentId} — unbind.
func (s *Server) handleUnbindTaskAsset(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	taskID := r.PathValue("taskId")
	if _, _, err := s.findTaskInProject(project, taskID); err != nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, "task not found")
		return
	}
	a, ok, err := s.controlDB.AssetAttachment(r.PathValue("attachmentId"))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !ok || a.TaskID != taskID || a.ProjectID != project {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAssetNotFound, "attachment not found on this task")
		return
	}
	if err := s.controlDB.DeleteAssetAttachment(a.ID); err != nil {
		s.serverError(w, err)
		return
	}
	writeAssetJSON(w, http.StatusOK, map[string]string{"status": "unbound", "id": a.ID})
}

// projectAssetFile loads the file row and enforces project ownership — the
// single choke point for file-id addressing (IDs are unguessable random hex,
// but ownership is checked anyway, never assumed).
func (s *Server) projectAssetFile(w http.ResponseWriter, r *http.Request, project string) (*controldb.AssetFile, bool) {
	f, ok, err := s.controlDB.AssetFile(r.PathValue("fileId"))
	if err != nil {
		s.serverError(w, err)
		return nil, false
	}
	if !ok || f.ProjectID != project {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAssetNotFound, "asset file not found")
		return nil, false
	}
	return f, true
}
