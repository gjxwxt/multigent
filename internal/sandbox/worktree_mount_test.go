package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeParentMountLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")

	wt := filepath.Join(repo, ".multigent", "worktrees", "t-1")
	if err := os.MkdirAll(filepath.Dir(wt), 0755); err != nil {
		t.Fatal(err)
	}
	run("worktree", "add", wt, "-b", "feature/t-1")

	got := WorktreeParentMount(wt, false)
	want := repo + ":" + repo
	if got != want {
		// macOS temp dirs are symlinked (/var -> /private/var); compare cleaned forms.
		repoReal, _ := filepath.EvalSymlinks(repo)
		wtReal, _ := filepath.EvalSymlinks(wt)
		if WorktreeParentMount(wtReal, false) != repoReal+":"+repoReal {
			t.Fatalf("WorktreeParentMount() = %q, want %q (real: %q)", got, want, repoReal+":"+repoReal)
		}
	}
	if ro := WorktreeParentMount(wt, true); !strings.HasSuffix(ro, ":ro") {
		t.Fatalf("readOnly mount = %q, want :ro suffix", ro)
	}
	// The parent checkout itself needs no extra mount.
	if got := WorktreeParentMount(repo, false); got != "" {
		t.Fatalf("WorktreeParentMount(parent) = %q, want empty", got)
	}
	// A directory without a .git file needs no extra mount.
	if got := WorktreeParentMount(t.TempDir(), false); got != "" {
		t.Fatalf("WorktreeParentMount(plain dir) = %q, want empty", got)
	}
}
