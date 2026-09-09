package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

// Frozen design-gate snapshot. The design_review step is a generic node: it
// only promises the approved_design_* output fields. To keep downstream steps
// decoupled from OD's live state (projects keep evolving there), the confirm
// path captures the referenced artifacts once and freezes them:
//
//   - approved_design_html: the entry artifact's full HTML (inline in outputs,
//     capped; OD may keep editing the project after approval)
//   - approved_design_snapshot_path: workspace-relative path of the full
//     snapshot bundle (all HTML/CSS/JS artifacts) for agent consumption
//
// Everything else the UI or agents might want (project id, links) is already
// carried by the other approved_design_* fields, so the wire contract never
// changes when OD grows new metadata.
const (
	designSnapshotDir       = ".multigent/files/design-snapshots"
	designSnapshotInlineCap = 512 << 10 // inline HTML cap in outputs (512 KiB)
	designSnapshotFetchCap  = 8 << 20   // per-file fetch cap
)

type designSnapshotFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// fetchedDesignFile caches the raw bytes of an artifact, aligned 1:1 with the
// `artifacts` slice from designSnapshotArtifacts. Capturing bytes once (instead
// of re-indexing artifacts[i] during persist) removes the former index-
// misalignment bug, where a mid-list fetch failure shrank manifest.Files but
// left artifacts untouched, causing artifacts[i] to resolve to the wrong file.
type fetchedDesignFile struct {
	Path string
	Kind string
	Size int64
	Raw  []byte
}

type designSnapshotManifest struct {
	Version   int                  `json:"version"`
	TaskID    string               `json:"taskId"`
	ProjectID string               `json:"odProjectId"`
	Source    string               `json:"odSource"`
	TakenAt   time.Time            `json:"takenAt"`
	Entry     string               `json:"entry,omitempty"`
	Files     []designSnapshotFile `json:"files"`
}

