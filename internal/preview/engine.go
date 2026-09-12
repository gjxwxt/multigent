package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/multigent/multigent/internal/agentcli"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/sandbox"
)

// ProjectType defines the detected type of application in a workspace.
type ProjectType string

const (
	ProjectTypeFullstack ProjectType = "fullstack"
	ProjectTypeFrontend  ProjectType = "frontend"
	ProjectTypeBackend   ProjectType = "backend"
	ProjectTypeCLI       ProjectType = "cli"
)

// PreviewInstance represents an active preview server container for a task.
type PreviewInstance struct {
	TaskID      string      `json:"taskId"`
	Project     string      `json:"project"`
	Type        ProjectType `json:"type"`
	WorktreeDir string      `json:"worktreeDir"`
	Port        int         `json:"port"`
	ContainerID string      `json:"containerId"`
	URL         string      `json:"url"`
	Status      string      `json:"status"` // starting | running | stopped | error
	Error       string      `json:"error,omitempty"`
	StartedAt   time.Time   `json:"startedAt"`
	ExpiresAt   time.Time   `json:"expiresAt,omitempty"`
	ReadOnly    bool        `json:"readOnly,omitempty"`
}

const previewLease = 30 * time.Minute

// Engine manages ephemeral preview containers for tasks.
type Engine struct {
	mu        sync.RWMutex
	instances map[string]*PreviewInstance
}

// NewEngine creates a new preview engine and starts the background
// lease-reaper that removes expired preview containers while the service
// is running (Reconcile only runs once at startup).
func NewEngine() *Engine {
	e := &Engine{
		instances: make(map[string]*PreviewInstance),
	}
	go e.reapLoop()
	return e
}

func (e *Engine) reapLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		e.reapExpired()
		// Reconcile here too, not only at startup: it re-adopts labeled
		// containers the in-memory map lost (server restarts, orphaned
		// containers) and removes expired ones docker-side. This is what
		// keeps an orphaned container from living for days unseen.
		if err := e.Reconcile(context.Background()); err != nil {
			log.Printf("[preview-reconcile] %v", err)
		}
	}
}

// reapExpired force-removes running instances whose lease has elapsed.
func (e *Engine) reapExpired() {
	now := time.Now().UTC()
	e.mu.Lock()
	var expired []string
	for taskID, inst := range e.instances {
		if inst != nil && inst.Status == "running" && !inst.ExpiresAt.IsZero() && !now.Before(inst.ExpiresAt) {
			expired = append(expired, taskID)
		}
	}
	e.mu.Unlock()
	for _, taskID := range expired {
		_ = e.StopEphemeralPreview(taskID)
	}
}

// DetectProjectType analyzes files in worktreeDir to determine the application type.
func DetectProjectType(worktreeDir string) ProjectType {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return ProjectTypeCLI
	}

	hasFrontend := fileExists(filepath.Join(worktreeDir, "package.json")) ||
		fileExists(filepath.Join(worktreeDir, "web", "package.json")) ||
		fileExists(filepath.Join(worktreeDir, "frontend", "package.json")) ||
		fileExists(filepath.Join(worktreeDir, "client", "package.json")) ||
		fileExists(filepath.Join(worktreeDir, "app", "package.json"))

	hasBackend := fileExists(filepath.Join(worktreeDir, "go.mod")) ||
		fileExists(filepath.Join(worktreeDir, "server", "go.mod")) ||
		fileExists(filepath.Join(worktreeDir, "server", "main.go")) ||
		fileExists(filepath.Join(worktreeDir, "backend", "go.mod")) ||
		fileExists(filepath.Join(worktreeDir, "requirements.txt"))

	if hasFrontend && hasBackend {
		return ProjectTypeFullstack
	}
	if hasFrontend {
		return ProjectTypeFrontend
	}
	if hasBackend {
		return ProjectTypeBackend
	}
	return ProjectTypeCLI
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func backendCommandFor(worktreeDir string) string {
	if fileExists(filepath.Join(worktreeDir, "server", "main.go")) || fileExists(filepath.Join(worktreeDir, "server", "go.mod")) {
		return "(cd server && PORT=8080 go run -buildvcs=false .) & "
	}
	if fileExists(filepath.Join(worktreeDir, "main.go")) || fileExists(filepath.Join(worktreeDir, "go.mod")) {
		return "(PORT=8080 go run -buildvcs=false .) & "
	}
	if fileExists(filepath.Join(worktreeDir, "cmd", "api", "main.go")) {
		return "(PORT=8080 go run -buildvcs=false ./cmd/api/main.go) & "
	}
	if fileExists(filepath.Join(worktreeDir, "server", "index.js")) {
		return "(PORT=8080 node server/index.js) & "
	}
	if fileExists(filepath.Join(worktreeDir, "main.py")) {
		return "(PORT=8080 python3 main.py) & "
	}
	return ""
}

