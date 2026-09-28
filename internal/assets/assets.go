// Package assets implements the content-addressed attachment model: blob
// bytes on disk, integrity verification, per-run staging for sandboxes, and
// the budget-capped prompt manifest. Row persistence lives in internal/db;
// this package owns the filesystem side and the fail-closed staging contract:
// a task that declares assets but cannot stage them must not start a run
// whose manifest points at nothing.
package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	controldb "github.com/multigent/multigent/internal/db"
)

// ManifestMountTarget is the read-only in-container location the staged cache
// is mounted at. The prompt manifest references this path; the transport that
// fills it (local copy on the control-plane host, authenticated node download
// later) may change without touching this contract.
const ManifestMountTarget = "/mnt/multigent/assets"

// MaxTaskAttachments caps the prompt manifest. Binding more is rejected with
// an explicit error so a human decides — never silently truncated.
const MaxTaskAttachments = 8

// BlobStore manages content-addressed bytes under <control-data>/.multigent/assets.
// Identity is the SHA-256 of the content: the storage path is derived from it,
// so identical content is stored once per deployment no matter who uploads it.
type BlobStore struct {
	root string
}

// BlobStoreRoot resolves the blob root from the control data directory (the
// directory that contains multigent.db).
func BlobStoreRoot() (string, error) {
	dbPath, err := controldb.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dbPath), "assets"), nil
}

func NewBlobStore() (*BlobStore, error) {
	root, err := BlobStoreRoot()
	if err != nil {
		return nil, err
	}
	return &BlobStore{root: root}, nil
}

func NewBlobStoreAt(root string) *BlobStore { return &BlobStore{root: root} }

func (s *BlobStore) Root() string { return s.root }

// Path is where the blob with the given SHA lives, relative layout
// <root>/<sha[0:2]>/<sha>. Callers never hand this path to agents — agents
// only ever see a staged copy inside their own read-only mount.
func (s *BlobStore) Path(sha256 string) string {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if len(sha256) != 64 {
		return ""
	}
	return filepath.Join(s.root, sha256[:2], sha256)
}

