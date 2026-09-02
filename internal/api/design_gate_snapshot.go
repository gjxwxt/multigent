package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
		manifest.Files = append(manifest.Files, designSnapshotFile{
			Path: art.Path, Kind: art.Kind, Size: int64(len(raw)),
		})
		if i == 0 {
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

	// Persist under the workspace's .multigent dir (same store root that owns
	// agency.yaml); the contract field carries the workspace-relative path so
	// consumers resolve it against s.st.Root().
	if root := strings.TrimSpace(s.st.Root()); root != "" {
		absDir := filepath.Join(root, designSnapshotDir, t.ID)
		if mkErr := os.MkdirAll(absDir, 0o755); mkErr == nil {
			for i := range manifest.Files {
				art := artifacts[i]
				if raw, err := client.GetProjectFile(ctx, odProjectID, art.Path); err == nil && int64(len(raw)) <= designSnapshotFetchCap {
					_ = os.WriteFile(filepath.Join(absDir, filepath.FromSlash(art.Path)), raw, 0o644)
				}
			}
			rawManifest, _ := json.MarshalIndent(manifest, "", "  ")
			_ = os.WriteFile(filepath.Join(absDir, "manifest.json"), rawManifest, 0o644)
			snapshotPath = designSnapshotDir + "/" + t.ID + "/manifest.json"
		}
	}
	return inline, snapshotPath
}

// designSnapshotArtifacts keeps html artifacts first (deterministic order),
// followed by css/js companions that make the snapshot self-contained.
func designSnapshotArtifacts(files []ODProjectFile) []ODProjectFile {
	keep := make([]ODProjectFile, 0, len(files))
	for _, f := range files {
		k := strings.ToLower(strings.TrimSpace(f.Kind))
		if k == "" {
			ext := strings.ToLower(filepath.Ext(f.Path))
			switch ext {
			case ".html", ".css", ".js":
				k = strings.TrimPrefix(ext, ".")
			default:
				continue
			}
		}
		if k == "html" || k == "css" || k == "js" {
			keep = append(keep, f)
		}
	}
	sort.SliceStable(keep, func(i, j int) bool {
		ki, kj := strings.ToLower(keep[i].Kind), strings.ToLower(keep[j].Kind)
		if ki != kj {
			return ki == "html"
		}
		return keep[i].Path < keep[j].Path
	})
	return keep
}

func redactODDetail(err error) string {
	detail := err.Error()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return detail
}
