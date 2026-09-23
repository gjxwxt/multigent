package workflow

// Regression suite for fix rounds S2-1 (Fix A + Fix B) and S2-2 (baseline
// trust model): gate rejections must leave zero terminal state behind,
// corrected re-reports succeed, the baseline delta catches committed
// changes / same-path re-modification / deletion, and the baseline itself
// is TRUSTED ONLY from the control plane — the agent-writable worktree
// copy can never win, and a lost baseline fails closed instead of
// degrading to the weaker absolute surface.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/gitworktree"
)

// gitCommand builds a git command with the same env isolation the shared
// worktree fixture uses (the fixture's run helper is file-local there).
func gitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	return cmd
}

// newBaselineStore builds a workflow Store over a real control DB, matching
// the production wiring where baselines live in kv_records.
func newBaselineStore(t *testing.T) *Store {
	t.Helper()
	controlDB, err := db.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { controlDB.Close() })
	if err := controlDB.UpsertWorkspace(db.Workspace{ID: "ws-baseline", Name: "WS", Slug: "ws-baseline", Root: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return NewStore(controlDB, "ws-baseline")
}

// captureBaselineForTask mirrors the production capture flow: fingerprint +
// persist to the control plane + write the recovery copy and manifest.
func captureBaselineForTask(t *testing.T, store *Store, project, taskID, wt string) {
	t.Helper()
	if err := store.CaptureQABaselineRecord(project, taskID, wt); err != nil {
		t.Fatalf("capture qa baseline record: %v", err)
	}
}

// verifyWithBaseline runs the delivery-scope gate exactly as the branch join
// does: resolve the trust surface from the control plane, then cross-check.
func verifyWithBaseline(
	t *testing.T,
	store *Store,
	project, taskID, declared, wt string,
) error {
	t.Helper()
	surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), project, taskID)
	if err != nil {
		return err
	}
	return verifyWorktreeDeltaAgainstDeclaration(declared, wt, surf)
}

func TestQABaselineDeltaIgnoresPlatformScaffolding(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	// Platform materialization noise, present BEFORE baseline capture.
	for _, p := range []string{".cursor/settings.json", ".mcp.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, p)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, p), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	captureBaselineForTask(t, store, "proj", "task-scaffold", wt)
	// The task's only change: a test file.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyWithBaseline(t, store, "proj", "task-scaffold", "server_test.go", wt); err != nil {
		t.Fatalf("scaffold noise must not fail an honest completion: %v", err)
	}
	// The task's change is still mandatory to declare.
	err := verifyWithBaseline(t, store, "proj", "task-scaffold", "none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("undeclared real change must still fail, got: %v", err)
	}
}

// TestQABaselineDeltaCatchesCommittedChange: committed changes empty the
// git status, so a path-set-only comparison would miss them. The
// fingerprinted baseline still reports the file as changed (baseline
// fingerprint != committed content fingerprint).
func TestQABaselineDeltaCatchesCommittedChange(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-committed", wt)
	run := func(args ...string) {
		cmd := gitCommand(t, wt, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	// The task commits its work: status is now clean.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestCommitted() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "task work")
	if out, err := gitCommand(t, wt, "status", "--porcelain").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("test setup: worktree should be clean after commit, got %q (%v)", out, err)
	}
	// Declaring "none" must STILL fail: the committed delta is a delivery.
	err := verifyWithBaseline(t, store, "proj", "task-committed", "none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("committed change must not escape the gate, got: %v", err)
	}
	// Declaring it honestly passes the cross-check (whitelist then rules).
	if err := verifyWithBaseline(t, store, "proj", "task-committed", "server_test.go", wt); err != nil {
		t.Fatalf("honest committed-change declaration must pass: %v", err)
	}
}

// TestQABaselineDeltaCatchesSamePathReModification: a file modified before
// the task (baseline captured the modified content) and modified AGAIN by
// the task must be reported as changed even though its path was already in
// the baseline.
func TestQABaselineDeltaCatchesSamePathReModification(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	// Dirty BEFORE baseline: server_test.go already modified vs HEAD.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc PreExisting() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	captureBaselineForTask(t, store, "proj", "task-remodify", wt)
	// The task modifies the SAME file again.
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TaskEdit() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyWithBaseline(t, store, "proj", "task-remodify", "none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("same-path re-modification must be reported, got: %v", err)
	}
	if err := verifyWithBaseline(t, store, "proj", "task-remodify", "server_test.go", wt); err != nil {
		t.Fatalf("declaring the re-modification must pass: %v", err)
	}
}

// TestQABaselineDeltaCatchesDeletion: a baseline file deleted by the task
// surfaces as a delta path under its original name.
func TestQABaselineDeltaCatchesDeletion(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-delete", wt)
	if err := os.Remove(filepath.Join(wt, "server_test.go")); err != nil {
		t.Fatal(err)
	}
	err := verifyWithBaseline(t, store, "proj", "task-delete", "none", wt)
	if err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("deletion must be reported, got: %v", err)
	}
}

