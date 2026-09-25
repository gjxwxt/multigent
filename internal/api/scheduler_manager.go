package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

type schedulerProcess struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	mode      string
	project   string
	agent     string
	startedAt time.Time
	stopped   bool
	exitErr   error
	doneCh    chan struct{}
}

const (
	schedulerModeLocal       = "local"
	schedulerModeRuntimeNode = "runtime-node"
	schedulerModeWakeup      = "wakeup"
	schedulerModeManualTask  = "manual-task"
)

type SchedulerManager struct {
	mu      sync.Mutex
	root    string
	binPath string
	procs   map[string]*schedulerProcess // key = "all", "project", "project/agent", or "worker/<id>"
}

func newSchedulerManager(root string) *SchedulerManager {
	bin, _ := os.Executable()
	return &SchedulerManager{
		root:    root,
		binPath: bin,
		procs:   make(map[string]*schedulerProcess),
	}
}

func schedKey(project, agent string) string {
	if project == "" {
		return "all"
	}
	if agent == "" {
		return project
	}
	return project + "/" + agent
}

func (m *SchedulerManager) Start(project, agent string) error {
	key := schedKey(project, agent)
	args := []string{"--dir", m.root, "scheduler", "start"}
	if project != "" {
		args = append(args, "--project", project)
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}

	cmd := exec.Command(m.binPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcGroup(cmd)

	_, err := m.StartManagedCommand(key, project, agent, schedulerModeLocal, cmd)
	return err
}

// StartWorkspace starts the single scheduler owned by the API service. The
// scheduler scans the whole workspace; projects are execution context only.
func (m *SchedulerManager) StartWorkspace() error {
	return m.Start("", "")
}

func (m *SchedulerManager) StartManagedCommand(key, project, agent, mode string, cmd *exec.Cmd) (int, error) {
	if cmd == nil {
		return 0, fmt.Errorf("command is nil")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = schedKey(project, agent)
	}
	if strings.TrimSpace(mode) == "" {
		mode = schedulerModeLocal
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[key]; ok {
		select {
		case <-p.doneCh:
			// process already exited, allow restart
		default:
			return 0, fmt.Errorf("scheduler already running for %q", key)
		}
	}
	logFile := m.attachManagedCommandLog(key, cmd)
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return 0, fmt.Errorf("start managed command: %w", err)
	}

	proc := &schedulerProcess{
		cmd:       cmd,
		mode:      mode,
		project:   project,
		agent:     agent,
		startedAt: time.Now(),
		doneCh:    make(chan struct{}),
	}

	go func() {
		err := cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		proc.mu.Lock()
		proc.exitErr = err
		proc.stopped = true
		proc.mu.Unlock()
		close(proc.doneCh)
	}()

	m.procs[key] = proc
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	return pid, nil
}

func (m *SchedulerManager) attachManagedCommandLog(key string, cmd *exec.Cmd) *os.File {
	if cmd == nil {
		return nil
	}
	cmd.Stdin = nil
	dir := filepath.Join(m.root, ".multigent", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("warning: failed to create managed command log dir: %v", err)
		return nil
	}
	name := strings.TrimSpace(key)
	if name == "" {
		name = "command"
	}
	name = strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-").Replace(name)
	path := filepath.Join(dir, "managed-"+name+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("warning: failed to open managed command log %s: %v", path, err)
		return nil
	}
	fmt.Fprintf(f, "\n--- %s start %s %s ---\n", time.Now().UTC().Format(time.RFC3339), cmd.Path, strings.Join(cmd.Args[1:], " "))
	cmd.Stdout = f
	cmd.Stderr = f
	return f
}

func (m *SchedulerManager) StartLoop(project, agent, mode string, loop func(context.Context)) error {
	return m.StartLoopWithKey(schedKey(project, agent), project, agent, mode, loop)
}

func (m *SchedulerManager) StartLoopWithKey(key, project, agent, mode string, loop func(context.Context)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key = strings.TrimSpace(key)
	if key == "" {
		key = schedKey(project, agent)
	}
	if p, ok := m.procs[key]; ok {
		select {
		case <-p.doneCh:
			// loop already exited, allow restart
		default:
			return fmt.Errorf("scheduler already running for %q", key)
		}
	}
	if strings.TrimSpace(mode) == "" {
		mode = schedulerModeRuntimeNode
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc := &schedulerProcess{
		cancel:    cancel,
		mode:      mode,
		project:   project,
		agent:     agent,
		startedAt: time.Now(),
		doneCh:    make(chan struct{}),
	}
	go func() {
		defer close(proc.doneCh)
		defer cancel()
		loop(ctx)
		proc.mu.Lock()
		proc.stopped = true
		proc.mu.Unlock()
	}()
	m.procs[key] = proc
	return nil
}

func (m *SchedulerManager) Stop(project, agent string) error {
	return m.StopKey(schedKey(project, agent))
}

func (m *SchedulerManager) StopKey(key string) error {
	m.mu.Lock()
	key = strings.TrimSpace(key)
	proc, ok := m.procs[key]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no scheduler running for %q", key)
	}

	select {
	case <-proc.doneCh:
		return fmt.Errorf("scheduler for %q already stopped", key)
	default:
	}

	if proc.cancel != nil {
		proc.cancel()
	} else if proc.cmd != nil && proc.cmd.Process != nil {
		killProcessGroup(proc.cmd.Process.Pid)
	}

	select {
	case <-proc.doneCh:
	case <-time.After(5 * time.Second):
		if proc.cmd != nil && proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
		}
	}

	m.mu.Lock()
	delete(m.procs, key)
	m.mu.Unlock()
	return nil
}

