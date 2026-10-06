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
	v := validateDeliveryEvidence(c, "=== exit code: 0 ===", "", "", "", "", nil)
	if v == "" || !strings.Contains(v, "no model activity") {
		t.Fatalf("expected no-model-activity violation, got %q", v)
	}
	v = validateDeliveryEvidence(c, "{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n", "", "", "", "", nil)
	if v != "" {
		t.Fatalf("assistant line should satisfy model activity, got %q", v)
	}
}

func TestDeliveryContractTestsRunEvidence(t *testing.T) {
	c := deliveryContract{RequireTestsRun: true}
	if v := validateDeliveryEvidence(c, "Tests run: 0, Failures: 0", "", "", "", "", nil); v == "" {
		t.Fatal("Tests run: 0 must not satisfy the contract")
	}
	if v := validateDeliveryEvidence(c, "Tests run: 20, Failures: 0, Errors: 0", "", "", "", "", nil); v != "" {
		t.Fatalf("Tests run: 20 should satisfy the contract, got %q", v)
	}
	if v := validateDeliveryEvidence(c, "BUILD SUCCESS", "", "", "", "", nil); v == "" {
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
	commitOK, _, err := gitDeliveryEvidence(dir, "main", "", nil)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if commitOK {
		t.Fatal("branch even with main must not count as a commit beyond base")
	}
	// Round-3 item 1: the frozen BaseCommit SHA must behave identically to
	// the branch name when they point at the same commit.
	baseSHA := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "main"))
	commitOK, _, err = gitDeliveryEvidence(dir, baseSHA, "", nil)
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
	commitOK, _, err = gitDeliveryEvidence(dir, "main", "", nil)
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
	r.applyDeliveryContractGate(result, task, "=== exit code: 0 ===", t.TempDir(), nil, &logBuf)
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
	_, _, err := gitDeliveryEvidence(dir, "main", "", nil)
	if err == nil {
		t.Fatal("missing base branch must fail closed, not fall back")
	}
}

// B1 (2026-09-29): a project with NO origin remote delivers on LOCAL
// evidence — commit beyond the frozen base plus a verified local branch
// tip. The old gate demanded `ls-remote origin <branch>`, which forced
// agents on remote-less projects to `git init --bare` fake origins to
// satisfy the evidence check.
func TestDeliveryContractNoRemoteAcceptsLocalEvidence(t *testing.T) {
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

	c := deliveryContract{RequireGitCommit: true, RequirePush: true}
	// Nothing committed beyond base: the no-remote gate still requires the
	// real commit evidence.
	if v, _ := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil); v == "" {
		t.Fatal("no-remote mode must still demand a commit beyond the frozen base")
	}
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "local delivery")

	v, push := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil)
	if v != "" {
		t.Fatalf("a committed local delivery on a remote-less project must satisfy the contract, got %q", v)
	}
	local := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "refs/heads/task/x"))
	if push.LocalSHA != local {
		t.Fatalf("evidence must carry the verified local tip %q, got %+v", local, push)
	}
	// Delivery SHA hand-off: the local tip is machine-verified (rev-parse),
	// so the proven pair is surfaced even without a remote — RemoteSHA ==
	// LocalSHA is the hand-off's acceptance shape.
	if push.RemoteSHA != local || len(push.RemoteSHA) != 40 {
		t.Fatalf("no-remote evidence must carry the verified local tip as the pair, got %+v", push)
	}
	out := map[string]string{"delivery_sha": "forged"}
	MergeDeliveryEvidence(out, "task/x", push)
	if out["delivery_sha"] != local {
		t.Fatalf("no-remote proven pair must anchor delivery_sha on the verified local tip, got %q", out["delivery_sha"])
	}
}

