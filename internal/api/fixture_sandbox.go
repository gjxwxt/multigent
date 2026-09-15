// Test-data sandbox (V1) integration: wiring + the provision call used by
// the preview engine. See internal/fixturesandbox for the contract, lease
// and artifact machinery; this file only adapts it to the Server.
package api

import (
	"context"
	"log"
	"time"

	"github.com/multigent/multigent/internal/fixturesandbox"
)

// initFixtureSandbox wires the sandbox provisioner into the preview engine
// when the environment supports it. Every failure path degrades to "no
// sandbox" (contract-less projects never notice; contract-bearing projects
// fail closed at provision time with a clear error).
func (s *Server) initFixtureSandbox() {
	dataDir := defaultWorkspaceDataDir()
	store := fixturesandbox.NewStore(s.controlDB, s.currentWorkspaceIDForSandbox(), dataDir)
	p := fixturesandbox.NewProvisioner(store, fixturesandbox.DefaultContainerGenerator)
	s.fixtureSandbox = p
	if eng, ok := s.previewEngine.(interface {
		SetSandboxHooks(provision func(ctx context.Context, taskID, projectName, worktreeDir string) ([]string, error), release func(taskID, reason string))
	}); ok {
		eng.SetSandboxHooks(p.ProvisionForPreview, p.ReleaseForStop)
	}
	// Sandbox lease reaper: every minute, aligned with the preview reaper.
	// Preview containers are stopped by the preview engine's own reaper,
	// whose release hook reclaims the lease; this loop catches headless
	// (exec-kind) leases and orphaned directories after restarts.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := p.ReapExpired(); err != nil {
				log.Printf("[fixture-sandbox] reap: %v", err)
			}
		}
	}()
}

// currentWorkspaceIDForSandbox resolves the workspace ID without a request
// context (the sandbox store is workspace-scoped like every kv_records user).
func (s *Server) currentWorkspaceIDForSandbox() string {
	if id, err := s.currentWorkspaceID(); err == nil && id != "" {
		return id
	}
	return workspaceID(s.root)
}
