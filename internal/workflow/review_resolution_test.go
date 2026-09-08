package workflow

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestDetermineUXMode(t *testing.T) {
	// Case 1: All Evidence / locked -> zero_input
	p1 := []ApprovalParameterPreview{
		{Key: "commit_sha", Kind: KindEvidence, Resolution: ResolutionSystemLocked},
	}
	if mode := DetermineUXMode(p1); mode != "zero_input" {
		t.Fatalf("expected zero_input, got %q", mode)
	}

	// Case 2: Has Inherited Contract -> can_override
	p2 := []ApprovalParameterPreview{
		{Key: "commit_sha", Kind: KindEvidence, Resolution: ResolutionSystemLocked},
		{Key: "approved_scope", Kind: KindInheritedContract, Resolution: ResolutionInheritOrOverride},
	}
	if mode := DetermineUXMode(p2); mode != "can_override" {
		t.Fatalf("expected can_override, got %q", mode)
	}

	// Case 3: Has Human Required Decision -> decision_required
	p3 := []ApprovalParameterPreview{
		{Key: "commit_sha", Kind: KindEvidence, Resolution: ResolutionSystemLocked},
		{Key: "approved_scope", Kind: KindInheritedContract, Resolution: ResolutionInheritOrOverride},
		{Key: "target_env", Kind: KindHumanDecision, Resolution: ResolutionHumanRequired},
	}
	if mode := DetermineUXMode(p3); mode != "decision_required" {
		t.Fatalf("expected decision_required, got %q", mode)
	}
}

func TestComputeReviewSnapshotHash_Determinism(t *testing.T) {
	params := []ApprovalParameterPreview{
		{Key: "commit_sha", Kind: KindEvidence, CandidateValue: "a81b9dc", Source: "task.IntegratedCommit"},
		{Key: "approved_scope", Kind: KindInheritedContract, CandidateValue: "auth features", Source: "upstream_output.clarified"},
	}
	hash1 := ComputeReviewSnapshotHash("step-review", params, 1788700000000)
	hash2 := ComputeReviewSnapshotHash("step-review", params, 1788700000000)
	if hash1 != hash2 {
		t.Fatalf("hashes should be deterministic: %q != %q", hash1, hash2)
	}

	// Changing candidate value changes hash
	params[1].CandidateValue = "auth features and billing"
	hash3 := ComputeReviewSnapshotHash("step-review", params, 1788700000000)
	if hash1 == hash3 {
		t.Fatalf("hash should change when candidate value changes")
	}
}

