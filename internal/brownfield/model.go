// Package brownfield implements the deterministic read-only detection, baseline
// inspection, and readiness evaluation gate for brownfield (existing) repositories.
// All logic is pure verification and contract synthesis; no repo modification
// or agent guessing is performed in this package.
package brownfield

import "github.com/multigent/multigent/internal/preview"

// TechStackType identifies detected language/runtime ecosystems.
type TechStackType string

const (
	TechStackNode   TechStackType = "node"
	TechStackJava   TechStackType = "java"
	TechStackGo     TechStackType = "go"
	TechStackPython TechStackType = "python"
	TechStackRust   TechStackType = "rust"
	TechStackOther  TechStackType = "other"
)

// PackageManager identifies the dependency and build manager.
type PackageManager string

const (
	PkgNPM    PackageManager = "npm"
	PkgPNPM   PackageManager = "pnpm"
	PkgYarn   PackageManager = "yarn"
	PkgBun    PackageManager = "bun"
	PkgMaven  PackageManager = "maven"
	PkgGradle PackageManager = "gradle"
	PkgGoMod  PackageManager = "go"
	PkgPoetry PackageManager = "poetry"
	PkgPip    PackageManager = "pip"
	PkgPipenv PackageManager = "pipenv"
	PkgCargo  PackageManager = "cargo"
)

// Severity categorizes the impact of an evaluation issue.
type Severity string

const (
	// SeverityBlocking indicates a hard blocker that requires remediation before onboarding.
	SeverityBlocking Severity = "blocking"
	// SeverityWarning indicates weak determinism or a recommendation that does not block onboarding.
	SeverityWarning Severity = "warning"
)

// ReadinessStatus indicates overall onboarding gate decision.
type ReadinessStatus string

const (
	StatusReady    ReadinessStatus = "ready"
	StatusNotReady ReadinessStatus = "not_ready"
)

// Issue records one specific finding during readiness evaluation.
type Issue struct {
	Code           string   `json:"code"`
	Severity       Severity `json:"severity"`
	Message        string   `json:"message"`
	Recommendation string   `json:"recommendation"`
	Remediation    string   `json:"remediation,omitempty"`
}

// ServiceDetection records a discovered service or component inside the repository.
type ServiceDetection struct {
	Name           string            `json:"name"`                     // e.g. "frontend", "backend", "root"
	Directory      string            `json:"directory"`                // relative path, e.g. "", "web", "server"
	TechStack      TechStackType     `json:"techStack"`                // node, java, go, python, rust
	PackageManager PackageManager    `json:"packageManager"`           // npm, gradle, go, etc.
	Lockfile       string            `json:"lockfile,omitempty"`       // filename of detected lockfile
	HasLockfile    bool              `json:"hasLockfile"`              // true if deterministic lockfile exists
	Wrapper        string            `json:"wrapper,omitempty"`        // e.g. gradlew, mvnw
	HasWrapper     bool              `json:"hasWrapper"`               // true if wrapper script is present
	WrapperExec    bool              `json:"wrapperExec"`              // true if wrapper has 0111 executable bits
	Scripts        map[string]string `json:"scripts,omitempty"`        // discovered scripts/commands
	DetectedPort   int               `json:"detectedPort,omitempty"`   // suggested port (e.g. 3000, 8080)
	HealthPath     string            `json:"healthPath,omitempty"`     // suggested health check path
}

// ScanReport contains the complete read-only scan outcome for a repository.
type ScanReport struct {
	Repo                string             `json:"repo"`
	TechStacks          []TechStackType    `json:"techStacks"`
	Services            []ServiceDetection `json:"services"`
	CIConfigs           []string           `json:"ciConfigs"`
	ContainerConfigs    []string           `json:"containerConfigs"`
	ExistingRuntimeJSON bool               `json:"existingRuntimeJson"`
	RecommendedProfile  string             `json:"recommendedProfile"`
}

// ReadinessResult represents the structured evaluation decision and synthesized runtime contract.
type ReadinessResult struct {
	Status                ReadinessStatus      `json:"status"`
	Summary               string               `json:"summary"`
	Issues                []Issue              `json:"issues"`
	RecommendedProfile    string               `json:"recommendedProfile"`
	SynthesizedRuntime    *preview.RuntimeSpec `json:"synthesizedRuntime,omitempty"`
	SynthesizedRuntimeRaw string               `json:"synthesizedRuntimeRaw,omitempty"`
}
