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
	// On a detached head or missing base, fall back to requiring HEAD != base.
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", base+"..HEAD").CombinedOutput()
	if err != nil {
		// Base branch may not exist locally (shallow clone without base):
		// fall back to comparing against the empty tree of HEAD~0 semantics.
		out2, err2 := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").CombinedOutput()
		if err2 != nil {
			return false, false, fmt.Errorf("git rev-list: %v: %s", err, strings.TrimSpace(string(out)))
		}
		commitBeyondBase = strings.TrimSpace(string(out2)) != ""
	} else {
		commitBeyondBase = strings.TrimSpace(string(out)) != "0"
	}
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
