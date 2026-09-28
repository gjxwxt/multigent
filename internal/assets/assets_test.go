package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func putTestBlob(t *testing.T, bs *BlobStore, content string) string {
	t.Helper()
	sha, size, err := bs.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("Put size = %d, want %d", size, len(content))
	}
	return sha
}

func seedStagingFixture(t *testing.T) (*controldb.SQLiteStore, *BlobStore, string) {
	t.Helper()
	store, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open control db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bs := NewBlobStoreAt(filepath.Join(t.TempDir(), "assets"))
	sha := putTestBlob(t, bs, "# 需求\n\n## 1. 范围\n正文")
	if err := store.UpsertAssetBlob(controldb.AssetBlob{Sha256: sha, Size: int64(len("# 需求\n\n## 1. 范围\n正文")), Mime: "text/markdown", StoragePath: sha[:2] + "/" + sha}); err != nil {
		t.Fatalf("upsert blob row: %v", err)
	}
	f := &controldb.AssetFile{WorkspaceID: "ws-1", ProjectID: "proj-1", DisplayName: "需求说明.md", CurrentSha: sha}
	if err := store.InsertAssetFile(f); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	return store, bs, sha
}

func TestBlobStorePutDedupAndVerify(t *testing.T) {
	bs := NewBlobStoreAt(filepath.Join(t.TempDir(), "assets"))
	sha1 := putTestBlob(t, bs, "same bytes")
	sha2 := putTestBlob(t, bs, "same bytes")
	if sha1 != sha2 {
		t.Fatalf("identical content must produce identical sha")
	}
	if err := bs.Verify(sha1); err != nil {
		t.Fatalf("verify intact blob: %v", err)
	}
	// Corrupt the stored bytes in place; Verify must catch it — the corruption
	// check is the thing that keeps a silent bad read impossible.
	if err := os.WriteFile(bs.Path(sha1), []byte("tampered!"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := bs.Verify(sha1); err == nil {
		t.Fatalf("verify must fail on corrupted blob")
	}
	if err := bs.Verify(strings.Repeat("a", 64)); err == nil {
		t.Fatalf("verify of absent blob must fail")
	}
}

func TestStageTaskAssetsHappyPathAndManifest(t *testing.T) {
	store, bs, sha := seedStagingFixture(t)
	a := &controldb.AssetAttachment{FileID: "", Sha256: sha, ProjectID: "proj-1", TaskID: "t-1", Role: controldb.AssetRoleRequirementInput, Required: true}
	files, _ := store.ListAssetFilesForProject("ws-1", "proj-1", false)
	a.FileID = files[0].ID
	if err := store.InsertAssetAttachment(a); err != nil {
		t.Fatalf("bind: %v", err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache-run1")
	staged, err := StageTaskAssets(store, bs, "t-1", cacheDir)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(staged) != 1 || staged[0].RelPath != sha[:2]+"/需求说明.md" {
		t.Fatalf("unexpected staging result: %+v", staged)
	}
	// The staged copy exists and hashes to the pinned value.
	data, err := os.ReadFile(filepath.Join(cacheDir, staged[0].RelPath))
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
		t.Fatalf("staged bytes do not match pinned sha")
	}

	manifest := ManifestSection(staged, ManifestMountTarget)
	for _, want := range []string{
		"## Task assets (read on demand, do NOT inline full content)",
		"[required] 需求说明.md",
		filepath.Join(ManifestMountTarget, sha[:2], "需求说明.md"),
		"sha256:" + sha,
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	if strings.Contains(manifest, "# 需求") {
		t.Fatalf("manifest must not inline file content:\n%s", manifest)
	}
}

func TestStageTaskAssetsFailClosed(t *testing.T) {
	store, bs, sha := seedStagingFixture(t)
	files, _ := store.ListAssetFilesForProject("ws-1", "proj-1", false)

	// Case 1: attachment whose blob file is absent from the store.
	missingSha := strings.Repeat("e", 64)
	if err := store.UpsertAssetBlob(controldb.AssetBlob{Sha256: missingSha, Size: 1, StoragePath: "ee/" + missingSha}); err != nil {
		t.Fatalf("seed missing blob row: %v", err)
	}
	a := &controldb.AssetAttachment{FileID: files[0].ID, Sha256: missingSha, ProjectID: "proj-1", TaskID: "t-missing", Role: controldb.AssetRoleReference}
	if err := store.InsertAssetAttachment(a); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := StageTaskAssets(store, bs, "t-missing", filepath.Join(t.TempDir(), "c1")); err == nil {
		t.Fatalf("staging with missing blob must fail closed")
	}

	// Case 2: stored blob corrupted on disk → hash mismatch must abort.
	a2 := &controldb.AssetAttachment{FileID: files[0].ID, Sha256: sha, ProjectID: "proj-1", TaskID: "t-corrupt", Role: controldb.AssetRoleReference}
	if err := store.InsertAssetAttachment(a2); err != nil {
		t.Fatalf("bind 2: %v", err)
	}
	if err := os.WriteFile(bs.Path(sha), []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := StageTaskAssets(store, bs, "t-corrupt", filepath.Join(t.TempDir(), "c2")); err == nil {
		t.Fatalf("staging with corrupted blob must fail closed")
	}
}

func TestStageTaskAssetsBudget(t *testing.T) {
	store, bs, _ := seedStagingFixture(t)
	files, _ := store.ListAssetFilesForProject("ws-1", "proj-1", false)
	shas := make([]string, 0, MaxTaskAttachments+1)
	for i := 0; i <= MaxTaskAttachments; i++ {
		shas = append(shas, putTestBlob(t, bs, strings.Repeat("x", 10)+strings.Repeat(string(rune('a'+i)), 5)))
		if err := store.UpsertAssetBlob(controldb.AssetBlob{Sha256: shas[i], Size: 15, StoragePath: shas[i][:2] + "/" + shas[i]}); err != nil {
			t.Fatalf("upsert blob %d: %v", i, err)
		}
		a := &controldb.AssetAttachment{FileID: files[0].ID, Sha256: shas[i], ProjectID: "proj-1", TaskID: "t-budget", Role: controldb.AssetRoleReference}
		if err := store.InsertAssetAttachment(a); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
	}
	if _, err := StageTaskAssets(store, bs, "t-budget", filepath.Join(t.TempDir(), "c")); err == nil {
		t.Fatalf("over-budget task must fail with explicit error, never truncate")
	}
}

func TestStageTaskAssetsEmptyAndDisplayNameSafety(t *testing.T) {
	store, bs, _ := seedStagingFixture(t)
	if staged, err := StageTaskAssets(store, bs, "t-none", filepath.Join(t.TempDir(), "c")); err != nil || staged != nil {
		t.Fatalf("task without attachments must be a no-op: staged=%v err=%v", staged, err)
	}
	if got := SanitizeDisplayName("../../etc/passwd"); got != "passwd" {
		t.Fatalf("path traversal must be stripped, got %q", got)
	}
	if got := SanitizeDisplayName(""); got != "asset.bin" {
		t.Fatalf("empty name must fall back, got %q", got)
	}
}
