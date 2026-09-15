package gitworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRepo creates a git repo at dir with one commit on main and returns the
// commit SHA.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return run("rev-parse", "HEAD")
}

func TestPurifiedCloneStripsRemotesAndRejectsPushToSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	commit := initRepo(t, src)
	dest := filepath.Join(base, "agent-clone")

	cleanup, err := EnsurePurifiedClone(PurifiedCloneOptions{
		SourceRepo: src,
		Commit:     commit,
		Dest:       dest,
	})
	if err != nil {
		t.Fatalf("purified clone: %v", err)
	}
	defer cleanup()

	// No remotes survive.
	cmd := exec.Command("git", "remote")
	cmd.Dir = dest
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git remote: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("purified clone must have no remotes, got %q", strings.TrimSpace(string(out)))
	}

	// Round-10 P0 regression: pushing to a source path must fail (no remote),
	// never create refs in the source repository.
	push := exec.Command("git", "push", "origin", "HEAD:refs/heads/injected")
	push.Dir = dest
	if err := push.Run(); err == nil {
		t.Fatal("push from purified clone must fail (no remote configured)")
	}
	lsRemote := exec.Command("git", "rev-parse", "--verify", "refs/heads/injected")
	lsRemote.Dir = src
	if err := lsRemote.Run(); err == nil {
		t.Fatal("source repository must not receive refs from the purified clone")
	}
}

func TestPurifiedCloneVerifiesCommitAndRejectsAlternates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	commit := initRepo(t, src)

	// Missing commit must fail the post-clone verification.
	if _, err := EnsurePurifiedClone(PurifiedCloneOptions{SourceRepo: src, Commit: "deadbeef" + strings.Repeat("0", 32), Dest: filepath.Join(base, "c1")}); err == nil {
		t.Fatal("clone with unknown commit must fail")
	}

	// Alternates in the produced clone would defeat --no-local: simulate by
	// planting one and verifying rejectAlternates fires.
	dest := filepath.Join(base, "c2")
	cleanup, err := EnsurePurifiedClone(PurifiedCloneOptions{SourceRepo: src, Commit: commit, Dest: dest})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer cleanup()
	altDir := filepath.Join(base, "borrowed", "objects")
	if err := os.MkdirAll(altDir, 0755); err != nil {
		t.Fatalf("mkdir alt: %v", err)
	}
	altPath := filepath.Join(dest, ".git", "objects", "info", "alternates")
	if err := os.WriteFile(altPath, []byte(altDir+"\n"), 0644); err != nil {
		t.Fatalf("plant alternates: %v", err)
	}
	if err := rejectAlternates(dest); err == nil {
		t.Fatal("non-empty alternates file must be rejected")
	}
}

func TestPurifiedGitEnvScrubsHostRedirects(t *testing.T) {
	t.Setenv("GIT_DIR", "/host/checkout/.git")
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", "/host/objects")
	t.Setenv("GIT_ASKPASS", "/host/helper.sh")
	t.Setenv("HOME", "/host/home")

	env := purifiedGitEnv([]string{"EXTRA=1"})
	seen := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	for _, banned := range envScrubKeys {
		if _, ok := seen[banned]; ok {
			t.Fatalf("scrubbed env must not carry %s", banned)
		}
	}
	if seen["EXTRA"] != "1" {
		t.Fatal("extra env must be preserved")
	}
	if seen["PATH"] == "" {
		t.Fatal("unrelated env (PATH) must be preserved")
	}
}

func TestSanitizedDiffArgsDisableExecutableConfig(t *testing.T) {
	args := SanitizedDiffArgs("diff", "HEAD~1", "HEAD")
	joined := strings.Join(args, " ")
	for _, want := range []string{"core.fsmonitor=", "core.hooksPath=", "--no-ext-diff", "--no-textconv"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sanitized diff args must contain %s: %s", want, joined)
		}
	}
}

// Round-11: the clone is checked out at the baseline commit and the working
// tree HEAD is verified — the agent receives a real tree, not a
// --no-checkout shell.
func TestPurifiedCloneChecksOutBaselineCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	commit := initRepo(t, src)
	dest := filepath.Join(base, "agent-clone")

	cleanup, err := EnsurePurifiedClone(PurifiedCloneOptions{SourceRepo: src, Commit: commit, Dest: dest})
	if err != nil {
		t.Fatalf("purified clone: %v", err)
	}
	defer cleanup()

	// The seeded file must exist in the working tree (a --no-checkout shell
	// would leave it absent).
	if _, err := os.Stat(filepath.Join(dest, "file.txt")); err != nil {
		t.Fatalf("baseline working tree must contain seeded files: %v", err)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = dest
	out, err := head.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if strings.TrimSpace(string(out)) != commit {
		t.Fatalf("HEAD %q must equal baseline %q", strings.TrimSpace(string(out)), commit)
	}
}

// Round-11: HOME / XDG_* are stripped so host global git config
// (~/.gitconfig, includeIf, global hooks) cannot leak into the purified
// clone's git operations; GIT_CONFIG_NOSYSTEM=1 is forced.
func TestPurifiedGitEnvStripsHomeAndXDG(t *testing.T) {
	t.Setenv("HOME", "/host/home")
	t.Setenv("XDG_CONFIG_HOME", "/host/xdg-config")
	t.Setenv("XDG_CACHE_HOME", "/host/xdg-cache")

	env := purifiedGitEnv(nil)
	seen := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	for _, banned := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		if _, ok := seen[banned]; ok {
			t.Fatalf("scrubbed env must not carry %s", banned)
		}
	}
	if seen["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatal("GIT_CONFIG_NOSYSTEM=1 must be forced in the purified env")
	}
}

// Round-13 P1: caller-provided extras cannot re-inject protected keys — the
// scrub filter applies to them too, and GIT_CONFIG_NOSYSTEM=1 is forced last
// so it cannot be weakened.
func TestPurifiedGitEnvExtraCannotReinjectProtectedKeys(t *testing.T) {
	t.Setenv("HOME", "/host/home")

	env := purifiedGitEnv([]string{
		"HOME=/attacker/home",
		"GIT_DIR=/attacker/.git",
		"GIT_CONFIG_NOSYSTEM=0",
		"LEGIT_VAR=fine",
	})
	seen := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v
	}
	if _, ok := seen["HOME"]; ok {
		t.Fatal("extra must not re-inject HOME")
	}
	if _, ok := seen["GIT_DIR"]; ok {
		t.Fatal("extra must not re-inject GIT_DIR")
	}
	if seen["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatalf("GIT_CONFIG_NOSYSTEM must stay forced to 1, got %q", seen["GIT_CONFIG_NOSYSTEM"])
	}
	if seen["LEGIT_VAR"] != "fine" {
		t.Fatal("legitimate extra vars must pass through")
	}
}

func TestAcquireProjectLockExportedSerialization(t *testing.T) {
	base := t.TempDir()
	unlock1, err := AcquireProjectLock(base)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := AcquireProjectLock(base)
		done <- err
	}()
	// The second acquirer must block until the first releases (30s wait
	// budget far exceeds this probe window).
	select {
	case err := <-done:
		t.Fatalf("second acquire must block while first lock is held, got err=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock1()
	if err := <-done; err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
}
