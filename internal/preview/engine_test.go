package preview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/sandbox"
)

func TestDetectProjectType(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "preview-detect-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Case 1: Empty dir -> CLI
	if pt := DetectProjectType(tempDir); pt != ProjectTypeCLI {
		t.Fatalf("expected CLI for empty dir, got %v", pt)
	}

	// Case 2: Frontend only (package.json)
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"name":"frontend"}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if pt := DetectProjectType(tempDir); pt != ProjectTypeFrontend {
		t.Fatalf("expected Frontend for package.json, got %v", pt)
	}

	// Case 3: Backend only (go.mod)
	os.Remove(filepath.Join(tempDir, "package.json"))
	if err := os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte(`module myapp`), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if pt := DetectProjectType(tempDir); pt != ProjectTypeBackend {
		t.Fatalf("expected Backend for go.mod, got %v", pt)
	}

	// Case 4: Fullstack (both frontend package.json and root go.mod)
	if err := os.MkdirAll(filepath.Join(tempDir, "frontend"), 0755); err != nil {
		t.Fatalf("mkdir frontend: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "frontend", "package.json"), []byte(`{"name":"web"}`), 0644); err != nil {
		t.Fatalf("write frontend package.json: %v", err)
	}
	if pt := DetectProjectType(tempDir); pt != ProjectTypeFullstack {
		t.Fatalf("expected Fullstack for frontend+backend, got %v", pt)
	}
}

func TestBackendCommandUsesNestedGoModuleDirectory(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "preview-command-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	if err := os.MkdirAll(filepath.Join(tempDir, "server"), 0755); err != nil {
		t.Fatalf("mkdir server: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "server", "go.mod"), []byte("module example.com/server\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	command := backendCommandFor(tempDir)
	if !strings.Contains(command, "cd server &&") {
		t.Fatalf("command = %q, want nested server directory", command)
	}
	if strings.Contains(command, "./server/...") {
		t.Fatalf("command still uses invalid root-module pattern: %q", command)
	}
}

func TestRuntimeContractBuildsDeterministicFullstackCommand(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tempDir, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	contract := `{"version":1,"frontend":{"directory":"web","command":"npm run dev -- --host 0.0.0.0 --port ${PORT}"},"backend":{"directory":"server","command":"go run .","port":8080,"healthPath":"/health"},"preview":{"healthPath":"/ready","startupTimeoutSeconds":45}}`
	if err := os.WriteFile(filepath.Join(tempDir, ".multigent", "runtime.json"), []byte(contract), 0644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadRuntimeSpec(tempDir)
	if err != nil {
		t.Fatalf("LoadRuntimeSpec: %v", err)
	}
	command, healthPath, timeout, err := spec.StartupCommand(ProjectTypeFullstack, 43123)
	if err != nil {
		t.Fatalf("StartupCommand: %v", err)
	}
	if !strings.Contains(command, "cd 'server'") || !strings.Contains(command, "PORT=8080 go run .") {
		t.Fatalf("unexpected backend command: %q", command)
	}
	if !strings.Contains(command, "cd 'web'") || !strings.Contains(command, "43123") {
		t.Fatalf("unexpected frontend command: %q", command)
	}
	if healthPath != "/ready" || timeout != 45 {
		t.Fatalf("unexpected readiness settings: path=%q timeout=%d", healthPath, timeout)
	}
}

func TestRuntimeContractRejectsPathEscape(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tempDir, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	contract := `{"version":1,"backend":{"directory":"../outside","command":"go run ."}}`
	if err := os.WriteFile(filepath.Join(tempDir, ".multigent", "runtime.json"), []byte(contract), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeSpec(tempDir); err == nil {
		t.Fatal("expected path escape to be rejected")
	}
}

func TestPreviewWorktreeMountReadOnly(t *testing.T) {
	if got := previewWorktreeMount("/tmp/worktree", true); got != "/tmp/worktree:/workspace:ro" {
		t.Fatalf("read-only mount = %q", got)
	}
	if got := previewWorktreeMount("/tmp/worktree", false); got != "/tmp/worktree:/workspace" {
		t.Fatalf("writable mount = %q", got)
	}
}

func TestCheckHTTPReadyRequiresSuccessfulStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	var port int
	if _, err := fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port); err != nil {
		// httptest may use an equivalent loopback spelling; keep this test
		// portable by extracting the final port component.
		parts := strings.Split(server.Listener.Addr().String(), ":")
		port, err = strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			t.Fatalf("parse test server port: %v", err)
		}
	}
	if CheckHTTPReadyAt(port, "/", 250*time.Millisecond) {
		t.Fatal("5xx response must not be considered ready")
	}
}

