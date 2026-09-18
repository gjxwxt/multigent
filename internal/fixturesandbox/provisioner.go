// Preview integration: the Server-facing provisioner. It resolves the
// project's fixture contract, provisions (or reuses) the task-private
// database and returns the docker env assignments. A missing artifact is
// generated ONCE per (worktree schema digest, scenario) via the configured
// container generator — the generator argv from the contract never runs on
// the Go host process.
package fixturesandbox

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScenarioSummary summarizes one scenario declared in the fixture contract.
type ScenarioSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// TaskSandboxStatus reports the test-data sandbox status for a task's worktree.
type TaskSandboxStatus struct {
	HasContract        bool              `json:"hasContract"`
	Engine             string            `json:"engine,omitempty"`
	Storage            string            `json:"storage,omitempty"`
	FixtureVersion     string            `json:"fixtureVersion,omitempty"`
	ActiveScenario     string            `json:"activeScenario,omitempty"`
	AvailableScenarios []ScenarioSummary `json:"availableScenarios,omitempty"`
	LeaseID            string            `json:"leaseId,omitempty"`
	State              string            `json:"state,omitempty"`
	ResetCount         int               `json:"resetCount,omitempty"`
	ExpiresAt          *time.Time        `json:"expiresAt,omitempty"`
}

// ContainerGenerator runs the contract's argv inside a disposable sandbox
// container with the worktree mounted, producing the seeded database at the
// contract's storage path. Implementations enforce: no host execution of the
// argv, no production credentials, no model keys, bounded time and output.
type ContainerGenerator func(ctx context.Context, worktreeDir string, argv string, timeout time.Duration) (dbPath string, err error)

// Provisioner is the platform-side orchestrator the preview engine calls.
type Provisioner struct {
	store     *Store
	generate  ContainerGenerator
	contracts map[string]*cachedContract // worktree root -> loaded contract
	artifacts map[string]string          // "schemaDigest|scenario" -> artifact digest
}

type cachedContract struct {
	contract *Contract
	err      error
}

// NewProvisioner wires a provisioner over the lease store and a container
// generator (nil generator = artifacts must already exist; provisioning a
// contract with no frozen artifact then fails closed with a clear message).
func NewProvisioner(store *Store, generate ContainerGenerator) *Provisioner {
	return &Provisioner{
		store:     store,
		generate:  generate,
		contracts: map[string]*cachedContract{},
		artifacts: map[string]string{},
	}
}

// TaskStatus inspects the worktree's fixture contract and active lease.
func (p *Provisioner) TaskStatus(ctx context.Context, taskID, projectName, worktreeDir string) (*TaskSandboxStatus, error) {
	contract, err := p.loadContract(worktreeDir)
	if err != nil {
		return nil, err
	}
	if contract == nil {
		return &TaskSandboxStatus{HasContract: false}, nil
	}

	scenarios := make([]ScenarioSummary, 0, len(contract.Scenarios)+1)
	scenarioKeys := make([]string, 0, len(contract.Scenarios))
	for k := range contract.Scenarios {
		scenarioKeys = append(scenarioKeys, k)
	}
	sort.Strings(scenarioKeys)
	hasDefault := false
	for _, k := range scenarioKeys {
		if k == "default" {
			hasDefault = true
		}
		desc := ""
		if sc, ok := contract.Scenarios[k]; ok {
			desc = sc.Description
		}
		scenarios = append(scenarios, ScenarioSummary{Name: k, Description: desc})
	}
	if !hasDefault {
		scenarios = append([]ScenarioSummary{{Name: "default", Description: "Default baseline scenario"}}, scenarios...)
	}

	status := &TaskSandboxStatus{
		HasContract:        true,
		Engine:             contract.Engine,
		Storage:            contract.Storage,
		FixtureVersion:     contract.DefaultFixtureVersion,
		ActiveScenario:     "default",
		AvailableScenarios: scenarios,
	}

	active, err := p.findActiveLease(taskID)
	if err != nil {
		return nil, err
	}
	if active != nil {
		status.LeaseID = active.ID
		status.State = active.State
		if active.Scenario != "" {
			status.ActiveScenario = active.Scenario
		}
		status.ResetCount = active.ResetCount
		status.ExpiresAt = &active.ExpiresAt
	}
	return status, nil
}

