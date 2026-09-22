package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestStripCredentials(t *testing.T) {
	cases := map[string]string{
		"http://oauth2:tok@gitlab.internal:8083/g/p.git": "http://gitlab.internal:8083/g/p.git",
		"https://gitlab.com/g/p.git":                     "https://gitlab.com/g/p.git",
		"http://user:pass@host/x":                        "http://host/x",
		"https://host:8443/a/b.git":                      "https://host:8443/a/b.git",
	}
	for in, want := range cases {
		if got := stripCredentials(in); got != want {
			t.Errorf("stripCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeWorkspaceHealthStates(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	_ = workspaceID
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	// Missing: no workspace dir at all.
	h := s.probeWorkspaceHealth(t.Context(), "sample")
	if h.State != WorkspaceStateMissing {
		t.Fatalf("missing dir: got %s (%s), want %s", h.State, h.Detail, WorkspaceStateMissing)
	}

	// Unmanaged: dir without .git, no verified binding.
	wsDir := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h = s.probeWorkspaceHealth(t.Context(), "sample")
	if h.State != WorkspaceStateUnmanaged {
		t.Fatalf("unmanaged: got %s (%s), want %s", h.State, h.Detail, WorkspaceStateUnmanaged)
	}

	// Fresh: a real local-only git checkout (no origin) reports no_remote,
	// which is not a staleness signal.
	seedWorkspaceGitRepo(t, wsDir)
	h = s.probeWorkspaceHealth(t.Context(), "sample")
	if h.State != WorkspaceStateNoRemote || !h.HasGit {
		t.Fatalf("no_remote: got %s hasGit=%v (%s)", h.State, h.HasGit, h.Detail)
	}
}

// seedWorkspaceGitRepo turns dir into a minimal git checkout with one commit
// and no origin remote.
func seedWorkspaceGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
}
