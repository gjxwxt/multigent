package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/multigent/multigent/internal/projecttemplate"
)

type initializeProjectTemplateBody struct {
	Repo       string `json:"repo"`
	TemplateID string `json:"templateId"`
}

// handleInitializeProjectTemplate materializes a deterministic starter before
// the initialization task runs. The task remains responsible for dependency
// installation, validation, Git initialization and remote synchronization.
func (s *Server) handleInitializeProjectTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.checkProjectManager(w, r, name) {
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
	var body initializeProjectTemplateBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	repo := strings.TrimSpace(body.Repo)
	if repo == "" {
		repo = filepath.Join(s.st.ProjectDir(name), "workspace")
	}
	templateID := strings.TrimSpace(body.TemplateID)
	if templateID == "" {
		templateID = projecttemplate.ReactGoFullstackID
	}
	report, err := projecttemplate.Materialize(repo, templateID)
	if err != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, err.Error())
		return
	}
	project.Repo = filepath.Clean(repo)
	project.TemplateID = report.ID
	project.TemplateVersion = report.Version
	project.TemplateDigest = report.Digest
	if err := s.st.SaveProject(name, project); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		Action:       "project.template_initialize",
		ResourceType: "project",
		ResourceID:   name,
		Summary:      "Deterministic project template materialized",
		After: map[string]any{
			"repo":            project.Repo,
			"templateId":      report.ID,
			"templateVersion": report.Version,
			"templateDigest":  report.Digest,
		},
		Request: r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(report)
}
