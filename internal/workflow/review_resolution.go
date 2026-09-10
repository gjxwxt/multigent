package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

var (
	ErrReviewStaleVersion = errors.New("review stale: state version mismatch")
	ErrReviewStaleHash    = errors.New("review stale: snapshot hash mismatch")
)

var shaRegex = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
var semverTagRegex = regexp.MustCompile(`^v[0-9]+(\.[0-9]+)*(-[0-9A-Za-z.-]+)?$`)

func structuredRefOK(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return shaRegex.MatchString(s) || semverTagRegex.MatchString(s)
}

type ParameterKind string

const (
	KindInheritedContract ParameterKind = "inherited_contract"
	KindEvidence          ParameterKind = "evidence"
	KindHumanDecision     ParameterKind = "human_decision"
	KindHumanCommentary   ParameterKind = "human_commentary"
)

type ParameterResolution string

const (
	ResolutionInheritOrOverride ParameterResolution = "inherit_or_override"
	ResolutionSystemLocked      ParameterResolution = "system_locked"
	ResolutionHumanRequired     ParameterResolution = "human_required"
	ResolutionHumanOptional     ParameterResolution = "human_optional"
)

type ApprovalParameterPreview struct {
	Key            string              `json:"key"`
	Label          string              `json:"label"`
	Kind           ParameterKind       `json:"kind"`
	Resolution     ParameterResolution `json:"resolution"`
	CandidateValue string              `json:"candidateValue"`
	Source         string              `json:"source"`
	Required       bool                `json:"required"`
}

type ReviewResolutionPreview struct {
	TaskID               string                     `json:"taskId"`
	StepID               string                     `json:"stepId"`
	StepTitle            string                     `json:"stepTitle"`
	ExpectedStateVersion int64                      `json:"expectedStateVersion"`
	ReviewSnapshotHash   string                     `json:"reviewSnapshotHash"`
	Parameters           []ApprovalParameterPreview `json:"parameters"`
	UXMode               string                     `json:"uxMode"` // "zero_input" | "can_override" | "decision_required"
}

type ResolutionTraceItem struct {
	Mode   string `json:"mode"` // "inherit" | "system_locked" | "override" | "human_input"
	Source string `json:"source"`
	Actor  string `json:"actor,omitempty"`
}

type ResolvedApprovalSnapshot struct {
	Decision           string                         `json:"decision"`
	ReviewSnapshotHash string                         `json:"reviewSnapshotHash"`
	ResolvedOutputs    map[string]string              `json:"resolvedOutputs"`
	ResolutionTrace    map[string]ResolutionTraceItem `json:"resolutionTrace"`
}

type reviewResolutionContext struct {
	run      entity.WorkflowRun
	def      entity.WorkflowDefinition
	steps    []entity.WorkflowStepInstance
	events   []entity.WorkflowStepEvent
	step     entity.WorkflowStep
	instance entity.WorkflowStepInstance
	task     *entity.Task
}

// ComputeReviewSnapshotHash produces a deterministic SHA256 over canonical parameter representations.
func ComputeReviewSnapshotHash(stepID string, params []ApprovalParameterPreview, stateVer int64) string {
	type hashCanonicalParam struct {
		Key   string `json:"key"`
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Src   string `json:"src"`
	}
	canonicalList := make([]hashCanonicalParam, len(params))
	for i, p := range params {
		canonicalList[i] = hashCanonicalParam{
			Key:   p.Key,
			Kind:  string(p.Kind),
			Value: strings.TrimSpace(p.CandidateValue),
			Src:   p.Source,
		}
	}
	sort.Slice(canonicalList, func(i, j int) bool {
		return canonicalList[i].Key < canonicalList[j].Key
	})
	payload, _ := json.Marshal(struct {
		StepID   string               `json:"stepId"`
		StateVer int64                `json:"stateVer"`
		Params   []hashCanonicalParam `json:"params"`
	}{
		StepID:   stepID,
		StateVer: stateVer,
		Params:   canonicalList,
	})
	h := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(h[:])
}