// captureDesignGateSnapshot freezes the OD project's HTML artifacts at
// confirm time. Atomic: all artifacts and the manifest must be successfully
// fetched and written to disk, or an error is returned to fail closed.
// Downstream steps consume the frozen artifacts via approved_design_snapshot_path.
// The error paths never carry OD credentials: GetProjectFile reports HTTP
// status only, and odDo redacts the token from upstream bodies.
func (s *Server) captureDesignGateSnapshot(r *http.Request, project, agent string, t *entity.Task, odProjectID string) (inlineHTML, snapshotPath string, err error) {
	if strings.TrimSpace(odProjectID) == "" {
		return "", "", errors.New("missing design project id")
	}
	client := s.defaultODClient()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	files, err := client.ListProjectFiles(ctx, odProjectID)
	if err != nil {
		detail := redactODDetail(err)
		s.addComment(t, project, agent, "design snapshot: list files failed: "+detail)
		return "", "", fmt.Errorf("list files failed: %s", detail)
	}
	artifacts := designSnapshotArtifacts(files)
	if len(artifacts) == 0 {
		s.addComment(t, project, agent, "design snapshot: no html artifacts in OD project "+odProjectID)
		return "", "", fmt.Errorf("no html artifacts in OD project %s", odProjectID)
	}

	manifest := designSnapshotManifest{
		Version:   1,
		TaskID:    t.ID,
		ProjectID: odProjectID,
		Source:    strings.TrimSpace(t.DesignSource),
		TakenAt:   time.Now().UTC(),
		Entry:     artifacts[0].Path,
	}

	fetched := make([]fetchedDesignFile, len(artifacts))
	var inline string
	for i, art := range artifacts {
		raw, err := client.GetProjectFile(ctx, odProjectID, art.Path)
		if err != nil {
			detail := redactODDetail(err)
			s.addComment(t, project, agent, "design snapshot: fetch "+art.Path+" failed: "+detail)
			return "", "", fmt.Errorf("fetch %s failed: %s", art.Path, detail)
		}
		if int64(len(raw)) > designSnapshotFetchCap {
			raw = raw[:designSnapshotFetchCap]
		}
		kind := strings.ToLower(strings.TrimSpace(art.Kind))
		if kind == "" {
			kind = strings.TrimPrefix(strings.ToLower(filepath.Ext(art.Path)), ".")
		}
		fetched[i] = fetchedDesignFile{Path: art.Path, Kind: kind, Size: int64(len(raw)), Raw: raw}
		manifest.Files = append(manifest.Files, designSnapshotFile{Path: art.Path, Kind: kind, Size: int64(len(raw))})
		if isHTML(art.Path) && inline == "" {
			// Inline the entry HTML so the contract field is self-contained;
			// cap it — downstream consumers get the full file from the bundle.
			inline = string(raw)
			if len(inline) > designSnapshotInlineCap {
				inline = inline[:designSnapshotInlineCap]
			}
		}
	}
	if len(manifest.Files) < len(artifacts) {
		s.addComment(t, project, agent, "design snapshot: incomplete — captured "+
			strconv.Itoa(len(manifest.Files))+" of "+strconv.Itoa(len(artifacts))+" artifacts")
		return "", "", fmt.Errorf("incomplete snapshot: captured %d of %d artifacts", len(manifest.Files), len(artifacts))
	}

	root := strings.TrimSpace(s.st.Root())
	if root == "" {
		root = s.root
	}
	if root == "" {
		return "", "", errors.New("workspace store root not configured")
	}

	parentDir := filepath.Join(root, designSnapshotDir)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		s.addComment(t, project, agent, "design snapshot: mkdir parent failed: "+err.Error())
		return "", "", fmt.Errorf("failed to create parent snapshot dir: %w", err)
	}

	tmpDir, err := os.MkdirTemp(parentDir, ".tmp-"+t.ID+"-*")
	if err != nil {
		s.addComment(t, project, agent, "design snapshot: mkdir temp failed: "+err.Error())
		return "", "", fmt.Errorf("failed to create temp snapshot dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	for _, f := range fetched {
		if f.Raw == nil {
			return "", "", fmt.Errorf("missing artifact content for %s", f.Path)
		}
		relPath := filepath.Clean(filepath.FromSlash(f.Path))
		if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, "\\") || relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
			return "", "", fmt.Errorf("invalid artifact path traversal attempt: %q", f.Path)
		}
		destPath := filepath.Join(tmpDir, relPath)
		relToTmp, err := filepath.Rel(tmpDir, destPath)
		if err != nil || strings.HasPrefix(relToTmp, "..") || strings.Contains(relToTmp, "..") {
			return "", "", fmt.Errorf("invalid artifact path traversal attempt: %q", f.Path)
		}
		if dir := filepath.Dir(destPath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", "", fmt.Errorf("failed to create dir for %s: %w", f.Path, err)
			}
		}
		if fi, err := os.Lstat(destPath); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				return "", "", fmt.Errorf("invalid artifact: symlinks are not permitted: %q", f.Path)
			}
		}
		if err := os.WriteFile(destPath, f.Raw, 0o644); err != nil {
			return "", "", fmt.Errorf("failed to write %s: %w", f.Path, err)
		}
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.json"), rawManifest, 0o644); err != nil {
		return "", "", fmt.Errorf("failed to write manifest.json: %w", err)
	}

	absDir := filepath.Join(parentDir, t.ID)
	_ = os.RemoveAll(absDir)
	if err := os.Rename(tmpDir, absDir); err != nil {
		return "", "", fmt.Errorf("failed to publish snapshot atomically: %w", err)
	}
	snapshotPath = designSnapshotDir + "/" + t.ID + "/manifest.json"
	return inline, snapshotPath, nil
}