// GetInstance returns an existing preview instance for a task.
func (e *Engine) GetInstance(taskID string) (*PreviewInstance, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	inst, ok := e.instances[taskID]
	return inst, ok
}

// StartEphemeralPreview launches a preview container for the given task worktree.
func (e *Engine) StartEphemeralPreview(ctx context.Context, taskID, projectName, worktreeDir string) (*PreviewInstance, error) {
	return e.StartEphemeralPreviewWithRuntime(ctx, taskID, projectName, worktreeDir, RuntimeSelection{})
}

// StartSnapshotPreview starts a preview that is pinned to a completed task's
// detached worktree. The API can use this mode to keep completed artifacts
// viewable without allowing the preview assistant to mutate them.
func (e *Engine) StartSnapshotPreview(ctx context.Context, taskID, projectName, worktreeDir string) (*PreviewInstance, error) {
	return e.StartSnapshotPreviewWithRuntime(ctx, taskID, projectName, worktreeDir, RuntimeSelection{})
}

// RuntimeSelection carries the per-project resolved runtime for a preview
// container (profile + image reference), resolved by the API layer through
// sandbox.ResolveRuntime so previews and agent sandboxes of the same project
// always agree.
type RuntimeSelection = sandbox.RuntimeSelection

// StartEphemeralPreviewWithRuntime is StartEphemeralPreview with the
// project-resolved runtime. A zero RuntimeSelection falls back to the managed
// default base image (used by tests and callers without project context).
func (e *Engine) StartEphemeralPreviewWithRuntime(ctx context.Context, taskID, projectName, worktreeDir string, runtime RuntimeSelection) (*PreviewInstance, error) {
	return e.startPreview(ctx, taskID, projectName, worktreeDir, false, runtime)
}

// StartSnapshotPreviewWithRuntime is StartSnapshotPreview with the
// project-resolved runtime.
func (e *Engine) StartSnapshotPreviewWithRuntime(ctx context.Context, taskID, projectName, worktreeDir string, runtime RuntimeSelection) (*PreviewInstance, error) {
	return e.startPreview(ctx, taskID, projectName, worktreeDir, true, runtime)
}

