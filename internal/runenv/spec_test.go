package runenv

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/agentcli"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runtimecli"
	"github.com/multigent/multigent/internal/sandbox"
)

func TestDockerProviderWrapsManagedAgentCLI(t *testing.T) {
	dir := t.TempDir()
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Image:    "example/runtime-base:test",
		Docker:   &entity.DockerSandboxConfig{Image: "example/runtime-base:test"},
	}
	cli := &entity.AgentCLIConfig{
		Vendor:         "codex",
		Version:        "1.2.3",
		Binary:         "codex",
		PackageManager: "npm",
		Package:        "@openai/codex",
	}

	_, args, err := DockerProvider{}.Command(ProcessSpec{
		AgentDir: dir,
		Model:    entity.ModelCodex,
		Runtime:  runtime,
		AgentCLI: cli,
		Command:  []string{"codex", "exec", "-"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"multigent-toolchains:" + agentcli.ToolchainHome,
		"PATH=" + runtimecli.ManagedBinDir,
		runtimecli.BinDir,
		agentcli.ToolchainBin,
		"example/runtime-base:test",
		"npm install -g --no-audit --no-fund --loglevel=notice @openai/codex@1.2.3",
		"MULTIGENT_AGENT_CLI_INSTALL_TIMEOUT",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args missing %q:\n%s", want, joined)
		}
	}
}

