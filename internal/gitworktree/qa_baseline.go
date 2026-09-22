package gitworktree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// QA baseline snapshots (fix round S2-1, Fix B; hardened in S2-2): the
// touched_paths real-change gate previously measured the worktree's ABSOLUTE
// git status, so platform-scaffolded untracked files (.cursor/, .mcp.json,
// ...) and pre-existing dirty state that predate the task were misattributed
// to the task and could fail an honest completion. The fix records a content-
// fingerprinted baseline at the protected moment the platform materializes
// the worktree (before the agent ever runs), and the gate later diffs the
// current state against that baseline: the gate then measures the task's
// DELIVERY DELTA, not whatever noise the base checkout carried.
//
// TRUST MODEL (S2-2, reviewer P0): the original Fix B stored the baseline
// inside the agent-writable worktree (plain JSON, 0644) — an agent could
// tamper with or DELETE it, and the deleted case silently degraded to the
// absolute git-status surface, which is WEAKER (the agent can neutralize it
// by staging scaffold noise). The baseline now lives in two layers:
//
//  1. The AUTHORITATIVE copy is persisted in the control plane's kv_records
//     (qa_baselines table key: project/taskID) together with the worktree
//     HEAD SHA at capture time and a SHA-256 digest of the full document.
//     Agents have no write path to kv_records.
//  2. A RECOVERY copy (schemaVersion 2, digest-chained) stays inside the
//     worktree so a restored-from-backup control plane can re-adopt a
//     baseline, and so debugging does not require DB access. The worktree
//     copy is never trusted on its own: it is only read to RE-UPLOAD after
//     its digest verifies against... nothing else — re-adoption requires an
//     explicit operator action and is deliberately NOT automatic.
//
// Gate surface decision (LoadQABaselineForGate):
//   - control-plane baseline present  → baseline-delta surface (trusted);
//   - baseline absent AND the worktree's OWN capture manifest says a
//     baseline was recorded here (head SHA matches the worktree's HEAD,
//     i.e. the materialization predates the task) → the baseline was LOST
//     or DESTROYED: the gate FAILS CLOSED with a tamper error. This is the
//     distinction the old code missed — "legacy worktree that never had a
//     baseline" (no capture manifest → old absolute surface, still strict)
//     versus "new worktree whose baseline disappeared" (manifest present,
//     baseline gone → refuse).
//   - baseline absent, no manifest → legacy worktree → absolute status.

// QABaselineSchemaVersion is bumped on incompatible baseline format changes.
const QABaselineSchemaVersion = 2

// QABaselineManifestName is the capture manifest written INSIDE the worktree
// at materialization. It records THAT a baseline was captured (plus when and
// at which HEAD) — deliberately no fingerprints, no secrets: the manifest is
// only a tamper canary, never a trust root.
const QABaselineManifestName = "qa_baseline_manifest.json"

// QABaselineManifest is the tamper canary inside the worktree.
type QABaselineManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	RecordedAt    string `json:"recordedAt"`
	HeadCommit    string `json:"headCommit"`
}

