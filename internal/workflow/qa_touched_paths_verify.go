// Real-change verification for the QA touched_paths checkpoint (round-18
// P0-4): the QA agent DECLARING paths is not evidence — the worktree's
// actual git delta is. When the workflow store carries a WorktreeResolver,
// a qa step that opted in (declares touched_paths as an output) must have
// its declared paths EXACTLY match the worktree's changed files: neither
// a business file modified but declared as a test artifact, nor "none"
// while the tree is dirty, passes.
package workflow

import (
	"errors"
	"fmt"
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

// qaBaselineSurface bundles the trust decision for one gate invocation:
// WHICH measurement surface to use and the baseline document to diff
// against (nil for the legacy absolute surface).
//
// Surface decision (S2-2, reviewer P0 — trust model):
//   - control-plane baseline found → baseline-delta surface (trusted;
//     the document lives in kv_records, agents cannot touch it);
//   - baseline absent but the worktree's capture manifest proves one
//     EXISTED (new-task tamper/loss case) → ErrQABaselineLost, the gate
//     FAILS CLOSED instead of degrading to a weaker surface;
//   - baseline absent, no manifest (legacy worktree that never had a
//     baseline) → the old absolute git-status surface, still strict.
type qaBaselineSurface struct {
	baseline *gitworktree.QABaseline
	// deliveryDelta marks a DELIVERY checkpoint (branch join, S2-2 ⑤):
	// the branch agent's whole delta vs its baseline IS the deliverable,
	// so Direction 3 (test-artifact whitelist) must NOT apply — a branch
	// delivering business code would otherwise be structurally rejected.
	// The linear QA checkpoint keeps the whitelist: QA edits a business
	// file "to write a regression test" must keep failing.
	deliveryDelta bool
}

// resolveQABaselineSurface decides the measurement surface for a gate run.
// lookup may be nil (store built without control-plane access — tests);
// a nil lookup is treated as "no baseline anywhere" and falls through to
// the manifest check so tamper detection still applies.
func resolveQABaselineSurface(worktreeDir string, lookup gitworktree.QABaselineLookup, project, taskID string) (qaBaselineSurface, error) {
	if lookup == nil {
		b, err := gitworktree.LoadQABaselineForGate(worktreeDir, nil, project, taskID)
		switch {
		case err == nil:
			bb := b
			return qaBaselineSurface{baseline: &bb}, nil
		case errors.Is(err, gitworktree.ErrNoQABaseline):
			return qaBaselineSurface{}, nil
		default:
			return qaBaselineSurface{}, err
		}
	}
	b, err := gitworktree.LoadQABaselineForGate(worktreeDir, lookup, project, taskID)
	switch {
	case err == nil:
		bb := b
		return qaBaselineSurface{baseline: &bb}, nil
	case errors.Is(err, gitworktree.ErrNoQABaseline):
		return qaBaselineSurface{}, nil
	default:
		return qaBaselineSurface{}, err
	}
}

// realDelta returns the gate's measurement surface for one gate invocation:
// baseline delta when a trusted baseline exists, absolute status otherwise.
func (surf qaBaselineSurface) realDelta(worktreeDir string) ([]string, error) {
	if surf.baseline != nil {
		return gitworktree.QABaselineWorktreeDelta(worktreeDir, *surf.baseline)
	}
	return worktreeChangedPaths(worktreeDir)
}

// verifyWorktreeDeltaAgainstDeclaration cross-checks ONE gate's declared
// paths against ONE measurement surface (see
// verifyQATouchedPathsAgainstWorktree for the full contract text); the
// surface parameter is what makes the two checkpoint kinds distinct
// (S2-2, ⑤): callers pass the surface matching their own scope.
func verifyWorktreeDeltaAgainstDeclaration(declared string, worktreeDir string, surf qaBaselineSurface) error {
	real, err := surf.realDelta(worktreeDir)
	if err != nil {
		return fmt.Errorf("read worktree changes: %w", err)
	}
	return surf.verifyDeclared(declared, real)
}

// verifyDeclared applies the declaration contract (directions 1–3, same
// wording as the historical implementation) to a concrete real-change set.
// The checkpoint kind lives on the surface: deliveryDelta suppresses
// Direction 3 (the test-artifact whitelist), the legacy QA surface keeps it.
func (surf qaBaselineSurface) verifyDeclared(declared string, real []string) error {
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
	// Delivery checkpoints (branch joins, surf.deliveryDelta) skip it: the
	// whole baseline delta is the deliverable there (S2-2 ⑤).
	if !surf.deliveryDelta && len(real) > 0 {
		if err := ValidateQATouchedPaths(strings.Join(real, "\n")); err != nil {
			return fmt.Errorf("worktree changes fail the test-artifact rule: %w", err)
		}
	}
	return nil
}

// unquoteGitPath decodes a C-quoted git path (octal escapes, backslashes).
func unquoteGitPath(p string) string {
	if len(p) < 2 || p[0] != '"' || p[len(p)-1] != '"' {
		return p
	}
	p = p[1 : len(p)-1]
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+1 < len(p) && p[i+1] >= '0' && p[i+1] <= '7' {
			// Exactly three octal digits (git's C-quote format); a short or
			// non-octal tail is kept literal instead of silently decoding.
			if i+3 >= len(p) || p[i+2] < '0' || p[i+2] > '7' || p[i+3] < '0' || p[i+3] > '7' {
				b.WriteByte(p[i])
				continue
			}
			v := int(p[i+1]-'0')*64 + int(p[i+2]-'0')*8 + int(p[i+3]-'0')
			if v > 255 {
				b.WriteByte(p[i])
				continue
			}
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

// worktreeObservable reports whether dir is an observable git worktree for
// the QA real-change gate. It delegates to gitworktree.ObservableWorktree —
// the single shared implementation — so the gate and the api-side resolver
// (which skips resolver candidates by the same test) can never drift apart
// (D-N: a resolver/gate disagreement is exactly what wedged qa completions
// behind "requires an observable worktree").
func worktreeObservable(worktreeDir string) bool {
	return gitworktree.ObservableWorktree(worktreeDir)
}

// verifyQATouchedPathsAgainstWorktree cross-checks the declared paths
// against the LEGACY absolute status surface. S2-2 ⑤: this helper no longer
// decides the surface — the two checkpoint kinds carry different scopes and
// call verifyWorktreeDeltaAgainstDeclaration with their own
// qaBaselineSurface. The legacy surface keeps the strict historical
// semantics (absolute status, both directions, test-artifact whitelist).
func verifyQATouchedPathsAgainstWorktree(declared, worktreeDir string) error {
	return verifyWorktreeDeltaAgainstDeclaration(declared, worktreeDir, qaBaselineSurface{})
}