func (e *Engine) startPreview(ctx context.Context, taskID, projectName, worktreeDir string, readOnly bool, runtime RuntimeSelection) (*PreviewInstance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	taskID = strings.TrimSpace(taskID)
	if inst, ok := e.instances[taskID]; ok && inst.Status == "running" {
		if !readOnly || inst.ReadOnly {
			return inst, nil
		}
		// A task can transition to completed while its writable preview is
		// still alive. Replace that container before exposing a read-only
		// completion snapshot.
		_ = exec.Command("docker", "rm", "-f", inst.ContainerID).Run()
		delete(e.instances, taskID)
	}

	projType := DetectProjectType(worktreeDir)
	runtimeSpec, err := LoadRuntimeSpec(worktreeDir)
	if err != nil {
		return nil, err
	}
	if projType == ProjectTypeCLI {
		return &PreviewInstance{
			TaskID:      taskID,
			Project:     projectName,
			Type:        projType,
			WorktreeDir: worktreeDir,
			Status:      "not_applicable",
			URL:         "",
			StartedAt:   time.Now().UTC(),
		}, nil
	}

	port, err := findFreePort()
	if err != nil {
		return nil, fmt.Errorf("find free port for preview: %w", err)
	}

	containerName := fmt.Sprintf("multigent-preview-%s", sanitizeContainerName(taskID))
	// Remove any existing container with the same name
	_ = exec.Command("docker", "rm", "-f", containerName).Run()

	instance := &PreviewInstance{
		TaskID:      taskID,
		Project:     projectName,
		Type:        projType,
		WorktreeDir: worktreeDir,
		Port:        port,
		ContainerID: containerName,
		URL:         fmt.Sprintf("/preview/%s/", taskID),
		Status:      "starting",
		StartedAt:   time.Now().UTC(),
		ReadOnly:    readOnly,
	}
	instance.ExpiresAt = instance.StartedAt.Add(previewLease)
	e.instances[taskID] = instance

	// Determine container startup command based on project layout. Keep startup
	// errors visible: a healthy Vite process must not hide a dead API process.
	setupEnv := "git config --global --add safe.directory '*' || true; export GOFLAGS='-buildvcs=false'; "

	var runCmd []string
	healthPath := "/"
	startupTimeout := 20 * time.Second
	backendHostPort := 0
	backendContainerPort := 8080
	backendHealthPath := ""
	if runtimeSpec != nil {
		command, contractHealthPath, contractTimeout, contractErr := runtimeSpec.StartupCommand(projType, port)
		if contractErr != nil {
			instance.Status = "error"
			instance.Error = contractErr.Error()
			return instance, contractErr
		}
		healthPath = contractHealthPath
		if runtimeSpec.Backend != nil {
			backendContainerPort = runtimeSpec.BackendPort()
			backendHostPort, err = findFreePort()
			if err != nil {
				return nil, fmt.Errorf("find free port for preview backend: %w", err)
			}
			backendHealthPath = runtimeSpec.Backend.HealthPath
		}
		if contractTimeout > 0 {
			startupTimeout = time.Duration(contractTimeout) * time.Second
		}
		if installCmd := frontendInstallCommand(worktreeDir, runtimeSpec.Frontend); installCmd != "" {
			// A cold worktree has no node_modules (gitignored), so the dev
			// server would die instantly and take the whole container down.
			// The install must complete BEFORE any service starts: the service
			// chain backgrounded with "&" would otherwise race the install
			// ("install && A & B" backgrounds {install && A} and runs B
			// immediately, so vite can be missing when the dev server starts).
			// Braces keep install in the foreground of the whole chain.
			command = "{ " + strings.TrimSuffix(strings.TrimSuffix(installCmd, " "), "&&") + " ; } && { " + command + " ; }"
			if startupTimeout < 300*time.Second {
				startupTimeout = 300 * time.Second
			}
		}
		runCmd = []string{"sh", "-c", setupEnv + command}
	} else {
		switch projType {
		case ProjectTypeFullstack:
			if fileExists(filepath.Join(worktreeDir, "web", "package.json")) {
				backendCmd := backendCommandFor(worktreeDir)
				runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("%s(cd web && npx vite --port %d --host 0.0.0.0 || npx vite preview --port %d --host 0.0.0.0)", backendCmd, port, port)}
			} else if fileExists(filepath.Join(worktreeDir, "frontend", "package.json")) {
				backendCmd := backendCommandFor(worktreeDir)
				runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("%s(cd frontend && npx vite --port %d --host 0.0.0.0 || npx vite preview --port %d --host 0.0.0.0)", backendCmd, port, port)}
			} else if fileExists(filepath.Join(worktreeDir, "package.json")) {
				runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("npm run seed || true; PORT=%d npm start || PORT=%d npm run dev -- --port %d --host 0.0.0.0 || PORT=%d node server/index.js", port, port, port, port)}
			} else if fileExists(filepath.Join(worktreeDir, "main.go")) || fileExists(filepath.Join(worktreeDir, "go.mod")) {
				runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("(cd frontend && npm run build || true); PORT=%d ./bin/server || PORT=%d go run -buildvcs=false . || PORT=%d go run -buildvcs=false ./cmd/... || PORT=%d go run -buildvcs=false ./...", port, port, port, port)}
			} else {
				runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("PORT=%d ./bin/server || PORT=%d go run -buildvcs=false ./... || PORT=%d go run -buildvcs=false .", port, port, port)}
			}
		case ProjectTypeFrontend:
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("npm run seed 2>/dev/null || true; PORT=%d npm start || PORT=%d node server/index.js || npx vite --port %d --host 0.0.0.0 || npx vite preview --port %d --host 0.0.0.0", port, port, port, port)}
		case ProjectTypeBackend:
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("PORT=%d ./bin/server || PORT=%d npm start || PORT=%d go run -buildvcs=false . || PORT=%d go run -buildvcs=false ./cmd/... || PORT=%d go run -buildvcs=false ./... || python3 main.py --port %d", port, port, port, port, port, port)}
		default:
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("PORT=%d ./bin/server || PORT=%d go run -buildvcs=false . || PORT=%d go run -buildvcs=false ./...", port, port, port)}
		}
	}
	if backendHostPort == 0 && projType == ProjectTypeFullstack && backendCommandFor(worktreeDir) != "" {
		backendHostPort, err = findFreePort()
		if err != nil {
			return nil, fmt.Errorf("find free port for preview backend: %w", err)
		}
	}

	dockerArgs := []string{
		"run", "-d",
		"--name", containerName,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"--label", "com.multigent.preview=true",
		"--label", "com.multigent.preview.project=" + projectName,
		"--label", "com.multigent.preview.task=" + taskID,
		"--label", "com.multigent.preview.type=" + string(projType),
		"--label", "com.multigent.preview.worktree=" + worktreeDir,
		"--label", "com.multigent.preview.port=" + strconv.Itoa(port),
		"--label", "com.multigent.preview.started_at=" + instance.StartedAt.Format(time.RFC3339),
		"--label", "com.multigent.preview.expires_at=" + instance.ExpiresAt.Format(time.RFC3339),
		"--label", "com.multigent.preview.read_only=" + strconv.FormatBool(readOnly),
		"-v", previewWorktreeMount(worktreeDir, readOnly),
		"-v", "multigent-toolchains:/opt/multigent/toolchains",
		"-v", "multigent-npm-cache:/root/.npm",
		"-v", "multigent-go-cache:/root/go/pkg/mod",
		"-v", "multigent-go-build-cache:/root/.cache/go-build",
		// Note: a shared gradle cache volume is intentionally NOT mounted here
		// yet (experimental P2): the agent side runs as the host user while
		// previews run as root, so one shared volume would fight over
		// ownership. See docs/intranet-runtime-plan.md.
		"-w", "/workspace",
		"-e", fmt.Sprintf("PORT=%d", port),
		"-e", "GOFLAGS=-buildvcs=false",
		"-e", "NPM_CONFIG_PREFIX=/opt/multigent/toolchains/npm",
	}
	dockerArgs = append(dockerArgs, e.profilePreviewEnv(runtime)...)
	dockerArgs = append(dockerArgs, sandbox.TransportDockerArgs()...)
	// Linked worktrees record their parent gitdir as an absolute host path;
	// mount the parent repo at the same path so git inside the preview
	// container can resolve it (otherwise every git command fails with
	// "not a git repository" — see sandbox.WorktreeParentMount).
	if parentMount := sandbox.WorktreeParentMount(worktreeDir, readOnly); parentMount != "" {
		dockerArgs = append(dockerArgs, "-v", parentMount)
	}
	if backendHostPort > 0 {
		dockerArgs = append(dockerArgs, "-p", fmt.Sprintf("127.0.0.1:%d:%d", backendHostPort, backendContainerPort))
	}
	dockerArgs = append(dockerArgs, e.previewImage(runtime))
	dockerArgs = append(dockerArgs, runCmd...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		instance.Status = "error"
		instance.Error = fmt.Sprintf("docker run failed: %v, output: %s", err, string(out))
		return instance, fmt.Errorf("start preview container: %w (%s)", err, string(out))
	}

	backendReady := true
	if backendHostPort > 0 {
		if strings.TrimSpace(backendHealthPath) == "" || backendHealthPath == "/" {
			backendReady = CheckTCPReady(backendHostPort, startupTimeout)
		} else {
			backendReady = CheckHTTPReadyAt(backendHostPort, backendHealthPath, startupTimeout)
		}
	}
	if backendHostPort > 0 && !backendReady {
		return instance, failInstance(instance, containerName, "preview backend did not become ready")
	}
	if !CheckHTTPReadyAt(port, healthPath, startupTimeout) {
		return instance, failInstance(instance, containerName, "preview web server did not become ready")
	}

	instance.Status = "running"
	return instance, nil
}

