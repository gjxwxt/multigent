package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// Draft prefixes and sources are part of the UI contract: the dialog renders
// the marker badge and lets the reviewer regenerate or discard the draft.
const reviewDraftMarker = "[草稿,请审核修改]"
const reviewDraftMarkerEn = "[draft, please review and edit]"

// draftSource labels where a prefill value came from, so the reviewer can
// judge trust without re-tracing the pipeline.
const (
	draftSourceUpstreamOutput = "upstream_output" // structured $output from a completed step
	draftSourceTaskCommit     = "task_commit"     // Integrated/RemoteSync/Completion commit on the task
	draftSourceSummary        = "summary"         // rule-assembled key facts from step summaries
	draftSourceNone           = "none"            // nothing extractable; field left empty on purpose
)

// shaPattern matches a git short/full SHA and nothing else. Draft rules must
// never emit a branch name or a string scraped from free text: a report may
// cite a rollback target or any other SHA, so only structured sources pass.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

type workflowReviewDraft struct {
	Fields []workflowReviewDraftField `json:"fields"`
}

type workflowReviewDraftField struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	SourceLabel string `json:"sourceLabel"`
	Generated   bool   `json:"generated"`
}

type reviewDraftContext struct {
	run      entity.WorkflowRun
	def      entity.WorkflowDefinition
	steps    []entity.WorkflowStepInstance
	events   []entity.WorkflowStepEvent
	step     entity.WorkflowStep
	instance entity.WorkflowStepInstance
	task     *entity.Task
}

