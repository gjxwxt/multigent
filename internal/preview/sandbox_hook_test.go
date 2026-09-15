package preview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/sandbox"
)

// fullstackWorktree creates the minimal file layout DetectProjectType maps
// to ProjectTypeFullstack so tests reach the provisioning hook (CLI
// short-circuits before it).
func fullstackWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"package.json":                       "{}",
		filepath.Join("web", "package.json"): "{}",
		filepath.Join("server", "go.mod"):    "module example.com/x\n",
	}
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSandboxHookFailClosed: a provisioning error must abort the preview
// start before any docker call (a contract-bearing project must never boot
// against a missing or drifted database). The hook error surfaces BEFORE
// docker is touched — asserted by short-circuiting with an error and
// checking no docker invocation happened (docker call would take seconds
// and leave a container; the failure here is instant).
func TestSandboxHookFailClosed(t *testing.T) {
	e := NewEngine()
	t.Cleanup(func() { _ = e.StopEphemeralPreview("t-sb-1") })
	calls := 0
	e.SetSandboxHooks(func(ctx context.Context, taskID, projectName, worktreeDir string) ([]string, error) {
		calls++
		return nil, errors.New("schema drift detected")
	}, nil)
	dir := fullstackWorktree(t)
	done := make(chan error, 1)
	go func() {
		_, err := e.StartEphemeralPreview(context.Background(), "t-sb-1", "proj", dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "fixture sandbox provisioning failed") {
			t.Fatalf("expected fail-closed provision error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provision failure must short-circuit before docker (still running after 5s)")
	}
	if calls != 1 {
		t.Fatalf("hook must run exactly once, ran %d", calls)
	}
}

// TestSandboxHookRunsBeforeDocker: with a working hook the start proceeds to
// the docker boundary; we don't run docker in unit tests — if docker IS
// available the test must not spin containers, so we assert only the ordering
// guarantee via the hook flag and skip past docker failures.
func TestSandboxHookRunsBeforeDocker(t *testing.T) {
	if err := sandbox.CheckDocker(); err == nil {
		t.Skip("docker available; a real container would be started — covered by deployment E2E instead")
	}
	e := NewEngine()
	provisioned := false
	e.SetSandboxHooks(func(ctx context.Context, taskID, projectName, worktreeDir string) ([]string, error) {
		provisioned = true
		return []string{"APP_DB_PATH=/data/sandbox/app.db"}, nil
	}, nil)
	dir := fullstackWorktree(t)
	done := make(chan error, 1)
	go func() {
		_, err := e.StartEphemeralPreview(context.Background(), "t-sb-2", "proj", dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("docker is unavailable; the start must fail")
		}
		if strings.Contains(err.Error(), "fixture sandbox provisioning failed") {
			t.Fatalf("provisioning must succeed: %v", err)
		}
		if !provisioned {
			t.Fatal("provision hook must run before docker")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("docker-failure path took too long")
	}
}

// TestSandboxHookNotCalledWhenNil: legacy behavior (no sandbox integration)
// must be preserved when hooks are unset — the start goes straight to docker
// without any provision step. Only run without docker to avoid real starts.
func TestSandboxHookNotCalledWhenNil(t *testing.T) {
	if err := sandbox.CheckDocker(); err == nil {
		t.Skip("docker available; a real container would be started")
	}
	e := NewEngine()
	dir := fullstackWorktree(t)
	done := make(chan error, 1)
	go func() {
		_, err := e.StartEphemeralPreview(context.Background(), "t-sb-3", "proj", dir)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("no-hook start must still terminate (docker failure)")
	}
	// No panic and no hook: nothing else to assert — the guarantee is that
	// the nil-hook path compiles and behaves like the pre-sandbox engine.
}

// TestReleaseHookOnStop: stopping a preview must fire the release hook.
func TestReleaseHookOnStop(t *testing.T) {
	e := NewEngine()
	released := make(chan string, 1)
	e.SetSandboxHooks(nil, func(taskID, reason string) {
		released <- taskID + "|" + reason
	})
	_ = e.StopEphemeralPreview("t-sb-release")
	got := <-released
	if got != "t-sb-release|stopped" {
		t.Fatalf("release hook not fired as expected: %q", got)
	}
}
