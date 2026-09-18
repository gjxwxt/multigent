// Test-data sandbox (V1) integration: wiring + the provision call used by
// the preview engine. See internal/fixturesandbox for the contract, lease
// and artifact machinery; this file only adapts it to the Server.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/fixturesandbox"
)

// initFixtureSandbox wires the sandbox provisioner into the preview engine
// when the environment supports it. Every failure path degrades to "no
// sandbox" (contract-less projects never notice; contract-bearing projects
// fail closed at provision time with a clear error).
func (s *Server) initFixtureSandbox() {
	dataDir := defaultWorkspaceDataDir()
	store := fixturesandbox.NewStore(s.controlDB, s.currentWorkspaceIDForSandbox(), dataDir)
	p := fixturesandbox.NewProvisioner(store, fixturesandbox.DefaultContainerGenerator)
	s.fixtureSandbox = p
	if eng, ok := s.previewEngine.(interface {
		SetSandboxHooks(provision func(ctx context.Context, taskID, projectName, worktreeDir string) ([]string, error), release func(taskID, reason string))
	}); ok {
		eng.SetSandboxHooks(p.ProvisionForPreview, p.ReleaseForStop)
	}
	// Sandbox lease reaper: every minute, aligned with the preview reaper.
	// Preview containers are stopped by the preview engine's own reaper,
	// whose release hook reclaims the lease; this loop catches headless
	// (exec-kind) leases and orphaned directories after restarts.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := p.ReapExpired(); err != nil {
				log.Printf("[fixture-sandbox] reap: %v", err)
			}
		}
	}()
}

// currentWorkspaceIDForSandbox resolves the workspace ID without a request
// context (the sandbox store is workspace-scoped like every kv_records user).
func (s *Server) currentWorkspaceIDForSandbox() string {
	if id, err := s.currentWorkspaceID(); err == nil && id != "" {
		return id
	}
	return workspaceID(s.root)
}

// handleGetTaskFixtureSandbox queries the fixture sandbox status for a task,
// including whether a contract is present, available scenarios, current active
// scenario, lease ID, reset count, and expiry.
func (s *Server) handleGetTaskFixtureSandbox(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	task := s.projectTaskResourceGuard(w, r, project, taskID)
	if task == nil {
		return
	}

	if s.fixtureSandbox == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixturesandbox.TaskSandboxStatus{HasContract: false})
		return
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	status, err := s.fixtureSandbox.TaskStatus(r.Context(), taskID, project, worktreeDir)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, "failed to query sandbox status: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleResetTaskFixtureSandbox executes a fast reset (<100ms) of the task's
// private database back to the immutable frozen artifact for the active scenario.
func (s *Server) handleResetTaskFixtureSandbox(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if !s.checkProjectOperator(w, r, project) {
		return
	}

	taskProject, _, task, err := s.ts.FindTaskByID(taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if strings.TrimSpace(taskProject) != "" && taskProject != project {
		s.jsonError(w, http.StatusNotFound, "task not found in this project")
		return
	}

	if s.fixtureSandbox == nil {
		s.jsonError(w, http.StatusBadRequest, "test-data sandbox is not configured on this server")
		return
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	res, err := s.fixtureSandbox.ResetTask(r.Context(), taskID, project, worktreeDir)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, "reset sandbox failed: "+err.Error())
		return
	}

	s.auditLog(auditLogInput{
		Action:       "task.fixture_sandbox.reset",
		ResourceType: "task",
		ResourceID:   taskID,
		Summary:      "Task test-data sandbox reset to immutable baseline",
		After: map[string]any{
			"project":        project,
			"leaseId":        res.Lease.ID,
			"scenario":       res.Lease.Scenario,
			"resetCount":     res.Lease.ResetCount,
			"artifactDigest": res.ArtifactDigest,
		},
		Request: r,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":        true,
		"leaseId":        res.Lease.ID,
		"scenario":       res.Lease.Scenario,
		"resetCount":     res.Lease.ResetCount,
		"artifactDigest": res.ArtifactDigest,
		"expiresAt":      res.Lease.ExpiresAt,
	})
}

// switchFixtureSandboxScenarioRequest carries the target scenario identifier.
type switchFixtureSandboxScenarioRequest struct {
	Scenario string `json:"scenario"`
}

// handleSwitchTaskFixtureSandboxScenario provisions or switches the task's
// active test-data dataset to a named scenario.
func (s *Server) handleSwitchTaskFixtureSandboxScenario(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if !s.checkProjectOperator(w, r, project) {
		return
	}

	taskProject, _, task, err := s.ts.FindTaskByID(taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return
	}
	if strings.TrimSpace(taskProject) != "" && taskProject != project {
		s.jsonError(w, http.StatusNotFound, "task not found in this project")
		return
	}

	if s.fixtureSandbox == nil {
		s.jsonError(w, http.StatusBadRequest, "test-data sandbox is not configured on this server")
		return
	}

	var req switchFixtureSandboxScenarioRequest
	if err := s.readJSON(w, r, &req); err != nil {
		return
	}

	worktreeDir := s.resolveTaskWorktreeDir(project, taskID)
	res, err := s.fixtureSandbox.SwitchScenario(r.Context(), taskID, project, worktreeDir, req.Scenario)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, "switch sandbox scenario failed: "+err.Error())
		return
	}

	s.auditLog(auditLogInput{
		Action:       "task.fixture_sandbox.switch_scenario",
		ResourceType: "task",
		ResourceID:   taskID,
		Summary:      "Task test-data sandbox scenario switched",
		After: map[string]any{
			"project":        project,
			"leaseId":        res.Lease.ID,
			"scenario":       res.Lease.Scenario,
			"resetCount":     res.Lease.ResetCount,
			"artifactDigest": res.ArtifactDigest,
		},
		Request: r,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":        true,
		"leaseId":        res.Lease.ID,
		"scenario":       res.Lease.Scenario,
		"resetCount":     res.Lease.ResetCount,
		"artifactDigest": res.ArtifactDigest,
		"expiresAt":      res.Lease.ExpiresAt,
	})
}