func TestDockerProviderMountsWorkspaceRuntimeToolCache(t *testing.T) {
	workspace := t.TempDir()
	agentDir := filepath.Join(workspace, "projects", "demo", "agents", "builder")
	cacheBin := filepath.Join(workspace, ".multigent", "tool-cache", "npm", "bin")
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Image:    sandbox.BaseImage,
		Docker:   &entity.DockerSandboxConfig{Image: sandbox.BaseImage},
		Env: []entity.RuntimeEnvVar{
			{Name: "MULTIGENT_TOOL_CACHE_BIN_DIR", Value: cacheBin},
		},
	}

	_, args, err := DockerProvider{}.Command(ProcessSpec{
		WorkspaceRoot: workspace,
		AgentDir:      agentDir,
		Model:         entity.ModelCodex,
		Runtime:       runtime,
		Command:       []string{"codex", "exec", "-"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	joined := strings.Join(args, "\n")
	cacheRoot := filepath.Join(workspace, ".multigent", "tool-cache")
	if !strings.Contains(joined, cacheRoot+":"+cacheRoot) {
		t.Fatalf("docker args missing runtime tool cache mount %q:\n%s", cacheRoot+":"+cacheRoot, joined)
	}
}

func TestDockerProviderPrependsRuntimeToolBin(t *testing.T) {
	dir := t.TempDir()
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Image:    sandbox.BaseImage,
		Docker:   &entity.DockerSandboxConfig{Image: sandbox.BaseImage},
		Env: []entity.RuntimeEnvVar{
			{Name: "MULTIGENT_TOOL_BIN_DIR", Value: filepath.Join(dir, ".multigent", "runtime-tools", "run", "bin")},
			{Name: "MULTIGENT_TOOL_CACHE_BIN_DIR", Value: "/workspace/.multigent/tool-cache/npm/bin"},
		},
	}

	_, args, err := DockerProvider{}.Command(ProcessSpec{
		AgentDir: dir,
		Model:    entity.ModelCodex,
		Runtime:  runtime,
		Command:  []string{"codex", "exec", "-"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	joined := strings.Join(args, "\n")
	want := "PATH=/workspace/.multigent/runtime-tools/run/bin:/workspace/.multigent/tool-cache/npm/bin:" + runtimecli.ManagedBinDir + ":" + runtimecli.BinDir
	if !strings.Contains(joined, want) {
		t.Fatalf("docker args missing tool bin path %q:\n%s", want, joined)
	}
}

func TestDockerProviderRunsRuntimeToolBootstrap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(runtimecli.ServerVersionEnv, "dev")
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Image:    sandbox.BaseImage,
		Docker:   &entity.DockerSandboxConfig{Image: sandbox.BaseImage},
		Env: []entity.RuntimeEnvVar{
			{Name: "MULTIGENT_TOOL_BOOTSTRAP_FILE", Value: filepath.Join(dir, ".multigent", "runtime-tools", "run", "bootstrap-tools.sh")},
		},
	}

	_, args, err := DockerProvider{}.Command(ProcessSpec{
		AgentDir: dir,
		Model:    entity.ModelCodex,
		Runtime:  runtime,
		Command:  []string{"codex", "exec", "-"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "/workspace/.multigent/runtime-tools/run/bootstrap-tools.sh") {
		t.Fatalf("docker args missing bootstrap script:\n%s", joined)
	}
	if !strings.Contains(joined, "exec \"$@\"") {
		t.Fatalf("docker args missing command handoff:\n%s", joined)
	}
	bootstrapIndex := strings.Index(joined, "/workspace/.multigent/runtime-tools/run/bootstrap-tools.sh")
	checkIndex := strings.Index(joined, "command -v 'mga'")
	if bootstrapIndex < 0 || checkIndex < 0 || bootstrapIndex > checkIndex {
		t.Fatalf("per-run bootstrap must run before the generic mga check:\n%s", joined)
	}
}

func TestDockerProviderDoesNotExposeInheritedSecretsInArgs(t *testing.T) {
	dir := t.TempDir()
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Image:    sandbox.BaseImage,
		Docker:   &entity.DockerSandboxConfig{Image: sandbox.BaseImage},
		Env: []entity.RuntimeEnvVar{
			{Name: "ANTHROPIC_API_KEY", Inherit: true},
		},
	}

	_, args, err := DockerProvider{}.Command(ProcessSpec{
		AgentDir: dir,
		Model:    entity.ModelClaudeCode,
		Runtime:  runtime,
		Command:  []string{"claude", "-p"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	joined := strings.Join(args, "\n")
	if strings.Contains(joined, "ANTHROPIC_API_KEY=") {
		t.Fatalf("docker args leaked secret value:\n%s", joined)
	}
	if !containsEnvArg(args, "ANTHROPIC_API_KEY") {
		t.Fatalf("docker args should inherit ANTHROPIC_API_KEY without value:\n%s", joined)
	}
}

func containsEnvArg(args []string, key string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" && args[i+1] == key {
			return true
		}
	}
	return false
}

// The shared gradle cache volume must ride along on every non-isolated agent
// container (host-user side) with GRADLE_USER_HOME pointed at it, and must be
// absent from isolated preview sandboxes (root-owned previews would fight the
// host user over the volume). Regression lock for the Batch 4.3 cache mount.
func TestDockerProviderMountsGradleCacheForAgentsOnly(t *testing.T) {
	workspace := t.TempDir()
	agentDir := filepath.Join(workspace, "projects", "demo", "agents", "builder")
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Image:    sandbox.BaseImage,
		Docker:   &entity.DockerSandboxConfig{Image: sandbox.BaseImage},
	}

	_, args, err := DockerProvider{}.Command(ProcessSpec{
		WorkspaceRoot: workspace,
		AgentDir:      agentDir,
		Model:         entity.ModelCodex,
		Runtime:       runtime,
		Command:       []string{"codex", "exec", "-"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	joined := strings.Join(args, "\n")
	wantMount := "multigent-gradle-cache:" + sandbox.HostUserCacheHome + "/gradle"
	if !strings.Contains(joined, wantMount) {
		t.Fatalf("agent container missing gradle cache mount %q:\n%s", wantMount, joined)
	}
	if !strings.Contains(joined, "GRADLE_USER_HOME="+sandbox.HostUserCacheHome+"/gradle") {
		t.Fatalf("agent container missing GRADLE_USER_HOME env:\n%s", joined)
	}
}
