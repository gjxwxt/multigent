package gitworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newAnchorRepo builds a git repo mirroring the fu91es delivery shape:
// base commit (task creation) -> delivery commit (implementation's
// delivery_sha) -> the QA agent's own delta on top.
func newAnchorRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		run("add", "-A")
		run("commit", "-m", msg)
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	write("base.txt", "base\n")
	write("tracked.txt", "v1\n")
	commit("base")
	// The recorded delivery: implementation's work, anchored at delivery_sha.
	write("tracked.txt", "v2 (delivered)\n")
	write("delivered.go", "package delivered\n")
	commit("delivery")
	// The QA agent's own delta ON TOP of the delivery: an uncommitted edit to
	// a tracked file, an untracked new test file, and platform runtime noise
	// that must stay excluded.
	write("tracked.txt", "v2 (delivered)\nqa touched this\n")
	write("docs/qa/qa-evidence.md", "evidence\n")
	if err := os.MkdirAll(filepath.Join(dir, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	write(".multigent/runtime.json", "{}\n")
	return dir
}

func TestWorktreeDeltaSinceCommitExcludesDelivery(t *testing.T) {
	dir := newAnchorRepo(t)
	delivery := revParse(t, dir, "HEAD")
	delta, err := WorktreeDeltaSinceCommit(dir, delivery)
	if err != nil {
		t.Fatalf("delta since delivery: %v", err)
	}
	got := strings.Join(delta, "\n")
	if !strings.Contains(got, "tracked.txt") || !strings.Contains(got, "docs/qa/qa-evidence.md") {
		t.Fatalf("QA's own delta missing, got: %q", got)
	}
	if strings.Contains(got, "delivered.go") || strings.Contains(got, "base.txt") {
		t.Fatalf("delivery content must not count as the QA delta, got: %q", got)
	}
	if strings.Contains(got, ".multigent") {
		t.Fatalf("platform runtime files must stay excluded, got: %q", got)
	}
}

func TestWorktreeDeltaSinceCommitStaleAnchorFailsClosed(t *testing.T) {
	dir := newAnchorRepo(t)
	if _, err := WorktreeDeltaSinceCommit(dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("an anchor the worktree cannot resolve must fail closed")
	}
}

func TestWorktreeDeltaSinceCommitAncestorOfHead(t *testing.T) {
	dir := newAnchorRepo(t)
	// Anchor = the commit BEFORE the delivery (base): the delta now includes
	// the delivery itself plus the QA delta — proving the anchor semantics
	// follow the given commit, not "HEAD minus nothing".
	base := revParse(t, dir, "HEAD~1")
	delta, err := WorktreeDeltaSinceCommit(dir, base)
	if err != nil {
		t.Fatalf("delta since base: %v", err)
	}
	got := strings.Join(delta, "\n")
	if !strings.Contains(got, "delivered.go") || !strings.Contains(got, "docs/qa/qa-evidence.md") {
		t.Fatalf("delta vs base must include delivery AND QA work, got: %q", got)
	}
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}