// DetermineUXMode classifies the UI interaction needed for Mattermost or other client surfaces.
func DetermineUXMode(params []ApprovalParameterPreview) string {
	hasHumanRequired := false
	hasInherited := false
	for _, p := range params {
		if p.Resolution == ResolutionHumanRequired {
			hasHumanRequired = true
		} else if p.Resolution == ResolutionInheritOrOverride {
			hasInherited = true
		}
	}
	if hasHumanRequired {
		return "decision_required"
	}
	if hasInherited {
		return "can_override"
	}
	return "zero_input"
}

// GetReviewResolutionPreview builds a canonical preview of all review parameters for a step.
func (s *Store) GetReviewResolutionPreview(project string, task *entity.Task, stepID string) (ReviewResolutionPreview, error) {
	if task == nil {
		return ReviewResolutionPreview{}, errors.New("task is nil")
	}
	run, found, err := s.RunForTask(project, task.ID)
	if err != nil || !found {
		return ReviewResolutionPreview{}, fmt.Errorf("workflow run not found for task: %w", err)
	}
	def, found, err := s.RunDefinition(run)
	if err != nil || !found {
		return ReviewResolutionPreview{}, fmt.Errorf("workflow definition not found: %w", err)
	}
	steps, err := s.ListStepInstances(run.ID)
	if err != nil {
		return ReviewResolutionPreview{}, err
	}
	events, _ := s.ListStepEvents(run.ID)

	targetStepID := strings.TrimSpace(stepID)
	if targetStepID == "" {
		targetStepID = run.ActiveStepID
	}

	var currentStep entity.WorkflowStep
	var currentInst entity.WorkflowStepInstance
	for _, st := range def.Steps {
		if st.ID == targetStepID {
			currentStep = st
			break
		}
	}
	if currentStep.ID == "" {
		return ReviewResolutionPreview{}, fmt.Errorf("step %q not found in workflow definition", targetStepID)
	}
	for _, inst := range steps {
		if inst.StepID == targetStepID {
			currentInst = inst
			break
		}
	}

	ctx := &reviewResolutionContext{
		run:      run,
		def:      def,
		steps:    steps,
		events:   events,
		step:     currentStep,
		instance: currentInst,
		task:     task,
	}

	params := make([]ApprovalParameterPreview, 0, len(currentStep.OutputFields))
	for _, f := range currentStep.OutputFields {
		name := strings.TrimSpace(f.Name)
		if name == "" || name == "decision" {
			continue // Decision is the action verb, not a payload parameter
		}
		param := classifyParameter(ctx, f)
		params = append(params, param)
	}

	stateVersion := task.UpdatedAt.UnixMilli()
	if stateVersion <= 0 {
		stateVersion = task.CreatedAt.UnixMilli()
	}

	snapshotHash := ComputeReviewSnapshotHash(currentStep.ID, params, stateVersion)
	uxMode := DetermineUXMode(params)

	return ReviewResolutionPreview{
		TaskID:               task.ID,
		StepID:               currentStep.ID,
		StepTitle:            currentStep.Title,
		ExpectedStateVersion: stateVersion,
		ReviewSnapshotHash:   snapshotHash,
		Parameters:           params,
		UXMode:               uxMode,
	}, nil
}