func (p *Provisioner) findActiveLease(taskID string) (*Lease, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	leases, err := p.store.ListLeases()
	if err != nil {
		return nil, err
	}
	for _, l := range leases {
		if l.TaskID == taskID && l.State == StateActive {
			cp := l
			return &cp, nil
		}
	}
	return nil, nil
}

// ResetTask rebuilds the task's private database from its immutable frozen artifact.
func (p *Provisioner) ResetTask(ctx context.Context, taskID, projectName, worktreeDir string) (*ProvisionResult, error) {
	contract, err := p.loadContract(worktreeDir)
	if err != nil {
		return nil, err
	}
	if contract == nil {
		return nil, fmt.Errorf("no fixture contract declared in worktree")
	}

	active, err := p.findActiveLease(taskID)
	if err != nil {
		return nil, err
	}
	if active != nil {
		res, err := p.store.Reset(active.ID)
		if err != nil {
			return nil, err
		}
		if _, err := p.store.Renew(res.Lease.ID, DefaultLeaseTTL); err != nil {
			return nil, fmt.Errorf("renew sandbox lease after reset: %w", err)
		}
		return res, nil
	}

	// No active lease: provision fresh default
	res, err := p.provision(ctx, projectName, taskID, worktreeDir, KindPreview, "default")
	if err != nil {
		return nil, err
	}
	if _, err := p.store.Renew(res.Lease.ID, DefaultLeaseTTL); err != nil {
		return nil, fmt.Errorf("renew sandbox lease: %w", err)
	}
	return res, nil
}

// SwitchScenario switches the active dataset scenario for a task.
func (p *Provisioner) SwitchScenario(ctx context.Context, taskID, projectName, worktreeDir, scenario string) (*ProvisionResult, error) {
	contract, err := p.loadContract(worktreeDir)
	if err != nil {
		return nil, err
	}
	if contract == nil {
		return nil, fmt.Errorf("no fixture contract declared in worktree")
	}

	scenario = strings.TrimSpace(scenario)
	if scenario == "" {
		scenario = "default"
	}
	if scenario != "default" {
		if _, ok := contract.Scenarios[scenario]; !ok {
			return nil, fmt.Errorf("scenario %q is not declared in .multigent/fixtures.json", scenario)
		}
	}

	active, err := p.findActiveLease(taskID)
	if err != nil {
		return nil, err
	}
	if active != nil {
		if active.Scenario == scenario {
			return p.ResetTask(ctx, taskID, projectName, worktreeDir)
		}
		_ = p.store.Reclaim(active.ID)
	}

	res, err := p.provision(ctx, projectName, taskID, worktreeDir, KindPreview, scenario)
	if err != nil {
		return nil, err
	}
	if _, err := p.store.Renew(res.Lease.ID, DefaultLeaseTTL); err != nil {
		return nil, fmt.Errorf("renew sandbox lease: %w", err)
	}
	return res, nil
}

// ProvisionForPreview is the engine hook: returns docker env assignments
// (KEY=VALUE) or an error that fails the preview start. If the worktree has
// no fixture contract, it returns nil, nil (contract-less projects need no
// database sandbox).
func (p *Provisioner) ProvisionForPreview(ctx context.Context, taskID, projectName, worktreeDir string) ([]string, error) {
	contract, err := p.loadContract(worktreeDir)
	if err != nil {
		return nil, err
	}
	if contract == nil {
		return nil, nil
	}
	res, err := p.provision(ctx, projectName, taskID, worktreeDir, KindPreview, "default")
	if err != nil {
		return nil, err
	}
	// Touch the lease so a live preview keeps its sandbox alive.
	if _, err := p.store.Renew(res.Lease.ID, DefaultLeaseTTL); err != nil {
		return nil, fmt.Errorf("renew sandbox lease: %w", err)
	}
	return []string{res.EnvVar + "=" + res.DBPath}, nil
}

// ReleaseForStop tears down the task's sandbox lease (stop/reap path). The
// preview container is already gone when this runs.
func (p *Provisioner) ReleaseForStop(taskID, reason string) {
	leases, err := p.store.ListLeases()
	if err != nil {
		return
	}
	for _, l := range leases {
		if l.TaskID != taskID || l.State != StateActive {
			continue
		}
		_ = p.store.Reclaim(l.ID)
	}
}

