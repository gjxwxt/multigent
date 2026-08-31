package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/codehost"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/store"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

type workflowCreateBody struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	TemplateID  string                `json:"templateId"`
	Locale      string                `json:"locale"`
	StartStepID string                `json:"startStepId"`
	Steps       []entity.WorkflowStep `json:"steps"`
	Edges       []entity.WorkflowEdge `json:"edges"`
}

type taskWorkflowResponse struct {
	Definition entity.WorkflowDefinition       `json:"definition"`
	Run        entity.WorkflowRun              `json:"run"`
	Steps      []entity.WorkflowStepInstance   `json:"steps"`
	Branches   []entity.WorkflowBranchInstance `json:"branches,omitempty"`
	History    []entity.WorkflowStepEvent      `json:"history,omitempty"`
	DocTitles  map[string]string               `json:"docTitles,omitempty"`
}

type taskWorkflowReviewResponse struct {
	OK                          bool                         `json:"ok"`
	TaskID                      string                       `json:"taskId"`
	WorkflowRunID               string                       `json:"workflowRunId"`
	Status                      string                       `json:"status"`
	ActiveStepID                string                       `json:"activeStepId,omitempty"`
	CurrentAssigneeType         string                       `json:"currentAssigneeType"`
	CurrentAssigneeID           string                       `json:"currentAssigneeId"`
	CurrentAssigneeMembershipID string                       `json:"currentAssigneeMembershipId"`
	Done                        bool                         `json:"done"`
	NextActor                   *entity.WorkflowActorBinding `json:"nextActor,omitempty"`
}

type workflowReviewBody struct {
	Decision string            `json:"decision"`
	Comments string            `json:"comments"`
	Outputs  map[string]string `json:"outputs"`
}

func (s *Server) workflowStoreForRequest(w http.ResponseWriter, r *http.Request) (*workflowstore.Store, bool) {
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return nil, false
	}
	if !s.checkWorkspaceAccess(w, r, workspaceID) {
		return nil, false
	}
	return workflowstore.NewStore(s.controlDB, workspaceID), true
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	wfStore, ok := s.workflowStoreForRequest(w, r)
	if !ok {
		return
	}
	defs, err := wfStore.ListDefinitions()
	if err != nil {
		s.serverError(w, err)
		return
	}
	workspaceID, _ := s.currentWorkspaceID()
	provenance, _ := s.playbookProvenanceMap(workspaceID, "workflow")
	for i := range defs {
		if p, ok := provenance[playbookProvenanceKey("", defs[i].ID)]; ok {
			cp := p
			defs[i].Provenance = &cp
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"workflows": defs})
}

