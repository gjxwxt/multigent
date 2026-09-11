package runner

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runenv"
	"github.com/multigent/multigent/internal/sandbox"
	"github.com/multigent/multigent/internal/store"
)

// testWorkspaceID mirrors store.workspaceID: kv_records rows are keyed by the
// sha1-derived workspace id of the workspace root, so the workspace row must
// use that id for SaveProject to satisfy its foreign key.
func testWorkspaceID(root string) string {
	absRoot, _ := filepath.Abs(root)
	sum := sha1.Sum([]byte(absRoot))
	return hex.EncodeToString(sum[:])[:12]
}

// TestSameWorkerDifferentProjectsResolveOwnRuntime is the cross-project
// pollution regression: one shared agent worker joined into a base project and
// a jvm21 project must resolve the base image for the base project and a
// jvm21-family image for the jvm21 project — from the same untouched worker
// sandbox config.
func TestSameWorkerDifferentProjectsResolveOwnRuntime(t *testing.T) {
	root := t.TempDir()
	db, err := controldb.Open(filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspaceID := testWorkspaceID(root)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: workspaceID, Name: "t", Slug: workspaceID, Root: root}); err != nil {
		t.Fatal(err)
	}
	// One worker, no per-worker profile/image — the project declares runtime.
	if err := db.UpsertAgentWorker(controldb.AgentWorker{ID: "aw-shared", WorkspaceID: workspaceID, Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []controldb.ProjectMembership{
		{ID: "pm-base", WorkspaceID: workspaceID, ProjectID: "base-proj", MemberType: "agent_worker", MemberID: "aw-shared", Role: "developer"},
		{ID: "pm-jvm", WorkspaceID: workspaceID, ProjectID: "jvm-proj", MemberType: "agent_worker", MemberID: "aw-shared", Role: "developer"},
	} {
		if err := db.UpsertProjectMembership(m); err != nil {
			t.Fatal(err)
		}
	}
	st := store.NewDB(root, db)
	if err := st.SaveProject("base-proj", &entity.Project{Name: "base-proj"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProject("jvm-proj", &entity.Project{Name: "jvm-proj", RuntimeProfile: sandbox.ProfileJVM21}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.EnvRuntimeProfile, "")
	t.Setenv(sandbox.EnvRuntimeImage, "")

	r := &Runner{root: root, agentStore: st}

	// Base project: no profile → managed base image.
	baseCfg := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{NetworkMode: "bridge"}}
	r.applyProjectRuntimeProfile("base-proj", baseCfg)
	baseImage, err := sandbox.EffectiveImage(entity.ModelCodex, baseCfg.Docker)
	if err != nil {
		t.Fatalf("base resolve: %v", err)
	}
	if strings.Contains(baseImage, "runtime-jvm21") {
		t.Fatalf("base project must resolve a base image, got %q", baseImage)
	}

	// JVM project: project profile → jvm21-family image.
	jvmCfg := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{NetworkMode: "bridge"}}
	r.applyProjectRuntimeProfile("jvm-proj", jvmCfg)
	if jvmCfg.Docker.Profile != sandbox.ProfileJVM21 {
		t.Fatalf("project profile should be injected, got %q", jvmCfg.Docker.Profile)
	}
	jvmImage, err := sandbox.EffectiveImage(entity.ModelCodex, jvmCfg.Docker)
	if err != nil {
		t.Fatalf("jvm resolve: %v", err)
	}
	if !strings.Contains(jvmImage, "runtime-jvm21") {
		t.Fatalf("jvm project must resolve a jvm21 image, got %q", jvmImage)
	}

	// The shared worker record never gained a profile (no pollution).
	worker, found, err := db.AgentWorkerByID(workspaceID, "aw-shared")
	if err != nil {
		t.Fatal(err)
	}
	if found && worker.RuntimeConfigJSON != "" && strings.Contains(worker.RuntimeConfigJSON, "profile") {
		t.Fatalf("shared worker config must not be polluted: %s", worker.RuntimeConfigJSON)
	}
}

// TestAgentPreferenceYieldsToProjectAuthority proves the delivery rule: in a
// project that declares jvm21, an agent's personal base preference is
// overridden so its sandbox runs the same runtime as the project's preview.
// Agent-level profiles only apply in projects that declare none.
func TestAgentPreferenceYieldsToProjectAuthority(t *testing.T) {
	root := t.TempDir()
	db, err := controldb.Open(filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspaceID := testWorkspaceID(root)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: workspaceID, Name: "t", Slug: workspaceID, Root: root}); err != nil {
		t.Fatal(err)
	}
	st := store.NewDB(root, db)
	if err := st.SaveProject("jvm-proj", &entity.Project{Name: "jvm-proj", RuntimeProfile: sandbox.ProfileJVM21}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{root: root, agentStore: st}

	cfg := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{Profile: sandbox.ProfileBase}}
	r.applyProjectRuntimeProfile("jvm-proj", cfg)
	if cfg.Docker.Profile != sandbox.ProfileJVM21 {
		t.Fatalf("project authority must override agent preference, got %q", cfg.Docker.Profile)
	}
	image, err := sandbox.EffectiveImage(entity.ModelCodex, cfg.Docker)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(image, "runtime-jvm21") {
		t.Fatalf("jvm project agent sandbox must run jvm21 image, got %q", image)
	}

	// An explicitly pinned agent image still outranks the project profile
	// (deliberate operator choice, not an accident of priority).
	pinned := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{Image: "harbor.corp/custom:v1", Profile: sandbox.ProfileBase}}
	r.applyProjectRuntimeProfile("jvm-proj", pinned)
	if pinned.Docker.Profile != sandbox.ProfileBase || pinned.Docker.Image != "harbor.corp/custom:v1" {
		t.Fatalf("explicit agent image must be left untouched, got %+v", pinned.Docker)
	}
}

