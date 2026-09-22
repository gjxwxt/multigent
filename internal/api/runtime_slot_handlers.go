package api

// Agent execution-slot observability (scheduling observability module).
//
// One agent runs at most one runtime run at a time; the slot decision lives in
// runOccupiesWorkerSlot / hasActiveRuntimeRunForTarget. Until now that state
// was invisible: an agent wedged behind a stale run (e.g. a task parked
// mid-flight since August holding Lina's slot) looked identical to a busy
// agent. These endpoints expose who owns the slot and allow a human to
// release a provably-stale run using the SAME fenced semantics as the
// reaper — no ad-hoc state surgery.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

type runtimeSlotRunView struct {
	ID             string `json:"id"`
	TaskID         string `json:"taskId,omitempty"`
	Status         string `json:"status"`
	RuntimeNodeID  string `json:"runtimeNodeId,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	LeaseExpiresAt string `json:"leaseExpiresAt,omitempty"`
	LeaseExpired   bool   `json:"leaseExpired"`
	ErrorCode      string `json:"errorCode,omitempty"`
}

type runtimeSlotTaskView struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Status  string `json:"status"`
	Agent   string `json:"agent,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type runtimeSlotResponse struct {
	// Occupied: any queued/running run row exists for this agent. Note this
	// is the ROW-LIFECYCLE view, not the scheduler's dispatch gate: an
	// expired-lease run still shows here (that's the stuck run a human needs
	// to see) even though the scheduler would not let it block dispatch.
	Occupied bool `json:"occupied"`
	// Dispatchable: the scheduler's own view (runtimeRunBlocksAgent) — would
	// the next dispatch be gated right now?
	Dispatchable bool `json:"dispatchable"`
	// Stale: running with lease expired past the reaper grace. Releasable
	// mirrors it for the UI: POST .../slot/release performs the reaper's
	// exact fenced transition on demand.
	Stale        bool               `json:"stale"`
	Releasable   bool               `json:"releasable"`
	Run          *runtimeSlotRunView `json:"run,omitempty"`
	Task         *runtimeSlotTaskView `json:"task,omitempty"`
	SlotClass    string             `json:"slotClass,omitempty"`
	LeaseGraceSeconds int           `json:"leaseGraceSeconds,omitempty"`
	Note         string             `json:"note,omitempty"`
}

// handleGetAgentRuntimeSlot reports who currently holds the agent's execution
// slot and whether the holder looks stale (lease expired past the reaper
// grace). Read-only.
func (s *Server) handleGetAgentRuntimeSlot(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	agent := r.PathValue("agent")
	if !s.checkAgentAccess(w, r, project, agent) {
		return
	}
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	resp := s.runtimeSlotState(workspaceID, project, agent, time.Now().UTC())
	_ = json.NewEncoder(w).Encode(resp)
}

// runtimeSlotState resolves the slot view; shared by the GET handler.
func (s *Server) runtimeSlotState(workspaceID, project, agent string, now time.Time) runtimeSlotResponse {
	resp := runtimeSlotResponse{Occupied: false, LeaseGraceSeconds: int(runtimeReaperLeaseGrace.Seconds())}
	if s == nil || s.controlDB == nil {
		resp.Note = "control database unavailable"
		return resp
	}
	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
	filter := controldb.RuntimeRunFilter{WorkspaceID: workspaceID, Limit: 50}
	if strings.TrimSpace(target.workerID) != "" {
		filter.AgentWorkerID = target.workerID
	} else {
		filter.ProjectID = target.project
		filter.AgentID = target.agent
	}
	var holder *controldb.RuntimeRun
	for _, status := range []string{"queued", "running"} {
		filter.Status = status
		runs, err := s.controlDB.ListRuntimeRuns(filter)
		if err != nil {
			continue
		}
		for i := range runs {
			run := runs[i]
			// Row-level occupancy: any queued/running run is shown; prefer a
			// dispatch-gating run if several exist.
			if holder == nil || runtimeRunBlocksAgent(run, now) {
				holder = &run
			}
			if runtimeRunBlocksAgent(run, now) {
				break
			}
		}
		if holder != nil && runtimeRunBlocksAgent(*holder, now) {
			break
		}
	}
	if holder == nil {
		return resp
	}
	resp.Occupied = true
	resp.Dispatchable = runtimeRunBlocksAgent(*holder, now)
	resp.Run = &runtimeSlotRunView{
		ID:             holder.ID,
		TaskID:         holder.TaskID,
		Status:         holder.Status,
		RuntimeNodeID:  holder.RuntimeNodeID,
		StartedAt:      holder.StartedAt,
		LeaseExpiresAt: holder.LeaseExpiresAt,
		ErrorCode:      holder.ErrorCode,
	}
	if holder.Status == "running" {
		resp.SlotClass = holder.SlotClass
		if expires, err := time.Parse(time.RFC3339, strings.TrimSpace(holder.LeaseExpiresAt)); err == nil {
			resp.Run.LeaseExpired = now.After(expires.Add(runtimeReaperLeaseGrace))
		}
	}
	// Stale = past the reaper's lease+grace window. The reaper would reap it
	// on its next pass; exposing it here just makes the wait visible and
	// offers the same transition on demand.
	resp.Stale = resp.Run.LeaseExpired
	resp.Releasable = resp.Stale
	if holder.TaskID != "" && s.ts != nil {
		if task, err := s.ts.GetTask(target.project, target.agent, holder.TaskID); err == nil && task != nil {
			resp.Task = &runtimeSlotTaskView{
				ID:      task.ID,
				Title:   task.Title,
				Status:  string(task.Status),
				Agent:   target.agent,
				Summary: task.Summary,
			}
		}
	}
	if resp.Stale {
		resp.Note = "Lease expired past the reaper grace; the reaper will reap it on its next pass, or release it manually."
	}
	return resp
}