func TestFrontendInstallCommandOnlyForColdWorktrees(t *testing.T) {
	root := t.TempDir()
	spec := &RuntimeServiceSpec{Directory: "web", Command: "npm run dev"}
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Cold worktree (gitignored node_modules absent): the dev server would
	// die instantly, so an install prefix is mandatory.
	cmd := frontendInstallCommand(root, spec)
	if !strings.Contains(cmd, "npm install") || !strings.Contains(cmd, "cd 'web'") {
		t.Fatalf("cold worktree must install frontend deps, got %q", cmd)
	}
	if err := os.MkdirAll(filepath.Join(root, "web", "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if cmd := frontendInstallCommand(root, spec); cmd != "" {
		t.Fatalf("warm worktree must not reinstall, got %q", cmd)
	}
	if cmd := frontendInstallCommand(root, nil); cmd != "" {
		t.Fatalf("nil frontend must produce no install, got %q", cmd)
	}
	if cmd := frontendInstallCommand(root, &RuntimeServiceSpec{Directory: "apps", Command: "npm run dev"}); cmd != "" {
		t.Fatalf("missing package.json must produce no install, got %q", cmd)
	}
}

func TestRuntimeContractBuildsGradleBootRunOptimization(t *testing.T) {
	spec := &RuntimeServiceSpec{
		Directory: "server",
		Command:   "./gradlew bootRun",
		Port:      8080,
	}
	cmd := renderServiceCommand(spec, 8080, true)
	if !strings.Contains(cmd, "build/libs/*.jar") || !strings.Contains(cmd, "java -jar") {
		t.Fatalf("expected bootRun command to include prebuilt jar fast-path, got: %q", cmd)
	}
}

func TestPreviewImageResolution(t *testing.T) {
	e := &Engine{}
	if got := e.previewImage(RuntimeSelection{}); got != sandbox.DefaultBaseImage() {
		t.Errorf("empty selection should resolve default base image, got %q", got)
	}
	if got := e.previewImage(RuntimeSelection{ImageRef: "harbor.corp/multigent/runtime-jvm21:2026.10.1"}); got != "harbor.corp/multigent/runtime-jvm21:2026.10.1" {
		t.Errorf("per-project image should win, got %q", got)
	}
}

func TestProfilePreviewEnv(t *testing.T) {
	base := &Engine{}
	baseEnv := base.profilePreviewEnv(RuntimeSelection{})
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, "JAVA_HOME=") || strings.HasPrefix(kv, "PATH=") {
			t.Errorf("base profile must not set JAVA_HOME/PATH overrides: %v", baseEnv)
		}
	}

	jvm := &Engine{}
	env := jvm.profilePreviewEnv(RuntimeSelection{Profile: sandbox.ProfileJVM21, ImageRef: "ghcr.io/multigent/multigent/runtime-jvm21:latest"})
	var hasJavaHome, hasPath bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "JAVA_HOME=/opt/multigent/jdk") {
			hasJavaHome = true
		}
		if strings.HasPrefix(kv, "PATH=") && strings.Contains(kv, "/opt/multigent/jdk/bin") {
			hasPath = true
		}
	}
	if !hasJavaHome || !hasPath {
		t.Errorf("jvm21 profile should set JAVA_HOME and jdk PATH, got %v", env)
	}

	// An image that LOOKS like jvm21 but carries no declared profile must NOT
	// trigger the JVM env: capability comes from the profile, never the name.
	renamed := (&Engine{}).profilePreviewEnv(RuntimeSelection{Profile: sandbox.ProfileBase, ImageRef: "harbor.corp/enterprise/jdk-stack:2026.9.1"})
	for _, kv := range renamed {
		if strings.HasPrefix(kv, "JAVA_HOME=") {
			t.Errorf("renamed jvm image without declared profile must not get JAVA_HOME, got %v", renamed)
		}
	}
}
