// Package fixturesandbox implements the test-data sandbox V1 (Phase 1):
// contract loading from .multigent/fixtures.json, artifact lifecycle with
// persistent leases, task-private database provisioning and expiry reaping.
//
// Design invariants (batch-execution-review §3.2):
//   - engine is sqlite-only in V1; paths must stay inside the worktree;
//   - every data directory belongs to a lease with an ID, a state machine and
//     an expiry (no anonymous dirs the reaper cannot account for);
//   - state transitions are kv_records revision-CAS so multiple console
//     processes sharing the SQLite store serialize correctly;
//   - generators never run on the Go host process — the caller executes the
//     contract's argv inside a disposable container and only hands the
//     produced db file to this package.
package fixturesandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const contractPath = ".multigent/fixtures.json"

// Contract is the validated .multigent/fixtures.json document.
type Contract struct {
	Version               int                  `json:"version"`
	Engine                string               `json:"engine"`
	Storage               string               `json:"storage"`
	DefaultFixtureVersion string               `json:"defaultFixtureVersion"`
	SchemaFingerprintPaths []string            `json:"schemaFingerprintPaths"`
	Generator             GeneratorSpec        `json:"generator"`
	Scenarios             map[string]ScenarioSpec `json:"scenarios"`

	// schemaDigest is derived at load time from schemaFingerprintPaths
	// (sorted glob, sha256 over "path\x00content" pairs).
	schemaDigest string
	// sourceDir is the worktree root the contract was loaded from.
	sourceDir string
}

// GeneratorSpec is the baseline production command (executed inside a
// disposable sandbox container, never on the host).
type GeneratorSpec struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// ScenarioSpec is one named deterministic delta command.
type ScenarioSpec struct {
	Description string `json:"description,omitempty"`
	Command     string `json:"command"`
}

// MaxGeneratorTimeout caps the generator wall clock regardless of contract
// contents (a hostile or careless contract cannot pin the platform).
const MaxGeneratorTimeout = 600

// LoadContract reads and fully validates .multigent/fixtures.json under
// worktreeDir. An absent contract returns (nil, nil): the sandbox is opt-in
// per project. Any existing-but-invalid contract fails closed.
func LoadContract(worktreeDir string) (*Contract, error) {
	root := filepath.Clean(strings.TrimSpace(worktreeDir))
	if root == "" {
		return nil, fmt.Errorf("worktree dir is required")
	}
	path := filepath.Join(root, filepath.FromSlash(contractPath))
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", contractPath, err)
	}
	var c Contract
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", contractPath, err)
	}
	if err := c.validate(root); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", contractPath, err)
	}
	digest, err := schemaFingerprint(root, c.SchemaFingerprintPaths)
	if err != nil {
		return nil, fmt.Errorf("schema fingerprint: %w", err)
	}
	c.schemaDigest = digest
	c.sourceDir = root
	return &c, nil
}

func (c *Contract) validate(root string) error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1, got %d", c.Version)
	}
	if c.Engine != "sqlite" {
		return fmt.Errorf("engine must be \"sqlite\" in V1, got %q", c.Engine)
	}
	if err := validateRelPath("storage", c.Storage); err != nil {
		return err
	}
	if strings.TrimSpace(c.DefaultFixtureVersion) == "" {
		return fmt.Errorf("defaultFixtureVersion is required")
	}
	if strings.TrimSpace(c.Generator.Command) == "" {
		return fmt.Errorf("generator.command is required")
	}
	if c.Generator.TimeoutSeconds < 0 || c.Generator.TimeoutSeconds > MaxGeneratorTimeout {
		return fmt.Errorf("generator.timeoutSeconds must be between 0 and %d", MaxGeneratorTimeout)
	}
	if len(c.SchemaFingerprintPaths) > 32 {
		return fmt.Errorf("schemaFingerprintPaths supports at most 32 entries")
	}
	for _, p := range c.SchemaFingerprintPaths {
		if err := validateGlobPath(p); err != nil {
			return err
		}
	}
	if len(c.Scenarios) > 32 {
		return fmt.Errorf("scenarios supports at most 32 entries")
	}
	for name, sc := range c.Scenarios {
		if !validScenarioName(name) {
			return fmt.Errorf("scenario name %q must match [a-z0-9_-]{1,64}", name)
		}
		if strings.TrimSpace(sc.Command) == "" {
			return fmt.Errorf("scenario %q command is required", name)
		}
	}
	return nil
}

func validateRelPath(field, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s is required", field)
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") {
		return fmt.Errorf("%s must be a relative path, got %q", field, value)
	}
	clean := filepath.Clean(trimmed)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must stay inside the worktree, got %q", field, value)
	}
	return nil
}

// validateGlobPath is validateRelPath with glob characters allowed.
func validateGlobPath(pattern string) error {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" {
		return fmt.Errorf("schemaFingerprintPaths entry is required")
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") {
		return fmt.Errorf("schemaFingerprintPaths entry must be relative, got %q", pattern)
	}
	// Reject traversal segments anywhere in the pattern (Glob would resolve
	// them against the worktree root and escape it).
	for _, part := range strings.Split(filepath.ToSlash(trimmed), "/") {
		if part == ".." {
			return fmt.Errorf("schemaFingerprintPaths entry must not contain \"..\", got %q", pattern)
		}
	}
	return nil
}

func validScenarioName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// SchemaDigest returns the derived schema fingerprint ("" before load).
func (c *Contract) SchemaDigest() string { return c.schemaDigest }

// SourceDir returns the worktree root the contract was loaded from.
func (c *Contract) SourceDir() string { return c.sourceDir }

// Scenario returns the named scenario spec.
func (c *Contract) Scenario(name string) (ScenarioSpec, bool) {
	sc, ok := c.Scenarios[name]
	return sc, ok
}

// SchemaFingerprintFor computes the sorted-glob sha256 for arbitrary paths —
// exported for tests and cross-checking a provisioned artifact against the
// current worktree.
func SchemaFingerprintFor(root string, patterns []string) (string, error) {
	return schemaFingerprint(root, patterns)
}

// schemaFingerprint resolves every glob (sorted), reads the matched files and
// hashes "path\x00content\x00" pairs. A pattern matching zero files is fine
// (empty schema is a valid early-project state); a matched unreadable file
// fails closed.
func schemaFingerprint(root string, patterns []string) (string, error) {
	type entry struct{ path, content string }
	var entries []entry
	seen := map[string]bool{}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(strings.TrimSpace(pattern))))
		if err != nil {
			return "", fmt.Errorf("glob %q: %w", pattern, err)
		}
		sort.Strings(matches)
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil {
				return "", fmt.Errorf("stat %s: %w", m, err)
			}
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("schema path %s is not a regular file", m)
			}
			raw, err := os.ReadFile(m)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", m, err)
			}
			rel, err := filepath.Rel(root, m)
			if err != nil {
				return "", fmt.Errorf("rel %s: %w", m, err)
			}
			if !seen[rel] {
				seen[rel] = true
				entries = append(entries, entry{path: filepath.ToSlash(rel), content: string(raw)})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.path))
		h.Write([]byte{0})
		h.Write([]byte(e.content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
