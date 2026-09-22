package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/multigent/multigent/internal/brownfield"
)

type brownfieldScanRequest struct {
	Repo string `json:"repo,omitempty"`
}

type brownfieldScanResponse struct {
	Report     brownfield.ScanReport      `json:"report"`
	Evaluation brownfield.ReadinessResult `json:"evaluation"`
}

// handleBrownfieldScan runs a read-only scan and readiness evaluation against
// the target project repository. It is registered on the main authenticated mux
// with checkProjectOperator RBAC gate (Operator or Manager role required; Viewers
// and unauthenticated callers are rejected).
func (s *Server) handleBrownfieldScan(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.checkProjectOperator(w, r, name) {
		return
	}

	project, err := s.st.Project(name)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}

	var req brownfieldScanRequest
	if r.Body != nil && r.ContentLength > 0 {
		_ = s.readJSON(w, r, &req)
	}

	repoDir := strings.TrimSpace(req.Repo)
	if repoDir == "" {
		// Scan targets the local workspace materialization by default.
		// project.Repo may hold a forge path (e.g. "owner/repo") recorded at
		// creation, which is not a scannable directory — only use it when it
		// resolves to an existing local directory. Explicit request paths are
		// honored as-is (operator choice).
		ws := filepath.Join(s.st.ProjectDir(name), "workspace")
		if st, err := os.Stat(ws); err == nil && st.IsDir() {
			repoDir = ws
		} else if p := strings.TrimSpace(project.Repo); p != "" && !strings.Contains(p, "://") {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				repoDir = filepath.Clean(p)
			}
		} else {
			repoDir = ws
		}
	}
	if !strings.Contains(repoDir, "://") {
		repoDir = filepath.Clean(repoDir)
	}

	report, err := brownfield.Detect(repoDir)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, "scan failed: "+err.Error())
		return
	}

	evaluation := brownfield.Evaluate(report)

	s.auditLog(auditLogInput{
		Action:       "project.brownfield_scan",
		ResourceType: "project",
		ResourceID:   name,
		Summary:      "Brownfield repository scanned and evaluated",
		After: map[string]any{
			"status":             evaluation.Status,
			"recommendedProfile": evaluation.RecommendedProfile,
			"servicesCount":      len(report.Services),
			"issuesCount":        len(evaluation.Issues),
		},
		Request: r,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(brownfieldScanResponse{
		Report:     report,
		Evaluation: evaluation,
	})
}

// handleRuntimeBrownfieldEvaluate runs the brownfield readiness evaluation gate
// within the runtime task sandbox context. It is registered on runtimeMux with task.use capability.
func (s *Server) handleRuntimeBrownfieldEvaluate(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}

	project, err := s.st.Project(principal.Project)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}

	var req brownfieldScanRequest
	if r.Body != nil && r.ContentLength > 0 {
		_ = s.readJSON(w, r, &req)
	}

	repoDir := strings.TrimSpace(req.Repo)
	if repoDir == "" {
		repoDir = strings.TrimSpace(project.Repo)
	}
	if repoDir == "" {
		repoDir = filepath.Join(s.st.ProjectDir(principal.Project), "workspace")
	}
	if !strings.Contains(repoDir, "://") {
		repoDir = filepath.Clean(repoDir)
	}

	report, err := brownfield.Detect(repoDir)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, "runtime scan failed: "+err.Error())
		return
	}

	evaluation := brownfield.Evaluate(report)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(brownfieldScanResponse{
		Report:     report,
		Evaluation: evaluation,
	})
}
