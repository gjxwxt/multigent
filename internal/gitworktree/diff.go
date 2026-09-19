package gitworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Review gates need to show what an agent actually changed. Everything here is
// read-only: the platform never mutates a worktree through this file.
const (
	// MaxDiffFiles bounds how many changed paths one response carries.
	MaxDiffFiles = 200
	// MaxDiffLines bounds the patch by changed lines before it is fetched at
	// all, so a giant refactor cannot make the console fetch hundreds of
	// megabytes of text.
	MaxDiffLines = 5000
	// MaxDiffBytes is a second ceiling on the rendered patch itself.
	MaxDiffBytes = 400 << 10

	minCommitSHALength = 7
)

var commitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// ErrUnknownCommit marks a well-formed commit hash that this repository cannot
// resolve, so callers can answer 404 instead of 500 without string-matching a
// git diagnostic.
var ErrUnknownCommit = errors.New("unknown commit")

// DiffFile is one changed path in a commit range.
type DiffFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

// Diff is the reviewable result of `base..head` where both sides are immutable
// commits. Empty Files with no error means the two commits have identical trees.
type Diff struct {
	Base      string     `json:"base"`
	Head      string     `json:"head"`
	Files     []DiffFile `json:"files"`
	Patch     string     `json:"patch"`
	Truncated bool       `json:"truncated"`
	Note      string     `json:"note,omitempty"`
}

// ValidateCommitSHA rejects everything except an abbreviated or full commit
// hash. This is not cosmetic: refs, revision syntax and leading-dash arguments
// would let a caller widen the range or steer git into option parsing, and the
// endpoint is reachable by anyone who can open the task.
func ValidateCommitSHA(which, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s commit is required", which)
	}
	if !commitSHAPattern.MatchString(trimmed) {
		return "", fmt.Errorf("%s must be a commit hash of %d-%d hex characters, got %q", which, minCommitSHALength, 40, trimmed)
	}
	return strings.ToLower(trimmed), nil
}

// truncatePatch keeps a patch inside maxBytes on a line boundary so the
// rendered diff never ends mid-line.
func truncatePatch(patch string, maxBytes int) (string, bool) {
	if len(patch) <= maxBytes {
		return patch, false
	}
	cut := strings.LastIndex(patch[:maxBytes], "\n")
	if cut <= 0 {
		return patch[:maxBytes], true
	}
	return patch[:cut+1], true
}

// gitSanitized runs one read-only git command against a tree an agent may have
// written, using the same two safeguards the review-commit and change-run paths
// use: sanitized args (no external diff driver, no textconv, no fsmonitor, no
// hooks, no LFS filters) and SanitizedGitEnv (no GIT_* redirection, no global
// config, no credential surfaces). Without both, a planted .git/config entry
// would execute arbitrary programs on the console the moment a reviewer opened a
// diff.
func gitSanitized(dir string, sanitize func(...string) []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitLocalTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", sanitize(args...)...)
	cmd.Dir = dir
	cmd.Env = SanitizedGitEnv()
	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), RedactGitOutput(strings.TrimSpace(string(exitErr.Stderr))))
	}
	return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// DiffCommits returns the change between two commits in a repository or
