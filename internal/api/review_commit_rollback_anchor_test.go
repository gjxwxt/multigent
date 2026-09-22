package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// Step-1 收编 (antigravity directive): a review commit whose push fails must
// leave a STRUCTURED rollback anchor on the task — preimage SHA → checkpoint
// SHA plus the failure — so an operator can surgically revert or replay the
// push without git-log archaeology. The anchor rides the existing failure
// comment (no new notification channel).
func TestReviewCommitPushFailureRecordsRollbackAnchor(t *testing.T) {
	repo := newReviewCommitRepo(t)
	// No remote → remote get-url fails → the push block is skipped entirely.
	// To hit the push-FAILURE path we need a remote whose push cannot succeed:
	// a bare repo fetched, then made read-only by pointing origin at a
	// nonexistent path (get-url succeeds, push fails).
	bare := filepath.Join(t.TempDir(), "origin.git")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	run("init", "--bare", bare)
	run("remote", "add", "origin", bare)
	// Point origin at a path that stops existing → push fails deterministically.
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	s, _ := newConnectionGrantPolicyServer(t)
	task := &entity.Task{ID: "t-anchor", BranchName: "main", WorktreeDir: repo}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "copilot_fix.txt"), []byte("reviewed fix"), 0644); err != nil {
		t.Fatal(err)
	}

	preImage := func() string {
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = repo
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	base := preImage()

	_, _, _ = s.commitAndPushReviewChanges("sample", "pm", task)

	// The commit DID land locally.
	after := preImage()
	if after == base {
		t.Fatal("precondition: checkpoint commit must land locally even when push fails")
	}
	// And the failure comment carries the anchor pair.
	comments, err := s.ts.ListComments("sample", "pm", "t-anchor")
	if err != nil || len(comments) == 0 {
		t.Fatalf("expected failure comment with anchor, err=%v n=%d", err, len(comments))
	}
	var anchor string
	for _, c := range comments {
		if strings.Contains(c.Body, "回滚锚点") {
			anchor = c.Body
		}
	}
	if anchor == "" {
		t.Fatalf("no rollback anchor in comments")
	}
	if !strings.Contains(anchor, base[:7]) {
		t.Fatalf("anchor must name the preimage SHA %s, got: %s", base[:7], anchor)
	}
	if !strings.Contains(anchor, after[:7]) {
		t.Fatalf("anchor must name the checkpoint SHA %s, got: %s", after[:7], anchor)
	}
	if !strings.Contains(anchor, "git reset --hard") {
		t.Fatalf("anchor must carry the revert instruction, got: %s", anchor)
	}
}
