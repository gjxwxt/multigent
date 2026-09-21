package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// Characterization tests for the step-ID/title fuzzy matchers that Task 0.1
// (PlatformGate registry convergence) plans to delete. These tests lock the
// CURRENT behavior — including its quirks — so the registry migration cannot
// silently drift. Each group mirrors one matcher:
//
//	isPullRequestReviewStep  workflow_handlers.go (title substring guessing)
//	isDesignGateStep         workflow_handlers.go (config flag + ID fallback)
//	isQASignoffStep          workflow_handlers.go (ID substring)
//
// When the PlatformGate registry lands, update these to assert the registry
// resolves the same inputs, then delete the matchers and this file's
// matcher-specific cases.

func TestCharacterizationIsPullRequestReviewStep(t *testing.T) {
	cases := []struct {
		name string
		step entity.WorkflowStep
		want bool
	}{
		{name: "exact id pr_review", step: entity.WorkflowStep{ID: "pr_review"}, want: true},
		{name: "exact id mr_review", step: entity.WorkflowStep{ID: "mr_review"}, want: true},
		{name: "exact id merge_and_sync", step: entity.WorkflowStep{ID: "merge_and_sync"}, want: true},
		{name: "exact id push_to_gitlab", step: entity.WorkflowStep{ID: "push_to_gitlab"}, want: true},
		{name: "id contains pr_review suffix", step: entity.WorkflowStep{ID: "auto_pr_review_v2"}, want: true},
		{name: "id contains merge_and_sync prefix", step: entity.WorkflowStep{ID: "merge_and_sync_release"}, want: true},
		{name: "title substring pull request", step: entity.WorkflowStep{ID: "totally_unrelated", Title: "Open a Pull Request for review"}, want: true},
		{name: "title substring merge request", step: entity.WorkflowStep{ID: "totally_unrelated", Title: "创建 Merge Request"}, want: true},
		{name: "title substring merge and sync", step: entity.WorkflowStep{ID: "totally_unrelated", Title: "Merge and Sync to main"}, want: true},
		{name: "title case insensitive", step: entity.WorkflowStep{ID: "x", Title: "PULL REQUEST gate"}, want: true},
		// The known quirk this matcher was flagged for in the reduction plan:
		// renaming a step title can flip the match. Lock the flip side too.
		{name: "renamed title no longer matches", step: entity.WorkflowStep{ID: "pr_review", Title: "代码合入与远端同步"}, want: true}, // still matches via id
		{name: "unrelated id and renamed title", step: entity.WorkflowStep{ID: "deliver", Title: "代码合入与远端同步"}, want: false},
		{name: "create_pr id is NOT a review step", step: entity.WorkflowStep{ID: "create_pr"}, want: false},
		{name: "empty step", step: entity.WorkflowStep{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPullRequestReviewStep(tc.step); got != tc.want {
				t.Fatalf("isPullRequestReviewStep(%+v) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}

func TestCharacterizationIsDesignGateStep(t *testing.T) {
	cases := []struct {
		name string
		step entity.WorkflowStep
		want bool
	}{
		{name: "config designGate true", step: entity.WorkflowStep{ID: "anything", Config: map[string]string{"designGate": "true"}}, want: true},
		{name: "config designGate TRUE case-insensitive", step: entity.WorkflowStep{ID: "anything", Config: map[string]string{"designGate": "TRUE"}}, want: true},
		{name: "config designGate false does not match", step: entity.WorkflowStep{ID: "anything", Config: map[string]string{"designGate": "false"}}, want: false},
		{name: "config designGate empty string does not match", step: entity.WorkflowStep{ID: "anything", Config: map[string]string{"designGate": ""}}, want: false},
		{name: "exact id design_review", step: entity.WorkflowStep{ID: "design_review"}, want: true},
		{name: "id contains design_review", step: entity.WorkflowStep{ID: "greenfield_design_review"}, want: true},
		{name: "renamed id without marker does not match", step: entity.WorkflowStep{ID: "design_signoff"}, want: false},
		{name: "config marker survives rename", step: entity.WorkflowStep{ID: "design_signoff", Config: map[string]string{"designGate": "true"}}, want: true},
		{name: "empty step", step: entity.WorkflowStep{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDesignGateStep(tc.step); got != tc.want {
				t.Fatalf("isDesignGateStep(%+v) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}

func TestCharacterizationIsQASignoffStep(t *testing.T) {
	cases := []struct {
		name string
		step entity.WorkflowStep
		want bool
	}{
		{name: "exact id qa_signoff", step: entity.WorkflowStep{ID: "qa_signoff"}, want: true},
		{name: "id contains qa_signoff", step: entity.WorkflowStep{ID: "qa_signoff_v2"}, want: true},
		{name: "qa step without signoff does not match", step: entity.WorkflowStep{ID: "qa"}, want: false},
		{name: "unrelated id", step: entity.WorkflowStep{ID: "release"}, want: false},
		{name: "empty step", step: entity.WorkflowStep{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQASignoffStep(tc.step); got != tc.want {
				t.Fatalf("isQASignoffStep(%+v) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}
