package api

// Review-findings import (large-requirement optimization plan stage 3, W3):
// batch-review runs (ZCode dynamic workflows) export a structured findings
// JSON; this endpoint converts each finding into a platform task through the
// same createProjectTaskFromBody pipeline the deploy-AI drawer uses, so the
// "34/36 unfixed with no loop-back" failure mode gets an in-platform path.
// Operator-gated: import creates tasks in bulk.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

const reviewImportCreatedBy = "review-import"

type reviewFinding struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Severity     string `json:"severity"` // P0/P1/P2/P3 or high/medium/low
	Evidence     string `json:"evidence"`
	SuggestedFix string `json:"suggestedFix"`
	Fixed        *bool  `json:"fixed,omitempty"`
}

type reviewImportRequest struct {
	Findings []reviewFinding `json:"findings"`
	// SourceLabel rides the task prompt so the reviewer knows where the
	// batch came from (e.g. "dwfrun-6eded1db 2026-09-26").
	SourceLabel string `json:"sourceLabel,omitempty"`
	Agent       string `json:"agent,omitempty"`
}

type reviewImportResponse struct {
	Created int                 `json:"created"`
	Skipped int                 `json:"skipped"` // already-fixed findings
	Failed  int                 `json:"failed"`
	Errors  []string            `json:"errors,omitempty"`
	Tasks   []map[string]string `json:"tasks,omitempty"` // findingId -> taskId
}

func (s *Server) handleReviewFindingsImport(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	var body reviewImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Findings) == 0 {
		s.jsonError(w, http.StatusBadRequest, "at least one finding is required")
		return
	}
	if len(body.Findings) > 100 {
		s.jsonError(w, http.StatusBadRequest, "at most 100 findings per import")
		return
	}
	agent := strings.TrimSpace(body.Agent)
	if agent == "" {
		agent = s.deployAIProjectAgent(project)
	}
	if agent == "" {
		s.jsonError(w, http.StatusServiceUnavailable, "no agent in this project; hire one before importing findings")
		return
	}

	resp := reviewImportResponse{Tasks: []map[string]string{}}
	source := strings.TrimSpace(body.SourceLabel)
	for _, f := range body.Findings {
		if f.Fixed != nil && *f.Fixed {
			resp.Skipped++
			continue
		}
		title := strings.TrimSpace(f.Title)
		if title == "" {
			resp.Failed++
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: empty title", findingLabel(f)))
			continue
		}
		if len(title) > 200 {
			title = title[:200]
		}
		var b strings.Builder
		b.WriteString("来源：批次审查发现回流（created_by " + reviewImportCreatedBy)
		if source != "" {
			b.WriteString("；批次：" + source)
		}
		b.WriteString("）。请勿当作已验证结论，先复核再修。\n\n")
		if f.Evidence != "" {
			b.WriteString("【证据】\n" + f.Evidence + "\n\n")
		}
		if f.SuggestedFix != "" {
			b.WriteString("【建议修复】\n" + f.SuggestedFix + "\n\n")
		}
		sev := reviewFindingSeverity(f.Severity)
		taskType := entity.TaskTypeBug
		if sev >= 2 {
			taskType = entity.TaskTypeChore
		}
		// createProjectTaskFromBody writes the 201 body itself; capture the
		// task id per finding by swapping in a recorder response writer.
		rec := &statusBodyRecorder{ResponseWriter: w}
		s.createProjectTaskFromBody(rec, r, project, postTaskBody{
			Agent:       agent,
			Title:       title,
			Description: "批次审查发现回流",
			Prompt:      b.String(),
			Type:        string(taskType),
			Priority:    sev,
			Assignee:    project + "/" + agent,
			CreatedBy:   reviewImportCreatedBy,
			Labels:      []string{"batch-review"},
		})
		if rec.status >= 400 {
			resp.Failed++
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %s", findingLabel(f), strings.TrimSpace(string(rec.body))))
			continue
		}
		var created struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.body, &created)
		resp.Created++
		resp.Tasks = append(resp.Tasks, map[string]string{"findingId": f.ID, "taskId": created.ID})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func findingLabel(f reviewFinding) string {
	if strings.TrimSpace(f.ID) != "" {
		return f.ID
	}
	return "finding"
}

// reviewFindingSeverity maps P0-P3 / high-medium-low to the platform 0-3
// priority (0 highest). Unknown labels default to P2 — worth tracking, not
// panic-grade.
func reviewFindingSeverity(raw string) int {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "p0", "high", "critical":
		return 0
	case "p1", "medium", "major":
		return 1
	case "p3", "low", "minor":
		return 3
	default: // "", "p2", anything else
		return 2
	}
}

// statusBodyRecorder captures the status code and body written by
// createProjectTaskFromBody so the import loop can classify per-finding
// results without losing the standard 201 body contract.
type statusBodyRecorder struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (r *statusBodyRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusBodyRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body = append(r.body, p...)
	return r.ResponseWriter.Write(p)
}
