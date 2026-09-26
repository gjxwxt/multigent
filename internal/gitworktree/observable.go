package gitworktree

import (
	"os"
	"os/exec"
	"strings"
)

// ObservableWorktree reports whether dir exists, is a directory, and is a
// readable git worktree. It is the SINGLE source of truth for the
// "is this directory a worktree we can measure" decision:
//
//   - the linear QA real-change gate (internal/workflow) uses it to fail
//     closed on a missing or unreadable worktree, and
//   - the production worktree resolver (internal/api) uses the same test to
//     skip non-worktree candidates while walking its fallback chain.
//
// One implementation on purpose: the two call sites must never drift, or the
// resolver would hand the gate a directory the gate then refuses to measure
// (the D-N deadlock), or worse, silently skip a candidate the gate would
// have accepted.
func ObservableWorktree(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	cmd.Env = SanitizedGitEnv()
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