// handleReleaseAgentRuntimeSlot force-releases a STALE run from the agent's
// execution slot. Only lease-expired-past-grace runs are releasable — for
// everything else the reaper semantics already exist and racing them here
// would be state surgery. The release itself reuses the reaper's exact
// fenced path (ReapExpiredRuntimeRun CAS + transitionReapedTask + token
// clear), so an operator click is byte-for-byte what the reaper would have
// done on its next pass.
func (s *Server) handleReleaseAgentRuntimeSlot(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	agent := r.PathValue("agent")
	if !s.checkAgentAccess(w, r, project, agent) {
		return
	}
	// Destructive ops action: operator-level project role (admin/manager/operator).
	if !s.canOperateProject(r, project) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeProjectAccessRequired, "operator project access required to release a slot")
		return
	}
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	state := s.runtimeSlotState(workspaceID, project, agent, now)
	if !state.Occupied || state.Run == nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "agent slot is not occupied")
		return
	}
	if !state.Releasable {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict,
			"run is not stale (lease not expired past the reaper grace); wait for the run to finish or let the reaper handle it")
		return
	}
	runID := state.Run.ID
	if err := s.releaseStaleSlot(workspaceID, project, agent, now); err != nil {
		s.serverError(w, err)
		return
	}
	run, _, _ := s.controlDB.RuntimeRunByID(workspaceID, runID)
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID,
		Action:       "runtime_run.slot_released",
		ResourceType: "runtime_run",
		ResourceID:   runID,
		Summary:      fmt.Sprintf("Stale runtime run force-released from %s/%s slot via slot observability endpoint", project, agent),
		After: map[string]any{
			"taskId":         run.TaskID,
			"leaseExpiredAt": run.LeaseExpiresAt,
			"actor":          requestUsername(r),
		},
		Request: r,
	})
	_ = json.NewEncoder(w).Encode(map[string]any{"released": true, "runId": runID, "taskId": run.TaskID})
}

// releaseStaleSlot force-releases the stale run holding project/agent's slot
// via the reaper's exact fenced path. Returns an error when the slot is
// unexpectedly empty or the run was renewed/finished concurrently.
func (s *Server) releaseStaleSlot(workspaceID, project, agent string, now time.Time) error {
	state := s.runtimeSlotState(workspaceID, project, agent, now)
	if !state.Occupied || state.Run == nil {
		return fmt.Errorf("agent slot is not occupied")
	}
	if !state.Releasable {
		return fmt.Errorf("run is not stale (lease not expired past the reaper grace)")
	}
	runID := state.Run.ID
	cutoff := controldb.LeaseExpiredCutoff(runtimeReaperLeaseGrace, now)
	run, found, err := s.controlDB.RuntimeRunByID(workspaceID, runID)
	if err != nil || !found {
		return fmt.Errorf("runtime run %s not found", runID)
	}
	reaped, err := s.controlDB.ReapExpiredRuntimeRun(workspaceID, runID, run.LeaseGeneration, cutoff)
	if err != nil {
		return err
	}
	if !reaped {
		return fmt.Errorf("run %s was renewed or finished concurrently; nothing to release", runID)
	}
	s.transitionReapedTask(workspaceID, run)
	if strings.TrimSpace(run.TaskID) != "" && s.ts != nil {
		if !s.clearStaleTaskRuntimeTokenByID(workspaceID, run) {
			s.reconcileStaleTaskToken(workspaceID, run)
		}
	}
	return nil
}
