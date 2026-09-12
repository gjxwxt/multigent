package api

import (
	"context"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/codehost"
	"github.com/multigent/multigent/internal/entity"
	gitworktree "github.com/multigent/multigent/internal/gitworktree"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

// After the initialization workflow's sync step, the agent-created remote
// exists but the control plane has no record of it: RemoteProjectID is only
// written by the project PUT, and bindDefaultRunner runs at initialize-
// template time when that field is still empty. Every pipeline of the new
// repository then stays pending forever ("no runners attached"), which the
// p15 canary §8.4 traced end-to-end.
//
// adoptAgentCreatedRemote closes that gap from the platform side: when an
// initialization-workflow sync step completes, read the origin URL the agent
// actually pushed to, resolve it against the pinned GitLab host, persist
// RemoteProjectID (+RemoteProvider/RemoteURL), and bind the configured
// default runner. Best-effort by design — an unresolvable origin (no remote,
// non-GitLab, host mismatch) is logged and skipped; the next PUT or a later
// sync can still bind.
//
// The sandbox controls the worktree's origin URL, so origin alone must never
// authorize adoption: an agent repointing origin at any other repository on
// the same GitLab would otherwise get the platform runner attached to a repo
// it should not own. Adoption therefore requires the resolved path to match
// a PLATFORM-controlled remote identity (see originAdoptAuthorized).

// adoptRemoteAfterSync runs the remote adoption for a completed init-workflow
// sync step. Called from the runtime step-complete path; never fails the step.
func (s *Server) adoptRemoteAfterSync(ctx context.Context, project string, t *entity.Task) {
	if s == nil || s.st == nil || t == nil {
		return
	}
	p, err := s.st.Project(project)
	if err != nil || p == nil {
		return
	}
	if strings.TrimSpace(p.RemoteProjectID) != "" {
		return // already bound; nothing to adopt
	}
	if !strings.EqualFold(strings.TrimSpace(p.RemoteProvider), "gitlab") && strings.TrimSpace(p.RemoteConnection) == "" {
		// No remote intent declared on the project record — but the standard
		// init flow pushes via the connection-pinned credential helper without
		// a prior PUT, so fall through and let the origin lookup decide.
	}
	host, _, err := s.pinnedGitLabHost(ctx, project, p)
	if err != nil {
		log.Printf("[remote-adopt] %s: no GitLab host pinned, skip origin adoption: %v", project, err)
		return
	}
	originURL := s.initWorktreeOriginURL(project, t)
	if originURL == "" {
		log.Printf("[remote-adopt] %s: task %s has no origin remote to adopt", project, t.ID)
		return
	}
	projectPath := gitlabProjectPathFromURL(originURL, host)
	if projectPath == "" {
		log.Printf("[remote-adopt] %s: origin is not on the pinned GitLab host, skip (%s)", project, gitworktree.RedactGitOutput(originURL))
		return
	}
	if !originAdoptAuthorized(p, projectPath) {
		log.Printf("[remote-adopt] %s: origin path %s is not a platform-controlled remote (no CloneURL match, not allowlisted); refusing to adopt or bind a runner — worktree origin is agent-writable and must not grant authorization", project, projectPath)
		return
	}
	repo, err := host.RepositoryByProjectPath(ctx, projectPath)
	if err != nil {
		log.Printf("[remote-adopt] %s: lookup %s failed: %v", project, projectPath, err)
		return
	}
	p.RemoteProvider = "gitlab"
	p.RemoteProjectID = repo.ID
	p.RemoteURL = repo.HTTPCloneURL
	if repo.DefaultBranch != "" {
		p.DefaultBranch = repo.DefaultBranch
	}
	if err := s.st.SaveProject(project, p); err != nil {
		log.Printf("[remote-adopt] %s: persist RemoteProjectID failed: %v", project, err)
		return
	}
	log.Printf("[remote-adopt] %s: adopted gitlab project %s (%s) from origin of task %s", project, repo.ID, repo.PathWithNamespace, t.ID)
	s.bindDefaultRunner(ctx, project, p)
}

// originAdoptAuthorizedNamespaceEnv lists path-with-namespace prefixes the
// operator explicitly trusts for adoption (comma-separated full namespaces,
// e.g. "gao,platform/sandbox"). It is an escape hatch for deployments where
// the platform does not pre-create the remote but provisioned namespaces are
// tightly controlled; an empty value disables the escape hatch.
const originAdoptAuthorizedNamespaceEnv = "MULTIGENT_GITLAB_ADOPT_NAMESPACE_ALLOWLIST"

// originAdoptAuthorized decides whether an origin path observed in the
// agent-controlled worktree may be adopted (persisting RemoteProjectID and
// binding the default runner). Origin is attacker-writable input, so it only
// authorizes when it matches an independent platform record:
//
//   - the project's CloneURL (the UI create-repo flow persists the platform
//     -created repository's clean clone URL before the init task starts), or
//   - the operator allowlist env above (explicit namespace trust).
//
// RemoteURL is NOT sufficient even though handleGitLabCreateProject's own
// response handling writes it too: handlePutProject accepts remoteUrl from
// the client, so the field cannot distinguish platform record from a value
// that merely echoes the agent's chosen origin. Only CloneURL must stay
// out of the PUT path for this check to remain sound (see A2: the create-repo
// endpoint persists the platform identity server-side).
func originAdoptAuthorized(p *entity.Project, originPath string) bool {
	if p == nil || strings.TrimSpace(originPath) == "" {
		return false
	}
	if expected := cloneURLProjectPath(p.CloneURL); expected != "" && strings.EqualFold(expected, originPath) {
		return true
	}
	for _, ns := range strings.Split(os.Getenv(originAdoptAuthorizedNamespaceEnv), ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if originPath == ns || strings.HasPrefix(originPath, ns+"/") {
			return true
		}
	}
	return false
}

// cloneURLProjectPath extracts the path-with-namespace from a platform-record
// clone/web URL. Uses the shared origin parser with a nil host (no host
// filtering — the caller already resolved the GitLab host, and the record was
// produced by the platform's own API response).
func cloneURLProjectPath(recorded string) string {
	return gitlabProjectPathFromURL(recorded, nil)
}

// initWorktreeOriginURL reads the origin remote URL of the worktree/repo the
// init task ran in. Prefers the task's WorktreeDir; falls back to the project
// repo path.
func (s *Server) initWorktreeOriginURL(project string, t *entity.Task) string {
	candidates := []string{strings.TrimSpace(t.WorktreeDir)}
	if s.st != nil {
		if p, err := s.st.Project(project); err == nil && p != nil && strings.TrimSpace(p.Repo) != "" {
			candidates = append(candidates, strings.TrimSpace(p.Repo))
		}
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
		if err != nil {
			continue
		}
		if u := strings.TrimSpace(string(out)); u != "" {
			return u
		}
	}
	return ""
}

// gitlabProjectPathFromURL extracts the path-with-namespace from a clone URL
// when the URL's host matches the GitLab host's base host (any scheme or port
// spelling — agents reach the same GitLab via host.docker.internal or the LAN
// address). Returns "" for URLs on other hosts.
func gitlabProjectPathFromURL(cloneURL string, host *codehost.GitLabHost) string {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return ""
	}
	u, err := url.Parse(cloneURL)
	if err != nil || u.Host == "" || u.Path == "" {
		return ""
	}
	baseHost := ""
	if host != nil {
		baseHost = gitlabBaseHost(host)
	}
	if baseHost != "" && !sameHostIgnorePortSpellings(u.Hostname(), baseHost) {
		return ""
	}
	p := strings.TrimPrefix(u.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	if p == "" {
		return ""
	}
	return p
}

// gitlabBaseHost extracts the hostname from the host's configured base URL.
func gitlabBaseHost(host *codehost.GitLabHost) string {
	if host == nil {
		return ""
	}
	raw := host.BaseURL()
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

// sameHostIgnorePortSpellings compares hostnames case-insensitively. Port
// spelling differences are irrelevant because callers pass hostnames. Loopback
// and container-host aliases all denote "the same GitLab server reached by a
// different spelling" — the agent may push to any reachable alias of the host
// the connection pins.
func sameHostIgnorePortSpellings(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	aliases := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "host.docker.internal": true, "host.orb.internal": true}
	return aliases[a] && aliases[b]
}

// adoptRemoteIfNeededAfterStep is the hook installed in the runtime
// step-complete path: it fires only for the initialization workflow's sync
// step. All context deadlines are short — adoption must not delay the step
// response.
func (s *Server) adoptRemoteIfNeededAfterStep(project string, t *entity.Task, stepStatus, definitionID, stepID string) {
	if s == nil || t == nil {
		return
	}
	if definitionID != workflowstore.ProjectInitializationWorkflowID || stepID != "sync" {
		return
	}
	// The task record is reused across workflow steps: by the time this hook
	// runs, a mid-workflow transition has usually already reset it to pending
	// for the next step, so its status says nothing about the step outcome.
	// The step instance status passed by the caller is the authoritative
	// completion signal; "failed" is rejected to avoid adopting a remote the
	// agent never pushed to.
	if strings.EqualFold(strings.TrimSpace(stepStatus), "failed") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.adoptRemoteAfterSync(ctx, project, t)
}
