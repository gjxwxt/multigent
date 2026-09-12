package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStagingSpec() *RuntimeSpec {
	return &RuntimeSpec{
		Backend:  &RuntimeServiceSpec{Directory: "server", Command: "go run .", Port: 8080, HealthPath: "/api/health"},
		Frontend: &RuntimeServiceSpec{Directory: "web", Command: "npm run dev -- --host 0.0.0.0 --port ${PORT}"},
	}
}

// Regression (p15 snapshot preview EROFS): a completed task's snapshot mounts
// the worktree :ro, but vite's ESM config loader writes
// "vite.config.ts.timestamp-*.mjs" beside vite.config.ts before importing it,
// so every snapshot preview of an ESM frontend died at config load. The
// composed readOnly command must therefore run the frontend (and backend)
// from writable staging copies, never from the original /workspace paths.
func TestReadOnlyStagedRuntimeSpecRedirectsServiceDirectories(t *testing.T) {
	spec := testStagingSpec()
	staged := readOnlyStagedRuntimeSpec(spec)

	if staged.Frontend.Directory != "/tmp/multigent-preview-stage/frontend-web" {
		t.Fatalf("staged frontend dir = %q", staged.Frontend.Directory)
	}
	if staged.Backend.Directory != "/tmp/multigent-preview-stage/backend-server" {
		t.Fatalf("staged backend dir = %q", staged.Backend.Directory)
	}
	// The caller keeps using the original spec for host-side checks; it must
	// not be mutated as a side effect.
	if spec.Frontend.Directory != "web" || spec.Backend.Directory != "server" {
		t.Fatalf("original spec mutated: frontend=%q backend=%q", spec.Frontend.Directory, spec.Backend.Directory)
	}

	command, healthPath, _, err := staged.StartupCommand(ProjectTypeFullstack, 4173)
	if err != nil {
		t.Fatalf("StartupCommand: %v", err)
	}
	if !strings.Contains(command, "cd '/tmp/multigent-preview-stage/frontend-web'") {
		t.Fatalf("frontend must run from the staging copy: %q", command)
	}
	if !strings.Contains(command, "cd '/tmp/multigent-preview-stage/backend-server'") {
		t.Fatalf("backend must run from the staging copy: %q", command)
	}
	if strings.Contains(command, "cd 'web'") || strings.Contains(command, "cd 'server'") {
		t.Fatalf("staged command must not reference read-only worktree dirs: %q", command)
	}
	if healthPath != "/" {
		t.Fatalf("fullstack contract healthPath = %q, want / (frontend proxy serves /)", healthPath)
	}

	// Nested service directories must flatten into one path segment without
	// escaping the staging root.
	nested := stagedServicePath("frontend", "apps/web")
	if nested != "/tmp/multigent-preview-stage/frontend-apps-web" {
		t.Fatalf("nested staged path = %q", nested)
	}
}

func TestReadOnlyStagedRuntimeSpecNilSafety(t *testing.T) {
	if got := readOnlyStagedRuntimeSpec(nil); got != nil {
		t.Fatalf("nil spec must stay nil, got %v", got)
	}
	frontendOnly := &RuntimeSpec{Frontend: &RuntimeServiceSpec{Directory: ".", Command: "npm start"}}
	staged := readOnlyStagedRuntimeSpec(frontendOnly)
	if staged.Frontend.Directory != "/tmp/multigent-preview-stage/frontend-frontend" {
		t.Fatalf("dot directory should fall back to the role name, got %q", staged.Frontend.Directory)
	}
}

func TestStageReadOnlyServicesPrefixCopiesAndInstalls(t *testing.T) {
	if got := stageReadOnlyServicesPrefix(nil); got != "" {
		t.Fatalf("nil spec must produce no prefix, got %q", got)
	}

	prefix := stageReadOnlyServicesPrefix(testStagingSpec())
	if prefix == "" || !strings.HasSuffix(prefix, "&& ") {
		t.Fatalf("prefix must end with the same '&& ' contract as frontendInstallCommand, got %q", prefix)
	}
	// Both service dirs are copied from the read-only mount into writable
	// staging copies (rm + mkdir + cp so reruns start clean, dotfiles intact).
	for _, want := range []string{
		"cp -a '/workspace/server/.' '/tmp/multigent-preview-stage/backend-server/'",
		"cp -a '/workspace/web/.' '/tmp/multigent-preview-stage/frontend-web/'",
	} {
		if !strings.Contains(prefix, want) {
			t.Fatalf("missing staging copy step %q in %q", want, prefix)
		}
	}
	// The staging copy never carries node_modules (gitignored, absent in
	// snapshots), so the frontend must reinstall there before any service
	// starts — same foreground-install contract as the writable path.
	if !strings.Contains(prefix, "npm install --no-audit --no-fund") {
		t.Fatalf("frontend staging copy must npm install: %q", prefix)
	}
	if strings.Index(prefix, "cp -a '/workspace/web/") > strings.Index(prefix, "npm install") {
		t.Fatalf("npm install must run after the frontend copy: %q", prefix)
	}
}

