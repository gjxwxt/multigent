package api

import (
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestStructuredRefOK(t *testing.T) {
	positive := []string{
		"92573d15a47976feed3742844735e24fb2a953ca", // full SHA
		"92573d1", // short SHA
		"92573D1", // uppercase tolerated and normalized by caller
		"v0.2.0",  // pushed tag
		"v1.20.3", // multi-digit tag
		"refs/tags/v0.2.0",
	}
	negative := []string{
		"",                                  // empty
		"main",                              // branch name must never pass
		"origin/main",                       // moving ref with slash
		"feat/devpulse",                     // branch with slash
		"92573d1 (main HEAD, merged for x)", // free text citation
		"http://localhost:8083/root/1test",  // URL
		"v",                                 // not a tag
		"v0.2.0-rc1",                        // pre-release suffix rejected for safety
		"92573d15a47976feed3742844735e24fb2a953caextra", // 41 hex chars = too long
	}
	for _, v := range positive {
		if !structuredRefOK(v) {
			t.Errorf("structuredRefOK(%q) = false, want true", v)
		}
	}
	for _, v := range negative {
		if structuredRefOK(v) {
			t.Errorf("structuredRefOK(%q) = true, want false", v)
		}
	}
}

func draftTestContext() *reviewDraftContext {
	def := entity.WorkflowDefinition{
		Name: "统一交付流水线",
		Steps: []entity.WorkflowStep{
			{ID: "clarify_review", Type: "human_review", Title: "需求快审", OutputFields: []entity.WorkflowField{{Name: "decision"}, {Name: "comments"}, {Name: "approved_scope"}}},
			{ID: "code_review", Type: "human_review", Title: "人工代码审核", OutputFields: []entity.WorkflowField{{Name: "decision"}, {Name: "comments"}, {Name: "approved_change"}}},
			{ID: "qa_signoff", Type: "human_review", Title: "QA 准出", OutputFields: []entity.WorkflowField{{Name: "decision"}, {Name: "comments"}, {Name: "release_candidate"}}},
		},
	}
	events := []entity.WorkflowStepEvent{
		{StepID: "clarify_review", Status: "completed", OutputValues: map[string]string{"approved_scope": "范围A; 验收标准B"}},
		{StepID: "code_review", Status: "completed", OutputValues: map[string]string{"approved_change": "分支 feat/x, 提交 abc123def"}},
		{StepID: "merge_sync", Status: "completed", OutputValues: map[string]string{"merged_sha": "92573d15a47976feed3742844735e24fb2a953ca"}},
	}
	// Note: newest-first iteration means later entries win; merge_sync's
	// merged_sha is the latest structured ref.
	return &reviewDraftContext{
		def:    def,
		events: events,
		task:   &entity.Task{BranchName: "feat/devpulse"},
	}
}

func TestDraftImmutableRefPrefersUpstreamSHAOverBranch(t *testing.T) {
	ctx := draftTestContext()
	v, src := draftImmutableRef(ctx)
	if !structuredRefOK(v) || src != draftSourceUpstreamOutput {
		t.Fatalf("draftImmutableRef = %q/%q, want structured SHA from upstream", v, src)
	}
}

func TestDraftImmutableRefFallsBackToTaskCommit(t *testing.T) {
	ctx := draftTestContext()
	ctx.events = nil
	ctx.task.IntegratedCommit = "abc123def4567"
	v, src := draftImmutableRef(ctx)
	if v != "abc123def4567" || src != draftSourceTaskCommit {
		t.Fatalf("draftImmutableRef = %q/%q, want task commit fallback", v, src)
	}
}

func TestDraftImmutableRefNeverEmitsBranchName(t *testing.T) {
	ctx := draftTestContext()
	ctx.events = nil
	ctx.task.IntegratedCommit = ""
	v, src := draftImmutableRef(ctx)
	if v != "" || src != draftSourceNone {
		t.Fatalf("draftImmutableRef = %q/%q, want empty/none when no structured ref exists (branch name must not be invented)", v, src)
	}
}

func TestDraftCommentsHasMarkerAndFacts(t *testing.T) {
	ctx := draftTestContext()
	ctx.instance = entity.WorkflowStepInstance{StepID: "qa_signoff"}
	v, src := draftComments(ctx)
	if src != draftSourceSummary {
		t.Fatalf("source = %q, want summary", src)
	}
	if !strings.HasPrefix(v, reviewDraftMarker) {
		t.Fatalf("comments draft must carry the edit-forced marker, got: %q", v)
	}
	if !strings.Contains(v, "92573d15a479") {
		t.Fatalf("comments draft should cite the merged SHA, got: %q", v)
	}
	if strings.Contains(v, "http") {
		t.Fatalf("comments draft must not scrape URLs, got: %q", v)
	}
}

func TestDraftFieldDecisionNeverPrefilled(t *testing.T) {
	// The handler-level filter skips decision; guard the rule here so the
	// invariant survives refactors of reviewDraftForField.
	ctx := draftTestContext()
	v, _ := reviewDraftForField(ctx, "decision")
	if v != "" {
		t.Fatalf("decision must never be drafted, got %q", v)
	}
}

func TestDraftFromUpstreamOutput(t *testing.T) {
	ctx := draftTestContext()
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"approved_scope": "冻结的范围文本"}}
	v, src := draftFromUpstreamOutput(ctx, "approved_scope")
	if v != "冻结的范围文本" || src != draftSourceUpstreamOutput {
		t.Fatalf("draftFromUpstreamOutput = %q/%q", v, src)
	}
	v2, src2 := draftFromUpstreamOutput(ctx, "approved_scope_missing")
	if v2 != "" || src2 != draftSourceNone {
		t.Fatalf("missing upstream must be empty/none, got %q/%q", v2, src2)
	}
}

