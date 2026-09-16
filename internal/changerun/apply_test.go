package changerun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRepoFixture builds a real git repo with one commit, returns its path.
func newRepoFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package main\n\nfunc Hi() string { return \"hi\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "baseline")
	return dir
}

func patchFor(oldLine, newLine string) string {
	return `diff --git a/app.go b/app.go
--- a/app.go
+++ b/app.go
@@ -1,3 +1,3 @@
 package main
 
-func Hi() string { return "` + oldLine + `" }
+func Hi() string { return "` + newLine + `" }
`
}

func newEngine(t *testing.T) (*ApplyEngine, *Store) {
	t.Helper()
	repo := newRepoFixture(t)
	s := newTestStore(t)
	return &ApplyEngine{
		WorktreeDir: repo,
		ProjectRoot: repo,
		CloneParent: t.TempDir(),
	}, s
}

func TestApplyEndToEndAndRollback(t *testing.T) {
	e, s := newEngine(t)
	p, err := s.Create("proj", "task-1", "agent", "greet differently", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := e.Apply(s, p.ID, "operator", context.Background())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Postimage) != 1 || res.Postimage["app.go"] == "" {
		t.Fatalf("postimage missing: %v", res.Postimage)
	}
	content, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if !strings.Contains(string(content), `"hello"`) {
		t.Fatalf("patch not applied to worktree: %s", content)
	}
	got, _ := s.Get(p.ID)
	if got.State != StateApplied {
		t.Fatalf("state: %s", got.State)
	}

	// No further edits after apply → rollback must restore HEAD content.
	if _, err := e.Rollback(s, p.ID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	content, _ = os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if !strings.Contains(string(content), `"hi"`) {
		t.Fatalf("rollback did not restore HEAD content: %s", content)
	}
	rolled, _ := s.Get(p.ID)
	if rolled.State != StateRejected || rolled.RollbackState != "rolled_back" {
		t.Fatalf("rollback state: %s/%s", rolled.State, rolled.RollbackState)
	}
}

func TestApplyRefusesHighRiskPaths(t *testing.T) {
	e, s := newEngine(t)
	for i, paths := range [][]string{{".gitlab-ci.yml"}, {"deploy/prod.yaml"}, {"certs/server.pem"}} {
		patch := "diff --git a/" + paths[0] + " b/" + paths[0] + "\n--- a/" + paths[0] + "\n+++ b/" + paths[0] + "\n"
		task := fmt.Sprintf("task-risk-%d", i)
		p, err := s.Create("proj", task, "agent", "r", patch, patch, paths)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := e.Apply(s, p.ID, "op", context.Background()); err == nil || !strings.Contains(err.Error(), "high-risk") {
			t.Fatalf("path %v must be rejected, got: %v", paths, err)
		}
		// The apply itself auto-rejects blacklisted proposals (P1-2: no
		// awaiting_approval stragglers) — the slot must already be free.
		got, err := s.Get(p.ID)
		if err != nil {
			t.Fatalf("get after auto-reject: %v", err)
		}
		if got.State != StateRejected {
			t.Fatalf("blacklisted proposal state = %s, want rejected", got.State)
		}
		// And the task may immediately take a new proposal.
		if _, err := s.Create("proj", task, "agent", "r2", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"}); err != nil {
			t.Fatalf("create after auto-reject: %v", err)
		}
	}
}

func TestApplyRefusesDirtyBaseline(t *testing.T) {
	e, s := newEngine(t)
	if err := os.WriteFile(filepath.Join(e.WorktreeDir, "README.md"), []byte("# dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	p, err := s.Create("proj", "task-1", "agent", "r", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty baseline must be rejected, got: %v", err)
	}
	// Worktree file must be untouched by the failed apply.
	content, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if strings.Contains(string(content), `"hello"`) {
		t.Fatal("rejected apply must not touch the worktree")
	}
}

func TestApplyRejectsUndeclaredPath(t *testing.T) {
	e, s := newEngine(t)
	patch := patchFor("hi", "hello")
	p, err := s.Create("proj", "task-1", "agent", "r", patch, patch, []string{"README.md"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("undeclared path must be rejected, got: %v", err)
	}
}

func TestApplyRejectsBinaryPatch(t *testing.T) {
	e, s := newEngine(t)
	patch := "diff --git a/blob.bin b/blob.bin\n--- a/blob.bin\n+++ b/blob.bin\nGIT binary patch\nliteral 10\n"
	p, err := s.Create("proj", "task-1", "agent", "r", patch, patch, []string{"blob.bin"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary patch must be rejected, got: %v", err)
	}
}

func TestPatchTouchedPathsCoversDeleteRenameAndBothSides(t *testing.T) {
	patch := `diff --git a/deploy/old.yaml b/deploy/new.yaml
similarity index 100%
rename from deploy/old.yaml
rename to deploy/new.yaml
diff --git a/.gitlab-ci.yml b/.gitlab-ci.yml
deleted file mode 100644
index 1234567..0000000
diff --git a/app.go b/app.go
--- a/app.go
+++ b/app.go
@@ -1 +1 @@
-old
+new
`
	got := PatchTouchedPaths(patch)
	want := map[string]bool{"deploy/old.yaml": true, "deploy/new.yaml": true, ".gitlab-ci.yml": true, "app.go": true}
	if len(got) != 4 {
		t.Fatalf("touched paths = %v, want 4 entries", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("unexpected path %q in %v", p, got)
		}
	}
	// Every extracted path must trip the blacklist where applicable — a
	// delete/rename of a CI file can no longer slip past the gate.
	for _, p := range got {
		if p == "app.go" {
			continue
		}
		if !isHighRiskPath(p) {
			t.Fatalf("path %q must be classified high-risk", p)
		}
	}
}

func TestApplyRejectsBrokenPatchParksVerificationFailed(t *testing.T) {
	e, s := newEngine(t)
	badPatch := `diff --git a/missing.go b/missing.go
--- a/missing.go
+++ b/missing.go
@@ -1,2 +1,2 @@
-no such line
+replacement
`
	p, err := s.Create("proj", "task-1", "agent", "r", badPatch, badPatch, []string{"missing.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err == nil {
		t.Fatal("broken patch must fail")
	}
	got, _ := s.Get(p.ID)
	if got.State != StateVerificationFaile {
		t.Fatalf("state after failed clone apply: %s", got.State)
	}
	// Verification-failed is terminal for v1: a new proposal may be created.
	if _, err := s.Create("proj", "task-1", "agent", "retry", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"}); err != nil {
		t.Fatalf("create after verification failure: %v", err)
	}
}

func TestConcurrentAppliesExactlyOneWins(t *testing.T) {
	e, s := newEngine(t)
	p, err := s.Create("proj", "task-1", "agent", "r", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.Apply(s, p.ID, "op", context.Background())
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one apply must win, got %d: %v", wins, errs)
	}
	content, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if strings.Count(string(content), `"hello"`) != 1 {
		t.Fatalf("patch must land exactly once: %s", content)
	}
}

func TestRollbackRefusesPostApplyEdits(t *testing.T) {
	e, s := newEngine(t)
	p, _ := s.Create("proj", "task-1", "agent", "r", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Simulate newer work on top of the applied change.
	if err := os.WriteFile(filepath.Join(e.WorktreeDir, "app.go"), []byte("package main\n\nfunc Hi() string { return \"newer work\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Rollback(s, p.ID); err == nil || !strings.Contains(err.Error(), "refusing to clobber") {
		t.Fatalf("rollback over newer edits must refuse, got: %v", err)
	}
}

func TestScopeMismatchIsNotFound(t *testing.T) {
	e, s := newEngine(t)
	// Proposal belongs to (proj, task-1); engine scoped to another task.
	p, err := s.Create("proj", "task-1", "agent", "r", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.Scope = Scope{Project: "proj", TaskID: "task-OTHER"}
	for name, op := range map[string]func() error{
		"apply":    func() error { _, err := e.Apply(s, p.ID, "op", context.Background()); return err },
		"rollback": func() error { _, err := e.Rollback(s, p.ID); return err },
	} {
		err := op()
		var nf *NotFoundError
		if err == nil || !errorsAsNotFound(err, &nf) {
			t.Fatalf("%s must surface NotFoundError on scope mismatch, got: %v", name, err)
		}
	}
	// State untouched — the operator never reached the record.
	got, _ := s.Get(p.ID)
	if got.State != StateAwaitingApproval {
		t.Fatalf("scope-violated apply must not change state, got %s", got.State)
	}
	// Matching scope proceeds normally.
	e.Scope = Scope{Project: "proj", TaskID: "task-1"}
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err != nil {
		t.Fatalf("apply with matching scope: %v", err)
	}
	if _, err := e.Rollback(s, p.ID); err != nil {
		t.Fatalf("rollback with matching scope: %v", err)
	}
}

func errorsAsNotFound(err error, target **NotFoundError) bool {
	nf, ok := err.(*NotFoundError)
	if ok {
		*target = nf
	}
	return ok
}

func TestConcurrentCreatesExactlyOneWins(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, errs[slot] = s.Create("proj", "task-race", "agent", "r", "p", "d", nil)
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent create must win, got %d: %v", winners, errs)
	}
}

func TestRollbackCoversAddDeleteAndModify(t *testing.T) {
	e, s := newEngine(t)
	// Baseline: app.go exists ("hi"), deleted.go will be removed by the patch.
	if err := os.WriteFile(filepath.Join(e.WorktreeDir, "deleted.go"), []byte("package main\n\nfunc Gone() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.WorktreeDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1", "HOME="+e.WorktreeDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	run("add", ".")
	run("commit", "-m", "with deleted.go")

	patch := `diff --git a/app.go b/app.go
--- a/app.go
+++ b/app.go
@@ -1,3 +1,3 @@
 package main
 
-func Hi() string { return "hi" }
+func Hi() string { return "hello" }
diff --git a/added.go b/added.go
new file mode 100644
--- /dev/null
+++ b/added.go
@@ -0,0 +1 @@
+package main
diff --git a/deleted.go b/deleted.go
deleted file mode 100644
--- a/deleted.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package main
-
-func Gone() {}
`
	p, err := s.Create("proj", "task-1", "agent", "mixed ops", patch, patch, []string{"app.go", "added.go", "deleted.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// All three effects landed.
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "added.go")); err != nil {
		t.Fatal("added.go missing after apply")
	}
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "deleted.go")); !os.IsNotExist(err) {
		t.Fatal("deleted.go still present after apply")
	}
	if _, err := e.Rollback(s, p.ID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// Rollback inverted all three.
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "added.go")); !os.IsNotExist(err) {
		t.Fatal("added.go must be deleted by rollback")
	}
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "deleted.go")); err != nil {
		t.Fatal("deleted.go must be restored by rollback")
	}
	content, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if !strings.Contains(string(content), `"hi"`) {
		t.Fatalf("app.go must be restored: %s", content)
	}
}

func TestValidatorRunsInCloneAndFailureLeavesWorktreeUntouched(t *testing.T) {
	e, s := newEngine(t)
	p, err := s.Create("proj", "task-1", "agent", "r", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var gotDir string
	e.Validator = ValidatorFunc(func(ctx context.Context, dir string) ([]VerificationResult, error) {
		gotDir = dir
		// The clone must already contain the PATCHED content — verification
		// runs after clone-apply.
		content, err := os.ReadFile(filepath.Join(dir, "app.go"))
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(content), `"hello"`) {
			t.Fatalf("validator saw pre-patch clone: %s", content)
		}
		return []VerificationResult{{Command: "cat app.go", OK: true}}, nil
	})
	if _, err := e.Apply(s, p.ID, "op", context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if gotDir == e.WorktreeDir {
		t.Fatal("validator must run in the clone, not the worktree")
	}
	// Failed validator → worktree untouched, proposal parked with trail.
	e2, s2 := newEngine(t)
	p2, _ := s2.Create("proj", "task-1", "agent", "r", patchFor("hi", "hello"), patchFor("hi", "hello"), []string{"app.go"})
	e2.Validator = ValidatorFunc(func(ctx context.Context, dir string) ([]VerificationResult, error) {
		return []VerificationResult{{Command: "make test", OK: false, Output: "FAIL"}}, fmt.Errorf("exit 1")
	})
	if _, err := e2.Apply(s2, p2.ID, "op", context.Background()); err == nil {
		t.Fatal("failed verification must fail the apply")
	}
	content, _ := os.ReadFile(filepath.Join(e2.WorktreeDir, "app.go"))
	if strings.Contains(string(content), `"hello"`) {
		t.Fatal("failed verification must leave the worktree untouched")
	}
	got, _ := s2.Get(p2.ID)
	if got.State != StateVerificationFaile {
		t.Fatalf("state after failed verification: %s", got.State)
	}
	if got.Verification["stage"] != "clone_verify" {
		t.Fatalf("verification trail missing: %v", got.Verification)
	}
}

func TestContainerValidatorRefusesHostExecution(t *testing.T) {
	v := &ContainerValidator{
		Opts: ContainerVerifyOptions{
			Commands: []string{"rm -rf /"},
		},
		RunContainer: func(ctx context.Context, dir, image, workdir, argv string, timeout time.Duration) (string, error) {
			t.Fatal("RunContainer must not be called without an image")
			return "", nil
		},
	}
	if _, err := v.VerifyDir(context.Background(), "/tmp"); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("commands without image must refuse host execution, got: %v", err)
	}
}

func TestContainerValidatorRunsCommandsInOrder(t *testing.T) {
	var ran []string
	v := &ContainerValidator{
		Opts: ContainerVerifyOptions{
			Commands: []string{"go vet ./...", "go test ./..."},
			Image:    "runtime-base:test",
		},
		RunContainer: func(ctx context.Context, dir, image, workdir, argv string, timeout time.Duration) (string, error) {
			ran = append(ran, argv)
			if image != "runtime-base:test" || workdir != "/workspace" || dir == "" {
				t.Fatalf("container args wrong: dir=%q image=%q workdir=%q", dir, image, workdir)
			}
			if argv == "go test ./..." {
				return "ok", nil
			}
			return "", nil
		},
	}
	results, err := v.VerifyDir(context.Background(), "/some/clone")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(ran) != 2 || ran[0] != "go vet ./..." || ran[1] != "go test ./..." {
		t.Fatalf("commands ran out of order: %v", ran)
	}
	if len(results) != 2 || !results[1].OK {
		t.Fatalf("results wrong: %+v", results)
	}
}

// Round-19 P0: container output (and command/error text) must be redacted
// BEFORE anything lands in the proposal record — build/test logs carry
// tokens. Source-level redaction in VerifyDir and sink-level redaction in
// verificationSummary are both asserted, the latter covering custom
// Validators that bypass the source.
func TestVerificationOutputIsRedactedBeforePersistence(t *testing.T) {
	v := &ContainerValidator{
		Opts: ContainerVerifyOptions{
			Commands: []string{"curl -H \"Authorization: Bearer eyhbXhh.eyJzdWIi.9f8e7d6c\" https://ci.internal"},
			Image:    "runtime-base:test",
		},
		RunContainer: func(ctx context.Context, dir, image, workdir, argv string, timeout time.Duration) (string, error) {
			return "running with token sk-proj-abcdef12345678 password=hunter2222\nexit ok", nil
		},
	}
	results, err := v.VerifyDir(context.Background(), "/some/clone")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if strings.Contains(results[0].Command, "eyhbXhh") {
		t.Fatalf("command text not redacted: %s", results[0].Command)
	}
	if strings.Contains(results[0].Output, "sk-proj-") || strings.Contains(results[0].Output, "hunter2222") {
		t.Fatalf("output not redacted: %s", results[0].Output)
	}
	if !strings.Contains(results[0].Output, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] markers, got: %s", results[0].Output)
	}

	// Sink-level: a custom validator returning raw secrets still cannot
	// reach the persisted summary.
	raw := []VerificationResult{
		{Command: "deploy --token ghp_AbcdefghijKlmnopqrst1234567890Abcdefgh", OK: true, Output: "Bearer eyhbGciOiJIUzI1NiJ9.eyJzIn0.5f4e3d2c1b payload"},
	}
	summary := verificationSummary(raw)
	for k, val := range summary {
		if strings.Contains(val, "ghp_Abcdefghij") || strings.Contains(val, "eyhbGciOiJIUzI1NiJ9") {
			t.Fatalf("summary %q leaks secrets: %s", k, val)
		}
	}
	if !strings.Contains(summary["cmd_0"], "[REDACTED]") {
		t.Fatalf("summary must carry redaction markers: %v", summary)
	}
}

// Round-19 P1: the failure compensations (recordPostimage / MarkApplied) run
// `git apply --reverse` on the ORIGINAL patch. The hand-rolled inversion they
// previously used produced illegal hunk headers ("@@ +1,7 -1,5"), so the
// compensation could not be trusted. These tests pin the fixed machinery in
// a real git repo: apply → reverse → tree back to baseline, clean status,
// across modify-only and add/delete/modify patches.
func TestReverseCompensationRestoresBaseline(t *testing.T) {
	e, s := newEngine(t)
	patch := patchFor("hi", "hello")
	p, err := s.Create("proj", "task-1", "agent", "r", patch, patch, []string{"app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.BeginApply(p.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyPatchIn(e.WorktreeDir, patch); err != nil {
		t.Fatalf("apply: %v", err)
	}
	content, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if !strings.Contains(string(content), `"hello"`) {
		t.Fatalf("precondition: patch applied, got %s", content)
	}
	if err := applyPatchReverse(e.WorktreeDir, patch); err != nil {
		t.Fatalf("reverse apply must succeed on the original patch: %v", err)
	}
	restored, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if string(restored) != "package main\n\nfunc Hi() string { return \"hi\" }\n" {
		t.Fatalf("worktree must return to baseline, got: %q", restored)
	}
	if dirty, _ := worktreeDirty(e.WorktreeDir); dirty {
		t.Fatal("reverse must leave a clean tree")
	}
}

func TestReverseCompensationCoversAddDeleteModify(t *testing.T) {
	e, s := newEngine(t)
	addPatch := `diff --git a/added.go b/added.go
new file mode 100644
--- /dev/null
+++ b/added.go
@@ -0,0 +1,2 @@
+package main
+
diff --git a/del.go b/del.go
deleted file mode 100644
--- a/del.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-
diff --git a/app.go b/app.go
--- a/app.go
+++ b/app.go
@@ -1,3 +1,3 @@
 package main
 
-func Hi() string { return "hi" }
+func Hi() string { return "hello" }
`
	// Fixture needs del.go present.
	if err := os.WriteFile(filepath.Join(e.WorktreeDir, "del.go"), []byte("package main\n\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.WorktreeDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	run("add", ".")
	run("commit", "-m", "del.go baseline")

	p, err := s.Create("proj", "task-1", "agent", "r", addPatch, addPatch, []string{"added.go", "del.go", "app.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.BeginApply(p.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyPatchIn(e.WorktreeDir, addPatch); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "added.go")); err != nil {
		t.Fatal("precondition: added.go must exist after apply")
	}
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "del.go")); !os.IsNotExist(err) {
		t.Fatal("precondition: del.go must be deleted after apply")
	}
	if err := applyPatchReverse(e.WorktreeDir, addPatch); err != nil {
		t.Fatalf("reverse must undo add/delete/modify: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.WorktreeDir, "added.go")); !os.IsNotExist(err) {
		t.Fatal("reverse must remove the added file")
	}
	del, err := os.ReadFile(filepath.Join(e.WorktreeDir, "del.go"))
	if err != nil || string(del) != "package main\n\n" {
		t.Fatalf("reverse must restore the deleted file, got %q (%v)", del, err)
	}
	app, _ := os.ReadFile(filepath.Join(e.WorktreeDir, "app.go"))
	if !strings.Contains(string(app), `"hi"`) {
		t.Fatalf("reverse must restore the modified file, got %s", app)
	}
	if dirty, _ := worktreeDirty(e.WorktreeDir); dirty {
		t.Fatal("reverse must leave a clean tree across add/delete/modify")
	}
}