// frontendInstallCommand returns a shell prefix that installs frontend
// dependencies when the contract's frontend directory has a package.json but
// no node_modules. Worktrees are materialized from git, so the gitignored
// node_modules never exists until something installs it; without this the dev
// server exits instantly ("vite: not found") and, because it runs as the
// container's foreground process, takes the whole preview down with it.
func frontendInstallCommand(worktreeDir string, frontend *RuntimeServiceSpec) string {
	if frontend == nil {
		return ""
	}
	dir := strings.TrimSpace(frontend.Directory)
	if dir == "" {
		dir = "."
	}
	base := filepath.Join(worktreeDir, dir)
	if !fileExists(filepath.Join(base, "package.json")) {
		return ""
	}
	if info, err := os.Stat(filepath.Join(base, "node_modules")); err == nil && info.IsDir() {
		return ""
	}
	return "(cd " + shellQuote(dir) + " && npm install --no-audit --no-fund) && "
}

// failInstance records a startup failure with the container's exit state and
// tail logs (collected before the container is removed, since the logs die
// with it) and returns the error to surface to the caller. An early container
// exit is reported as such: a dead frontend process otherwise masquerades as
// "backend did not become ready".
func failInstance(instance *PreviewInstance, containerName, reason string) error {
	logs, _ := exec.Command("docker", "logs", "--tail", "120", containerName).CombinedOutput()
	exit := ""
	if out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}} exitcode={{.State.ExitCode}}", containerName).Output(); err == nil {
		if s := strings.TrimSpace(string(out)); strings.HasPrefix(s, "exited") {
			exit = s
		}
	}
	if exit != "" {
		reason = "preview container exited during startup (" + exit + ")"
	}
	instance.Status = "error"
	instance.Error = fmt.Sprintf("%s; logs: %s", reason, strings.TrimSpace(string(logs)))
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	return errors.New(reason)
}

