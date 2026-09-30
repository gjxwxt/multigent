package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	taskstore "github.com/multigent/multigent/internal/taskstore"
)

func testAssetSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// seedAttentionAssetsFixture builds the control-DB state an attention wakeup
// needs: a worker membership, a target task carrying asset bindings, and a
// task-sourced attention signal pointing at it.
func seedAttentionAssetsFixture(t *testing.T) (root string, db *controldb.SQLiteStore) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", "")
	t.Setenv("MULTIGENT_DATA_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Test\nlang: zh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	controlDB, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	now := time.Now().UTC().Format(time.RFC3339)
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := controlDB.UpsertAgentWorker(controldb.AgentWorker{ID: "aw-dev", WorkspaceID: "ws", Name: "dev", DisplayName: "dev", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := controlDB.UpsertProjectMembership(controldb.ProjectMembership{
		ID:          "pm-1",
		WorkspaceID: "ws",
		ProjectID:   "sample",
		MemberType:  "agent_worker",
		MemberID:    "aw-dev",
		Role:        "dev",
		Title:       "dev",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := controlDB.UpsertAttentionSignal(controldb.AttentionSignal{
		ID:            "sig-task",
		WorkspaceID:   "ws",
		AgentWorkerID: "aw-dev",
		DedupeKey:     "task:sample:t-req-1:dev:task_assigned",
		SourceKind:    "task",
		SourceID:      "t-req-1",
		SourceChannel: "project:sample",
		Reason:        "task_assigned",
		Priority:      "normal",
		ActorType:     "user",
		ActorID:       "owner-a",
		RefsJSON:      `{"project":"sample","agent":"dev","taskId":"t-req-1"}`,
		Summary:       "请处理需求任务",
		Status:        "pending",
		CreatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	return root, controlDB
}

// bindTestAsset inserts a project-library file and binds it to taskID with the
// given role, mirroring the API's task-create binding flow.
func bindTestAsset(t *testing.T, db *controldb.SQLiteStore, taskID, role string, required bool) string {
	t.Helper()
	content := "requirement doc for " + taskID + " " + role
	sha := testAssetSHA(content)
	if err := db.UpsertAssetBlob(controldb.AssetBlob{
		Sha256:      sha,
		Size:        int64(len(content)),
		Mime:        "text/markdown",
		StoragePath: sha[:2] + "/" + sha,
	}); err != nil {
		t.Fatal(err)
	}
	f := &controldb.AssetFile{WorkspaceID: "ws", ProjectID: "sample", DisplayName: "需求说明-" + role + ".md", CurrentSha: sha}
	if err := db.InsertAssetFile(f); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertAssetAttachment(&controldb.AssetAttachment{
		FileID:    f.ID,
		Sha256:    sha,
		ProjectID: "sample",
		TaskID:    taskID,
		Role:      role,
		Required:  required,
		AddedBy:   "test",
	}); err != nil {
		t.Fatal(err)
	}
	return sha
}

// Regression (t-20260929-lwi06q): an attention wakeup runs a synthetic task
// with its own fresh ID; asset staging resolves attachments per task ID, so
// without re-binding the target's assets the container had no
// /mnt/multigent/assets mount and the agent invented progress. The wakeup task
// must carry the target's input assets.
func TestAttentionWakeupInheritsTargetTaskAssets(t *testing.T) {
	root, db := seedAttentionAssetsFixture(t)
	reqSHA := bindTestAsset(t, db, "t-req-1", controldb.AssetRoleRequirementInput, true)
	refSHA := bindTestAsset(t, db, "t-req-1", controldb.AssetRoleReference, false)
	bindTestAsset(t, db, "t-req-1", controldb.AssetRoleDeliverable, false)

	// The target task must exist in the same store the deployment uses.
	ts := taskstore.NewDB(root, db)
	if err := ts.AddTask("sample", "dev", &entity.Task{
		ID:        "t-req-1",
		Title:     "需求任务",
		Status:    entity.TaskStatusPending,
		Type:      "feature",
		CreatedBy: "owner-a",
		Prompt:    "按需求文档实现",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	section, ids, _, _, targetTaskID, targetProj, err := pendingAttentionSection(root, "sample", "dev", wakeupStrings("zh"))
	if err != nil {
		t.Fatal(err)
	}
	if targetTaskID != "t-req-1" || targetProj != "sample" {
		t.Fatalf("target not resolved: task=%q proj=%q section=%s", targetTaskID, targetProj, section)
	}
	if len(ids) != 1 || ids[0] != "sig-task" {
		t.Fatalf("unexpected ids: %#v", ids)
	}

	// The wakeup task is persisted before the asset copy runs (the copy needs
	// the wakeup's own ID), then RunTask stages by that ID. Verify the helper
	// against the same store the run path opens (controldb.OpenDefault via
	// MULTIGENT_DATA_DIR).
	wakeupID := "t-wakeup-1"
	schedulerAttentionTargetProjectDir(root, targetProj, targetTaskID, wakeupID)

	wakeupAtts, err := db.ListAssetAttachmentsForTask(wakeupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(wakeupAtts) != 2 {
		t.Fatalf("wakeup task must carry the target's 2 input assets, got %d: %+v", len(wakeupAtts), wakeupAtts)
	}
	got := map[string]controldb.AssetAttachmentWithFile{}
	for _, att := range wakeupAtts {
		got[att.Role] = att
	}
	reqAtt, ok := got[controldb.AssetRoleRequirementInput]
	if !ok || reqAtt.Sha256 != reqSHA || !reqAtt.Required {
		t.Fatalf("requirement_input binding missing or wrong: %+v", reqAtt)
	}
	refAtt := got[controldb.AssetRoleReference]
	if refAtt.Sha256 != refSHA || refAtt.Required {
		t.Fatalf("reference binding wrong: %+v", refAtt)
	}
	for _, att := range wakeupAtts {
		if att.Role == controldb.AssetRoleDeliverable {
			t.Fatalf("deliverable assets must not be re-bound onto the wakeup: %+v", att)
		}
		if att.TaskID != wakeupID {
			t.Fatalf("attachment must point at the wakeup task id: %+v", att)
		}
		if !strings.HasPrefix(att.ProjectID, "sample") {
			t.Fatalf("attachment project mismatch: %+v", att)
		}
	}
}

// A second wakeup for the same attention cycle must not accumulate duplicate
// bindings (the wakeup task is deduped by the scheduler across retries).
func TestAttentionWakeupAssetCopyIsIdempotentPerFile(t *testing.T) {
	root, db := seedAttentionAssetsFixture(t)
	bindTestAsset(t, db, "t-req-1", controldb.AssetRoleRequirementInput, true)
	ts := taskstore.NewDB(root, db)
	if err := ts.AddTask("sample", "dev", &entity.Task{
		ID:        "t-req-1",
		Title:     "需求任务",
		Status:    entity.TaskStatusPending,
		Type:      "feature",
		CreatedBy: "owner-a",
		Prompt:    "按需求文档实现",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	const wakeupID = "t-wakeup-idem"
	schedulerAttentionTargetProjectDir(root, "sample", "t-req-1", wakeupID)
	schedulerAttentionTargetProjectDir(root, "sample", "t-req-1", wakeupID)

	atts, err := db.ListAssetAttachmentsForTask(wakeupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 {
		t.Fatalf("re-copy must not duplicate bindings, got %d: %+v", len(atts), atts)
	}
}

// A target without asset bindings must not create empty attachment rows.
func TestAttentionWakeupWithoutTargetAssetsIsNoop(t *testing.T) {
	root, db := seedAttentionAssetsFixture(t)
	ts := taskstore.NewDB(root, db)
	if err := ts.AddTask("sample", "dev", &entity.Task{
		ID:        "t-req-1",
		Title:     "无资产任务",
		Status:    entity.TaskStatusPending,
		Type:      "chore",
		CreatedBy: "owner-a",
		Prompt:    "无资产",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	schedulerAttentionTargetProjectDir(root, "sample", "t-req-1", "t-wakeup-none")
	atts, err := db.ListAssetAttachmentsForTask("t-wakeup-none")
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 0 {
		t.Fatalf("no-op copy created bindings: %+v", atts)
	}
}

// Asset staging (the runner's fail-closed stager) must find the copied
// bindings when it is handed the wakeup task — the acceptance contract for
// this fix.
func TestStagedAssetsResolveForWakeupTask(t *testing.T) {
	root, db := seedAttentionAssetsFixture(t)
	bindTestAsset(t, db, "t-req-1", controldb.AssetRoleRequirementInput, true)
	ts := taskstore.NewDB(root, db)
	if err := ts.AddTask("sample", "dev", &entity.Task{
		ID:        "t-req-1",
		Title:     "需求任务",
		Status:    entity.TaskStatusPending,
		Type:      "feature",
		CreatedBy: "owner-a",
		Prompt:    "按需求文档实现",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	const wakeupID = "t-wakeup-stage"
	schedulerAttentionTargetProjectDir(root, "sample", "t-req-1", wakeupID)

	// The runner stages via controldb.OpenDefault(), which resolves to the
	// same control DB because MULTIGENT_DATA_DIR points at root.
	controlDB, err := controldb.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	defer controlDB.Close()
	atts, err := controlDB.ListAssetAttachmentsForTask(wakeupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 {
		t.Fatalf("staging lookup must see the copied binding, got %d", len(atts))
	}
}
