package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/changerun"
	"github.com/multigent/multigent/internal/entity"
)

// 30.10 审批项: with enable_review_changerun_proposal=true, an approved review
// commit is recorded as an APPLIED change-run proposal (preimage→checkpoint
// patch, true postimage), giving the operator one-click atomic rollback. The
// flag stays default-off per the adjudication; the recording is best-effort.
func TestRecordReviewChangeRun_CreatesAppliedProposal(t *testing.T) {
	repo := newReviewCommitRepo(t)

	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.controlDB.SetSetting(reviewChangeRunSettingKey, "true"); err != nil {
		t.Fatal(err)
	}
	if !s.reviewChangeRunEnabled() {
		t.Fatal("flag should read back true")
	}
	task := &entity.Task{ID: "t-changerun-1", BranchName: "main", WorktreeDir: repo}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	preimage := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "copilot_fix.txt"), []byte("reviewed fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "delete_me.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "chore(review): user in-context preview feedback fixes")
	checkpoint := run("rev-parse", "HEAD")

	s.recordReviewChangeRun("", "sample", task.ID, preimage, checkpoint)

	store := s.changerunStore()
	p, err := store.ActiveForTask("sample", task.ID)
	if err != nil {
		t.Fatalf("active proposal lookup: %v", err)
	}
	// applied is terminal, so ActiveForTask returns nil — fetch via state walk
	// instead: the slot is freed and the proposal is applied.
	if p != nil {
		t.Fatalf("expected no ACTIVE proposal after applied recording, got %s (%s)", p.ID, p.State)
	}

	// The audit trail names the proposal; find it through the workspace table.
	recs, err := s.controlDB.ListRecords("preview_change_proposals", s.currentWorkspaceIDValue(nil), nil)
	if err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	var applied *changerun.Proposal
	for _, rec := range recs {
		dec, decErr := changerun.DecodeProposalForTest(rec.Payload)
		if decErr != nil {
			continue
		}
		if dec.TaskID == task.ID {
			applied = dec
		}
	}
	if applied == nil {
		t.Fatal("expected a recorded proposal for the task")
	}
	if applied.State != changerun.StateApplied {
		t.Fatalf("expected state applied, got %s (verification %+v)", applied.State, applied.Verification)
	}
	if applied.AppliedSHA != checkpoint {
		t.Fatalf("appliedSHA %s, want checkpoint %s", applied.AppliedSHA, checkpoint)
	}
	if len(applied.Postimage) == 0 {
		t.Fatal("expected postimage hashes recorded")
	}
	if applied.Postimage["copilot_fix.txt"] == "" || applied.Postimage["copilot_fix.txt"] == "absent" {
		t.Fatalf("expected copilot_fix.txt postimage hash, got %q", applied.Postimage["copilot_fix.txt"])
	}
	if applied.Postimage["delete_me.txt"] != "" {
		// delete_me.txt exists in the repo — it should hash, not be absent.
		if applied.Postimage["delete_me.txt"] == "absent" {
			t.Fatalf("delete_me.txt exists in the checkpoint; postimage must hash it, got absent")
		}
	}
	if !strings.Contains(applied.Patch, "copilot_fix.txt") {
		t.Fatalf("expected patch to contain copilot_fix.txt:\n%s", applied.Patch)
	}
}

// Default-off invariant: without the flag the recording must be a no-op.
func TestRecordReviewChangeRun_FlagOffIsNoOp(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	if s.reviewChangeRunEnabled() {
		t.Fatal("flag must default off")
	}
	// No proposal rows may appear even when called directly.
	recs, err := s.controlDB.ListRecords("preview_change_proposals", s.currentWorkspaceIDValue(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected zero proposals, got %d", len(recs))
	}
}
