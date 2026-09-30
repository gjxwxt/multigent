package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// PushEvidence is the SHA-accurate push state of the delivery branch
// (review round 3, item 2): a remote branch merely EXISTING is not push
// evidence — it may sit at an older commit while the local delivery commit
// is unpushed. RemoteHasBranch + RemoteSHA == LocalSHA is the only "pushed"
// answer; the SHAs are surfaced so the failure message can tell the user
// exactly what the remote is missing, and the proven pair is returned to
// the API-side completion gate for the delivery SHA hand-off (the full
// RemoteSHA is the machine-verified candidate anchor for QA).
type PushEvidence struct {
	RemoteHasBranch bool
	RemoteSHA       string
	LocalSHA        string
}

// originEvidenceMode classifies the worktree's `origin` remote for the push
// evidence check.
//
//	originBound       — a real (non-local) remote exists; the SHA-accurate
//	                    ls-remote comparison applies unchanged.
//	originLocalFabric — origin points INSIDE the workspace's .multigent/ tree
//	                    (e.g. a bare repo the agent created at
//	                    /workspace/.multigent/origin-<x>.git to satisfy the
//	                    old push gate). A remote the run itself authored is
//	                    not independent delivery evidence — treating it as
//	                    one is exactly the fabrication the gate exists to
//	                    prevent — so it is downgraded to no-remote mode.
//	originAbsent      — no origin URL configured: the project has no remote,
//	                    and push evidence is unprovable by definition.
type originEvidenceMode int

const (
	originBound originEvidenceMode = iota
	originLocalFabric
	originAbsent
)

// originEvidenceForDir resolves the origin URL read-only from repo config
// (no network, no fetch) and classifies it. Any read error means "cannot
// prove an origin" — the caller then behaves as originAbsent, which for a
// RequirePush contract surfaces a clear violation instead of a fabricated
// remote check.
func originEvidenceForDir(dir string) originEvidenceMode {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return originAbsent
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return originAbsent
	}
	return classifyOriginURL(url)
}

// classifyOriginURL reports originLocalFabric for any spelling of a path
// inside a .multigent/ directory — the platform-managed runtime tree. Both
// POSIX (file:///x/.multigent/origin.git, /x/.multigent/origin.git) and
// scp-like (/x/.multigent:y.git cannot occur but the separator split keeps
// the check path-separator agnostic) spellings are covered; http(s) URLs
// never carry a .multigent path segment in legitimate deployments and fall
// through to originBound.
func classifyOriginURL(url string) originEvidenceMode {
	candidate := url
	// file:// URLs and plain paths share the filesystem namespace; scp-like
	// (host:path) remotes are real network remotes in every deployment shape
	// the platform provisions, so only strip an explicit file:// scheme.
	candidate = strings.TrimPrefix(candidate, "file://")
	for _, segment := range strings.FieldsFunc(candidate, func(r rune) bool {
		return r == '/' || r == filepath.Separator
	}) {
		if segment == ".multigent" {
			return originLocalFabric
		}
	}
	return originBound
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
//
// No-remote mode (B1, 2026-09-29): when the workspace has no usable origin
// (none configured, or one pointing inside .multigent/ — a bare repo the
// agent authored to fake the old push gate), the ls-remote comparison is
// SKIPPED and the local branch tip is returned as the proven evidence pair
// (RemoteSHA == LocalSHA == verified refs/heads/<branch>). The local tip is
// machine-verified by rev-parse, so the delivery SHA hand-off still carries
// a platform-proven anchor; what changes is only that a project without a
// remote can deliver on local evidence instead of being forced to invent
// one.
func gitDeliveryEvidence(dir, baseRef, branchName string, env []string) (commitBeyondBase bool, push PushEvidence, err error) {
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
		return false, PushEvidence{}, fmt.Errorf("git rev-list %s..HEAD (base ref not resolvable): %v: %s", base, err, strings.TrimSpace(string(out)))
	}
	commitBeyondBase = strings.TrimSpace(string(out)) != "0"
	branch := strings.TrimSpace(branchName)
	if branch == "" {
		return commitBeyondBase, PushEvidence{}, nil
	}
	// Local branch tip — resolved via refs/heads/<branch>, not HEAD: a
	// detached worktree must not silently compare a different ref.
	localOut, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		return commitBeyondBase, PushEvidence{}, fmt.Errorf("git rev-parse refs/heads/%s: %v: %s", branch, err, strings.TrimSpace(string(localOut)))
	}
	push.LocalSHA = strings.TrimSpace(string(localOut))
	if originEvidenceForDir(dir) != originBound {
		// No-remote mode: the local tip IS the evidence. Mark the remote side
		// proven so callers that surface RemoteSHA see the verified pair; the
		// RemoteHasBranch flag stays honest (there is no remote branch).
		push.RemoteSHA = push.LocalSHA
		return commitBeyondBase, push, nil
	}
	lsRemote := exec.Command("git", "-C", dir, "ls-remote", "--heads", "origin", branch)
	// Round 6 (D-D): the API-side gate runs on the console host, where the
	// credentials-not-on-disk invariant means no git credential helper is
	// configured — a bare inheriting process env can never read a PRIVATE
	// remote, so the push requirement failed closed on every private project.
	// Callers may now pass a TRANSIENT credential env (the API injects the
	// project connection token for the duration of this read-only command,
	// exactly like the workspace clone path does). nil keeps the previous
	// behaviour (inherit the process env) for the sandbox runner, whose run
	// environment already carries GIT_CONFIG_GLOBAL.
	if env != nil {
		lsRemote.Env = env
	}
	ls, err := lsRemote.CombinedOutput()
	if err != nil {
		return commitBeyondBase, PushEvidence{}, fmt.Errorf("git ls-remote: %v: %s", err, strings.TrimSpace(string(ls)))
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
