package brownfield

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/sandbox"
)

// Detect performs a pure, read-only scan of the given repository directory
// to discover technology stacks, services, package managers, lockfiles,
// scripts, and container/CI configurations. It never modifies the repository.
func Detect(repoDir string) (ScanReport, error) {
	cleanRepo := filepath.Clean(repoDir)
	report := ScanReport{
		Repo: cleanRepo,
	}

	// 1. Check existing .multigent/runtime.json
	if _, err := os.Stat(filepath.Join(cleanRepo, ".multigent", "runtime.json")); err == nil {
		report.ExistingRuntimeJSON = true
	}

	// 2. Discover CI configurations
	report.CIConfigs = detectCIConfigs(cleanRepo)

	// 3. Discover Container / Compose configurations
	report.ContainerConfigs = detectContainerConfigs(cleanRepo)

	// 4. Discover candidate directories to scan
	candidateDirs := []string{""}
	entries, err := os.ReadDir(cleanRepo)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") ||
				name == "node_modules" ||
				name == "dist" ||
				name == "build" ||
				name == "target" ||
				name == "vendor" ||
				name == "bin" ||
				name == "out" {
				continue
			}
			candidateDirs = append(candidateDirs, name)
		}
	}

	// 5. Scan each candidate directory
	var services []ServiceDetection
	stackSet := make(map[TechStackType]bool)

	for _, relDir := range candidateDirs {
		dirPath := filepath.Join(cleanRepo, relDir)
		detected := scanDirectory(cleanRepo, dirPath, relDir)
		for _, svc := range detected {
			services = append(services, svc)
			stackSet[svc.TechStack] = true
		}
	}

	// If subdirectories were detected as services (e.g. web/ and server/),
	// eliminate any duplicate root service that only exists because of monorepo root package.json
	// if the subdirectories already represent the real services.
	report.Services = normalizeServices(services)

	// Collect unique tech stacks
	for stack := range stackSet {
		report.TechStacks = append(report.TechStacks, stack)
	}
	sort.Slice(report.TechStacks, func(i, j int) bool {
		return report.TechStacks[i] < report.TechStacks[j]
	})

	// Recommended runtime profile: JVM if Java is detected, otherwise Base
	if stackSet[TechStackJava] {
		report.RecommendedProfile = sandbox.ProfileJVM21
	} else {
		report.RecommendedProfile = sandbox.ProfileBase
	}

	return report, nil
}

func detectCIConfigs(repoDir string) []string {
	var configs []string
	candidates := []string{
		".gitlab-ci.yml",
		"Jenkinsfile",
		".travis.yml",
		"azure-pipelines.yml",
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(repoDir, c)); err == nil {
			configs = append(configs, c)
		}
	}
	if info, err := os.Stat(filepath.Join(repoDir, ".github", "workflows")); err == nil && info.IsDir() {
		configs = append(configs, ".github/workflows")
	}
	return configs
}

func detectContainerConfigs(repoDir string) []string {
	var configs []string
	candidates := []string{
		"Dockerfile",
		"docker-compose.yml",
		"docker-compose.yaml",
		"compose.yml",
		"compose.yaml",
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(repoDir, c)); err == nil {
			configs = append(configs, c)
		}
	}
	return configs
}