func classifyParameter(ctx *reviewResolutionContext, field entity.WorkflowField) ApprovalParameterPreview {
	name := strings.TrimSpace(field.Name)
	label := name
	if field.Description != "" {
		label = field.Description
	}

	// 0. Explicit ReviewKind on field definition takes precedence
	if field.ReviewKind != "" {
		switch ParameterKind(field.ReviewKind) {
		case KindEvidence:
			val, src := resolveEvidenceRef(ctx, name)
			res := ResolutionSystemLocked
			if field.Overrideable != nil && *field.Overrideable {
				res = ResolutionInheritOrOverride
			}
			return ApprovalParameterPreview{
				Key:            name,
				Label:          label,
				Kind:           KindEvidence,
				Resolution:     res,
				CandidateValue: val,
				Source:         src,
				Required:       !field.Optional,
			}
		case KindInheritedContract:
			val, src := resolveInheritedCandidate(ctx, name)
			if val == "" && name == "approved_change" {
				val, src = resolveApprovedChange(ctx)
			}
			res := ResolutionInheritOrOverride
			if field.Overrideable != nil && !*field.Overrideable {
				res = ResolutionSystemLocked
			}
			return ApprovalParameterPreview{
				Key:            name,
				Label:          label,
				Kind:           KindInheritedContract,
				Resolution:     res,
				CandidateValue: val,
				Source:         src,
				Required:       !field.Optional,
			}
		case KindHumanCommentary:
			return ApprovalParameterPreview{
				Key:            name,
				Label:          label,
				Kind:           KindHumanCommentary,
				Resolution:     ResolutionHumanOptional,
				CandidateValue: "",
				Source:         "human",
				Required:       !field.Optional,
			}
		case KindHumanDecision:
			res := ResolutionHumanRequired
			if field.Optional {
				res = ResolutionHumanOptional
			}
			return ApprovalParameterPreview{
				Key:            name,
				Label:          label,
				Kind:           KindHumanDecision,
				Resolution:     res,
				CandidateValue: "",
				Source:         "human_decision",
				Required:       !field.Optional,
			}
		}
	}

	// 1. Evidence (System Facts): Immutable git SHAs, commit references, test reports
	switch name {
	case "commit_sha", "merged_sha", "git_tag", "release_candidate", "candidate", "test_report_sha":
		val, src := resolveEvidenceRef(ctx, name)
		res := ResolutionSystemLocked
		if field.Overrideable != nil && *field.Overrideable {
			res = ResolutionInheritOrOverride
		}
		return ApprovalParameterPreview{
			Key:            name,
			Label:          label,
			Kind:           KindEvidence,
			Resolution:     res,
			CandidateValue: val,
			Source:         src,
			Required:       !field.Optional,
		}
	}

	// 2. Human Commentary: Comments, notes, feedback
	switch name {
	case "comments", "review_notes", "rejection_reason", "feedback":
		res := ResolutionHumanOptional
		return ApprovalParameterPreview{
			Key:            name,
			Label:          label,
			Kind:           KindHumanCommentary,
			Resolution:     res,
			CandidateValue: "",
			Source:         "human",
			Required:       !field.Optional,
		}
	}

	// 3. Inherited Contract: Pre-existing drafts or candidate specs produced by upstream steps
	// approved_change is prefilled from upstream PR/branch and overrideable per store.go:162
	if name == "approved_change" {
		val, src := resolveApprovedChange(ctx)
		res := ResolutionInheritOrOverride
		if field.Overrideable != nil && !*field.Overrideable {
			res = ResolutionSystemLocked
		}
		return ApprovalParameterPreview{
			Key:            name,
			Label:          label,
			Kind:           KindInheritedContract,
			Resolution:     res,
			CandidateValue: val,
			Source:         src,
			Required:       !field.Optional,
		}
	}

	candidateVal, source := resolveInheritedCandidate(ctx, name)
	if candidateVal != "" {
		res := ResolutionInheritOrOverride
		if field.Overrideable != nil && !*field.Overrideable {
			res = ResolutionSystemLocked
		}
		return ApprovalParameterPreview{
			Key:            name,
			Label:          label,
			Kind:           KindInheritedContract,
			Resolution:     res,
			CandidateValue: candidateVal,
			Source:         source,
			Required:       !field.Optional,
		}
	}

	// 4. Human Decision: Fields requiring active selection/input (e.g. target_environment, skip_canary)
	res := ResolutionHumanRequired
	if field.Optional {
		res = ResolutionHumanOptional
	}
	return ApprovalParameterPreview{
		Key:            name,
		Label:          label,
		Kind:           KindHumanDecision,
		Resolution:     res,
		CandidateValue: "",
		Source:         "human_decision",
		Required:       !field.Optional,
	}
}

