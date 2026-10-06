package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// deliveryContractVar is the task var key carrying the caller's completion
// contract (JSON, sanitized into task.Vars at creation time). The contract is
// deterministic evidence gating ONLY: when present, an exit-0 agent run is
// not sufficient to report done_success — the required evidence must be
// observable on the run transcript/output. It never runs model calls, never
// validates content quality, and never gates plain command tasks (no
// contract var = no gating).
const deliveryContractVar = "MULTIGENT_DELIVERY_CONTRACT"

// DeliveryContractVar exposes the task var key to the API side, which seeds
// the contract into fan-out branch tasks and enforces the git dimension at
// the control-plane completion gate.
const DeliveryContractVar = deliveryContractVar

type deliveryContract struct {
	// RequireModelActivity: at least one model response must appear in the
	// transcript. Guard against the zero-output exit-0 false success
	// (2026-09-24: unbound-node worker fell back to local execution without
	// model credentials and the task was marked done_success).
	// Trust boundary (review P1-i): the transcript is written by the local
	// agent CLI, so a compromised agent could fabricate an assistant line.
	// This gate is deterministic anti-omission defense, not adversarial
	// proof — final acceptance still cross-checks the remote SHA and diff
	// independently of run status.
	RequireModelActivity bool `json:"requireModelActivity"`
	// RequireGitCommit: a commit must be observable on the current branch
	// beyond the task's BaseBranch (or main when unset). When RequirePush is
	// also set, the commit must additionally exist on the remote tracking
	// branch (verified via git ls-remote against BranchName).
	RequireGitCommit bool `json:"requireGitCommit"`
	// RequirePush: remote must contain BranchName (fails closed when
	// BranchName is empty).
	RequirePush bool `json:"requirePush"`
	// RequireTestsRun: "Tests run: N" must appear with N > 0.
	RequireTestsRun bool `json:"requireTestsRun"`
}

var testsRunRe = regexp.MustCompile(`Tests run:\s*(\d+)`)

// validateDeliveryEvidence checks the contract against the captured run
// transcript. workspaceDir is the run working directory (empty = skip the
// git evidence beyond transcript matching). baseCommit is the task's FROZEN
// baseline SHA (takes precedence over baseBranch when set — review round 3
// item 1: the increment must be measured against the exact commit the task
// was created from, not a branch ref that can move); baseBranch is the
// fallback. gitEnv is the run's materialized git environment for the
// ls-remote evidence read (see applyDeliveryContractGate); nil inherits the
// process env (legacy behaviour). Returned string explains the first unmet
// requirement; empty means all requirements are met.
func validateDeliveryEvidence(c deliveryContract, transcript, workspaceDir, baseCommit, baseBranch, branchName string, gitEnv []string) string {
	fail := func(what, hint string) string {
		return fmt.Sprintf("delivery contract unmet: %s (%s)", what, hint)
	}
	if c.RequireModelActivity {
		if !transcriptHasModelActivity(transcript) {
			return fail("no model activity on the run transcript",
				"the agent produced zero model output; if the task is a plain command instead, remove the delivery contract from the task")
		}
	}
	if c.RequireTestsRun {
		if !transcriptHasTestsRun(transcript) {
			return fail("no \"Tests run: N\" evidence in the run output",
				"run the test suite so its summary is visible to the platform")
		}
	}
	if c.RequireGitCommit || c.RequirePush {
		if violation, _ := ValidateGitDeliveryEvidence(c, workspaceDir, baseCommit, baseBranch, branchName, gitEnv); violation != "" {
			return violation
		}
	}
	return ""
}

