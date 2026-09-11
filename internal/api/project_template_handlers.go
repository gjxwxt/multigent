package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/projecttemplate"
	"github.com/multigent/multigent/internal/sandbox"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

type initializeProjectTemplateBody struct {
	Repo       string `json:"repo"`
	TemplateID string `json:"templateId"`
	Agent      string `json:"agent"`
}

type projectInitializationStatusResponse struct {
	Status string   `json:"status"`
	Task   *taskRow `json:"task,omitempty"`
}

// handleGetProjectInitialization returns the latest durable initialization
// task. The UI uses this after a reload instead of trusting modal state or
// browser storage. A label alone is not sufficient: the workflow run must
// also be the built-in initialization workflow.
func (s *Server) handleGetProjectInitialization(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.checkProjectAccess(w, r, name) {
		return
	}
	if _, err := s.st.Project(name); err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}
	agents, err := s.projectAgentNames(workspaceID, name)
	if err != nil {
		s.serverError(w, err)
		return
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	type candidate struct {
		task     *entity.Task
		agent    string
		archived bool
		run      entity.WorkflowRun
	}
	var candidates []candidate
	seen := make(map[string]bool)
	for _, agentName := range agents {
		if !s.canAccessAgent(r, name, agentName) {
			continue
		}
		activeTasks, listErr := s.ts.ListTasks(name, agentName)
		if listErr != nil {
			s.serverError(w, listErr)
			return
		}
		archivedTasks, listErr := s.ts.ListArchivedTasks(name, agentName)
		if listErr != nil {
			s.serverError(w, listErr)
			return
		}
		for _, list := range []struct {
			tasks    []*entity.Task
			archived bool
		}{{tasks: activeTasks}, {tasks: archivedTasks, archived: true}} {
			for _, task := range list.tasks {
				if task == nil || seen[task.ID] || !hasTaskLabel(task, "project-initialization") {
					continue
				}
				seen[task.ID] = true
				run, found, runErr := wfStore.RunForTask(name, task.ID)
				if runErr != nil {
					s.serverError(w, runErr)
					return
				}
				if !found || run.DefinitionID != workflowstore.ProjectInitializationWorkflowID {
					continue
				}
				candidates = append(candidates, candidate{task: task, agent: agentName, archived: list.archived, run: run})
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].task.UpdatedAt.Equal(candidates[j].task.UpdatedAt) {
			return candidates[i].task.ID > candidates[j].task.ID
		}
		return candidates[i].task.UpdatedAt.After(candidates[j].task.UpdatedAt)
	})
	if len(candidates) == 0 {
		_ = json.NewEncoder(w).Encode(projectInitializationStatusResponse{Status: "idle"})
		return
	}
	latest := candidates[0]
	status := strings.TrimSpace(latest.run.Status)
	if status == "" {
		status = string(latest.task.Status)
	}
	row := s.taskToRowWithWorkflow(workspaceID, latest.task, name, latest.agent, latest.archived)
	_ = json.NewEncoder(w).Encode(projectInitializationStatusResponse{
		Status: status,
		Task:   &row,
	})
}

func hasTaskLabel(task *entity.Task, label string) bool {
	for _, item := range task.Labels {
		if strings.TrimSpace(item) == label {
			return true
		}
	}
	return false
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
	// A remote URL must survive untouched: filepath.Clean would collapse
	// "http://" into "http:/" and later break the GitLab remote binding
	// (ci_ready then reports the project as not bound and skips pipeline
	// evidence). Only real filesystem paths get path-normalised.
	if !strings.Contains(repo, "://") {
		repo = filepath.Clean(repo)
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
	agent := strings.TrimSpace(body.Agent)
	if agent != "" {
		if !s.agentExistsInProject(name, agent) {
			removeMaterializedTemplate(repo, report.Files)
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAgentNotFound, "agent not found")
			return
		}
		// If the repository is distinct from the agent directory, do not duplicate
		// template files into the agent directory. The project repo / workspace is
		// the single source of truth for project code.
		agentDir := filepath.Clean(s.st.AgentDir(name, agent))
		if repoClean := filepath.Clean(repo); repoClean == agentDir {
			if _, err := projecttemplate.Seed(agentDir, templateID); err != nil {
				removeMaterializedTemplate(repo, report.Files)
				s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, err.Error())
				return
			}
		}
	}
	if strings.Contains(repo, "://") {
		project.Repo = repo
	} else {
		project.Repo = filepath.Clean(repo)
	}
	project.TemplateID = report.ID
	project.TemplateVersion = report.Version
	project.TemplateDigest = report.Digest
	// Templates declare their runtime capability on the PROJECT, not on shared
	// agent workers: the Spring Boot template pins the jvm21 profile so every
	// agent and preview in this project gets a JDK-capable runtime, while the
	// same worker reused in other projects keeps its own project's profile.
	// Explicit declaration, never build-file sniffing.
	if templateID == projecttemplate.ReactSpringBootID {
		project.RuntimeProfile = sandbox.ProfileJVM21
	}
	// Reserve a stable deploy port from the platform pool. The port only
	// persists with the SaveProject below, so a failed initialization
	// leaks nothing back into the pool.
	if _, err := s.ensureDeployPort(project); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.st.SaveProject(name, project); err != nil {
		s.serverError(w, err)
		return
	}
	// Best-effort: no-op until the project has a bound GitLab remote (the
	// initialization workflow binds it later; handlePutProject re-pushes).
	s.pushDeployPortVariable(r.Context(), name, project)
	s.bindDefaultRunner(r.Context(), name, project)
	s.auditLog(auditLogInput{
		Action:       "project.template_initialize",
		ResourceType: "project",
		ResourceID:   name,
		Summary:      "Deterministic project template materialized",
		After: map[string]any{
			"repo":            project.Repo,
			"agent":           agent,
			"templateId":      report.ID,
			"templateVersion": report.Version,
			"templateDigest":  report.Digest,
			"deployPort":      project.DeployPort,
		},
		Request: r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(report)
}

func removeMaterializedTemplate(root string, files []string) {
	for i := len(files) - 1; i >= 0; i-- {
		_ = os.Remove(filepath.Join(root, filepath.FromSlash(files[i])))
	}
}
