package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// D-5 regression (2026-09-24): a task with a delivery contract must NOT be
// reported done_success when the run produced zero model activity (the
// unbound-node worker fell back to local execution without model credentials
// and exit-0 was recorded as a hollow success).
func TestDeliveryContractFailsWithoutModelActivity(t *testing.T) {
	c := deliveryContract{RequireModelActivity: true}
	v := validateDeliveryEvidence(c, "=== exit code: 0 ===", "", "", "", "")
	if v == "" || !strings.Contains(v, "no model activity") {
		t.Fatalf("expected no-model-activity violation, got %q", v)
	}
	v = validateDeliveryEvidence(c, "{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n", "", "", "", "")
	if v != "" {
		t.Fatalf("assistant line should satisfy model activity, got %q", v)
	}
}

func TestDeliveryContractTestsRunEvidence(t *testing.T) {
	c := deliveryContract{RequireTestsRun: true}
	if v := validateDeliveryEvidence(c, "Tests run: 0, Failures: 0", "", "", "", ""); v == "" {
		t.Fatal("Tests run: 0 must not satisfy the contract")
	}
	if v := validateDeliveryEvidence(c, "Tests run: 20, Failures: 0, Errors: 0", "", "", "", ""); v != "" {
		t.Fatalf("Tests run: 20 should satisfy the contract, got %q", v)
	}
	if v := validateDeliveryEvidence(c, "BUILD SUCCESS", "", "", "", ""); v == "" {
		t.Fatal("output without a Tests run summary must not satisfy the contract")
	}
}