// B1 counterexample: a remote EXISTS and is bound — the exact previous
// behaviour must be preserved (branch missing on the remote = violation).
func TestDeliveryContractWithRemoteKeepsPushRequirement(t *testing.T) {
	remote := t.TempDir()
	seedGitRepo(t, remote, "init", "--bare", "-b", "main")
	dir := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "unpushed delivery")

	c := deliveryContract{RequireGitCommit: true, RequirePush: true}
	v, _ := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil)
	if v == "" || !strings.Contains(v, "branch not found on the remote") {
		t.Fatalf("a bound remote without the branch must still fail the push evidence, got %q", v)
	}

	// Push and the exact previous pass shape must return.
	seedGitRepo(t, dir, "push", "origin", "task/x")
	if v, _ := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil); v != "" {
		t.Fatalf("pushed state must satisfy the contract as before, got %q", v)
	}
}

// B1 anti-fabrication: an origin pointing INSIDE .multigent/ (the bare repo
// the agent authored at /workspace/.multigent/origin-<x>.git in the real
// incident) is not independent evidence — the gate must treat it exactly
// like "no remote" and accept the LOCAL tip instead, so a self-created
// origin neither helps nor is required.
func TestDeliveryContractMultigentLocalOriginIsNotRemoteEvidence(t *testing.T) {
	fabric := filepath.Join(t.TempDir(), "workspace", ".multigent", "origin-batch-test1.git")
	if err := os.MkdirAll(fabric, 0o755); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, fabric, "init", "--bare", "-b", "main")
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
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "local delivery")

	c := deliveryContract{RequireGitCommit: true, RequirePush: true}

	// The fabricated remote has NEVER seen the branch — under the old gate
	// this state failed ("branch not found on the remote"), which is what
	// pushed the agent to push into its own bare repo. The classification
	// alone must NOT count as push evidence.
	if mode := originEvidenceForDir(dir); mode == originBound {
		t.Fatal("a .multigent/ origin must not classify as a bound remote")
	}
	if v, _ := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil); v != "" {
		t.Fatalf("a .multigent/ origin must downgrade to no-remote mode and accept the local tip, got %q", v)
	}

	// Even when the agent DID push into its fabricated bare repo, the
	// fabricated remote must not be consulted: evidence stays the local tip
	// (which coincides here), never a "remote push succeeded" verdict built
	// on the agent's own repo.
	seedGitRepo(t, dir, "remote", "add", "origin", fabric)
	seedGitRepo(t, dir, "push", "origin", "task/x")
	v, push := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil)
	if v != "" {
		t.Fatalf("pushing into a .multigent/ origin must not fail the contract (local evidence stands), got %q", v)
	}
	local := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "refs/heads/task/x"))
	if push.LocalSHA != local || push.RemoteSHA != local {
		t.Fatalf("evidence must be the locally verified tip, got %+v", push)
	}
}

// B1 classification unit: every legitimate remote spelling stays bound;
// every .multigent/ path spelling is fabrication.
func TestClassifyOriginURL(t *testing.T) {
	bound := []string{
		"https://gitlab.com/group/repo.git",
		"http://git.example.invalid/private/repo.git",
		"git@gitlab.com:group/repo.git",
		"ssh://git@host/group/repo.git",
		"file:///srv/git/repo.git",
		"/srv/git/repo.git",
	}
	for _, u := range bound {
		if mode := classifyOriginURL(u); mode != originBound {
			t.Errorf("%q must classify as originBound, got %d", u, mode)
		}
	}
	fabric := []string{
		"/workspace/.multigent/origin-x.git",
		"file:///workspace/.multigent/origin-x.git",
		"workspace/.multigent/origin-x.git",
		"/workspace/.multigent/runtime-home/origin-x.git",
	}
	for _, u := range fabric {
		if mode := classifyOriginURL(u); mode != originLocalFabric {
			t.Errorf("%q must classify as originLocalFabric, got %d", u, mode)
		}
	}
	// Lookalike that must NOT trip the check: .multigentX or multigent/.
	if mode := classifyOriginURL("/srv/.multigentx/repo.git"); mode != originBound {
		t.Errorf("partial-segment match must stay bound, got %d", mode)
	}
	if mode := classifyOriginURL("/srv/multigent/repo.git"); mode != originBound {
		t.Errorf("non-dotted segment must stay bound, got %d", mode)
	}
}