// ReapExpired reclaims expired leases; called from a background loop.
// The caller is responsible for stopping any preview container first (the
// preview engine's reaper does this through ReleaseForStop).
func (p *Provisioner) ReapExpired() error {
	expired, err := p.store.ExpiredLeases(time.Now().UTC())
	if err != nil {
		return err
	}
	for _, l := range expired {
		if err := p.store.Reclaim(l.ID); err != nil {
			// A lease whose preview still runs (renewal raced the reaper)
			// fails the active->reclaiming CAS and is retried next pass.
			continue
		}
	}
	// Restart recovery: directories no lease references are removed.
	_, err = p.store.ReclaimOrphanDirs()
	return err
}

// provision resolves contract -> artifact -> private db.
func (p *Provisioner) provision(ctx context.Context, project, taskID, worktreeDir, kind, scenario string) (*ProvisionResult, error) {
	contract, err := p.loadContract(worktreeDir)
	if err != nil {
		return nil, err
	}
	if contract == nil {
		return nil, fmt.Errorf("no fixture contract: internal caller error (provision must only be called for contract-bearing worktrees)")
	}
	schemaDigest := contract.SchemaDigest()

	artifactKey := schemaDigest + "|" + scenario
	var artifact *Artifact
	var artifactPath string
	if digest, ok := p.artifacts[artifactKey]; ok {
		if a, err := p.store.GetArtifact(digest); err == nil {
			artifact = a
			artifactPath = p.store.artifactPath(digest)
		}
	}
	if artifact == nil {
		if p.generate == nil {
			return nil, fmt.Errorf("no frozen fixture artifact for schema %s and no generator configured — run the project's baseline seed in a sandbox container first", schemaDigest[:12])
		}
		cmd := contract.Generator.Command
		if scenario != "" && scenario != "default" {
			if sc, ok := contract.Scenarios[scenario]; ok && strings.TrimSpace(sc.Command) != "" {
				cmd = sc.Command
			}
		}
		outPath, err := p.generate(ctx, worktreeDir, cmd, p.generatorTimeout(contract))
		if err != nil {
			return nil, fmt.Errorf("fixture generator failed: %w", err)
		}
		fr, err := p.store.FreezeFile(outPath, project, contract.DefaultFixtureVersion, scenario, schemaDigest)
		if err != nil {
			return nil, err
		}
		artifact = &fr.Artifact
		artifactPath = fr.ArtifactPath
		p.artifacts[artifactKey] = artifact.Digest
	}

	res, err := p.store.Provision(ProvisionOptions{
		Project: project, TaskID: taskID, WorktreeDir: worktreeDir,
		Contract: contract, Scenario: scenario,
		ArtifactPath: artifactPath, Artifact: artifact, Kind: kind,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *Provisioner) generatorTimeout(c *Contract) time.Duration {
	if c.Generator.TimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(c.Generator.TimeoutSeconds) * time.Second
}

// loadContract caches contract loads per worktree root; a changed schema
// digest (migration edit) invalidates the cache naturally because the
// fingerprint is recomputed on each load.
func (p *Provisioner) loadContract(worktreeDir string) (*Contract, error) {
	root := filepath.Clean(strings.TrimSpace(worktreeDir))
	if cached, ok := p.contracts[root]; ok {
		return cached.contract, cached.err
	}
	contract, err := LoadContract(worktreeDir)
	// Negative caching is bounded: re-stat the contract file mtime to bust.
	p.contracts[root] = &cachedContract{contract: contract, err: err}
	return contract, err
}

// InvalidateContractCache drops cached contracts (called when a worktree is
// known to have changed, e.g. after a rebase or checkout).
func (p *Provisioner) InvalidateContractCache(worktreeDir string) {
	root := filepath.Clean(strings.TrimSpace(worktreeDir))
	delete(p.contracts, root)
}

// Store exposes the underlying lease store (for reaper wiring in tests).
func (p *Provisioner) Store() *Store { return p.store }

// OpenPrivateDB opens the provisioned private database (helper for tests).
func OpenPrivateDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", path+"?mode=ro")
}
