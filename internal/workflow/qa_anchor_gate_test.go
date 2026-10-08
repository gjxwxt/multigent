package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
)

// Defect ⑮ regression (fu91es QA, 2026-10-06): a QA step that runs ON TOP of
// a recorded delivery must measure the QA agent's own delta against the
// delivery anchor (delivery_sha), not the task-creation baseline. Against the
// creation baseline the whole delivery counts as the QA delta, and Direction
// 1 (declare every changed path) × Direction 3 (test artifacts only) becomes
// mutually unsatisfiable — the honest fu91es QA run (74/80 matrix green,
// evidence pushed) could never complete.

// newDeliveryWorktree extends the base fixture with a recorded delivery:
// base commit (task creation, baseline captured) -> delivery commit. The QA
// agent's delta lands after this function returns.
func newDeliveryWorktree(t *testing.T) (string, string) {
	t.Helper()
	wt := newGitWorktree(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+wt)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	// The implementation step's delivery: business code + a test file.
	if err := os.WriteFile(filepath.Join(wt, "delivered.go"), []byte("package main\n\nfunc Delivered() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestDelivered() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "delivery")
	out, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return wt, strings.TrimSpace(string(out))
}

func TestQAGateOnDeliveryAnchor(t *testing.T) {
	store, taskID := newGreenfieldRunStore(t)
	wt, deliverySHA := newDeliveryWorktree(t)
	store.WorktreeResolver = func(project, taskID string) string { return wt }
	const project = "project"

	// Walk to the qa step exactly like the acceptance-spec e2e (linear path),
	// after the v7 preflight head pair.
	drivePastPreflight(t, store, project, taskID)
	if _, err := store.CompleteAndAdvance(project, taskID, "drafted", "", map[string]string{
		"requirement_draft": "Structured requirement",
		"open_questions":    "none",
	}, "completed"); err != nil {
		t.Fatalf("req draft: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "approved", "", map[string]string{
		"decision":             "approve",
		"comments":             "looks right",
		"approved_requirement": "Approved requirement text",
	}, "completed"); err != nil {
		t.Fatalf("req review: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "design ok", "", map[string]string{
		"decision": "approve",
		"comments": "ship it",
	}, "completed"); err != nil {
		t.Fatalf("design review: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "linear", "", map[string]string{
		"scale_verdict": "linear",
	}, "completed"); err != nil {
		t.Fatalf("scale gate: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "spec done", "", map[string]string{
		"test_spec_doc":      "full spec",
		"test_spec_manifest": gfManifest,
		"test_spec_summary":  "2 cases",
	}, "completed"); err != nil {
		t.Fatalf("acceptance test design: %v", err)
	}
	// Implementation records the machine-verified delivery anchor.
	if _, err := store.CompleteAndAdvance(project, taskID, "impl", "", map[string]string{
		"pr":                           "diff summary",
		"tests_run":                    "go test ./...",
		"risks":                        "none",
		"test_implementation_evidence": `{"AUTH-001":{"test":"T","file":"a_test.go","result":"pass"}}`,
		"delivery_sha":                 deliverySHA,
		"delivery_branch":              "feature/test",
	}, "completed"); err != nil {
		t.Fatalf("implementation: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "self review", "", map[string]string{
		"self_review_verdict": "pass",
		"review_comments":     "clean",
	}, "completed"); err != nil {
		t.Fatalf("self review: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "cr ok", "", map[string]string{
		"decision":        "approve",
		"comments":        "lgtm",
		"approved_change": "approved diff",
	}, "completed"); err != nil {
		t.Fatalf("code review: %v", err)
	}

	// Round 1 — the QA agent ALSO edits a business file on top of the
	// delivery: the whitelist must still catch it (the anchor relaxes the
	// measurement surface, never the role rule).
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n\nfunc QAhacked() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := store.CompleteAndAdvance(project, taskID, "qa", "", map[string]string{
		"risk_coverage_matrix": `[{"item_id":"A1","status":"passed","evidence":"ok"}]`,
		"test_report":          "all green",
		"touched_paths":        "docs/qa/qa-evidence.md",
	}, "completed")
	if err == nil || !strings.Contains(err.Error(), "server.go") {
		t.Fatalf("business edit on top of the delivery must still be rejected, got: %v", err)
	}

	// Round 2 — honest QA delta only: revert the business file, add the
	// evidence file (the fu91es shape: evidence committed on top of the
	// delivery). The anchor surface measures exactly that delta and the step
	// completes — before the fix this was structurally impossible.
	if err := os.WriteFile(filepath.Join(wt, "server.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "docs", "qa"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "docs", "qa", "qa-evidence.md"), []byte("evidence\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The delivery commit is still an ancestor of HEAD and the evidence is
	// uncommitted — verify the anchor resolves against the working tree.
	if _, err := gitworktree.WorktreeDeltaSinceCommit(wt, deliverySHA); err != nil {
		t.Fatalf("anchor must resolve in the worktree: %v", err)
	}
	if _, err := store.CompleteAndAdvance(project, taskID, "qa done", "", map[string]string{
		"risk_coverage_matrix": `[{"item_id":"A1","status":"passed","evidence":"ok"}]`,
		"test_report":          "all green",
		"touched_paths":        "docs/qa/qa-evidence.md",
	}, "completed"); err != nil {
		t.Fatalf("honest QA delta must complete against the delivery anchor: %v", err)
	}
	inst := gfStepInstance(t, store, taskID, "qa")
	if inst.Status != "completed" {
		t.Fatalf("qa step must be completed, got %s", inst.Status)
	}
}

func TestLatestDeliveryAnchorPicksMostRecentFinish(t *testing.T) {
	// ListStepInstances orders by step ID alphabetically; recency must come
	// from FinishedAt, never slice position. "code_review" sorts FIRST.
	mk := func(stepID, sha string, at time.Time) entity.WorkflowStepInstance {
		return entity.WorkflowStepInstance{
			ID: "wfs-" + stepID, RunID: "wfr-t", StepID: stepID, Status: "completed",
			FinishedAt: at,
			OutputValues: map[string]string{
				"delivery_sha": sha,
			},
		}
	}
	base := time.Now()
	early := mk("code_review", "1111111111111111111111111111111111111111", base.Add(-time.Hour))
	late := mk("implementation", "2222222222222222222222222222222222222222", base)
	stale := mk("implementation", "3333333333333333333333333333333333333333", base.Add(-2*time.Hour))
	got := latestDeliveryAnchor([]entity.WorkflowStepInstance{early, stale, late})
	if got != "2222222222222222222222222222222222222222" {
		t.Fatalf("most recently finished delivery_sha must win, got %q", got)
	}
	if latestDeliveryAnchor(nil) != "" {
		t.Fatal("no instances -> no anchor")
	}
	garbage := mk("implementation", "none", base.Add(time.Hour))
	if latestDeliveryAnchor([]entity.WorkflowStepInstance{garbage}) != "" {
		t.Fatal("non-SHA placeholder must be ignored")
	}
}
