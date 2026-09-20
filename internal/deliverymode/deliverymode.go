// Package deliverymode answers one question for two different consumers — the
// CI readiness gate in the API, and the task prompt built for an agent: given
// what a project declares and whether it has a *verified* remote binding, how
// far can this delivery actually get?
//
// It exists as its own package because internal/api imports internal/runner: the
// precedence rule needs exactly one owner, and runner must not import api.
package deliverymode

import (
	"os"
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// RemotePipelineRequiredEnv is the server-level DEFAULT for projects that do not
// declare RemotePipelineRequired themselves. Any of "1", "true", "yes",
// "required" (case-insensitive) turns the gate strict workspace-wide.
// It is a default, not a floor: a project may still declare "local" to opt out,
// because there is no organization-enforcement deployment today — the name
// deliberately keeps "REQUIRED" (not "DEFAULT") since the value it selects is
// the strict gate, and if an org-enforced floor is ever needed it should be a
// separate setting that ignores project-level "local".
const RemotePipelineRequiredEnv = "MULTIGENT_CI_REMOTE_PIPELINE_REQUIRED"

// RemotePipelineRequired resolves the effective gate semantics: explicit project
// declaration first, then the server env default, then local-only (the
// historical behavior). The declared- vs-bound-inference ambiguity flagged in
// review 2026-09-14 is resolved by making both sides explicit. Precedence
// (review round 3, C): project declaration > env default > fallback local. The
// env is a default only — "local" at project level always wins until an explicit
// org-floor setting exists.
func RemotePipelineRequired(p *entity.Project) bool {
	if p != nil {
		switch strings.ToLower(strings.TrimSpace(p.RemotePipelineRequired)) {
		case "required":
			return true
		case "local":
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RemotePipelineRequiredEnv))) {
	case "1", "true", "yes", "required":
		return true
	}
	return false
}

// Mode names how far a delivery can get. Bound and LocalBranch are both healthy
// outcomes; RequiredUnbound is the one an agent must escalate rather than retry.
type Mode string

const (
	// ModeBound: a verified remote binding exists, so merge-request creation,
	// pipeline evidence and the platform-side MR record are all available.
	ModeBound Mode = "remote_bound"
	// ModeLocalBranch: no verified binding and none required. Git push and the
	// task branch still work — only the three remote artifacts above are out of
	// reach. Naming exactly what is unavailable is the point: agents that infer
	// "no binding" means "cannot push" start skipping pushes, which is a worse
	// failure than the placeholder value this mode is meant to prevent.
	ModeLocalBranch Mode = "local_branch"
	// ModeRequiredUnbound: remote pipeline evidence is required but no verified
	// binding exists. Remote-dependent steps are held by the platform for a
	// human; retrying cannot clear it.
	ModeRequiredUnbound Mode = "required_unbound"
	// ModeUnknown is not a resolution outcome — Resolve never returns it. It is
	// what a consumer reports when it could not read the project record or the
	// binding table. Saying so is safer than defaulting to LocalBranch, which
	// would tell an agent to stop trying the very action that may be available.
	ModeUnknown Mode = "unknown"
)

// Resolve is the single source of the three-way split. hasVerifiedBinding must
// come from the verified binding table — the project record's RemoteProvider and
// RemoteConnection are client-writable and prove nothing (see the P0.6 note in
// internal/api/ci_ready_handlers.go).
func Resolve(p *entity.Project, hasVerifiedBinding bool) Mode {
	switch {
	case hasVerifiedBinding:
		return ModeBound
	case RemotePipelineRequired(p):
		return ModeRequiredUnbound
	default:
		return ModeLocalBranch
	}
}
