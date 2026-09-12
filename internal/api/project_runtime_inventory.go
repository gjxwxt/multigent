package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// handleProjectRuntimeInventory is a read-only migration aid: for every
// project it reports the declared runtime profile ("" stays "" — reported to
// the caller as unknown, never inferred from build files), the effective
// runtime source for a worker joined to the project, and a suggested action.
// It only observes; backfill happens per project through the audited PUT.
func (s *Server) handleProjectRuntimeInventory(w http.ResponseWriter, r *http.Request) {
	if !s.canAdminCurrentWorkspace(r) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeAdminRequired, "workspace admin access required")
		return
	}
	projects, err := s.st.ListProjects()
	if err != nil {
		s.serverError(w, err)
		return
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })

	workers := s.projectRuntimeWorkers()
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		if p == nil {
			continue
		}
		declared := strings.TrimSpace(p.RuntimeProfile)
		source, value := s.effectiveRuntimeSource(declared, workers[p.Name])

		// Suggested action is conservative: declared projects need nothing;
		// undeclared ones are flagged for human review (never auto-backfilled).
		action := "none"
		if declared == "" && source != "agent_image" {
			action = "review"
		}
		out = append(out, map[string]any{
			"project":         p.Name,
			"declaredProfile": declared,
			"declaredKnown":   declared != "",
			"effectiveSource": source,
			"effectiveValue":  value,
			"suggestedAction": action,
			"templateId":      p.TemplateID,
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// projectRuntimeWorkers maps project name -> one representative worker
// runtime config for members joined to that project.
func (s *Server) projectRuntimeWorkers() map[string]*agentWorkerRuntimeConfig {
	out := map[string]*agentWorkerRuntimeConfig{}
	if s == nil || s.agentDirectory == nil {
		return out
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return out
	}
	list, err := s.agentDirectory.ProjectWorkers(workspaceID, "")
	if err != nil || list == nil {
		// Fall through: projects without resolvable members report
		// server_default, which is the truthful answer.
		return out
	}
	for _, pw := range list {
		project := strings.TrimSpace(pw.Membership.ProjectID)
		if project == "" || out[project] != nil {
			continue
		}
		cfg := decodeAgentWorkerRuntimeConfig(pw.Worker)
		out[project] = &cfg
	}
	return out
}

// effectiveRuntimeSource resolves which authority governs a project's
// runtime today, mirroring taskExecutingAgentRuntime + sandbox.ResolveRuntime
// order but without validating: the inventory reports observations, not
// decisions.
func (s *Server) effectiveRuntimeSource(declared string, cfg *agentWorkerRuntimeConfig) (source, value string) {
	if declared != "" {
		return "project", declared
	}
	if cfg == nil || cfg.Sandbox == nil {
		return "server_default", ""
	}
	// A pinned image wins regardless of profiles. The reference may live at
	// the provider-neutral sandbox.image or the docker-specific override —
	// the same double lookup taskExecutingAgentRuntime applies.
	if image := strings.TrimSpace(cfg.Sandbox.Image); image != "" {
		return "agent_image", image
	}
	if cfg.Sandbox.Docker != nil {
		if image := strings.TrimSpace(cfg.Sandbox.Docker.Image); image != "" {
			return "agent_image", image
		}
		if profile := strings.TrimSpace(cfg.Sandbox.Docker.Profile); profile != "" {
			return "agent", profile
		}
	}
	return "server_default", ""
}