func previewWorktreeMount(worktreeDir string, readOnly bool) string {
	mode := ""
	if readOnly {
		mode = ":ro"
	}
	return fmt.Sprintf("%s:/workspace%s", worktreeDir, mode)
}

// Reconcile removes expired preview containers and restores live instances
// after a service restart. Containers are only managed when they carry the
// Multigent preview label; unrelated Docker workloads are untouched.
func (e *Engine) Reconcile(ctx context.Context) error {
	// List container IDs with the preview label
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=com.multigent.preview=true").Output()
	if err != nil {
		return fmt.Errorf("list preview containers: %w", err)
	}
	containerIDs := strings.Fields(string(out))
	if len(containerIDs) == 0 {
		return nil
	}

	// Batch inspect container state and labels in a single invocation
	format := `{{.Id}}\t{{.State.Status}}\t{{json .Config.Labels}}`
	inspectArgs := append([]string{"inspect", "--format", format}, containerIDs...)
	inspectOut, err := exec.CommandContext(ctx, "docker", inspectArgs...).Output()
	if err != nil {
		return fmt.Errorf("inspect preview containers: %w", err)
	}

	now := time.Now().UTC()
	for _, line := range strings.Split(string(inspectOut), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		id, state := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		var labels map[string]string
		if err := json.Unmarshal([]byte(parts[2]), &labels); err != nil {
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(labels["com.multigent.preview.expires_at"]))
		if err != nil || !now.Before(expiresAt) || state != "running" {
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", id).Run()
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(labels["com.multigent.preview.port"]))
		if err != nil || port <= 0 {
			continue
		}
		taskID := strings.TrimSpace(labels["com.multigent.preview.task"])
		if taskID == "" {
			continue
		}
		startedAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(labels["com.multigent.preview.started_at"]))
		if startedAt.IsZero() {
			startedAt = now
		}
		projType := ProjectType(strings.TrimSpace(labels["com.multigent.preview.type"]))
		if projType == "" {
			projType = ProjectTypeCLI
		}
		instance := &PreviewInstance{
			TaskID:      taskID,
			Project:     strings.TrimSpace(labels["com.multigent.preview.project"]),
			Type:        projType,
			WorktreeDir: strings.TrimSpace(labels["com.multigent.preview.worktree"]),
			Port:        port,
			ContainerID: id,
			URL:         fmt.Sprintf("/preview/%s/", taskID),
			Status:      "running",
			StartedAt:   startedAt,
			ExpiresAt:   expiresAt,
			ReadOnly:    strings.EqualFold(strings.TrimSpace(labels["com.multigent.preview.read_only"]), "true"),
		}
		e.mu.Lock()
		e.instances[taskID] = instance
		e.mu.Unlock()
	}
	return nil
}