// ValidateGitDeliveryEvidence is the git-dimension half of the delivery
// contract, exported for the API-side completion gate (review round 3, item
// 3): workflow branch tasks complete via the control plane
// (completeRuntimeWorkflowBranch), not the runner, so the fan-out delivery
// contract must be enforceable there too — same rules, same messages.
// The proven push evidence is returned alongside the violation so the
// caller can hand the machine-verified candidate SHA to downstream
// acceptance steps (delivery SHA hand-off): a non-empty violation means the
// evidence was NOT proven and must be discarded.
//
// No-remote mode (B1, 2026-09-29): a project with NO origin remote — or one
// pointing inside .multigent/ (a bare repo the agent authored to satisfy the
// old gate) — cannot prove push evidence by construction, so the gate
// accepts LOCAL evidence instead: a commit beyond the frozen base plus the
// committed (push.LocalSHA == verified local branch tip) delivery. The
// branchName requirement stays: without a declared branch there is no
// delivery ref to verify at all. Projects WITH a real remote keep the exact
// SHA-accurate remote==local comparison.
func ValidateGitDeliveryEvidence(c deliveryContract, workspaceDir, baseCommit, baseBranch, branchName string, env []string) (string, PushEvidence) {
	fail := func(what, hint string) string {
		return fmt.Sprintf("delivery contract unmet: %s (%s)", what, hint)
	}
	if !c.RequireGitCommit && !c.RequirePush {
		return "", PushEvidence{}
	}
	wsDir := strings.TrimSpace(workspaceDir)
	if wsDir == "" {
		return fail("delivery contract requires a git workspace, run has none", "dispatch this task on a runtime node with a workspace mount"), PushEvidence{}
	}
	// Review round 4 (P0-3): the resolver chain can fall back to paths that
	// are NOT the task's delivery tree (agent dir, project workspace, project
	// root). Validating git evidence there would measure the WRONG repository
	// and could pass by accident. Require the workspace to actually be a git
	// work tree; anything else fails closed.
	if _, err := exec.Command("git", "-C", wsDir, "rev-parse", "--is-inside-work-tree").Output(); err != nil {
		return fail("delivery contract requires a git workspace", fmt.Sprintf("resolved run directory %s is not a git work tree; refusing to measure evidence in the wrong tree", wsDir)), PushEvidence{}
	}
	baseRef := strings.TrimSpace(baseCommit)
	if baseRef == "" {
		baseRef = baseBranch
	}
	commitOK, push, err := gitDeliveryEvidence(workspaceDir, baseRef, branchName, env)
	if err != nil {
		return fail("git delivery evidence unreadable", err.Error()), PushEvidence{}
	}
	if c.RequireGitCommit && !commitOK {
		return fail("no commit beyond the frozen base "+baseRef, "commit the delivery on the declared branch"), PushEvidence{}
	}
	if c.RequirePush {
		// No-remote mode is signalled by gitDeliveryEvidence leaving
		// RemoteHasBranch false while still returning a verified local tip
		// (RemoteSHA == LocalSHA, no ls-remote error). A bound remote that
		// simply lacks the branch also produces RemoteHasBranch == false, so
		// distinguish by the origin classification, not by the flag.
		noRemote := originEvidenceForDir(workspaceDir) != originBound
		if !noRemote {
			if strings.TrimSpace(branchName) == "" {
				return fail("push required but task has no BranchName", "set BranchName on the task"), PushEvidence{}
			}
			// SHA-accurate push evidence (review round 3, item 2): a remote
			// branch EXISTING is not proof — it may sit at an older commit while
			// the local delivery commit is unpushed.
			if !push.RemoteHasBranch {
				return fail("branch not found on the remote", "push the declared branch before completing"), PushEvidence{}
			}
			if push.RemoteSHA != push.LocalSHA {
				return fail(fmt.Sprintf("remote branch is at %s but the local delivery commit is %s (unpushed)", shortSHA(push.RemoteSHA), shortSHA(push.LocalSHA)),
					"push the latest commit so the remote tip matches the delivery"), PushEvidence{}
			}
		} else if strings.TrimSpace(branchName) == "" || push.LocalSHA == "" {
			// No-remote projects still need a declared branch carrying the
			// delivery — without one nothing verifiable exists at all.
			return fail("no remote configured and no delivery branch to verify", "commit the delivery on the declared branch (no push required: this project has no remote)"), PushEvidence{}
		}
	}
	return "", push
}

// DeliveryEvidenceFieldSHA / DeliveryEvidenceFieldBranch are the
// machine-generated output keys carrying the proven candidate SHA to
// downstream acceptance steps (delivery SHA hand-off). The completion gate
// writes them AFTER the push evidence has been verified; they always
// overwrite agent-provided values so the QA anchor can never be authored by
// the agent's free text.
const (
	DeliveryEvidenceFieldSHA    = "delivery_sha"
	DeliveryEvidenceFieldBranch = "delivery_branch"
)