func scanDirectory(repoDir, dirPath, relDir string) []ServiceDetection {
	var results []ServiceDetection

	// 1. Node.js inspection
	if pkgRaw, err := os.ReadFile(filepath.Join(dirPath, "package.json")); err == nil {
		svc := ServiceDetection{
			Directory: relDir,
			TechStack: TechStackNode,
			Scripts:   make(map[string]string),
		}

		var pkg struct {
			Name    string            `json:"name"`
			Scripts map[string]string `json:"scripts"`
		}
		if err := json.Unmarshal(pkgRaw, &pkg); err == nil {
			svc.Scripts = pkg.Scripts
		}

		// Lockfile detection
		if _, err := os.Stat(filepath.Join(dirPath, "package-lock.json")); err == nil {
			svc.PackageManager = PkgNPM
			svc.Lockfile = "package-lock.json"
			svc.HasLockfile = true
		} else if _, err := os.Stat(filepath.Join(dirPath, "pnpm-lock.yaml")); err == nil {
			svc.PackageManager = PkgPNPM
			svc.Lockfile = "pnpm-lock.yaml"
			svc.HasLockfile = true
		} else if _, err := os.Stat(filepath.Join(dirPath, "yarn.lock")); err == nil {
			svc.PackageManager = PkgYarn
			svc.Lockfile = "yarn.lock"
			svc.HasLockfile = true
		} else if _, err := os.Stat(filepath.Join(dirPath, "bun.lockb")); err == nil {
			svc.PackageManager = PkgBun
			svc.Lockfile = "bun.lockb"
			svc.HasLockfile = true
		} else if relDir != "" {
			// Check root for monorepo lockfile
			if _, err := os.Stat(filepath.Join(repoDir, "package-lock.json")); err == nil {
				svc.PackageManager = PkgNPM
				svc.Lockfile = "../package-lock.json"
				svc.HasLockfile = true
			} else if _, err := os.Stat(filepath.Join(repoDir, "pnpm-lock.yaml")); err == nil {
				svc.PackageManager = PkgPNPM
				svc.Lockfile = "../pnpm-lock.yaml"
				svc.HasLockfile = true
			} else if _, err := os.Stat(filepath.Join(repoDir, "yarn.lock")); err == nil {
				svc.PackageManager = PkgYarn
				svc.Lockfile = "../yarn.lock"
				svc.HasLockfile = true
			} else {
				svc.PackageManager = PkgNPM
				svc.HasLockfile = false
			}
		} else {
			svc.PackageManager = PkgNPM
			svc.HasLockfile = false
		}

		// Heuristic naming & ports
		assignNodeServiceRole(&svc, relDir)
		results = append(results, svc)
	}

	// 2. JVM inspection (Gradle / Maven)
	isGradle := fileExists(filepath.Join(dirPath, "build.gradle")) || fileExists(filepath.Join(dirPath, "build.gradle.kts"))
	isMaven := fileExists(filepath.Join(dirPath, "pom.xml"))

	if isGradle || isMaven {
		svc := ServiceDetection{
			Name:         "backend",
			Directory:    relDir,
			TechStack:    TechStackJava,
			DetectedPort: 8080,
			HealthPath:   "/api/health",
			Scripts:      make(map[string]string),
		}

		if isGradle {
			svc.PackageManager = PkgGradle
			// Check wrapper in dirPath or root
			wrapperPath := filepath.Join(dirPath, "gradlew")
			if !fileExists(wrapperPath) && relDir != "" {
				wrapperPath = filepath.Join(repoDir, "gradlew")
			}
			if fi, err := os.Stat(wrapperPath); err == nil {
				svc.Wrapper = "gradlew"
				svc.HasWrapper = true
				svc.WrapperExec = (fi.Mode() & 0111) != 0
			}
			svc.HasLockfile = true // Gradle builds rely on build.gradle dependency declarations
			svc.Scripts["dev"] = "./gradlew bootRun"
			svc.Scripts["test"] = "./gradlew test"
			svc.Scripts["build"] = "./gradlew build"
		} else {
			svc.PackageManager = PkgMaven
			wrapperPath := filepath.Join(dirPath, "mvnw")
			if !fileExists(wrapperPath) && relDir != "" {
				wrapperPath = filepath.Join(repoDir, "mvnw")
			}
			if fi, err := os.Stat(wrapperPath); err == nil {
				svc.Wrapper = "mvnw"
				svc.HasWrapper = true
				svc.WrapperExec = (fi.Mode() & 0111) != 0
			}
			svc.HasLockfile = true
			svc.Scripts["dev"] = "./mvnw spring-boot:run"
			svc.Scripts["test"] = "./mvnw test"
			svc.Scripts["build"] = "./mvnw package"
		}

		results = append(results, svc)
	}

	// 3. Go inspection
	if fileExists(filepath.Join(dirPath, "go.mod")) {
		svc := ServiceDetection{
			Name:           "backend",
			Directory:      relDir,
			TechStack:      TechStackGo,
			PackageManager: PkgGoMod,
			DetectedPort:   8080,
			HealthPath:     "/api/health",
			Scripts: map[string]string{
				"test":  "go test ./...",
				"build": "go build -o bin/server .",
				"dev":   "go run .",
			},
		}
		if fileExists(filepath.Join(dirPath, "go.sum")) {
			svc.HasLockfile = true
			svc.Lockfile = "go.sum"
		} else if relDir != "" && fileExists(filepath.Join(repoDir, "go.sum")) {
			svc.HasLockfile = true
			svc.Lockfile = "../go.sum"
		} else {
			svc.HasLockfile = false
		}
		results = append(results, svc)
	}

	// 4. Python inspection
	hasPyproject := fileExists(filepath.Join(dirPath, "pyproject.toml"))
	hasReqs := fileExists(filepath.Join(dirPath, "requirements.txt"))
	hasPipfile := fileExists(filepath.Join(dirPath, "Pipfile"))

	if hasPyproject || hasReqs || hasPipfile {
		svc := ServiceDetection{
			Name:         "backend",
			Directory:    relDir,
			TechStack:    TechStackPython,
			DetectedPort: 8000,
			HealthPath:   "/health",
			Scripts: map[string]string{
				"test": "pytest",
				"dev":  "python main.py",
			},
		}
		if fileExists(filepath.Join(dirPath, "poetry.lock")) {
			svc.PackageManager = PkgPoetry
			svc.Lockfile = "poetry.lock"
			svc.HasLockfile = true
			svc.Scripts["dev"] = "poetry run python main.py"
			svc.Scripts["test"] = "poetry run pytest"
		} else if fileExists(filepath.Join(dirPath, "Pipfile.lock")) {
			svc.PackageManager = PkgPipenv
			svc.Lockfile = "Pipfile.lock"
			svc.HasLockfile = true
		} else if hasPipfile {
			svc.PackageManager = PkgPipenv
			svc.HasLockfile = false
		} else if hasPyproject {
			svc.PackageManager = PkgPoetry
			svc.HasLockfile = false
		} else if hasReqs {
			svc.PackageManager = PkgPip
			svc.Lockfile = "requirements.txt"
			// Check if requirements.txt has hashes
			if content, err := os.ReadFile(filepath.Join(dirPath, "requirements.txt")); err == nil {
				svc.HasLockfile = bytes.Contains(content, []byte("--hash="))
			}
		} else {
			svc.PackageManager = PkgPip
			svc.HasLockfile = false
		}
		results = append(results, svc)
	}

	// 5. Rust inspection
	if fileExists(filepath.Join(dirPath, "Cargo.toml")) {
		svc := ServiceDetection{
			Name:           "backend",
			Directory:      relDir,
			TechStack:      TechStackRust,
			PackageManager: PkgCargo,
			DetectedPort:   8080,
			HealthPath:     "/health",
			Scripts: map[string]string{
				"dev":   "cargo run",
				"test":  "cargo test",
				"build": "cargo build",
			},
		}
		if fileExists(filepath.Join(dirPath, "Cargo.lock")) {
			svc.HasLockfile = true
			svc.Lockfile = "Cargo.lock"
		} else {
			svc.HasLockfile = false
		}
		results = append(results, svc)
	}

	return results
}

