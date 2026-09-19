package gitworktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	// Round-11: HOME / XDG_CONFIG_HOME select the host's global git config
	// (~/.gitconfig, ~/.config/git/*, includeIf chains, global hooks,
	// credential stores). The purified clone runs git with no host config at
	// all — GIT_CONFIG_NOSYSTEM=1 plus stripped HOME/XDG.
	"HOME",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
	"XDG_DATA_HOME",
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
	// Ref is an optional specific ref to fetch if Commit is not on a default branch
	// (e.g. refs/mg-turns/<turnID>).
	Ref string
	// Dest is the destination directory (must not exist or must be empty).
	Dest string
	// Env adds extra environment variables for git subprocesses (on top of
	// the scrubbed baseline).
	Env []string
}

// EnsurePurifiedClone creates an isolated clone of SourceRepo at Dest,
// containing exactly Commit, honoring the round-10/round-11 purification
// contract:
//
//  1. `git clone --no-local` — forces the pack transport so objects are
//     copied, never hardlinked into the source object store.
//  2. `--no-checkout` — the platform controls what the working tree gets.
//  3. reject the clone if it has alternates (defense in depth: an
//     objects/info/alternates file would defeat --no-local).
//  4. delete every remote before returning — the agent-facing clone has no
//     push/fetch targets (push attempts fail with "No such remote").
//  5. scrub GIT_* / credential / editor / HOME / XDG environment from every
//     git subprocess this helper runs, so host-side global config
//     (~/.gitconfig, includeIf, hooks, fsmonitors) cannot leak in.
//  6. checkout the requested commit and VERIFY the working tree HEAD equals
//     it — the agent must receive a real, correct working tree, not a
//     --no-checkout shell (round-11 fix: cat-file alone is not enough).
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
	runIn := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	run := func(args ...string) error {
		_, err := runIn(dest, args...)
		if err != nil {
			return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, RedactGitOutput(strings.TrimSpace("")))
		}
		return nil
	}

	if out, err := runIn(filepath.Dir(dest), "clone", "--no-local", "--no-checkout", src, dest); err != nil {
		cleanup()
		return nil, fmt.Errorf("git clone --no-local: %w (%s)", err, RedactGitOutput(strings.TrimSpace(out)))
	}

	// Round-10 P0 defense: alternates would silently re-share objects with
	// the source even after --no-local.
	if err := rejectAlternates(dest); err != nil {
		cleanup()
		return nil, err
	}

	// Ensure the requested commit is available in the clone. When Commit
	// is on a non-branch ref (e.g. refs/mg-turns/*), git clone --no-local
	// does not fetch it automatically. Fetch it from origin before stripping remotes.
	if err := run("cat-file", "-e", commit+"^{commit}"); err != nil {
		fetchTarget := commit
		if ref := strings.TrimSpace(opts.Ref); ref != "" {
			fetchTarget = ref
		}
		if out, fetchErr := runIn(dest, "fetch", "--no-tags", "origin", fetchTarget); fetchErr != nil {
			cleanup()
			return nil, fmt.Errorf("purified clone fetch commit %s (target %s): %w (%s)", commit, fetchTarget, fetchErr, RedactGitOutput(strings.TrimSpace(out)))
		}
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

	// Round-11 fix: the agent must receive a REAL working tree at the exact
	// baseline commit — checkout the commit and verify HEAD == commit.
	// cat-file alone would hand over an empty --no-checkout shell.
	if err := run("checkout", "--detach", commit); err != nil {
		cleanup()
		return nil, fmt.Errorf("checkout baseline commit %s: %w", commit, err)
	}
	headOut, err := runIn(dest, "rev-parse", "HEAD")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("verify clone HEAD: %w", err)
	}
	if head := strings.TrimSpace(headOut); head != commit {
		cleanup()
		return nil, fmt.Errorf("purified clone HEAD %s does not match baseline %s", head, commit)
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
// could redirect Git into the host checkout, read host global config, or leak
// credentials — plus GIT_CONFIG_NOSYSTEM=1 (round-11: HOME/XDG stripping alone
// leaves /etc/gitconfig readable) and any caller-provided additions.
// Round-13 P1: caller-provided extras are filtered against the same protected
// key set AND against the forced key, so a caller cannot re-inject HOME,
// GIT_DIR, or GIT_CONFIG_NOSYSTEM=0 behind the scrub.
func purifiedGitEnv(extra []string) []string {
	keep := os.Environ()
	env := make([]string, 0, len(keep)+len(extra)+1)
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
	// Caller additions last-partial-wins per normal env semantics, but
	// protected keys never survive the filter.
	for _, kv := range extra {
		key, _, _ := strings.Cut(kv, "=")
		if key == forcedConfigNosystemKey {
			continue // forced below; callers cannot weaken it
		}
		banned := false
		for _, b := range envScrubKeys {
			if key == b {
				banned = true
				break
			}
		}
		if !banned && strings.Contains(kv, "=") {
			env = append(env, kv)
		}
	}
	// Forced LAST so it wins over any same-named entry from the base
	// environment: config authority starts from the clone itself.
	return append(env, forcedConfigNosystemKey+"=1")
}

// forcedConfigNosystemKey is the single env key the purification contract
// always forces to "1", regardless of caller extras.
const forcedConfigNosystemKey = "GIT_CONFIG_NOSYSTEM"

// SanitizedDiffArgs returns the git arguments for extracting a diff from an
// untrusted clone (round-10 P0): disable external diff drivers and textconv
// (they execute project-controlled config) and neutralize fsmonitors/hooks.
//
// The diff-specific flags must follow the subcommand: git rejects
// "--no-ext-diff" as a global option with "unknown option", so simply
// prefixing all four produces a command that cannot run at all.
func SanitizedDiffArgs(args ...string) []string {
	out := append([]string{
		"-c", "core.fsmonitor=",
		"-c", "core.hooksPath=",
	}, args...)
	for i := 0; i < len(out); i++ {
		if out[i] == "-c" {
			i++ // skip the name=value pair so a value like "diff.x=y" is not mistaken for the subcommand
			continue
		}
		if out[i] == "diff" || out[i] == "log" || out[i] == "show" {
			rest := append([]string{"--no-ext-diff", "--no-textconv"}, out[i+1:]...)
			return append(out[:i+1], rest...)
		}
	}
	return append(out, "--no-ext-diff", "--no-textconv")
}

// SanitizedExecutionArgs returns the git arguments for running git commands against
// untrusted repositories or worktrees, neutralizing fsmonitor, hooks, filters, and external diffs.
func SanitizedExecutionArgs(args ...string) []string {
	base := []string{
		"-c", "core.fsmonitor=",
		"-c", "core.useBuiltinFSMonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "filter.lfs.smudge=",
		"-c", "filter.lfs.clean=",
		"-c", "filter.lfs.process=",
		"-c", "filter.lfs.required=false",
		"-c", "diff.external=",
	}
	return append(base, args...)
}

// SanitizedGitEnv returns the environment for git subprocesses that run
// against a repository whose config/hooks an agent (or preview copilot) may
// have influenced. It strips every variable that redirects Git into a host
// checkout or attacker-controlled object store (GIT_DIR, GIT_*_OBJECT_*,
// global config pointers, credential surfaces, editors) and forces
// GIT_CONFIG_NOSYSTEM=1 — the same scrub the purified clone uses, exported
// for the review-commit path and other host-side git calls touching
// agent-writable trees (Batch 3 wiring, round-14). Caller extras are filtered
// against the protected key set and cannot re-inject anything scrubbed.
func SanitizedGitEnv(extra ...string) []string {
	return purifiedGitEnv(extra)
}

var filterPattern = regexp.MustCompile(`(?i)(?:^|[\s,;])(?:filter|diff)\s*=\s*([a-zA-Z0-9_\-\.]+)`)

// NeutralizeFilterArgsForDir inspects any .gitattributes in dir (and .git/info/attributes)
// for custom filter and diff driver names, returning CLI overrides (-c filter.<name>.clean=, etc.)
// that prevent host git from executing external processes defined in git configuration.
func NeutralizeFilterArgsForDir(dir string) []string {
	if dir == "" {
		return nil
	}
	drivers := make(map[string]bool)
	drivers["lfs"] = true

	scanFile := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		matches := filterPattern.FindAllStringSubmatch(string(data), -1)
		for _, m := range matches {
			if len(m) > 1 {
				name := strings.TrimSpace(m[1])
				if name != "" && name != "none" {
					drivers[name] = true
				}
			}
		}
	}

	// 1. Root .gitattributes
	scanFile(filepath.Join(dir, ".gitattributes"))
	// 2. .git/info/attributes
	scanFile(filepath.Join(dir, ".git", "info", "attributes"))
	// 3. Subdirectories (up to depth 4)
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" || base == ".multigent" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() == ".gitattributes" && p != filepath.Join(dir, ".gitattributes") {
			scanFile(p)
		}
		return nil
	})

	var sortedDrivers []string
	for d := range drivers {
		sortedDrivers = append(sortedDrivers, d)
	}
	sort.Strings(sortedDrivers)

	var overrides []string
	for _, d := range sortedDrivers {
		overrides = append(overrides,
			"-c", fmt.Sprintf("filter.%s.clean=", d),
			"-c", fmt.Sprintf("filter.%s.process=", d),
			"-c", fmt.Sprintf("filter.%s.smudge=", d),
			"-c", fmt.Sprintf("filter.%s.required=false", d),
			"-c", fmt.Sprintf("diff.%s.command=", d),
			"-c", fmt.Sprintf("diff.%s.textconv=", d),
		)
	}
	return overrides
}

