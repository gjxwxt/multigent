package runner

import (
	"fmt"
	"os/exec"
	"strings"
)

// pushEvidence is the SHA-accurate push state of the delivery branch
// (review round 3, item 2): a remote branch merely EXISTING is not push
// evidence — it may sit at an older commit while the local delivery commit
// is unpushed. RemoteHasBranch + RemoteSHA == LocalSHA is the only "pushed"
// answer; the SHAs are surfaced so the failure message can tell the user
// exactly what the remote is missing.
type pushEvidence struct {
	RemoteHasBranch bool
	RemoteSHA       string
	LocalSHA        string
}

// gitDeliveryEvidence reports delivery evidence for the run workspace.
// baseRef is the FROZEN base (task.BaseCommit when set, else the base
// branch name — resolved by the caller); commit evidence counts commits on
// HEAD not reachable from that base ref, so a moved base branch cannot
// launder or hide the increment.
//
// Fallback responsibility (review round 4, P1-1): baseCommit/BaseBranch
// selection happens entirely in ValidateGitDeliveryEvidence; the final
// "main" default for a fully-unset task lives HERE and is the only place
// it does. Changing either side without the other can break the fail-closed
// guarantee (unresolvable base = error, never a silent success). Read-only and local: every git invocation
// is `git -C dir`, no fetch, no push; failures return an error for the
// caller to surface (unprovable is not delivered).
func gitDeliveryEvidence(dir, baseRef, branchName string) (commitBeyondBase bool, push pushEvidence, err error) {
	base := strings.TrimSpace(baseRef)
	if base == "" {
		base = "main"
	}
	// Commit evidence: at least one commit on HEAD not reachable from the
	// frozen base. Review fix (P1-ii, 2026-09-24): when the base ref is not
	// resolvable (shallow clone, unfetched base SHA) we MUST fail closed —
	// the previous fallback (rev-list --count HEAD) turned "cannot prove a
	// commit beyond base" into "delivered" (count "0" is non-empty), a false
	// positive. Round-3 item 1: prefer the frozen BaseCommit SHA over the
	// base branch name so the increment is measured against the exact
	// baseline the task was created from.
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", base+"..HEAD").CombinedOutput()
	if err != nil {
		return false, pushEvidence{}, fmt.Errorf("git rev-list %s..HEAD (base ref not resolvable): %v: %s", base, err, strings.TrimSpace(string(out)))
	}
	commitBeyondBase = strings.TrimSpace(string(out)) != "0"
	branch := strings.TrimSpace(branchName)
	if branch == "" {
		return commitBeyondBase, pushEvidence{}, nil
	}
	// Local branch tip — resolved via refs/heads/<branch>, not HEAD: a
	// detached worktree must not silently compare a different ref.
	localOut, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		return commitBeyondBase, pushEvidence{}, fmt.Errorf("git rev-parse refs/heads/%s: %v: %s", branch, err, strings.TrimSpace(string(localOut)))
	}
	push.LocalSHA = strings.TrimSpace(string(localOut))
	ls, err := exec.Command("git", "-C", dir, "ls-remote", "--heads", "origin", branch).CombinedOutput()
	if err != nil {
		return commitBeyondBase, pushEvidence{}, fmt.Errorf("git ls-remote: %v: %s", err, strings.TrimSpace(string(ls)))
	}
	if line := strings.TrimSpace(string(ls)); line != "" {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			push.RemoteHasBranch = true
			push.RemoteSHA = fields[0]
		}
	}
	return commitBeyondBase, push, nil
}