// Wiring regression: startPreview must combine the staging prefix with the
// STAGED spec's command. Staging the files while still running "cd 'web'"
// against the :ro mount would reproduce the EROFS defect this staging exists
// to fix. This test exercises startPreview's actual composition code path —
// the first live deployment showed the pure-function tests passed while the
// engine discarded the staging prefix by overwriting command after composing
// it (container ran "cd /tmp/multigent-preview-stage/…" with no rm/mkdir/cp).
func TestReadOnlyCompositionRunsServicesFromStagingCopies(t *testing.T) {
	spec := testStagingSpec()
	prefix := stageReadOnlyServicesPrefix(spec)
	if prefix == "" {
		t.Fatal("readOnly contract services always need a staging prefix")
	}
	command, _, _, err := readOnlyStagedRuntimeSpec(spec).StartupCommand(ProjectTypeFullstack, 4173)
	if err != nil {
		t.Fatal(err)
	}
	composed := "{ " + strings.TrimSuffix(strings.TrimSuffix(prefix, " "), "&&") + " ; } && { " + command + " ; }"

	if strings.Contains(composed, "cd 'web'") || strings.Contains(composed, "cd 'server'") {
		t.Fatalf("readOnly composition must not cd into :ro worktree dirs: %q", composed)
	}
	if strings.Count(composed, "/tmp/multigent-preview-stage/") < 2 {
		t.Fatalf("both services must run from staging copies: %q", composed)
	}
	if strings.Index(composed, "npm install") > strings.Index(composed, "npm run dev") {
		t.Fatalf("install must complete before the dev server starts: %q", composed)
	}
}

// startPreviewProbeBuilder writes the fake docker that records its argv into
// MULTIGENT_TEST_PROBE_ARGS (unit-separator records, field-separator args).
func startPreviewProbeBuilder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "docker")
	script := "#!/bin/sh\ni=0; rec=\"\"\nfor a in \"$@\"; do\n  if [ \"$i\" -gt 0 ]; then rec=\"$rec\\x1f\"; fi\n  rec=\"$rec$a\"; i=$((i+1))\ndone\nprintf '\\036%s' \"$rec\" >> \"$MULTIGENT_TEST_PROBE_FILE\"\nexit 1\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// End-to-end wiring regression against startPreview itself: the first live
// deployment shipped staged cd targets WITHOUT the staging prefix because the
// engine composed the prefix and then overwrote command with the staged
// StartupCommand result. Pure-function tests could not see that; capturing the
// real docker argv can. No container is started — the fake docker records the
// invocation and exits 1.
func TestStartPreviewReadOnlyComposesStagingBeforeStartupCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := `{"version":1,"frontend":{"directory":"web","command":"npm run dev -- --host 0.0.0.0 --port ${PORT}"},"backend":{"directory":"server","command":"go run .","port":8080,"healthPath":"/api/health"},"preview":{"healthPath":"/","startupTimeoutSeconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".multigent", "runtime.json"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	probeDir := startPreviewProbeBuilder(t)
	probeFile := filepath.Join(t.TempDir(), "probe")
	f, err := os.Create(probeFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("PATH", probeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MULTIGENT_TEST_PROBE_FILE", probeFile)

	e := NewEngine()
	_, _ = e.startPreview(context.Background(), "t-stage-wiring", "proj", root, true, RuntimeSelection{}) //nolint:errcheck — fake docker always "fails"; argv is what we assert

	raw, err := os.ReadFile(probeFile)
	if err != nil || len(raw) == 0 {
		t.Fatalf("probe recorded no docker invocation: %v", err)
	}
	var runArgs []string
	for _, rec := range strings.Split(strings.TrimPrefix(string(raw), "\x1e"), "\x1e") {
		fields := strings.Split(rec, `\x1f`)
		if len(fields) > 2 && fields[0] == "run" {
			runArgs = fields
		}
	}
	if runArgs == nil {
		t.Fatalf("no docker run invocation recorded: %q", string(raw))
	}
	// The image ref is the last arg before the command; the sh command is last.
	cmdText := runArgs[len(runArgs)-1]
	if !strings.Contains(cmdText, "cp -a '/workspace/server/.' '/tmp/multigent-preview-stage/backend-server/'") {
		t.Fatalf("startPreview dropped the staging copy steps: %q", cmdText)
	}
	if !strings.Contains(cmdText, "cd '/tmp/multigent-preview-stage/frontend-web'") {
		t.Fatalf("services must run from staged copies: %q", cmdText)
	}
	if !strings.Contains(cmdText, "npm install --no-audit --no-fund") {
		t.Fatalf("staged frontend must npm install before starting: %q", cmdText)
	}
	if strings.Contains(cmdText, "cd 'web'") || strings.Contains(cmdText, "cd 'server'") {
		t.Fatalf("command must not cd into the :ro worktree dirs: %q", cmdText)
	}
	stagingIdx := strings.Index(cmdText, "cp -a '/workspace/server/")
	devIdx := strings.Index(cmdText, "npm run dev")
	if stagingIdx > devIdx {
		t.Fatalf("staging copies must complete before the dev server starts: %q", cmdText)
	}
}