// SanitizeUntrustedClone scrubs all external hooks, worktree configurations, attributes,
// and custom filter/diff/include/alias sections from a clone's .git directory.
// This guarantees that host git operations on an untrusted clone cannot invoke arbitrary processes.
func SanitizeUntrustedClone(dir string) error {
	gitDir := filepath.Join(dir, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return nil
	}

	// Remove external hooks, attributes and worktree configs
	_ = os.RemoveAll(filepath.Join(gitDir, "hooks"))
	_ = os.Remove(filepath.Join(gitDir, "config.worktree"))
	_ = os.Remove(filepath.Join(gitDir, "info", "attributes"))

	// Scrub .git/config
	cfgPath := filepath.Join(gitDir, "config")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read clone git config: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	var sanitizedLines []string
	inBannedSection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			sec := strings.ToLower(trimmed)
			if strings.HasPrefix(sec, "[filter") ||
				strings.HasPrefix(sec, "[diff") ||
				strings.HasPrefix(sec, "[include") ||
				strings.HasPrefix(sec, "[alias") {
				inBannedSection = true
				continue
			}
			inBannedSection = false
		}
		if inBannedSection {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "fsmonitor") ||
			strings.HasPrefix(lower, "hookspath") ||
			strings.HasPrefix(lower, "attributesfile") ||
			strings.HasPrefix(lower, "pager") ||
			strings.HasPrefix(lower, "editor") ||
			strings.HasPrefix(lower, "sshcommand") ||
			strings.HasPrefix(lower, "askpass") {
			continue
		}
		sanitizedLines = append(sanitizedLines, line)
	}

	if err := os.WriteFile(cfgPath, []byte(strings.Join(sanitizedLines, "\n")), 0600); err != nil {
		return fmt.Errorf("write sanitized clone git config: %w", err)
	}
	return nil
}

// SanitizedExecutionArgsForDir returns SanitizedExecutionArgs plus directory-specific
// filter/diff neutralization arguments.
func SanitizedExecutionArgsForDir(dir string, args ...string) []string {
	base := SanitizedExecutionArgs()
	neutral := NeutralizeFilterArgsForDir(dir)
	var full []string
	full = append(full, base...)
	full = append(full, neutral...)
	full = append(full, args...)
	return full
}