// B1 prompt text: a contract on a remote-less worktree must tell the agent
// NOT to push and NOT to fabricate a remote; a bound worktree keeps the
// exact push instruction.
func TestDeliveryContractSectionNoRemoteWording(t *testing.T) {
	dir := t.TempDir()
	seedGitRepo(t, dir, "init", "-b", "main")
	contractVars := map[string]string{DeliveryContractVar: `{"requireGitCommit":true,"requirePush":true}`}

	noRemote := deliveryContractSection(&entity.Task{
		WorktreeDir: dir,
		BranchName:  "task/x",
		Vars:        contractVars,
	})
	for _, want := range []string{"NO remote configured", "Do **not** push", "git remote add/set-url", "fabricated evidence"} {
		if !strings.Contains(noRemote, want) {
			t.Fatalf("no-remote contract section must contain %q, got:\n%s", want, noRemote)
		}
	}
	for _, bad := range []string{"must exist on `origin`", "ls-remote origin"} {
		if strings.Contains(noRemote, bad) {
			t.Fatalf("no-remote contract section must not demand pushing, got:\n%s", noRemote)
		}
	}

	boundDir := t.TempDir()
	remote := t.TempDir()
	seedGitRepo(t, remote, "init", "--bare", "-b", "main")
	seedGitRepo(t, boundDir, "clone", remote, boundDir)
	bound := deliveryContractSection(&entity.Task{
		WorktreeDir: boundDir,
		BranchName:  "task/x",
		Vars:        contractVars,
	})
	for _, want := range []string{"must exist on `origin`", "ls-remote origin"} {
		if !strings.Contains(bound, want) {
			t.Fatalf("bound contract section must keep the push instruction, got:\n%s", bound)
		}
	}
	if strings.Contains(bound, "NO remote configured") {
		t.Fatalf("bound contract section must not render the no-remote wording, got:\n%s", bound)
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
	v, push := ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil)
	if v == "" {
		t.Fatal("remote at an older commit while local is ahead must NOT count as pushed")
	}
	if !strings.Contains(v, "unpushed") || !strings.Contains(v, "remote branch is at") {
		t.Fatalf("violation must name the stale remote state, got %q", v)
	}
	// Delivery SHA hand-off: a violated gate must NOT emit usable evidence.
	if push.RemoteSHA != "" || push.LocalSHA != "" {
		t.Fatalf("failed gate must not surface push evidence, got %+v", push)
	}

	// The same state WITHOUT the unpushed commit must pass — proves the
	// gate fails on the stale remote, not on something else.
	seedGitRepo(t, dir, "reset", "--hard", "HEAD~1")
	v, push = ValidateGitDeliveryEvidence(c, dir, "", "main", "task/x", nil)
	if v != "" {
		t.Fatalf("pushed state should satisfy the contract, got %q", v)
	}
	// Delivery SHA hand-off: the passing gate surfaces the proven pair —
	// full 40-hex remote SHA equal to local HEAD — which is exactly what
	// the platform merges into delivery_sha/delivery_branch outputs.
	if push.RemoteSHA == "" || push.LocalSHA == "" || push.RemoteSHA != push.LocalSHA || len(push.RemoteSHA) != 40 {
		t.Fatalf("passing gate must surface the proven SHA pair, got %+v", push)
	}
	local := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "HEAD"))
	if push.LocalSHA != local {
		t.Fatalf("proven local SHA %q must equal git rev-parse HEAD %q", push.LocalSHA, local)
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
	if v, _ := ValidateGitDeliveryEvidence(c, dir, frozenBase, "main", "", nil); v != "" {
		t.Fatalf("frozen base must still see the branch increment: %q", v)
	}
	// The frozen SHA is the contract anchor: a branch with no increment
	// beyond it must fail, and the violation must name the exact base the
	// increment is measured against (the SHA, not a movable branch name).
	seedGitRepo(t, dir, "checkout", "-b", "task/empty", frozenBase)
	if v, _ := ValidateGitDeliveryEvidence(c, dir, frozenBase, "", "", nil); v == "" || !strings.Contains(v, "no commit beyond the frozen base "+frozenBase) {
		t.Fatalf("violation must reference the frozen base commit, got %q", v)
	}
}

