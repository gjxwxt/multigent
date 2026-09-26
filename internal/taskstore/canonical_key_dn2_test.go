package taskstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func newCanonicalKeyStore(t *testing.T) (*DBStore, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	st := NewDB(root, db).(*DBStore)
	return st, root
}

func seedCanonicalWorker(t *testing.T, st *DBStore, project, dirName, workerID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := st.db.UpsertAgentWorker(controldb.AgentWorker{
		ID: workerID, WorkspaceID: st.workspaceID, Name: dirName, DisplayName: dirName,
		Model: "codex", Status: "available", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.UpsertProjectMembership(controldb.ProjectMembership{
		ID: "pm-" + workerID, WorkspaceID: st.workspaceID, ProjectID: project,
		MemberType: "agent_worker", MemberID: workerID, Title: dirName, Role: "developer",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func countCopiesAcrossAllSpellings(t *testing.T, st *DBStore, project, taskID string) int {
	t.Helper()
	recs, err := st.ListAllTaskRecords(project)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.Task != nil && r.Task.ID == taskID {
			n++
		}
	}
	return n
}

// rawCopyKeys lists the exact byte keys holding the task, bypassing alias
// resolution — the residue detector for case-variant spellings.
func rawCopyKeys(t *testing.T, st *DBStore, project, taskID string) []string {
	t.Helper()
	recs, err := st.db.ListRecords("tasks", st.workspaceID, []string{project})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, rec := range recs {
		if len(rec.Key) > 2 && rec.Key[2] == taskID {
			out = append(out, rec.Key[1])
		}
	}
	return out
}

// The write path must land on ONE canonical byte key per worker: alias
// spellings ("Agent", "agent", worker ID) resolve to the same worker for
// reads, so writes filed under the caller's spelling would leave residue
// copies that diverge depending on which copy a read hits first.
func TestTaskWriteReadDeleteSingleCanonicalCopy(t *testing.T) {
	st, _ := newCanonicalKeyStore(t)
	// Worker registered as "agent" (ID aw-x); callers use case variants.
	seedCanonicalWorker(t, st, "proj", "agent", "aw-x")

	if err := st.AddTask("proj", "Agent", &entity.Task{
		ID: "t-alias", Title: "v1", Status: entity.TaskStatusPending,
	}); err != nil {
		t.Fatalf("add via alias spelling: %v", err)
	}
	if n := countCopiesAcrossAllSpellings(t, st, "proj", "t-alias"); n != 1 {
		t.Fatalf("after AddTask via alias: %d copies, want exactly 1", n)
	}

	// PersistTask (update path) through a different spelling must update the
	// SAME single copy, not file a second one.
	if err := st.PersistTask("proj", "AGENT", &entity.Task{
		ID: "t-alias", Title: "v2", Status: entity.TaskStatusPending,
	}); err != nil {
		t.Fatalf("persist via alias spelling: %v", err)
	}
	if n := countCopiesAcrossAllSpellings(t, st, "proj", "t-alias"); n != 1 {
		t.Fatalf("after PersistTask via alias: %d copies, want exactly 1", n)
	}
	got, err := st.GetTask("proj", "agent", "t-alias")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Title != "v2" {
		t.Fatalf("canonical copy not updated: title=%q", got.Title)
	}

	// Delete via any spelling removes the one copy.
	if err := st.DeleteTask("proj", "aGeNt", "t-alias"); err != nil {
		t.Fatalf("delete via alias spelling: %v", err)
	}
	if n := countCopiesAcrossAllSpellings(t, st, "proj", "t-alias"); n != 0 {
		t.Fatalf("after DeleteTask via alias: %d copies remain, want 0", n)
	}
}

// Workers addressed by their worker ID (the scheduler's spelling) must land
// on the same canonical key as the directory-name spelling.
func TestTaskCanonicalKeyUnifiesWorkerIDAndNameSpellings(t *testing.T) {
	st, _ := newCanonicalKeyStore(t)
	seedCanonicalWorker(t, st, "proj", "review-team", "aw-review")

	if err := st.AddTask("proj", "aw-review", &entity.Task{
		ID: "t-two", Title: "by-id", Status: entity.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTask("proj", "review-team", &entity.Task{
		ID: "t-two", Title: "by-name", Status: entity.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	// Second AddTask over the same task ID through another alias must have
	// overwritten the single canonical copy, not created a sibling.
	if n := countCopiesAcrossAllSpellings(t, st, "proj", "t-two"); n != 1 {
		t.Fatalf("two spellings, %d copies, want 1", n)
	}
	// The canonical byte key is the directory-visible spelling (membership
	// title), not the worker ID — every consumer (FS dirs, runner logs,
	// scheduler targets, store-layer AgentMeta) resolves the title spelling,
	// none resolves the raw worker ID.
	keys := rawCopyKeys(t, st, "proj", "t-two")
	if len(keys) != 1 || keys[0] != "review-team" {
		t.Fatalf("canonical byte key = %v, want [review-team]", keys)
	}
	got, err := st.GetTask("proj", "review-team", "t-two")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "by-name" {
		t.Fatalf("canonical copy holds stale payload: %q", got.Title)
	}
}

// A move to a destination with no existing copy must not file the fresh
// copy under the caller's raw spelling: the destination key is the
// canonical spelling, or the copy is invisible to the destination queue's
// own reads and alias-wide deletes (reviewer blind-review P1).
func TestMoveTaskUnseenDestinationLandsOnCanonicalKey(t *testing.T) {
	st, _ := newCanonicalKeyStore(t)
	seedCanonicalWorker(t, st, "proj", "S2 Dev A", "aw-dev-a")
	seedCanonicalWorker(t, st, "proj", "review-team", "aw-review")

	if err := st.AddTask("proj", "review-team", &entity.Task{
		ID: "t-mv", Title: "mover", Status: entity.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	// Destination "S2 DEV A" is a case-variant spelling of the same worker,
	// with no existing copy in its alias set (disjoint-move norm).
	if err := st.MoveTask("proj", "review-team", "S2 DEV A", &entity.Task{
		ID: "t-mv", Title: "moved", Status: entity.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	keys := rawCopyKeys(t, st, "proj", "t-mv")
	if len(keys) != 1 || keys[0] != "S2 Dev A" {
		t.Fatalf("copy keys after move = %v, want [S2 Dev A] (canonical), not a raw-spelling residue", keys)
	}
	if _, err := st.GetTask("proj", "S2 Dev A", "t-mv"); err != nil {
		t.Fatalf("copy unreadable via canonical spelling: %v", err)
	}
}

// The alias scan must NOT dedup case-folded: a residue copy filed under a
// case-variant byte key ("Agent" vs "agent") must remain visible to reads
// and reachable by alias-wide deletes, or it lingers forever. Regression
// guard for the EqualFold dedup that hid exactly this shape.
func TestTaskAgentKeysIncludeCaseVariantByteKeys(t *testing.T) {
	st, _ := newCanonicalKeyStore(t)
	seedCanonicalWorker(t, st, "proj", "agent", "aw-cv")

	// Request the agent through the case-variant spelling: the alias set must
	// still include the canonical byte key "agent" AND the requested spelling
	// "Agent" — they are distinct byte keys that resolve to the same worker,
	// so both must be scanned by reads and wiped by alias-wide deletes.
	keys, err := st.taskAgentKeys("proj", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	for _, k := range []string{"agent", "Agent", "aw-cv"} {
		if !got[k] {
			t.Fatalf("taskAgentKeys = %v, missing %q (case-variant byte keys must not be deduped away)", keys, k)
		}
	}
	// A pre-existing residue copy filed under the case-variant byte key is
	// NOT visible through the canonical spelling's alias set — byte keys are
	// scanned literally. What matters is the visible-and-wipeable contract on
	// the spellings that DO reach the variant key: a read through "Agent"
	// finds the copy, and the alias-wide delete through the same spelling (or
	// a MoveTask source removal) removes every byte key in its alias set.
	if err := st.db.UpsertRecord("tasks", st.workspaceID, []string{"proj", "Agent", "t-res"}, `{"id":"t-res","title":"residue","status":"pending"}`); err != nil {
		t.Fatal(err)
	}
	residue, err := st.GetTask("proj", "Agent", "t-res")
	if err != nil {
		t.Fatalf("residue copy under case-variant key not readable via that spelling: %v", err)
	}
	if residue.Title != "residue" {
		t.Fatalf("unexpected residue payload: %q", residue.Title)
	}
	if err := st.DeleteTask("proj", "Agent", "t-res"); err != nil {
		t.Fatal(err)
	}
	if keys := rawCopyKeys(t, st, "proj", "t-res"); len(keys) != 0 {
		t.Fatalf("alias-wide delete left %v behind", keys)
	}
}
