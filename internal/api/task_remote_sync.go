package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handlePostTaskRemoteSyncRetry retries only the remote delivery side effect.
// It never re-runs the task or changes its terminal status.
func (s *Server) handlePostTaskRemoteSyncRetry(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	task, agent, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if strings.TrimSpace(task.CompletionCommit) == "" {
		s.jsonError(w, http.StatusConflict, "task has no completion snapshot")
		return
	}

	s.syncTaskCompletionRemote(project, task)
	task.UpdatedAt = time.Now().UTC()
	if err := s.ts.PersistTask(project, agent, task); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":               task.RemoteSyncStatus == "synced" || task.RemoteSyncStatus == "not_applicable",
		"taskId":           taskID,
		"remoteSyncStatus": task.RemoteSyncStatus,
		"remoteSyncCommit": task.RemoteSyncCommit,
		"attempts":         task.RemoteSyncAttempts,
		"error":            task.RemoteSyncError,
	})
}