// worktree directory. Both revisions must resolve to commits; a blob or tree
// hash is rejected so a caller cannot smuggle arbitrary object content in as a
// "diff".
func (m *Manager) DiffCommits(dir, base, head string) (Diff, error) {
	normalizedBase, err := ValidateCommitSHA("base", base)
	if err != nil {
		return Diff{}, err
	}
	normalizedHead, err := ValidateCommitSHA("head", head)
	if err != nil {
		return Diff{}, err
	}
	if _, err := os.Stat(dir); err != nil {
		return Diff{}, fmt.Errorf("repository directory unavailable: %w", err)
	}
	// Serialize with worktree setup, snapshot capture and cleanup: reading a
	// range while one of those rewrites refs would otherwise render a torn view
	// of history to a reviewer.
	unlock, err := acquireProjectLock(ProjectRootForWorktree(dir))
	if err != nil {
		return Diff{}, fmt.Errorf("project git lock unavailable: %w", err)
	}
	defer unlock()
	for _, sha := range []string{normalizedBase, normalizedHead} {
		if _, err := gitSanitized(dir, SanitizedExecutionArgs, "cat-file", "-e", sha+"^{commit}"); err != nil {
			return Diff{}, fmt.Errorf("commit %s is not available in this repository: %w (%w)", shortSHA(sha), ErrUnknownCommit, err)
		}
	}
	if normalizedBase == normalizedHead {
		return Diff{Base: normalizedBase, Head: normalizedHead}, nil
	}

	numstat, err := gitSanitized(dir, SanitizedDiffArgs, "-c", "core.quotepath=false", "diff", "--no-color", "--find-renames", "--numstat", normalizedBase, normalizedHead, "--")
	if err != nil {
		return Diff{}, err
	}
	nameStatus, err := gitSanitized(dir, SanitizedDiffArgs, "-c", "core.quotepath=false", "diff", "--no-color", "--find-renames", "--name-status", normalizedBase, normalizedHead, "--")
	if err != nil {
		return Diff{}, err
	}

	files, totalLines, overflow := parseDiffEntries(numstat, nameStatus)
	diff := Diff{Base: normalizedBase, Head: normalizedHead, Files: files}
	if overflow {
		diff.Truncated = true
		diff.Note = fmt.Sprintf("more than %d files changed; showing the first %d", MaxDiffFiles, len(files))
		return diff, nil
	}
	if totalLines > MaxDiffLines {
		diff.Truncated = true
		diff.Note = fmt.Sprintf("patch omitted: %d changed lines exceeds the %d line review limit", totalLines, MaxDiffLines)
		return diff, nil
	}
	patch, err := gitSanitized(dir, SanitizedDiffArgs, "-c", "core.quotepath=false", "diff", "--no-color", "--find-renames", normalizedBase, normalizedHead, "--")
	if err != nil {
		return Diff{}, err
	}
	patch, truncated := truncatePatch(patch, MaxDiffBytes)
	diff.Patch = patch
	diff.Truncated = diff.Truncated || truncated
	return diff, nil
}

// parseDiffEntries zips `--numstat` with `--name-status`. git emits both in the
// same path order, so pairing by index avoids re-parsing quoted paths.
func parseDiffEntries(numstat, nameStatus string) ([]DiffFile, int, bool) {
	stats := strings.FieldsFunc(numstat, func(r rune) bool { return r == '\n' })
	names := strings.FieldsFunc(nameStatus, func(r rune) bool { return r == '\n' })
	files := make([]DiffFile, 0, len(stats))
	totalLines := 0
	for i, line := range stats {
		additions, deletions, path, binary, ok := parseNumstatLine(line)
		if !ok {
			continue
		}
		entry := DiffFile{Path: path, Additions: additions, Deletions: deletions, Binary: binary}
		if i < len(names) {
			entry.Status, entry.OldPath = parseNameStatusLine(names[i])
		}
		if entry.Status == "" {
			entry.Status = "modified"
		}
		totalLines += additions + deletions
		if len(files) >= MaxDiffFiles {
			return files, totalLines, true
		}
		files = append(files, entry)
	}
	return files, totalLines, false
}

func parseNumstatLine(line string) (additions, deletions int, path string, binary, ok bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) != 3 {
		return 0, 0, "", false, false
	}
	path = normalizeDiffPath(strings.TrimSpace(parts[2]))
	if path == "" {
		return 0, 0, "", false, false
	}
	if parts[0] == "-" || parts[1] == "-" {
		return 0, 0, path, true, true
	}
	additions, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, "", false, false
	}
	deletions, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, path, false, false
	}
	return additions, deletions, path, false, true
}

var diffStatusNames = map[string]string{
	"A": "added",
	"M": "modified",
	"D": "deleted",
	"R": "renamed",
	"C": "copied",
	"T": "type-changed",
	"U": "unmerged",
}

func parseNameStatusLine(line string) (status, oldPath string) {
	parts := strings.Split(line, "\t")
	if len(parts) == 0 {
		return "", ""
	}
	code := strings.TrimSpace(parts[0])
	if len(code) > 1 {
		code = code[:1] // similarity score, e.g. R100
	}
	status = diffStatusNames[code]
	if status == "" {
		status = "modified"
	}
	if (code == "R" || code == "C") && len(parts) >= 3 {
		oldPath = strings.TrimSpace(parts[1])
	}
	return status, oldPath
}

// normalizeDiffPath turns git's rename notation into the single path a reviewer
// should see: "a/{old => new}/b.ts" and "{old => new}/x" both collapse to their
// destination path. OldPath comes from --name-status, which is unambiguous.
func normalizeDiffPath(path string) string {
	if !strings.Contains(path, " => ") {
		return path
	}
	if open := strings.Index(path, "{"); open >= 0 {
		if close := strings.Index(path[open:], "}"); close > 0 {
			abs := open + close
			if from, to, found := strings.Cut(path[open+1:abs], " => "); found && from != "" {
				return path[:open] + strings.TrimSpace(to) + path[abs+1:]
			}
		}
	}
	if _, to, found := strings.Cut(path, " => "); found {
		return strings.TrimSpace(to)
	}
	return path
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
