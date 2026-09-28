package db

import (
	"path/filepath"
	"testing"
)

func newAssetTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

const (
	assetTestShaA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	assetTestShaB = "b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1"
)

func seedAssetBlobAndFile(t *testing.T, store *SQLiteStore, wsID, project, sha string) AssetFile {
	t.Helper()
	if err := store.UpsertAssetBlob(AssetBlob{Sha256: sha, Size: 123, Mime: "text/markdown", StoragePath: "ab/" + sha, CreatedBy: "tester"}); err != nil {
		t.Fatalf("UpsertAssetBlob: %v", err)
	}
	f := &AssetFile{WorkspaceID: wsID, ProjectID: project, DisplayName: "需求说明.md", CurrentSha: sha, CreatedBy: "tester"}
	if err := store.InsertAssetFile(f); err != nil {
		t.Fatalf("InsertAssetFile: %v", err)
	}
	return *f
}

func TestAssetBlobDedupAndLookup(t *testing.T) {
	store := newAssetTestStore(t)
	if err := store.UpsertAssetBlob(AssetBlob{Sha256: assetTestShaA, Size: 5, StoragePath: "a1/" + assetTestShaA}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Second upsert of the same content: no error, single row (dedup).
	if err := store.UpsertAssetBlob(AssetBlob{Sha256: assetTestShaA, Size: 5, StoragePath: "a1/" + assetTestShaA, CreatedBy: "someone-else"}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, ok, err := store.AssetBlob(assetTestShaA)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Size != 5 || got.CreatedBy != "" {
		t.Fatalf("first upload must win: %+v", got)
	}
	if _, ok, _ := store.AssetBlob(assetTestShaB); ok {
		t.Fatalf("unknown blob must not be found")
	}
	if err := store.UpsertAssetBlob(AssetBlob{Sha256: "short", Size: 1, StoragePath: "x"}); err == nil {
		t.Fatalf("invalid sha must be rejected")
	}
}

func TestAssetAttachmentLifecycle(t *testing.T) {
	store := newAssetTestStore(t)
	f := seedAssetBlobAndFile(t, store, "ws-1", "proj-1", assetTestShaA)

	a := &AssetAttachment{FileID: f.ID, Sha256: f.CurrentSha, ProjectID: "proj-1", TaskID: "t-1", Role: AssetRoleRequirementInput, Required: true, AddedBy: "tester"}
	if err := store.InsertAssetAttachment(a); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	if a.ID == "" {
		t.Fatalf("attachment id must be assigned")
	}

	atts, err := store.ListAssetAttachmentsForTask("t-1")
	if err != nil || len(atts) != 1 {
		t.Fatalf("list for task: n=%d err=%v", len(atts), err)
	}
	if atts[0].DisplayName != "需求说明.md" || atts[0].BlobSize != 123 || atts[0].FileWorkspaceID != "ws-1" {
		t.Fatalf("join fields missing: %+v", atts[0])
	}

	usage, err := store.AssetFileUsage("proj-1")
	if err != nil || usage[f.ID] != 1 {
		t.Fatalf("usage: %+v err=%v", usage, err)
	}

	files, err := store.ListAssetFilesForProject("ws-1", "proj-1", false)
	if err != nil || len(files) != 1 {
		t.Fatalf("list files: n=%d err=%v", len(files), err)
	}

	// Unbind leaves file and blob untouched.
	if err := store.DeleteAssetAttachment(a.ID); err != nil {
		t.Fatalf("delete attachment: %v", err)
	}
	if _, ok, _ := store.AssetFile(f.ID); !ok {
		t.Fatalf("file must survive attachment delete")
	}
	fileRefs, attRefs, err := store.CountAssetBlobReferences(assetTestShaA)
	if err != nil || fileRefs != 1 || attRefs != 0 {
		t.Fatalf("blob refs after unbind: files=%d atts=%d err=%v", fileRefs, attRefs, err)
	}
}

func TestAssetAttachmentValidation(t *testing.T) {
	store := newAssetTestStore(t)
	f := seedAssetBlobAndFile(t, store, "ws-1", "proj-1", assetTestShaA)

	cases := []struct {
		name string
		att  AssetAttachment
	}{
		{"project-level row forbidden", AssetAttachment{FileID: f.ID, Sha256: f.CurrentSha, ProjectID: "proj-1", TaskID: "", Role: AssetRoleReference}},
		{"unknown role forbidden", AssetAttachment{FileID: f.ID, Sha256: f.CurrentSha, ProjectID: "proj-1", TaskID: "t-1", Role: "owner"}},
		{"missing sha forbidden", AssetAttachment{FileID: f.ID, ProjectID: "proj-1", TaskID: "t-1", Role: AssetRoleReference}},
	}
	for _, tc := range cases {
		att := tc.att
		if err := store.InsertAssetAttachment(&att); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}

	// Foreign-key closure: attachment must reference an existing blob row.
	bad := AssetAttachment{FileID: f.ID, Sha256: assetTestShaB, ProjectID: "proj-1", TaskID: "t-1", Role: AssetRoleReference}
	if err := store.InsertAssetAttachment(&bad); err == nil {
		t.Fatalf("attachment pinning a non-existent blob must fail (FK)")
	}
}

func TestAssetFilePointerMoveAndArchive(t *testing.T) {
	store := newAssetTestStore(t)
	f := seedAssetBlobAndFile(t, store, "ws-1", "proj-1", assetTestShaA)
	if err := store.UpsertAssetBlob(AssetBlob{Sha256: assetTestShaB, Size: 9, StoragePath: "b2/" + assetTestShaB}); err != nil {
		t.Fatalf("seed blob b: %v", err)
	}
	// Pin the old version on a task, then edit the file (pointer move).
	a := &AssetAttachment{FileID: f.ID, Sha256: assetTestShaA, ProjectID: "proj-1", TaskID: "t-old", Role: AssetRoleReference}
	if err := store.InsertAssetAttachment(a); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := store.MoveAssetFilePointer(f.ID, assetTestShaB); err != nil {
		t.Fatalf("pointer move: %v", err)
	}
	// The pinned attachment still references the OLD blob — versioning for free.
	atts, err := store.ListAssetAttachmentsForTask("t-old")
	if err != nil || len(atts) != 1 || atts[0].Sha256 != assetTestShaA {
		t.Fatalf("pinned version must survive pointer move: %+v err=%v", atts, err)
	}
	if err := store.ArchiveAssetFile(f.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if files, _ := store.ListAssetFilesForProject("ws-1", "proj-1", false); len(files) != 0 {
		t.Fatalf("archived file must be hidden by default")
	}
	if files, _ := store.ListAssetFilesForProject("ws-1", "proj-1", true); len(files) != 1 {
		t.Fatalf("archived file must be visible with includeArchived")
	}
}
