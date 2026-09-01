package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// WorktreeParentMount returns a docker bind-mount argument ("-v src:dst")
// that makes the parent repository of a linked git worktree reachable inside
// the container at the exact absolute host path recorded in the worktree's
// `.git` file, or "" when workspaceDir is not a linked worktree whose gitdir
// lies outside the mounted directory. readOnly mirrors the worktree mount's
// own read-only mode.
//
// A linked worktree's `.git` file records its admin directory as an absolute
// host path (`<parent>/.git/worktrees/<id>`). When a container mounts only
// the worktree directory, that path never resolves inside the container, so
// every git command fails ("not a git repository") and git even marks the
// registration prunable — a `git worktree prune` run inside such a container
// destroys the shared registry of sibling worktrees. Mounting the parent at
// its recorded path makes git work normally and renders prune harmless.
func WorktreeParentMount(workspaceDir string, readOnly bool) string {
	ws, err := filepath.Abs(strings.TrimSpace(workspaceDir))
	if err != nil || ws == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".git"))
	if err != nil {
		// A real `.git` directory (main checkout) or no repository at all.
		return ""
	}
	line := strings.TrimSpace(string(raw))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if gitdir == "" {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(ws, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	// Expected shape: <parent>/.git/worktrees/<id>
	sep := string(filepath.Separator)
	idx := strings.LastIndex(gitdir, sep+"worktrees"+sep)
	if idx < 0 {
		return ""
	}
	parentGit := gitdir[:idx]
	if filepath.Base(parentGit) != ".git" {
		return ""
	}
	parent := filepath.Dir(parentGit)
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return ""
	}
	// Already covered by the workspace mount itself.
	if parent == ws || strings.HasPrefix(parent+sep, ws+sep) {
		return ""
	}
	mount := parent + ":" + parent
	if readOnly {
		mount += ":ro"
	}
	return mount
}