func resolveEvidenceRef(ctx *reviewResolutionContext, field string) (string, string) {
	for _, key := range []string{"merged_sha", "git_tag", "release_candidate", "candidate", "commit_sha"} {
		if v, ok := latestCompletedOutput(ctx, key, structuredRefOK); ok {
			return v, "upstream_output." + key
		}
	}
	if ctx.task != nil {
		for _, commitField := range []struct {
			name string
			val  string
		}{
			{"IntegratedCommit", ctx.task.IntegratedCommit},
			{"RemoteSyncCommit", ctx.task.RemoteSyncCommit},
			{"CompletionCommit", ctx.task.CompletionCommit},
			{"BaseCommit", ctx.task.BaseCommit},
		} {
			v := strings.TrimSpace(commitField.val)
			if structuredRefOK(v) {
				return v, "task." + commitField.name
			}
		}
	}
	return "", "none"
}

func resolveApprovedChange(ctx *reviewResolutionContext) (string, string) {
	for _, key := range []string{"implementation", "fix_artifact", "pr"} {
		if v, ok := latestCompletedOutput(ctx, key, func(s string) bool { return len(strings.TrimSpace(s)) > 0 }); ok {
			return v, "upstream_output." + key
		}
	}
	if ctx.task != nil && strings.TrimSpace(ctx.task.BranchName) != "" {
		return "branch:" + ctx.task.BranchName, "task.BranchName"
	}
	return "", "none"
}

func resolveInheritedCandidate(ctx *reviewResolutionContext, field string) (string, string) {
	mapping := map[string][]string{
		"approved_requirement":    {"requirement_draft", "requirement", "spec"},
		"approved_scope":          {"clarified", "scope", "clarified_scope"},
		"approved_prd":            {"prd", "product_requirement_doc"},
		"approved_technical_spec": {"technical_spec", "tech_spec", "architecture_spec"},
		"approved_fix":            {"diagnosis", "fix_plan", "triage"},
		"qa_evidence":             {"fix_artifact", "qa_report", "test_report"},
		"ship_candidate":          {"implementation_artifact", "artifact"},
	}
	if keys, ok := mapping[field]; ok {
		for _, k := range keys {
			if v, found := latestCompletedOutput(ctx, k, func(s string) bool { return len(strings.TrimSpace(s)) > 0 }); found {
				return v, "upstream_output." + k
			}
			if ctx.instance.InputValues != nil {
				if v, ok := ctx.instance.InputValues[k]; ok && len(strings.TrimSpace(v)) > 0 {
					return strings.TrimSpace(v), "step_input." + k
				}
			}
		}
	}
	// Fallback to same-named output from upstream or step input
	if v, found := latestCompletedOutput(ctx, field, func(s string) bool { return len(strings.TrimSpace(s)) > 0 }); found {
		return v, "upstream_output." + field
	}
	if ctx.instance.InputValues != nil {
		if v, ok := ctx.instance.InputValues[field]; ok && len(strings.TrimSpace(v)) > 0 {
			return strings.TrimSpace(v), "step_input." + field
		}
	}
	return "", ""
}

func latestCompletedOutput(ctx *reviewResolutionContext, key string, check func(string) bool) (string, bool) {
	for i := len(ctx.steps) - 1; i >= 0; i-- {
		st := ctx.steps[i]
		if st.Status != "completed" {
			continue
		}
		if val, ok := st.OutputValues[key]; ok {
			val = strings.TrimSpace(val)
			if check == nil || check(val) {
				return val, true
			}
		}
	}
	return "", false
}

func isRejectionDecision(d string) bool {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "reject", "rejected", "request_changes", "needs_revision", "needs_changes", "rework", "changes_requested":
		return true
	default:
		return false
	}
}

func isApprovalDecision(d string) bool {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "approve", "approved", "pass", "passed", "ok", "yes", "approve_push", "approve_local":
		return true
	default:
		return false
	}
}