// StopEphemeralPreview stops and removes the preview container for a task.
// It is idempotent: a missing container counts as success. A container that
// survives rm (after one retry) is a hard error — callers must not proceed
// with worktree cleanup while a bind-mounted preview container still exists.
// The ground-truth "gone" check uses the task label, so name-based and
// ID-based removal paths agree on what "removed" means.
func (e *Engine) StopEphemeralPreview(taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	taskID = strings.TrimSpace(taskID)
	containerName := fmt.Sprintf("multigent-preview-%s", sanitizeContainerName(taskID))

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		cmd := exec.Command("docker", "rm", "-f", containerName)
		out, err := cmd.CombinedOutput()
		if err == nil {
			lastErr = nil
			break
		}
		if strings.Contains(strings.ToLower(string(out)), "no such container") {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("docker rm %s: %w (%s)", containerName, err, strings.TrimSpace(string(out)))
	}
	if lastErr == nil {
		delete(e.instances, taskID)
		return nil
	}
	// Distinguish "rm failed but container is actually gone" from a real
	// failure so retries cannot strand phantom instances in the map.
	if exists, checkErr := containerExistsByTaskLabel(taskID); checkErr == nil && !exists {
		delete(e.instances, taskID)
		return nil
	} else if checkErr != nil {
		return fmt.Errorf("stopping preview %s: %v (container existence check failed: %w)", containerName, lastErr, checkErr)
	}
	delete(e.instances, taskID) // container exists but rm failed; keep map in sync with reality
	return lastErr
}

// containerExistsByTaskLabel reports whether any container carries the
// preview task label, regardless of its lifecycle state.
func containerExistsByTaskLabel(taskID string) (bool, error) {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.multigent.preview.task="+taskID).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func sanitizeContainerName(s string) string {
	s = strings.TrimSpace(s)
	r := strings.NewReplacer("/", "-", ":", "-", "_", "-", " ", "-")
	return r.Replace(s)
}

func findFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// CheckHTTPReady checks whether the preview server is responding on its port.
func CheckHTTPReady(port int, timeout time.Duration) bool {
	return CheckHTTPReadyAt(port, "/", timeout)
}

// CheckHTTPReadyAt checks whether a preview server responds successfully on
// the declared health endpoint. HTTP 4xx/5xx responses are not readiness.
func CheckHTTPReadyAt(port int, path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	path = normalizeHealthPath(path)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func CheckTCPReady(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// SeedInstanceForTest installs a pre-built instance without launching any
// container. Test-only helper.
func (e *Engine) SeedInstanceForTest(inst *PreviewInstance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.instances[inst.TaskID] = inst
}

// previewImage resolves the preview container image: the per-project runtime
// selection when present, otherwise the managed default base image.
func (e *Engine) previewImage(runtime RuntimeSelection) string {
	if image := strings.TrimSpace(runtime.ImageRef); image != "" {
		return image
	}
	return sandbox.DefaultBaseImage()
}

// profilePreviewEnv returns profile-dependent "-e KEY=VALUE" docker args for
// the preview container. Only variables the image itself cannot supply are set
// here. For base images PATH is NOT overridden — the image ENV PATH is already
// correct, and a docker -e PATH would clobber profile-specific image PATHs.
// The jvm21 profile (declared by the project, carried in the runtime selection
// — never inferred from the image name) prepends the npm toolchain bin so
// user-installed CLIs keep precedence, keeping parity with the historical
// layout. Every entry MUST carry its own "-e" flag: docker treats the first
// bare KEY=VALUE argument as the image reference (invalid reference format).
func (e *Engine) profilePreviewEnv(runtime RuntimeSelection) []string {
	env := []string{"-e", "MULTIGENT_TOOLCHAIN_HOME=" + agentcli.ToolchainHome}
	if runtime.Profile == sandbox.ProfileJVM21 {
		env = append(env,
			"-e", "JAVA_HOME=/opt/multigent/jdk",
			"-e", "PATH=/opt/multigent/toolchains/npm/bin:/opt/multigent/jdk/bin:/usr/local/go/bin:/root/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		)
		// Gradle/JVM ignore HTTPS_PROXY env; without explicit -D system
		// properties the wrapper download and dependency resolution hang in
		// proxied environments. Derived from the typed network config at
		// container-create time — nothing environment-specific is committed.
		env = append(env, sandbox.ProfileDockerArgs(&entity.DockerSandboxConfig{Profile: runtime.Profile})...)
	}
	return env
}
