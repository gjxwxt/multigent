// Real-change verification for the QA touched_paths checkpoint (round-18
// P0-4): the QA agent DECLARING paths is not evidence — the worktree's
// actual git delta is. When the workflow store carries a WorktreeResolver,
// a qa step that opted in (declares touched_paths as an output) must have
// its declared paths EXACTLY match the worktree's changed files: neither
// a business file modified but declared as a test artifact, nor "none"
// while the tree is dirty, passes.
package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/gitworktree"
)

// WorktreeResolver maps a (project, taskID) to its worktree directory.
// Empty string / not-found = the platform has no worktree for the task
// (e.g. non-code tasks): the real-change cross-check then degrades to the
// declaration-only gate instead of failing the step.
type WorktreeResolver func(project, taskID string) string

// worktreeChangedPaths returns the worktree's real delta as a path set:
// every path with unstaged/staged changes plus untracked files, from
// git status --porcelain. Paths are slash-normalized and git-quoted names
// are unquoted. Platform runtime dirs (.multigent/.git) are excluded.
func worktreeChangedPaths(worktreeDir string) ([]string, error) {
	cmd := exec.Command("git", "status", "--porcelain", "-z")
	cmd.Dir = worktreeDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", worktreeDir, err)
	}
	// -z: entries are NUL-separated; renames carry "new\0old\0" pairs.
	var paths []string
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		status, path := entry[:2], entry[3:]
		if status == "??" && strings.HasPrefix(path, ".multigent") {
			continue
		}
		if strings.HasPrefix(path, `"`) && strings.HasSuffix(path, `"`) {
			path = unquoteGitPath(path)
		}
		paths = append(paths, filepath.ToSlash(path))
		if status[0] == 'R' || status[1] == 'R' {
			// Rename entries carry the old path as the next NUL field.
			if i+1 < len(entries) {
				old := entries[i+1]
				if strings.HasPrefix(old, `"`) && strings.HasSuffix(old, `"`) {
					old = unquoteGitPath(old)
				}
				paths = append(paths, filepath.ToSlash(old))
				i++
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// unquoteGitPath decodes a C-quoted git path (octal escapes, backslashes).
func unquoteGitPath(p string) string {
	p = p[1 : len(p)-1]
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) && p[i+1] >= '0' && p[i+1] <= '7' {
			var v int
			_, _ = fmt.Sscanf(p[i+1:i+4], "%o", &v)
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		if p[i] == '\\' && i+1 < len(p) {
			i++
			b.WriteByte(p[i])
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// worktreeObservable reports whether dir exists AND is a readable git
// worktree. The QA real-change gate is fail-closed (round-19 P0): a
// declared touched_paths step whose worktree is missing or unreadable must
// NOT silently downgrade to declaration-only — the caller rejects the
// completion instead. (An unreadable-but-existing dir also fails here; the
// distinction is surfaced by the gate's error message.)
func worktreeObservable(worktreeDir string) bool {
	info, err := os.Stat(worktreeDir)
	if err != nil || !info.IsDir() {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = worktreeDir
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// verifyQATouchedPathsAgainstWorktree cross-checks the declared paths
// against the worktree's real delta. Both directions must hold:
//
//   - every REAL change is declared (a business file modified without
//     declaration = the drift the gate exists to catch; "none" with a dirty
//     tree fails here);
//   - every DECLARED path is really changed (claiming test-file edits that
//     did not happen is treated the same way — declarations must be true).
//
// The whitelist (test-artifact classification) then applies to the REAL
// delta's paths via the same ValidateQATouchedPaths used for declarations,
// so the accepted surface is identical no matter which side produced it.
func verifyQATouchedPathsAgainstWorktree(declared, worktreeDir string) error {
	real, err := worktreeChangedPaths(worktreeDir)
	if err != nil {
		return fmt.Errorf("read worktree changes: %w", err)
	}
	declaredSet := map[string]bool{}
	if strings.TrimSpace(declared) != "" {
		for _, line := range strings.Split(declared, "\n") {
			p := strings.TrimSpace(line)
			if p != "" {
				declaredSet[filepath.ToSlash(p)] = true
			}
		}
	}
	declaredNone := strings.EqualFold(strings.TrimSpace(declared), "none")

	realSet := map[string]bool{}
	for _, p := range real {
		realSet[p] = true
	}

	// Direction 1: every real change must be declared.
	var undeclared []string
	for _, p := range real {
		if !declaredSet[p] {
			undeclared = append(undeclared, p)
		}
	}
	if len(undeclared) > 0 {
		return fmt.Errorf("worktree changes not declared in touched_paths: %s — declare every changed path (or revert)", strings.Join(undeclared, ", "))
	}
	if declaredNone && len(real) > 0 {
		return fmt.Errorf("touched_paths declared \"none\" but the worktree holds %d changed path(s): %s", len(real), strings.Join(real, ", "))
	}

	// Direction 2: every declared path must be really changed (a literal
	// "none" declares no paths, so it only faces direction 1).
	var phantom []string
	for p := range declaredSet {
		if !declaredNone && !realSet[p] {
			phantom = append(phantom, p)
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		return fmt.Errorf("declared paths with no worktree change: %s — touched_paths must list only actually changed files", strings.Join(phantom, ", "))
	}

	// Direction 3: the whitelist applies to the REAL delta (same classifier
	// as the declaration gate) — a declared-and-real business file still
	// fails; declaration cannot launder a path past the test-artifact rule.
	if len(real) > 0 {
		if err := ValidateQATouchedPaths(strings.Join(real, "\n")); err != nil {
			return fmt.Errorf("worktree changes fail the test-artifact rule: %w", err)
		}
	}
	return nil
}
