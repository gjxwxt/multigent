package runner

import (
	"encoding/json"
	"fmt"
	"io"
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
// git evidence beyond transcript matching). Returned string explains the
// first unmet requirement; empty means all requirements are met.
func validateDeliveryEvidence(c deliveryContract, transcript, workspaceDir, baseBranch, branchName string) string {
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
		if strings.TrimSpace(workspaceDir) != "" {
			commitOK, pushOK, err := gitDeliveryEvidence(workspaceDir, baseBranch, branchName)
			if err != nil {
				return fail("git delivery evidence unreadable", err.Error())
			}
			if c.RequireGitCommit && !commitOK {
				return fail("no commit beyond base branch", "commit the delivery on the declared branch")
			}
			if c.RequirePush && !pushOK {
				if strings.TrimSpace(branchName) == "" {
					return fail("push required but task has no BranchName", "set BranchName on the task")
				}
				return fail("branch not found on the remote", "push the declared branch before completing")
			}
		} else {
			return fail("delivery contract requires a git workspace, run has none", "dispatch this task on a runtime node with a workspace mount")
		}
	}
	return ""
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

// applyDeliveryContractGate is the single done_success choke point for task
// runs (review P1-iii, 2026-09-24): CLI-agent and HTTP-agent paths both call
// it right after setting TaskStatusDoneSuccess. When the task carries no
// contract var it is a no-op; otherwise an unmet requirement flips the result
// to done_failed with the violation as ErrorMsg and mirrors it into the
// redacted run log for the user-visible record.
func (r *Runner) applyDeliveryContractGate(result *RunResult, task *entity.Task, transcript, execAgentDir string, redactedLog io.Writer) {
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
	violation := validateDeliveryEvidence(contract, transcript, execAgentDir, task.BaseBranch, task.BranchName)
	if violation == "" {
		return
	}
	result.Status = entity.TaskStatusDoneFailed
	result.ErrorMsg = violation
	if redactedLog != nil {
		fmt.Fprintf(redactedLog, "\n=== delivery contract violated ===\n%s\n", violation)
	}
}
