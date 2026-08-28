// Package projecttemplate materializes versioned, reviewable project starters.
package projecttemplate

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ReactGoFullstackID      = "react_go_fullstack"
	ReactGoFullstackVersion = "1.0.0"
)

//go:embed files/react_go_fullstack/* files/react_go_fullstack/.gitignore files/react_go_fullstack/.env.example files/react_go_fullstack/.multigent/* files/react_go_fullstack/web/* files/react_go_fullstack/web/src/* files/react_go_fullstack/server/*
var templateFiles embed.FS

type Report struct {
	ID              string   `json:"templateId"`
	Version         string   `json:"templateVersion"`
	Digest          string   `json:"templateDigest"`
	Files           []string `json:"files"`
	RuntimeContract string   `json:"runtimeContract"`
}

// Materialize writes a new template into an empty directory. Existing files
// are never overwritten; callers must explicitly choose a new/empty target.
func Materialize(root, templateID string) (Report, error) {
	if strings.TrimSpace(templateID) != ReactGoFullstackID {
		return Report{}, fmt.Errorf("unsupported project template %q", templateID)
	}
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return Report{}, fmt.Errorf("template target directory is required")
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return Report{}, fmt.Errorf("create template target: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Report{}, fmt.Errorf("inspect template target: %w", err)
	}
	if len(entries) != 0 {
		return Report{}, fmt.Errorf("template target must be empty: %s", root)
	}

	type fileEntry struct {
		path string
		data []byte
	}
	var files []fileEntry
	err = fs.WalkDir(templateFiles, "files/react_go_fullstack", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("files/react_go_fullstack", filepath.FromSlash(path))
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid template path %q", path)
		}
		data, err := templateFiles.ReadFile(path)
		if err != nil {
			return err
		}
		rel = filepath.Clean(rel)
		// Go refuses to embed a nested go.mod as a second module. Keep the
		// source asset reviewable as go.mod.template and materialize the real
		// module file name.
		if strings.HasSuffix(rel, ".template") {
			rel = strings.TrimSuffix(rel, ".template")
		}
		files = append(files, fileEntry{path: rel, data: data})
		return nil
	})
	if err != nil {
		return Report{}, fmt.Errorf("read %s template: %w", ReactGoFullstackID, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	// WalkDir order is stable today, but sort the digest input as well so the
	// recorded identity cannot depend on embed traversal details.
	var canonical []byte
	for _, file := range files {
		canonical = append(canonical, []byte(file.path+"\x00")...)
		canonical = append(canonical, file.data...)
		canonical = append(canonical, 0)
	}
	digest := sha256.Sum256(canonical)
	created := make([]string, 0, len(files))
	cleanup := true
	defer func() {
		if cleanup {
			for i := len(created) - 1; i >= 0; i-- {
				_ = os.Remove(filepath.Join(root, filepath.FromSlash(created[i])))
			}
		}
	}()
	for _, file := range files {
		target := filepath.Join(root, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return Report{}, fmt.Errorf("create template directory: %w", err)
		}
		if err := os.WriteFile(target, file.data, 0644); err != nil {
			return Report{}, fmt.Errorf("write template file %s: %w", file.path, err)
		}
		created = append(created, file.path)
	}
	cleanup = false
	return Report{
		ID:              ReactGoFullstackID,
		Version:         ReactGoFullstackVersion,
		Digest:          hex.EncodeToString(digest[:]),
		Files:           created,
		RuntimeContract: filepath.ToSlash(filepath.Join(".multigent", "runtime.json")),
	}, nil
}
