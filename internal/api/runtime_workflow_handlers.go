package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/errs"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/taskstore"
	"github.com/multigent/multigent/internal/tasktemplate"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

type runtimeTaskBody struct {
	Project          string            `json:"project"`
	Agent            string            `json:"agent"`
	Title            string            `json:"title"`
	Prompt           string            `json:"prompt"`
	Description      string            `json:"description"`
	Type             string            `json:"type"`
	Priority         int               `json:"priority"`
	Assignee         string            `json:"assignee"`
	Labels           []string          `json:"labels"`
	ParentID         string            `json:"parentId"`
	DueDate          string            `json:"dueDate"`
	NotBefore        string            `json:"notBefore"`
	EstimateDuration string            `json:"estimateDuration"`
	Vars             map[string]string `json:"vars"`
}

type runtimeTaskUpdateBody struct {
	Agent            string    `json:"agent"`
	Title            *string   `json:"title,omitempty"`
	Description      *string   `json:"description,omitempty"`
	Status           *string   `json:"status,omitempty"`
	Priority         *int      `json:"priority,omitempty"`
	Type             *string   `json:"type,omitempty"`
	Summary          *string   `json:"summary,omitempty"`
	Error            *string   `json:"error,omitempty"`
	Labels           *[]string `json:"labels,omitempty"`
	ParentID         *string   `json:"parentId,omitempty"`
	DueDate          *string   `json:"dueDate,omitempty"`
	NotBefore        *string   `json:"notBefore,omitempty"`
	EstimateDuration *string   `json:"estimateDuration,omitempty"`
	Position         *float64  `json:"position,omitempty"`
	Assignee         *string   `json:"assignee,omitempty"`
	Prompt           *string   `json:"prompt,omitempty"`
}

type runtimeTaskCompleteBody struct {
	Agent   string            `json:"agent"`
	Status  string            `json:"status"`
	Summary string            `json:"summary"`
	Error   string            `json:"error"`
	Outputs map[string]string `json:"outputs"`
}

const (
	workflowRootTaskIDVar = "workflow_root_task_id"
	workflowRunIDVar      = "workflow_run_id"
	workflowStepIDVar     = "workflow_step_id"
	workflowBranchIDVar   = "workflow_branch_id"
)

type runtimeConfirmRequestBody struct {
	Agent       string   `json:"agent"`
	To          string   `json:"to"`
	Summary     string   `json:"summary"`
	ActionHint  string   `json:"actionHint"`
	ActionItems []string `json:"actionItems"`
}