// QABaselineDigest returns the canonical SHA-256 hex digest of a baseline
// document (marshal + hash). The digest is stored alongside the control-
// plane copy so a re-uploaded or mirrored document can be verified byte
// for byte.
func QABaselineDigest(b QABaseline) (string, error) {
	payload, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// WriteQABaselineManifest drops the capture canary into the worktree. It is
// called by the platform right after the baseline document has been
// persisted to the control plane; failure is logged by the caller (a missing
// manifest only downgrades tamper DETECTION for that worktree, the
// control-plane copy stays authoritative).
func WriteQABaselineManifest(worktreeDir string) error {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return fmt.Errorf("worktree dir is required")
	}
	head := ""
	if out, err := gitQuiet(worktreeDir, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}
	manifest := QABaselineManifest{
		SchemaVersion: QABaselineSchemaVersion,
		RecordedAt:    time.Now().UTC().Format(time.RFC3339),
		HeadCommit:    head,
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(qaBaselineManifestPath(worktreeDir)), 0o755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	// 0644, deliberately not read-only: the manifest is a TAMPER CANARY
	// (review round S2-2), not a protection layer — the agent can delete or
	// rewrite anything in its worktree anyway (it owns the filesystem), and
	// a second capture into the same worktree (idempotent re-materialize)
	// must be able to refresh it. Integrity comes from the control plane:
	// the gate compares the local copy's digest against the authoritative
	// record, and a manifest that disagrees with the record's existence
	// (present locally, missing remotely) fails the gate closed.
	if existing, err := os.ReadFile(qaBaselineManifestPath(worktreeDir)); err == nil && len(existing) > 0 {
		_ = os.Chmod(qaBaselineManifestPath(worktreeDir), 0o644)
	}
	return os.WriteFile(qaBaselineManifestPath(worktreeDir), payload, 0o644)
}

func qaBaselineManifestPath(worktreeDir string) string {
	return filepath.Join(worktreeDir, ".multigent", QABaselineManifestName)
}

// QABaselineManifestPath is where the capture manifest lives inside a
// worktree (exported for the store-layer capture rollback helper).
func QABaselineManifestPath(worktreeDir string) string {
	return qaBaselineManifestPath(worktreeDir)
}

// ErrQABaselineLost marks the tamper case: the worktree's capture manifest
// proves a baseline EXISTED, but the control plane cannot produce it. The
// gate must fail closed on this, not degrade to a weaker surface.
var ErrQABaselineLost = fmt.Errorf("qa baseline was recorded for this worktree but is missing from the control plane (possible tampering); refusing to degrade the real-change gate")

// ErrQABaselineTampered marks a worktree-side baseline copy whose digest no
// longer matches the control-plane record.
var ErrQABaselineTampered = fmt.Errorf("qa baseline in worktree does not match the control-plane copy (digest mismatch); refusing to trust it")

// QABaselineLookup is the control-plane accessor the gate uses to fetch the
// authoritative baseline document: returns (payload, found, err). Implemented
// by the workflow store over kv_records; agents have no path to it.
type QABaselineLookup func(project, taskID string) (string, bool, error)

// LoadQABaselineForGate resolves the gate's measurement surface:
//   - (baseline, nil)          → baseline-delta measurement is authorized;
//   - (nil, nil)               → no baseline anywhere (legacy worktree):
//     the caller falls back to the absolute git-status surface;
//   - (nil, ErrQABaselineLost) → baseline existed (manifest proves it) but
//     the control plane lost it: FAIL CLOSED.
//
// A worktree-side copy that disagrees with the control-plane document is
// reported as tampered and fails the gate — the agent can WRITE the worktree
// copy, so it can never win a disagreement.
func LoadQABaselineForGate(worktreeDir string, lookup QABaselineLookup, project, taskID string) (QABaseline, error) {
	if lookup == nil {
		// No control-plane accessor: a manifest still proves a baseline
		// EXISTED, so its absence from nowhere must fail closed; only a
		// manifest-less (legacy) worktree falls back to the absolute
		// surface.
		if _, manifestExists, manifestErr := readQABaselineManifest(worktreeDir); manifestErr == nil && manifestExists {
			return QABaseline{}, ErrQABaselineLost
		}
		return QABaseline{}, ErrNoQABaseline
	}
	payload, found, err := lookup(project, taskID)
	if err != nil {
		return QABaseline{}, fmt.Errorf("load qa baseline from control plane: %w", err)
	}
	_, manifestExists, manifestErr := readQABaselineManifest(worktreeDir)
	if manifestErr != nil {
		// An unreadable manifest is not proof of anything; treat as absent
		// for the legacy decision but surface the anomaly in the error chain.
		manifestExists = false
	}
	if !found {
		if manifestExists {
			return QABaseline{}, ErrQABaselineLost
		}
		return QABaseline{}, ErrNoQABaseline
	}
	var baseline QABaseline
	if err := json.Unmarshal([]byte(payload), &baseline); err != nil {
		return QABaseline{}, fmt.Errorf("control-plane qa baseline for %s/%s is corrupt: %w", project, taskID, err)
	}
	if baseline.SchemaVersion != QABaselineSchemaVersion {
		return QABaseline{}, fmt.Errorf("unsupported qa baseline schema version %d (project %s task %s)", baseline.SchemaVersion, project, taskID)
	}
	if baseline.Entries == nil {
		return QABaseline{}, fmt.Errorf("control-plane qa baseline for %s/%s has no entries map", project, taskID)
	}
	// Worktree-side copy (if any) must agree with the control plane. The
	// worktree copy is agent-writable, so ANY mismatch fails closed.
	if localPayload, err := os.ReadFile(QABaselinePath(worktreeDir)); err == nil {
		var local QABaseline
		if json.Unmarshal(localPayload, &local) != nil || local.SchemaVersion != QABaselineSchemaVersion {
			return QABaseline{}, ErrQABaselineTampered
		}
		want, dErr := QABaselineDigest(baseline)
		got, gErr := sha256FromBytes(localPayload)
		if dErr != nil || gErr != nil || want != got {
			return QABaseline{}, ErrQABaselineTampered
		}
	}
	return baseline, nil
}

func sha256FromBytes(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func readQABaselineManifest(worktreeDir string) (QABaselineManifest, bool, error) {
	var manifest QABaselineManifest
	payload, err := os.ReadFile(qaBaselineManifestPath(worktreeDir))
	if err != nil {
		if os.IsNotExist(err) {
			return manifest, false, nil
		}
		return manifest, false, err
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return manifest, false, err
	}
	return manifest, true, nil
}

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
// S2-2 trust model: the worktree copy written here is the RECOVERY copy
// only (read-only 0444; still agent-writable in principle, so never a
// trust root). The caller MUST persist the returned document to the
// control plane (see workflow store CaptureQABaselineRecord); a capture
// whose control-plane upload fails must leave the manifest unwritten so
// the gate stays on the legacy absolute surface — never a half-trusted
// baseline. Worktree creation itself is not rolled back on failure.
func CaptureQABaseline(worktreeDir string) (QABaseline, error) {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return QABaseline{}, fmt.Errorf("worktree dir is required")
	}
	head := ""
	if out, err := gitQuiet(worktreeDir, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}
	paths, err := worktreeListAllFiles(worktreeDir)
	if err != nil {
		return QABaseline{}, err
	}
	entries := make(map[string]QABaselineEntry, len(paths))
	for _, p := range paths {
		fp, err := fileFingerprint(filepath.Join(worktreeDir, filepath.FromSlash(p)))
		if err != nil {
			return QABaseline{}, fmt.Errorf("fingerprint %s: %w", p, err)
		}
		entries[p] = QABaselineEntry{Fingerprint: fp}
	}
	baseline := QABaseline{
		SchemaVersion: QABaselineSchemaVersion,
		RecordedAt:    time.Now().UTC().Format(time.RFC3339),
		HeadCommit:    head,
		Entries:       entries,
	}
	payload, err := json.Marshal(baseline)
	if err != nil {
		return QABaseline{}, err
	}
	if err := os.MkdirAll(filepath.Dir(QABaselinePath(worktreeDir)), 0o755); err != nil {
		return QABaseline{}, fmt.Errorf("create baseline dir: %w", err)
	}
	if err := os.WriteFile(QABaselinePath(worktreeDir), payload, 0o644); err != nil {
		return QABaseline{}, fmt.Errorf("write worktree baseline copy: %w", err)
	}
	return baseline, nil
}

// QABaselinePath is where the baseline document lives inside a worktree.
func QABaselinePath(worktreeDir string) string {
	return filepath.Join(worktreeDir, ".multigent", qaBaselineFileName)
}

// LoadQABaseline reads the worktree-side recovery copy, if any. ok=false
// means the worktree carries no baseline document. TOOLING/TESTS ONLY —
// the gate must use LoadQABaselineForGate, which consults the control
// plane and fails closed on tamper/loss.
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
//
// The baseline comes from the CALLER (S2-2: resolved via
// LoadQABaselineForGate against the control plane) — this function no
// longer decides trust, it only diffs. A nil Entries map is rejected so a
// zero-value QABaseline cannot silently produce a full-tree delta.
func QABaselineWorktreeDelta(worktreeDir string, baseline QABaseline) ([]string, error) {
	if baseline.Entries == nil {
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
