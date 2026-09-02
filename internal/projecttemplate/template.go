// Package projecttemplate materializes versioned, reviewable project starters.
package projecttemplate

import (
	"bytes"
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
	ReactGoFullstackVersion = "1.1.0"
)

// CI baseline assets shipped with the starter. They are exported separately
// (see CIBaselineFiles) so existing repositories can be upgraded to the
// CI/CD chain without re-materializing the whole starter.
var ciBaselinePaths = []string{".gitlab-ci.yml", "deploy/Dockerfile", "deploy/compose.yml"}

//go:embed files/react_go_fullstack/* files/react_go_fullstack/.gitignore files/react_go_fullstack/.env.example files/react_go_fullstack/.gitlab-ci.yml files/react_go_fullstack/.multigent/* files/react_go_fullstack/deploy/* files/react_go_fullstack/web/* files/react_go_fullstack/web/src/* files/react_go_fullstack/server/*
var templateFiles embed.FS

type Report struct {
	ID              string   `json:"templateId"`
	Version         string   `json:"templateVersion"`
	Digest          string   `json:"templateDigest"`
	Files           []string `json:"files"`
	RuntimeContract string   `json:"runtimeContract"`
}

type fileEntry struct {
	path string
	data []byte
}

func templateEntries(templateID string) ([]fileEntry, Report, error) {
	if strings.TrimSpace(templateID) != ReactGoFullstackID {
		return nil, Report{}, fmt.Errorf("unsupported project template %q", templateID)
	}

	var files []fileEntry
	err := fs.WalkDir(templateFiles, "files/react_go_fullstack", func(path string, entry fs.DirEntry, walkErr error) error {
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
		return nil, Report{}, fmt.Errorf("read %s template: %w", ReactGoFullstackID, err)
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
	report := Report{
		ID:              ReactGoFullstackID,
		Version:         ReactGoFullstackVersion,
		Digest:          hex.EncodeToString(digest[:]),
		RuntimeContract: filepath.ToSlash(filepath.Join(".multigent", "runtime.json")),
	}
	return files, report, nil
}

// CIBaselineFiles returns the CI/CD baseline assets (repo-relative path ->
// content) so deterministic tooling can seed them into existing repositories
// without touching any other starter file.
func CIBaselineFiles() (map[string][]byte, error) {
	files, _, err := templateEntries(ReactGoFullstackID)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(ciBaselinePaths))
	for _, p := range ciBaselinePaths {
		wanted[p] = true
	}
	out := make(map[string][]byte, len(ciBaselinePaths))
	for _, file := range files {
		if wanted[file.path] {
			out[file.path] = file.data
		}
	}
	if len(out) != len(ciBaselinePaths) {
		return nil, fmt.Errorf("CI baseline incomplete: %d/%d files", len(out), len(ciBaselinePaths))
	}
	return out, nil
}

// Materialize writes a new template into an empty directory. Existing files
// are never overwritten; callers must explicitly choose a new/empty target.
func Materialize(root, templateID string) (Report, error) {
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
	files, report, err := templateEntries(templateID)
	if err != nil {
		return Report{}, err
	}
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
	report.Files = created
	return report, nil
}

// Seed adds a deterministic template to an existing Agent workspace without
// touching platform-owned runtime files or user files. Existing template files
// are accepted only when their contents are identical, making retries safe.
func Seed(root, templateID string) (Report, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return Report{}, fmt.Errorf("template seed directory is required")
	}
	files, report, err := templateEntries(templateID)
	if err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return Report{}, fmt.Errorf("create template seed directory: %w", err)
	}

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
		info, err := os.Lstat(target)
		if err == nil {
			if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return Report{}, fmt.Errorf("template seed target is not a regular file: %s", file.path)
			}
			existing, readErr := os.ReadFile(target)
			if readErr != nil {
				return Report{}, fmt.Errorf("read existing template file %s: %w", file.path, readErr)
			}
			if !bytes.Equal(existing, file.data) {
				return Report{}, fmt.Errorf("template seed would overwrite existing file: %s", file.path)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return Report{}, fmt.Errorf("inspect template seed file %s: %w", file.path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return Report{}, fmt.Errorf("create template seed directory for %s: %w", file.path, err)
		}
		if err := os.WriteFile(target, file.data, 0644); err != nil {
			return Report{}, fmt.Errorf("write template seed file %s: %w", file.path, err)
		}
		created = append(created, file.path)
	}
	cleanup = false
	report.Files = make([]string, 0, len(files))
	for _, file := range files {
		report.Files = append(report.Files, file.path)
	}
	return report, nil
}