// TestAgentSandboxAndPreviewRuntimeAgree is the Agent/Preview consistency
// contract: for the same project profile, the runner-side agent sandbox
// resolution and the API-side preview resolution must land on the same
// profile and image family — the exact gap the GPT review flagged.
func TestAgentSandboxAndPreviewRuntimeAgree(t *testing.T) {
	root := t.TempDir()
	db, err := controldb.Open(filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workspaceID := testWorkspaceID(root)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: workspaceID, Name: "t", Slug: workspaceID, Root: root}); err != nil {
		t.Fatal(err)
	}
	st := store.NewDB(root, db)
	if err := st.SaveProject("jvm-proj", &entity.Project{Name: "jvm-proj", RuntimeProfile: sandbox.ProfileJVM21}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.EnvRuntimeProfile, "")
	t.Setenv(sandbox.EnvRuntimeImage, "")

	// Agent side: runner injects project authority, then resolves.
	r := &Runner{root: root, agentStore: st}
	agentCfg := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{NetworkMode: "bridge"}}
	r.applyProjectRuntimeProfile("jvm-proj", agentCfg)
	agentImage, err := sandbox.EffectiveImage(entity.ModelCodex, agentCfg.Docker)
	if err != nil {
		t.Fatalf("agent resolve: %v", err)
	}

	// Preview side: same value flows through ResolveRuntime as the API does.
	previewSel, err := sandbox.ResolveRuntime(sandbox.RuntimeRequest{ProjectProfile: sandbox.ProfileJVM21})
	if err != nil {
		t.Fatalf("preview resolve: %v", err)
	}

	if agentImage != previewSel.ImageRef {
		t.Fatalf("agent sandbox and preview must agree on the image: agent=%q preview=%q", agentImage, previewSel.ImageRef)
	}

	// Same agreement holds for a base project whose agent carries a personal
	// jvm21 preference: both sides see the agent preference (the preview
	// resolver receives the task's executing agent, mirroring the API path).
	if err := st.SaveProject("base-proj", &entity.Project{Name: "base-proj"}); err != nil {
		t.Fatal(err)
	}
	baseCfg := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{Profile: sandbox.ProfileJVM21}}
	r.applyProjectRuntimeProfile("base-proj", baseCfg)
	baseImage, err := sandbox.EffectiveImage(entity.ModelCodex, baseCfg.Docker)
	if err != nil {
		t.Fatalf("base resolve: %v", err)
	}
	baseSel, err := sandbox.ResolveRuntime(sandbox.RuntimeRequest{AgentProfile: sandbox.ProfileJVM21})
	if err != nil {
		t.Fatalf("base preview resolve: %v", err)
	}
	if baseImage != baseSel.ImageRef {
		t.Fatalf("base project agent/preview must agree: agent=%q preview=%q", baseImage, baseSel.ImageRef)
	}

	// And in the same base project an agent WITHOUT a preference agrees with a
	// preview resolved without agent context (both default to base).
	plainCfg := &entity.SandboxConfig{Provider: entity.SandboxDocker, Docker: &entity.DockerSandboxConfig{}}
	r.applyProjectRuntimeProfile("base-proj", plainCfg)
	plainImage, err := sandbox.EffectiveImage(entity.ModelCodex, plainCfg.Docker)
	if err != nil {
		t.Fatalf("plain resolve: %v", err)
	}
	plainSel, err := sandbox.ResolveRuntime(sandbox.RuntimeRequest{})
	if err != nil {
		t.Fatalf("plain preview resolve: %v", err)
	}
	if plainImage != plainSel.ImageRef {
		t.Fatalf("preference-free agent/preview must agree: agent=%q preview=%q", plainImage, plainSel.ImageRef)
	}
}

// TestDockerConfigPreservesInjectedProfile guards the provider-neutral path:
// DockerConfig (SandboxConfig → DockerSandboxConfig) must not drop a profile
// injected before provider.Command runs.
func TestDockerConfigPreservesInjectedProfile(t *testing.T) {
	runtime := &entity.SandboxConfig{
		Provider: entity.SandboxDocker,
		Docker:   &entity.DockerSandboxConfig{Profile: sandbox.ProfileJVM21, NetworkMode: "bridge"},
	}
	cfg := runenv.DockerConfig(runtime)
	if cfg.Profile != sandbox.ProfileJVM21 {
		t.Fatalf("DockerConfig dropped the profile: %+v", cfg)
	}
}
