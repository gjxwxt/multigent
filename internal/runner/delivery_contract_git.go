package runner

import (
	"fmt"
	"os/exec"
	"strings"
)

// gitDeliveryEvidence reports whether a commit exists beyond the base branch
// and whether the declared branch is visible on the remote. Best-effort and
// read-only: every git invocation is local (git -C dir), no fetch, no push,
// and failures return an error for the caller to surface.
func gitDeliveryEvidence(dir, baseBranch, branchName string) (commitBeyondBase, remoteHasBranch bool, err error) {
	base := strings.TrimSpace(baseBranch)
	if base == "" {
		base = "main"
	}
	// Commit evidence: at least one commit on HEAD not reachable from base.
	// Review fix (P1-ii, 2026-09-24): when base is not resolvable (shallow
	// clone, renamed base) we MUST fail closed — the previous fallback
	// (rev-list --count HEAD) turned "cannot prove a commit beyond base"
	// into "delivered" (count "0" is non-empty), a false positive. Unprovable
	// is not delivered.
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", base+"..HEAD").CombinedOutput()
	if err != nil {
		return false, false, fmt.Errorf("git rev-list %s..HEAD (base branch not resolvable): %v: %s", base, err, strings.TrimSpace(string(out)))
	}
	commitBeyondBase = strings.TrimSpace(string(out)) != "0"
	branch := strings.TrimSpace(branchName)
	if branch == "" {
		return commitBeyondBase, false, nil
	}
	ls, err := exec.Command("git", "-C", dir, "ls-remote", "--heads", "origin", branch).CombinedOutput()
	if err != nil {
		return commitBeyondBase, false, fmt.Errorf("git ls-remote: %v: %s", err, strings.TrimSpace(string(ls)))
	}
	remoteHasBranch = strings.TrimSpace(string(ls)) != ""
	return commitBeyondBase, remoteHasBranch, nil
}