func TestResolveApprovalOutputs_MergeAndTrace(t *testing.T) {
	preview := ReviewResolutionPreview{
		TaskID:               "task-123",
		StepID:               "code-review",
		ExpectedStateVersion: 100,
		ReviewSnapshotHash:   "sha256:test",
		Parameters: []ApprovalParameterPreview{
			{Key: "commit_sha", Kind: KindEvidence, CandidateValue: "a81b9dc", Source: "task.IntegratedCommit"},
			{Key: "approved_scope", Kind: KindInheritedContract, CandidateValue: "v1.0 API", Source: "upstream.scope"},
			{Key: "target_env", Kind: KindHumanDecision, Resolution: ResolutionHumanRequired, Required: true},
			{Key: "comments", Kind: KindHumanCommentary, Resolution: ResolutionHumanOptional},
		},
	}

	// 1. Missing required decision fails
	_, err := ResolveApprovalOutputs(preview, "approved", map[string]string{}, "user:admin")
	if err == nil {
		t.Fatal("expected error when required decision is missing")
	}

	// 2. Full resolution: evidence locked, contract inherited, decision supplied
	humanInputs := map[string]string{
		"commit_sha": "fake_sha_attempt_to_override",
		"target_env": "production",
		"comments":   "looks great",
	}
	snapshot, err := ResolveApprovalOutputs(preview, "approved", humanInputs, "user:admin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Evidence must NOT be overridden by humanInputs
	if snapshot.ResolvedOutputs["commit_sha"] != "a81b9dc" {
		t.Fatalf("evidence must remain locked, got %q", snapshot.ResolvedOutputs["commit_sha"])
	}
	if snapshot.ResolutionTrace["commit_sha"].Mode != "system_locked" {
		t.Fatalf("expected system_locked, got %v", snapshot.ResolutionTrace["commit_sha"])
	}

	// Inherited contract without override must be "inherit"
	if snapshot.ResolvedOutputs["approved_scope"] != "v1.0 API" {
		t.Fatalf("expected inherited value, got %q", snapshot.ResolvedOutputs["approved_scope"])
	}
	if snapshot.ResolutionTrace["approved_scope"].Mode != "inherit" {
		t.Fatalf("expected inherit mode, got %v", snapshot.ResolutionTrace["approved_scope"])
	}

	// Human decision must be recorded
	if snapshot.ResolvedOutputs["target_env"] != "production" {
		t.Fatalf("expected production, got %q", snapshot.ResolvedOutputs["target_env"])
	}
	if snapshot.ResolutionTrace["target_env"].Mode != "human_input" {
		t.Fatalf("expected human_input, got %v", snapshot.ResolutionTrace["target_env"])
	}

	// 3. Inherited contract with explicit override
	humanInputs["approved_scope"] = "v1.0 API with extensions"
	snapshot2, err := ResolveApprovalOutputs(preview, "approved", humanInputs, "user:admin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snapshot2.ResolvedOutputs["approved_scope"] != "v1.0 API with extensions" {
		t.Fatalf("expected override value, got %q", snapshot2.ResolvedOutputs["approved_scope"])
	}
	if snapshot2.ResolutionTrace["approved_scope"].Mode != "override" {
		t.Fatalf("expected override mode, got %v", snapshot2.ResolutionTrace["approved_scope"])
	}

	// 4. Reject requires comments
	_, err = ResolveApprovalOutputs(preview, "rejected", map[string]string{}, "user:admin")
	if err == nil {
		t.Fatal("expected error when reject without comments")
	}
	rejectSnap, err := ResolveApprovalOutputs(preview, "rejected", map[string]string{"comments": "need fix"}, "user:admin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rejectSnap.Decision != "rejected" || rejectSnap.ResolvedOutputs["comments"] != "need fix" {
		t.Fatalf("reject outputs unexpected: %+v", rejectSnap)
	}
}

func TestValidateReviewCAS(t *testing.T) {
	// Matching passes
	if err := ValidateReviewCAS(100, 100, "sha256:abc", "sha256:abc"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// Version mismatch
	if err := ValidateReviewCAS(101, 100, "sha256:abc", "sha256:abc"); err != ErrReviewStaleVersion {
		t.Fatalf("expected ErrReviewStaleVersion, got %v", err)
	}

	// Hash mismatch
	if err := ValidateReviewCAS(100, 100, "sha256:abc", "sha256:def"); err != ErrReviewStaleHash {
		t.Fatalf("expected ErrReviewStaleHash, got %v", err)
	}
}

func TestClassifyParameter_FieldMetadataAndApprovedChange(t *testing.T) {
	ctx := &reviewResolutionContext{
		task: &entity.Task{
			BranchName: "feat/issue-42",
		},
		steps: []entity.WorkflowStepInstance{
			{
				StepID: "step-1",
				Status: "completed",
				OutputValues: map[string]string{
					"commit_sha": "c0ffee1234567890",
					"pr":         "https://gitlab.example.com/proj/merge_requests/42",
				},
			},
		},
		instance: entity.WorkflowStepInstance{},
	}

	// 1. approved_change should default to KindInheritedContract, candidate from upstream pr / task branch, overrideable
	f1 := entity.WorkflowField{
		Name: "approved_change",
	}
	p1 := classifyParameter(ctx, f1)
	if p1.Kind != KindInheritedContract {
		t.Fatalf("expected approved_change to be KindInheritedContract, got %s", p1.Kind)
	}
	if p1.Resolution != ResolutionInheritOrOverride {
		t.Fatalf("expected ResolutionInheritOrOverride, got %s", p1.Resolution)
	}
	if p1.CandidateValue != "https://gitlab.example.com/proj/merge_requests/42" {
		t.Fatalf("expected candidate from upstream pr, got %q", p1.CandidateValue)
	}

	// 2. Explicit ReviewKind and Overrideable metadata
	lockedTrue := false
	f2 := entity.WorkflowField{
		Name:         "approved_change",
		ReviewKind:   "inherited_contract",
		Overrideable: &lockedTrue, // locked!
	}
	p2 := classifyParameter(ctx, f2)
	if p2.Resolution != ResolutionSystemLocked {
		t.Fatalf("expected ResolutionSystemLocked when Overrideable=false, got %s", p2.Resolution)
	}

	// 3. Evidence overrideable
	overrideTrue := true
	f3 := entity.WorkflowField{
		Name:         "commit_sha",
		ReviewKind:   "evidence",
		Overrideable: &overrideTrue,
	}
	p3 := classifyParameter(ctx, f3)
	if p3.Kind != KindEvidence || p3.Resolution != ResolutionInheritOrOverride {
		t.Fatalf("expected KindEvidence + ResolutionInheritOrOverride, got %s / %s", p3.Kind, p3.Resolution)
	}
}

