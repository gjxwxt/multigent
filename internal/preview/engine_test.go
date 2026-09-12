package preview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

	// Regression (docker "invalid reference format"): every env entry must be
	// preceded by its own "-e" flag. Docker treats the first bare KEY=VALUE
	// argument as the image reference, so a flagless env value silently shifts
	// the whole command tail and breaks every preview start.
	for name, args := range map[string][]string{"base": baseEnv, "jvm21": env, "renamed": renamed} {
		for i, arg := range args {
			if strings.Contains(arg, "=") && !strings.HasPrefix(arg, "-") {
				if i == 0 || args[i-1] != "-e" {
					t.Errorf("%s: env value %q at %d must be preceded by -e, got %v", name, arg, i, args)
				}
			}
		}
	}
}

// TestProfilePreviewEnvJTOFromSelection closes the pinned-image parity gap: a
// project that declares jvm21 while the executing agent pins an explicit image
// resolves to RuntimeSelection{Profile: jvm21, ImageRef: pinned} — the pinned
// image decides the container image, and the recorded profile must still drive
// JAVA_TOOL_OPTIONS so Gradle gets the platform proxy properties.
func TestProfilePreviewEnvJTOFromSelection(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:17890")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1")

	e := &Engine{}
	env := e.profilePreviewEnv(RuntimeSelection{
		Profile:  sandbox.ProfileJVM21,
		ImageRef: "harbor.example.com/platform/jdk-stack:21",
	})
	hasJTO := false
	for _, kv := range env {
		if strings.HasPrefix(kv, sandbox.EnvJVMToolOptions+"=") && strings.Contains(kv, "-Dhttps.proxyHost=proxy.internal") {
			hasJTO = true
		}
	}
	if !hasJTO {
		t.Fatalf("pinned-image selection with jvm21 profile must inject %s, got %v", sandbox.EnvJVMToolOptions, env)
	}
}

// Regression: "install && A & B" backgrounds {install && A} and starts B
// immediately, so a cold worktree raced the install and vite came up missing
// (exit 127). The install prefix must sit in a foreground brace group wrapping
// the whole backgrounded service chain.
func TestColdInstallPrefixRunsBeforeBackgroundedServices(t *testing.T) {
	runtimeSpec := &RuntimeSpec{
		Backend:  &RuntimeServiceSpec{Directory: "server", Command: "go run .", Port: 8080},
		Frontend: &RuntimeServiceSpec{Directory: "web", Command: "npm run dev -- --host 0.0.0.0 --port ${PORT}"},
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	command, _, _, err := runtimeSpec.StartupCommand(ProjectTypeFullstack, 5173)
	if err != nil {
		t.Fatal(err)
	}
	installCmd := frontendInstallCommand(root, runtimeSpec.Frontend)
	if installCmd == "" {
		t.Fatal("expected cold install prefix")
	}
	composed := "{ " + strings.TrimSuffix(strings.TrimSuffix(installCmd, " "), "&&") + " ; } && { " + command + " ; }"

	// The composed one-liner must be valid sh AND start the dev server only
	// after the install subshell completes. Exercise it: a slow fake install
	// that creates vite only at the end must not race the frontend service.
	dir := t.TempDir()
	fake := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "npm"), []byte("#!/bin/sh\nsleep 0.2\nmkdir -p node_modules/.bin\ntouch node_modules/.bin/vite\nchmod +x node_modules/.bin/vite\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+":"+os.Getenv("PATH"))
	script := strings.ReplaceAll(composed, "cd 'web'", "cd '"+dir+"'")
	script = strings.ReplaceAll(script, "cd 'server'", "cd '"+dir+"'")
	script = strings.ReplaceAll(script, "go run .", "echo backend-up")
	script = strings.ReplaceAll(script, "npm run dev", "test -x node_modules/.bin/vite && echo VITE_WAS_READY || exit 1")
	// The backend subshell exits after echoing, which makes the backgrounded
	// job's nonzero status irrelevant; only the install group's exit feeds
	// the "&&", so a failing install must still fail the whole script.
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("composed startup script failed: %v output=%s", err, out)
	}
	if !strings.Contains(string(out), "VITE_WAS_READY") {
		t.Fatalf("frontend service ran before install completed: %s", out)
	}
}