type schedStatus struct {
	Key       string `json:"key"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Project   string `json:"project,omitempty"`
	Agent     string `json:"agent,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (m *SchedulerManager) Status() []schedStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]schedStatus, 0, len(m.procs))
	for key, proc := range m.procs {
		s := schedStatus{
			Key:     key,
			Mode:    proc.mode,
			Project: proc.project,
			Agent:   proc.agent,
		}
		select {
		case <-proc.doneCh:
			s.Running = false
			proc.mu.Lock()
			if proc.exitErr != nil {
				s.Error = proc.exitErr.Error()
			}
			proc.mu.Unlock()
		default:
			s.Running = true
			if proc.cmd != nil && proc.cmd.Process != nil {
				s.PID = proc.cmd.Process.Pid
			}
			s.StartedAt = proc.startedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	return out
}

func (m *SchedulerManager) WaitKey(key string) error {
	m.mu.Lock()
	proc, ok := m.procs[strings.TrimSpace(key)]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no scheduler running for %q", key)
	}
	<-proc.doneCh
	proc.mu.Lock()
	err := proc.exitErr
	proc.mu.Unlock()
	m.pruneStopped()
	return err
}

type desiredSchedulerSpec struct {
	Key     string `json:"key,omitempty"`
	Project string `json:"project,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

func (spec desiredSchedulerSpec) schedulerKey() string {
	if key := strings.TrimSpace(spec.Key); key != "" {
		return key
	}
	return schedKey(spec.Project, spec.Agent)
}

func schedulerDesiredSettingKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "scheduler.desired." + hex.EncodeToString(sum[:8])
}

func (s *Server) loadDesiredSchedulers() ([]desiredSchedulerSpec, error) {
	raw, ok, err := s.controlDB.GetSetting(schedulerDesiredSettingKey(s.root))
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return nil, err
	}
	var specs []desiredSchedulerSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil, err
	}
	return specs, nil
}

func (s *Server) saveDesiredSchedulers(specs []desiredSchedulerSpec) error {
	b, err := json.Marshal(specs)
	if err != nil {
		return err
	}
	return s.controlDB.SetSetting(schedulerDesiredSettingKey(s.root), string(b))
}

func (s *Server) setSchedulerDesired(project, agent, mode string, running bool) {
	s.setSchedulerDesiredKey(schedKey(project, agent), project, agent, mode, running)
}

func (s *Server) setSchedulerDesiredKey(key, project, agent, mode string, running bool) {
	s.schedulerDesiredMu.Lock()
	defer s.schedulerDesiredMu.Unlock()

	specs, err := s.loadDesiredSchedulers()
	if err != nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = schedKey(project, agent)
	}
	next := make([]desiredSchedulerSpec, 0, len(specs)+1)
	found := false
	for _, spec := range specs {
		if spec.schedulerKey() == key {
			found = true
			if running {
				next = append(next, desiredSchedulerSpec{Key: key, Project: project, Agent: agent, Mode: strings.TrimSpace(mode)})
			}
			continue
		}
		next = append(next, spec)
	}
	if running && !found {
		next = append(next, desiredSchedulerSpec{Key: key, Project: project, Agent: agent, Mode: strings.TrimSpace(mode)})
	}
	_ = s.saveDesiredSchedulers(next)
}

func (s *Server) restoreDesiredSchedulers() {
	time.Sleep(300 * time.Millisecond)
	specs, err := s.loadDesiredSchedulers()
	if err != nil || len(specs) == 0 {
		return
	}
	for _, spec := range specs {
		if spec.Agent != "" && spec.Project == "" {
			continue
		}
		if spec.Mode == schedulerModeRuntimeNode {
			log.Printf("runtime-node scheduler %s restored as desired but not auto-started; start it from the web/API so the runtime API URL is known", schedKey(spec.Project, spec.Agent))
			continue
		}
		if err := s.sched.Start(spec.Project, spec.Agent); err != nil {
			continue
		}
	}
}

// StartWorkspaceScheduler makes periodic activity part of the server
// lifecycle. Callers should not need to start a project scheduler manually.
// Also starts the workspace-level runtime reaper singleton (Q0 PR-2):
// idempotent — repeated starts never spawn a second loop.
func (s *Server) StartWorkspaceScheduler() error {
	if s == nil || s.sched == nil {
		return fmt.Errorf("scheduler manager is unavailable")
	}
	s.startRuntimeReaper(context.Background())
	s.startWorktreeReaper(context.Background())
	return s.sched.StartWorkspace()
}

func (m *SchedulerManager) Cleanup() {
	m.mu.Lock()
	keys := make([]string, 0, len(m.procs))
	for k := range m.procs {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	for _, k := range keys {
		_ = m.StopKey(k)
	}
}

func (m *SchedulerManager) GracefulShutdown(ctx context.Context) []schedStatus {
	m.mu.Lock()
	procs := make(map[string]*schedulerProcess, len(m.procs))
	for key, proc := range m.procs {
		procs[key] = proc
	}
	m.mu.Unlock()

	for _, proc := range procs {
		if proc.cancel != nil {
			proc.cancel()
			continue
		}
		// Long-lived scheduler commands should stop accepting new work. One-shot
		// manual runs and wakeups are left alone so the active agent can finish.
		if proc.mode == schedulerModeLocal && proc.cmd != nil && proc.cmd.Process != nil {
			_ = proc.cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		running := m.runningStatuses()
		if len(running) == 0 {
			m.pruneStopped()
			return nil
		}
		select {
		case <-ctx.Done():
			return running
		case <-ticker.C:
		}
	}
}

func (m *SchedulerManager) runningStatuses() []schedStatus {
	statuses := m.Status()
	running := make([]schedStatus, 0, len(statuses))
	for _, status := range statuses {
		if status.Running {
			running = append(running, status)
		}
	}
	return running
}

func (m *SchedulerManager) pruneStopped() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, proc := range m.procs {
		select {
		case <-proc.doneCh:
			delete(m.procs, key)
		default:
		}
	}
}

// ── HTTP handlers ──

func (s *Server) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	statuses := s.sched.Status()
	cur := s.currentUser(r)
	if cur.Role != RoleAdmin && !s.canAdminCurrentWorkspace(r) {
		filtered := make([]schedStatus, 0, len(statuses))
		for _, st := range statuses {
			if st.Project == "" {
				continue
			}
			if _, ok := s.users.HasProjectAccess(cur.Username, st.Project); ok {
				filtered = append(filtered, st)
			}
		}
		statuses = filtered
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schedulers": statuses,
	})
}

// ensureAgentSchedulerRunning keeps scheduler lifecycle behind agent settings.
// The web UI should configure an agent, not manage a process-wide switch.
func (s *Server) ensureAgentSchedulerRunning(r *http.Request, workspaceID, project, agent string) {
	project = strings.TrimSpace(project)
	agent = strings.TrimSpace(agent)
	if project == "" || agent == "" {
		return
	}
	for _, status := range s.sched.Status() {
		if status.Running && (status.Key == "all" || status.Key == schedKey(project, agent)) {
			return
		}
	}
	mode := schedulerModeLocal
	key := schedKey(project, agent)
	useRuntimeNode := false
	if meta, err := s.agentMetaForProjectMember(workspaceID, project, agent); err == nil && meta != nil {
		useRuntimeNode = s.usesAssignedRuntimeNode(workspaceID, meta)
	}
	if useRuntimeNode {
		serverURL := externalServerURL(r)
		key = s.schedulerProcessKeyForProjectAgent(workspaceID, project, agent, schedulerModeRuntimeNode)
		mode = schedulerModeRuntimeNode
		if err := s.sched.StartLoopWithKey(key, project, agent, mode, func(ctx context.Context) {
			s.runtimeSchedulerLoop(ctx, workspaceID, project, agent, serverURL, requestUsername(r))
		}); err != nil {
			log.Printf("scheduler auto-start %s: %v", key, err)
			return
		}
	} else {
		// Local heartbeat/cron execution is owned by the one workspace scheduler
		// started with the API service. Agent settings only wake it when needed.
		if err := s.sched.StartWorkspace(); err != nil && !strings.Contains(err.Error(), "already running") {
			log.Printf("workspace scheduler auto-start: %v", err)
		}
		return
	}
	s.setSchedulerDesiredKey(key, project, agent, mode, true)
}

type schedActionBody struct {
	Project string `json:"project"`
	Agent   string `json:"agent"`
}

func (s *Server) handleSchedulerStart(w http.ResponseWriter, r *http.Request) {
	var body schedActionBody
	if r.ContentLength > 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	project := strings.TrimSpace(body.Project)
	agent := strings.TrimSpace(body.Agent)

	if agent != "" && project == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "agent requires project")
		return
	}
	if project == "" {
		if !s.checkCurrentWorkspaceAdmin(w, r) {
			return
		}
	} else if !s.checkProjectManager(w, r, project) {
		return
	}

	workspaceID, workspaceOK := s.currentWorkspaceForRequest(w, r)
	if !workspaceOK {
		return
	}
	mode := schedulerModeLocal
	key := schedKey(project, agent)
	useRuntimeNode := false
	if project != "" && agent != "" {
		if meta, err := s.agentMetaForProjectMember(workspaceID, project, agent); err == nil && meta != nil {
			useRuntimeNode = s.usesAssignedRuntimeNode(workspaceID, meta)
		}
	}
	if useRuntimeNode {
		serverURL := externalServerURL(r)
		actor := requestUsername(r)
		mode = schedulerModeRuntimeNode
		key = s.schedulerProcessKeyForProjectAgent(workspaceID, project, agent, mode)
		if err := s.sched.StartLoopWithKey(key, project, agent, mode, func(ctx context.Context) {
			s.runtimeSchedulerLoop(ctx, workspaceID, project, agent, serverURL, actor)
		}); err != nil {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeSchedulerConflict, err.Error())
			return
		}
	} else if err := s.sched.Start(project, agent); err != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeSchedulerConflict, err.Error())
		return
	}
	s.setSchedulerDesiredKey(key, project, agent, mode, true)
	s.auditLog(auditLogInput{
		Action:       "scheduler.start",
		ResourceType: "scheduler",
		ResourceID:   schedKey(project, agent),
		Summary:      "Scheduler started",
		After: map[string]any{
			"project": project,
			"agent":   agent,
			"key":     key,
			"mode":    mode,
		},
		Request: r,
	})

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":   true,
		"key":  key,
		"mode": mode,
	})
}

func (s *Server) handleSchedulerWakeup(w http.ResponseWriter, r *http.Request) {
	var body schedActionBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	project := strings.TrimSpace(body.Project)
	agent := strings.TrimSpace(body.Agent)
	if project == "" || agent == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "project and agent are required")
		return
	}
	if !s.checkProjectManager(w, r, project) {
		return
	}
	workspaceID, workspaceOK := s.currentWorkspaceForRequest(w, r)
	if !workspaceOK {
		return
	}

	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
	hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
	if err != nil || hb == nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeValidationFailed, "heartbeat not found")
		return
	}
	if hb.PID > 0 && hb.LastWakeupStatus == "running" && processAlive(hb.PID) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeSchedulerWakeupFailed, fmt.Sprintf("agent %s/%s is already running", project, agent))
		return
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeAgentNotFound, "agent not found")
			return
		}
		s.serverError(w, err)
		return
	}
	if readiness := s.runtimeReadinessForExecution(workspaceID, meta); readiness.Blocking {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeRuntimeNotReady, runtimeReadinessErrorMessage(readiness))
		return
	}
	if s.usesAssignedRuntimeNode(workspaceID, meta) {
		run, task, err := s.enqueueRuntimeWakeupRunFromRequest(workspaceID, project, agent, hb, externalServerURL(r), requestUsername(r))
		if err != nil {
			s.jsonErrorCode(w, http.StatusInternalServerError, ErrCodeSchedulerWakeupFailed, fmt.Sprintf("queue runtime wakeup failed: %v", err))
			return
		}
		_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
		s.auditLog(auditLogInput{
			Action:       "scheduler.wakeup",
			ResourceType: "agent",
			ResourceID:   project + "/" + agent,
			Summary:      "Agent wakeup queued on runtime node",
			After: map[string]any{
				"project":      project,
				"agent":        agent,
				"taskId":       task.ID,
				"runtimeRunId": run.ID,
				"runtime":      "node",
			},
			Request: r,
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "queued", "runtimeRunId": run.ID, "taskId": task.ID})
		return
	}

	args := []string{"--dir", s.sched.root, "scheduler", "wakeup", "--project", project, "--agent", agent}
	cmd := exec.Command(s.sched.binPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcGroup(cmd)
	procKey := fmt.Sprintf("wakeup/%s/%s/%d", project, agent, time.Now().UnixNano())
	pid, err := s.sched.StartManagedCommand(procKey, project, agent, schedulerModeWakeup, cmd)
	if err != nil {
		s.jsonErrorCode(w, http.StatusInternalServerError, ErrCodeSchedulerWakeupFailed, fmt.Sprintf("start wakeup failed: %v", err))
		return
	}
	go func() {
		if err := s.sched.WaitKey(procKey); err != nil {
			log.Printf("scheduler wakeup %s/%s exited with error: %v", project, agent, err)
		}
	}()
	s.auditLog(auditLogInput{
		Action:       "scheduler.wakeup",
		ResourceType: "agent",
		ResourceID:   project + "/" + agent,
		Summary:      "Agent wakeup requested",
		After: map[string]any{
			"project": project,
			"agent":   agent,
		},
		Request: r,
	})
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid, "status": "started"})
}

func (s *Server) handleStartProjectTask(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	if project == "" || taskID == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "project and taskId are required")
		return
	}
	if !s.checkProjectManager(w, r, project) {
		return
	}
	workspaceID, workspaceOK := s.currentWorkspaceForRequest(w, r)
	if !workspaceOK {
		return
	}
	task, agent, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeValidationFailed, "task not found")
		return
	}
	resumedStage, err := s.reconcileWorkflowTaskBeforeManualStart(workspaceID, project, taskID, r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if resumedStage {
		// Review round 5 (D-B): the workflow was parked on a parallel stage
		// whose fan-out never materialized; the reconcile re-drove it and the
		// branch tasks are now dispatched. Starting an agent run on the PARENT
		// task here would be wrong (no agent owns the stage) — report the
		// resume instead of dispatching.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"status": "workflow_stage_resumed",
			"detail": "workflow run was parked on a parallel stage without its branch instances; the fan-out was (re)driven and the branch tasks were dispatched. The parent task is not agent-dispatchable at this step.",
		})
		return
	}
	task, agent, err = s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeValidationFailed, "task not found")
		return
	}
	// S2 round 9 (D-J): a run that a terminated console left FAILED refuses
	// re-entry forever and the platform has no run-level recovery — the real
	// acceptance run died at its implementation step after a deploy restart.
	// Manual start re-activates exactly that failed step and FALLS THROUGH into
	// the ordinary dispatch below (this lever exists to run the agent again).
	reactivated, _, rErr := s.reactivateFailedRunForManualStart(workspaceID, project, agent, task)
	if rErr != nil {
		// A refusal (non-agent step, wrong agent) is an operator error, not a
		// server fault: surface it as a conflict with the workflow's message.
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, rErr.Error())
		return
	}
	if reactivated {
		log.Printf("[workflow-run-reactivate] task %s: continuing into the normal dispatch after re-activating the failed run", task.ID)
	}
	// Review round 5 (D-B companion): a task whose workflow is parked on a
	// step that no agent owns (a human gate, or a materialized parallel stage
	// waiting on its branch children) refuses manual start — the reconcile
	// above already re-drove anything it could, and dispatching an agent run
	// at a step the agent does not own only burns a run and muddies the task.
	if stepType, parked, stepErr := s.workflowActiveStepType(workspaceID, project, taskID); stepErr != nil {
		s.serverError(w, stepErr)
		return
	} else if parked && stepType != "agent_task" {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "workflow task is parked at a "+stepType+" step that no agent owns; resuming it is a workflow action, not a task start (parallel stages dispatch their branch tasks, human gates wait for their reviewer)")
		return
	}
	// S2 round 7 (D-G): a fan-out branch child whose delivery was recorded but
	// whose join never converged (rejected report, or the evidence worktree was
	// retired) has no agent left to re-report it — the child run is terminal and
	// the fan-out re-drive skips branches that already have instances. Manual
	// start is the platform's operator lever, so it re-drives the join here:
	// restore the delivery worktree from the recorded branch and re-run the SAME
	// gate against live evidence with the agent's recorded declaration. Nothing
	// about the delivery is asserted by the operator.
	if resumed, status, joinErr := s.resumePendingBranchJoinForTask(workspaceID, project, agent, task, r); resumed {
		if joinErr != nil {
			if status == "branch_join_rejected" || status == "branch_join_restore_failed" || status == "branch_join_declaration_missing" {
				s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, joinErr.Error())
				return
			}
			if reason, isRefusal := planRefusalReason(joinErr); isRefusal {
				s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, reason)
				return
			}
			s.serverError(w, joinErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"status": status,
			"taskId": task.ID,
			"detail": "a parked branch join was re-driven: the delivery worktree was restored from the recorded branch and the join gate re-evaluated the agent's recorded declaration against live git evidence",
		})
		return
	}
	if task.Status.IsTerminal() {
		if task.Status == entity.TaskStatusDoneFailed || task.Status == entity.TaskStatusCancelled {
			// Allow restarting/retrying failed or cancelled tasks
			task.Status = entity.TaskStatusPending
			task.StartedAt = nil
			task.FinishedAt = nil
			task.LastError = ""
			task.UpdatedAt = time.Now().UTC()
			if err := s.ts.UpdateTask(project, agent, task); err != nil {
				s.serverError(w, err)
				return
			}
		} else {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, "task is already finished")
			return
		}
	}
	if task.Status == entity.TaskStatusAwaitingConfirmation {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, "current assignee is not an agent")
		return
	}
	// Q0 D4: infra-blocked tasks refuse manual start until explicitly
	// unblocked; backoff tasks refuse until NotBefore passes.
	if taskBlockedByInfraFailures(task) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, "task is blocked after repeated infrastructure failures; resolve the failure and POST /api/v1/projects/"+project+"/tasks/"+task.ID+"/unblock to re-enable dispatch")
		return
	}
	if task.Status == entity.TaskStatusPending && task.NotBefore != nil {
		if remaining := time.Until(*task.NotBefore); remaining > 0 {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, fmt.Sprintf("task is backing off after an infrastructure failure; retry available in %s", remaining.Round(time.Second)))
			return
		}
	}
	pid, runID, err := s.startProjectTaskDirect(workspaceID, project, agent, task, r)
	if err != nil {
		if errors.Is(err, errAgentAlreadyRunning) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeSchedulerWakeupFailed, err.Error())
			return
		}
		if errors.Is(err, errRuntimeNotReady) {
			s.jsonErrorCode(w, http.StatusConflict, ErrCodeRuntimeNotReady, err.Error())
			return
		}
		s.jsonErrorCode(w, http.StatusInternalServerError, ErrCodeSchedulerWakeupFailed, err.Error())
		return
	}
	if runID != "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "queued", "runtimeRunId": runID, "taskId": task.ID, "agent": agent})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid, "status": "started", "taskId": task.ID, "agent": agent})
}

var (
	errAgentAlreadyRunning = errors.New("agent is already running")
	errRuntimeNotReady     = errors.New("runtime not ready")
)

// acquireAgentStartGate serializes task/wakeup starts per agent (P2 soak
// race): two autoStarts landing on the same agent in one tick must not race
// the heartbeat-PID and interaction-lock ladders, so the loser's first run
// exited 1 ("agent busy in manual_run") and only a later scheduler wake
// recovered it. Holding a gate across the busy check and the enqueue/exec
// hand-off turns that race into queue-join semantics: the loser enqueues its
// own run (node path) or starts after the winner (local path).
// The gate keys on the resolved AgentWorker (workspaceID + workerID), NOT on
// project/agent: the same AgentWorker can be addressed from multiple projects
// (shared agent), and two project-keyed gates would run the busy ladders
// concurrently against one worker — exactly the soak race this lock exists
// to prevent. Falls back to project/agent only when the directory cannot
// resolve a worker (e.g. local-exec agents without a mailbox).
// The returned function releases the gate.
func (s *Server) acquireAgentStartGate(workspaceID, project, agent string) func() {
	if s == nil {
		return func() {}
	}
	key := workspaceID + "/" + project + "/" + agent
	if s.agentDirectory != nil && strings.TrimSpace(workspaceID) != "" {
		if workerID, _ := s.agentWorkerContextForProjectAgent(workspaceID, project, agent); workerID != "" {
			key = workspaceID + "/worker/" + workerID
		}
	}
	if s.agentStartTestHook != nil {
		return s.agentStartTestHook(key)
	}
	s.agentStartMu.Lock()
	if s.agentStartGates == nil {
		s.agentStartGates = map[string]*uint32{}
	}
	gate, ok := s.agentStartGates[key]
	if !ok {
		gate = new(uint32)
		s.agentStartGates[key] = gate
	}
	s.agentStartMu.Unlock()
	for {
		if atomic.CompareAndSwapUint32(gate, 0, 1) {
			return func() { atomic.StoreUint32(gate, 0) }
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *Server) startProjectTaskDirect(workspaceID, project, agent string, task *entity.Task, r *http.Request) (int, string, error) {
	// P2 soak autoStart race: two autoStarts landing on the same agent in one
	// tick must not race the busy-check ladders below — serialize per worker
	// first so the loser joins the queue instead of exit-1ing.
	releaseGate := s.acquireAgentStartGate(workspaceID, project, agent)
	defer releaseGate()
	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
	hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
	if err != nil || hb == nil {
		return 0, "", fmt.Errorf("heartbeat not found")
	}
	if hb.PID > 0 && hb.LastWakeupStatus == "running" && processAlive(hb.PID) {
		return 0, "", fmt.Errorf("%w: agent %s/%s is already running", errAgentAlreadyRunning, project, agent)
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil {
		return 0, "", err
	}
	if readiness := s.runtimeReadinessForExecution(workspaceID, meta); readiness.Blocking {
		return 0, "", fmt.Errorf("%w: %s", errRuntimeNotReady, runtimeReadinessErrorMessage(readiness))
	}
	if s.usesAssignedRuntimeNode(workspaceID, meta) {
		// Q0 收口 5 (task-queue semantics): manual start JOINS the queue
		// instead of 409ing on any queued/running run — run_key idempotency
		// (idx_runtime_runs_active_key) converges a double click or a
		// scheduler tick racing this start onto the SAME run, and a running
		// run simply keeps its lease while the enqueue is deduped. The
		// per-task slot guard lives in the fence (task.ActiveRuntimeRunID),
		// not here.
		run, err := s.enqueueSpecificRuntimeTaskRunFromRequest(workspaceID, project, agent, task, hb, externalServerURL(r), requestUsername(r))
		if err != nil {
			// D-5 fix (2026-09-24): a worker explicitly bound to a runtime node
			// must NEVER silently fall back to local console execution — that
			// fallback produced hollow done_success runs (no model, no delivery).
			// Fail loudly; the caller surfaces the error to the user.
			return 0, "", fmt.Errorf("queue task run on runtime node failed: %w", err)
		}
		_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
		s.auditLog(auditLogInput{
			Action:       "task.start",
			ResourceType: "task",
			ResourceID:   project + "/" + agent + "/" + task.ID,
			Summary:      "Specific task queued on runtime node",
			After: map[string]any{
				"project":      project,
				"agent":        agent,
				"taskId":       task.ID,
				"runtimeRunId": run.ID,
				"runtime":      "node",
			},
			Request: r,
		})
		return 0, run.ID, nil
	}

	args := []string{"--dir", s.sched.root, "run", "--project", project, "--agent", agent, "--task", task.ID}
	cmd := exec.Command(s.sched.binPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcGroup(cmd)
	procKey := fmt.Sprintf("manual-task/%s/%s/%s", project, agent, task.ID)
	pid, err := s.sched.StartManagedCommand(procKey, project, agent, schedulerModeManualTask, cmd)
	if err != nil {
		return 0, "", fmt.Errorf("start task run failed: %w", err)
	}
	now := time.Now().UTC()
	hb.LastWakeup = &now
	hb.LastWakeupStatus = "running"
	hb.PID = pid
	_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
	go func() {
		if err := s.sched.WaitKey(procKey); err != nil {
			log.Printf("manual task run %s/%s task=%s exited with error: %v", project, agent, task.ID, err)
		}
		if hb2, err := s.loadSchedulerTargetHeartbeat(workspaceID, target); err == nil && hb2 != nil && hb2.PID == pid {
			hb2.PID = 0
			if hb2.LastWakeupStatus == "running" {
				hb2.LastWakeupStatus = "done"
			}
			_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb2)
		}
	}()
	s.auditLog(auditLogInput{
		Action:       "task.start",
		ResourceType: "task",
		ResourceID:   project + "/" + agent + "/" + task.ID,
		Summary:      "Specific task run requested",
		After: map[string]any{
			"project": project,
			"agent":   agent,
			"taskId":  task.ID,
			"pid":     pid,
		},
		Request: r,
	})
	return pid, "", nil
}

func (s *Server) reconcileWorkflowTaskBeforeManualStart(workspaceID, project, taskID string, r *http.Request) (bool, error) {
	if s == nil || s.controlDB == nil || strings.TrimSpace(workspaceID) == "" {
		return false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil || !found || strings.TrimSpace(run.ActiveStepID) == "" || strings.TrimSpace(run.Status) == "completed" {
		return false, err
	}
	if run.DefinitionID == workflowstore.ProjectInitializationWorkflowID && run.Status == "failed" {
		steps, err := wfStore.ListStepInstances(run.ID)
		if err != nil {
			return false, err
		}
		now := time.Now().UTC()
		for i := range steps {
			if steps[i].StepID != run.ActiveStepID {
				continue
			}
			steps[i].Status = "pending"
			steps[i].Summary = ""
			steps[i].OutputArtifact = ""
			steps[i].OutputValues = nil
			steps[i].StartedAt = time.Time{}
			steps[i].FinishedAt = time.Time{}
			steps[i].UpdatedAt = now
			if err := wfStore.SaveStepInstance(&steps[i]); err != nil {
				return false, err
			}
			break
		}
		run.Status = "active"
		run.UpdatedAt = now
		if err := wfStore.SaveRun(&run); err != nil {
			return false, err
		}
	}
	def, found, err := wfStore.RunDefinition(run)
	if err != nil || !found {
		return false, err
	}
	steps, err := wfStore.ListStepInstances(run.ID)
	if err != nil {
		return false, err
	}
	resumed, err := s.resumeParkedParallelStage(workspaceID, project, taskID, run, def, steps, r)
	if err != nil {
		return false, err
	}
	if err := s.reconcileActiveWorkflowTaskQueue(workspaceID, project, taskID, run, def, steps, r); err != nil {
		return false, err
	}
	return resumed, nil
}

// resumeParkedParallelStage re-drives the fan-out activation for a run parked
// on a parallel stage that has NO branch instances (review round 5, D-B).
//
// The activation runs AFTER the parent transition commits, so any failure
// inside it (a missing branch binding, a worktree materialization error)
// leaves exactly this shape: contract_review completed, the run active on the
// parallel stage, zero branch instances, and the stage itself refusing step
// reports (D-C guard). No other production path re-enters
// activateParallelWorkflowStep — it is reached only from step-completion
// callbacks whose transition targets the stage, and that transition is
// already consumed — so real S2 run wfr-wlhdwalv (2026-09-24) deadlocked here
// until an operator had no sanctioned action at all.
//
// The manual start path ("也可手动启动" / the console's resume action) is the
// sanctioned operator entry, and activateParallelWorkflowStep is idempotent
// (branches with instances are skipped, and the branch-task idempotency key
// dedups a partial re-drive), so re-running it here is safe by construction.
// The frozen fan-out base commit is read from the parent task's Vars, so a
// late re-drive cannot silently re-baseline the branches against a moved
// main. Returns true when the stage was resumed.
func (s *Server) resumeParkedParallelStage(workspaceID, project, taskID string, run entity.WorkflowRun, def entity.WorkflowDefinition, steps []entity.WorkflowStepInstance, r *http.Request) (bool, error) {
	if s == nil || s.controlDB == nil || strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.ActiveStepID) == "" {
		return false, nil
	}
	step, ok := workflowDefinitionStepByID(def.Steps, run.ActiveStepID)
	if !ok || strings.TrimSpace(step.Type) != "parallel_stage" {
		return false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	instances, err := wfStore.BranchInstancesForStep(run.ID, step.ID)
	if err != nil {
		return false, err
	}
	if len(instances) > 0 {
		return false, nil
	}
	inst, ok := workflowStepInstanceByStepID(steps, run.ActiveStepID)
	if !ok {
		return false, nil
	}
	task, agent, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		return false, nil
	}
	transition := workflowstore.TransitionResult{Run: run, Current: inst, Next: &step, NextInst: &inst}
	if err := s.activateParallelWorkflowStep(workspaceID, project, agent, task, transition, r); err != nil {
		return false, err
	}
	log.Printf("[fanout] re-drove parked parallel stage %s for run %s (task %s): branches materialized from the manual start path", step.ID, run.ID, taskID)
	return true, nil
}

// workflowActiveStepType reports the run's active step type for the manual
// start path (review round 5, D-B companion). A task whose workflow is parked
// on a step no agent owns — a human gate, or a parallel stage waiting on its
// branch children — must not be dispatched as an agent run: the agent would
// burn a run on work it does not own, and before the D-C guard it could even
// complete the stage report-style and skip the fan-out.
func (s *Server) workflowActiveStepType(workspaceID, project, taskID string) (string, bool, error) {
	if s == nil || s.controlDB == nil {
		return "", false, nil
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	run, found, err := wfStore.RunForTask(project, taskID)
	if err != nil || !found {
		return "", false, err
	}
	switch strings.TrimSpace(run.Status) {
	case "completed", "failed", "cancelled":
		return "", false, nil
	}
	if strings.TrimSpace(run.ActiveStepID) == "" {
		return "", false, nil
	}
	def, found, err := wfStore.RunDefinition(run)
	if err != nil || !found {
		return "", false, err
	}
	step, ok := workflowDefinitionStepByID(def.Steps, run.ActiveStepID)
	if !ok {
		return "", false, nil
	}
	return strings.TrimSpace(step.Type), true, nil
}

func (s *Server) enqueueSpecificRuntimeTaskRunFromRequest(workspaceID, project, agent string, task *entity.Task, hb *entity.HeartbeatConfig, serverURL, actor string) (controldb.RuntimeRun, error) {
	if task == nil {
		return controldb.RuntimeRun{}, fmt.Errorf("task not found")
	}
	now := time.Now().UTC()
	prev := task.Status
	task.Status = entity.TaskStatusInProgress
	task.UpdatedAt = now
	task.FinishedAt = nil
	entity.ApplyStatusTimestamps(task, prev, now)
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		return controldb.RuntimeRun{}, err
	}
	sessionID := ""
	if hb != nil {
		sessionID = strings.TrimSpace(hb.SessionID)
	}
	run, err := s.enqueueRuntimeTaskRun(workspaceID, project, agent, task, sessionID, serverURL, actor)
	if err != nil {
		task.Status = prev
		task.UpdatedAt = now
		_ = s.ts.UpdateTask(project, agent, task)
		return controldb.RuntimeRun{}, err
	}
	if hb != nil {
		hb.LastWakeup = &now
		hb.LastWakeupStatus = "running"
		hb.PID = 0
	}
	return run, nil
}

func (s *Server) enqueueRuntimeWakeupRunFromRequest(workspaceID, project, agent string, hb *entity.HeartbeatConfig, serverURL, actor string) (controldb.RuntimeRun, *entity.Task, error) {
	// Bind every scheduler-created task and run to the canonical project
	// membership title. Channel bindings and old installations may still use
	// the worker name, but mixing those identities creates tasks that the
	// running agent cannot complete through mga.
	if target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent); target.agent != "" {
		project = target.project
		agent = target.agent
	}
	task, attentionIDs, err := s.nextRuntimeWakeupTask(workspaceID, project, agent, hb)
	if err != nil {
		return controldb.RuntimeRun{}, nil, err
	}
	if task == nil {
		return controldb.RuntimeRun{}, nil, fmt.Errorf("no pending task or wakeup prompt")
	}
	now := time.Now().UTC()
	prev := task.Status
	task.Status = entity.TaskStatusInProgress
	task.UpdatedAt = now
	entity.ApplyStatusTimestamps(task, prev, now)
	if err := s.ts.UpdateTask(project, agent, task); err != nil {
		return controldb.RuntimeRun{}, nil, err
	}
	run, err := s.enqueueRuntimeTaskRun(workspaceID, project, agent, task, strings.TrimSpace(hb.SessionID), serverURL, actor)
	if err != nil {
		return controldb.RuntimeRun{}, nil, err
	}
	s.markAttentionSignalsSeen(workspaceID, attentionIDs)
	hb.LastWakeup = &now
	hb.LastWakeupStatus = "running"
	hb.PID = 0
	return run, task, nil
}

func (s *Server) nextRuntimeWakeupTask(workspaceID, project, agent string, hb *entity.HeartbeatConfig) (*entity.Task, []string, error) {
	if due, err := s.nextRuntimeDueScheduledTask(project, agent); err != nil {
		return nil, nil, err
	} else if due != nil {
		return due, nil, nil
	}
	if urgent, err := s.nextRuntimeUrgentPendingTask(project, agent); err != nil {
		return nil, nil, err
	} else if urgent != nil {
		return urgent, nil, nil
	}
	if task, ids, err := s.ensurePendingAttentionWakeupTask(workspaceID, project, agent); err != nil {
		return nil, nil, err
	} else if task != nil {
		return task, ids, nil
	}
	selected, err := s.nextRuntimePendingTask(project, agent)
	if err != nil {
		return nil, nil, err
	}
	if selected != nil {
		return selected, nil, nil
	}
	prompt, err := s.resolveRuntimeWakeupPrompt(project, agent, hb)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, nil
	}
	var attentionIDs []string
	section, ids, vars, secErr := s.pendingAttentionWakeupSectionAndVars(workspaceID, project, agent)
	if secErr == nil && strings.TrimSpace(section) != "" {
		prompt = section + prompt
		attentionIDs = ids
	}
	task := s.createRuntimeWakeupTask(project, agent, prompt)
	if len(vars) > 0 {
		task.Vars = mergeTaskVars(task.Vars, vars)
		if dir := strings.TrimSpace(vars["MULTIGENT_WAKEUP_WORKTREE_DIR"]); dir != "" {
			task.WorktreeDir = dir
		}
		if branch := strings.TrimSpace(vars["MULTIGENT_WAKEUP_BRANCH"]); branch != "" {
			task.BranchName = branch
		}
	}
	if err := s.ts.AddTask(project, agent, task); err != nil {
		return nil, nil, err
	}
	return task, attentionIDs, nil
}

// pendingWakeupTaskForDiagnosis returns, read-only, the task a withheld
// heartbeat wakeup would have dispatched (Task 0.4 gap-1 diagnostics). It
// mirrors nextRuntimeWakeupTask's selection order but never creates wakeup
// tasks; when only a synthetic wakeup prompt would run, it returns nil and
// the caller just logs.
func (s *Server) pendingWakeupTaskForDiagnosis(project, agent string) *entity.Task {
	if due, err := s.nextRuntimeDueScheduledTask(project, agent); err == nil && due != nil {
		return due
	}
	if urgent, err := s.nextRuntimeUrgentPendingTask(project, agent); err == nil && urgent != nil {
		return urgent
	}
	if selected, err := s.nextRuntimePendingTask(project, agent); err == nil {
		return selected
	}
	return nil
}

func (s *Server) nextRuntimeDueScheduledTask(project, agent string) (*entity.Task, error) {
	tasks, err := s.ts.ListTasks(project, agent, entity.TaskStatusPending)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var selected *entity.Task
	for _, task := range tasks {
		if task == nil || task.NotBefore == nil || task.NotBefore.After(now) {
			continue
		}
		if selected == nil ||
			task.NotBefore.Before(*selected.NotBefore) ||
			(task.NotBefore.Equal(*selected.NotBefore) && (task.Priority < selected.Priority ||
				(task.Priority == selected.Priority && task.CreatedAt.Before(selected.CreatedAt)))) {
			selected = task
		}
	}
	return selected, nil
}

func (s *Server) createRuntimeWakeupTask(project, agent, prompt string) *entity.Task {
	now := time.Now().UTC()
	return &entity.Task{
		ID:        "t-" + now.Format("20060102") + "-" + randomRuntimeHex(3),
		Title:     "[wakeup] routine",
		Status:    entity.TaskStatusPending,
		Type:      "wakeup",
		Priority:  9,
		Prompt:    prompt,
		CreatedBy: "heartbeat:wakeup",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (s *Server) nextRuntimeUrgentPendingTask(project, agent string) (*entity.Task, error) {
	tasks, err := s.ts.ListTasks(project, agent, entity.TaskStatusPending)
	if err != nil {
		return nil, err
	}
	var selected *entity.Task
	now := time.Now().UTC()
	for _, task := range tasks {
		if task == nil || task.Type == "wakeup" {
			continue
		}
		// Q0 D4: infra-blocked tasks only re-enter dispatch via unblock.
		if taskBlockedByInfraFailures(task) {
			continue
		}
		if !entity.TaskReady(task, now) {
			continue
		}
		if task.Priority > 0 {
			continue
		}
		if selected == nil ||
			task.Priority < selected.Priority ||
			(task.Priority == selected.Priority && task.CreatedAt.Before(selected.CreatedAt)) ||
			(task.Priority == selected.Priority && task.CreatedAt.Equal(selected.CreatedAt) && task.ID < selected.ID) {
			selected = task
		}
	}
	return selected, nil
}

func (s *Server) nextRuntimePendingTask(project, agent string) (*entity.Task, error) {
	tasks, err := s.ts.ListTasks(project, agent, entity.TaskStatusPending)
	if err != nil {
		return nil, err
	}
	var selected *entity.Task
	now := time.Now().UTC()
	for _, task := range tasks {
		if task == nil || task.Type == "wakeup" {
			continue
		}
		// Q0 D4: infra-blocked tasks only re-enter dispatch via unblock.
		if taskBlockedByInfraFailures(task) {
			continue
		}
		if !entity.TaskReady(task, now) {
			continue
		}
		if selected == nil ||
			task.Priority < selected.Priority ||
			(task.Priority == selected.Priority && task.CreatedAt.Before(selected.CreatedAt)) ||
			(task.Priority == selected.Priority && task.CreatedAt.Equal(selected.CreatedAt) && task.ID < selected.ID) {
			selected = task
		}
	}
	return selected, nil
}

func (s *Server) resolveRuntimeWakeupPrompt(project, agent string, hb *entity.HeartbeatConfig) (string, error) {
	raw := ""
	if hb != nil {
		raw = strings.TrimSpace(hb.WakeupPrompt)
	}
	if raw == "" {
		return "Execute your wakeup routine. Check pending tasks, unread messages, and your scheduled activities.", nil
	}
	if !strings.HasPrefix(raw, "@") {
		return raw, nil
	}
	rel := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
	if rel == "" {
		return "", nil
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.st.AgentDir(project, agent), rel)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (s *Server) handleSchedulerStop(w http.ResponseWriter, r *http.Request) {
	var body schedActionBody
	if r.ContentLength > 0 {
		if err := s.readJSON(w, r, &body); err != nil {
			s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
			return
		}
	}
	project := strings.TrimSpace(body.Project)
	agent := strings.TrimSpace(body.Agent)
	if agent != "" && project == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "agent requires project")
		return
	}
	if project == "" {
		if !s.checkCurrentWorkspaceAdmin(w, r) {
			return
		}
	} else if !s.checkProjectManager(w, r, project) {
		return
	}

	workspaceID, workspaceOK := s.currentWorkspaceForRequest(w, r)
	if !workspaceOK {
		return
	}
	key := s.schedulerProcessKeyForProjectAgent(workspaceID, project, agent, schedulerModeRuntimeNode)
	if err := s.sched.StopKey(key); err != nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeSchedulerNotFound, err.Error())
		return
	}
	s.setSchedulerDesiredKey(key, project, agent, "", false)

	s.clearSchedulerRuntimeFields(workspaceID, project, agent)
	s.auditLog(auditLogInput{
		Action:       "scheduler.stop",
		ResourceType: "scheduler",
		ResourceID:   schedKey(project, agent),
		Summary:      "Scheduler stopped",
		After: map[string]any{
			"project": project,
			"agent":   agent,
			"key":     schedKey(project, agent),
		},
		Request: r,
	})

	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) schedulerProcessKeyForProjectAgent(workspaceID, project, agent, mode string) string {
	if strings.TrimSpace(mode) == schedulerModeRuntimeNode && strings.TrimSpace(project) != "" && strings.TrimSpace(agent) != "" {
		target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
		if workerID := strings.TrimSpace(target.workerID); workerID != "" {
			return "worker/" + workerID
		}
	}
	return schedKey(project, agent)
}

func (s *Server) clearSchedulerRuntimeFields(workspaceID, project, agent string) {
	if project == "" {
		return
	}
	if strings.TrimSpace(agent) != "" {
		target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
		hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
		if err != nil || hb == nil {
			return
		}
		hb.NextWakeupAt = nil
		hb.SchedulerStartedAt = nil
		hb.PID = 0
		if hb.LastWakeupStatus == "running" {
			hb.LastWakeupStatus = "done"
		}
		_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
		return
	}
	targets := []runtimeSchedulerAgentTarget{s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)}
	if agent == "" {
		targets = s.runtimeSchedulerTargets(workspaceID, project, "")
	}
	for _, target := range targets {
		hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
		if err != nil || hb == nil {
			continue
		}
		hb.NextWakeupAt = nil
		hb.SchedulerStartedAt = nil
		hb.PID = 0
		if hb.LastWakeupStatus == "running" {
			hb.LastWakeupStatus = "done"
		}
		_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
	}
}

func (s *Server) runtimeSchedulerLoop(ctx context.Context, workspaceID, project, agent, serverURL, actor string) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	s.runtimeSchedulerTick(ctx, workspaceID, project, agent, serverURL, actor)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runtimeSchedulerTick(ctx, workspaceID, project, agent, serverURL, actor)
		}
	}
}

func (s *Server) runtimeSchedulerTick(ctx context.Context, workspaceID, project, agent, serverURL, actor string) {
	if ctx.Err() != nil {
		return
	}
	targets := s.runtimeSchedulerTargets(workspaceID, project, agent)
	now := time.Now()
	// Task 0.4: one shared Docker/image cache per tick — the readiness probes
	// below run per agent, but Docker availability and image presence are
	// host-global facts; probing them once per tick avoids a probe storm.
	dockerCache := &runtimeReadinessDockerCache{}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		hb, err := s.loadSchedulerTargetHeartbeat(workspaceID, target)
		if err != nil || hb == nil {
			continue
		}
		if !s.runtimeHeartbeatDue(workspaceID, target, hb, now) {
			continue
		}
		execTarget := s.selectRuntimeSchedulerExecutionTarget(target)
		meta, err := s.agentMetaForProjectMember(workspaceID, execTarget.project, execTarget.agent)
		if err != nil || meta == nil || meta.Model == entity.ModelHuman {
			continue
		}
		if !s.usesAssignedRuntimeNode(workspaceID, meta) {
			continue
		}
		if s.hasActiveRuntimeRunForTarget(workspaceID, target, "") {
			continue
		}
		// Task 0.4 gap 1: engine-internal readiness callback for heartbeat
		// wakeup dispatch. This path has no HTTP entry point, so probe before
		// enqueueing; on a blocking environment failure, record a structured
		// diagnosis on the pending wakeup task instead of burning tokens.
		if readiness := s.runtimeReadinessForExecutionCached(workspaceID, meta, dockerCache); readiness.Blocking {
			detail := runtimeReadinessErrorMessage(readiness)
			if task := s.pendingWakeupTaskForDiagnosis(execTarget.project, execTarget.agent); task != nil {
				s.addTaskSystemComment(execTarget.project, execTarget.agent, task,
					"[platform] 环境就绪检查未通过，wakeup 未派发（fail-fast，不烧 token）：",
					detail+"\n修复后等待下一次心跳即可自动继续。")
			}
			log.Printf("runtime scheduler wakeup %s/%s withheld: runtime not ready: %s", execTarget.project, execTarget.agent, detail)
			continue
		}
		run, task, err := s.enqueueRuntimeWakeupRunFromRequest(workspaceID, execTarget.project, execTarget.agent, hb, serverURL, actor)
		if err != nil {
			log.Printf("runtime scheduler enqueue %s/%s failed: %v", execTarget.project, execTarget.agent, err)
			continue
		}
		if hb2, err := s.loadSchedulerTargetHeartbeat(workspaceID, target); err == nil && hb2 != nil {
			if next := nextRuntimeHeartbeatAt(hb2, time.Now()); !next.IsZero() {
				nextUTC := next.UTC()
				hb2.NextWakeupAt = &nextUTC
			}
			_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb2)
		}
		log.Printf("runtime scheduler queued %s/%s task=%s run=%s", execTarget.project, execTarget.agent, task.ID, run.ID)
	}
}

type runtimeSchedulerAgentTarget struct {
	project      string
	agent        string
	workerID     string
	membershipID string
	memberships  []runtimeSchedulerAgentTarget
}

func (s *Server) runtimeSchedulerTargets(workspaceID, project, agent string) []runtimeSchedulerAgentTarget {
	project = strings.TrimSpace(project)
	agent = strings.TrimSpace(agent)
	if project != "" && agent != "" {
		return []runtimeSchedulerAgentTarget{s.runtimeSchedulerTargetGroupForProjectAgent(workspaceID, project, agent)}
	}
	projects := []string{}
	if project != "" {
		projects = []string{project}
	} else if rows, err := s.ts.ListProjects(); err == nil {
		projects = rows
	}
	out := []runtimeSchedulerAgentTarget{}
	seen := map[string]bool{}
	seenWorkers := map[string]bool{}
	for _, p := range projects {
		if s != nil && s.controlDB != nil && strings.TrimSpace(workspaceID) != "" {
			memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
				WorkspaceID: workspaceID,
				ProjectID:   p,
				MemberType:  "agent_worker",
			})
			if err == nil {
				for _, membership := range memberships {
					if !membership.AutoPickTasks {
						continue
					}
					name := strings.TrimSpace(membership.Title)
					if name == "" {
						worker, ok, err := s.agentDirectory.Worker(workspaceID, membership.MemberID)
						if err == nil && ok {
							name = worker.Name
						}
					}
					if name == "" || strings.HasPrefix(name, ".") {
						continue
					}
					if strings.TrimSpace(membership.MemberID) != "" {
						if seenWorkers[membership.MemberID] {
							continue
						}
						seenWorkers[membership.MemberID] = true
						out = append(out, s.runtimeSchedulerTargetGroupForWorker(workspaceID, membership.MemberID, p, name))
						continue
					}
					key := p + "/" + name
					if seen[key] {
						continue
					}
					seen[key] = true
					out = append(out, runtimeSchedulerAgentTarget{
						project:      p,
						agent:        name,
						workerID:     membership.MemberID,
						membershipID: membership.ID,
					})
				}
			}
		}
	}
	return out
}

func (s *Server) runtimeSchedulerTargetGroupForProjectAgent(workspaceID, project, agent string) runtimeSchedulerAgentTarget {
	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
	if strings.TrimSpace(target.workerID) == "" {
		target.memberships = []runtimeSchedulerAgentTarget{target}
		return target
	}
	group := s.runtimeSchedulerTargetGroupForWorker(workspaceID, target.workerID, target.project, target.agent)
	if len(group.memberships) == 0 {
		group.memberships = []runtimeSchedulerAgentTarget{target}
	}
	return group
}

func (s *Server) runtimeSchedulerTargetGroupForWorker(workspaceID, workerID, preferredProject, preferredAgent string) runtimeSchedulerAgentTarget {
	base := runtimeSchedulerAgentTarget{project: strings.TrimSpace(preferredProject), agent: strings.TrimSpace(preferredAgent), workerID: strings.TrimSpace(workerID)}
	if s == nil || s.controlDB == nil || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(workerID) == "" {
		base.memberships = []runtimeSchedulerAgentTarget{base}
		return base
	}
	memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
		WorkspaceID: workspaceID,
		MemberType:  "agent_worker",
		MemberID:    workerID,
	})
	if err != nil || len(memberships) == 0 {
		base.memberships = []runtimeSchedulerAgentTarget{base}
		return base
	}
	worker := controldb.AgentWorker{}
	if loaded, ok, err := s.agentDirectory.Worker(workspaceID, workerID); err == nil && ok {
		worker = loaded
	}
	group := make([]runtimeSchedulerAgentTarget, 0, len(memberships))
	for _, membership := range memberships {
		if !membership.AutoPickTasks {
			continue
		}
		name := schedulerMembershipRuntimeAgentName(membership, worker)
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		item := runtimeSchedulerAgentTarget{
			project:      strings.TrimSpace(membership.ProjectID),
			agent:        name,
			workerID:     workerID,
			membershipID: membership.ID,
		}
		group = append(group, item)
		if item.project == base.project && item.agent == base.agent {
			base.membershipID = item.membershipID
		}
	}
	if len(group) == 0 {
		base.memberships = []runtimeSchedulerAgentTarget{base}
		return base
	}
	base.memberships = group
	if strings.TrimSpace(base.project) == "" || strings.TrimSpace(base.agent) == "" {
		base.project = group[0].project
		base.agent = group[0].agent
		base.membershipID = group[0].membershipID
	}
	return base
}

func schedulerMembershipRuntimeAgentName(membership controldb.ProjectMembership, worker controldb.AgentWorker) string {
	for _, value := range []string{membership.Title, membership.Role, worker.Name, worker.DisplayName} {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	return ""
}

func (s *Server) runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent string) runtimeSchedulerAgentTarget {
	target := runtimeSchedulerAgentTarget{project: strings.TrimSpace(project), agent: strings.TrimSpace(agent)}
	if s == nil || s.agentDirectory == nil || strings.TrimSpace(workspaceID) == "" {
		return target
	}
	resolved, ok, err := s.agentDirectory.ResolveProjectMailbox(workspaceID, target.project+"/"+target.agent)
	if err == nil && ok {
		target.workerID = resolved.Worker.ID
		target.membershipID = resolved.Membership.ID
		if title := strings.TrimSpace(resolved.Membership.Title); title != "" {
			target.agent = title
		}
	}
	return target
}

func (s *Server) selectRuntimeSchedulerExecutionTarget(target runtimeSchedulerAgentTarget) runtimeSchedulerAgentTarget {
	memberships := target.memberships
	if len(memberships) == 0 {
		return target
	}
	best := memberships[0]
	var bestTask *entity.Task
	for _, membership := range memberships {
		task, err := s.nextRuntimePendingTask(membership.project, membership.agent)
		if err != nil || task == nil {
			continue
		}
		if bestTask == nil ||
			task.Priority < bestTask.Priority ||
			(task.Priority == bestTask.Priority && task.CreatedAt.Before(bestTask.CreatedAt)) ||
			(task.Priority == bestTask.Priority && task.CreatedAt.Equal(bestTask.CreatedAt) && task.ID < bestTask.ID) {
			best = membership
			bestTask = task
		}
	}
	if bestTask != nil {
		return best
	}
	if strings.TrimSpace(target.project) != "" && strings.TrimSpace(target.agent) != "" {
		return target
	}
	return best
}

func (s *Server) loadSchedulerTargetHeartbeat(workspaceID string, target runtimeSchedulerAgentTarget) (*entity.HeartbeatConfig, error) {
	if strings.TrimSpace(target.workerID) == "" {
		return nil, fmt.Errorf("agent worker membership not found for %s/%s", target.project, target.agent)
	}
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, target.workerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("agent worker %s not found", target.workerID)
	}
	hb := &entity.HeartbeatConfig{}
	if raw := strings.TrimSpace(worker.ScheduleJSON); raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), hb); err != nil {
			return nil, fmt.Errorf("parse agent worker schedule %s: %w", worker.ID, err)
		}
	}
	return hb, nil
}

func (s *Server) saveSchedulerTargetHeartbeat(workspaceID string, target runtimeSchedulerAgentTarget, hb *entity.HeartbeatConfig) error {
	if hb == nil {
		return nil
	}
	if strings.TrimSpace(target.workerID) == "" {
		return fmt.Errorf("agent worker membership not found for %s/%s", target.project, target.agent)
	}
	worker, ok, err := s.controlDB.AgentWorkerByID(workspaceID, target.workerID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("agent worker %s not found", target.workerID)
	}
	raw, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	worker.ScheduleJSON = string(raw)
	worker.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.controlDB.UpsertAgentWorker(worker)
}

func (s *Server) runtimeHeartbeatDue(workspaceID string, target runtimeSchedulerAgentTarget, hb *entity.HeartbeatConfig, now time.Time) bool {
	if hb == nil || !hb.Enabled || hb.Paused {
		return false
	}
	if hb.LastWakeupStatus == "running" {
		if s.hasActiveRuntimeRunForTarget(workspaceID, target, "") {
			return false
		}
		hb.LastWakeupStatus = "done"
		hb.PID = 0
		_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
	}
	if !runtimeActiveDay(hb.ActiveDays, now) {
		return false
	}
	if hb.ActiveHours != "" {
		ok, _ := runtimeActiveHourAt(hb.ActiveHours, now)
		if !ok {
			return false
		}
	}
	interval := runtimeHeartbeatInterval(hb)
	next := now
	if hb.LastWakeup != nil {
		next = hb.LastWakeup.Add(interval)
	}
	if next.After(now) {
		nextUTC := next.UTC()
		if hb.NextWakeupAt == nil || !hb.NextWakeupAt.Equal(nextUTC) {
			hb.NextWakeupAt = &nextUTC
			_ = s.saveSchedulerTargetHeartbeat(workspaceID, target, hb)
		}
		return false
	}
	return true
}

func runtimeHeartbeatInterval(hb *entity.HeartbeatConfig) time.Duration {
	if hb == nil || strings.TrimSpace(hb.Interval) == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(strings.TrimSpace(hb.Interval))
	if err != nil || d <= 0 {
		return 30 * time.Minute
	}
	return d
}

func nextRuntimeHeartbeatAt(hb *entity.HeartbeatConfig, now time.Time) time.Time {
	if hb == nil {
		return time.Time{}
	}
	base := now
	if hb.LastWakeup != nil {
		base = *hb.LastWakeup
	}
	return base.Add(runtimeHeartbeatInterval(hb))
}

func (s *Server) hasActiveRuntimeRun(workspaceID, project, agent, taskID string) bool {
	target := s.runtimeSchedulerTargetForProjectAgent(workspaceID, project, agent)
	return s.hasActiveRuntimeRunForTarget(workspaceID, target, taskID)
}

// dispatchTaskTriggerViaRuntime is the triggerManager's node-dispatch hook: a
// task trigger for an agent pinned to a runtime node must join the runtime
// dispatch queue, not run the local wakeup cycle (the console host has no
// agent CLI; the local cycle archived a node task done_failed mid-node-run in
// production). Returns true when the trigger was handled (node path or active
// run dedupe) so the caller skips the local wakeup; false falls back to local
// execution for locally-run agents. r may be nil (poller context).
func (s *Server) dispatchTaskTriggerViaRuntime(project, agent, reason string, r *http.Request) bool {
	if s == nil || s.controlDB == nil || s.ts == nil {
		return false
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil || strings.TrimSpace(workspaceID) == "" {
		return false
	}
	meta, err := s.agentMetaForProjectMember(workspaceID, project, agent)
	if err != nil || meta == nil || !s.usesAssignedRuntimeNode(workspaceID, meta) {
		return false
	}
	// The agent runs on a runtime node: route the trigger through the queue.
	// hasActiveRuntimeRun inside fireTaskTriggerOrQueueRuntime dedupes a
	// trigger racing an in-flight node run (queue-join semantics).
	tasks, listErr := s.ts.ListTasks(project, agent, entity.TaskStatusPending)
	if listErr != nil || len(tasks) == 0 {
		// No pending task row locally (it may live under a membership-title
		// alias); treat as handled so the local cycle never touches a
		// node-assigned agent's queue.
		return true
	}
	dispatchedAny := false
	for _, task := range tasks {
		if task == nil || task.Status.IsTerminal() {
			continue
		}
		if err := s.fireTaskTriggerOrQueueRuntime(workspaceID, project, agent, task, r, reason); err != nil {
			log.Printf("[trigger] runtime dispatch %s/%s task=%s failed: %v", project, agent, task.ID, err)
			continue
		}
		dispatchedAny = true
	}
	return dispatchedAny
}

func (s *Server) hasActiveRuntimeRunForTarget(workspaceID string, target runtimeSchedulerAgentTarget, taskID string) bool {
	if s == nil || s.controlDB == nil {
		return false
	}
	filter := controldb.RuntimeRunFilter{
		WorkspaceID: workspaceID,
		TaskID:      taskID,
		Limit:       50,
	}
	if strings.TrimSpace(target.workerID) != "" {
		filter.AgentWorkerID = target.workerID
	} else {
		filter.ProjectID = target.project
		filter.AgentID = target.agent
	}
	if strings.TrimSpace(filter.WorkspaceID) == "" {
		if ws, err := s.currentWorkspaceID(); err == nil {
			filter.WorkspaceID = ws
		}
	}
	if strings.TrimSpace(filter.WorkspaceID) == "" {
		return false
	}
	for _, status := range []string{"queued", "running"} {
		filter.Status = status
		runs, err := s.controlDB.ListRuntimeRuns(filter)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if runtimeRunBlocksAgent(run, time.Now().UTC()) {
				return true
			}
		}
	}
	return false
}

// runtimeRunBlocksAgent is declared in runtime_reaper.go (Q0 PR-2) and now
// delegates slot occupancy to runOccupiesWorkerSlot: queued runs still gate
// dispatch (already-dispatched means don't dispatch again), but only a
// running run with an unexpired lease and non-readonly slot_class occupies
// the Worker's execution slot.

func runtimeActiveHourAt(activeHours string, t time.Time) (bool, time.Duration) {
	parts := strings.SplitN(strings.TrimSpace(activeHours), "-", 2)
	if len(parts) != 2 {
		return true, 0
	}
	parse := func(s string) (int, bool) {
		v, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			return 0, false
		}
		return v.Hour()*60 + v.Minute(), true
	}
	start, ok1 := parse(parts[0])
	end, ok2 := parse(parts[1])
	if !ok1 || !ok2 || start == end {
		return true, 0
	}
	nowMin := t.Hour()*60 + t.Minute()
	if start < end {
		if nowMin >= start && nowMin < end {
			return true, 0
		}
		openAt := time.Date(t.Year(), t.Month(), t.Day(), start/60, start%60, 0, 0, t.Location())
		if openAt.Before(t) {
			openAt = openAt.Add(24 * time.Hour)
		}
		return false, openAt.Sub(t)
	}
	if nowMin >= start || nowMin < end {
		return true, 0
	}
	openAt := time.Date(t.Year(), t.Month(), t.Day(), start/60, start%60, 0, 0, t.Location())
	if openAt.Before(t) {
		openAt = openAt.Add(24 * time.Hour)
	}
	return false, openAt.Sub(t)
}

func runtimeActiveDay(activeDays string, now time.Time) bool {
	activeDays = strings.TrimSpace(activeDays)
	if activeDays == "" {
		return true
	}
	day := strings.ToLower(now.Weekday().String()[:3])
	for _, token := range strings.Split(activeDays, ",") {
		tok := strings.ToLower(strings.TrimSpace(token))
		switch tok {
		case "weekdays":
			if now.Weekday() >= time.Monday && now.Weekday() <= time.Friday {
				return true
			}
		case "weekends":
			if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
				return true
			}
		case day:
			return true
		}
	}
	return false
}

func (s *Server) handleSchedulerAbort(w http.ResponseWriter, r *http.Request) {
	var body schedActionBody
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON body")
		return
	}
	project := strings.TrimSpace(body.Project)
	agent := strings.TrimSpace(body.Agent)
	if project == "" || agent == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "project and agent are required")
		return
	}
	if !s.checkProjectManager(w, r, project) {
		return
	}
	currentWorkspaceID, workspaceOK := s.currentWorkspaceForRequest(w, r)
	if !workspaceOK {
		return
	}

	target := s.runtimeSchedulerTargetForProjectAgent(currentWorkspaceID, project, agent)
	hb, err := s.loadSchedulerTargetHeartbeat(currentWorkspaceID, target)
	if err != nil || hb == nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeHeartbeatNotFound, "heartbeat config not found")
		return
	}

	cancelledRemote, err := s.cancelRuntimeRunsForAgent(currentWorkspaceID, project, agent, "", "agent run aborted")
	if err != nil {
		s.serverError(w, err)
		return
	}

	if hb.PID <= 0 || hb.LastWakeupStatus != "running" {
		if cancelledRemote > 0 {
			hb.PID = 0
			hb.LastWakeupStatus = "aborted"
			_ = s.saveSchedulerTargetHeartbeat(currentWorkspaceID, target, hb)
			s.auditLog(auditLogInput{
				Action:       "scheduler.abort",
				ResourceType: "agent",
				ResourceID:   project + "/" + agent,
				Summary:      "Remote agent run cancelled",
				After: map[string]any{
					"project":              project,
					"agent":                agent,
					"runtimeRunsCancelled": cancelledRemote,
				},
				Request: r,
			})
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "runtimeRunsCancelled": cancelledRemote})
			return
		}
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeAgentNotRunning, "agent is not currently running")
		return
	}

	pid := hb.PID

	proc, err := os.FindProcess(pid)
	if err != nil {
		s.jsonErrorCode(w, http.StatusInternalServerError, ErrCodeProcessNotFound, fmt.Sprintf("cannot find process %d: %v", pid, err))
		return
	}

	// Signal 0 checks liveness.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		hb.PID = 0
		hb.LastWakeupStatus = "aborted"
		_ = s.saveSchedulerTargetHeartbeat(currentWorkspaceID, target, hb)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "msg": "process already dead, status updated"})
		return
	}

	// Kill the process group to ensure child processes (docker, claude) are also terminated.
	killProcessGroup(pid)

	// Give it a moment then force kill if needed.
	time.Sleep(500 * time.Millisecond)
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		_ = proc.Kill()
	}

	hb.PID = 0
	hb.LastWakeupStatus = "aborted"
	_ = s.saveSchedulerTargetHeartbeat(currentWorkspaceID, target, hb)
	s.auditLog(auditLogInput{
		Action:       "scheduler.abort",
		ResourceType: "agent",
		ResourceID:   project + "/" + agent,
		Summary:      "Agent run aborted",
		After: map[string]any{
			"project":              project,
			"agent":                agent,
			"pid":                  pid,
			"runtimeRunsCancelled": cancelledRemote,
		},
		Request: r,
	})

	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid, "runtimeRunsCancelled": cancelledRemote})
}