func TestDraftApprovedScopeFallsBackToClarified(t *testing.T) {
	ctx := draftTestContext()
	// Requirement gate: the step input carries the clarification result and
	// no same-named approved_scope — the draft passes the clarification
	// through so a plain "approve" (optional field left empty) forwards the
	// clarified scope untouched.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"clarified": "范围A; 验收标准B; 非目标C"}}
	v, src := reviewDraftForField(ctx, "approved_scope")
	if v != "范围A; 验收标准B; 非目标C" || src != draftSourceUpstreamOutput {
		t.Fatalf("approved_scope draft = %q/%q, want clarified passthrough", v, src)
	}
	// A same-named upstream value still wins over the fallback.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"approved_scope": "人工改判的范围", "clarified": "旧澄清"}}
	v2, _ := reviewDraftForField(ctx, "approved_scope")
	if v2 != "人工改判的范围" {
		t.Fatalf("same-named upstream must win, got %q", v2)
	}
	// Neither present: empty, never fabricated.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{}}
	v3, src3 := reviewDraftForField(ctx, "approved_scope")
	if v3 != "" || src3 != draftSourceNone {
		t.Fatalf("missing both must be empty/none, got %q/%q", v3, src3)
	}
}

func TestDraftPassThroughGateFieldsFallBackToGateInput(t *testing.T) {
	// Every "approved X" gate field with no same-named upstream falls back
	// to the artifact actually under review at that gate, so approve-as-is
	// forwards it untouched (same pattern as approved_scope -> clarified).
	cases := []struct {
		field, inputKey, value string
	}{
		{"approved_prd", "prd", "PRD 草稿文本"},
		{"approved_technical_spec", "technical_spec", "技术方案文本"},
		{"qa_evidence", "fix_artifact", "修复产物文本"},
	}
	for _, c := range cases {
		ctx := draftTestContext()
		ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{c.inputKey: c.value}}
		v, src := reviewDraftForField(ctx, c.field)
		if v != c.value || src != draftSourceUpstreamOutput {
			t.Errorf("%s draft = %q/%q, want %q passthrough from %s", c.field, v, src, c.value, c.inputKey)
		}
	}
}