func (s *Server) handleListWorkflowTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.workflowStoreForRequest(w, r); !ok {
		return
	}
	locale := strings.TrimSpace(r.URL.Query().Get("locale"))
	_ = json.NewEncoder(w).Encode(map[string]any{"templates": workflowstore.Templates(locale)})
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	wfStore, ok := s.workflowStoreForRequest(w, r)
	if !ok {
		return
	}
	def, found, err := wfStore.Definition(r.PathValue("workflowId"))
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonError(w, http.StatusNotFound, "workflow not found")
		return
	}
	def.Provenance = s.playbookObjectProvenanceForRequest(r, "workflow", "", def.ID)
	_ = json.NewEncoder(w).Encode(def)
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	var body workflowCreateBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if strings.TrimSpace(body.TemplateID) != "" {
		def, found := workflowstore.DefinitionFromTemplate(body.TemplateID, body.Locale, name)
		if !found {
			s.jsonError(w, http.StatusNotFound, "workflow template not found")
			return
		}
		wfStore, ok := s.workflowStoreForRequest(w, r)
		if !ok {
			return
		}
		if err := wfStore.SaveDefinition(&def); err != nil {
			s.serverError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(def)
		return
	}
	if name == "" {
		s.jsonError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(body.Steps) == 0 {
		s.jsonError(w, http.StatusBadRequest, "at least one step is required")
		return
	}
	start := strings.TrimSpace(body.StartStepID)
	if start == "" {
		start = body.Steps[0].ID
	}
	now := time.Now().UTC()
	def := entity.WorkflowDefinition{
		ID:          entity.NewWorkflowID(),
		Name:        name,
		Description: strings.TrimSpace(body.Description),
		Version:     1,
		Scope:       "workspace",
		StartStepID: start,
		Steps:       body.Steps,
		Edges:       body.Edges,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	wfStore, ok := s.workflowStoreForRequest(w, r)
	if !ok {
		return
	}
	if err := wfStore.SaveDefinition(&def); err != nil {
		s.serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(def)
}

func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	wfStore, ok := s.workflowStoreForRequest(w, r)
	if !ok {
		return
	}
	workflowID := r.PathValue("workflowId")
	existing, found, err := wfStore.Definition(workflowID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || existing.Scope != "workspace" || existing.Project != "" {
		s.jsonError(w, http.StatusNotFound, "workflow not found")
		return
	}
	var body workflowCreateBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		s.jsonError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(body.Steps) == 0 {
		s.jsonError(w, http.StatusBadRequest, "at least one step is required")
		return
	}
	start := strings.TrimSpace(body.StartStepID)
	if start == "" {
		start = body.Steps[0].ID
	}
	existing.Name = name
	existing.Description = strings.TrimSpace(body.Description)
	existing.StartStepID = start
	existing.Steps = body.Steps
	existing.Edges = body.Edges
	existing.Scope = "workspace"
	existing.Project = ""
	existing.Version++
	if err := wfStore.SaveDefinition(&existing); err != nil {
		s.serverError(w, err)
		return
	}
	s.markPlaybookObjectCustomized(r, "workflow", "", workflowID)
	existing.Provenance = s.playbookObjectProvenanceForRequest(r, "workflow", "", workflowID)
	_ = json.NewEncoder(w).Encode(existing)
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	wfStore, ok := s.workflowStoreForRequest(w, r)
	if !ok {
		return
	}
	workflowID := r.PathValue("workflowId")
	existing, found, err := wfStore.Definition(workflowID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || existing.Scope != "workspace" || existing.Project != "" {
		s.jsonError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if err := wfStore.DeleteDefinition(workflowID); err != nil {
		s.serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetTaskWorkflow(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}
	wfStore, ok := s.workflowStoreForRequest(w, r)
	if !ok {
		return
	}
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	def, found, err := wfStore.RunDefinition(run)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonError(w, http.StatusNotFound, "workflow definition not found")
		return
	}
	steps, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.reconcileActiveWorkflowTaskQueue(workspaceID, project, taskID, run, def, steps, r); err != nil {
		s.serverError(w, err)
		return
	}
	steps, err = wfStore.ListStepInstances(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	history, err := wfStore.ListStepEvents(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	branches, err := wfStore.ListBranchInstances(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(taskWorkflowResponse{Definition: def, Run: run, Steps: steps, Branches: branches, History: history, DocTitles: s.workflowDocTitles()})
}

func (s *Server) workflowDocTitles() map[string]string {
	docs, err := store.NewDocsStore(s.root).List()
	if err != nil || len(docs) == 0 {
		return nil
	}
	out := make(map[string]string, len(docs))
	for _, doc := range docs {
		if doc == nil || strings.TrimSpace(doc.ID) == "" || strings.TrimSpace(doc.Title) == "" {
			continue
		}
		out[doc.ID] = strings.TrimSpace(doc.Title)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) reconcileActiveWorkflowTaskQueue(workspaceID, project, taskID string, run entity.WorkflowRun, def entity.WorkflowDefinition, steps []entity.WorkflowStepInstance, r *http.Request) error {
	if s == nil || strings.TrimSpace(run.ActiveStepID) == "" || strings.TrimSpace(run.Status) == "completed" || strings.TrimSpace(run.Status) == "failed" || strings.TrimSpace(run.Status) == "cancelled" {
		return nil
	}
	inst, ok := workflowStepInstanceByStepID(steps, run.ActiveStepID)
	if !ok {
		return nil
	}
	step, _ := workflowDefinitionStepByID(def.Steps, run.ActiveStepID)
	actorType := strings.TrimSpace(inst.ActorType)
	if actorType == "" {
		actorType = workflowActorTypeForStep(step)
	}
	actorID := strings.TrimSpace(inst.ActorID)
	if actorID == "" {
		actorID = workflowActorIDForStep(run.ActorBindings, step)
	}
	if actorID == "" {
		return nil
	}
	if strings.TrimSpace(inst.ActorType) != actorType || strings.TrimSpace(inst.ActorID) != actorID {
		inst.ActorType = actorType
		inst.ActorID = actorID
		inst.UpdatedAt = time.Now().UTC()
		if err := workflowstore.NewStore(s.controlDB, workspaceID).SaveStepInstance(&inst); err != nil {
			return err
		}
	}
	if _, _, err := workflowstore.NewStore(s.controlDB, workspaceID).ReconcileRunCurrentAssigneeForTask(project, taskID); err != nil {
		return err
	}
	task, currentAgent, err := s.findTaskInProject(project, taskID)
	if err != nil {
		return nil
	}
	if task.Status == entity.TaskStatusBlocked {
		return nil
	}
	now := time.Now().UTC()
	switch actorType {
	case "agent":
		wantAssignee := project + "/" + actorID
		if currentAgent == actorID && strings.TrimSpace(task.Assignee) == wantAssignee && (task.Status == entity.TaskStatusPending || task.Status == entity.TaskStatusInProgress) {
			return nil
		}
		status := entity.TaskStatusPending
		if s.hasActiveRuntimeRun(workspaceID, project, actorID, task.ID) {
			status = entity.TaskStatusInProgress
		}
		if err := s.moveWorkflowTaskToAgent(workspaceID, project, currentAgent, actorID, task, status, now); err != nil {
			return err
		}
	case "human":
		if currentAgent == "" || strings.TrimSpace(task.Assignee) == actorID && task.Status == entity.TaskStatusAwaitingConfirmation {
			return nil
		}
		task.Status = entity.TaskStatusAwaitingConfirmation
		task.Assignee = actorID
		task.UpdatedAt = now
		task.FinishedAt = nil
		s.annotateTaskAssignee(workspaceID, project, task)
		return s.ts.PersistTask(project, currentAgent, task)
	}
	return nil
}

type activeWorkflowTaskPresentation struct {
	Agent    string
	Assignee string
	Status   string
}

func (s *Server) activeWorkflowTaskView(workspaceID, project, taskID string, taskStatus entity.TaskStatus) (activeWorkflowTaskPresentation, bool) {
	var out activeWorkflowTaskPresentation
	if s == nil || s.controlDB == nil || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(project) == "" || strings.TrimSpace(taskID) == "" {
		return out, false
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil || !found || strings.TrimSpace(run.ActiveStepID) == "" || strings.TrimSpace(run.Status) == "completed" {
		return out, false
	}
	def, found, err := wfStore.RunDefinition(run)
	if err != nil || !found {
		return out, false
	}
	steps, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return out, false
	}
	inst, ok := workflowStepInstanceByStepID(steps, run.ActiveStepID)
	if !ok {
		return out, false
	}
	step, _ := workflowDefinitionStepByID(def.Steps, run.ActiveStepID)
	actorType := strings.TrimSpace(inst.ActorType)
	if actorType == "" {
		actorType = workflowActorTypeForStep(step)
	}
	actorID := strings.TrimSpace(inst.ActorID)
	if actorID == "" {
		actorID = workflowActorIDForStep(run.ActorBindings, step)
	}
	switch actorType {
	case "agent":
		if actorID == "" {
			return out, false
		}
		out.Agent = actorID
		out.Assignee = project + "/" + actorID
		if taskStatus == entity.TaskStatusInProgress || s.hasActiveRuntimeRun(workspaceID, project, actorID, "") || s.hasActiveRuntimeRun(workspaceID, project, actorID, taskID) || workflowStepInstanceOpen(inst.Status) || run.Status == "active" {
			out.Status = string(entity.TaskStatusInProgress)
		} else {
			out.Status = string(entity.TaskStatusPending)
		}
		return out, true
	case "human":
		if actorID == "" {
			return out, false
		}
		out.Assignee = actorID
		if workflowStepInstanceOpen(inst.Status) {
			out.Status = string(entity.TaskStatusAwaitingConfirmation)
		}
		return out, out.Status != ""
	default:
		return out, false
	}
}

func workflowStepInstanceByStepID(steps []entity.WorkflowStepInstance, stepID string) (entity.WorkflowStepInstance, bool) {
	for _, inst := range steps {
		if inst.StepID == stepID {
			return inst, true
		}
	}
	return entity.WorkflowStepInstance{}, false
}

func workflowDefinitionStepByID(steps []entity.WorkflowStep, stepID string) (entity.WorkflowStep, bool) {
	for _, step := range steps {
		if step.ID == stepID {
			return step, true
		}
	}
	return entity.WorkflowStep{}, false
}

func workflowActorTypeForStep(step entity.WorkflowStep) string {
	switch step.Type {
	case "human_review":
		return "human"
	case "agent_task":
		return "agent"
	default:
		return ""
	}
}

func workflowActorIDForStep(bindings map[string]entity.WorkflowActorBinding, step entity.WorkflowStep) string {
	for _, key := range []string{step.ID, step.ActorRole} {
		binding, ok := bindings[strings.TrimSpace(key)]
		if ok {
			return strings.TrimSpace(binding.ID)
		}
	}
	if strings.TrimSpace(step.ActorRole) == "" && strings.TrimSpace(step.Type) == "human_review" {
		out := ""
		for _, binding := range bindings {
			if strings.TrimSpace(binding.Type) != "human" || strings.TrimSpace(binding.ID) == "" {
				continue
			}
			if out != "" {
				return ""
			}
			out = strings.TrimSpace(binding.ID)
		}
		return out
	}
	return ""
}

func workflowStepInstanceRunning(status string) bool {
	normalized := strings.TrimSpace(status)
	return normalized == "running" || normalized == "in_progress"
}

func workflowStepInstanceOpen(status string) bool {
	normalized := strings.TrimSpace(status)
	return normalized == "" || normalized == "pending" || workflowStepInstanceRunning(normalized)
}

func (s *Server) handlePostTaskWorkflowReview(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	var body workflowReviewBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	currentWorkspaceID, err := s.currentWorkspaceID()
	if err != nil {
		s.serverError(w, err)
		return
	}
	resp, status, err := s.submitTaskWorkflowReview(r, currentWorkspaceID, project, taskID, body)
	if err != nil {
		if status == http.StatusInternalServerError {
			s.serverError(w, err)
			return
		}
		s.jsonError(w, status, err.Error())
		return
	}
	out := taskWorkflowReviewResponse{
		OK:                          true,
		TaskID:                      taskID,
		WorkflowRunID:               resp.Run.ID,
		Status:                      resp.Run.Status,
		ActiveStepID:                resp.Run.ActiveStepID,
		CurrentAssigneeType:         resp.Run.CurrentAssigneeType,
		CurrentAssigneeID:           resp.Run.CurrentAssigneeID,
		CurrentAssigneeMembershipID: resp.Run.CurrentAssigneeMembershipID,
		Done:                        strings.TrimSpace(resp.Run.Status) == "completed",
	}
	for _, step := range resp.Steps {
		if step.StepID == resp.Run.ActiveStepID {
			out.NextActor = &entity.WorkflowActorBinding{Type: step.ActorType, ID: step.ActorID}
			break
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) submitTaskWorkflowReview(r *http.Request, workspaceID, project, taskID string, body workflowReviewBody) (taskWorkflowResponse, int, error) {
	decision := normalizeWorkflowReviewDecision(body.Decision)
	t, agent, err := s.findTaskInProject(project, taskID)
	if err != nil {
		return taskWorkflowResponse{}, http.StatusNotFound, errors.New("task not found")
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	outputs := body.Outputs
	if outputs == nil {
		outputs = map[string]string{}
	}
	if outputDecision := normalizeWorkflowReviewDecision(outputs["decision"]); outputDecision != "" {
		outputs["decision"] = outputDecision
	} else if decision != "" {
		outputs["decision"] = decision
	}
	comments := strings.TrimSpace(body.Comments)
	if comments == "" {
		comments = strings.TrimSpace(outputs["comments"])
	}
	if comments != "" {
		outputs["comments"] = comments
	}
	if updateTaskRemoteMR(t, outputs) {
		t.UpdatedAt = time.Now().UTC()
		if err := s.ts.PersistTask(project, agent, t); err != nil {
			return taskWorkflowResponse{}, http.StatusInternalServerError, err
		}
	}
	summary := formatWorkflowReviewFields(outputs)
	workflowStore := wfStore
	run, runFound, err := workflowStore.RunForTask(project, taskID)
	if err != nil {
		return taskWorkflowResponse{}, http.StatusInternalServerError, err
	}
	var currentStep entity.WorkflowStep
	if runFound {
		if def, found, err := workflowStore.RunDefinition(run); err != nil {
			return taskWorkflowResponse{}, http.StatusInternalServerError, err
		} else if found {
			for _, step := range def.Steps {
				if step.ID == run.ActiveStepID {
					currentStep = step
					break
				}
			}
		}
	}

	// Terminal delivery has external side effects. Complete those side effects
	// before committing the workflow terminal state, otherwise a failed merge
	// would be reported as a successful task.
	deliveryPrepared := false
	if runFound && isPullRequestReviewStep(currentStep) {
		willComplete, err := workflowStore.WillComplete(project, taskID, outputs, summary, "", "completed")
		if err != nil {
			return taskWorkflowResponse{}, http.StatusBadRequest, err
		}
		if willComplete {
			if !isApprovalDecision(outputs["decision"]) {
				return taskWorkflowResponse{}, http.StatusBadRequest, errors.New("pull request review requires an approval decision")
			}
			if err := s.prepareTaskDelivery(r, project, t, ""); err != nil {
				return taskWorkflowResponse{}, http.StatusConflict, err
			}
			deliveryPrepared = true
		}
	}
	if isApprovalDecision(outputs["decision"]) {
		s.commitAndPushReviewChanges(project, t)
	}
	transition, err := wfStore.CompleteAndAdvance(project, taskID, summary, "", outputs, "completed")
	if err != nil {
		return taskWorkflowResponse{}, http.StatusBadRequest, err
	}
	_ = s.ts.RemoveFromInbox(taskID)
	if transition.Done {
		now := time.Now().UTC()
		t.Status = entity.TaskStatusDoneSuccess
		t.Summary = summary
		t.UpdatedAt = now
		t.FinishedAt = &now
		if !deliveryPrepared && isPullRequestReviewStep(currentStep) {
			return taskWorkflowResponse{}, http.StatusConflict, errors.New("terminal pull request review completed without delivery preparation")
		}
		snapshotErr := s.captureTaskCompletionSnapshot(t)
		s.syncTaskCompletionRemote(project, t)
		if snapshotErr != nil {
			// Keep the worktree and preview alive: the snapshot holds the
			// only copy of unpushed work, so cleanup must not run.
		} else {
			s.cleanupTaskDeliveryArtifacts(project, taskID)
		}
		if err := s.ts.PersistTask(project, agent, t); err != nil {
			return taskWorkflowResponse{}, http.StatusInternalServerError, err
		}
		if strings.TrimSpace(t.Vars[workflowBranchIDVar]) != "" {
			branchResult, err := s.completeRuntimeWorkflowBranch(workspaceID, project, t, outputs, "completed")
			if err != nil {
				return taskWorkflowResponse{}, http.StatusBadRequest, err
			}
			if err := s.advanceParentAfterBranchCompletion(workspaceID, project, branchResult, r); err != nil {
				return taskWorkflowResponse{}, http.StatusInternalServerError, err
			}
		}
	} else if err := s.activateNextWorkflowStep(workspaceID, project, agent, t, transition, r); err != nil {
		return taskWorkflowResponse{}, http.StatusInternalServerError, err
	}
	steps, err := wfStore.ListStepInstances(transition.Run.ID)
	if err != nil {
		return taskWorkflowResponse{}, http.StatusInternalServerError, err
	}
	history, err := wfStore.ListStepEvents(transition.Run.ID)
	if err != nil {
		return taskWorkflowResponse{}, http.StatusInternalServerError, err
	}
	branches, err := wfStore.ListBranchInstances(transition.Run.ID)
	if err != nil {
		return taskWorkflowResponse{}, http.StatusInternalServerError, err
	}
	def, found, err := wfStore.RunDefinition(transition.Run)
	if err != nil {
		return taskWorkflowResponse{}, http.StatusInternalServerError, err
	}
	if !found {
		return taskWorkflowResponse{}, http.StatusNotFound, errors.New("workflow definition not found")
	}
	return taskWorkflowResponse{Definition: def, Run: transition.Run, Steps: steps, Branches: branches, History: history}, http.StatusOK, nil
}

func isPullRequestReviewStep(step entity.WorkflowStep) bool {
	id := strings.ToLower(strings.TrimSpace(step.ID))
	title := strings.ToLower(strings.TrimSpace(step.Title))
	return strings.Contains(id, "pr_review") || strings.Contains(id, "mr_review") || strings.Contains(id, "merge_and_sync") || strings.Contains(id, "push_to_gitlab") || strings.Contains(title, "pull request") || strings.Contains(title, "merge request") || strings.Contains(title, "merge and sync")
}

func isApprovalDecision(decision string) bool {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approve", "approved", "pass", "ok", "yes", "approve_push", "approve_local":
		return true
	default:
		return false
	}
}

func (s *Server) commitAndPushReviewChanges(project string, t *entity.Task) {
	if t == nil {
		return
	}
	gitRoot := strings.TrimSpace(t.WorktreeDir)
	if gitRoot == "" {
		gitRoot = s.resolveProjectGitRoot(project)
	} else if _, err := os.Stat(gitRoot); err != nil {
		gitRoot = s.resolveProjectGitRoot(project)
	}
	if _, err := os.Stat(filepath.Join(gitRoot, ".git")); err != nil {
		return
	}

	// 1. Check for uncommitted working tree changes from in-context preview copilot
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = gitRoot
	statusOut, err := statusCmd.Output()
	if err != nil || len(bytes.TrimSpace(statusOut)) == 0 {
		return // working tree is clean
	}

	// 2. Stage and commit
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = gitRoot
	_ = addCmd.Run()

	commitMsg := "chore(review): user in-context preview feedback fixes"
	commitCmd := exec.Command("git", "commit", "-m", commitMsg)
	commitCmd.Dir = gitRoot
	if err := commitCmd.Run(); err != nil {
		return
	}

	// 3. Push if remote is configured
	branchName := strings.TrimSpace(t.BranchName)
	if branchName == "" && s.worktreeMgr != nil {
		branchName, _ = s.worktreeMgr.CheckedOutBranch(gitRoot)
	}
	if branchName == "" {
		branchName = "main"
	}

	remoteCmd := exec.Command("git", "remote", "get-url", "origin")
	remoteCmd.Dir = gitRoot
	if remoteOut, err := remoteCmd.Output(); err == nil && len(bytes.TrimSpace(remoteOut)) > 0 {
		pushCmd := exec.Command("git", "push", "origin", branchName)
		pushCmd.Dir = gitRoot
		_ = pushCmd.Run()
	}
}

func updateTaskRemoteMR(task *entity.Task, outputs map[string]string) bool {
	if task == nil {
		return false
	}
	first := func(keys ...string) string {
		for _, key := range keys {
			if value := strings.TrimSpace(outputs[key]); value != "" {
				return value
			}
		}
		return ""
	}
	urlValue := first("pr_url", "mr_url", "change_request_url")
	if urlValue == "none" || strings.HasPrefix(strings.ToLower(urlValue), "branch:") {
		return false
	}
	iid := first("pr_number", "mr_iid", "mr_number", "change_request_number")
	if iid == "none" {
		iid = ""
	}
	headSHA := first("head_sha", "headSha", "commit_sha", "commitSha")
	state := first("mr_state", "pr_state", "change_request_state")
	changed := false
	if iid != "" && task.RemoteMRIID != iid {
		task.RemoteMRIID = iid
		changed = true
	}
	if urlValue != "" && task.RemoteMRURL != urlValue {
		task.RemoteMRURL = urlValue
		changed = true
	}
	if headSHA != "" && task.RemoteMRHeadSHA != headSHA {
		task.RemoteMRHeadSHA = headSHA
		changed = true
	}
	if state == "" && (iid != "" || urlValue != "") {
		state = "opened"
	}
	if state != "" && task.RemoteMRState != state {
		task.RemoteMRState = state
		changed = true
	}
	return changed
}

// captureTaskCompletionSnapshot records a content-addressed Git snapshot
// before the task worktree is cleaned up. It deliberately does not claim the
// remote branch is synchronized; that is a separate delivery step. A capture
// failure is returned so callers can keep the worktree alive instead of
// destroying uncommitted work.
func (s *Server) captureTaskCompletionSnapshot(task *entity.Task) error {
	if s == nil || task == nil || s.worktreeMgr == nil || strings.TrimSpace(task.WorktreeDir) == "" {
		return nil
	}
	commit, err := s.worktreeMgr.CaptureSnapshot(task.WorktreeDir)
	if err != nil {
		task.RemoteSyncStatus = "failed"
		task.RemoteSyncError = fmt.Sprintf("capture completion snapshot: %v", err)
		return err
	}
	task.CompletionCommit = commit
	if strings.TrimSpace(task.RemoteSyncStatus) == "" {
		task.RemoteSyncStatus = "pending"
	}
	task.RemoteSyncError = ""
	return nil
}

// syncTaskCompletionRemote is intentionally best-effort after local task
// completion. A remote outage must be visible as a retryable sync failure, not
// turn an otherwise valid local task into a failed task.
func (s *Server) syncTaskCompletionRemote(project string, task *entity.Task) {
	if s == nil || task == nil || s.worktreeMgr == nil || strings.TrimSpace(task.CompletionCommit) == "" {
		return
	}
	p, err := s.st.Project(project)
	if err != nil || p == nil || strings.TrimSpace(p.RemoteProvider) == "" {
		task.RemoteSyncStatus = "not_applicable"
		task.RemoteSyncError = ""
		return
	}
	if strings.EqualFold(strings.TrimSpace(task.RemoteMRState), "merged") {
		task.RemoteSyncAttempts++
		task.RemoteSyncStatus = "synced"
		task.RemoteSyncCommit = firstNonEmpty(task.RemoteMRHeadSHA, task.CompletionCommit)
		task.RemoteSyncError = ""
		return
	}
	if strings.TrimSpace(task.BranchName) == "" {
		task.RemoteSyncAttempts++
		task.RemoteSyncStatus = "failed"
		task.RemoteSyncError = "task branch is required for remote synchronization"
		return
	}
	task.RemoteSyncAttempts++
	task.RemoteSyncStatus = "syncing"
	if err := s.worktreeMgr.PushBranch(s.resolveProjectGitRoot(project), task.BranchName, task.CompletionCommit); err != nil {
		task.RemoteSyncStatus = "failed"
		task.RemoteSyncError = err.Error()
		return
	}
	task.RemoteSyncStatus = "synced"
	task.RemoteSyncCommit = task.CompletionCommit
	task.RemoteSyncError = ""
}

func (s *Server) checkTaskIntegrationBase(project string, task *entity.Task) error {
	if s == nil || task == nil || s.worktreeMgr == nil || strings.TrimSpace(task.BaseTaskID) == "" {
		return nil
	}
	baseTask, _, err := s.findTaskInProject(project, task.BaseTaskID)
	if err != nil || baseTask == nil {
		return fmt.Errorf("base task %s not found", task.BaseTaskID)
	}
	integrated := strings.TrimSpace(baseTask.IntegratedCommit)
	if integrated == "" || strings.TrimSpace(task.BranchName) == "" {
		return nil
	}
	if strings.EqualFold(integrated, strings.TrimSpace(baseTask.CompletionCommit)) {
		task.RebaseStatus = "not_required"
		return nil
	}
	aligned, err := s.worktreeMgr.IsAncestor(s.resolveProjectGitRoot(project), integrated, task.BranchName)
	if err != nil {
		return fmt.Errorf("check task base integration: %w", err)
	}
	if !aligned {
		task.RebaseStatus = "required"
		return fmt.Errorf("task branch %s must be rebased onto integrated base %s before delivery", task.BranchName, integrated)
	}
	task.RebaseStatus = "aligned"
	return nil
}

func (s *Server) prepareTaskDelivery(r *http.Request, project string, task *entity.Task, requestedCommitMessage string) error {
	if s.worktreeMgr == nil {
		return errors.New("worktree manager is unavailable")
	}
	projectRoot := s.st.ProjectDir(project)
	wsDir := filepath.Join(projectRoot, "workspace")
	gitRoot := projectRoot
	if _, err := os.Stat(filepath.Join(wsDir, ".git")); err == nil {
		gitRoot = wsDir
	}
	p, err := s.st.Project(project)
	if err != nil || p == nil {
		return fmt.Errorf("load project for delivery: %w", err)
	}
	defaultBranch := p.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	commitMsg := strings.TrimSpace(requestedCommitMessage)
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("merge: task %s (%s)", task.ID, task.Title)
	}
	if err := s.checkTaskIntegrationBase(project, task); err != nil {
		return err
	}
	if p.RemoteProvider == "gitlab" || p.RemoteProvider == "github" {
		if p.RemoteProjectID == "" || task.RemoteMRIID == "" {
			return fmt.Errorf("remote %s delivery is incomplete: project ID and change-request ID are required", p.RemoteProvider)
		}
		var host codehost.CodeHost
		var err error
		if p.RemoteProvider == "gitlab" {
			host, _, err = s.resolveGitLabHost(p.RemoteConnection)
		} else {
			host, _, err = s.resolveGitHubHost(p.RemoteConnection)
		}
		if err != nil {
			return fmt.Errorf("resolve %s connection: %w", p.RemoteProvider, err)
		}
		// Always read the remote MR before deciding whether delivery is done.
		// Task metadata can be stale or reported by an agent, so it must not be
		// trusted as proof that a remote merge already happened.
		mr, err := host.GetMR(r.Context(), p.RemoteProjectID, task.RemoteMRIID)
		if err != nil {
			return fmt.Errorf("read %s change request %s before merge: %w", p.RemoteProvider, task.RemoteMRIID, err)
		}
		if task.RemoteMRURL == "" {
			task.RemoteMRURL = mr.WebURL
		}
		if strings.EqualFold(strings.TrimSpace(mr.State), "merged") {
			task.RemoteMRState = "merged"
		} else {
			expectedHeadSHA := strings.TrimSpace(task.RemoteMRHeadSHA)
			if expectedHeadSHA == "" {
				expectedHeadSHA = strings.TrimSpace(mr.HeadSHA)
				task.RemoteMRHeadSHA = expectedHeadSHA
			}
			if expectedHeadSHA == "" {
				return fmt.Errorf("%s change request %s has no source head SHA to merge", p.RemoteProvider, task.RemoteMRIID)
			}
			mergeMessage := strings.TrimSpace(requestedCommitMessage)
			if mergeMessage == "" {
				mergeMessage = fmt.Sprintf("Merge MR !%s (%s)", task.RemoteMRIID, task.Title)
			}
			if err := host.MergeMR(r.Context(), p.RemoteProjectID, task.RemoteMRIID, expectedHeadSHA, mergeMessage); err != nil {
				return fmt.Errorf("merge %s change request %s: %w", p.RemoteProvider, task.RemoteMRIID, err)
			}
		}
		if err := s.worktreeMgr.SyncMain(gitRoot, defaultBranch); err != nil {
			return fmt.Errorf("sync %s after GitLab merge: %w", defaultBranch, err)
		}
		integrated, err := s.worktreeMgr.ResolveBaseCommit(gitRoot, defaultBranch)
		if err != nil {
			return fmt.Errorf("read integrated %s revision: %w", defaultBranch, err)
		}
		task.IntegratedCommit = integrated
		task.RemoteMRState = "merged"
		return nil
	}
	if strings.TrimSpace(p.RemoteProvider) != "" {
		return fmt.Errorf("remote provider %q is not supported by task delivery yet", p.RemoteProvider)
	}

	if task.BranchName == "" {
		return errors.New("local delivery requires a task branch")
	}
	mergedSHA, err := s.worktreeMgr.MergeBranchLocally(gitRoot, defaultBranch, task.BranchName, commitMsg)
	if err != nil {
		return fmt.Errorf("local merge failed: %w", err)
	}
	task.RemoteMRHeadSHA = mergedSHA
	task.IntegratedCommit = mergedSHA
	task.RemoteMRState = "merged"
	return nil
}

func (s *Server) cleanupTaskDeliveryArtifacts(project, taskID string) {
	if s.previewEngine != nil {
		_ = s.previewEngine.StopEphemeralPreview(taskID)
	}
	if s.worktreeMgr != nil {
		_ = s.worktreeMgr.CleanupWorktree(s.resolveProjectGitRoot(project), taskID)
	}
}

func formatWorkflowReviewFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		value := strings.TrimSpace(fields[key])
		if value == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(value)
	}
	return b.String()
}

func normalizeWorkflowReviewDecision(decision string) string {
	switch strings.TrimSpace(decision) {
	case "approved":
		return "approve"
	case "needs_changes":
		return "request_changes"
	default:
		return strings.TrimSpace(decision)
	}
}

func (s *Server) findTaskInProject(project, taskID string) (*entity.Task, string, error) {
	workspaceID, workspaceErr := s.currentWorkspaceID()
	var agents []string
	var err error
	if workspaceErr == nil && strings.TrimSpace(workspaceID) != "" {
		agents, err = s.projectAgentNames(workspaceID, project)
	} else {
		err = workspaceErr
	}
	if err != nil {
		return nil, "", err
	}
	for _, agentName := range agents {
		t, err := s.ts.GetTask(project, agentName, taskID)
		if err == nil {
			return t, agentName, nil
		}
	}
	return nil, "", errors.New("task not found")
}

func (s *Server) handleRuntimeTaskWorkflow(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.runtimeRequireCapability(w, r, "task.use")
	if !ok {
		return
	}
	t, _, _, err := s.runtimeFindTask(principal, r.PathValue("id"), r.URL.Query().Get("agent"))
	if err != nil || t == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	wfStore := workflowstore.NewStore(s.controlDB, principal.WorkspaceID)
	run, found, err := wfStore.RunForTask(principal.Project, t.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	def, found, err := wfStore.RunDefinition(run)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.jsonError(w, http.StatusNotFound, "workflow definition not found")
		return
	}
	steps, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	history, err := wfStore.ListStepEvents(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	branches, err := wfStore.ListBranchInstances(run.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(taskWorkflowResponse{Definition: def, Run: run, Steps: steps, Branches: branches, History: history})
}

func (s *Server) recoverActiveWorkflowRuns() {
	if s == nil || s.controlDB == nil || s.ts == nil {
		return
	}
	// Allow 3 seconds for network listeners and dependencies to complete startup
	time.Sleep(3 * time.Second)

	workspaceID, err := s.currentWorkspaceID()
	if err != nil || strings.TrimSpace(workspaceID) == "" {
		return
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)

	projects, err := s.ts.ListProjects()
	if err != nil {
		return
	}

	for _, projectName := range projects {
		agents, err := s.ts.ListAgents(projectName)
		if err != nil {
			continue
		}
		for _, agentName := range agents {
			tasks, err := s.ts.ListTasks(projectName, agentName)
			if err != nil {
				continue
			}
			for _, task := range tasks {
				if task == nil {
					continue
				}
				// Only recover tasks that are active or pending
				if task.Status != entity.TaskStatusInProgress && task.Status != entity.TaskStatusPending {
					continue
				}
				run, runFound, err := wfStore.RunForTask(projectName, task.ID)
				if err != nil || !runFound || run.Status != "active" || strings.TrimSpace(run.ActiveStepID) == "" {
					continue
				}
				def, defFound, err := wfStore.RunDefinition(run)
				if err != nil || !defFound {
					continue
				}
				var activeStep entity.WorkflowStep
				foundStep := false
				for _, step := range def.Steps {
					if step.ID == run.ActiveStepID {
						activeStep = step
						foundStep = true
						break
					}
				}
				// Never auto-resume human review steps
				if !foundStep || activeStep.Type == "human_review" {
					continue
				}

				// Find step instance
				instances, err := wfStore.ListStepInstances(run.ID)
				if err != nil {
					continue
				}
				var activeInst *entity.WorkflowStepInstance
				for i := range instances {
					if instances[i].StepID == run.ActiveStepID {
						activeInst = &instances[i]
						break
					}
				}
				if activeInst == nil || (activeInst.Status != "pending" && activeInst.Status != "running") {
					continue
				}

				targetAgent := strings.TrimSpace(activeInst.ActorID)
				if targetAgent == "" {
					targetAgent = agentName
				}

				log.Printf("[auto-recovery] resuming in-flight workflow task %s (project: %s, agent: %s, step: %s)", task.ID, projectName, targetAgent, run.ActiveStepID)

				// Trigger wakeup in background
				go func(p, a string, t *entity.Task) {
					_ = s.fireTaskTriggerOrQueueRuntime(workspaceID, p, a, t, nil, "startup auto-recovery for task "+t.ID)
				}(projectName, targetAgent, task)
			}
		}
	}
}