// handleGetTaskWorkflowReviewDraft returns rule-computed prefill values for
// the active human_review step's output fields. Rules are table-driven and
// deterministic: no LLM, no regex over free text. A field with nothing
// extractable is returned with source "none" and an empty value — the dialog
// leaves it empty rather than guessing.
func (s *Server) handleGetTaskWorkflowReviewDraft(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	task, _, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, runFound, err := wfStore.RunForTask(project, taskID)
	if err != nil || !runFound {
		s.jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	def, defFound, err := wfStore.RunDefinition(run)
	if err != nil || !defFound {
		s.jsonError(w, http.StatusNotFound, "workflow definition not found")
		return
	}
	steps, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	events, _ := wfStore.ListStepEvents(run.ID)

	instance, instOK := workflowStepInstanceByStepID(steps, run.ActiveStepID)
	if !instOK || instance.Status == "completed" {
		s.jsonError(w, http.StatusConflict, "no active review step to draft for")
		return
	}
	step, stepOK := workflowDefinitionStepByID(def.Steps, run.ActiveStepID)
	if !stepOK || step.Type != "human_review" {
		s.jsonError(w, http.StatusConflict, "active step is not a human review")
		return
	}

	ctx := &reviewDraftContext{
		run: run, def: def, steps: steps, events: events,
		step: step, instance: instance, task: task,
	}
	out := workflowReviewDraft{Fields: make([]workflowReviewDraftField, 0, len(step.OutputFields))}
	for _, f := range step.OutputFields {
		name := strings.TrimSpace(f.Name)
		if name == "" || name == "decision" {
			continue // decision is always human's to make
		}
		value, source := reviewDraftForField(ctx, name)
		out.Fields = append(out.Fields, workflowReviewDraftField{
			Name:        name,
			Value:       value,
			Source:      source,
			SourceLabel: draftSourceLabel(source),
			Generated:   source != draftSourceNone && strings.TrimSpace(value) != "",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// backfillOptionalReviewOutputs fills empty optional human_review output
// fields from the same deterministic draft rules the dialog prefills with,
// so a reviewer can confirm a gate without retyping machine-known values
// (branch+SHA carried from upstream, merged SHA...). A field the rules
// cannot derive stays empty and still fails validation: the human must
// supply it. Runs before summary/WillComplete so downstream edges see the
// backfilled contract values.
func backfillOptionalReviewOutputs(wfStore *workflowstore.Store, run entity.WorkflowRun, def entity.WorkflowDefinition, step entity.WorkflowStep, task *entity.Task, outputs map[string]string) {
	needs := false
	for _, f := range step.OutputFields {
		if f.Optional && strings.TrimSpace(outputs[f.Name]) == "" {
			needs = true
			break
		}
	}
	if !needs {
		return
	}
	steps, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return
	}
	instance, instOK := workflowStepInstanceByStepID(steps, run.ActiveStepID)
	if !instOK {
		return
	}
	events, _ := wfStore.ListStepEvents(run.ID)
	ctx := &reviewDraftContext{run: run, def: def, steps: steps, events: events, step: step, instance: instance, task: task}
	for _, f := range step.OutputFields {
		name := strings.TrimSpace(f.Name)
		if !f.Optional || name == "" || strings.TrimSpace(outputs[name]) != "" {
			continue
		}
		if value, _ := reviewDraftForField(ctx, name); strings.TrimSpace(value) != "" {
			outputs[name] = value
		}
	}
}

// reviewDraftForField dispatches on the output field name. The table covers
// the gate fields that exist across built-in templates; unknown fields fall
// through to the generic upstream-output lookup.
func reviewDraftForField(ctx *reviewDraftContext, field string) (string, string) {
	switch field {
	case "release_candidate", "candidate", "merged_sha", "git_tag":
		return draftImmutableRef(ctx)
	case "approved_change":
		return draftApprovedChange(ctx)
	case "approved_scope":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		// The requirement gate receives the clarification result as its
		// input and has no same-named value: the scope under approval IS the
		// clarification, so passing it through lets a plain "approve" with
		// the optional contract left empty carry the clarified scope forward
		// untouched.
		return draftFromUpstreamOutput(ctx, "clarified")
	case "approved_requirement":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		// The requirement gate produces the draft it is approving: there is
		// no same-named upstream value, so an approve left empty must carry
		// requirement_draft forward — otherwise the mapped input on the next
		// agent step silently arrives blank.
		return draftFromUpstreamOutput(ctx, "requirement_draft")
	case "approved_prd":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		return draftFromUpstreamOutput(ctx, "prd")
	case "approved_technical_spec":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		return draftFromUpstreamOutput(ctx, "technical_spec")
	case "qa_evidence":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		return draftFromUpstreamOutput(ctx, "fix_artifact")
	case "ship_candidate":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		// The QA gate reviews the QA report, but the ship candidate is the
		// build artifact produced upstream: pass it through so a plain
		// "approve" names what is actually being shipped.
		if v, ok := latestCompletedOutput(ctx, "implementation_artifact", func(s string) bool { return len(s) > 0 }); ok {
			return v, draftSourceUpstreamOutput
		}
		return "", draftSourceNone
	case "approved_fix":
		if v, source := draftFromUpstreamOutput(ctx, field); strings.TrimSpace(v) != "" {
			return v, source
		}
		// The hotfix plan gate receives the triage diagnosis as its input,
		// not a same-named value: the plan under approval IS the diagnosis
		// (root cause + fix approach + frozen base_commit), so passing it
		// through lets a plain "approve" carry the agent's proposal forward.
		return draftFromUpstreamOutput(ctx, "diagnosis")
	case "comments":
		return draftComments(ctx)
	default:
		return draftFromUpstreamOutput(ctx, field)
	}
}

// draftImmutableRef resolves the shipped-artifact reference: the completed
// merge/verify step's structured SHA/tag output wins, then task-level commit
// fields. Everything must pass the SHA/tag sanity check; a value that does
// not is treated as absent rather than guessed.
func draftImmutableRef(ctx *reviewDraftContext) (string, string) {
	for _, key := range []string{"merged_sha", "git_tag", "release_candidate", "candidate"} {
		if v, ok := latestCompletedOutput(ctx, key, structuredRefOK); ok {
			return v, draftSourceUpstreamOutput
		}
	}
	for _, key := range []string{"IntegratedCommit", "RemoteSyncCommit", "CompletionCommit"} {
		v := strings.TrimSpace(taskCommitValue(ctx.task, key))
		if structuredRefOK(v) {
			return v, draftSourceTaskCommit
		}
	}
	return "", draftSourceNone
}

// draftApprovedChange names the reviewed code artifact: branch + SHA from the
// most recent implementation/PR step outputs, annotated when the review-time
// auto-collect will produce the final commit.
func draftApprovedChange(ctx *reviewDraftContext) (string, string) {
	if v, ok := latestCompletedOutput(ctx, "implementation", func(s string) bool { return len(s) > 0 }); ok {
		return v, draftSourceUpstreamOutput
	}
	if v, ok := latestCompletedOutput(ctx, "fix_artifact", func(s string) bool { return len(s) > 0 }); ok {
		return v, draftSourceUpstreamOutput
	}
	// Legacy delivery templates name the implementation output "pr".
	if v, ok := latestCompletedOutput(ctx, "pr", func(s string) bool { return len(s) > 0 }); ok {
		return v, draftSourceUpstreamOutput
	}
	if branch := strings.TrimSpace(ctx.task.BranchName); branch != "" {
		if base := strings.TrimSpace(ctx.task.BaseCommit); base != "" && structuredRefOK(base) {
			return "分支 " + branch + "（基线 " + shortSHA(base) + "）；审核通过时未提交改动将自动收编为 checkpoint commit", draftSourceSummary
		}
		return "分支 " + branch, draftSourceSummary
	}
	return "", draftSourceNone
}

// draftFromUpstreamOutput passes through the same-named (or mapping-declared)
// value the step received as input — the "already decided upstream, just
// confirm it" case, e.g. approved_scope flowing into later gates.
func draftFromUpstreamOutput(ctx *reviewDraftContext, field string) (string, string) {
	if v := strings.TrimSpace(ctx.instance.InputValues[field]); v != "" {
		return v, draftSourceUpstreamOutput
	}
	return "", draftSourceNone
}

// draftComments assembles a starting point from verifiable facts only: what
// completed, key structured numbers, and the pending gate's own inputs. It is
// explicitly a checklist scaffold, never a verdict — the marker prefix forces
// the reviewer to edit before submitting.
func draftComments(ctx *reviewDraftContext) (string, string) {
	var b strings.Builder
	completed := 0
	var names []string
	for _, ev := range ctx.events {
		if ev.Status != "completed" {
			continue
		}
		completed++
		for _, st := range ctx.def.Steps {
			if st.ID == ev.StepID {
				names = append(names, st.Title)
				break
			}
		}
	}
	if completed == 0 {
		return "", draftSourceNone
	}
	marker := reviewDraftMarker
	if workflowTemplateLocaleOf(ctx.def) == "en" {
		marker = reviewDraftMarkerEn
	}
	b.WriteString(marker + "\n")
	if len(names) > 0 {
		b.WriteString("已完成步骤: " + strings.Join(uniqueStrings(names), " → ") + "。\n")
	}
	if ref, ok := latestCompletedOutput(ctx, "merged_sha", structuredRefOK); ok {
		b.WriteString("合并提交: " + shortSHA(ref) + "。\n")
	} else if commit := strings.TrimSpace(taskCommitValue(ctx.task, "IntegratedCommit")); structuredRefOK(commit) {
		b.WriteString("集成提交: " + shortSHA(commit) + "。\n")
	}
	if br := strings.TrimSpace(ctx.task.BranchName); br != "" {
		b.WriteString("交付分支: " + br + "。\n")
	}
	b.WriteString("裁决要点: <通过/打回的理由,请补充>")
	return b.String(), draftSourceSummary
}

// latestCompletedOutput scans step events newest-first for a completed
// instance's structured output passing accept.
func latestCompletedOutput(ctx *reviewDraftContext, key string, accept func(string) bool) (string, bool) {
	for i := len(ctx.events) - 1; i >= 0; i-- {
		ev := ctx.events[i]
		if ev.Status != "completed" {
			continue
		}
		v := strings.TrimSpace(ev.OutputValues[key])
		if v != "" && accept(v) {
			return v, true
		}
	}
	// Fall back to live step instances (events may be sparse on older runs).
	for i := len(ctx.steps) - 1; i >= 0; i-- {
		inst := ctx.steps[i]
		if inst.Status != "completed" {
			continue
		}
		v := strings.TrimSpace(inst.OutputValues[key])
		if v != "" && accept(v) {
			return v, true
		}
	}
	return "", false
}

// structuredRefOK admits plain SHAs and pushed tag names (v-prefixed semver
// style) while rejecting anything that smells like a moving ref (branch
// names, URLs, "origin/...", free text).
func structuredRefOK(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 120 {
		return false
	}
	if strings.ContainsAny(v, " \t\n/:") && !strings.HasPrefix(v, "refs/tags/") {
		return false
	}
	v = strings.TrimPrefix(v, "refs/tags/")
	if shaPattern.MatchString(v) {
		return true
	}
	if strings.HasPrefix(v, "v") {
		rest := strings.TrimPrefix(v, "v")
		dots := strings.Count(rest, ".")
		if dots >= 1 && dots <= 2 && !strings.ContainsAny(rest, " -_") {
			parts := strings.Split(rest, ".")
			okLen := true
			for _, p := range parts {
				if p == "" || len(p) > 4 {
					okLen = false
					break
				}
				for _, ch := range p {
					if ch < '0' || ch > '9' {
						okLen = false
						break
					}
				}
				if !okLen {
					break
				}
			}
			if okLen {
				return true
			}
		}
	}
	return false
}

func shortSHA(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

func taskCommitValue(t *entity.Task, field string) string {
	if t == nil {
		return ""
	}
	switch field {
	case "IntegratedCommit":
		return t.IntegratedCommit
	case "RemoteSyncCommit":
		return t.RemoteSyncCommit
	case "CompletionCommit":
		return t.CompletionCommit
	}
	return ""
}

func draftSourceLabel(source string) string {
	switch source {
	case draftSourceUpstreamOutput:
		return "来自上游结构化输出"
	case draftSourceTaskCommit:
		return "来自任务提交记录"
	case draftSourceSummary:
		return "按规则拼接,需人工补充"
	default:
		return "无可提取内容"
	}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func workflowTemplateLocaleOf(def entity.WorkflowDefinition) string {
	// No locale field on WorkflowDefinition; step titles are the cheapest
	// reliable signal (built-in templates localize titles per locale).
	for _, st := range def.Steps {
		for _, r := range st.Title {
			if r >= 0x4E00 && r <= 0x9FFF {
				return "zh"
			}
		}
	}
	return "en"
}