// TestQAGateLostBaselineFailsClosed (S2-2, reviewer P0-1): the worktree's
// capture manifest proves a baseline EXISTED; if the control plane cannot
// produce it (agent destroyed the record path? control-plane loss?), the
// gate must fail closed — silently degrading to the weaker absolute status
// surface would let an agent neutralize the gate by staging noise.
func TestQAGateLostBaselineFailsClosed(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-lost", wt)
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The control-plane record disappears.
	if err := store.DeleteQABaselineRecord("proj", "task-lost"); err != nil {
		t.Fatal(err)
	}
	err := verifyWithBaseline(t, store, "proj", "task-lost", "server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("lost baseline must fail closed, got: %v", err)
	}
}

// TestQAGateTamperedWorktreeCopyFailsClosed (S2-2, reviewer P0-1): the
// agent-writable worktree copy must never win a disagreement with the
// control plane — a forged local baseline cannot launder real changes.
func TestQAGateTamperedWorktreeCopyFailsClosed(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-tamper", wt)
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc Bad() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The agent rewrites the local copy to claim the worktree was born
	// dirty (laundering its own edit into "pre-existing state").
	forged := map[string]gitworktree.QABaselineEntry{}
	for _, p := range []string{"server.go", "server_test.go"} {
		forged[p] = gitworktree.QABaselineEntry{Fingerprint: "forged"}
	}
	payload, _ := json.Marshal(gitworktree.QABaseline{
		SchemaVersion: gitworktree.QABaselineSchemaVersion,
		HeadCommit:    "deadbeef",
		Entries:       forged,
	})
	if err := os.WriteFile(gitworktree.QABaselinePath(wt), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyWithBaseline(t, store, "proj", "task-tamper", "server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("forged worktree baseline must fail closed, got: %v", err)
	}
}

// TestQAGateCorruptControlPlaneBaselineFailsClosed: a corrupt control-plane
// document must not silently downgrade to absolute measurement.
func TestQAGateCorruptControlPlaneBaselineFailsClosed(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-corrupt", wt)
	// Corrupt the authoritative record.
	if err := store.db.UpsertRecord(qaBaselineTable, store.workspaceID, []string{"proj", "task-corrupt"}, "{not json"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyWithBaseline(t, store, "proj", "task-corrupt", "server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt control-plane baseline must fail closed, got: %v", err)
	}
}

// TestDeliveryCheckpointAllowsBusinessFiles (S2-2 ⑤, fix round S2-2.1): a
// BRANCH JOIN is a delivery checkpoint — the branch's whole baseline delta
// IS the deliverable, so the test-artifact whitelist (Direction 3) must NOT
// apply. A branch delivering business code (server.go) passes the
// declaration cross-check; the same delta on a linear QA checkpoint
// (legacy surface) still fails. Regression for the structural deadlock
// where the whitelist comment existed but Direction 3 fired anyway.
func TestDeliveryCheckpointAllowsBusinessFiles(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-delivery", wt)
	// The branch's real deliverable: a business-code edit, honestly declared.
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc Delivery() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "task-delivery")
	if err != nil {
		t.Fatal(err)
	}
	surf.deliveryDelta = true // branch-join checkpoint kind
	if err := verifyWorktreeDeltaAgainstDeclaration("server.go", wt, surf); err != nil {
		t.Fatalf("delivery checkpoint must accept business-code deliverables, got: %v", err)
	}

	// Same delta, same baseline, LEGACY linear-QA surface: whitelist still
	// applies — QA editing business files must keep failing.
	legacySurf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "task-delivery")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyWorktreeDeltaAgainstDeclaration("server.go", wt, legacySurf); err == nil || !strings.Contains(err.Error(), "test-artifact") {
		t.Fatalf("legacy QA checkpoint must keep rejecting business files, got: %v", err)
	}
}

