package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	return e.startPreview(ctx, taskID, projectName, worktreeDir, false)
}

// StartSnapshotPreview starts a preview that is pinned to a completed task's
// detached worktree. The API can use this mode to keep completed artifacts
// viewable without allowing the preview assistant to mutate them.
func (e *Engine) StartSnapshotPreview(ctx context.Context, taskID, projectName, worktreeDir string) (*PreviewInstance, error) {
	return e.startPreview(ctx, taskID, projectName, worktreeDir, true)
}

func (e *Engine) startPreview(ctx context.Context, taskID, projectName, worktreeDir string, readOnly bool) (*PreviewInstance, error) {
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
		"-w", "/workspace",
		"-e", fmt.Sprintf("PORT=%d", port),
		"-e", "GOFLAGS=-buildvcs=false",
		"-e", "NPM_CONFIG_PREFIX=/opt/multigent/toolchains/npm",
		"-e", "PATH=/opt/multigent/toolchains/npm/bin:/usr/local/go/bin:/root/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
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
	dockerArgs = append(dockerArgs, "ghcr.io/multigent/multigent/runtime-base:latest")
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
		logs, _ := exec.Command("docker", "logs", "--tail", "120", containerName).CombinedOutput()
		instance.Status = "error"
		instance.Error = fmt.Sprintf("preview backend did not become ready; logs: %s", strings.TrimSpace(string(logs)))
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return instance, fmt.Errorf("preview backend did not become ready")
	}
	if !CheckHTTPReadyAt(port, healthPath, startupTimeout) {
		logs, _ := exec.Command("docker", "logs", "--tail", "120", containerName).CombinedOutput()
		instance.Status = "error"
		instance.Error = fmt.Sprintf("preview web server did not become ready; logs: %s", strings.TrimSpace(string(logs)))
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return instance, fmt.Errorf("preview web server did not become ready")
	}

	instance.Status = "running"
	return instance, nil
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
	idsOut, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=com.multigent.preview=true").Output()
	if err != nil {
		return fmt.Errorf("list preview containers: %w", err)
	}

	now := time.Now().UTC()
	for _, rawID := range strings.Split(string(idsOut), "\n") {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		format := `{{.State.Status}}\t{{json .Config.Labels}}`
		metaOut, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, id).Output()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(string(metaOut))
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		var labels map[string]string
		if err := json.Unmarshal([]byte(parts[1]), &labels); err != nil {
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(labels["com.multigent.preview.expires_at"]))
		if err != nil || !now.Before(expiresAt) || strings.TrimSpace(parts[0]) != "running" {
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
func (e *Engine) StopEphemeralPreview(taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	taskID = strings.TrimSpace(taskID)
	containerName := fmt.Sprintf("multigent-preview-%s", sanitizeContainerName(taskID))

	// Docker rm -f
	cmd := exec.Command("docker", "rm", "-f", containerName)
	_ = cmd.Run()

	delete(e.instances, taskID)
	return nil
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
