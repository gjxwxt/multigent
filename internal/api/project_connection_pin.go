package api

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// Project-scoped connection pinning. Platform features (code host push, the
// design gate) resolve their workspace-level connection per project so that
// multi-connection workspaces keep projects isolated from each other:
//
//  1. the project's explicit RemoteConnection pin (gitlab/github only);
//  2. the provider's workspace default (is_default);
//  3. legacy fallback: newest-updated connection of that provider.
//
// Resolution results are transparent: the chosen connection is recorded on
// the design start audit entry and backfilled into RemoteConnection so the
// first successful pin sticks instead of silently drifting.

// pinnedCodeHost resolves the code host for a project, backfilling
// RemoteConnection on first successful resolution when the project names a
// remote provider but predates the pin. Returns the host and connection ID.
func (s *Server) pinnedCodeHost(ctx context.Context, project string, p *entity.Project, provider string) (codehost.CodeHost, string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	host, connID, err := s.resolveCodeHost(provider, p.RemoteConnection)
	if err != nil {
		return nil, "", err
	}
	s.backfillProjectRemoteConnection(project, p, connID)
	return host, connID, nil
}

// pinnedGitLabHost resolves the GitLab host for a project with the same
// pin/backfill semantics as pinnedCodeHost, returning the concrete type
// ci_ready needs for pipeline evidence.
func (s *Server) pinnedGitLabHost(ctx context.Context, project string, p *entity.Project) (*codehost.GitLabHost, string, error) {
	host, connID, err := s.pinnedCodeHost(ctx, project, p, "gitlab")
	if err != nil {
		return nil, "", err
	}
	gitlab, ok := host.(*codehost.GitLabHost)
	if !ok {
		return nil, "", fmt.Errorf("resolved code host is not GitLab")
	}
	return gitlab, connID, nil
}

// backfillProjectRemoteConnection pins the resolved connection onto the
// project once. Best-effort and idempotent: legacy projects that never had a
// RemoteConnection stop riding the fallback after their first successful
// resolve, and projects already pinned keep their pin untouched.
func (s *Server) backfillProjectRemoteConnection(project string, p *entity.Project, connID string) {
	if p == nil || strings.TrimSpace(connID) == "" || strings.TrimSpace(p.RemoteConnection) == connID {
		return
	}
	if strings.TrimSpace(p.RemoteConnection) != "" {
		return // explicit pin set; never overwrite user intent
	}
	p.RemoteConnection = connID
	if err := s.st.SaveProject(project, p); err != nil {
		log.Printf("[connection-pin] backfill RemoteConnection for %s failed: %v", project, err)
	}
}

// resolveDesignConnectionForProject resolves the OD connection for a design
// operation on a project. Same workspace-level pool as before (OD has no
// per-project pin field), but the chosen connection is deterministic since
// is_default exists and is reported for transparency.
func (s *Server) resolveDesignConnectionForProject(project string) (*designConnectionConfig, error) {
	cfg, err := s.resolveDesignConnection()
	if err != nil {
		return nil, err
	}
	if cfg != nil && cfg.ConnID != "" {
		log.Printf("[connection-pin] design connection for project %s: %s", project, cfg.ConnID)
	}
	return cfg, nil
}

// designConnectionAuditFields returns the audit map entries documenting which
// connection a design start used, so multi-connection workspaces can answer
// "which OD did this task talk to" from the audit trail alone.
func designConnectionAuditFields(cfg *designConnectionConfig) map[string]any {
	if cfg == nil {
		return nil
	}
	fields := map[string]any{"odConnectionId": cfg.ConnID}
	if cfg.BaseURL != "" {
		if parsed, err := url.Parse(cfg.BaseURL); err == nil && parsed.Host != "" {
			fields["odHost"] = parsed.Host
		}
	}
	return fields
}

var _ = controldb.ConnectionFilter{} // keep import stable for future pin lookups