// TestGitDeliveryEvidenceUsesInjectedCredentialEnv (round 6, D-D). The
// API-side gate runs on the console host, which by the credentials-not-on-disk
// invariant has NO git credential helper — a private remote is unreadable with
// the bare process env, so an honest push was rejected fail-closed on real S2
// branch task t-20260924-hps07k. The gate must therefore accept a transient
// credential env for its read-only ls-remote.
//
// The test reproduces the shape without a real server: the task worktree's
// `origin` points at an unreachable HTTPS URL (the credential-less case), and
// the injected env carries a `url.<local-bare>.insteadOf` mapping that stands
// in for the credential the platform injects. With the env the remote branch
// is visible; without it the check fails closed.
func TestGitDeliveryEvidenceUsesInjectedCredentialEnv(t *testing.T) {
	remote := t.TempDir()
	seedGitRepo(t, remote, "init", "--bare", "-b", "main")

	dir := t.TempDir()
	seedGitRepo(t, dir, "init", "-b", "main")
	seedGitRepo(t, dir, "config", "user.email", "t@t")
	seedGitRepo(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "base")
	seedGitRepo(t, dir, "checkout", "-b", "task/push")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGitRepo(t, dir, "add", ".")
	seedGitRepo(t, dir, "commit", "-m", "work")

	// The remote the worktree believes in is unreachable without credentials.
	const fakeURL = "https://git.example.invalid/private/repo.git"
	seedGitRepo(t, dir, "remote", "add", "origin", fakeURL)
	// Push through the mapping (stand-in for the injected credential) so the
	// remote branch exists.
	mapped := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.file://"+remote+".insteadOf",
		"GIT_CONFIG_VALUE_0="+fakeURL,
	)
	push := exec.Command("git", "-C", dir, "push", "origin", "task/push")
	push.Env = mapped
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("seed push: %v (%s)", err, out)
	}

	// Without an injected env the evidence read fails closed.
	if commitOK, pushEv, err := gitDeliveryEvidence(dir, "main", "task/push", nil); err == nil {
		t.Fatalf("credential-less host must not read a private remote (commit=%v push=%+v)", commitOK, pushEv)
	}

	// With the injected env the push is provable (remote SHA == local SHA).
	commitOK, pushEv, err := gitDeliveryEvidence(dir, "main", "task/push", mapped)
	if err != nil {
		t.Fatalf("injected env must make the remote readable: %v", err)
	}
	if !commitOK {
		t.Fatal("expected a commit beyond the frozen base")
	}
	if !pushEv.RemoteHasBranch {
		t.Fatal("expected the remote branch to be observed")
	}
	local := strings.TrimSpace(seedGitRepo(t, dir, "rev-parse", "HEAD"))
	if pushEv.RemoteSHA != local {
		t.Fatalf("remote SHA %s must equal local HEAD %s", pushEv.RemoteSHA, local)
	}
}

// TestDeliveryContractSectionRendersExactEvidenceShapes (round 6, D-E): the
// contract's run-ending gate matches exact output shapes, so the prompt must
// spell them out. Guards the drift between what the gate enforces and what the
// agent is told.
func TestDeliveryContractSectionRendersExactEvidenceShapes(t *testing.T) {
	task := &entity.Task{
		BaseCommit: "f34e686a1f347374690bd18b436c641f663aa29d",
		BranchName: "feature/wf-x-workstream_1",
		Vars: map[string]string{
			DeliveryContractVar: `{"requireModelActivity":true,"requireGitCommit":true,"requirePush":true,"requireTestsRun":true}`,
		},
	}
	section := deliveryContractSection(task)
	for _, want := range []string{
		"Delivery contract",
		"Tests run: <N>",
		"Write-Output",
		"do **not** match",
		"no runnable test suite",
		"f34e686a1f347374690bd18b436c641f663aa29d",
		"feature/wf-x-workstream_1",
		"REMOTE SHA EQUAL to your local HEAD SHA",
		"Model activity",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("contract section must mention %q, got:\n%s", want, section)
		}
	}
	// Only the ACTIVE requirements are rendered: the prompt and the gate read
	// the same parsed contract.
	lean := &entity.Task{Vars: map[string]string{DeliveryContractVar: `{"requireTestsRun":true}`}}
	leanSection := deliveryContractSection(lean)
	if strings.Contains(leanSection, "Model activity") || strings.Contains(leanSection, "Push") {
		t.Fatalf("inactive requirements must not be rendered:\n%s", leanSection)
	}
	if !strings.Contains(leanSection, "Tests run: <N>") {
		t.Fatalf("active requirement must be rendered:\n%s", leanSection)
	}
	if got := deliveryContractSection(&entity.Task{}); got != "" {
		t.Fatalf("a task without a contract renders no section, got %q", got)
	}
	if got := deliveryContractSection(&entity.Task{Vars: map[string]string{DeliveryContractVar: "{not json"}}); got != "" {
		t.Fatalf("a malformed contract renders no section, got %q", got)
	}
}

