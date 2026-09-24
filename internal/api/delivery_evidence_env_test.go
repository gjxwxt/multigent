package api

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

// TestDeliveryEvidenceEnvInjectsTransientCredential (round 6, D-D). The
// API-side delivery gate runs on the console host, which the
// credentials-not-on-disk invariant leaves without any git credential helper:
// its `git ls-remote` for push evidence therefore failed closed on every
// PRIVATE remote. Real S2 branch task t-20260924-hps07k pushed its commit
// (visible via ls-remote from inside the run) and was still rejected with
// "git delivery evidence unreadable". The fix injects the project connection
// credential transiently — process-scoped GIT_CONFIG_* env for the duration of
// one read-only command, never on disk, never in repo config, never in the
// remote URL.
func TestDeliveryEvidenceEnvInjectsTransientCredential(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)

	// No worktree / no resolvable connection → nil env: the gate keeps failing
	// closed exactly as before (unprovable is not delivered).
	if env := s.deliveryEvidenceEnv("sample", ""); env != nil {
		t.Fatalf("missing worktree must not receive credentials, got %d env entries", len(env))
	}

	// A worktree whose origin points at a FOREIGN host must never receive the
	// platform token (credential exfiltration guard): the gate then behaves as
	// before and fails closed.
	foreign := seedDeliveryWorktree(t, "http://attacker.example.invalid/root/sample.git")
	if env := s.deliveryEvidenceEnv("sample", foreign); env != nil {
		t.Fatalf("a foreign origin must not receive the connection credential, got %d env entries", len(env))
	}

	seedGitLabConnection(t, s, workspaceID, "conn-gitlab-evidence", "http://gitlab.example.invalid")
	if err := s.controlDB.UpsertVerifiedRemoteBinding(controldb.VerifiedRemoteBinding{
		WorkspaceID:       workspaceID,
		ProjectID:         "sample",
		Provider:          "gitlab",
		ConnectionID:      "conn-gitlab-evidence",
		RemoteProjectID:   "58",
		PathWithNamespace: "root/sample",
		Source:            controldb.BindingSourceExplicitVerify,
	}); err != nil {
		t.Fatalf("verified binding: %v", err)
	}

	matching := seedDeliveryWorktree(t, "http://gitlab.example.invalid/root/sample.git")
	env := s.deliveryEvidenceEnv("sample", matching)
	if env == nil {
		t.Fatal("a worktree whose origin matches the connection host must receive the transient credential env")
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=http.extraHeader") {
		t.Fatalf("transient env must carry the credential helper config, got:\n%s", joined)
	}
	if !strings.Contains(joined, "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
		t.Fatalf("transient env must carry the basic-auth header, got:\n%s", joined)
	}
	// The credential rides the child env only: it must NOT be installed into
	// the console process itself (credentials-not-on-disk / not-in-ambient-env).
	// The helper returns append(os.Environ(), GIT_CONFIG_*) — so a leaked
	// process-level config would show up as an ambient entry here.
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "" {
		t.Fatalf("transient git config must not leak into the console process env (GIT_CONFIG_COUNT=%q)", got)
	}
	if got := os.Getenv("GIT_CONFIG_KEY_0"); got != "" {
		t.Fatalf("transient git config must not leak into the console process env (GIT_CONFIG_KEY_0=%q)", got)
	}
}

// seedDeliveryWorktree creates a directory that looks like a task worktree with
// one origin remote. Only `git remote get-url origin` needs to resolve.
func seedDeliveryWorktree(t *testing.T, originURL string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("remote", "add", "origin", originURL)
	return dir
}
