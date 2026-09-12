package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// inventoryWorkerObservation reports one project member's own runtime
// configuration as configured — observations, not decisions.
type inventoryWorkerObservation struct {
	Worker string `json:"worker"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

// handleProjectRuntimeInventory is a read-only migration aid: for every
// project it reports the declared runtime profile ("" stays "" — reported to
// the caller as unknown, never inferred from build files), the runtime
// observations of ALL project members, and a suggested action. It only
// observes; backfill happens per project through the audited PUT.
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
		observations := workers[p.Name]

		// Effective source: a declared project is authoritative; undeclared
		// ones are summarized from their members — "mixed" when members
		// disagree, the single source otherwise, server_default with none.
		source, value := summarizeObservations(declared, observations)

		// Undeclared projects always stay up for review: no member's pin or
		// preference substitutes for a project-level declaration.
		action := "none"
		if declared == "" {
			action = "review"
		}
		out = append(out, map[string]any{
			"project":         p.Name,
			"declaredProfile": declared,
			"declaredKnown":   declared != "",
			"effectiveSource": source,
			"effectiveValue":  value,
			"workers":         observations,
			"suggestedAction": action,
			"templateId":      p.TemplateID,
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// projectRuntimeWorkers maps project name -> sorted member runtime
// observations for all agent workers joined to that project.
func (s *Server) projectRuntimeWorkers() map[string][]inventoryWorkerObservation {
	out := map[string][]inventoryWorkerObservation{}
	if s == nil || s.agentDirectory == nil {
		return out
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return out
	}
	list, err := s.agentDirectory.ProjectWorkers(workspaceID, "")
	if err != nil {
		// Projects without resolvable members report the server default,
		// which is the truthful answer.
		return out
	}
	for _, pw := range list {
		project := strings.TrimSpace(pw.Membership.ProjectID)
		if project == "" {
			continue
		}
		source, value := workerObservation(decodeAgentWorkerRuntimeConfig(pw.Worker))
		name := strings.TrimSpace(pw.Membership.Title)
		if name == "" {
			name = strings.TrimSpace(pw.Worker.Name)
		}
		out[project] = append(out[project], inventoryWorkerObservation{
			Worker: name,
			Source: source,
			Value:  value,
		})
	}
	for project := range out {
		obs := out[project]
		sort.Slice(obs, func(i, j int) bool { return obs[i].Worker < obs[j].Worker })
		out[project] = obs
	}
	return out
}

// workerObservation resolves one worker's configured runtime source, mirroring
// taskExecutingAgentRuntime's lookup order without validating: the inventory
// reports observations, not decisions.
func workerObservation(cfg agentWorkerRuntimeConfig) (source, value string) {
	if cfg.Sandbox == nil {
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

// summarizeObservations derives the row-level effective source from the
// project declaration and its members' observations.
func summarizeObservations(declared string, observations []inventoryWorkerObservation) (source, value string) {
	if declared != "" {
		return "project", declared
	}
	if len(observations) == 0 {
		return "server_default", ""
	}
	firstSource, firstValue := observations[0].Source, observations[0].Value
	for _, obs := range observations[1:] {
		if obs.Source != firstSource || obs.Value != firstValue {
			return "mixed", ""
		}
	}
	return firstSource, firstValue
}
