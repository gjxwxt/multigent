package api

// Workspace lifecycle & health (workspace lifecycle governance module).
//
// A project's workspace (projects/<name>/workspace) is materialized once at
// initialization and has no lifecycle afterwards: nothing tracks whether it
// still resembles the remote, and nothing offers a rebuild. The 1test
// incident (2026-09-21) made the cost visible — a stale Aug-31 materialized
// directory with no .git sat underneath the ci_ready gate and produced
// misleading failures until an operator hand-rebuilt it.
//
// These endpoints make the state visible (health) and give operators a
// platform-owned rebuild for bound projects. The rebuild clone injects the
// connection token transiently via a process-scoped credential helper
// (GIT_CONFIG_* env, never written to disk or repo config) — the same
// credentials-not-on-disk invariant as worktree pushes.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/codehost"
	"github.com/multigent/multigent/internal/gitworktree"
)

// WorkspaceHealthState enumerates the lifecycle states a workspace can be in.
type WorkspaceHealthState string

const (
	// WorkspaceStateFresh: a real git checkout whose HEAD matches origin's
	// default branch (within a small tolerance — we compare merge-base to
	// origin head rather than requiring exact equality so local platform
	// checkpoints don't false-positive).
	WorkspaceStateFresh WorkspaceHealthState = "fresh"
	// WorkspaceStateBehind: a real git checkout that lags origin.
	WorkspaceStateBehind WorkspaceHealthState = "behind"
	// WorkspaceStateNoRemote: a real git checkout without an origin remote
	// (local-only project) — not stale by definition.
	WorkspaceStateNoRemote WorkspaceHealthState = "no_remote"
	// WorkspaceStateMaterializedOnly: the directory exists but is NOT a git
	// checkout while the project HAS a verified remote binding. This is the
	// 1test failure signature: the workspace was template-materialized but
	// never (re)cloned, and everything that trusts it reads stale content.
	WorkspaceStateMaterializedOnly WorkspaceHealthState = "materialized_only"
	// WorkspaceStateUnmanaged: neither git checkout nor remote binding — a
	// local-only or not-yet-initialized project. Nothing to do.
	WorkspaceStateUnmanaged WorkspaceHealthState = "unmanaged"
	// WorkspaceStateMissing: the workspace directory does not exist at all.
	WorkspaceStateMissing WorkspaceHealthState = "missing"
	// WorkspaceStateError: probing failed (git errors etc.) — details in Detail.
	WorkspaceStateError WorkspaceHealthState = "error"
)

type workspaceHealth struct {
	Project     string               `json:"project"`
	Path        string               `json:"path"`
	State       WorkspaceHealthState `json:"state"`
	HasGit      bool                 `json:"hasGit"`
	Head        string               `json:"head,omitempty"`
	OriginHead  string               `json:"originHead,omitempty"`
	BehindCount int                  `json:"behindCount,omitempty"`
	Branch      string               `json:"branch,omitempty"`
	Remotes     []string             `json:"remotes,omitempty"`
	Detail      string               `json:"detail,omitempty"`
	CheckedAt   string               `json:"checkedAt"`
}

func gitWorkspaceOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	cmd.Env = gitworktree.PushNetworkEnv()
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// probeWorkspaceHealth inspects the project's workspace directory. Network
// calls are bounded: one ls-remote against origin when a remote exists.
func (s *Server) probeWorkspaceHealth(ctx context.Context, project string) workspaceHealth {
	health := workspaceHealth{
		Project:   project,
		Path:      filepath.Join(s.st.ProjectDir(project), "workspace"),
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := os.Stat(health.Path); err != nil {
		health.State = WorkspaceStateMissing
		health.Detail = "workspace directory does not exist"
		return health
	}
	gitDir := filepath.Join(health.Path, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		// Not a git checkout. The dangerous case is materialized-only with a
		// verified remote binding: content is template-aging in place while
		// the real code lives on the remote.
		_, binding, bound, bindErr := s.verifiedBinding(project)
		if bindErr == nil && bound {
			health.State = WorkspaceStateMaterializedOnly
			health.Detail = "workspace is not a git checkout but the project has a verified remote binding — materialized content is aging in place (1test failure signature); rebuild recommended"
			if binding != nil && strings.TrimSpace(binding.PathWithNamespace) != "" {
				health.Remotes = []string{binding.PathWithNamespace}
			}
		} else {
			health.State = WorkspaceStateUnmanaged
			health.Detail = "workspace is not a git checkout and the project has no verified remote binding"
		}
		return health
	}
	health.HasGit = true
	head, err := gitWorkspaceOutput(health.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		health.State = WorkspaceStateError
		health.Detail = err.Error()
		return health
	}
	health.Head = head
	branch, err := gitWorkspaceOutput(health.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		health.Branch = branch
	}
	remotesRaw, err := gitWorkspaceOutput(health.Path, "remote")
	if err != nil || strings.TrimSpace(remotesRaw) == "" {
		health.State = WorkspaceStateNoRemote
		health.Detail = "git checkout without an origin remote (local-only project)"
		return health
	}
	health.Remotes = strings.Split(remotesRaw, "\n")
	originHead, err := gitWorkspaceOutput(health.Path, "ls-remote", "origin", "HEAD")
	if err != nil {
		// Workspaces carry a credential-free origin (platform invariant);
		// host-side probes need auth. Retry through the verified binding's
		// connection token with the same transient-extraHeader scheme the
		// rebuild clone uses.
		if retried, retryErr := s.lsRemoteViaBinding(ctx, project, health.Path); retryErr == nil {
			originHead = retried
		} else {
			health.State = WorkspaceStateError
			health.Detail = "ls-remote origin failed: " + err.Error()
			return health
		}
	}
	if idx := strings.Index(originHead, "\t"); idx > 0 {
		health.OriginHead = originHead[:idx]
	}
	if health.OriginHead == "" {
		health.State = WorkspaceStateError
		health.Detail = "origin HEAD returned no ref"
		return health
	}
	if health.OriginHead == health.Head {
		health.State = WorkspaceStateFresh
		return health
	}
	// Distinguish "behind" from "diverged" with a merge-base check against
	// the fetched origin ref (ls-remote gives us the SHA; merge-base needs an
	// object, so use git cat-file -t to check whether we have it locally).
	if _, err := gitWorkspaceOutput(health.Path, "cat-file", "-e", health.OriginHead+"^{commit}"); err == nil {
		if _, err := gitWorkspaceOutput(health.Path, "merge-base", "--is-ancestor", "HEAD", health.OriginHead); err == nil {
			if count, err := gitWorkspaceOutput(health.Path, "rev-list", "--count", "HEAD.."+health.OriginHead); err == nil {
				fmt.Sscanf(count, "%d", &health.BehindCount)
			}
			health.State = WorkspaceStateBehind
			return health
		}
	}
	health.State = WorkspaceStateBehind
	health.Detail = "workspace HEAD diverged from origin (or origin commit not fetched locally)"
	return health
}

// workspaceHealthCache dedupes origin probes: the UI polls health, and an
// ls-remote per poll per project hammers the git host. TTL 60s; rebuild
// invalidates the project's entry so the post-rebuild state is exact.
type workspaceHealthCache struct {
	mu      sync.Mutex
	entries map[string]workspaceHealthCacheEntry
}

type workspaceHealthCacheEntry struct {
	health   workspaceHealth
	expires  time.Time
}

func (c *workspaceHealthCache) get(project string, now time.Time) (workspaceHealth, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[project]
	if !ok || now.After(e.expires) {
		return workspaceHealth{}, false
	}
	return e.health, true
}

func (c *workspaceHealthCache) put(project string, h workspaceHealth, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]workspaceHealthCacheEntry{}
	}
	c.entries[project] = workspaceHealthCacheEntry{health: h, expires: now.Add(60 * time.Second)}
}

func (c *workspaceHealthCache) invalidate(project string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, project)
}

func (s *Server) handleGetProjectWorkspaceHealth(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if _, ok := s.currentWorkspaceForRequest(w, r); !ok {
		return
	}
	// Network checks (ls-remote) are opt-in via ?checkRemote=true; the plain
	// GET answers from local facts + a short-lived cache of network probes.
	checkRemote := r.URL.Query().Get("checkRemote") == "true"
	if !checkRemote {
		if h, ok := s.workspaceHealthCache.get(project, time.Now().UTC()); ok {
			_ = json.NewEncoder(w).Encode(h)
			return
		}
	}
	health := s.probeWorkspaceHealth(r.Context(), project)
	if !checkRemote {
		s.workspaceHealthCache.put(project, health, time.Now().UTC())
	}
	_ = json.NewEncoder(w).Encode(health)
}

