package workflow

// C2 (2026-09 VM acceptance): the linear QA touched_paths gate rejected an
// HONEST completion whose declared paths exactly matched the agent's staged
// test files, spinning QA for ~25 minutes. Two line-level root causes:
//
//  1. The gate hard-coded the ABSOLUTE git-status surface (legacy
//     qaBaselineSurface{}), so platform scaffolding that predates the task
//     (.cursor/, .mcp.json, node_modules/) showed up as "undeclared worktree
//     changes" even though the control-plane baseline proves it was never
//     the agent's delta.
//  2. worktreeChangedPaths excluded .multigent/ runtime files only under the
//     "??" (untracked) status; an agent that runs `git add -A` stages them,
//     and "A .multigent/..." leaked into the real-delta set.
//
// These tests pin both fixes plus the boundary matrix (staged new file,
// unstaged modification, deletion, space-in-path) and the security
// invariants that must survive: business-file laundering still fails, the
// lost/tampered-baseline fail-closed contract still fires.

import (
	"os"
	"path"
	"strings"
	"testing"
)

// captureBaselineWithScaffolding mirrors the production materialization
// sequence: platform scaffold noise exists BEFORE the baseline capture, so
// the trusted control-plane document proves it predates the QA agent.
func captureBaselineWithScaffolding(t *testing.T, project, taskID string) (*Store, string) {
	t.Helper()
	store := newBaselineStore(t)
	wt := newGitWorktree(t)
	for _, p := range []string{".mcp.json", ".cursor/settings.json"} {
		if err := os.MkdirAll(path.Join(wt, path.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path.Join(wt, p), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	captureBaselineForTask(t, store, project, taskID, wt)
	return store, wt
}

// TestLinearQAGateHonestStagedTestFilePasses is the direct reproduction of
// the VM false rejection: a trusted baseline exists, platform scaffolding
// predates it, the QA agent stages a NEW test file and declares exactly it —
// the linear QA gate must pass. It failed before the fix (the gate measured
// the absolute status surface and reported .cursor/.mcp.json as undeclared).
func TestLinearQAGateHonestStagedTestFilePasses(t *testing.T) {
	store, wt := captureBaselineWithScaffolding(t, "proj", "task-c2-staged")

	if err := os.WriteFile(path.Join(wt, "extra_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitCommand(t, wt, "add", "extra_test.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}

	// Drive the gate exactly as store.go's linear qa checkpoint does.
	surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "task-c2-staged")
	if err != nil {
		t.Fatal(err)
	}
	if surf.baseline == nil {
		t.Fatal("control-plane baseline must be found for the linear gate surface")
	}
	if err := verifyWorktreeDeltaAgainstDeclaration("extra_test.go", wt, surf); err != nil {
		t.Fatalf("honest staged test file with pre-task scaffolding must pass the linear QA gate: %v", err)
	}
}

// TestLinearQAGateStagedMultigentRuntimeFilesExcluded: `git add -A` stages
// the platform's .multigent/ runtime files; they must never count as the
// agent's delta on either surface.
func TestLinearQAGateStagedMultigentRuntimeFilesExcluded(t *testing.T) {
	wt := newGitWorktree(t) // fixture writes .multigent/qa_baseline.json
	if out, err := gitCommand(t, wt, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add -A: %v (%s)", err, out)
	}
	paths, err := worktreeChangedPaths(wt)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, ".multigent") {
			t.Errorf("staged .multigent path %q must not leak into the real-delta set", p)
		}
	}
	// The agent's own test edit is still mandatory to declare.
	if err := os.WriteFile(path.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyQATouchedPathsAgainstWorktree("none", wt); err == nil || !strings.Contains(err.Error(), "server_test.go") {
		t.Fatalf("staged runtime files excluded, but an undeclared real change must still fail, got: %v", err)
	}
}

// TestLinearQAGateBoundaryMatrix pins the declaration contract across the
// git states the QA agent produces (staged new file, unstaged modification,
// deletion, space-in-path) on the baseline surface the linear gate now uses.
func TestLinearQAGateBoundaryMatrix(t *testing.T) {
	t.Run("staged new test file", func(t *testing.T) {
		store, wt := captureBaselineWithScaffolding(t, "proj", "bm-staged")
		if err := os.WriteFile(path.Join(wt, "extra_test.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := gitCommand(t, wt, "add", "extra_test.go").CombinedOutput(); err != nil {
			t.Fatalf("git add: %v (%s)", err, out)
		}
		surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "bm-staged")
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyWorktreeDeltaAgainstDeclaration("extra_test.go", wt, surf); err != nil {
			t.Errorf("staged new test file must pass: %v", err)
		}
		err = verifyWorktreeDeltaAgainstDeclaration("none", wt, surf)
		if err == nil || !strings.Contains(err.Error(), "extra_test.go") {
			t.Errorf("staged new test file must stay mandatory to declare, got: %v", err)
		}
	})

	t.Run("unstaged modification", func(t *testing.T) {
		store, wt := captureBaselineWithScaffolding(t, "proj", "bm-unstaged")
		if err := os.WriteFile(path.Join(wt, "server_test.go"), []byte("package main\n\nfunc TestX() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "bm-unstaged")
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyWorktreeDeltaAgainstDeclaration("server_test.go", wt, surf); err != nil {
			t.Errorf("unstaged test modification declared must pass: %v", err)
		}
		if err := verifyWorktreeDeltaAgainstDeclaration("none", wt, surf); err == nil {
			t.Error("unstaged modification with none must fail")
		}
	})

	t.Run("deletion", func(t *testing.T) {
		store, wt := captureBaselineWithScaffolding(t, "proj", "bm-delete")
		if err := os.Remove(path.Join(wt, "server_test.go")); err != nil {
			t.Fatal(err)
		}
		surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "bm-delete")
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyWorktreeDeltaAgainstDeclaration("server_test.go", wt, surf); err != nil {
			t.Errorf("deletion declared must pass: %v", err)
		}
	})

	t.Run("space in path", func(t *testing.T) {
		store, wt := captureBaselineWithScaffolding(t, "proj", "bm-space")
		if err := os.WriteFile(path.Join(wt, "sp ace_test.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := gitCommand(t, wt, "add", "sp ace_test.go").CombinedOutput(); err != nil {
			t.Fatalf("git add: %v (%s)", err, out)
		}
		surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "bm-space")
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyWorktreeDeltaAgainstDeclaration("sp ace_test.go", wt, surf); err != nil {
			t.Errorf("space-in-path declared raw must pass: %v", err)
		}
	})
}

// TestLinearQAGateKeepsWhitelistAndFailClosed: the surface switch must not
// weaken the security invariants — a business file in the delivery delta
// still fails the test-artifact whitelist even when honestly declared, and
// a lost control-plane baseline still fails closed.
func TestLinearQAGateKeepsWhitelistAndFailClosed(t *testing.T) {
	store, wt := captureBaselineWithScaffolding(t, "proj", "bm-guard")
	if err := os.WriteFile(path.Join(wt, "server.go"), []byte("package main\n\nfunc Bad() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	surf, err := resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "bm-guard")
	if err != nil {
		t.Fatal(err)
	}
	// deliveryDelta stays false on the linear QA checkpoint: Direction 3
	// (test-artifact whitelist) must fire on the business file.
	if surf.deliveryDelta {
		t.Fatal("linear QA surface must not be a delivery checkpoint")
	}
	err = verifyWorktreeDeltaAgainstDeclaration("server.go", wt, surf)
	if err == nil || !strings.Contains(err.Error(), "test-artifact") {
		t.Fatalf("honestly-declared business change must still fail the whitelist: %v", err)
	}

	// Lost baseline: fail closed, never degrade silently.
	if err := store.DeleteQABaselineRecord("proj", "bm-guard"); err != nil {
		t.Fatal(err)
	}
	surf, err = resolveQABaselineSurface(wt, store.QABaselineLookupAdapter(), "proj", "bm-guard")
	if err == nil {
		t.Fatal("lost baseline must fail closed at surface resolution")
	}
	if surf.baseline != nil {
		t.Fatal("no baseline surface may be used after a lost-baseline error")
	}
}

// TestLinearQAGateNilLookupManifestStillFailsClosed pins the S2-2 contract
// the surface switch must inherit unchanged: a store built WITHOUT a
// control-plane lookup that completes a worktree whose manifest PROVES a
// baseline existed must fail closed (ErrQABaselineLost), never silently
// degrade to the absolute surface.
func TestLinearQAGateNilLookupManifestStillFailsClosed(t *testing.T) {
	linStore := newLinearQAGateStore(t) // QABaselineLookup deliberately nil
	wt := newGitWorktree(t)
	linStore.WorktreeResolver = func(project, taskID string) string { return wt }
	if err := linStore.CaptureQABaselineRecord("project", "task-1", wt); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, err := linStore.CompleteAndAdvance("project", "task-1", "qa done", "", map[string]string{
		"touched_paths": "qa_probe_test.go",
	}, "completed"); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("nil-lookup manifest-present completion must fail closed, got: %v", err)
	}
}

// TestLegacySurfaceStillStrict pins the legacy absolute surface (the
// pre-baseline helper) so its documented degradation stays explicit: on it,
// scaffold noise still fails an honest completion. That strictness is WHY
// the baseline surface exists.
func TestLegacySurfaceStillStrict(t *testing.T) {
	_, wt := captureBaselineWithScaffolding(t, "proj", "bm-legacy")
	if err := os.WriteFile(path.Join(wt, "extra_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitCommand(t, wt, "add", "extra_test.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	err := verifyQATouchedPathsAgainstWorktree("extra_test.go", wt)
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("legacy absolute surface must keep flagging pre-task scaffolding, got: %v", err)
	}
}