// Put streams r into the store atomically: write to a temp file while hashing,
// fsync, then rename into its content-addressed location. Returns the SHA and
// byte size. Re-uploading identical content is a cheap no-op (the rename
// overwrites the same bytes).
func (s *BlobStore) Put(r io.Reader) (string, int64, error) {
	if err := os.MkdirAll(filepath.Join(s.root, "tmp"), 0o755); err != nil {
		return "", 0, fmt.Errorf("assets: create tmp dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("assets: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("assets: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("assets: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("assets: close temp file: %w", err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	dst := s.Path(sha)
	if dst == "" {
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("assets: invalid hash")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("assets: create blob dir: %w", err)
	}
	// Atomic publish: readers either see the full old blob or the full new
	// one; a crash mid-write leaves at most an orphan temp file behind.
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, fmt.Errorf("assets: publish blob: %w", err)
	}
	return sha, size, nil
}

// Verify re-hashes the stored blob and compares it to its address. This is
// the corruption check every read path runs before handing bytes onward.
func (s *BlobStore) Verify(shaHex string) error {
	p := s.Path(shaHex)
	if p == "" {
		return fmt.Errorf("assets: invalid sha256")
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("assets: blob %s… missing", shaHex[:12])
		}
		return fmt.Errorf("assets: open blob: %w", err)
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("assets: hash blob: %w", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != strings.ToLower(shaHex) {
		return fmt.Errorf("assets: blob %s… corrupt (hash mismatch)", shaHex[:12])
	}
	return nil
}

// StagedAsset is one task attachment materialized into the per-run cache dir.
type StagedAsset struct {
	AttachmentID string `json:"attachmentId"`
	Role         string `json:"role"`
	Required     bool   `json:"required"`
	DisplayName  string `json:"displayName"`
	Sha256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Mime         string `json:"mime"`
	// RelPath is the staged file's path under the cache dir
	// (<sha[0:2]>/<displayName>); callers join it with the cache root (host
	// view) or the mount target (container view).
	RelPath string `json:"relPath"`
}

// RelPathFor is the staged layout for one asset: <sha[0:2]>/<displayName>.
// Shared by the stager and the manifest renderer so both can never drift.
func RelPathFor(shaHex, displayName string) string {
	return filepath.Join(shaHex[:2], SanitizeDisplayName(displayName))
}

// ManifestFromAttachments renders the prompt manifest directly from the
// attachment join rows (control-plane prompt-build path, before any staging).
func ManifestFromAttachments(atts []controldb.AssetAttachmentWithFile) string {
	if len(atts) == 0 {
		return ""
	}
	staged := make([]StagedAsset, 0, len(atts))
	for _, att := range atts {
		staged = append(staged, StagedAsset{
			AttachmentID: att.ID,
			Role:         att.Role,
			Required:     att.Required,
			DisplayName:  att.DisplayName,
			Sha256:       att.Sha256,
			Size:         att.BlobSize,
			Mime:         att.BlobMime,
			RelPath:      RelPathFor(att.Sha256, att.DisplayName),
		})
	}
	return ManifestSection(staged, "")
}

// SanitizeDisplayName keeps the user-facing filename but strips anything that
// could escape the staged directory or confuse the manifest line.
func SanitizeDisplayName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "asset.bin"
	}
	return name
}

// StageTaskAssets materializes the task's bound blobs into cacheDir and
// verifies every byte against its pinned SHA. Fail-closed: missing blob,
// hash mismatch, or more than MaxTaskAttachments attachments abort the run
// with an explicit error — never a manifest pointing at nothing.
func StageTaskAssets(db controldb.Store, bs *BlobStore, taskID, cacheDir string) ([]StagedAsset, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, nil
	}
	atts, err := db.ListAssetAttachmentsForTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("assets: list attachments for task %s: %w", taskID, err)
	}
	if len(atts) == 0 {
		return nil, nil
	}
	if len(atts) > MaxTaskAttachments {
		return nil, fmt.Errorf("assets: task %s has %d bound assets, over the %d-attachment manifest budget; unbind some and re-run", taskID, len(atts), MaxTaskAttachments)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("assets: create cache dir: %w", err)
	}
	staged := make([]StagedAsset, 0, len(atts))
	for _, att := range atts {
		src := bs.Path(att.Sha256)
		if src == "" {
			return nil, fmt.Errorf("assets: task %s attachment %s has invalid sha256", taskID, att.ID)
		}
		if err := bs.Verify(att.Sha256); err != nil {
			return nil, fmt.Errorf("assets: task %s attachment %s (%s): %w", taskID, att.ID, att.DisplayName, err)
		}
		name := SanitizeDisplayName(att.DisplayName)
		rel := RelPathFor(att.Sha256, att.DisplayName)
		dst := filepath.Join(cacheDir, rel)
		if _, err := os.Stat(dst); err == nil {
			// Same bucket + same display name from two different blobs: keep
			// both distinguishable by folding the sha prefix into the name.
			ext := filepath.Ext(name)
			base := strings.TrimSuffix(name, ext)
			name = fmt.Sprintf("%s-%s%s", base, att.Sha256[:8], ext)
			rel = filepath.Join(att.Sha256[:2], name)
			dst = filepath.Join(cacheDir, rel)
		}
		if err := copyVerified(src, dst, att.Sha256); err != nil {
			return nil, fmt.Errorf("assets: task %s attachment %s (%s): %w", taskID, att.ID, att.DisplayName, err)
		}
		staged = append(staged, StagedAsset{
			AttachmentID: att.ID,
			Role:         att.Role,
			Required:     att.Required,
			DisplayName:  att.DisplayName,
			Sha256:       att.Sha256,
			Size:         att.BlobSize,
			Mime:         att.BlobMime,
			RelPath:      rel,
		})
	}
	return staged, nil
}

// copyVerified copies src to dst while hashing the bytes actually written,
// then compares against expected — the staged copy is what the agent reads,
// so it is the bytes that get verified, not just the store copy.
func copyVerified(src, dst, expectedSha string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source blob: %w", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create staged dir: %w", err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hasher), in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy blob: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close staged file: %w", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != strings.ToLower(expectedSha) {
		_ = os.Remove(dst)
		return fmt.Errorf("staged copy hash mismatch (expected %s…, got %s…)", expectedSha[:12], got[:12])
	}
	return nil
}

// ManifestSection renders the budget-capped prompt block. One line per asset,
// full SHA so the agent can verify what it cites; content itself never lands
// in the prompt.
func ManifestSection(staged []StagedAsset, mountTarget string) string {
	if len(staged) == 0 {
		return ""
	}
	if mountTarget == "" {
		mountTarget = ManifestMountTarget
	}
	var b strings.Builder
	b.WriteString("## Task assets (read on demand, do NOT inline full content)\n\n")
	for _, a := range staged {
		tag := "reference"
		if a.Required {
			tag = "required"
		}
		fmt.Fprintf(&b, "- [%s] %s -> %s (sha256:%s, %s, %s)\n", tag, a.DisplayName, filepath.Join(mountTarget, filepath.FromSlash(a.RelPath)), a.Sha256, humanSize(a.Size), mimeKind(a.Mime))
	}
	b.WriteString("\nRead section by section (grep/head by heading); quote file+SHA when citing; never invent content beyond these files.\n")
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dKB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func mimeKind(mime string) string {
	mime = strings.TrimSpace(mime)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if mime == "" {
		return "file"
	}
	return mime
}
