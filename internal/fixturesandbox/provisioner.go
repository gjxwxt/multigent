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
	"strings"
	"time"
)

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
		outPath, err := p.generate(ctx, worktreeDir, contract.Generator.Command, p.generatorTimeout(contract))
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