// MergeDeliveryEvidence enforces the machine-anchor contract on the
// completing step's outputs: the ONLY value that may survive under
// delivery_sha/delivery_branch is the gate-proven pair (full 40-hex,
// remote == local). When the evidence is not proven, any agent-provided
// values for those keys are DELETED, not preserved — "key absent" must
// reliably mean "evidence not proven" downstream, never "the agent's free
// text slipped through as the anchor" (run6 shipped a 7-hex summary as
// the QA anchor; that failure mode is closed here).
func MergeDeliveryEvidence(outputs map[string]string, branchName string, push PushEvidence) {
	if outputs == nil {
		return
	}
	sha := strings.TrimSpace(push.RemoteSHA)
	local := strings.TrimSpace(push.LocalSHA)
	if sha == "" || sha != local || len(sha) != 40 || !isHex(sha) {
		delete(outputs, DeliveryEvidenceFieldSHA)
		delete(outputs, DeliveryEvidenceFieldBranch)
		return
	}
	outputs[DeliveryEvidenceFieldSHA] = sha
	if bn := strings.TrimSpace(branchName); bn != "" {
		outputs[DeliveryEvidenceFieldBranch] = bn
	} else {
		delete(outputs, DeliveryEvidenceFieldBranch)
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func transcriptHasModelActivity(transcript string) bool {
	for _, line := range strings.Split(transcript, "\n") {
		var evt struct {
			Type    string `json:"type"`
			Message *struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt.Type == "assistant" || (evt.Message != nil && evt.Message.Role == "assistant") {
			return true
		}
	}
	return false
}

func transcriptHasTestsRun(transcript string) bool {
	m := testsRunRe.FindStringSubmatch(transcript)
	if m == nil {
		return false
	}
	var n int
	_, err := fmt.Sscanf(m[1], "%d", &n)
	return err == nil && n > 0
}

// parseDeliveryContract decodes and normalizes the contract var. Unknown
// fields are ignored so the schema can grow without breaking older runners.
func parseDeliveryContract(raw string) (deliveryContract, error) {
	var c deliveryContract
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return c, fmt.Errorf("empty delivery contract")
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, fmt.Errorf("decode delivery contract: %w", err)
	}
	return c, nil
}

// ParseDeliveryContract is the exported alias for the API-side completion
// gate, which reads the same task var from the control plane.
func ParseDeliveryContract(raw string) (deliveryContract, error) {
	return parseDeliveryContract(raw)
}

// applyDeliveryContractGate is the single done_success choke point for task
// runs (review P1-iii, 2026-09-24): CLI-agent and HTTP-agent paths both call
// it right after setting TaskStatusDoneSuccess. When the task carries no
// contract var it is a no-op; otherwise an unmet requirement flips the result
// to done_failed with the violation as ErrorMsg and mirrors it into the
// redacted run log for the user-visible record.
//
// gitEnv carries the run's materialized git environment (GIT_CONFIG_GLOBAL
// pointing at the connection's credential-helper config). The gate's
// ls-remote runs in the runner process AFTER the agent exited, and the runner
// process env does not carry that variable — without it the push-evidence
// check hits a private remote with no credentials and fails closed even when
// the agent genuinely pushed (wp-7, 2026-10-06). The helper files still
// exist at this point: the runtime-files cleanup is a deferred call that has
// not run yet.
func (r *Runner) applyDeliveryContractGate(result *RunResult, task *entity.Task, transcript, execAgentDir string, gitEnv []string, redactedLog io.Writer) {
	if result == nil || task == nil || len(task.Vars) == 0 {
		return
	}
	raw := strings.TrimSpace(task.Vars[deliveryContractVar])
	if raw == "" {
		return
	}
	contract, err := parseDeliveryContract(raw)
	if err != nil {
		result.Status = entity.TaskStatusDoneFailed
		result.ErrorMsg = fmt.Sprintf("delivery contract invalid: %v", err)
		if redactedLog != nil {
			fmt.Fprintf(redactedLog, "\n=== delivery contract invalid ===\n%s\n", result.ErrorMsg)
		}
		return
	}
	// Credential resolution order (defect ⑯, final form): the in-runner
	// connections resolver (origin-host matched, transient header) first —
	// the run's materialized git env can be present yet credential-less
	// (a [user]-only config, or a helper whose host list does not cover the
	// origin URL spelling). The API-injected EvidenceEnv callback is the
	// second opinion when wired, and the materialized env stays as the last
	// fallback for deployments without either. All candidates pass through
	// hostGateGitEnv, which upgrades a container-scoped helper path to its
	// host-side file when present.
	if r != nil && task != nil {
		if env := r.evidenceEnvForTask(task); len(env) > 0 {
			gitEnv = env
		} else if r.EvidenceEnv != nil {
			if env := r.EvidenceEnv(task.WorktreeDir); len(env) > 0 {
				gitEnv = env
			}
		}
	}
	gitEnv = hostGateGitEnv(gitEnv)
	violation := validateDeliveryEvidence(contract, transcript, execAgentDir, task.BaseCommit, task.BaseBranch, task.BranchName, gitEnv)
	if violation == "" {
		return
	}
	result.Status = entity.TaskStatusDoneFailed
	result.ErrorMsg = violation
	if redactedLog != nil {
		fmt.Fprintf(redactedLog, "\n=== delivery contract violated ===\n%s\n", violation)
	}
}

// hostGateGitEnv rewrites the container-scoped credential helper path inside
// the run's materialized git config into its HOST-side location, so the
// gate's ls-remote can actually execute the helper from the runner process.
//
// Defect ⑯ (fu91es QA, 2026-10-06): materializeGitLabConfig writes the
// config's `helper = ` line as containerRuntimePath(helperPath, agentDir) —
// a path that resolves inside the sandbox (/workspace/...) but not on the
// host. The wp-7 round fixed the ENV EXTRACTION (GIT_CONFIG_GLOBAL now
// reaches the gate), but the config CONTENT was still container-scoped: the
// runner-side ls-remote could not execute the helper, git fell back to
// interactive auth and failed with "could not read Username ... No such
// device or address" even though the agent had genuinely pushed from inside
// the container (where the same absolute config resolves through the
// workspace mount). The helper script itself is written next to the config
// at <cfg>.credential-helper and still exists at gate time (the runtime-file
// cleanup is a deferred call that has not run yet), so the gate materializes
// a host-usable variant at <cfg>.hostgate and points GIT_CONFIG_GLOBAL at
// it. No new credential surface is created — the helper script and its token
// are the run's own already-materialized files; the variant carries only a
// path.
func hostGateGitEnv(gitEnv []string) []string {
	if len(gitEnv) == 0 {
		return gitEnv
	}
	const prefix = "GIT_CONFIG_GLOBAL="
	for i, entry := range gitEnv {
		if !strings.HasPrefix(entry, prefix) || len(entry) <= len(prefix) {
			continue
		}
		cfgPath := entry[len(prefix):]
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			// Config already cleaned up or unreadable: keep the previous
			// behavior (the gate fails closed downstream with its own
			// message) instead of inventing a different failure here.
			return gitEnv
		}
		hostHelper := cfgPath + ".credential-helper"
		if _, err := os.Stat(hostHelper); err != nil {
			return gitEnv
		}
		patched, changed := rewriteGitHelperPath(string(data), hostHelper)
		if !changed {
			return gitEnv // helper path already valid on the host
		}
		hostCfg := cfgPath + ".hostgate"
		if err := os.WriteFile(hostCfg, []byte(patched), 0o600); err != nil {
			return gitEnv
		}
		out := append([]string(nil), gitEnv...)
		out[i] = prefix + hostCfg
		return out
	}
	return gitEnv
}

// rewriteGitHelperPath points every `helper = <path>` line of a git config
// at hostHelper. changed is false when the config contains no helper line
// (nothing to rewrite — the caller passes the original through).
func rewriteGitHelperPath(config, hostHelper string) (string, bool) {
	lines := strings.Split(config, "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "helper") && strings.Contains(trimmed, "=") {
			lines[i] = "\thelper = " + hostHelper
			changed = true
		}
	}
	if !changed {
		return config, false
	}
	return strings.Join(lines, "\n"), true
}
