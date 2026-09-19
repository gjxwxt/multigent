package gitworktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRepoHead(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(string(runGitOutput(t, dir, "rev-parse", "HEAD")))
}

func initDiffTestRepo(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gitdiff-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@multigent.ai")
	runGit(t, dir, "config", "user.name", "Multigent Tester")
	return dir
}

// commitIn writes/updates one file and returns the new HEAD sha.
func commitIn(t *testing.T, dir, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGit(t, dir, "add", "--", name)
	runGit(t, dir, "commit", "-q", "-m", message)
	return testRepoHead(t, dir)
}

func TestDiffCommitsReportsFileAndPatch(t *testing.T) {
	repo := initDiffTestRepo(t)
	base := commitIn(t, repo, "server.go", "package main\n\nfunc main() {}\n", "base")
	head := commitIn(t, repo, "server.go", "package main\n\nfunc main() { println(\"hi\") }\n", "head")

	diff, err := NewManager().DiffCommits(repo, base, head)
	if err != nil {
		t.Fatalf("DiffCommits: %v", err)
	}
	if diff.Base != base || diff.Head != head {
		t.Fatalf("range echoed wrong: %#v", diff)
	}
	if len(diff.Files) != 1 {
		t.Fatalf("want 1 changed file, got %#v", diff.Files)
	}
	f := diff.Files[0]
	if f.Path != "server.go" || f.Status != "modified" {
		t.Fatalf("unexpected file entry: %#v", f)
	}
	if f.Additions != 1 || f.Deletions != 1 {
		t.Fatalf("unexpected line counts: %#v", f)
	}
	if !strings.Contains(diff.Patch, `+func main() { println("hi") }`) || !strings.Contains(diff.Patch, "@@") {
		t.Fatalf("patch is not a real diff:\n%s", diff.Patch)
	}
	if diff.Truncated {
		t.Fatalf("small diff must not be truncated: %#v", diff)
	}
}

func TestDiffCommitsSeparatesAddedDeletedAndRenamed(t *testing.T) {
	repo := initDiffTestRepo(t)
	commitIn(t, repo, "gone.txt", "bye\n", "seed delete target")
	base := commitIn(t, repo, "old-name.txt", "rename me\n", "seed rename source")

	runGit(t, repo, "rm", "-q", "gone.txt")
	runGit(t, repo, "mv", "old-name.txt", "new-name.txt")
	head := commitIn(t, repo, "added.txt", "new\n", "restructure")

	diff, err := NewManager().DiffCommits(repo, base, head)
	if err != nil {
		t.Fatalf("DiffCommits: %v", err)
	}
	statuses := map[string]DiffFile{}
	for _, f := range diff.Files {
		statuses[f.Path] = f
	}
	if got := statuses["added.txt"]; got.Status != "added" {
		t.Fatalf("added.txt status=%q, want added (%#v)", got.Status, diff.Files)
	}
	if got := statuses["gone.txt"]; got.Status != "deleted" {
		t.Fatalf("gone.txt status=%q, want deleted (%#v)", got.Status, diff.Files)
	}
	renamed, ok := statuses["new-name.txt"]
	if !ok || renamed.Status != "renamed" || renamed.OldPath != "old-name.txt" {
		t.Fatalf("rename not reported with its old path: %#v", diff.Files)
	}
}

func TestDiffCommitsRejectsAnythingButImmutableSHAs(t *testing.T) {
	repo := initDiffTestRepo(t)
	sha := commitIn(t, repo, "a.txt", "a\n", "seed")

	cases := map[string][2]string{
		"branch name instead of sha": {sha, "main"},
		"relative path":              {"../" + sha[:7], sha},
		"option injection":           {"--output=/tmp/pwn", sha},
		"short sha below minimum":    {"abc12", sha},
		"empty base":                 {"", sha},
		"revision syntax":            {sha + "^", sha},
		"range in one field":         {sha + ".." + sha, sha},
	}
	mgr := NewManager()
	for name, pair := range cases {
		if _, err := mgr.DiffCommits(repo, pair[0], pair[1]); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A well-formed but unknown sha must fail as a missing object, not panic.
	if _, err := mgr.DiffCommits(repo, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", sha); err == nil {
		t.Fatal("unknown revision accepted")
	}
}

func TestDiffCommitsIdenticalRevisionsIsEmpty(t *testing.T) {
	repo := initDiffTestRepo(t)
	sha := commitIn(t, repo, "a.txt", "a\n", "seed")
	diff, err := NewManager().DiffCommits(repo, sha, sha)
	if err != nil {
		t.Fatalf("identical range must not error: %v", err)
	}
	if len(diff.Files) != 0 || strings.TrimSpace(diff.Patch) != "" {
		t.Fatalf("identical range must be empty: %#v", diff)
	}
}

// The point of the sanitized invocation: an agent-authored repository must not
// be able to run a program on the console just because a reviewer opened a diff.
func TestDiffCommitsDoesNotExecuteRepoConfiguredDiffDriver(t *testing.T) {
	repo := initDiffTestRepo(t)
	base := commitIn(t, repo, "tracked.txt", "one\n", "base")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* diff=evil\n"), 0644); err != nil {
		t.Fatalf("write gitattributes: %v", err)
	}
	canary := filepath.Join(t.TempDir(), "executed")
	runGit(t, repo, "config", "diff.evil.external", "/bin/touch "+canary)
	head := commitIn(t, repo, "tracked.txt", "two\n", "head")

	diff, err := NewManager().DiffCommits(repo, base, head)
	if err != nil {
		t.Fatalf("DiffCommits: %v", err)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("repository-controlled diff driver executed on the host")
	}
	if !strings.Contains(diff.Patch, "-one") || !strings.Contains(diff.Patch, "+two") {
		t.Fatalf("diff not rendered as plain text:\n%s", diff.Patch)
	}
}

func TestTruncatePatchBoundsOversizedOutput(t *testing.T) {
	whole := strings.Repeat("line\n", 10)
	got, truncated := truncatePatch(whole, 1<<20)
	if truncated || got != whole {
		t.Fatalf("small patch altered: truncated=%v len=%d", truncated, len(got))
	}
	big := strings.Repeat("x", 4096)
	got, truncated = truncatePatch(big, 1000)
	if !truncated {
		t.Fatal("oversized patch not marked truncated")
	}
	if len(got) > 1000 {
		t.Fatalf("truncated patch not bounded: len=%d", len(got))
	}
}
