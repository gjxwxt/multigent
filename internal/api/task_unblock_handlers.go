package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Q0 PR-3: explicit unblock endpoint (GPT v3 hard requirement: backend RBAC +
// audit; frontend confirmation is UX sugar only). A task parked in blocked by
// consecutive infra failures re-enters dispatch ONLY here.

type unblockTaskResponse struct {
	TaskID string `json:"taskId"`
	Status string `json:"status"`
}

func (s *Server) handleUnblockProjectTask(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if project == "" || taskID == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "project and taskId are required")
		return
	}
	if !s.checkProjectManager(w, r, project) {
		return
	}
	task, agent, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeValidationFailed, "task not found")
		return
	}
	if !taskBlockedByInfraFailures(task) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "task is not blocked")
		return
	}
	unblockInfraBlockedTask(task, time.Now().UTC())
	if err := s.ts.PersistTask(project, agent, task); err != nil {
		s.serverError(w, err)
		return
	}
	workspaceID, _ := s.currentWorkspaceID()
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "task.unblock",
		ResourceType: "task",
		ResourceID:   task.ID,
		Summary:      "Task unblocked by human owner after infra-failure cap",
		After: map[string]any{
			"project": project,
			"agent":   agent,
			"taskId":  task.ID,
			"actor":   requestUsername(r),
		},
	})
	_ = json.NewEncoder(w).Encode(unblockTaskResponse{TaskID: task.ID, Status: string(task.Status)})
}