func TestDraftShipCandidateFromBuildArtifact(t *testing.T) {
	ctx := draftTestContext()
	// The QA gate's own input is the QA report; the ship candidate is the
	// build artifact produced upstream — the draft names what ships.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"qa_report_doc_id": "doc-20260831-aaa111"}}
	ctx.events = []entity.WorkflowStepEvent{
		{StepID: "complete_slice", Status: "completed", OutputValues: map[string]string{"implementation_artifact": "PR #42: 完整切片实现"}},
	}
	v, src := reviewDraftForField(ctx, "ship_candidate")
	if v != "PR #42: 完整切片实现" || src != draftSourceUpstreamOutput {
		t.Fatalf("ship_candidate draft = %q/%q, want build artifact passthrough", v, src)
	}
	// No build artifact anywhere: empty, never fabricated.
	ctx.events = nil
	v2, src2 := reviewDraftForField(ctx, "ship_candidate")
	if v2 != "" || src2 != draftSourceNone {
		t.Fatalf("missing build artifact must be empty/none, got %q/%q", v2, src2)
	}
}

func TestDraftApprovedFixFallsBackToDiagnosis(t *testing.T) {
	ctx := draftTestContext()
	// Hotfix plan gate: the step input carries the triage diagnosis and no
	// same-named approved_fix — the draft passes the diagnosis through so a
	// plain "approve" forwards the agent's proposal untouched.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"diagnosis": "根因X；修复方案Y；base_commit=abc1234"}}
	v, src := reviewDraftForField(ctx, "approved_fix")
	if v != "根因X；修复方案Y；base_commit=abc1234" || src != draftSourceUpstreamOutput {
		t.Fatalf("approved_fix draft = %q/%q, want diagnosis passthrough", v, src)
	}
	// A same-named upstream value still wins over the fallback.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"approved_fix": "人工改判的方案", "diagnosis": "旧诊断"}}
	v2, _ := reviewDraftForField(ctx, "approved_fix")
	if v2 != "人工改判的方案" {
		t.Fatalf("same-named upstream must win, got %q", v2)
	}
	// Neither present: empty, never fabricated.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{}}
	v3, src3 := reviewDraftForField(ctx, "approved_fix")
	if v3 != "" || src3 != draftSourceNone {
		t.Fatalf("missing both must be empty/none, got %q/%q", v3, src3)
	}
}

func TestDraftApprovedRequirementFallsBackToDraft(t *testing.T) {
	ctx := draftTestContext()
	// Greenfield requirement gate: the step's own input IS requirement_draft
	// and there is no same-named approved_requirement — a plain "approve"
	// must carry the draft forward instead of mapping an empty value into
	// the implementation step's approved_requirement input.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"requirement_draft": "问题P；目标G；范围S；非目标N"}}
	v, src := reviewDraftForField(ctx, "approved_requirement")
	if v != "问题P；目标G；范围S；非目标N" || src != draftSourceUpstreamOutput {
		t.Fatalf("approved_requirement draft = %q/%q, want requirement_draft passthrough", v, src)
	}
	// A human-edited same-named value still wins over the fallback.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{"approved_requirement": "人工修订后的需求", "requirement_draft": "原始草稿"}}
	v2, _ := reviewDraftForField(ctx, "approved_requirement")
	if v2 != "人工修订后的需求" {
		t.Fatalf("same-named upstream must win, got %q", v2)
	}
	// Neither present: empty, never fabricated.
	ctx.instance = entity.WorkflowStepInstance{InputValues: map[string]string{}}
	v3, src3 := reviewDraftForField(ctx, "approved_requirement")
	if v3 != "" || src3 != draftSourceNone {
		t.Fatalf("missing both must be empty/none, got %q/%q", v3, src3)
	}
}
