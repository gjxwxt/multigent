package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/assets"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runtimeexec"
	"github.com/multigent/multigent/internal/sandbox"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
)

func newAssetsRunner(t *testing.T) (*Runner, *controldb.SQLiteStore) {
	t.Helper()
	t.Setenv("MULTIGENT_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	root := t.TempDir()
	controlDB, err := controldb.OpenDefault()
	if err != nil {
		t.Fatalf("open control db: %v", err)
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	r := New(root, taskstore.NewDB(root, controlDB), store.NewDB(root, controlDB))
	return r, controlDB
}

func seedTaskAsset(t *testing.T, db *controldb.SQLiteStore, taskID, content string) (sha string) {
	t.Helper()
	bs := assets.NewBlobStoreAt(mustBlobRoot(t))
	sha, _, err := bs.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	if err := db.UpsertAssetBlob(controldb.AssetBlob{Sha256: sha, Size: int64(len(content)), Mime: "text/markdown", StoragePath: sha[:2] + "/" + sha}); err != nil {
		t.Fatalf("upsert blob row: %v", err)
	}
	f := &controldb.AssetFile{WorkspaceID: "ws", ProjectID: "proj", DisplayName: "需求说明.md", CurrentSha: sha}
	if err := db.InsertAssetFile(f); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	a := &controldb.AssetAttachment{FileID: f.ID, Sha256: sha, ProjectID: "proj", TaskID: taskID, Role: controldb.AssetRoleRequirementInput, Required: true}
	if err := db.InsertAssetAttachment(a); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return sha
}

func mustBlobRoot(t *testing.T) string {
	t.Helper()
	root, err := assets.BlobStoreRoot()
	if err != nil {
		t.Fatalf("blob root: %v", err)
	}
	return root
}

func TestStageLocalTaskAssetsAndPromptSection(t *testing.T) {
	r, db := newAssetsRunner(t)
	sha := seedTaskAsset(t, db, "t-assets-1", "# 需求\n\n## 1. 范围\n正文")

	staged, err := r.stageLocalTaskAssets(&entity.Task{ID: "t-assets-1"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if staged == nil || staged.CacheDir == "" {
		t.Fatalf("expected staged assets for task with attachments")
	}
	staged.Cleanup()
	if _, err := os.Stat(staged.CacheDir); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the cache dir")
	}

	// Re-stage and verify the manifest contract.
	staged, err = r.stageLocalTaskAssets(&entity.Task{ID: "t-assets-1"})
	if err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	defer staged.Cleanup()
	data, err := os.ReadFile(filepath.Join(staged.CacheDir, sha[:2], "需求说明.md"))
	if err != nil || string(data) != "# 需求\n\n## 1. 范围\n正文" {
		t.Fatalf("staged bytes mismatch: err=%v data=%q", err, data)
	}

	section := r.taskAssetsPromptSection("t-assets-1")
	for _, want := range []string{
		"## Task assets (read on demand, do NOT inline full content)",
		"[required] 需求说明.md",
		filepath.Join(sandbox.AssetsMount, sha[:2], "需求说明.md"),
		"sha256:" + sha,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("prompt section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "正文") {
		t.Fatalf("prompt section must not inline file content")
	}

	// A task without attachments is a no-op on both layers.
	if got, err := r.stageLocalTaskAssets(&entity.Task{ID: "t-none"}); err != nil || got != nil {
		t.Fatalf("task without assets must be a no-op: staged=%v err=%v", got, err)
	}
	if got := r.taskAssetsPromptSection("t-none"); got != "" {
		t.Fatalf("task without assets must have no manifest, got:\n%s", got)
	}
}

func TestStageLocalTaskAssetsFailClosed(t *testing.T) {
	r, db := newAssetsRunner(t)
	sha := seedTaskAsset(t, db, "t-assets-2", "content")
	// Corrupt the stored blob: the run must fail, not proceed silently.
	os.WriteFile(filepath.Join(mustBlobRoot(t), sha[:2], sha), []byte("corrupted"), 0o644)
	if _, err := r.stageLocalTaskAssets(&entity.Task{ID: "t-assets-2"}); err == nil {
		t.Fatalf("staging with corrupted blob must fail the run")
	}
}

func TestAppendTaskAssetsMount(t *testing.T) {
	dir := t.TempDir()
	base := []entity.RuntimeMount{{Source: "/host/ws", Target: "/workspace", Mode: "rw"}}

	// Docker provider + staged dir → read-only mount at the contract target.
	got := appendTaskAssetsMount(base, map[string]string{runtimeexec.RuntimeTaskAssetsDirEnv: dir}, entity.SandboxDocker)
	if len(got) != 2 || got[1].Target != sandbox.AssetsMount || got[1].Mode != "ro" || got[1].Source != dir {
		t.Fatalf("assets mount missing or wrong: %+v", got)
	}

	// Idempotent: the mount is not duplicated.
	got2 := appendTaskAssetsMount(got, map[string]string{runtimeexec.RuntimeTaskAssetsDirEnv: dir}, entity.SandboxDocker)
	if len(got2) != 2 {
		t.Fatalf("assets mount must not duplicate: %+v", got2)
	}

	// No env var, missing dir, or non-Docker provider → unchanged.
	if got3 := appendTaskAssetsMount(base, nil, entity.SandboxDocker); len(got3) != 1 {
		t.Fatalf("no env var must leave mounts unchanged")
	}
	if got4 := appendTaskAssetsMount(base, map[string]string{runtimeexec.RuntimeTaskAssetsDirEnv: "/nonexistent/dir"}, entity.SandboxDocker); len(got4) != 1 {
		t.Fatalf("missing dir must leave mounts unchanged")
	}
	if got5 := appendTaskAssetsMount(base, map[string]string{runtimeexec.RuntimeTaskAssetsDirEnv: dir}, entity.SandboxNone); len(got5) != 1 {
		t.Fatalf("direct host execution must not get the container mount")
	}
}
