package runtimeexec

import "github.com/multigent/multigent/internal/entity"

const KindExecPrompt = "exec_prompt"
const KindTask = "task"
const KindForkSession = "fork_session"

// SpecAsset describes one project asset a task run must have staged and
// mounted read-only before the agent starts. The control plane resolves the
// task's attachments at spec-build time (its DB is authoritative); the node
// downloads the bytes by SHA and verifies them — the prompt's manifest and
// this list must never disagree, so both are derived from the same rows.
type SpecAsset struct {
	Sha256      string `json:"sha256"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// RuntimeTaskAssetsDirEnv carries, in the runtime control env, the host path
// of the staged per-run assets cache dir. The runner turns it into a
// read-only container mount; the prompt manifest (already inside spec.Prompt)
// references that same mount target.
const RuntimeTaskAssetsDirEnv = "MULTIGENT_TASK_ASSETS_DIR"

type Spec struct {
	Kind              string            `json:"kind"`
	WorkspaceID       string            `json:"workspaceId"`
	ProjectID         string            `json:"projectId"`
	AgentID           string            `json:"agentId"`
	TaskID            string            `json:"taskId,omitempty"`
	ForkSessionID     string            `json:"forkSessionId,omitempty"`
	SessionID         string            `json:"sessionId,omitempty"`
	Prompt            string            `json:"prompt"`
	Agent             entity.AgentMeta  `json:"agent"`
	ProviderEnv       map[string]string `json:"providerEnv,omitempty"`
	RuntimeControlEnv map[string]string `json:"runtimeControlEnv"`
	Assets            []SpecAsset       `json:"assets,omitempty"`
}