func TestDeliveryContractParse(t *testing.T) {
	if _, err := parseDeliveryContract(""); err == nil {
		t.Fatal("empty contract must be rejected")
	}
	if _, err := parseDeliveryContract("not-json"); err == nil {
		t.Fatal("non-JSON contract must be rejected")
	}
	c, err := parseDeliveryContract(`{"requireModelActivity":true,"requireGitCommit":true,"requirePush":true,"futureField":1}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !c.RequireModelActivity || !c.RequireGitCommit || !c.RequirePush {
		t.Fatalf("fields not parsed: %+v", c)
	}
}

func seedGitRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitDeliveryEvidenceCommitBeyondBase(t *testing.T) {
	dir := t.TempDir()
	seedGitRepo(t, dir, "init", "-b", "main")
	seedGitRepo(t, dir, "config", "user.email", "t@t")
	seedGitRepo(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "base")
	seedGitRepo(t, dir, "checkout", "-b", "task/x")
	commitOK, _, err := gitDeliveryEvidence(dir, "main", "")
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if commitOK {
		t.Fatal("branch even with main must not count as a commit beyond base")
	}
	// Round-3 item 1: the frozen BaseCommit SHA must behave identically to
	// the branch name when they point at the same commit.
	baseSHA := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "main"))
	commitOK, _, err = gitDeliveryEvidence(dir, baseSHA, "")
	if err != nil {
		t.Fatalf("evidence(baseSHA): %v", err)
	}
	if commitOK {
		t.Fatal("frozen base SHA must measure the same increment as the branch name")
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "work")
	commitOK, _, err = gitDeliveryEvidence(dir, "main", "")
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if !commitOK {
		t.Fatal("one commit beyond main must be detected")
	}
}

// D-7 regression (2026-09-24): a plain task that declares a worktree must
// fail closed when the worktree is unreachable — never silently run on the
// agent home while presenting the worktree boundary.
func TestRunTaskPlainTaskWorktreeUnreachableFailsClosed(t *testing.T) {
	r := New(t.TempDir(), nil, nil)
	r.SetAgentMetaOverride("proj", "agent", &entity.AgentMeta{
		Name:    "agent",
		Project: "proj",
	})
	missing := filepath.Join(t.TempDir(), "gone")
	task := &entity.Task{
		ID:          "t-wt-missing",
		Title:       "plain task with declared worktree",
		Status:      entity.TaskStatusPending,
		BranchName:  "task/x",
		WorktreeDir: missing,
	}
	_, err := r.RunTask("proj", "agent", task, "")
	if err == nil {
		t.Fatal("unreachable worktree must fail the run")
	}
	if !strings.Contains(err.Error(), "execution_scope_mismatch") {
		t.Fatalf("expected execution_scope_mismatch, got %v", err)
	}
}

// P2-vi (review round): the gate itself — a task carrying a contract var
// with zero model activity must flip done_success → done_failed.
func TestApplyDeliveryContractGateFlipsHollowSuccess(t *testing.T) {
	r := New(t.TempDir(), nil, nil)
	task := &entity.Task{
		ID:    "t-contract",
		Title: "contracted task",
		Vars: map[string]string{
			deliveryContractVar: `{"requireModelActivity":true,"requireGitCommit":true}`,
		},
		BaseBranch: "main",
	}
	result := &RunResult{Status: entity.TaskStatusDoneSuccess}
	var logBuf strings.Builder
	r.applyDeliveryContractGate(result, task, "=== exit code: 0 ===", t.TempDir(), &logBuf)
	if result.Status != entity.TaskStatusDoneFailed {
		t.Fatalf("hollow success must flip to done_failed, got %s (%s)", result.Status, result.ErrorMsg)
	}
	if !strings.Contains(logBuf.String(), "delivery contract violated") {
		t.Fatalf("violation must be mirrored to the run log, got %q", logBuf.String())
	}
}

// Base branch not resolvable (shallow clone) must fail closed — the old
// rev-list HEAD fallback reported delivered for an empty history.
func TestGitDeliveryEvidenceMissingBaseFailsClosed(t *testing.T) {
	dir := t.TempDir()
	seedGitRepo(t, dir, "init", "-b", "task/x")
	seedGitRepo(t, dir, "config", "user.email", "t@t")
	seedGitRepo(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "only commit")
	_, _, err := gitDeliveryEvidence(dir, "main", "")
	if err == nil {
		t.Fatal("missing base branch must fail closed, not fall back")
	}
}

// Round-3 item 2 counterexample: the remote branch EXISTS but sits at an
// OLDER commit while the local delivery commit is unpushed — the push
// evidence must NOT count as pushed, and the violation must name both SHAs.
func TestDeliveryContractRemoteStaleWhileLocalAhead(t *testing.T) {
	dir := t.TempDir()
	remote := t.TempDir()
	seedGitRepo(t, remote, "init", "--bare", "-b", "main")
	seedGitRepo(t, dir, "clone", remote, dir)
	seedGitRepo(t, dir, "config", "user.email", "t@t")
	seedGitRepo(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "base")
	seedGitRepo(t, dir, "push", "origin", "main")
	seedGitRepo(t, dir, "checkout", "-b", "task/x")
	if err := os.WriteFile(filepath.Join(dir, "old.go"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "old delivery")
	seedGitRepo(t, dir, "push", "origin", "task/x")
	// Local advances BEYOND the pushed tip: the new commit is NOT on the
	// remote — the classic "remote at old commit, local unpushed" shape.
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "new delivery (unpushed)")

	c := deliveryContract{RequireGitCommit: true, RequirePush: true}
	v := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x")
	if v == "" {
		t.Fatal("remote at an older commit while local is ahead must NOT count as pushed")
	}
	if !strings.Contains(v, "unpushed") || !strings.Contains(v, "remote branch is at") {
		t.Fatalf("violation must name the stale remote state, got %q", v)
	}

	// The same state WITHOUT the unpushed commit must pass — proves the
	// gate fails on the stale remote, not on something else.
	seedGitRepo(t, dir, "reset", "--hard", "HEAD~1")
	if v := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x"); v != "" {
		t.Fatalf("pushed state should satisfy the contract, got %q", v)
	}
}

// Round-3 item 1: the frozen BaseCommit SHA gates the increment — a base
// branch that moved after task creation cannot hide or launder the
// increment (rev-list is anchored to the SHA, not the moving ref).
func TestDeliveryContractFrozenBaseCommitAnchorsIncrement(t *testing.T) {
	dir := t.TempDir()
	seedGitRepo(t, dir, "init", "-b", "main")
	seedGitRepo(t, dir, "config", "user.email", "t@t")
	seedGitRepo(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "base")
	frozenBase := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "HEAD"))
	seedGitRepo(t, dir, "checkout", "-b", "task/x")
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "delivery")
	// The base branch ADVANCES after task creation (platform-side merge).
	seedGitRepo(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "unrelated.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "platform-side advance")
	seedGitRepo(t, dir, "checkout", "task/x")

	c := deliveryContract{RequireGitCommit: true}
	if v := ValidateGitDeliveryEvidence(c, dir, frozenBase, "main", ""); v != "" {
		t.Fatalf("frozen base must still see the branch increment: %q", v)
	}
	// The frozen SHA is the contract anchor: a branch with no increment
	// beyond it must fail, and the violation must name the exact base the
	// increment is measured against (the SHA, not a movable branch name).
	seedGitRepo(t, dir, "checkout", "-b", "task/empty", frozenBase)
	if v := ValidateGitDeliveryEvidence(c, dir, frozenBase, "", ""); v == "" || !strings.Contains(v, "no commit beyond the frozen base "+frozenBase) {
		t.Fatalf("violation must reference the frozen base commit, got %q", v)
	}
}