// ResolveApprovalOutputs merges HumanInputs with system Evidence and Inherited Candidates into a complete ResolvedApprovalSnapshot.
func ResolveApprovalOutputs(preview ReviewResolutionPreview, decision string, humanInputs map[string]string, actor string) (ResolvedApprovalSnapshot, error) {
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision == "" {
		decision = "approved"
	}

	outputs := make(map[string]string)
	trace := make(map[string]ResolutionTraceItem)

	// Handle reject / request changes
	if isRejectionDecision(decision) {
		comments := strings.TrimSpace(humanInputs["comments"])
		if comments == "" {
			comments = strings.TrimSpace(humanInputs["rejection_reason"])
		}
		if comments == "" {
			return ResolvedApprovalSnapshot{}, errors.New("rejection requires comments")
		}
		outputs["decision"] = "request_changes"
		outputs["comments"] = comments
		trace["comments"] = ResolutionTraceItem{
			Mode:   "human_input",
			Source: "dialog.rejection_reason",
			Actor:  actor,
		}
		return ResolvedApprovalSnapshot{
			Decision:           decision,
			ReviewSnapshotHash: preview.ReviewSnapshotHash,
			ResolvedOutputs:    outputs,
			ResolutionTrace:    trace,
		}, nil
	}

	if isApprovalDecision(decision) {
		outputs["decision"] = "approve"
	} else {
		outputs["decision"] = decision
	}

	// Handle approve
	for _, p := range preview.Parameters {
		switch p.Kind {
		case KindEvidence:
			if p.Resolution == ResolutionInheritOrOverride {
				userVal, hasUser := humanInputs[p.Key]
				userVal = strings.TrimSpace(userVal)
				if hasUser && userVal != "" && userVal != p.CandidateValue {
					outputs[p.Key] = userVal
					trace[p.Key] = ResolutionTraceItem{
						Mode:   "override",
						Source: "human_override",
						Actor:  actor,
					}
					continue
				}
			}
			// Evidence cannot be modified by human input when locked; always system locked
			outputs[p.Key] = p.CandidateValue
			trace[p.Key] = ResolutionTraceItem{
				Mode:   "system_locked",
				Source: p.Source,
			}
		case KindInheritedContract:
			if p.Resolution == ResolutionSystemLocked {
				outputs[p.Key] = p.CandidateValue
				trace[p.Key] = ResolutionTraceItem{
					Mode:   "system_locked",
					Source: p.Source,
				}
				continue
			}
			userVal, hasUser := humanInputs[p.Key]
			userVal = strings.TrimSpace(userVal)
			if hasUser && userVal != "" && userVal != p.CandidateValue {
				// User explicitly overridden the contract
				outputs[p.Key] = userVal
				trace[p.Key] = ResolutionTraceItem{
					Mode:   "override",
					Source: "human_override",
					Actor:  actor,
				}
			} else {
				// User inherited default candidate
				outputs[p.Key] = p.CandidateValue
				trace[p.Key] = ResolutionTraceItem{
					Mode:   "inherit",
					Source: p.Source,
					Actor:  actor,
				}
			}
		case KindHumanDecision:
			userVal := strings.TrimSpace(humanInputs[p.Key])
			if userVal == "" && p.Required {
				return ResolvedApprovalSnapshot{}, fmt.Errorf("required decision parameter %q is missing", p.Key)
			}
			outputs[p.Key] = userVal
			trace[p.Key] = ResolutionTraceItem{
				Mode:   "human_input",
				Source: "dialog.submission",
				Actor:  actor,
			}
		case KindHumanCommentary:
			userVal := strings.TrimSpace(humanInputs[p.Key])
			if userVal == "" && p.Key == "comments" {
				userVal = strings.TrimSpace(humanInputs["comments"])
			}
			if userVal == "" {
				userVal = "Approved via ChatOps"
			}
			outputs[p.Key] = userVal
			trace[p.Key] = ResolutionTraceItem{
				Mode:   "human_input",
				Source: "chatops.review",
				Actor:  actor,
			}
		}
	}

	return ResolvedApprovalSnapshot{
		Decision:           decision,
		ReviewSnapshotHash: preview.ReviewSnapshotHash,
		ResolvedOutputs:    outputs,
		ResolutionTrace:    trace,
	}, nil
}

// ValidateReviewCAS performs strict dual check on expectedStateVersion and reviewSnapshotHash.
func ValidateReviewCAS(currentVersion, expectedVersion int64, currentHash, expectedHash string) error {
	if expectedVersion > 0 && currentVersion != expectedVersion {
		return ErrReviewStaleVersion
	}
	if expectedHash != "" && currentHash != "" && currentHash != expectedHash {
		return ErrReviewStaleHash
	}
	return nil
}
