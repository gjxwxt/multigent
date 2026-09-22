package gitworktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// QA baseline snapshots (fix round S2-1, Fix B): the touched_paths
// real-change gate previously measured the worktree's ABSOLUTE git status,
// so platform-scaffolded untracked files (.cursor/, .mcp.json, ...) and
// pre-existing dirty state that predate the task were misattributed to the
// task and could fail an honest completion. The fix records a content-
// fingerprinted baseline at the protected moment the platform materializes
// the worktree (before the agent ever runs), and the gate later diffs the
// current state against that baseline: the gate then measures the task's
// DELIVERY DELTA, not whatever noise the base checkout carried.
//
// The baseline is stored INSIDE the worktree under .multigent/ (already
// excluded from worktreeChangedPaths and from the QA gate's own surface),
// so it travels with the worktree, needs no extra DB plumbing, and dies
// with the worktree on cleanup. Its JSON carries a schemaVersion so future
// format changes are detectable.

// QABaselineSchemaVersion is bumped on incompatible baseline format changes.
const QABaselineSchemaVersion = 1

// qaBaselineFileName is the baseline document stored inside the worktree.
const qaBaselineFileName = "qa_baseline.json"

// QABaselineEntry is one path's state at baseline time. Fingerprint is a
// content hash that distinguishes "same path, changed content" from
// "same path, untouched" — a plain path set cannot do that (a file that
// was modified before the task AND modified again by the task must still
// be reported as changed).
type QABaselineEntry struct {
	Fingerprint string `json:"fingerprint"`
}

// QABaseline is the persisted baseline document.
type QABaseline struct {
	SchemaVersion int                        `json:"schemaVersion"`
	RecordedAt    string                     `json:"recordedAt"`
	HeadCommit    string                     `json:"headCommit"`
	Entries       map[string]QABaselineEntry `json:"entries"`
}

// CaptureQABaseline records the worktree's current file state as the task's
// baseline, keyed by path with content fingerprints. It must be called at
// the protected materialization point — right after `git worktree add`,
// before any agent execution — by the platform only; agents have no
// function that writes this file. A best-effort contract: failure returns
// an error so the caller can decide (worktree creation itself is not
// rolled back, but the caller logs the warning and the gate falls back to
// fail-closed absolute measurement when no baseline exists).
func CaptureQABaseline(worktreeDir string) error {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return fmt.Errorf("worktree dir is required")
	}
	head := ""
	if out, err := gitQuiet(worktreeDir, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}
	paths, err := worktreeListAllFiles(worktreeDir)
	if err != nil {
		return err
	}
	entries := make(map[string]QABaselineEntry, len(paths))
	for _, p := range paths {
		fp, err := fileFingerprint(filepath.Join(worktreeDir, filepath.FromSlash(p)))
		if err != nil {
			return fmt.Errorf("fingerprint %s: %w", p, err)
		}
		entries[p] = QABaselineEntry{Fingerprint: fp}
	}
	baseline := QABaseline{
		SchemaVersion: QABaselineSchemaVersion,
		HeadCommit:    head,
		Entries:       entries,
	}
	payload, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(QABaselinePath(worktreeDir)), 0o755); err != nil {
		return fmt.Errorf("create baseline dir: %w", err)
	}
	return os.WriteFile(QABaselinePath(worktreeDir), payload, 0o644)
}

// QABaselinePath is where the baseline document lives inside a worktree.
func QABaselinePath(worktreeDir string) string {
	return filepath.Join(worktreeDir, ".multigent", qaBaselineFileName)
}

// LoadQABaseline reads the recorded baseline, if any. ok=false means the
// worktree has no usable baseline (created before this fix, or capture
// failed) — callers must then fall back to the previous fail-closed
// absolute measurement instead of inventing a baseline from current state
// (a baseline taken at completion time would launder the very changes the
// gate is supposed to catch).
func LoadQABaseline(worktreeDir string) (QABaseline, bool, error) {
	var baseline QABaseline
	payload, err := os.ReadFile(QABaselinePath(worktreeDir))
	if err != nil {
		if os.IsNotExist(err) {
			return baseline, false, nil
		}
		return baseline, false, err
	}
	if err := json.Unmarshal(payload, &baseline); err != nil {
		return baseline, false, err
	}
	if baseline.SchemaVersion != QABaselineSchemaVersion {
		return baseline, false, fmt.Errorf("unsupported qa baseline schema version %d", baseline.SchemaVersion)
	}
	if baseline.Entries == nil {
		return baseline, false, fmt.Errorf("qa baseline has no entries map")
	}
	return baseline, true, nil
}

