package gitworktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Purified clone contracts (batch plan §4.1, round-10 P0): an agent-facing
// clone of project work must not share object storage with the source
// repository, must not carry a remote pointing back at the source, and must
// not inherit environment that redirects Git into the host checkout.
//
// Round-10 evidence: a plain local clone hardlinks source objects AND keeps
// an origin remote pointing at the source — `git push origin <ref>` inside
// the clone creates refs in the source repository. `--no-local` forces the
// transport machinery (no hardlinks), and every remote is deleted before the
// clone is handed to the agent.

// envScrubKeys are environment variables that redirect Git (or its hooks)
// into a host checkout or attacker-controlled object store. The purified
// clone environment removes them all.
var envScrubKeys = []string{
	"GIT_DIR",
	"GIT_COMMON_DIR",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_WORK_TREE",
	"GIT_NAMESPACE",
	"GIT_CONFIG",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_SYSTEM",
	"GIT_CONFIG_GLOBAL",
	// Hook and diff execution redirection from the host checkout.
	"EDITOR",
	"VISUAL",
	"GIT_EDITOR",
	"GIT_PAGER",
	"PAGER",
	// Credential surface: the purified clone must never see host remotes'
	// credentials; pushes from it fail closed instead of authenticating.
	"GIT_ASKPASS",
	"SSH_AUTH_SOCK",
	"GIT_SSH",
	"GIT_SSH_COMMAND",
}

// PurifiedCloneOptions controls EnsurePurifiedClone.
type PurifiedCloneOptions struct {
	// SourceRepo is the repository (or worktree path) to clone from.
	SourceRepo string
	// Commit is the exact commit the clone must contain (verified after
	// clone; a mismatch is a hard error, not a warning).
	Commit string
	// Dest is the destination directory (must not exist or must be empty).
	Dest string
	// Env adds extra environment variables for git subprocesses (on top of
	// the scrubbed baseline).
	Env []string
}

// EnsurePurifiedClone creates an isolated clone of SourceRepo at Dest,
// containing exactly Commit, honoring the round-10 purification contract:
//
//  1. `git clone --no-local` — forces the pack transport so objects are
//     copied, never hardlinked into the source object store.
//  2. `--no-checkout` — the platform controls what the working tree gets.
//  3. reject the clone if it has alternates (defense in depth: an
//     objects/info/alternates file would defeat --no-local).
//  4. delete every remote before returning — the agent-facing clone has no
//     push/fetch targets (push attempts fail with "No such remote").
//  5. scrub GIT_* / credential / editor environment from every git
//     subprocess this helper runs.
//  6. verify Commit exists in the clone after purification.
//
// The returned cleanup removes the clone directory; callers own it from
// creation to cleanup.
func EnsurePurifiedClone(opts PurifiedCloneOptions) (cleanup func(), err error) {
	src := strings.TrimSpace(opts.SourceRepo)
	dest := strings.TrimSpace(opts.Dest)
	commit := strings.TrimSpace(opts.Commit)
	if src == "" || dest == "" || commit == "" {
		return nil, fmt.Errorf("purified clone requires source, destination, and commit")
	}
	if filepath.Clean(src) == filepath.Clean(dest) {
		return nil, fmt.Errorf("purified clone destination must differ from source")
	}

	cleanup = func() { _ = os.RemoveAll(dest) }
	// If a previous run left debris, start clean.
	if err := os.RemoveAll(dest); err != nil {
		return nil, fmt.Errorf("clear clone destination: %w", err)
	}

	env := purifiedGitEnv(opts.Env)
	// runIn executes git with the given working directory (the clone step
	// must run in the parent — Dest does not exist yet; post-clone steps run
	// inside the clone).
	runIn := func(dir string, args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, RedactGitOutput(strings.TrimSpace(string(out))))
		}
		return nil
	}
	run := func(args ...string) error { return runIn(dest, args...) }

	if err := runIn(filepath.Dir(dest), "clone", "--no-local", "--no-checkout", src, dest); err != nil {
		cleanup()
		return nil, err
	}

	// Round-10 P0 defense: alternates would silently re-share objects with
	// the source even after --no-local.
	if err := rejectAlternates(dest); err != nil {
		cleanup()
		return nil, err
	}

	// Strip every remote: the agent-facing clone has no upstream to push to.
	remotes, err := listRemotes(dest, env)
	if err != nil {
		cleanup()
		return nil, err
	}
	for _, remote := range remotes {
		if err := run("remote", "remove", remote); err != nil {
			cleanup()
			return nil, fmt.Errorf("remove remote %q: %w", remote, err)
		}
	}

	// The clone must contain the exact requested commit.
	if err := run("cat-file", "-e", commit+"^{commit}"); err != nil {
		cleanup()
		return nil, fmt.Errorf("purified clone missing commit %s", commit)
	}

	return cleanup, nil
}

// rejectAlternates fails when the clone at dir borrows objects from anywhere
// else (its own alternates file exists and is non-empty).
func rejectAlternates(dir string) error {
	alternates := filepath.Join(dir, ".git", "objects", "info", "alternates")
	data, err := os.ReadFile(alternates)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read clone alternates: %w", err)
	}
	if strings.TrimSpace(string(data)) != "" {
		return fmt.Errorf("purified clone must not use alternates objects")
	}
	return nil
}

// listRemotes enumerates configured remotes in the clone.
func listRemotes(dir string, env []string) ([]string, error) {
	cmd := exec.Command("git", "remote")
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git remote: %w", err)
	}
	var remotes []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			remotes = append(remotes, name)
		}
	}
	return remotes, nil
}

// purifiedGitEnv builds a scrubbed environment for git subprocesses operating
// inside the purified clone: the current environment minus everything that
// could redirect Git into the host checkout or leak credentials, plus any
// caller-provided additions.
func purifiedGitEnv(extra []string) []string {
	keep := os.Environ()
	env := make([]string, 0, len(keep)+len(extra))
	for _, kv := range keep {
		key, _, _ := strings.Cut(kv, "=")
		skip := false
		for _, banned := range envScrubKeys {
			if key == banned {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// SanitizedDiffArgs returns the git arguments for extracting a diff from an
// untrusted clone (round-10 P0): disable external diff drivers and textconv
// (they execute project-controlled config) and neutralize fsmonitors/hooks.
func SanitizedDiffArgs(args ...string) []string {
	base := []string{
		"-c", "core.fsmonitor=",
		"-c", "core.hooksPath=",
		"--no-ext-diff",
		"--no-textconv",
	}
	return append(base, args...)
}