// TestDeliveryCheckpointKeepsDeclarationDirections (S2-2.2 review P2): the
// delivery flag suppresses ONLY Direction 3 (the test-artifact whitelist).
// Directions 1 and 2 — the honest-declaration cross-check — must stay
// active, or the delivery checkpoint would degrade into a whitelist-only
// gate that lets undeclared changes through.
func TestDeliveryCheckpointKeepsDeclarationDirections(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	captureBaselineForTask(t, store, "proj", "task-delivery-directions", wt)
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc Delivery() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "client.go"), []byte("package main\n\nfunc Undeclared() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "task-delivery-directions")
	if err != nil {
		t.Fatal(err)
	}
	surf.deliveryDelta = true

	// Direction 1: an undeclared real change must still be rejected.
	err = verifyWorktreeDeltaAgainstDeclaration("server.go", wt, surf)
	if err == nil || !strings.Contains(err.Error(), "client.go") {
		t.Fatalf("delivery checkpoint must keep Direction 1 (undeclared change), got: %v", err)
	}

	// Direction 2: a "none" declaration over real changes must still fail.
	err = verifyWorktreeDeltaAgainstDeclaration("none", wt, surf)
	if err == nil || !strings.Contains(err.Error(), "changed path") {
		t.Fatalf("delivery checkpoint must keep Direction 2 (none over real changes), got: %v", err)
	}

	// Phantom declarations (Direction 2b): a declared-but-unchanged path
	// fails. Direction 1 fires first here (client.go is also undeclared),
	// so use a declaration that covers the real changes plus one phantom.
	err = verifyWorktreeDeltaAgainstDeclaration("server.go\nclient.go\nmissing.go", wt, surf)
	if err == nil || !strings.Contains(err.Error(), "missing.go") {
		t.Fatalf("delivery checkpoint must keep Direction 2 phantom rejection, got: %v", err)
	}

	// Honest full declaration still passes.
	if err := verifyWorktreeDeltaAgainstDeclaration("server.go\nclient.go", wt, surf); err != nil {
		t.Fatalf("honest full declaration must pass the delivery checkpoint, got: %v", err)
	}
}

// TestQAGateNoBaselineFallsBackAbsolute: worktrees created before the fix
// have NO baseline AND no capture manifest; the gate falls back to the
// previous absolute status measurement (still fail-closed, still strict).
func TestQAGateNoBaselineFallsBackAbsolute(t *testing.T) {
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	// Legacy worktree: remove the recovery copy (the fixture captures the
	// baseline document but never uploads it, so no manifest was ever
	// written and no control-plane record exists).
	if err := os.Remove(gitworktree.QABaselinePath(wt)); err != nil {
		t.Fatal(err)
	}
	// Scaffold noise now FAILS again (absolute surface) — documented
	// degradation for legacy worktrees, never a silent pass.
	if err := os.WriteFile(filepath.Join(wt, ".mcp.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := verifyWithBaseline(t, store, "proj", "task-legacy", "server_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("legacy worktree without baseline must use absolute status, got: %v", err)
	}
}

// TestQABaselineCaptureUploadFailureLeavesNoCanary (S2-2 ordering): when the
// control-plane upload fails, neither the recovery copy nor the manifest may
// remain — that combination would trigger the fail-closed tamper path for
// the whole life of the worktree.
func TestQABaselineCaptureUploadFailureLeavesNoCanary(t *testing.T) {
	wt := newGitWorktree(t)
	// The fixture already wrote a worktree-side copy via CaptureQABaseline;
	// simulate a failed upload by removing the DB-backed store's ability to
	// persist (nil db) and re-running the record capture.
	broken := &Store{workspaceID: "ws-baseline"}
	err := broken.CaptureQABaselineRecord("proj", "task-upload-fail", wt)
	if err == nil {
		t.Fatal("upload failure must surface as an error")
	}
	if _, err := os.Stat(gitworktree.QABaselinePath(wt)); !os.IsNotExist(err) {
		t.Fatalf("recovery copy must be rolled back after failed upload: %v", err)
	}
	if _, err := os.Stat(gitworktree.QABaselineManifestPath(wt)); !os.IsNotExist(err) {
		t.Fatalf("manifest must not be written when upload failed: %v", err)
	}
}