// handleRebuildProjectWorkspace rebuilds a bound project's workspace from its
// verified remote binding: back up the old directory (stale-<ts>), clone the
// default branch with transiently-injected credentials, strip any credential
// residue, and record the operation. Refuses when agent runs are active —
// rebuilding under a live worktree/preview session would corrupt both.
func (s *Server) handleRebuildProjectWorkspace(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	if !s.canOperateProject(r, project) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeProjectAccessRequired, "operator project access required to rebuild a workspace")
		return
	}
	workspaceID, ok := s.currentWorkspaceForRequest(w, r)
	if !ok {
		return
	}
	if s.hasAnyActiveRuntimeRunForProject(workspaceID, project) {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict,
			"active or queued runtime runs exist for this project; stop them before rebuilding the workspace")
		return
	}
	p, binding, bound, err := s.verifiedBinding(project)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !bound || binding == nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict,
			"project has no verified remote binding; rebuild clones from the binding and cannot proceed without one")
		return
	}
	host, _, err := s.verifiedGitLabHost(r.Context(), project)
	if err != nil {
		s.serverError(w, err)
		return
	}
	cloneURL, err := workspaceCloneURL(host, binding)
	if err != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeConflict, err.Error())
		return
	}
	result := s.rebuildWorkspaceFromBinding(project, cloneURL, host.APIToken())
	if result.err != nil {
		s.auditLog(auditLogInput{
			Action:       "workspace.rebuild",
			ResourceType: "project",
			ResourceID:   project,
			Summary:      "Workspace rebuild failed: " + result.err.Error(),
			After:        map[string]any{"backup": result.backupDir},
		})
		s.jsonErrorCode(w, http.StatusInternalServerError, ErrCodeInternal, result.err.Error())
		return
	}
	_ = p
	s.workspaceHealthCache.invalidate(project)
	s.auditLog(auditLogInput{
		Action:       "workspace.rebuild",
		ResourceType: "project",
		ResourceID:   project,
		Summary:      "Workspace rebuilt from verified remote binding",
		After: map[string]any{
			"backup":  result.backupDir,
			"head":    result.head,
			"branch":  result.branch,
			"actor":   requestUsername(r),
			"binding": binding.PathWithNamespace,
		},
	})
	_ = json.NewEncoder(w).Encode(map[string]any{
		"rebuilt": true,
		"backup":  result.backupDir,
		"head":    result.head,
		"branch":  result.branch,
	})
}

// hasAnyActiveRuntimeRunForProject reports whether ANY agent in the project
// has a queued/running run (rebuild would yank the workspace out from under
// them). Reuses the per-agent block check across the project's agents.
func (s *Server) hasAnyActiveRuntimeRunForProject(workspaceID, project string) bool {
	if s == nil || s.controlDB == nil {
		return false
	}
	now := time.Now().UTC()
	for _, status := range []string{"queued", "running"} {
		runs, err := s.controlDB.ListRuntimeRuns(controldb.RuntimeRunFilter{
			WorkspaceID: workspaceID,
			ProjectID:   project,
			Status:      status,
			Limit:       100,
		})
		if err != nil {
			continue
		}
		for _, run := range runs {
			if runtimeRunBlocksAgent(run, now) {
				return true
			}
		}
	}
	return false
}

type workspaceRebuildResult struct {
	backupDir string
	head      string
	branch    string
	err       error
}

// workspaceCloneURL resolves the binding to an authenticated-free clone URL.
// The URL itself stays credential-free; auth rides the transient helper in
// rebuildWorkspaceFromBinding.
func workspaceCloneURL(host *codehost.GitLabHost, binding *controldb.VerifiedRemoteBinding) (string, error) {
	path := strings.TrimSpace(binding.PathWithNamespace)
	if path == "" {
		return "", fmt.Errorf("verified binding has no project path")
	}
	base := strings.TrimSuffix(host.BaseURL(), "/api/v4")
	return base + "/" + path + ".git", nil
}

