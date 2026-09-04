package api

import (
	"context"
	"encoding/json"
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
	designSnapshotDir       = ".multigent/design-snapshots"
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
// confirm time. Best-effort: a failed capture degrades to the pre-existing
// behaviour (project id only) with a task comment trail — it must never block
// the review transition, because the workflow state is the source of truth.
// The error paths never carry OD credentials: GetProjectFile reports HTTP
// status only, and odDo redacts the token from upstream bodies.
func (s *Server) captureDesignGateSnapshot(r *http.Request, project, agent string, t *entity.Task, odProjectID string) (inlineHTML, snapshotPath string) {
	if odProjectID == "" {
		return "", ""
	}
	client := s.defaultODClient()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	files, err := client.ListProjectFiles(ctx, odProjectID)
	if err != nil {
		s.addComment(t, project, agent, "design snapshot: list files failed: "+redactODDetail(err))
		return "", ""
	}
	artifacts := designSnapshotArtifacts(files)
	if len(artifacts) == 0 {
		s.addComment(t, project, agent, "design snapshot: no html artifacts in OD project "+odProjectID)
		return "", ""
	}

	manifest := designSnapshotManifest{
		Version:   1,
		TaskID:    t.ID,
		ProjectID: odProjectID,
		Source:    strings.TrimSpace(t.DesignSource),
		TakenAt:   time.Now().UTC(),
		Entry:     artifacts[0].Path,
	}

	// Fetch each artifact exactly once and cache the raw bytes aligned 1:1 with
	// `artifacts`. The former code re-fetched during persist via
	// `artifacts[i]` indexed by manifest.Files, which misaligned when a mid-list
	// fetch failed (manifest.Files shrank, artifacts did not).
	fetched := make([]fetchedDesignFile, len(artifacts))
	var inline string
	for i, art := range artifacts {
		raw, err := client.GetProjectFile(ctx, odProjectID, art.Path)
		if err != nil {
			s.addComment(t, project, agent, "design snapshot: fetch "+art.Path+" failed: "+redactODDetail(err))
			continue
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
	if len(manifest.Files) == 0 {
		return "", ""
	}

	// Observability: a companion (css/js) was requested but dropped because its
	// fetch failed. Fail-soft (the snapshot still ships) but make the
	// incompleteness visible instead of silently freezing a shell — the
	// 2026-09-03 4test incident shipped a 595-byte index.html with dangling
	// <link>/<script> refs precisely because this used to be invisible.
	if len(manifest.Files) < len(artifacts) {
		s.addComment(t, project, agent, "design snapshot: incomplete — captured "+
			strconv.Itoa(len(manifest.Files))+" of "+strconv.Itoa(len(artifacts))+
			" artifacts; see prior fetch errors above")
	}

	// Persist under the workspace's .multigent dir (same store root that owns
	// agency.yaml); the contract field carries the workspace-relative path so
	// consumers resolve it against s.st.Root().
	if root := strings.TrimSpace(s.st.Root()); root != "" {
		absDir := filepath.Join(root, designSnapshotDir, t.ID)
		if mkErr := os.MkdirAll(absDir, 0o755); mkErr == nil {
			for _, f := range fetched {
				if f.Raw == nil {
					continue
				}
				_ = os.WriteFile(filepath.Join(absDir, filepath.FromSlash(f.Path)), f.Raw, 0o644)
			}
			rawManifest, _ := json.MarshalIndent(manifest, "", "  ")
			_ = os.WriteFile(filepath.Join(absDir, "manifest.json"), rawManifest, 0o644)
			snapshotPath = designSnapshotDir + "/" + t.ID + "/manifest.json"
		}
	}
	return inline, snapshotPath
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