type runtimeMessageBody struct {
	To      any    `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	ReplyTo string `json:"replyTo"`
}

type runtimeReplyMessageBody struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type runtimeWorkflowPendingReviewsResponse struct {
	Reviews []runtimeWorkflowPendingReview `json:"reviews"`
}

type runtimeWorkflowPendingReview struct {
	Project        string                  `json:"project"`
	TaskID         string                  `json:"taskId"`
	TaskTitle      string                  `json:"taskTitle"`
	TaskStatus     string                  `json:"taskStatus"`
	TaskAgent      string                  `json:"taskAgent"`
	TaskAssignee   string                  `json:"taskAssignee,omitempty"`
	WorkflowRunID  string                  `json:"workflowRunId"`
	WorkflowID     string                  `json:"workflowId"`
	WorkflowName   string                  `json:"workflowName"`
	StepID         string                  `json:"stepId"`
	StepTitle      string                  `json:"stepTitle"`
	StepStatus     string                  `json:"stepStatus"`
	Reviewer       string                  `json:"reviewer,omitempty"`
	WaitingSeconds int64                   `json:"waitingSeconds,omitempty"`
	UpdatedAt      time.Time               `json:"updatedAt"`
	InputValues    map[string]string       `json:"inputValues,omitempty"`
	OutputValues   map[string]string       `json:"outputValues,omitempty"`
	InputArtifact  string                  `json:"inputArtifact,omitempty"`
	OutputArtifact string                  `json:"outputArtifact,omitempty"`
	OutputFields   []entity.WorkflowField  `json:"outputFields,omitempty"`
	DocumentRefs   []runtimeWorkflowDocRef `json:"documentRefs,omitempty"`
	RouteOptions   []runtimeWorkflowRoute  `json:"routeOptions,omitempty"`
}

type runtimeWorkflowDocRef struct {
	Field string `json:"field,omitempty"`
	ID    string `json:"id"`
}

type runtimeWorkflowRoute struct {
	ID        string                        `json:"id"`
	Label     string                        `json:"label,omitempty"`
	To        string                        `json:"to"`
	Default   bool                          `json:"default,omitempty"`
	Condition *entity.WorkflowEdgeCondition `json:"condition,omitempty"`
}

var workflowDocIDPattern = regexp.MustCompile(`\bkb-doc-[A-Za-z0-9_-]+\b`)

func (s *Server) runtimeRequireCapability(w http.ResponseWriter, r *http.Request, capability string) (runtimeAgentPrincipal, bool) {
	principal, ok := runtimeAgentFromRequest(r)
	if !ok {
		s.jsonErrorCode(w, http.StatusUnauthorized, ErrCodeRuntimeAgentTokenRequired, "runtime agent token required")
		return runtimeAgentPrincipal{}, false
	}
	if !runtimeHasCapability(principal, capability) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeRuntimeCapabilityRequired, "runtime token lacks "+capability+" capability")
		return runtimeAgentPrincipal{}, false
	}
	return principal, true
}

func runtimeAgentAddress(principal runtimeAgentPrincipal) string {
	return principal.Project + "/" + principal.Agent
}

func (s *Server) runtimeTargetAgent(w http.ResponseWriter, principal runtimeAgentPrincipal, requested string) (string, bool) {
	return s.runtimeTargetAgentInProject(w, principal, principal.Project, requested)
}

func (s *Server) runtimeTargetAgentInProject(w http.ResponseWriter, principal runtimeAgentPrincipal, project, requested string) (string, bool) {
	agent := strings.TrimSpace(requested)
	if agent == "" {
		agent = principal.Agent
	}
	if !s.agentExistsInWorkspaceProject(principal.WorkspaceID, project, agent) {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAgentNotFound, "agent not found in target project")
		return "", false
	}
	return agent, true
}

// runtimeProjectForAgent permits an agent to dispatch work into another project
// only when the same workspace agent worker has a membership there. Project
// membership is the existing runtime access boundary; no global project access
// is inferred from the source runtime token.
func (s *Server) runtimeProjectForAgent(w http.ResponseWriter, principal runtimeAgentPrincipal, requested string) (string, bool) {
	project := strings.TrimSpace(requested)
	if project == "" {
		project = principal.Project
	}
	if project == principal.Project {
		return project, true
	}
	if _, err := s.st.Project(project); err != nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "target project not found")
		return "", false
	}
	if !s.agentExistsInWorkspaceProject(principal.WorkspaceID, project, principal.Agent) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeAgentOperatorRequired, "runtime agent is not a member of target project")
		return "", false
	}
	return project, true
}

func (s *Server) runtimeFindTask(principal runtimeAgentPrincipal, id, requestedAgent string) (*entity.Task, string, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, "", false, fmt.Errorf("task id is required")
	}
	agents := s.runtimeTaskAgentAliases(principal, requestedAgent)
	for _, agent := range agents {
		t, err := s.ts.GetTask(principal.Project, agent, id)
		if err == nil {
			archived := t.Status.IsTerminal()
			return t, agent, archived, nil
		}
	}
	return nil, "", false, fmt.Errorf("task not found")
}

// runtimeTaskAgentAliases handles the identity transition from worker names
// to project membership titles. New tasks use the canonical membership title;
// aliases keep an already queued task operable after a membership was renamed
// or after an older scheduler stored the worker name.
func (s *Server) runtimeTaskAgentAliases(principal runtimeAgentPrincipal, requested string) []string {
	values := []string{strings.TrimSpace(principal.Agent)}
	if s != nil && s.agentDirectory != nil {
		if resolved, ok, err := s.agentDirectory.ProjectWorker(principal.WorkspaceID, principal.Project, principal.Agent); err == nil && ok {
			values = append(values,
				resolved.Membership.Title,
				resolved.Membership.MemberID,
				resolved.Worker.Name,
				resolved.Worker.DisplayName,
			)
		}
	}
	// An explicit --agent is only an alias selector for this same worker, not
	// a way for a runtime token to reach another agent's task queue.
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(value), requested) {
				values = append(values, requested)
				break
			}
		}
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func (s *Server) handleRuntimeTasks(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	q := r.URL.Query()
	qStatus := strings.TrimSpace(q.Get("status"))
	qAgent := strings.TrimSpace(q.Get("agent"))
	qScope := strings.TrimSpace(q.Get("scope"))
	if qScope == "" {
		qScope = "all"
	}
	if qScope != "active" && qScope != "archived" && qScope != "all" {
		s.jsonError(w, http.StatusBadRequest, "scope must be active, archived, or all")
		return
	}
	agents, err := s.projectAgentNames(principal.WorkspaceID, principal.Project)
	if err != nil {
		s.serverError(w, err)
		return
	}
	rows := make([]taskRow, 0)
	addTasks := func(agent string, archived bool) {
		var tasks []*entity.Task
		var err error
		if archived {
			tasks, err = s.ts.ListArchivedTasks(principal.Project, agent)
		} else {
			tasks, err = s.ts.ListTasks(principal.Project, agent)
		}
		if err != nil {
			return
		}
		for _, t := range tasks {
			if t == nil {
				continue
			}
			if qStatus != "" && string(t.Status) != qStatus {
				continue
			}
			rows = append(rows, taskToRow(t, principal.Project, agent, archived))
		}
	}
	for _, agent := range agents {
		if qAgent != "" && agent != qAgent {
			continue
		}
		if qScope == "active" || qScope == "all" {
			addTasks(agent, false)
		}
		if qScope == "archived" || qScope == "all" {
			addTasks(agent, true)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
	})
	_ = json.NewEncoder(w).Encode(rows)
}

func (s *Server) handleRuntimeTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	t, agent, archived, err := s.runtimeFindTask(principal, r.PathValue("id"), r.URL.Query().Get("agent"))
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	_ = json.NewEncoder(w).Encode(taskToRow(t, principal.Project, agent, archived))
}

func (s *Server) handleRuntimeWorkflowPendingReviews(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	reviewer := strings.TrimSpace(r.URL.Query().Get("reviewer"))
	limit := 50
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 200 {
			s.jsonError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	agents, err := s.projectAgentNames(principal.WorkspaceID, principal.Project)
	if err != nil {
		s.serverError(w, err)
		return
	}
	wfStore := workflowstore.NewStore(s.controlDB, principal.WorkspaceID)
	reviews := make([]runtimeWorkflowPendingReview, 0)
	for _, agent := range agents {
		tasks, err := s.ts.ListTasks(principal.Project, agent)
		if err != nil {
			continue
		}
		for _, task := range tasks {
			if task == nil || task.Status.IsTerminal() {
				continue
			}
			run, found, err := wfStore.RunForTask(principal.Project, task.ID)
			if err != nil {
				s.serverError(w, err)
				return
			}
			if !found || strings.TrimSpace(run.ActiveStepID) == "" || isWorkflowRunTerminal(run.Status) {
				continue
			}
			def, found, err := wfStore.RunDefinition(run)
			if err != nil {
				s.serverError(w, err)
				return
			}
			if !found {
				continue
			}
			step, found := workflowDefinitionStepByID(def.Steps, run.ActiveStepID)
			if !found || step.Type != "human_review" {
				continue
			}
			instances, err := wfStore.ListStepInstances(run.ID)
			if err != nil {
				s.serverError(w, err)
				return
			}
			inst, found := workflowStepInstanceByStepID(instances, run.ActiveStepID)
			if !found || !workflowStepInstanceOpen(inst.Status) {
				continue
			}
			actorType := strings.TrimSpace(inst.ActorType)
			if actorType == "" {
				actorType = workflowActorTypeForStep(step)
			}
			actorID := strings.TrimSpace(inst.ActorID)
			if actorID == "" {
				actorID = workflowActorIDForStep(run.ActorBindings, step)
			}
			if actorType != "" && actorType != "human" {
				continue
			}
			if reviewer != "" && !strings.EqualFold(actorID, reviewer) {
				continue
			}
			reviews = append(reviews, runtimeWorkflowPendingReviewFromTask(principal.Project, agent, task, run, def, step, inst))
		}
	}
	sort.SliceStable(reviews, func(i, j int) bool {
		return reviews[i].UpdatedAt.Before(reviews[j].UpdatedAt)
	})
	if len(reviews) > limit {
		reviews = reviews[:limit]
	}
	_ = json.NewEncoder(w).Encode(runtimeWorkflowPendingReviewsResponse{Reviews: reviews})
}

func isWorkflowRunTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "done", "done_success", "done_failed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func runtimeWorkflowPendingReviewFromTask(project, agent string, task *entity.Task, run entity.WorkflowRun, def entity.WorkflowDefinition, step entity.WorkflowStep, inst entity.WorkflowStepInstance) runtimeWorkflowPendingReview {
	updatedAt := inst.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = run.UpdatedAt
	}
	waitingSeconds := int64(0)
	if !updatedAt.IsZero() {
		waitingSeconds = int64(time.Since(updatedAt).Seconds())
		if waitingSeconds < 0 {
			waitingSeconds = 0
		}
	}
	reviewer := strings.TrimSpace(inst.ActorID)
	if reviewer == "" {
		reviewer = workflowActorIDForStep(run.ActorBindings, step)
	}
	return runtimeWorkflowPendingReview{
		Project:        project,
		TaskID:         task.ID,
		TaskTitle:      task.Title,
		TaskStatus:     string(task.Status),
		TaskAgent:      agent,
		TaskAssignee:   task.Assignee,
		WorkflowRunID:  run.ID,
		WorkflowID:     def.ID,
		WorkflowName:   def.Name,
		StepID:         step.ID,
		StepTitle:      step.Title,
		StepStatus:     inst.Status,
		Reviewer:       reviewer,
		WaitingSeconds: waitingSeconds,
		UpdatedAt:      updatedAt,
		InputValues:    inst.InputValues,
		OutputValues:   inst.OutputValues,
		InputArtifact:  inst.InputArtifact,
		OutputArtifact: inst.OutputArtifact,
		OutputFields:   step.OutputFields,
		DocumentRefs:   runtimeWorkflowDocumentRefs(inst),
		RouteOptions:   runtimeWorkflowRoutes(def.Edges, step.ID),
	}
}

func runtimeWorkflowRoutes(edges []entity.WorkflowEdge, stepID string) []runtimeWorkflowRoute {
	routes := make([]runtimeWorkflowRoute, 0)
	for _, edge := range edges {
		if edge.From != stepID {
			continue
		}
		routes = append(routes, runtimeWorkflowRoute{
			ID:        edge.ID,
			Label:     edge.Label,
			To:        edge.To,
			Default:   edge.IsDefault,
			Condition: edge.Condition,
		})
	}
	return routes
}

func runtimeWorkflowDocumentRefs(inst entity.WorkflowStepInstance) []runtimeWorkflowDocRef {
	seen := map[string]bool{}
	refs := make([]runtimeWorkflowDocRef, 0)
	add := func(field, value string) {
		for _, match := range workflowDocIDPattern.FindAllString(value, -1) {
			if seen[match] {
				continue
			}
			seen[match] = true
			refs = append(refs, runtimeWorkflowDocRef{Field: field, ID: match})
		}
	}
	for field, value := range inst.InputValues {
		add(field, value)
	}
	for field, value := range inst.OutputValues {
		add(field, value)
	}
	add("inputArtifact", inst.InputArtifact)
	add("outputArtifact", inst.OutputArtifact)
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].Field == refs[j].Field {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Field < refs[j].Field
	})
	return refs
}

func (s *Server) handleRuntimeTaskTemplates(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	store := tasktemplate.NewStore(s.controlDB, principal.WorkspaceID)
	templates, err := store.List()
	if err != nil {
		s.serverError(w, err)
		return
	}
	project, ok := s.runtimeProjectForAgent(w, principal, r.URL.Query().Get("project"))
	if !ok {
		return
	}
	filtered := make([]entity.TaskTemplate, 0, len(templates))
	for _, template := range templates {
		if template.Project == project {
			filtered = append(filtered, template)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"project": project, "templates": filtered})
}

func (s *Server) handleRuntimePostTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body runtimeTaskBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	project, ok := s.runtimeProjectForAgent(w, principal, body.Project)
	if !ok {
		return
	}
	agent, ok := s.runtimeTargetAgentInProject(w, principal, project, body.Agent)
	if !ok {
		return
	}
	title := strings.TrimSpace(body.Title)
	prompt := strings.TrimSpace(body.Prompt)
	if title == "" || prompt == "" {
		s.jsonError(w, http.StatusBadRequest, "title and prompt are required")
		return
	}
	taskType := strings.TrimSpace(body.Type)
	if taskType == "" {
		taskType = string(entity.TaskTypeChore)
	}
	if !validTaskType(taskType) {
		s.jsonError(w, http.StatusBadRequest, "invalid task type")
		return
	}
	priority := body.Priority
	if priority < 0 || priority > 3 {
		s.jsonError(w, http.StatusBadRequest, "priority must be 0-3")
		return
	}
	assignee := strings.TrimSpace(body.Assignee)
	if assignee == "" {
		assignee = project + "/" + agent
	}
	if err := s.validateIdentity(assignee, "assignee"); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	t := &entity.Task{
		ID:          entity.NewTaskID(),
		Title:       title,
		Description: strings.TrimSpace(body.Description),
		Type:        entity.TaskType(taskType),
		Priority:    priority,
		Assignee:    assignee,
		CreatedBy:   runtimeAgentAddress(principal),
		Status:      entity.TaskStatusPending,
		Prompt:      prompt,
		Labels:      body.Labels,
		ParentID:    strings.TrimSpace(body.ParentID),
		Vars:        sanitizeTaskVars(body.Vars),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if est, err := entity.NormalizeEstimateDuration(body.EstimateDuration); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	} else {
		t.EstimateDuration = est
	}
	if body.DueDate != "" {
		dd, err := time.Parse("2006-01-02", body.DueDate)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "invalid due date, use YYYY-MM-DD")
			return
		}
		t.DueDate = &dd
	}
	if body.NotBefore != "" {
		nb, err := entity.ParseTaskNotBefore(body.NotBefore, now)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.NotBefore = nb
	}
	s.annotateTaskAssignee(principal.WorkspaceID, project, t)
	if err := s.ts.AddTask(project, agent, t); err != nil {
		s.serverError(w, err)
		return
	}
	s.recordTaskAttentionSignal(principal.WorkspaceID, project, agent, t, "task_assigned")
	s.triggers.Fire(project, agent, entity.TriggerOnTask, "task "+t.ID)
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.task.create",
		ResourceType: "task",
		ResourceID:   project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent created task",
		After:        taskToRow(t, project, agent, false),
		Request:      r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(taskToRow(t, project, agent, false))
}

func (s *Server) handleRuntimePostTaskFromTemplate(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body taskFromTemplateBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	store := tasktemplate.NewStore(s.controlDB, principal.WorkspaceID)
	template, found, err := store.Get(strings.TrimSpace(body.TemplateID))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonError(w, http.StatusNotFound, "task template not found")
		return
	}
	requestedProject := strings.TrimSpace(body.Project)
	if template.Project != "" && requestedProject != "" && template.Project != requestedProject {
		s.jsonError(w, http.StatusBadRequest, "task template does not belong to target project")
		return
	}
	targetProject := requestedProject
	if targetProject == "" {
		targetProject = template.Project
	}
	targetProject, ok = s.runtimeProjectForAgent(w, principal, targetProject)
	if !ok {
		return
	}
	taskBody, err := instantiateTaskTemplate(template, body)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if taskBody.Agent == "" {
		taskBody.Agent = s.initialWorkflowAgent(principal.WorkspaceID, template, taskBody.WorkflowActorBindings)
	}
	s.createRuntimeTaskFromBody(w, r, principal, targetProject, taskBody)
}

func (s *Server) createRuntimeTaskFromBody(w http.ResponseWriter, r *http.Request, principal runtimeAgentPrincipal, project string, body postTaskBody) {
	agent, ok := s.runtimeTargetAgentInProject(w, principal, project, body.Agent)
	if !ok {
		return
	}
	title := strings.TrimSpace(body.Title)
	prompt := strings.TrimSpace(body.Prompt)
	if title == "" || prompt == "" {
		s.jsonError(w, http.StatusBadRequest, "title and prompt are required")
		return
	}
	taskType := strings.TrimSpace(body.Type)
	if taskType == "" {
		taskType = string(entity.TaskTypeChore)
	}
	if !validTaskType(taskType) {
		s.jsonError(w, http.StatusBadRequest, "invalid task type")
		return
	}
	priority := body.Priority
	if priority < 0 || priority > 3 {
		s.jsonError(w, http.StatusBadRequest, "priority must be 0-3")
		return
	}
	assignee := strings.TrimSpace(body.Assignee)
	if assignee == "" {
		assignee = project + "/" + agent
	}
	workflowID := strings.TrimSpace(body.WorkflowDefinitionID)
	var workflowStore interface {
		Definition(string) (entity.WorkflowDefinition, bool, error)
		StartRunWithInput(string, string, string, map[string]entity.WorkflowActorBinding, map[string]string) (entity.WorkflowRun, []entity.WorkflowStepInstance, error)
	}
	var workflowDef entity.WorkflowDefinition
	if workflowID != "" {
		wfStore := workflowstore.NewStore(s.controlDB, principal.WorkspaceID)
		workflowStore = wfStore
		def, found, err := wfStore.Definition(workflowID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if !found {
			s.jsonError(w, http.StatusNotFound, "workflow definition not found")
			return
		}
		workflowDef = def
		if _, inst, ok := workflowStartActor(def, body.WorkflowActorBindings); ok {
			switch inst.ActorType {
			case "agent":
				startAgent := strings.TrimSpace(inst.ActorID)
				if startAgent == "" || !s.agentExistsInWorkspaceProject(principal.WorkspaceID, project, startAgent) {
					s.jsonError(w, http.StatusBadRequest, "workflow start agent not found in target project")
					return
				}
				agent = startAgent
				assignee = project + "/" + startAgent
			case "human":
				reviewer := strings.TrimSpace(inst.ActorID)
				if reviewer == "" {
					s.jsonError(w, http.StatusBadRequest, "workflow start reviewer is required")
					return
				}
				if err := s.validateIdentity(reviewer, "workflow start reviewer"); err != nil {
					s.jsonError(w, http.StatusBadRequest, err.Error())
					return
				}
				assignee = reviewer
			}
		}
	}
	if err := s.validateIdentity(assignee, "assignee"); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	t := &entity.Task{
		ID:          entity.NewTaskID(),
		Title:       title,
		Description: strings.TrimSpace(body.Description),
		Type:        entity.TaskType(taskType),
		Priority:    priority,
		Assignee:    assignee,
		CreatedBy:   runtimeAgentAddress(principal),
		Status:      entity.TaskStatusPending,
		Prompt:      prompt,
		Labels:      body.Labels,
		ParentID:    strings.TrimSpace(body.ParentID),
		Vars:        sanitizeTaskVars(body.Vars),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if est, err := entity.NormalizeEstimateDuration(body.EstimateDuration); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	} else {
		t.EstimateDuration = est
	}
	if body.DueDate != "" {
		dd, err := time.Parse("2006-01-02", body.DueDate)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "invalid due date, use YYYY-MM-DD")
			return
		}
		t.DueDate = &dd
	}
	if body.NotBefore != "" {
		nb, err := entity.ParseTaskNotBefore(body.NotBefore, now)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		t.NotBefore = nb
	}
	s.annotateTaskAssignee(principal.WorkspaceID, project, t)
	if err := s.ts.AddTask(project, agent, t); err != nil {
		s.serverError(w, err)
		return
	}
	if !strings.Contains(assignee, "/") {
		item := &entity.InboxItem{TaskID: t.ID, Project: project, Agent: agent, To: assignee, Title: t.Title, Summary: prompt}
		if err := s.ts.AddToInbox(item); err != nil {
			s.serverError(w, err)
			return
		}
	}
	if workflowID != "" {
		if workflowStore == nil {
			s.serverError(w, fmt.Errorf("workflow store unavailable"))
			return
		}
		initialInputs := workflowInitialInputsFromPrompt(workflowDef, prompt)
		if _, _, err := workflowStore.StartRunWithInput(project, t.ID, workflowID, body.WorkflowActorBindings, initialInputs); err != nil {
			s.serverError(w, err)
			return
		}
		s.notifyTaskThreadStarted(principal.WorkspaceID, project, t, workflowID)
	}
	if strings.Contains(assignee, "/") {
		reason := "task_assigned"
		if workflowID != "" {
			reason = string(entity.TriggerOnWorkflowStepAssigned)
		}
		signalID := s.recordTaskAttentionSignal(principal.WorkspaceID, project, agent, t, reason)
		s.requestTaskAttentionWakeup(principal.WorkspaceID, project, agent, t, reason, signalID)
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.task.create_from_template",
		ResourceType: "task",
		ResourceID:   project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent created task from template",
		After:        taskToRow(t, project, agent, false),
		Request:      r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(taskToRow(t, project, agent, false))
}

func (s *Server) handleRuntimePutTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body runtimeTaskUpdateBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	t, agent, _, err := s.runtimeFindTask(principal, r.PathValue("id"), body.Agent)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if body.Error != nil {
		t.LastError = strings.TrimSpace(*body.Error)
	}
	patch, err := runtimeTaskPatch(body)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if runtimeTaskPatchHasFields(body) {
		if _, err := taskstore.ApplyTaskPatch(t, patch, time.Now().UTC()); err != nil {
			s.jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if body.Error != nil {
		t.UpdatedAt = time.Now().UTC()
	} else {
		s.jsonError(w, http.StatusBadRequest, "at least one field to update is required")
		return
	}
	if err := s.ts.PersistTask(principal.Project, agent, t); err != nil {
		s.serverError(w, err)
		return
	}
	if patch.Status != nil && t.Status.IsTerminal() && t.CreatedBy != "" {
		s.notifyTaskDone(t, principal.Project, agent)
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.task.update",
		ResourceType: "task",
		ResourceID:   principal.Project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent updated task",
		After:        taskToRow(t, principal.Project, agent, t.Status.IsTerminal()),
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(taskToRow(t, principal.Project, agent, t.Status.IsTerminal()))
}

func runtimeTaskPatchHasFields(body runtimeTaskUpdateBody) bool {
	return body.Title != nil || body.Description != nil || body.Status != nil || body.Priority != nil ||
		body.Type != nil || body.Summary != nil || body.Labels != nil || body.ParentID != nil ||
		body.DueDate != nil || body.NotBefore != nil || body.EstimateDuration != nil || body.Position != nil || body.Assignee != nil ||
		body.Prompt != nil
}

func runtimeTaskPatch(body runtimeTaskUpdateBody) (taskstore.TaskPatch, error) {
	patch := taskstore.TaskPatch{
		Title: body.Title, Description: body.Description, Summary: body.Summary,
		Labels: body.Labels, ParentID: body.ParentID, DueDate: body.DueDate, NotBefore: body.NotBefore,
		EstimateDuration: body.EstimateDuration, Position: body.Position,
		Assignee: body.Assignee, Prompt: body.Prompt,
	}
	if body.Status != nil {
		st := strings.TrimSpace(*body.Status)
		if st == "" || !validTaskStatus(st) {
			return patch, fmt.Errorf("invalid task status")
		}
		status := entity.TaskStatus(st)
		patch.Status = &status
	}
	if body.Priority != nil {
		p := *body.Priority
		patch.Priority = &p
	}
	if body.Type != nil {
		typ := strings.TrimSpace(*body.Type)
		if typ == "" || !validTaskType(typ) {
			return patch, fmt.Errorf("invalid task type")
		}
		taskType := entity.TaskType(typ)
		patch.Type = &taskType
	}
	return patch, nil
}

func (s *Server) handleRuntimeTaskComplete(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body runtimeTaskCompleteBody
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	t, agent, _, err := s.runtimeFindTask(principal, r.PathValue("id"), body.Agent)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if strings.TrimSpace(t.Vars[workflowBranchIDVar]) != "" {
		s.completeRuntimeWorkflowBranchHTTP(w, r, principal, t, agent, body)
		return
	}
	if s.runtimeTaskHasWorkflow(principal.WorkspaceID, principal.Project, t.ID) {
		s.jsonError(w, http.StatusBadRequest, "workflow tasks must complete the current workflow step with `mga task step done`")
		return
	}
	status := normalizeDoneStatus(body.Status, body.Error)
	now := time.Now().UTC()
	prev := t.Status
	t.Status = status
	t.Summary = strings.TrimSpace(body.Summary)
	t.LastError = strings.TrimSpace(body.Error)
	t.UpdatedAt = now
	entity.ApplyStatusTimestamps(t, prev, now)
	if err := s.ts.ArchiveTask(principal.Project, agent, t); err != nil {
		s.serverError(w, err)
		return
	}
	if t.CreatedBy != "" {
		s.notifyTaskDone(t, principal.Project, agent)
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.task.complete",
		ResourceType: "task",
		ResourceID:   principal.Project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent completed task",
		After:        taskToRow(t, principal.Project, agent, true),
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(taskToRow(t, principal.Project, agent, true))
}

func (s *Server) handleRuntimeWorkflowBranchComplete(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body runtimeTaskCompleteBody
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	t, agent, _, err := s.runtimeFindTask(principal, r.PathValue("id"), body.Agent)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if strings.TrimSpace(t.Vars[workflowBranchIDVar]) == "" {
		s.jsonError(w, http.StatusBadRequest, "task is not attached to a workflow branch")
		return
	}
	s.completeRuntimeWorkflowBranchHTTP(w, r, principal, t, agent, body)
}

// resumeArchivedBranchJoin drives the idempotent branch-join path for an
// ARCHIVED branch child that re-reports its completion (S2-2, reviewer
// P1-2). The child run is already terminal, so the step-level transition
// would fail with ErrStaleWorkflowTransition and leave the parent join
// unattempted on every retry — exactly the wedge the S2 dogfood hit.
// CompleteBranchAndMaybeAdvance is idempotent for an already-terminal
// branch instance (returns the recorded result, zero writes), so routing
// the re-report here resumes the missed parent advance safely. A re-report
// for a branch that was recorded as FAILED returns the recorded failure
// verbatim: the agent cannot retry a failed branch by re-reporting (that
// contract predates S2-2 and the human gate owns failed-branch recovery).
func (s *Server) resumeArchivedBranchJoin(w http.ResponseWriter, r *http.Request, principal runtimeAgentPrincipal, t *entity.Task, agent string, body runtimeTaskCompleteBody, stepStatus string) {
	// Cleanup note (review P2-2): this path handles a branch task whose
	// completion ALREADY reached the child run's terminal state once —
	// i.e. the first completion ran handleRuntimeWorkflowStepComplete's
	// transition.Done branch, where the accepted-join cleanup
	// (cleanupTaskDeliveryArtifacts after completeRuntimeWorkflowBranch)
	// already retired the worktree, or the join was rejected and the
	// worktree was deliberately kept for this retry. Either way this
	// resume must NOT clean up again here: the join result decides, and
	// the first completion's cleanup already ran for the accepted case.
	result, err := s.completeRuntimeWorkflowBranch(principal.WorkspaceID, principal.Project, t, body.Outputs, stepStatus)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.workflow.branch.complete.resumed",
		ResourceType: "task",
		ResourceID:   principal.Project + "/" + agent + "/" + t.ID,
		Summary:      "Archived branch re-report resumed the parent join",
		After: map[string]any{
			"taskId":   t.ID,
			"branchId": strings.TrimSpace(t.Vars[workflowBranchIDVar]),
			"allDone":  result.AllDone,
		},
		Request: r,
	})
	if err := s.advanceParentAfterBranchCompletion(principal.WorkspaceID, principal.Project, result, r); err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task":    taskToRow(t, principal.Project, agent, true),
		"branch":  result.Branch,
		"allDone": result.AllDone,
	})
}

func (s *Server) completeRuntimeWorkflowBranchHTTP(w http.ResponseWriter, r *http.Request, principal runtimeAgentPrincipal, t *entity.Task, agent string, body runtimeTaskCompleteBody) {
	doneStatus := normalizeDoneStatus(body.Status, body.Error)
	stepStatus := "completed"
	if doneStatus == entity.TaskStatusDoneFailed {
		stepStatus = "failed"
	}
	result, err := s.completeRuntimeWorkflowBranch(principal.WorkspaceID, principal.Project, t, body.Outputs, stepStatus)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	prev := t.Status
	t.Status = doneStatus
	t.Summary = strings.TrimSpace(body.Summary)
	t.LastError = strings.TrimSpace(body.Error)
	t.UpdatedAt = now
	entity.ApplyStatusTimestamps(t, prev, now)
	if err := s.ts.ArchiveTask(principal.Project, agent, t); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.advanceParentAfterBranchCompletion(principal.WorkspaceID, principal.Project, result, r); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.workflow.branch.complete",
		ResourceType: "task",
		ResourceID:   principal.Project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent completed workflow branch",
		After:        taskToRow(t, principal.Project, agent, true),
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task":    taskToRow(t, principal.Project, agent, true),
		"branch":  result.Branch,
		"allDone": result.AllDone,
	})
}

func (s *Server) handleRuntimeWorkflowStepComplete(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body runtimeTaskCompleteBody
	if r.Body != nil && r.ContentLength != 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	t, agent, _, err := s.runtimeFindTask(principal, r.PathValue("id"), body.Agent)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	doneStatus := normalizeDoneStatus(body.Status, body.Error)
	stepStatus := "completed"
	if doneStatus == entity.TaskStatusDoneFailed {
		stepStatus = "failed"
	}
	now := time.Now().UTC()
	t.Summary = strings.TrimSpace(body.Summary)
	t.LastError = strings.TrimSpace(body.Error)
	t.UpdatedAt = now
	// S2-2 (reviewer P1-2): an archived branch child re-reporting after a
	// partial join (agent retried past a network blip, or the runtime
	// crashed between child-run completion and the parent join) must
	// resume the join, not get a stale rejection. The step-level
	// CompleteAndAdvance below would bounce off the terminal child run
	// with ErrStaleWorkflowTransition; detect that shape up front and
	// route the report straight to the idempotent branch-join path.
	if strings.TrimSpace(t.Vars[workflowBranchIDVar]) != "" && t.Status.IsTerminal() {
		s.resumeArchivedBranchJoin(w, r, principal, t, agent, body, stepStatus)
		return
	}
	// Platform-owned gates. An agent reporting that CI is ready is not evidence
	// that CI is ready: recompute here and hold the step when it is not. A failed
	// completion is never held — trapping an agent that is trying to report a
	// problem is how you lose the report.
	if stepStatus == "completed" {
		decision, isGate, gateErr := s.ciReadyGateDecisionForTask(r.Context(), principal.WorkspaceID, principal.Project, t)
		if isGate && gateErr != nil {
			log.Printf("[ci-ready-gate] evaluation failed for task %s (project %s): %v", t.ID, principal.Project, gateErr)
			s.jsonError(w, http.StatusInternalServerError, "could not evaluate the CI readiness gate; the step was not completed")
			return
		}
		if isGate && decision.Blocked {
			retryable := ciReadyGateIsRetryable(decision)
			status, code := http.StatusBadRequest, ErrCodeValidationFailed
			if retryable {
				status, code = http.StatusConflict, ErrCodeConflict
			}
			s.writeAPIError(w, status, code, ciReadyGateMessage(decision), map[string]any{
				"gate":       ciReadyGateKind,
				"stepId":     decision.StepID,
				"cause":      decision.Cause,
				"retryable":  retryable,
				"needsHuman": decision.NeedsHuman,
				"detail":     decision.Detail,
			})
			return
		}
	}
	// Branch-completion precheck (S2-1 fix): a branch child task whose
	// workflow reaches its terminal step with THIS completion triggers the
	// parent-side branch join, which runs the QA touched_paths gate. That
	// gate used to fire only AFTER completeRuntimeWorkflowStep had already
	// persisted the child run's terminal transition and the task's
	// done_success — a rejected branch output left the task finished, the
	// agent gone, and the branch instance stuck "running" forever (join
	// barrier never satisfied; S2 dogfood wfr-07249bes). Run the identical
	// gate here BEFORE any persistence: on rejection nothing is written, the
	// task stays in_progress, and the agent can correct and re-report. The
	// gate is fail-open for intermediate branch steps (their completion does
	// not reach the join) and for failed completions, mirroring the
	// store-level check.
	if stepStatus == "completed" && strings.TrimSpace(t.Vars[workflowBranchIDVar]) != "" {
		if err := s.precheckBranchJoinGate(principal.WorkspaceID, principal.Project, t, body.Outputs); err != nil {
			s.jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	transition, transitioned, err := s.completeRuntimeWorkflowStep(principal.WorkspaceID, principal.Project, t, body.Outputs, stepStatus)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !transitioned {
		s.jsonError(w, http.StatusBadRequest, "task is not attached to an active workflow")
		return
	}
	// After an initialization sync step the agent-created remote exists but the
	// control plane has no record of it; adopt the origin URL into
	// RemoteProjectID and bind the default runner so the ci_ready step's
	// pipeline evidence can actually run (p15 canary §8.4).
	s.adoptRemoteIfNeededAfterStep(principal.Project, t, stepStatus, transition.Run.DefinitionID, transition.Current.StepID)
	if transition.Done {
		prev := t.Status
		t.Status = entity.TaskStatusDoneSuccess
		if stepStatus == "failed" {
			t.Status = entity.TaskStatusDoneFailed
		}
		entity.ApplyStatusTimestamps(t, prev, now)
		// S2 v2 seam fix: for BRANCH completions, the authoritative join
		// gate (completeRuntimeWorkflowBranch → checkBranchQAGate) still
		// needs this task's delivery worktree — the QA baseline delta is
		// measured against the capture-time baseline ON DISK. The old
		// ordering cleaned up the worktree BEFORE the join ran, so the
		// gate silently degraded to the project-workspace fallback surface
		// and phantom-rejected honest deliveries. Cleanup for branch tasks
		// now happens AFTER the join + parent advance (see below); linear
		// tasks keep the immediate cleanup.
		isBranchCompletion := strings.TrimSpace(t.Vars[workflowBranchIDVar]) != ""
		if t.Status == entity.TaskStatusDoneSuccess {
			s.captureTaskCompletionSnapshot(t)
			s.syncTaskCompletionRemote(principal.Project, t)
			if !isBranchCompletion {
				s.cleanupTaskDeliveryArtifacts(principal.Project, t.ID)
			}
		}
		if err := s.ts.ArchiveTask(principal.Project, agent, t); err != nil {
			s.serverError(w, err)
			return
		}
		if t.CreatedBy != "" && !isBranchCompletion {
			s.notifyTaskDone(t, principal.Project, agent)
		}
		if isBranchCompletion {
			branchResult, err := s.completeRuntimeWorkflowBranch(principal.WorkspaceID, principal.Project, t, body.Outputs, stepStatus)
			if err != nil {
				s.jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			if err := s.advanceParentAfterBranchCompletion(principal.WorkspaceID, principal.Project, branchResult, r); err != nil {
				s.serverError(w, err)
				return
			}
			// Join succeeded: the branch delivery is accepted, so the
			// worktree can now be retired (same contract as the linear
			// path above, just ordered after the gate that measures it).
			if t.Status == entity.TaskStatusDoneSuccess {
				s.cleanupTaskDeliveryArtifacts(principal.Project, t.ID)
			}
		}
	} else if err := s.activateNextWorkflowStep(principal.WorkspaceID, principal.Project, agent, t, transition, r); err != nil {
		s.serverError(w, err)
		return
	}
	// Persist the next human-review assignee before projecting the review card.
	// The card's dual-CAS token is derived from task.UpdatedAt; projecting it
	// before activateNextWorkflowStep would sign the previous agent step and
	// make every click fail with 409 Conflict.
	s.notifyTaskThreadStepTransition(principal.WorkspaceID, principal.Project, t, transition, body.Outputs)
	archived := transition.Done && t.Status.IsTerminal()
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.workflow.step.complete",
		ResourceType: "task",
		ResourceID:   principal.Project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent completed workflow step",
		After:        taskToRow(t, principal.Project, agent, archived),
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(taskToRow(t, principal.Project, agent, archived))
}

// workflowStepByID is a local alias over the workflow package's step lookup.
func workflowStepByID(steps []entity.WorkflowStep, id string) (entity.WorkflowStep, bool) {
	for i := range steps {
		if strings.TrimSpace(steps[i].ID) == strings.TrimSpace(id) {
			return steps[i], true
		}
	}
	return entity.WorkflowStep{}, false
}

// workflowStepIsTerminal reports whether completing `step` ends the
// workflow run: a step with no outgoing edges is terminal, and for steps
// WITH outgoing edges the engine decides the next step from conditions —
// conservatively treat those as non-terminal here so the precheck stays
// fail-open and never blocks a completion the real transition would have
// allowed through.
func workflowStepIsTerminal(def entity.WorkflowDefinition, step entity.WorkflowStep) bool {
	for _, e := range def.Edges {
		if strings.TrimSpace(e.From) == strings.TrimSpace(step.ID) {
			return false
		}
	}
	return true
}

// branchOutputFields resolves the output contract the parent-side branch
// join validates against: the branch's own declared OutputFields. For
// generated single-step branch definitions this equals the start step's
// OutputFields; for embedded multi-step branch workflows the branch-level
// declaration is the contract that survives the whole branch.
func branchOutputFields(def entity.WorkflowDefinition, start entity.WorkflowStep) []entity.WorkflowField {
	// A branch definition that came from workflowDefinitionForBranch carries
	// the branch contract on its start step (single-step case). Definitions
	// supplied via branch.Workflow keep their own step contracts; the
	// branch-level join validates whatever the start step declares, which is
	// the contract the engine's aggregate actually maps through.
	if len(start.OutputFields) > 0 {
		return append([]entity.WorkflowField{}, start.OutputFields...)
	}
	for _, st := range def.Steps {
		if strings.TrimSpace(st.ID) == strings.TrimSpace(def.StartStepID) {
			return append([]entity.WorkflowField{}, st.OutputFields...)
		}
	}
	return nil
}

// precheckBranchJoinGate mirrors the branch-join QA gate BEFORE the child
// run's own transition is persisted. It must predict, not re-enforce: the
// authoritative gate still runs inside CompleteBranchAndMaybeAdvance after
// the child run completes. Prediction logic: the child workflow completes
// with this call when its active step is the LAST step of the child
// definition (any outgoing conditional edges are resolved by the same
// engine logic the transition will use; a terminal step has no outgoing
// edges to another step). For multi-step branches the intermediate
// completions return nil (fail-open) — the precheck fires only when the
// completion would actually reach the parent join.
func (s *Server) precheckBranchJoinGate(workspaceID, project string, t *entity.Task, outputs map[string]string) error {
	if s == nil || s.controlDB == nil || t == nil {
		return nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	// S2-2.3 (review round, item 2): the join gate measures the BRANCH's own
	// delivery — the branch is developed in its OWN worktree against its OWN
	// capture-time baseline, and the join runs BEFORE any merge, so the
	// parent task's worktree cannot hold the branch's delta yet. Both the
	// precheck and the authoritative join gate (completeRuntimeWorkflowBranch
	// passes this same taskID into CompleteBranchAndMaybeAdvance) must
	// resolve THIS branch task's worktree + baseline. The parent worktree is
	// only relevant AFTER the merge, which happens later in the pipeline.
	// (workflowRootTaskIDVar stays on the task for run-advance bookkeeping;
	// it is deliberately NOT used as the QA measurement key anymore.)
	wfStore.WorktreeResolver = func(project, taskID string) string {
		return s.resolveTaskWorktreeDir(project, taskID)
	}
	// S2-2 (reviewer P0-1): the precheck must measure against the same
	// trusted baseline the authoritative join gate will use. S2 v2 seam
	// fix: scope the lookup to THIS request's workspace so the read side
	// hits the rows the capture path wrote.
	wfStore.QABaselineLookup = s.QABaselineLookupForWorkspace(workspaceID)
	run, ok, err := wfStore.RunForTask(project, t.ID)
	if err != nil || !ok {
		// No child run: the store-level gate will judge later; nothing to
		// predict here.
		return nil
	}
	def, ok, err := wfStore.RunDefinition(run)
	if err != nil || !ok {
		return nil
	}
	current, ok := workflowStepByID(def.Steps, run.ActiveStepID)
	if !ok {
		return nil
	}
	if !workflowStepIsTerminal(def, current) {
		// Intermediate branch step: this completion advances within the
		// branch workflow and never reaches the parent join.
		return nil
	}
	// The join consumes the PARENT definition's branch contract (S2-2,
	// reviewer P1-1): CompleteBranchAndMaybeAdvance maps outputs through
	// branchDef.OutputFields from the PARENT run's snapshot, not the child
	// definition's step fields. Reading the child's own fields can pass a
	// contract the join will reject (or vice versa). Fail closed when the
	// parent run cannot be read: predicting nothing is safer than
	// predicting a contract that is not the one the join enforces.
	parentRunID := strings.TrimSpace(t.Vars[workflowRunIDVar])
	parentRun, parentFound, err := wfStore.RunByID(project, parentRunID)
	if err != nil {
		return fmt.Errorf("branch join precheck: read parent run %s: %w", parentRunID, err)
	}
	branchStep := entity.WorkflowStep{ID: current.ID, Title: current.Title}
	// S2-2.2 (review round P1): remember whether the parent-side contract
	// lookup itself errored (transient infrastructure), separately from a
	// clean miss (contract genuinely absent). A transient error must become
	// a retryable 5xx below, never a 4xx contract verdict.
	var parentReadError error
	// Preferred contract source: the branch INSTANCE's frozen OutputFields
	// (S2-2, reviewer P1-1) — captured when the run started, immune to later
	// parent-definition edits, and exactly what the join's aggregate maps
	// through for new completions. Fallback: the parent definition snapshot.
	if parentFound {
		if instances, ierr := wfStore.ListBranchInstances(parentRun.ID); ierr == nil {
			for _, inst := range instances {
				if inst.BranchID == strings.TrimSpace(t.Vars[workflowBranchIDVar]) && inst.StepID == strings.TrimSpace(t.Vars[workflowStepIDVar]) && len(inst.OutputFields) > 0 {
					branchStep.OutputFields = append([]entity.WorkflowField{}, inst.OutputFields...)
					break
				}
			}
		} else if parentReadError == nil {
			parentReadError = ierr
		}
		if len(branchStep.OutputFields) == 0 {
			parentDef, ok, derr := wfStore.RunDefinition(parentRun)
			if derr != nil {
				if parentReadError == nil {
					parentReadError = derr
				}
			} else if ok {
				if parentStep, ok := workflowStepByID(parentDef.Steps, strings.TrimSpace(t.Vars[workflowStepIDVar])); ok {
					if branchDef, ok := workflowstore.WorkflowBranchByID(parentStep.Branches, strings.TrimSpace(t.Vars[workflowBranchIDVar])); ok {
						branchStep.OutputFields = append([]entity.WorkflowField{}, branchDef.OutputFields...)
					}
				}
			}
		}
	}
	if len(branchStep.OutputFields) == 0 {
		// Parent contract unavailable: fall back to the child's start-step
		// fields ONLY for generated single-step branches (where they are
		// identical by construction); embedded multi-step branches must
		// fail closed — their contract lives in the parent snapshot and a
		// wrong prediction here would double-enforce a foreign contract.
		// S2-2.2 (review round P1): distinguish "the contract genuinely does
		// not exist yet" from "we could not read it right now" — the latter
		// is a transient infrastructure failure and must surface as a 5xx
		// (retryable), not a 4xx rejection the runtime treats as a verdict.
		if parentReadError != nil {
			return fmt.Errorf("branch join precheck: parent contract lookup for %s/%s failed transiently: %w", t.Vars[workflowStepIDVar], t.Vars[workflowBranchIDVar], parentReadError)
		}
		if branchHasEmbeddedWorkflow(def) {
			return fmt.Errorf("branch join precheck: parent branch contract for %s/%s is unavailable — refusing to pre-validate against the child step's fields", t.Vars[workflowStepIDVar], t.Vars[workflowBranchIDVar])
		}
		branchStep.OutputFields = branchOutputFields(def, current)
	}
	values, err := workflowstore.NormalizeWorkflowOutputValuesForPreview(branchStep, outputs, "", "")
	if err != nil {
		// Output normalization failures (missing required fields) also
		// deserve a zero-write rejection: the post-transition join would
		// fail the same way.
		return err
	}
	// S2-2.3: measurement owner is the BRANCH task (t.ID) — same key the
	// authoritative join gate uses.
	return wfStore.PreviewBranchQAGate(project, t.ID, branchStep, values)
}

// branchHasEmbeddedWorkflow reports whether a branch definition was derived
// from an embedded branch.Workflow (multi-step custom branch) rather than a
// generated single-step stub: the generated stub's start step carries the
// parent branch contract verbatim, an embedded one does not.
func branchHasEmbeddedWorkflow(def entity.WorkflowDefinition) bool {
	if len(def.Steps) != 1 {
		return true
	}
	// Generated stubs always use the canonical "start" step id.
	return strings.TrimSpace(def.Steps[0].ID) != "start"
}

func (s *Server) completeRuntimeWorkflowStep(workspaceID, project string, t *entity.Task, outputs map[string]string, stepStatus string) (workflowstore.TransitionResult, bool, error) {
	var result workflowstore.TransitionResult
	if s == nil || s.controlDB == nil || t == nil || strings.TrimSpace(workspaceID) == "" {
		return result, false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	// Real-change cross-check for the QA touched_paths checkpoint
	// (round-18 P0-4): agent sandbox completions are the primary QA path,
	// so the resolver must ride this store too.
	wfStore.WorktreeResolver = func(project, taskID string) string {
		return s.resolveTaskWorktreeDir(project, taskID)
	}
	// S2-2 (reviewer P0-1): linear QA steps keep the legacy whitelist
	// surface, but a lost/tampered baseline still has to fail closed.
	// S2 v2 seam fix: workspace-scoped lookup (see precheckBranchJoinGate).
	wfStore.QABaselineLookup = s.QABaselineLookupForWorkspace(workspaceID)
	if _, ok, err := wfStore.RunForTask(project, t.ID); err != nil || !ok {
		return result, false, err
	}
	output := strings.TrimSpace(t.Summary)
	if output == "" {
		output = strings.TrimSpace(t.LastError)
	}
	updateTaskRemoteMR(t, outputs)
	result, err := wfStore.CompleteAndAdvance(project, t.ID, t.Summary, output, outputs, stepStatus)
	if err != nil {
		return result, false, err
	}
	return result, result.Next != nil || result.Done, nil
}

func (s *Server) completeRuntimeWorkflowBranch(workspaceID, project string, t *entity.Task, outputs map[string]string, stepStatus string) (workflowstore.BranchTransitionResult, error) {
	var result workflowstore.BranchTransitionResult
	if s == nil || s.controlDB == nil || t == nil || strings.TrimSpace(workspaceID) == "" {
		return result, fmt.Errorf("workflow store is not available")
	}
	rootTaskID := strings.TrimSpace(t.Vars[workflowRootTaskIDVar])
	runID := strings.TrimSpace(t.Vars[workflowRunIDVar])
	stepID := strings.TrimSpace(t.Vars[workflowStepIDVar])
	branchID := strings.TrimSpace(t.Vars[workflowBranchIDVar])
	if rootTaskID == "" || runID == "" || stepID == "" || branchID == "" {
		return result, fmt.Errorf("workflow branch metadata is incomplete")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	// Real-change cross-check for the QA touched_paths checkpoint
	// (round-19 P0): branch completions run the same gate as linear
	// completions, so this store needs the resolver too.
	// S2-2.3 (review round, item 2): the gate receives t.ID — the BRANCH
	// task — so it measures the branch's own delivery worktree against the
	// branch's own capture-time baseline (the baseline lookup keys on the
	// same taskID). The join still advances the parent run; only the
	// measurement surface changed.
	wfStore.WorktreeResolver = func(project, taskID string) string {
		return s.resolveTaskWorktreeDir(project, taskID)
	}
	// S2-2 (reviewer P0-1): the authoritative join gate reads the trusted
	// baseline from the control plane, never from the agent-writable
	// worktree copy. S2 v2 seam fix: workspace-scoped lookup (see
	// precheckBranchJoinGate) — the zero-arg adapter resolved an
	// empty-workspace store and silently missed every production row.
	wfStore.QABaselineLookup = s.QABaselineLookupForWorkspace(workspaceID)
	summary := strings.TrimSpace(t.Summary)
	if summary == "" {
		summary = strings.TrimSpace(t.LastError)
	}
	// S2-2.3 (review round, item 2): the run handle stays the PARENT task
	// (rootTaskID) — it owns the active run/step/branch state — while the QA
	// measurement owner is the BRANCH task (t.ID): its own worktree + its
	// own capture-time baseline. The join advances the parent run; only the
	// measurement surface is the branch delivery.
	return wfStore.CompleteBranchAndMaybeAdvance(project, rootTaskID, runID, stepID, branchID, t.ID, summary, outputs, stepStatus)
}

func (s *Server) advanceParentAfterBranchCompletion(workspaceID, project string, result workflowstore.BranchTransitionResult, r *http.Request) error {
	rootTaskID := strings.TrimSpace(result.Transition.Run.TaskID)
	if rootTaskID == "" {
		return nil
	}
	root, rootAgent, err := s.findTaskInProject(project, rootTaskID)
	if err != nil || root == nil {
		return nil
	}
	now := time.Now().UTC()
	if result.Branch.Status == "failed" {
		root.Status = entity.TaskStatusBlocked
		root.LastError = strings.TrimSpace(result.Branch.Summary)
		root.UpdatedAt = now
		return s.ts.PersistTask(project, rootAgent, root)
	}
	if !result.AllDone {
		return nil
	}
	if result.Transition.Done {
		prev := root.Status
		root.Status = entity.TaskStatusDoneSuccess
		root.Summary = strings.TrimSpace(result.Transition.Current.Summary)
		root.UpdatedAt = now
		entity.ApplyStatusTimestamps(root, prev, now)
		s.captureTaskCompletionSnapshot(root)
		s.syncTaskCompletionRemote(project, root)
		s.cleanupTaskDeliveryArtifacts(project, root.ID)
		if err := s.ts.ArchiveTask(project, rootAgent, root); err != nil {
			return err
		}
		if root.CreatedBy != "" {
			s.notifyTaskDone(root, project, rootAgent)
		}
		return nil
	}
	return s.activateNextWorkflowStep(workspaceID, project, rootAgent, root, result.Transition, r)
}

func (s *Server) runtimeTaskHasWorkflow(workspaceID, project, taskID string) bool {
	if s == nil || s.controlDB == nil || strings.TrimSpace(workspaceID) == "" {
		return false
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	_, ok, err := wfStore.RunForTask(project, taskID)
	return err == nil && ok
}

func (s *Server) activateNextWorkflowStep(workspaceID, project, previousAgent string, completed *entity.Task, transition workflowstore.TransitionResult, r *http.Request) error {
	if completed == nil || transition.Done || transition.Next == nil || transition.NextInst == nil {
		return nil
	}
	if transition.Next.Type == "parallel_stage" {
		return s.activateParallelWorkflowStep(workspaceID, project, previousAgent, completed, transition, r)
	}
	inst := transition.NextInst
	now := time.Now().UTC()
	if inst.ActorType == "agent" && strings.TrimSpace(inst.ActorID) != "" {
		nextAgent := strings.TrimSpace(inst.ActorID)
		if err := s.moveWorkflowTaskToAgent(workspaceID, project, previousAgent, nextAgent, completed, entity.TaskStatusPending, now); err != nil {
			return err
		}
		return s.fireTaskTriggerOrQueueRuntime(workspaceID, project, nextAgent, completed, r, "workflow task "+completed.ID)
	}
	if inst.ActorType == "human" {
		reviewer := strings.TrimSpace(inst.ActorID)
		if reviewer == "" {
			return fmt.Errorf("workflow human review step %q has no assigned user", inst.StepID)
		}
		if err := s.validateIdentity(reviewer, "workflow reviewer"); err != nil {
			return err
		}
		completed.Status = entity.TaskStatusAwaitingConfirmation
		completed.Assignee = reviewer
		completed.UpdatedAt = now
		completed.FinishedAt = nil
		completed.ArchivedAt = nil
		s.annotateTaskAssignee(workspaceID, project, completed)
		if err := s.ts.PersistTask(project, previousAgent, completed); err != nil {
			return err
		}
		if err := s.ts.AddToInbox(&entity.InboxItem{
			TaskID:      completed.ID,
			Project:     project,
			Agent:       previousAgent,
			To:          reviewer,
			Title:       completed.Title,
			Summary:     strings.TrimSpace(inst.InputArtifact),
			ActionHint:  "Review the workflow step and choose approved or needs_changes.",
			ActionItems: []string{"Open the task workflow panel.", "Review the previous step output.", "Approve or request changes with clear comments."},
		}); err != nil {
			return err
		}
		if strings.TrimSpace(workspaceID) != "" {
			wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
			def, found, err := wfStore.RunDefinition(transition.Run)
			if err == nil && found {
				s.fireWorkflowStepTriggers(workspaceID, workflowTriggerEvent{
					Type:       "workflow.human_review.required",
					Project:    project,
					TaskID:     completed.ID,
					TaskTitle:  completed.Title,
					Run:        transition.Run,
					Definition: def,
					Step:       *transition.Next,
					Instance:   *transition.NextInst,
				}, r)
			}
		}
		return nil
	}
	return nil
}

func (s *Server) moveWorkflowTaskToAgent(workspaceID, project, previousAgent, nextAgent string, task *entity.Task, status entity.TaskStatus, now time.Time) error {
	if task == nil {
		return fmt.Errorf("task is required")
	}
	previousAgent = strings.TrimSpace(previousAgent)
	nextAgent = strings.TrimSpace(nextAgent)
	if nextAgent == "" {
		return fmt.Errorf("workflow next agent is required")
	}
	task.Status = status
	task.Assignee = project + "/" + nextAgent
	task.UpdatedAt = now
	task.FinishedAt = nil
	task.LastError = ""
	// A workflow task reused across steps may carry a stale ArchivedAt from a
	// scheduler failure archive; if not cleared, ListTasks hides it from the
	// queue forever and the workflow deadlocks on the next agent step.
	task.ArchivedAt = nil
	s.annotateTaskAssignee(workspaceID, project, task)
	if previousAgent == nextAgent {
		if err := s.ts.PersistTask(project, nextAgent, task); err != nil {
			return err
		}
		return nil
	}
	if _, err := s.ts.GetTask(project, nextAgent, task.ID); err == nil {
		if err := s.ts.PersistTask(project, nextAgent, task); err != nil {
			return err
		}
	} else {
		var notFound *errs.NotFoundError
		if !errors.As(err, &notFound) {
			return err
		}
		if err := s.ts.AddTask(project, nextAgent, task); err != nil {
			return err
		}
	}
	if previousAgent != "" {
		_ = s.ts.DeleteTask(project, previousAgent, task.ID)
	}
	return nil
}

func (s *Server) activateParallelWorkflowStep(workspaceID, project, previousAgent string, completed *entity.Task, transition workflowstore.TransitionResult, r *http.Request) error {
	if completed == nil || transition.Next == nil || transition.NextInst == nil {
		return nil
	}
	step := *transition.Next
	if len(step.Branches) == 0 {
		return fmt.Errorf("parallel workflow step %q has no branches", step.Title)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	existing, err := wfStore.BranchInstancesForStep(transition.Run.ID, step.ID)
	if err != nil {
		return err
	}
	existingByBranch := make(map[string]bool, len(existing))
	for _, inst := range existing {
		existingByBranch[inst.BranchID] = true
	}
	now := time.Now().UTC()
	completed.Status = entity.TaskStatusInProgress
	completed.Assignee = project + "/" + previousAgent
	completed.UpdatedAt = now
	completed.FinishedAt = nil
	s.annotateTaskAssignee(workspaceID, project, completed)
	if err := s.ts.PersistTask(project, previousAgent, completed); err != nil {
		return err
	}
	// Review fix (P1-1): resolve the fan-out baseline ONCE, before the
	// loop — every branch must inherit the SAME immutable commit. The
	// parent task's BaseCommit is the frozen value from task creation;
	// resolving main per-branch inside the loop could observe a moving
	// ref and give branches different bases.
	fanoutBaseCommit := strings.TrimSpace(completed.BaseCommit)
	if fanoutBaseCommit == "" {
		gitRootForResolve := s.resolveProjectGitRoot(project)
		if _, statErr := os.Stat(filepath.Join(gitRootForResolve, ".git")); statErr == nil {
			resolved, rErr := s.worktreeMgr.ResolveBaseCommit(gitRootForResolve, "main")
			if rErr != nil {
				return fmt.Errorf("fan-out: resolve base commit: %w", rErr)
			}
			fanoutBaseCommit = resolved
		}
	}
	for _, branch := range step.Branches {
		branch.ID = strings.TrimSpace(branch.ID)
		if branch.ID == "" || existingByBranch[branch.ID] {
			continue
		}
		childDef := workflowDefinitionForBranch(transition.Run.DefinitionID, step, branch)
		if err := wfStore.SaveDefinition(&childDef); err != nil {
			return err
		}
		startStep, startInst, ok := workflowStartActor(childDef, transition.Run.ActorBindings)
		if !ok || startStep == nil || startInst == nil {
			return fmt.Errorf("parallel branch %q has no start step", branch.Title)
		}
		if strings.TrimSpace(startInst.ActorType) != "agent" || strings.TrimSpace(startInst.ActorID) == "" {
			return fmt.Errorf("parallel branch %q requires an agent actor binding for role %q", branch.Title, startStep.ActorRole)
		}
		nextAgent := strings.TrimSpace(startInst.ActorID)
		inputValues := workflowBranchInputValuesForFields(transition.Current, workflowBranchInputFields(branch, *startStep))
		inputArtifact := workflowBranchInputArtifact(step, transition.Current, branch, inputValues)
		branchTask := &entity.Task{
			ID: entity.NewTaskID(),
			// Review fix (P0-1 re-drive dedup): the idempotency key makes a
			// partial-failure re-drive reuse the SAME branch task instead
			// of creating a duplicate pending task under the same branchID
			// (which would also orphan the first task's worktree+baseline).
			// AddTask returns Conflict with t.ID rewritten to the existing
			// task's ID when the key matches an active task.
			IdempotencyKey: "fanout/" + transition.Run.ID + "/" + step.ID + "/" + branch.ID,
			Title:          strings.TrimSpace(completed.Title + " · " + branch.Title),
			Type:        completed.Type,
			Priority:    completed.Priority,
			Assignee:    project + "/" + nextAgent,
			CreatedBy:   completed.CreatedBy,
			Status:      entity.TaskStatusPending,
			Description: strings.TrimSpace(branch.Description),
			Prompt:      workflowBranchTaskPrompt(completed, step, branch, *startStep, inputArtifact),
			Labels:      append([]string{}, completed.Labels...),
			ParentID:    completed.ID,
			CreatedAt:   now,
			UpdatedAt:   now,
			Vars: map[string]string{
				workflowRootTaskIDVar: completed.ID,
				workflowRunIDVar:      transition.Run.ID,
				workflowStepIDVar:     step.ID,
				workflowBranchIDVar:   branch.ID,
			},
		}
		s.annotateTaskAssignee(workspaceID, project, branchTask)
		if branchTask.Type == "" {
			branchTask.Type = entity.TaskTypeChore
		}
		if err := s.ts.AddTask(project, nextAgent, branchTask); err != nil {
			var conflict *errs.ConflictError
			if !errors.As(err, &conflict) {
				return err
			}
			// Idempotency hit: branchTask.ID now points at the existing
			// task (AddTask rewrote it). Keep going — the materialization
			// below re-checks the worktree/baseline state of THAT task.
			log.Printf("[fanout] branch %q re-drive reuses existing task %s (idempotency key match)", branch.ID, branchTask.ID)
		}
		// S2 v2 materialization seam: fan-out branch tasks must enter the
		// world exactly like HTTP-created branch tasks do (internal/api/write.go
		// handlePostProjectTask): frozen baseCommit → materialized worktree →
		// capture-time QA baseline persisted to the control plane BEFORE the
		// child run exists. Without this, the join gate resolves the agent's
		// own directory (resolver fallback chain) with no control-plane
		// baseline and fails closed; the agent then works on a self-created
		// branch the platform never measured. Idempotency: EnsureWorktreeAt
		// returns the existing worktree (zero capture) on re-drive, and
		// AddTask dedup is handled by the existingByBranch guard above (a
		// re-drive skips branches that already have instances).
		if s.worktreeMgr != nil {
			gitRoot := s.resolveProjectGitRoot(project)
			if _, statErr := os.Stat(filepath.Join(gitRoot, ".git")); statErr == nil {
				if fanoutBaseCommit == "" {
					return fmt.Errorf("fan-out branch %q: project is a git repository but no base commit could be resolved", branch.ID)
				}
				branchTask.BaseCommit = fanoutBaseCommit
				branchTask.BaseBranch = "main"
				// branch.ID comes from workflow definitions (template authors),
				// so sanitize it before it lands in a git refspec argument.
				branchTask.BranchName = "feature/wf-" + gitworktree.SanitizeTaskID(branch.ID)
				wtDir, branchName, wtErr, qaCapture := s.worktreeMgr.EnsureWorktreeAt(gitRoot, branchTask.ID, fanoutBaseCommit, branchTask.BranchName)
				if wtErr != nil {
					return fmt.Errorf("fan-out branch %q: materialize worktree: %w", branch.ID, wtErr)
				}
				branchTask.WorktreeDir = wtDir
				branchTask.BranchName = branchName
				// S2-2 trust model: the control-plane capture-time baseline is
				// the only copy the join gate trusts. Persist BEFORE StartRun
				// makes the child run (and its attention signal) visible.
				//
				// Review fix (P0-1, partial-failure re-drive): a crash between
				// worktree creation and baseline persist leaves the worktree
				// on disk with NO control-plane baseline; a re-drive then hits
				// EnsureWorktreeAt's existing-directory path which returns a
				// ZERO capture, and skipping the persist would brick the
				// branch at join time (ErrQABaselineLost fail-closed, no
				// in-platform recovery). Detect that shape and rebuild from
				// scratch: retire the stale worktree (it was never measured
				// and no agent has ever seen it — the child run does not
				// exist yet on this path) and materialize again so the
				// capture happens. Worktree retirement failure here is
				// fatal for THIS fan-out (fail closed), never silent.
				if qaCapture.Baseline.Entries == nil {
					if _, found, lErr := wfStore.LoadQABaselinePayload(project, branchTask.ID); lErr != nil {
						return fmt.Errorf("fan-out branch %q: check existing qa baseline: %w", branch.ID, lErr)
					} else if !found {
						log.Printf("[fanout] branch %q task %s: worktree exists without a control-plane baseline (interrupted prior materialization); rebuilding", branch.ID, branchTask.ID)
						if _, _, cErr, _ := s.worktreeMgr.EnsureWorktreeAt(gitRoot, branchTask.ID, fanoutBaseCommit, ""); cErr != nil {
							// best-effort first close is not required; CleanupWorktree below is the real retirement
							_ = cErr
						}
						if err := s.worktreeMgr.CleanupWorktree(gitRoot, branchTask.ID); err != nil {
							return fmt.Errorf("fan-out branch %q: retire unmeasured worktree for clean re-materialization: %w", branch.ID, err)
						}
						wtDir, branchName, wtErr, qaCapture = s.worktreeMgr.EnsureWorktreeAt(gitRoot, branchTask.ID, fanoutBaseCommit, branchTask.BranchName)
						if wtErr != nil {
							return fmt.Errorf("fan-out branch %q: re-materialize worktree: %w", branch.ID, wtErr)
						}
						branchTask.WorktreeDir = wtDir
						branchTask.BranchName = branchName
					}
				}
				if qaCapture.Baseline.Entries != nil {
					if err := wfStore.CaptureQABaselineRecord(project, branchTask.ID, wtDir); err != nil {
						return fmt.Errorf("fan-out branch %q: persist qa baseline: %w", branch.ID, err)
					}
				}
				if err := s.ts.PersistTask(project, nextAgent, branchTask); err != nil {
					return err
				}
			} else {
				// Review fix (P1-2): a non-git project silently skipping
				// materialization left operators with an unexplained join
				// blockage later. Surface it now.
				log.Printf("[fanout] project %s: no git repository at %s; branch %q skips materialization (join gate will fail closed for non-git projects)", project, gitRoot, branch.ID)
			}
		}
		childRun, childInstances, err := wfStore.StartRun(project, branchTask.ID, childDef.ID, transition.Run.ActorBindings)
		if err != nil {
			return err
		}
		for i := range childInstances {
			if childInstances[i].StepID != childRun.ActiveStepID {
				continue
			}
			childInstances[i].InputArtifact = inputArtifact
			childInstances[i].InputValues = inputValues
			childInstances[i].ActorType = startInst.ActorType
			childInstances[i].ActorID = startInst.ActorID
			childInstances[i].UpdatedAt = now
			if err := wfStore.SaveStepInstance(&childInstances[i]); err != nil {
				return err
			}
			break
		}
		inst := &entity.WorkflowBranchInstance{
			ID:            entity.NewWorkflowBranchInstanceID(),
			RunID:         transition.Run.ID,
			StepID:        step.ID,
			BranchID:      branch.ID,
			Status:        "running",
			ActorType:     "agent",
			ActorID:       nextAgent,
			ChildTaskID:   branchTask.ID,
			ChildRunID:    childRun.ID,
			StartedAt:     now,
			UpdatedAt:     now,
			InputArtifact: inputArtifact,
			InputValues:   inputValues,
			// S2-2 (reviewer P1-1): freeze the branch's output contract on the
			// instance so the join precheck reads the contract the run was
			// STARTED with, immune to later parent-definition edits.
			OutputFields: append([]entity.WorkflowField{}, branch.OutputFields...),
		}
		if err := wfStore.SaveBranchInstance(inst); err != nil {
			return err
		}
		if err := s.fireTaskTriggerOrQueueRuntime(workspaceID, project, nextAgent, branchTask, r, "workflow branch task "+branchTask.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) fireTaskTriggerOrQueueRuntime(workspaceID, project, agent string, task *entity.Task, r *http.Request, reason string) error {
	if s == nil || task == nil {
		return nil
	}
	signalID := s.recordTaskAttentionSignal(workspaceID, project, agent, task, reason)
	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
	hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
	if err != nil || hb == nil || hb.Paused || !hb.HasTrigger(entity.TriggerOnTask) {
		if s.triggers != nil {
			s.triggers.Fire(project, agent, entity.TriggerOnTask, reason)
		}
		return nil
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil || meta == nil || !s.usesAssignedRuntimeNode(workspaceID, meta) {
		if s.triggers != nil {
			s.triggers.Fire(project, agent, entity.TriggerOnTask, reason)
		}
		return nil
	}
	// Task 0.4 gap 1: engine-internal readiness callback. Workflow continuity
	// dispatches here without an HTTP entry point, so a Docker outage or a
	// runtime node going offline between steps would otherwise be discovered
	// blindly by the next agent run. Fail fast instead: leave the task
	// pending and post a structured environment diagnosis.
	//
	// This deliberately returns nil on a withheld dispatch: the previous step
	// HAS completed and the run state advanced before this function runs, so
	// surfacing an error would turn a successful completion callback into a
	// 500 (agent retries, completion appears failed). The withheld next step
	// stays pending and is re-driven by the scheduler tick, whose readiness
	// gate withholds again until the environment recovers.
	if readiness := s.runtimeReadinessForExecution(workspaceID, meta); readiness.Blocking {
		detail := runtimeReadinessErrorMessage(readiness)
		s.addTaskSystemComment(project, agent, task,
			"[platform] 环境就绪检查未通过，下一步骤暂缓派发（fail-fast，不烧 token）：",
			detail+"\n上一步已完成，运行状态已保存。环境恢复后调度器会自动继续；也可手动启动。")
		s.auditLog(auditLogInput{
			WorkspaceID:  workspaceID,
			Action:       "workflow.dispatch.withheld",
			ResourceType: "task",
			ResourceID:   project + "/" + agent + "/" + task.ID,
			Summary:      "Workflow next step dispatch withheld: runtime not ready",
			After: map[string]any{
				"project": project,
				"agent":   agent,
				"taskId":  task.ID,
				"detail":  detail,
			},
			Request: r,
		})
		return nil
	}
	if s.hasActiveRuntimeRun(workspaceID, project, agent, task.ID) {
		return nil
	}
	now := time.Now().UTC()
	prev := task.Status
	task.Status = entity.TaskStatusInProgress
	task.UpdatedAt = now
	task.FinishedAt = nil
	entity.ApplyStatusTimestamps(task, prev, now)
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		return err
	}
	run, err := s.enqueueRuntimeTaskRun(workspaceID, project, agent, task, "", externalServerURL(r), requestUsername(r))
	if err != nil {
		task.Status = prev
		task.UpdatedAt = now
		_ = s.ts.UpdateTask(project, agent, task)
		return err
	}
	if signalID != "" {
		_ = s.controlDB.MarkAttentionSignalStatus(workspaceID, signalID, "handling")
	}
	hb.LastWakeup = &now
	hb.LastWakeupStatus = "running"
	hb.PID = 0
	_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "runtime_run.enqueue",
		ResourceType: "task",
		ResourceID:   project + "/" + agent + "/" + task.ID,
		Summary:      "Workflow task queued on runtime node",
		After: map[string]any{
			"project":      project,
			"agent":        agent,
			"taskId":       task.ID,
			"runtimeRunId": run.ID,
			"reason":       reason,
		},
		Request: r,
	})
	return nil
}

func workflowBranchInputValues(parent entity.WorkflowStepInstance, branch entity.WorkflowBranch) map[string]string {
	return workflowBranchInputValuesForFields(parent, branch.InputFields)
}

func workflowBranchInputValuesForFields(parent entity.WorkflowStepInstance, fields []entity.WorkflowField) map[string]string {
	out := make(map[string]string)
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		if name == "" {
			continue
		}
		if value := strings.TrimSpace(parent.InputValues[name]); value != "" {
			out[name] = value
			continue
		}
		if value := strings.TrimSpace(parent.OutputValues[name]); value != "" {
			out[name] = value
		}
	}
	if len(out) > 0 || len(fields) > 0 {
		return out
	}
	for key, value := range parent.InputValues {
		if strings.TrimSpace(value) != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	for key, value := range parent.OutputValues {
		if strings.TrimSpace(value) != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

func workflowBranchInputFields(branch entity.WorkflowBranch, startStep entity.WorkflowStep) []entity.WorkflowField {
	if len(branch.InputFields) > 0 {
		return branch.InputFields
	}
	return startStep.InputFields
}

func workflowBranchInputArtifact(step entity.WorkflowStep, parent entity.WorkflowStepInstance, branch entity.WorkflowBranch, inputs map[string]string) string {
	payload := map[string]any{
		"parallel_stage": map[string]string{"id": step.ID, "title": step.Title},
		"branch":         map[string]string{"id": branch.ID, "title": branch.Title},
		"inputs":         inputs,
		"upstream":       parent.OutputValues,
	}
	if len(branch.InputFields) > 0 {
		names := make([]string, 0, len(branch.InputFields))
		for _, field := range branch.InputFields {
			if name := strings.TrimSpace(field.Name); name != "" {
				names = append(names, name)
			}
		}
		payload["expected_input_fields"] = names
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

func workflowDefinitionForBranch(parentDefinitionID string, parent entity.WorkflowStep, branch entity.WorkflowBranch) entity.WorkflowDefinition {
	defID := workflowBranchDefinitionID(parentDefinitionID, parent.ID, branch.ID)
	now := time.Now().UTC()
	if branch.Workflow != nil && len(branch.Workflow.Steps) > 0 {
		def := *branch.Workflow
		def.ID = defID
		if strings.TrimSpace(def.Name) == "" {
			def.Name = branch.Title
		}
		if strings.TrimSpace(def.Description) == "" {
			def.Description = branch.Description
		}
		if def.Version == 0 {
			def.Version = 1
		}
		def.Scope = "branch"
		def.Project = ""
		if strings.TrimSpace(def.StartStepID) == "" {
			def.StartStepID = strings.TrimSpace(def.Steps[0].ID)
		}
		def.CreatedAt = now
		def.UpdatedAt = now
		return def
	}
	actorRole := strings.TrimSpace(branch.ActorRole)
	if actorRole == "" {
		actorRole = strings.TrimSpace(branch.ID)
	}
	return entity.WorkflowDefinition{
		ID:          defID,
		Name:        strings.TrimSpace(branch.Title),
		Description: strings.TrimSpace(branch.Description),
		Version:     1,
		Scope:       "branch",
		StartStepID: "start",
		Steps: []entity.WorkflowStep{{
			ID:           "start",
			Type:         "agent_task",
			Title:        strings.TrimSpace(branch.Title),
			Description:  strings.TrimSpace(branch.Description),
			ActorRole:    actorRole,
			InputFields:  append([]entity.WorkflowField{}, branch.InputFields...),
			OutputFields: append([]entity.WorkflowField{}, branch.OutputFields...),
			Position:     entity.WorkflowPosition{X: 80, Y: 120},
		}},
		Edges:     []entity.WorkflowEdge{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func workflowBranchDefinitionID(parentDefinitionID, stepID, branchID string) string {
	parts := []string{"branch", parentDefinitionID, stepID, branchID}
	replacer := strings.NewReplacer(" ", "-", "/", "-", "\\", "-", "_", "-")
	for i, part := range parts {
		parts[i] = replacer.Replace(strings.TrimSpace(strings.ToLower(part)))
	}
	return strings.Join(parts, "-")
}

func workflowBranchTaskPrompt(root *entity.Task, step entity.WorkflowStep, branch entity.WorkflowBranch, startStep entity.WorkflowStep, inputArtifact string) string {
	var b strings.Builder
	b.WriteString("Continue this branch sub-workflow from the current active step.\n\n")
	b.WriteString("Root task: ")
	b.WriteString(root.ID)
	b.WriteString(" — ")
	b.WriteString(root.Title)
	b.WriteString("\n")
	b.WriteString("Parallel stage: ")
	b.WriteString(step.Title)
	b.WriteString(" (")
	b.WriteString(step.ID)
	b.WriteString(")\n")
	b.WriteString("Branch: ")
	b.WriteString(branch.Title)
	b.WriteString(" (")
	b.WriteString(branch.ID)
	b.WriteString(")\n\n")
	if strings.TrimSpace(startStep.Title) != "" {
		b.WriteString("Current step: ")
		b.WriteString(startStep.Title)
		b.WriteString(" (")
		b.WriteString(startStep.ID)
		b.WriteString(")\n")
	}
	if strings.TrimSpace(startStep.Description) != "" {
		b.WriteString("Step goal:\n")
		b.WriteString(strings.TrimSpace(startStep.Description))
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(inputArtifact) != "" {
		b.WriteString("Input from previous workflow step:\n")
		b.WriteString(inputArtifact)
		b.WriteString("\n\n")
	}
	if len(startStep.OutputFields) > 0 {
		b.WriteString("Required structured outputs:\n")
		for _, field := range startStep.OutputFields {
			name := strings.TrimSpace(field.Name)
			if name == "" {
				continue
			}
			b.WriteString("- ")
			b.WriteString(name)
			if strings.TrimSpace(field.Description) != "" {
				b.WriteString(": ")
				b.WriteString(strings.TrimSpace(field.Description))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("This branch step is a milestone contract, not a one-shot trigger. Long-running work may span multiple wakeups, conversations, external coordination, and human clarification. Keep progress honest; only report completion when the milestone outcome is actually clear.\n\n")
	b.WriteString("When the current branch step is genuinely finished, report completion with:\n")
	b.WriteString("mga task step done --id \"$MULTIGENT_TASK_ID\" --summary \"...\"")
	if len(startStep.OutputFields) > 0 {
		b.WriteString(" --output-json '{\"field\":\"value\"}'")
	}
	b.WriteString("\n")
	if len(startStep.OutputFields) > 0 {
		b.WriteString("Prefer --output-json for workflow outputs. Use --output field=value only for simple ASCII field names with no spaces; Chinese field names or field names with spaces must use --output-json.\n")
	}
	b.WriteString("If clarification or review is needed before safe completion, contact the right collaborator or request confirmation instead of leaving the branch silently stuck.\n")
	b.WriteString("Do not complete the root task directly. Multigent will advance the parent workflow after this branch sub-workflow reaches a terminal node.\n")
	return b.String()
}

func normalizeDoneStatus(status, errText string) entity.TaskStatus {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "failed", "failure", "error", string(entity.TaskStatusDoneFailed):
		return entity.TaskStatusDoneFailed
	case "success", "succeeded", "done", string(entity.TaskStatusDoneSuccess):
		return entity.TaskStatusDoneSuccess
	}
	if strings.TrimSpace(errText) != "" {
		return entity.TaskStatusDoneFailed
	}
	return entity.TaskStatusDoneSuccess
}

func (s *Server) handleRuntimeTaskConfirmRequest(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	var body runtimeConfirmRequestBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	summary := strings.TrimSpace(body.Summary)
	if summary == "" {
		s.jsonError(w, http.StatusBadRequest, "summary is required")
		return
	}
	to := strings.TrimSpace(body.To)
	if to == "" {
		to = "human"
	}
	resolvedTo, err := s.resolveRuntimeRecipient(principal, to)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, agent, _, err := s.runtimeFindTask(principal, r.PathValue("id"), body.Agent)
	if err != nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if s.runtimeTaskHasWorkflow(principal.WorkspaceID, principal.Project, t.ID) {
		s.jsonError(w, http.StatusBadRequest, "workflow tasks must request human review through the workflow step route")
		return
	}
	now := time.Now().UTC()
	prev := t.Status
	t.Status = entity.TaskStatusAwaitingConfirmation
	t.ConfirmationReq = &entity.ConfirmationRequest{
		Summary:     summary,
		ActionHint:  strings.TrimSpace(body.ActionHint),
		ActionItems: body.ActionItems,
	}
	t.UpdatedAt = now
	entity.ApplyStatusTimestamps(t, prev, now)
	if err := s.ts.PersistTask(principal.Project, agent, t); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.ts.AddToInbox(&entity.InboxItem{
		TaskID:      t.ID,
		Project:     principal.Project,
		Agent:       agent,
		To:          resolvedTo,
		Title:       t.Title,
		Summary:     summary,
		ActionHint:  strings.TrimSpace(body.ActionHint),
		ActionItems: body.ActionItems,
	}); err != nil {
		s.serverError(w, err)
		return
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      runtimeAgentAddress(principal),
		Action:       "runtime.task.confirm_request",
		ResourceType: "task",
		ResourceID:   principal.Project + "/" + agent + "/" + t.ID,
		Summary:      "Runtime agent requested confirmation",
		After:        taskToRow(t, principal.Project, agent, false),
		Request:      r,
	})
	_ = json.NewEncoder(w).Encode(taskToRow(t, principal.Project, agent, false))
}

func (s *Server) handleRuntimeMessages(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "message.use")
	if !ok {
		return
	}
	mailbox := strings.TrimSpace(r.URL.Query().Get("mailbox"))
	if mailbox == "" {
		mailbox = runtimeAgentAddress(principal)
	}
	if mailbox != runtimeAgentAddress(principal) {
		s.jsonError(w, http.StatusForbidden, "runtime agents can only read their own mailbox")
		return
	}
	includeArchived := strings.EqualFold(r.URL.Query().Get("archived"), "all") || r.URL.Query().Get("includeArchived") == "1"
	var msgs []*entity.Message
	var err error
	if includeArchived {
		msgs, err = s.ts.ListAllMessages(mailbox)
	} else {
		msgs, err = s.ts.ListMessages(mailbox)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	rows := make([]msgRow, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		rows = append(rows, messageToRow(m, mailbox))
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func (s *Server) handleRuntimeContacts(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "message.use")
	if !ok {
		return
	}
	rows, err := s.runtimeContacts(principal.WorkspaceID, principal.Project)
	if err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func messageToRow(m *entity.Message, mailbox string) msgRow {
	sent := m.SentAt.UTC()
	var read *time.Time
	if m.ReadAt != nil {
		t := m.ReadAt.UTC()
		read = &t
	}
	var archived *time.Time
	if m.ArchivedAt != nil {
		t := m.ArchivedAt.UTC()
		archived = &t
	}
	return msgRow{
		ID:         m.ID,
		From:       m.From,
		To:         m.To,
		Subject:    m.Subject,
		Body:       m.Body,
		SentAt:     sent,
		ReadAt:     read,
		ArchivedAt: archived,
		Mailbox:    mailbox,
	}
}

func (s *Server) handleRuntimePostMessage(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "message.use")
	if !ok {
		return
	}
	var body runtimeMessageBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	bodyText := strings.TrimSpace(body.Body)
	if bodyText == "" {
		s.jsonError(w, http.StatusBadRequest, "body is required")
		return
	}
	recipients, err := normalizeToRecipients(body.To)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	resolved := make([]string, 0, len(recipients))
	for _, rec := range recipients {
		canonical, err := s.resolveRuntimeRecipient(principal, rec)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		resolved = append(resolved, canonical)
	}
	from := runtimeAgentAddress(principal)
	sentAt := time.Now().UTC()
	ids := make([]string, 0, len(resolved))
	for _, rec := range resolved {
		msg := &entity.Message{
			ID:      entity.NewMessageID(),
			From:    from,
			To:      rec,
			Subject: strings.TrimSpace(body.Subject),
			Body:    bodyText,
			ReplyTo: strings.TrimSpace(body.ReplyTo),
			SentAt:  sentAt,
		}
		if err := s.ts.SendMessage(msg); err != nil {
			s.serverError(w, err)
			return
		}
		ids = append(ids, msg.ID)
		signalID := s.recordMessageAttentionSignal(principal.WorkspaceID, msg)
		s.requestMessageAttentionWakeup(principal.WorkspaceID, msg, signalID)
		if parts := strings.SplitN(rec, "/", 2); len(parts) == 2 {
			s.triggers.Fire(parts[0], parts[1], entity.TriggerOnMessage, "from "+from)
		}
	}
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      from,
		Action:       "runtime.message.send",
		ResourceType: "message",
		ResourceID:   strings.Join(ids, ","),
		Summary:      "Runtime agent sent message",
		After:        map[string]any{"to": resolved, "subject": strings.TrimSpace(body.Subject)},
		Request:      r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"ids": ids})
}

func (s *Server) validateRuntimeRecipient(principal runtimeAgentPrincipal, recipient string) error {
	if err := s.validateIdentity(recipient, "to"); err != nil {
		return err
	}
	if recipient == "human" {
		return nil
	}
	project, _, ok := splitAgentMailbox(recipient)
	if !ok {
		return nil
	}
	if project != principal.Project {
		return fmt.Errorf("runtime agent can only message human or agents in project %s", principal.Project)
	}
	return nil
}

func (s *Server) handleRuntimeReplyMessage(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "message.use")
	if !ok {
		return
	}
	var body runtimeReplyMessageBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	bodyText := strings.TrimSpace(body.Body)
	if bodyText == "" {
		s.jsonError(w, http.StatusBadRequest, "body is required")
		return
	}
	mailbox := runtimeAgentAddress(principal)
	msgs, err := s.ts.ListAllMessages(mailbox)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var original *entity.Message
	for _, m := range msgs {
		if m != nil && m.ID == r.PathValue("id") {
			original = m
			break
		}
	}
	if original == nil {
		s.jsonError(w, http.StatusNotFound, "message not found")
		return
	}
	subject := strings.TrimSpace(body.Subject)
	if subject == "" {
		subject = original.Subject
		if subject != "" && !strings.HasPrefix(strings.ToLower(subject), "re:") {
			subject = "Re: " + subject
		}
	}
	msg := &entity.Message{
		ID:      entity.NewMessageID(),
		From:    mailbox,
		To:      original.From,
		Subject: subject,
		Body:    bodyText,
		ReplyTo: original.ID,
		SentAt:  time.Now().UTC(),
	}
	if err := s.validateRuntimeRecipient(principal, msg.To); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.ts.SendMessage(msg); err != nil {
		s.serverError(w, err)
		return
	}
	signalID := s.recordMessageAttentionSignal(principal.WorkspaceID, msg)
	s.requestMessageAttentionWakeup(principal.WorkspaceID, msg, signalID)
	if parts := strings.SplitN(msg.To, "/", 2); len(parts) == 2 {
		s.triggers.Fire(parts[0], parts[1], entity.TriggerOnMessage, "from "+mailbox)
	}
	_ = s.ts.MarkMessageRead(mailbox, original.ID)
	s.auditLog(auditLogInput{
		WorkspaceID:  principal.WorkspaceID,
		ActorType:    "agent",
		ActorID:      mailbox,
		Action:       "runtime.message.reply",
		ResourceType: "message",
		ResourceID:   msg.ID,
		Summary:      "Runtime agent replied to message",
		After:        messageToRow(msg, msg.To),
		Request:      r,
	})
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": msg.ID})
}