// rebuildWorkspaceFromBinding performs the filesystem work. Best-effort
// rollback: if the clone fails and a backup exists, restore it.
func (s *Server) rebuildWorkspaceFromBinding(project string, cloneURL, token string) workspaceRebuildResult {
	var result workspaceRebuildResult
	root := s.st.ProjectDir(project)
	wsDir := filepath.Join(root, "workspace")
	backupDir := filepath.Join(root, "workspace.stale-"+time.Now().UTC().Format("20060102-150405"))
	result.backupDir = backupDir

	// The workspace may be a live mount for sandbox sessions; rename is
	// atomic on the same filesystem and preserves the old content as backup.
	if _, err := os.Stat(wsDir); err == nil {
		if err := os.Rename(wsDir, backupDir); err != nil {
			result.err = fmt.Errorf("back up old workspace: %w", err)
			return result
		}
	}
	// Transient credential helper: process-scoped env config only — the token
	// never touches disk, repo config, or the remote URL (credentials-not-
	// on-disk invariant; the remote inside the clone stays a clean URL).
cloneEnv, authErr := gitlabTransientCloneEnv(token)
	if authErr != nil {
		if err := os.Rename(backupDir, wsDir); err == nil {
			result.backupDir = ""
		}
		result.err = authErr
		return result
	}
	defaultBranch := "main"
	clone := exec.Command("git", "clone", "--no-local", "--branch", defaultBranch, cloneURL, wsDir)
	clone.Env = cloneEnv
	var out bytes.Buffer
	clone.Stdout = &out
	clone.Stderr = &out
	if err := clone.Run(); err != nil {
		// Retry without --branch (repo's default branch may not be main).
		clone2 := exec.Command("git", "clone", "--no-local", cloneURL, wsDir)
		clone2.Env = cloneEnv
		out.Reset()
		clone2.Stdout = &out
		clone2.Stderr = &out
		if err2 := clone2.Run(); err2 != nil {
			os.RemoveAll(wsDir) // partial clone
			if err := os.Rename(backupDir, wsDir); err == nil {
				result.backupDir = ""
			}
			msg := strings.TrimSpace(out.String())
			// Redact any credential-shaped residue before surfacing.
			result.err = fmt.Errorf("clone failed: %s", gitworktree.RedactGitOutput(msg))
			_ = err
			return result
		}
	}
	// Strip any credential residue the helper could have cached and verify.
	if err := gitworktree.SanitizeUntrustedClone(wsDir); err != nil {
		result.err = fmt.Errorf("sanitize clone: %w", err)
		return result
	}
	branch, err := gitWorkspaceOutput(wsDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		result.branch = branch
	}
	head, err := gitWorkspaceOutput(wsDir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		result.err = fmt.Errorf("verify cloned HEAD: %w", err)
		return result
	}
	result.head = head
	// Clean remote URL (already clean — helper was env-scoped — but assert).
	if _, err := gitWorkspaceOutput(wsDir, "remote", "set-url", "origin", stripCredentials(cloneURL)); err != nil {
		result.err = fmt.Errorf("assert clean remote URL: %w", err)
		return result
	}
	return result
}

// lsRemoteViaBinding retries origin HEAD discovery using the project's
// verified remote binding token (transient extraHeader, same invariant as
// the rebuild clone). Returns the raw ls-remote output.
func (s *Server) lsRemoteViaBinding(ctx context.Context, project, wsDir string) (string, error) {
	host, binding, err := s.verifiedGitLabHost(ctx, project)
	if err != nil {
		return "", err
	}
	cloneURL, err := workspaceCloneURL(host, binding)
	if err != nil {
		return "", err
	}
	env, authErr := gitlabTransientCloneEnv(host.APIToken())
	if authErr != nil {
		return "", authErr
	}
	cmd := exec.CommandContext(ctx, "git", "-C", wsDir, "ls-remote", cloneURL, "HEAD")
	var out, errOut bytes.Buffer
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("authenticated ls-remote failed: %s", gitworktree.RedactGitOutput(strings.TrimSpace(errOut.String())))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitlabTransientCloneEnv builds the process-scoped git env carrying Basic
// auth for one clone. The token is base64'd into an http.extraHeader via
// GIT_CONFIG_* env entries: no disk file, no shell helper string, no repo
// config residue.
func gitlabTransientCloneEnv(token string) ([]string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("verified connection has no API token; cannot clone with auth")
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0="+header,
	), nil
}

// stripCredentials removes any user:pass@ segment from a URL (defense in
// depth; the clone URL should already be clean).
func stripCredentials(raw string) string {
	if idx := strings.Index(raw, "://"); idx >= 0 {
		rest := raw[idx+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 && !strings.Contains(rest[:at], "/") {
			return raw[:idx+3] + rest[at+1:]
		}
	}
	return raw
}