// worktreeListAllFiles lists every tracked plus untracked (excluding
// .multigent/) file path currently present in the worktree, as
// existence+fingerprint entries so later diffs can detect modifications,
// deletions, and committed content. It is NOT the git-status delta surface:
// `git status --porcelain` only lists CHANGED paths, which would hide a
// committed change (status clean after commit) from the fingerprint
// comparison. Tracked files come from `git ls-files`, untracked files from
// `git status --porcelain -z --untracked-files=all`.
func worktreeListAllFiles(worktreeDir string) ([]string, error) {
	// NOTE: the -z porcelain output's leading status byte can be a SPACE
	// (e.g. " M file"), so the output must NEVER pass through a trimming
	// helper — TrimSpace would eat the leading space and shift every field
	// by one (" M server.go" parsed as path "erver.go"). Read raw bytes,
	// exactly like worktreeChangedPaths does.
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		p = filepath.ToSlash(p)
		if p == "" || strings.HasPrefix(p, ".multigent/") {
			return
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	// Tracked files: `git ls-files -z` lists EVERY tracked path regardless
	// of change state, so committed work stays visible to the fingerprint
	// diff.
	cmdLS, cancelLS := gitLocal(worktreeDir, "ls-files", "-z")
	defer cancelLS()
	lsRaw, err := cmdLS.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", worktreeDir, err)
	}
	for _, p := range strings.Split(string(lsRaw), "\x00") {
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) {
			p = unquoteGitPath(p)
		}
		add(p)
	}
	// Untracked files (incl. untracked dirs expanded): raw -z porcelain,
	// no trimming — leading status spaces are structural.
	cmd, cancel := gitLocal(worktreeDir, "status", "--porcelain", "-z", "--untracked-files=all")
	defer cancel()
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", worktreeDir, err)
	}
	entries := strings.Split(string(raw), "\x00")
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
		add(path)
		if status[0] == 'R' || status[1] == 'R' {
			if i+1 < len(entries) {
				old := entries[i+1]
				if strings.HasPrefix(old, `"`) && strings.HasSuffix(old, `"`) {
					old = unquoteGitPath(old)
				}
				add(old)
				i++
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// QABaselineWorktreeDelta diffs the worktree's current state against the
// recorded baseline and returns the task's delivery delta paths:
//   - files added since baseline (tracked additions and untracked files),
//   - files whose content fingerprint changed (same-path re-modification),
//   - deleted files (the pre-task path is reported — it must be declared),
//   - rename old/new paths both reported.
//
// Paths are slash-normalized and sorted. Baseline paths that no longer
// exist on disk are reported as deletions under their baseline path.
func QABaselineWorktreeDelta(worktreeDir string) ([]string, error) {
	baseline, ok, err := LoadQABaseline(worktreeDir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoQABaseline
	}
	current, err := worktreeListAllFiles(worktreeDir)
	if err != nil {
		return nil, err
	}
	currentSet := make(map[string]bool, len(current))
	for _, p := range current {
		currentSet[p] = true
	}
	var delta []string
	// Additions and content changes.
	for _, p := range current {
		base, existed := baseline.Entries[p]
		if !existed {
			delta = append(delta, p)
			continue
		}
		fp, fpErr := fileFingerprint(filepath.Join(worktreeDir, filepath.FromSlash(p)))
		if fpErr != nil {
			return nil, fmt.Errorf("fingerprint %s: %w", p, fpErr)
		}
		if fp != base.Fingerprint {
			delta = append(delta, p)
		}
	}
	// Deletions: baseline paths gone from disk. (A file deleted AND
	// re-added with identical content is indistinguishable from untouched
	// here, which is the correct lenient reading: the delivered content
	// equals the baseline.)
	for p := range baseline.Entries {
		if !currentSet[p] {
			delta = append(delta, p)
		}
	}
	sort.Strings(delta)
	return delta, nil
}

// ErrNoQABaseline marks a worktree without a usable baseline document.
var ErrNoQABaseline = fmt.Errorf("no qa baseline recorded for this worktree")

// gitQuiet runs a local git command and returns trimmed stdout. Only safe
// for outputs whose leading/trailing bytes carry no field structure (e.g.
// rev-parse); NEVER use it for -z porcelain output (see worktreeListAllFiles).
func gitQuiet(dir string, args ...string) (string, error) {
	cmd, cancel := gitLocal(dir, args...)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