func assignNodeServiceRole(svc *ServiceDetection, relDir string) {
	lowerDir := strings.ToLower(relDir)
	if lowerDir == "web" || lowerDir == "frontend" || lowerDir == "client" || lowerDir == "ui" {
		svc.Name = "frontend"
		svc.DetectedPort = 3000
		if strings.Contains(svc.Scripts["dev"], "vite") || strings.Contains(svc.Scripts["start"], "vite") {
			svc.DetectedPort = 5173
		}
		svc.HealthPath = "/"
		return
	}
	if lowerDir == "server" || lowerDir == "backend" || lowerDir == "api" {
		svc.Name = "backend"
		svc.DetectedPort = 8080
		svc.HealthPath = "/api/health"
		return
	}

	// Root directory heuristic
	devScript := svc.Scripts["dev"] + " " + svc.Scripts["start"]
	if strings.Contains(devScript, "vite") || strings.Contains(devScript, "react") || strings.Contains(devScript, "next") || strings.Contains(devScript, "vue") {
		svc.Name = "frontend"
		svc.DetectedPort = 3000
		if strings.Contains(devScript, "vite") {
			svc.DetectedPort = 5173
		}
		svc.HealthPath = "/"
	} else if strings.Contains(devScript, "express") || strings.Contains(devScript, "nest") || strings.Contains(devScript, "fastify") {
		svc.Name = "backend"
		svc.DetectedPort = 8080
		svc.HealthPath = "/api/health"
	} else {
		svc.Name = "root"
		svc.DetectedPort = 3000
		svc.HealthPath = "/"
	}
}

func normalizeServices(services []ServiceDetection) []ServiceDetection {
	if len(services) <= 1 {
		return services
	}

	// Check if we have subdirectories web/frontend and server/backend
	hasSubdirFrontend := false
	hasSubdirBackend := false
	for _, s := range services {
		if s.Directory != "" {
			if s.Name == "frontend" {
				hasSubdirFrontend = true
			}
			if s.Name == "backend" {
				hasSubdirBackend = true
			}
		}
	}

	if hasSubdirFrontend || hasSubdirBackend {
		var filtered []ServiceDetection
		for _, s := range services {
			if s.Directory == "" && (hasSubdirFrontend || hasSubdirBackend) {
				// Skip monorepo root package.json if children exist
				continue
			}
			filtered = append(filtered, s)
		}
		return filtered
	}

	return services
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