// handleGetDesignSnapshotFile serves frozen design snapshot files for a task.
// Route: GET /api/v1/projects/{name}/tasks/{taskId}/design/snapshot/{path...}
// Validates project access and restricts reads strictly to the task's snapshot directory,
// preventing path traversal attacks.
func (s *Server) handleGetDesignSnapshotFile(w http.ResponseWriter, r *http.Request) {
	projectName := r.PathValue("name")
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if taskID == "" {
		s.jsonError(w, http.StatusBadRequest, "missing taskId")
		return
	}
	task := s.projectTaskResourceGuard(w, r, projectName, taskID)
	if task == nil {
		return
	}

	root := strings.TrimSpace(s.st.Root())
	if root == "" {
		root = s.root
	}
	taskSnapshotDir := filepath.Join(root, designSnapshotDir, taskID)
	if _, err := os.Stat(taskSnapshotDir); err != nil {
		s.jsonError(w, http.StatusNotFound, "design snapshot not found")
		return
	}

	sub := r.PathValue("path")
	if strings.Contains(sub, "..") {
		s.jsonError(w, http.StatusBadRequest, "invalid snapshot path")
		return
	}
	cleanSub := filepath.Clean("/" + sub)
	cleanSub = strings.TrimPrefix(cleanSub, "/")
	if cleanSub == "." {
		cleanSub = ""
	}

	targetPath := filepath.Join(taskSnapshotDir, cleanSub)
	rel, err := filepath.Rel(taskSnapshotDir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, "..") {
		s.jsonError(w, http.StatusBadRequest, "invalid snapshot path")
		return
	}

	realSnapshotDir, err := filepath.EvalSymlinks(taskSnapshotDir)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "design snapshot not found")
		return
	}

	fi, err := os.Stat(targetPath)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "snapshot file not found")
		return
	}
	if fi.IsDir() {
		// Directory request: attempt manifest.json entry, fallback to index.html
		var entryFile string
		if raw, err := os.ReadFile(filepath.Join(targetPath, "manifest.json")); err == nil {
			var mf designSnapshotManifest
			if json.Unmarshal(raw, &mf) == nil && mf.Entry != "" {
				candidate := filepath.Join(targetPath, filepath.Clean(mf.Entry))
				if _, err := os.Stat(candidate); err == nil {
					entryFile = candidate
				}
			}
		}
		if entryFile == "" {
			candidate := filepath.Join(targetPath, "index.html")
			if _, err := os.Stat(candidate); err == nil {
				entryFile = candidate
			}
		}
		if entryFile == "" {
			s.jsonError(w, http.StatusNotFound, "snapshot entry file not found")
			return
		}
		targetPath = entryFile
	}

	realTarget, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "snapshot file not found")
		return
	}
	relToReal, err := filepath.Rel(realSnapshotDir, realTarget)
	if err != nil || strings.HasPrefix(relToReal, "..") || strings.Contains(relToReal, "..") {
		s.jsonError(w, http.StatusBadRequest, "invalid snapshot path: escaping symlink")
		return
	}

	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms; default-src 'self' data: blob: https: 'unsafe-inline' 'unsafe-eval'; frame-ancestors 'self'")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeFile(w, r, targetPath)
}

// designSnapshotArtifacts keeps the html/css/js files that make the snapshot
// self-contained. Selection is by FILE EXTENSION, not by OD's `kind` field:
// OpenDesign reports css/js with kind "code" (and markdown with "text"), so a
// kind-based match would drop every companion and freeze only the entry HTML
// shell. That is exactly what caused the 2026-09-03 4test incident — a
// 595-byte index.html with dangling <link>/<script> refs shipped to the
// implementer, which then rebuilt the app from scratch instead of from the
// prototype.
func designSnapshotArtifacts(files []ODProjectFile) []ODProjectFile {
	keep := make([]ODProjectFile, 0, len(files))
	for _, f := range files {
		switch strings.ToLower(filepath.Ext(f.Path)) {
		case ".html", ".css", ".js":
			keep = append(keep, f)
		}
	}
	sort.SliceStable(keep, func(i, j int) bool {
		// entry HTML first, then deterministic by path
		if isHTML(keep[i].Path) != isHTML(keep[j].Path) {
			return isHTML(keep[i].Path)
		}
		return keep[i].Path < keep[j].Path
	})
	return keep
}

// isHTML reports whether path is an .html file (extension match, case-insensitive).
func isHTML(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".html")
}

func redactODDetail(err error) string {
	detail := err.Error()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return detail
}
