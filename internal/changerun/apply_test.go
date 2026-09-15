package changerun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	res, err := e.Apply(s, p.ID, "operator")
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
		if _, err := e.Apply(s, p.ID, "op"); err == nil || !strings.Contains(err.Error(), "high-risk") {
			t.Fatalf("path %v must be rejected, got: %v", paths, err)
		}
		// Blacklist rejection happens before BeginApply — the proposal stays
		// awaiting_approval and still occupies the slot, so reject it to clean up.
		if _, err := s.Reject(p.ID, "op"); err != nil {
			t.Fatalf("cleanup reject: %v", err)
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
	if _, err := e.Apply(s, p.ID, "op"); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
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
	if _, err := e.Apply(s, p.ID, "op"); err == nil || !strings.Contains(err.Error(), "undeclared") {
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
	if _, err := e.Apply(s, p.ID, "op"); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary patch must be rejected, got: %v", err)
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
	if _, err := e.Apply(s, p.ID, "op"); err == nil {
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
			_, errs[i] = e.Apply(s, p.ID, "op")
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
	if _, err := e.Apply(s, p.ID, "op"); err != nil {
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