// Delivery SHA hand-off: MergeDeliveryEvidence only injects a PROVEN pair —
// full 40-hex, remote == local. Anything else leaves the outputs untouched
// so "key absent" reliably means "evidence not proven" downstream.
func TestMergeDeliveryEvidenceOnlyInjectsProvenPair(t *testing.T) {
	full := strings.Repeat("a", 40)
	// Proven pair: injected, overwriting agent-forged values.
	out := map[string]string{"delivery_sha": "forged", "pr": "branch:x"}
	MergeDeliveryEvidence(out, "task/x", PushEvidence{RemoteHasBranch: true, RemoteSHA: full, LocalSHA: full})
	if out["delivery_sha"] != full {
		t.Fatalf("proven pair must overwrite the forged anchor, got %q", out["delivery_sha"])
	}
	if out["delivery_branch"] != "task/x" {
		t.Fatalf("branch must ride the proven evidence, got %q", out["delivery_branch"])
	}
	// Short SHA (the run6 failure mode): never injected AND any agent-forged
	// residue is deleted — unproven evidence must leave the key ABSENT, not
	// leave the agent's value in place (blind-review blocker).
	out = map[string]string{"delivery_sha": "b2388d6", "delivery_branch": "agent/lie"}
	MergeDeliveryEvidence(out, "task/x", PushEvidence{RemoteHasBranch: true, RemoteSHA: "b2388d6", LocalSHA: "b2388d6"})
	if _, ok := out["delivery_sha"]; ok {
		t.Fatal("a short SHA must never be handed to QA as the anchor; forged residue must be deleted")
	}
	if _, ok := out["delivery_branch"]; ok {
		t.Fatal("unproven evidence must also delete the forged branch key")
	}
	// Mismatched pair: never injected, forged residue deleted.
	out = map[string]string{"delivery_sha": full, "pr": "branch:x"}
	MergeDeliveryEvidence(out, "task/x", PushEvidence{RemoteHasBranch: true, RemoteSHA: full, LocalSHA: strings.Repeat("b", 40)})
	if _, ok := out["delivery_sha"]; ok {
		t.Fatal("a remote!=local pair must never be handed to QA as the anchor")
	}
	if out["pr"] != "branch:x" {
		t.Fatalf("merge must not touch unrelated outputs, got pr=%q", out["pr"])
	}
	// Empty evidence (RequireGitCommit-only contract or no contract at all):
	// never injected, forged residue deleted.
	out = map[string]string{"delivery_sha": "agent-forged", "delivery_branch": "agent/branch"}
	MergeDeliveryEvidence(out, "task/x", PushEvidence{})
	if _, ok := out["delivery_sha"]; ok {
		t.Fatal("empty evidence must never be handed to QA as the anchor")
	}
	if _, ok := out["delivery_branch"]; ok {
		t.Fatal("empty evidence must delete the forged branch key")
	}
}

