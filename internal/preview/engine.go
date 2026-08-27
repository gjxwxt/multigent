package preview

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
}

// Engine manages ephemeral preview containers for tasks.
type Engine struct {
	mu        sync.RWMutex
	instances map[string]*PreviewInstance
}

// NewEngine creates a new preview engine.
func NewEngine() *Engine {
	return &Engine{
		instances: make(map[string]*PreviewInstance),
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

// GetInstance returns an existing preview instance for a task.
func (e *Engine) GetInstance(taskID string) (*PreviewInstance, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	inst, ok := e.instances[taskID]
	return inst, ok
}

// StartEphemeralPreview launches a preview container for the given task worktree.
func (e *Engine) StartEphemeralPreview(ctx context.Context, taskID, projectName, worktreeDir string) (*PreviewInstance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	taskID = strings.TrimSpace(taskID)
	if inst, ok := e.instances[taskID]; ok && inst.Status == "running" {
		return inst, nil
	}

	projType := DetectProjectType(worktreeDir)
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
	}
	e.instances[taskID] = instance

	// Determine container startup command based on project layout
	setupEnv := "git config --global --add safe.directory '*' 2>/dev/null || true; export GOFLAGS='-buildvcs=false'; "

	var runCmd []string
	switch projType {
	case ProjectTypeFullstack:
		if fileExists(filepath.Join(worktreeDir, "web", "package.json")) {
			backendCmd := ""
			if fileExists(filepath.Join(worktreeDir, "server", "main.go")) || fileExists(filepath.Join(worktreeDir, "server", "go.mod")) {
				backendCmd = "PORT=8080 go run -buildvcs=false ./server/... 2>/dev/null & "
			} else if fileExists(filepath.Join(worktreeDir, "main.go")) || fileExists(filepath.Join(worktreeDir, "go.mod")) {
				backendCmd = "PORT=8080 go run -buildvcs=false . 2>/dev/null & "
			}
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("%s(cd web && npx vite --port %d --host 0.0.0.0 || npx vite preview --port %d --host 0.0.0.0)", backendCmd, port, port)}
		} else if fileExists(filepath.Join(worktreeDir, "frontend", "package.json")) {
			backendCmd := ""
			if fileExists(filepath.Join(worktreeDir, "main.go")) || fileExists(filepath.Join(worktreeDir, "go.mod")) {
				backendCmd = "PORT=8080 go run -buildvcs=false . 2>/dev/null & "
			} else if fileExists(filepath.Join(worktreeDir, "cmd", "api", "main.go")) {
				backendCmd = "PORT=8080 go run -buildvcs=false ./cmd/api/main.go 2>/dev/null & "
			} else if fileExists(filepath.Join(worktreeDir, "server", "index.js")) {
				backendCmd = "PORT=8080 node server/index.js 2>/dev/null & "
			} else if fileExists(filepath.Join(worktreeDir, "main.py")) {
				backendCmd = "PORT=8080 python3 main.py 2>/dev/null & "
			}
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("%s(cd frontend && npx vite --port %d --host 0.0.0.0 || npx vite preview --port %d --host 0.0.0.0)", backendCmd, port, port)}
		} else if fileExists(filepath.Join(worktreeDir, "package.json")) {
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("npm run seed 2>/dev/null || true; PORT=%d npm start || PORT=%d npm run dev -- --port %d --host 0.0.0.0 || PORT=%d node server/index.js", port, port, port, port)}
		} else if fileExists(filepath.Join(worktreeDir, "main.go")) || fileExists(filepath.Join(worktreeDir, "go.mod")) {
			runCmd = []string{"sh", "-c", setupEnv + fmt.Sprintf("(cd frontend && npm run build 2>/dev/null || true); PORT=%d ./bin/server || PORT=%d go run -buildvcs=false . || PORT=%d go run -buildvcs=false ./cmd/... || PORT=%d go run -buildvcs=false ./...", port, port, port, port)}
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

	dockerArgs := []string{
		"run", "-d",
		"--name", containerName,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-v", fmt.Sprintf("%s:/workspace", worktreeDir),
		"-v", "multigent-toolchains:/opt/multigent/toolchains",
		"-v", "multigent-npm-cache:/root/.npm",
		"-v", "multigent-go-cache:/root/go/pkg/mod",
		"-v", "multigent-go-build-cache:/root/.cache/go-build",
		"-w", "/workspace",
		"-e", fmt.Sprintf("PORT=%d", port),
		"-e", "GOFLAGS=-buildvcs=false",
		"-e", "NPM_CONFIG_PREFIX=/opt/multigent/toolchains/npm",
		"-e", "PATH=/opt/multigent/toolchains/npm/bin:/usr/local/go/bin:/root/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"ghcr.io/multigent/multigent/runtime-base:latest",
	}
	dockerArgs = append(dockerArgs, runCmd...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		instance.Status = "error"
		instance.Error = fmt.Sprintf("docker run failed: %v, output: %s", err, string(out))
		return instance, fmt.Errorf("start preview container: %w (%s)", err, string(out))
	}

	instance.Status = "running"
	return instance, nil
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
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
