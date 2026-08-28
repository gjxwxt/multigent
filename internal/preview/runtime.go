package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const runtimeContractPath = ".multigent/runtime.json"

// RuntimeSpec is the deterministic startup contract produced by project
// initialization. It is intentionally small: the project owns commands, but
// Multigent owns the container, port, readiness and lifecycle around them.
type RuntimeSpec struct {
	Version  int                 `json:"version"`
	Frontend *RuntimeServiceSpec `json:"frontend,omitempty"`
	Backend  *RuntimeServiceSpec `json:"backend,omitempty"`
	Preview  RuntimePreviewSpec  `json:"preview,omitempty"`
}

type RuntimeServiceSpec struct {
	Directory  string `json:"directory,omitempty"`
	Command    string `json:"command"`
	Port       int    `json:"port,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
}

type RuntimePreviewSpec struct {
	HealthPath            string `json:"healthPath,omitempty"`
	StartupTimeoutSeconds int    `json:"startupTimeoutSeconds,omitempty"`
}

// LoadRuntimeSpec loads the optional project-owned contract. An existing but
// malformed contract fails closed so a typo cannot silently select a risky
// heuristic startup fallback.
func LoadRuntimeSpec(worktreeDir string) (*RuntimeSpec, error) {
	root := filepath.Clean(strings.TrimSpace(worktreeDir))
	var path string
	var raw []byte
	var err error
	for depth, candidateRoot := 0, root; depth < 4; depth, candidateRoot = depth+1, filepath.Dir(candidateRoot) {
		candidate := filepath.Join(candidateRoot, runtimeContractPath)
		raw, err = os.ReadFile(candidate)
		if err == nil {
			path = candidate
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", candidate, err)
		}
		if filepath.Dir(candidateRoot) == candidateRoot {
			break
		}
	}
	if path == "" {
		return nil, nil
	}
	var spec RuntimeSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", runtimeContractPath, err)
	}
	if spec.Version != 1 {
		return nil, fmt.Errorf("%s version must be 1, got %d", runtimeContractPath, spec.Version)
	}
	if err := validateRuntimeService("frontend", spec.Frontend); err != nil {
		return nil, err
	}
	if err := validateRuntimeService("backend", spec.Backend); err != nil {
		return nil, err
	}
	if spec.Frontend == nil && spec.Backend == nil {
		return nil, fmt.Errorf("%s must declare frontend or backend", runtimeContractPath)
	}
	if spec.Preview.StartupTimeoutSeconds < 0 || spec.Preview.StartupTimeoutSeconds > 300 {
		return nil, fmt.Errorf("%s preview.startupTimeoutSeconds must be between 0 and 300", runtimeContractPath)
	}
	spec.Preview.HealthPath = normalizeHealthPath(spec.Preview.HealthPath)
	return &spec, nil
}

func validateRuntimeService(name string, service *RuntimeServiceSpec) error {
	if service == nil {
		return nil
	}
	if strings.TrimSpace(service.Command) == "" {
		return fmt.Errorf("%s.%s.command is required", runtimeContractPath, name)
	}
	dir := strings.TrimSpace(service.Directory)
	if dir != "" {
		clean := filepath.Clean(dir)
		if filepath.IsAbs(dir) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s.%s.directory must be a relative path inside the worktree", runtimeContractPath, name)
		}
	}
	if service.Port < 0 || service.Port > 65535 {
		return fmt.Errorf("%s.%s.port must be between 0 and 65535", runtimeContractPath, name)
	}
	service.HealthPath = normalizeHealthPath(service.HealthPath)
	return nil
}

func normalizeHealthPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func (s *RuntimeSpec) StartupCommand(projectType ProjectType, frontendPort int) (command string, healthPath string, timeoutSeconds int, err error) {
	if s == nil {
		return "", "/", 0, nil
	}
	parts := make([]string, 0, 2)
	if s.Backend != nil {
		port := s.Backend.Port
		if port == 0 {
			port = 8080
		}
		parts = append(parts, renderServiceCommand(s.Backend, port, true))
	}
	if s.Frontend != nil {
		port := frontendPort
		parts = append(parts, renderServiceCommand(s.Frontend, port, false))
	}
	if len(parts) == 0 {
		return "", "/", 0, fmt.Errorf("%s has no startup service for %s", runtimeContractPath, projectType)
	}
	for i := range parts {
		if i < len(parts)-1 {
			parts[i] = parts[i] + " &"
		}
	}
	healthPath = s.Preview.HealthPath
	if healthPath == "/" && s.Backend != nil && s.Frontend == nil {
		healthPath = s.Backend.HealthPath
	}
	if healthPath == "/" && s.Frontend != nil && s.Backend == nil {
		healthPath = s.Frontend.HealthPath
	}
	return strings.Join(parts, " "), normalizeHealthPath(healthPath), s.Preview.StartupTimeoutSeconds, nil
}

func (s *RuntimeSpec) BackendPort() int {
	if s == nil || s.Backend == nil || s.Backend.Port == 0 {
		return 0
	}
	return s.Backend.Port
}

func renderServiceCommand(service *RuntimeServiceSpec, port int, backend bool) string {
	directory := strings.TrimSpace(service.Directory)
	command := strings.ReplaceAll(service.Command, "${PORT}", strconv.Itoa(port))
	command = strings.ReplaceAll(command, "$PORT", strconv.Itoa(port))
	env := "PORT=" + strconv.Itoa(port)
	if backend {
		env += " BACKEND_PORT=" + strconv.Itoa(port)
	}
	if directory == "" || directory == "." {
		return "(" + env + " " + command + ")"
	}
	return "(cd " + shellQuote(directory) + " && " + env + " " + command + ")"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