// wp-7 regression (2026-10-06): the gate's ls-remote runs in the runner
// process, whose env lacks GIT_CONFIG_GLOBAL — the credential-helper config
// exists only in the agent container's env. A genuine push then failed the
// push-evidence check ("could not read Username"). The gate must accept the
// run's materialized git env (minimal: GIT_CONFIG_GLOBAL only) so a private
// remote is readable, and must still fail closed without it.
func TestDeliveryEvidenceUsesMaterializedGitEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main", "repo")
	repo := filepath.Join(dir, "repo")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("f.txt", "one\n")
	run("-C", repo, "add", ".")
	run("-C", repo, "commit", "-m", "base")

	// Private "remote": a local bare repo reachable ONLY through the helper
	// config (origin URL uses a credential-requiring transport shape; with a
	// file:// remote git never asks for credentials, so emulate the private
	// case by pointing origin at an unreachable http URL and asserting the
	// env-free read fails while the env-carrying read at least reaches auth).
	remote := run("-C", repo, "clone", "--bare", repo, filepath.Join(dir, "remote.git"))
	_ = remote

	// The real regression guard: env extraction keeps ONLY GIT_CONFIG_GLOBAL.
	effective := []string{
		"PATH=/usr/bin:/bin",
		"SUPER_SECRET_TOKEN=must-not-leak",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "helper.gitconfig"),
	}
	got := gitEnvForEvidence(effective)
	if len(got) != 1 || !strings.HasPrefix(got[0], "GIT_CONFIG_GLOBAL=") {
		t.Fatalf("git env must carry only GIT_CONFIG_GLOBAL, got %v", got)
	}
	if strings.Contains(strings.Join(got, " "), "SUPER_SECRET_TOKEN") {
		t.Fatal("non-git secrets must never reach the evidence command")
	}
	if gitEnvForEvidence([]string{"PATH=/bin"}) != nil {
		t.Fatal("no git config in env must yield nil (legacy inherit behaviour)")
	}
	if gitEnvForEvidence(nil) != nil {
		t.Fatal("empty env must yield nil")
	}
}

func TestGitEnvForEvidenceFindsOnlyGitConfig(t *testing.T) {
	if got := gitEnvForEvidence([]string{"A=1", "GIT_CONFIG_GLOBAL=/tmp/x.gitconfig", "B=2"}); len(got) != 1 || got[0] != "GIT_CONFIG_GLOBAL=/tmp/x.gitconfig" {
		t.Fatalf("expected single entry, got %v", got)
	}
	// An empty-value entry must NOT be picked up (it would wipe a real
	// process-level config rather than add the helper).
	if got := gitEnvForEvidence([]string{"GIT_CONFIG_GLOBAL="}); got != nil {
		t.Fatalf("empty GIT_CONFIG_GLOBAL must be ignored, got %v", got)
	}
}

