package workflow

import (
	"fmt"
	"path"
	"strings"

	"github.com/multigent/multigent/internal/entity"
)

// QA test checkpoint (acceptance-test-design-plan §5.3, Batch B-b): the qa
// step declares every path it touched; the platform verifies each one is a
// test artifact before the step may complete. The allowlist covers the file
// kinds a QA agent legitimately writes when hardening a candidate:
// test sources, testdata/fixtures, probe scripts, and test-support config —
// everything else (business implementation, CI, deploy, credentials, agent
// instruction files) is rejected.

// qaTouchedPathAllowlist matches normalized path components. A path is a test
// artifact when its base name or any directory component signals test intent.
var qaTouchedPathAllowlist = []string{
	// Test source files (any language convention in use across templates).
	"_test.go", "_test.ts", "_test.tsx", "_test.js", "_test.jsx",
	".test.go", ".test.ts", ".test.tsx", ".test.js", ".test.jsx",
	".spec.go", ".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
	"_test.py", "test_.py", ".test.py", "test.py",
	"test.java", "tests.java", "it.java",
	".rs", /* rust: tests live in src with #[cfg(test)] — handled by dir below */
	// Fixtures, snapshots, testdata, probes.
	".fixture", ".fixtures", ".snap", ".golden",
}

// qaTouchedPathDirAllowlist matches directory components anywhere in the path.
var qaTouchedPathDirAllowlist = []string{
	"test", "tests", "__tests__", "testdata", "test-data",
	"fixtures", "fixture", "testing", "e2e", "integration", "spec", "specs",
	"probes", "probe", "acceptance", "qa",
	// Go: package named foo_test is a test artifact even without a dir hit.
	// Rust convention: tests/ directory or tests.rs.
}

// qaTouchedPathForbidden overrides the allowlist: paths matching these are
// rejected even if a component superficially signals "test" — these carry
// execution or supply-chain authority that a QA checkpoint must never touch.
var qaTouchedPathForbidden = []string{
	// CI/CD and supply-chain execution surfaces.
	".gitlab-ci.yml", ".github/workflows/", "jenkinsfile", ".circleci/",
	"cloudbuild.yaml", "azure-pipelines.yaml",
	// Deployment and infrastructure.
	"dockerfile", "docker-compose", ".dockerignore",
	"deploy/", "k8s/", "kubernetes/", "terraform/", ".helm/", "charts/",
	// Credentials and environment.
	".env", "credentials", ".pem", ".key", "id_rsa", "secret",
	// Agent instruction and platform contract files.
	"agents.md", "claude.md", ".multigent/",
	// Git internals and hooks.
	".git/", "hooks/",
}

// ValidateTouchedPathFormat is the UNIVERSAL path-sanity half of the
// touched_paths contract, split from the QA whitelist (S2-2.3, review round
// P0): non-empty, "none" allowed, forward slashes, relative, inside the
// project root. It contains NO role judgment — both a delivery branch and a
// linear QA step must pass it, because declaration cross-checking (Directions
// 1/2) needs well-formed paths in either checkpoint kind.
func ValidateTouchedPathFormat(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("touched_paths is required: declare every file the run created or modified (one per line), or \"none\" if the tree is untouched")
	}
	if strings.EqualFold(trimmed, "none") {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if err := validateTouchedPathFormat(p); err != nil {
			return fmt.Errorf("touched_paths line %d (%q): %w", i+1, p, err)
		}
	}
	return nil
}

// validateTouchedPathFormat is the universal half of one path check:
// backslash/absolute/escape rejection plus the FORBIDDEN-surfaces rule
// (CI/deploy/credentials/agent-config). The forbidden rule applies in BOTH
// checkpoint kinds — a delivery branch may hand business code to the join,
// but CI pipelines, deployment manifests, credentials, and agent-config files
// remain off-limits everywhere. The test-artifact allowlist (the QA-role
// judgment) is NOT here — it lives in validateQATouchedPath.
func validateTouchedPathFormat(p string) error {
	if strings.ContainsAny(p, "\\") {
		return fmt.Errorf("use forward slashes; backslashes are not path separators here")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("paths must be relative to the project root")
	}
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("path escapes the project root")
	}
	lower := strings.ToLower(cleaned)
	for _, f := range qaTouchedPathForbidden {
		if strings.Contains(lower, f) {
			return fmt.Errorf("path touches a forbidden surface (%s) — neither QA nor delivery may modify CI/deploy/credentials/agent-config files", f)
		}
	}
	return nil
}

// ValidateQATouchedPaths is the QA-ROLE declaration check: universal path
// format PLUS the test-artifact allowlist. Used by linear QA checkpoints
// (step id "qa" / the qa step of the delivery pipeline) where the QA agent
// may only write test artifacts. Branch completions (delivery checkpoints)
// must NOT call this — they validate through ValidateTouchedPathFormat plus
// the delivery-surface cross-check instead.
func ValidateQATouchedPaths(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("touched_paths is required: declare every file the QA run created or modified (one per line), or \"none\" if the tree is untouched")
	}
	if strings.EqualFold(trimmed, "none") {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if err := validateTouchedPathFormat(p); err != nil {
			return fmt.Errorf("touched_paths line %d (%q): %w", i+1, p, err)
		}
		cleaned := path.Clean(p)
		lower := strings.ToLower(cleaned)
		if !isQATestArtifactPath(cleaned, lower) {
			return fmt.Errorf("touched_paths line %d (%q): path does not look like a test artifact (test source, testdata, fixture, or probe script) — QA may only touch test files", i+1, p)
		}
	}
	return nil
}

// workflowFieldDeclared reports whether the field name is declared on the
// step — the opt-in signal for the QA touched-paths checkpoint.
func workflowFieldDeclared(fields []entity.WorkflowField, name string) bool {
	for _, f := range fields {
		if strings.TrimSpace(f.Name) == name {
			return true
		}
	}
	return false
}

func isQATestArtifactPath(cleaned, lower string) bool {
	base := path.Base(lower)
	for _, suffix := range qaTouchedPathAllowlist {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	// Go: the _test suffix already matched above; also accept *_test dirs'
	// contents and any file under a test-ish directory.
	for _, component := range strings.Split(lower, "/") {
		for _, dir := range qaTouchedPathDirAllowlist {
			if component == dir || component == dir+"_test" {
				return true
			}
		}
	}
	// Go package-external test dirs like "store_test/" are covered by the
	// suffix above; also accept "foo_test.go"-style names under any dir.
	if strings.HasSuffix(base, "test.go") || strings.HasSuffix(base, "tests.go") {
		return true
	}
	// Rust: tests.rs / tests/ handled; also integration test binaries.
	if strings.HasSuffix(base, "test.rs") || strings.HasSuffix(base, "tests.rs") {
		return true
	}
	return false
}