// Defect ⑯ regression (fu91es QA, 2026-10-06): the materialized git config's
// helper line is container-scoped (containerRuntimePath), so the runner-side
// ls-remote could not execute the helper even with GIT_CONFIG_GLOBAL in hand
// ("could not read Username ... No such device or address"). hostGateGitEnv
// must point GIT_CONFIG_GLOBAL at a host-usable variant whose helper is the
// sibling .credential-helper file that still exists at gate time.
func TestHostGateGitEnvRewritesContainerScopedHelper(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "materialized.gitconfig")
	hostHelper := cfg + ".credential-helper"
	// The helper script exists on the HOST (cleanup is deferred, has not run).
	if err := os.WriteFile(hostHelper, []byte("#!/bin/sh\ncase \"$1\" in get) ;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The config content references the helper by its CONTAINER path.
	if err := os.WriteFile(cfg, []byte("[credential]\n\thelper = /workspace/.multigent/tool-abc/git.gitconfig.credential-helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"GIT_CONFIG_GLOBAL=" + cfg}
	got := hostGateGitEnv(env)
	if len(got) != 1 {
		t.Fatalf("expected one env entry, got %v", got)
	}
	wantCfg := cfg + ".hostgate"
	if got[0] != "GIT_CONFIG_GLOBAL="+wantCfg {
		t.Fatalf("GIT_CONFIG_GLOBAL must point at the hostgate variant, got %q", got[0])
	}
	patched, err := os.ReadFile(wantCfg)
	if err != nil {
		t.Fatalf("hostgate config not written: %v", err)
	}
	if !strings.Contains(string(patched), "helper = "+hostHelper) {
		t.Fatalf("hostgate config must reference the host helper path, got: %s", patched)
	}
	original, _ := os.ReadFile(cfg)
	if strings.Contains(string(original), hostHelper) {
		t.Fatal("the container-facing config must stay untouched")
	}
}

func TestHostGateGitEnvPassthroughs(t *testing.T) {
	dir := t.TempDir()
	// No GIT_CONFIG_GLOBAL entry: returned unchanged.
	if got := hostGateGitEnv([]string{"FOO=bar"}); len(got) != 1 || got[0] != "FOO=bar" {
		t.Fatalf("non-git env must pass through, got %v", got)
	}
	// Config file missing (already cleaned up): unchanged, prior failure
	// behavior preserved.
	missing := filepath.Join(dir, "gone.gitconfig")
	env := []string{"GIT_CONFIG_GLOBAL=" + missing}
	if got := hostGateGitEnv(env); got[0] != env[0] {
		t.Fatalf("missing config must pass through, got %v", got)
	}
	// Helper path inside the config already valid on the host: unchanged.
	hostHelper := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(hostHelper, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "host.gitconfig")
	if err := os.WriteFile(cfg, []byte("[credential]\n\thelper = "+hostHelper+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env2 := []string{"GIT_CONFIG_GLOBAL=" + cfg}
	if got := hostGateGitEnv(env2); got[0] != env2[0] {
		t.Fatalf("host-usable helper must pass through untouched, got %v", got)
	}
	// Nil env: unchanged.
	if got := hostGateGitEnv(nil); got != nil {
		t.Fatalf("nil env must stay nil, got %v", got)
	}
}

// Defect ⑯ regression, part 2 (fu91es QA re-run, 2026-10-06): a MINIMAL
// credential env slice must not replace the process environment wholesale —
// git execs its git-remote-http transport helper via PATH, and a PATH-less
// env made ls-remote die with "'origin' does not appear to be a git
// repository" even though the credential config was fine. With the merge
// (os.Environ + caller entries), ls-remote over a bound origin succeeds.
func TestGitDeliveryEvidenceMinimalEnvKeepsPATH(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	run(origin, "init", "--bare", "-b", "main")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	run(wt, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(wt, "add", "-A")
	run(wt, "commit", "-m", "base")
	run(wt, "remote", "add", "origin", "file://"+origin)
	run(wt, "push", "origin", "main")
	run(wt, "branch", "feature/x")
	run(wt, "push", "origin", "feature/x")
	// A valid but empty credential config: the transport must still resolve
	// (PATH preserved) and the remote branch must be found.
	emptyCfg := filepath.Join(t.TempDir(), "empty.gitconfig")
	if err := os.WriteFile(emptyCfg, []byte("[credential]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, push, err := gitDeliveryEvidence(wt, "main", "feature/x", []string{"GIT_CONFIG_GLOBAL=" + emptyCfg})
	if err != nil {
		t.Fatalf("ls-remote must survive a minimal env slice: %v", err)
	}
	if !push.RemoteHasBranch || push.RemoteSHA == "" {
		t.Fatalf("remote branch evidence missing: %+v", push)
	}
}

// Defect ⑯, part 3: the connections-based evidence env must only inject a
// token when the connection's base-URL host matches the worktree origin's
// host (the exfiltration guard), and the env must carry the transient
// http.extraHeader (nothing on disk).
func TestGitlabEvidenceEnvHostGuard(t *testing.T) {
	if got := gitlabHeaderEvidenceEnv("tok"); len(got) != 3 || got[0] != "GIT_CONFIG_COUNT=1" ||
		got[1] != "GIT_CONFIG_KEY_0=http.extraHeader" || !strings.HasPrefix(got[2], "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
		t.Fatalf("unexpected transient env shape: %v", got)
	}
	if !sameGitHostPort("192.168.139.3:8083", "192.168.139.3:8083") {
		t.Fatal("identical host:port must match")
	}
	if sameGitHostPort("localhost:8083", "192.168.139.3:8083") {
		t.Fatal("different hosts must NOT match (exfiltration guard)")
	}
	if connectionBaseHost("") != "" {
		t.Fatal("empty base URL must yield empty host")
	}
	// A non-git directory yields no origin host (fail-closed, no injection).
	if gitOriginHost(t.TempDir()) != "" {
		t.Fatal("non-git worktree must yield empty origin host")
	}
}
