package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func bindingTimestamp() string { return time.Now().UTC().Format(time.RFC3339) }

// Verified remote binding service (P0.6): every external GitLab operation
// resolves its target through this file's helpers. The binding is written
// only by:
//
//   - platformCreateRemoteBinding  — the create-repo endpoint, from the
//     GitLab API response it just received;
//   - verifyProjectRemoteBinding   — the admin read-only verify endpoint,
//     from a live forge lookup.
//
// The project PUT can never create or alter a binding, and consumers refuse
// to act when no binding exists (fail-closed).

// checkConnectionUsable authorizes the caller to use the named platform
// connection for a forge write: admins pass by default; user-owned
// connections require ownership. connectionID may be empty (provider
// default) — there is nothing to authorize beyond the caller's own rights,
// so it returns true.
func (s *Server) checkConnectionUsable(w http.ResponseWriter, r *http.Request, connectionID string) bool {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return true
	}
	connection, ok, err := s.controlDB.ConnectionByID(connectionID)
	if err != nil {
		s.serverError(w, err)
		return false
	}
	if !ok {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "connection not found")
		return false
	}
	if !s.canReadConnection(r, connection, s.currentUser(r)) {
		s.jsonErrorCode(w, http.StatusForbidden, ErrCodeConnectionAccessRequired, "connection access required")
		return false
	}
	return true
}

// platformCreateRemoteBinding persists the verified binding from the
// create-repository flow: the caller must have just received repo from the
// forge via the platform's own connection (connID), and must have already
// verified the caller's project-management rights.
func (s *Server) platformCreateRemoteBinding(workspaceID, projectName, connID string, repo *codehost.Repository) error {
	b := controldb.VerifiedRemoteBinding{
		WorkspaceID:       workspaceID,
		ProjectID:         projectName,
		Provider:          "gitlab",
		ConnectionID:      connID,
		RemoteProjectID:   repo.ID,
		PathWithNamespace: repo.PathWithNamespace,
		VerifiedAt:        bindingTimestamp(),
		Source:            controldb.BindingSourcePlatformCreate,
	}
	if err := s.controlDB.UpsertVerifiedRemoteBinding(b); err != nil {
		return fmt.Errorf("persist verified remote binding for %s: %w", projectName, err)
	}
	return nil
}

// verifyProjectRemoteBinding runs the admin-initiated read-only verification:
// it looks the remote up live through the pinned connection and persists the
// binding only when the forge confirms the project exists at that identity.
// It never writes to the forge. The connection is chosen by connectionID when
// given, else by the project's current pin — the operator must be workspace
// admin, so an explicit connectionID is a deliberate choice, not a forgery
// vector.
//
// hintURL is the third way to name the remote, for bind-remote initialization
// where the project record carries no platform-verified path yet (the agent
// clones in the sandbox; only the operator knows the clone URL). The hint is
// a HINT, not authorization: it only extracts a path-with-namespace, which
// must then be confirmed by the live forge lookup through the pinned
// connection, and it is rejected outright when its host does not match the
// resolved connection's GitLab base host.
func (s *Server) verifyProjectRemoteBinding(ctx context.Context, workspaceID, projectName, connectionID, hintURL string) (*controldb.VerifiedRemoteBinding, error) {
	p, err := s.st.Project(projectName)
	if err != nil {
		return nil, err
	}
	host, connID, err := s.resolveGitLabHost(connectionID)
	if err != nil {
		return nil, fmt.Errorf("resolve gitlab connection: %w", err)
	}
	path := strings.TrimSpace(p.RemoteAdoptPath)
	if path == "" {
		path = gitlabProjectPathFromURL(p.CloneURL, nil)
	}
	if path == "" {
		path = gitlabProjectPathFromURL(hintURL, host)
	}
	if path == "" {
		return nil, fmt.Errorf("project %s has no platform-recorded remote path to verify", projectName)
	}
	repo, err := host.RepositoryByProjectPath(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("verify remote %s: %w", path, err)
	}
	b := controldb.VerifiedRemoteBinding{
		WorkspaceID:       workspaceID,
		ProjectID:         projectName,
		Provider:          "gitlab",
		ConnectionID:      connID,
		RemoteProjectID:   repo.ID,
		PathWithNamespace: repo.PathWithNamespace,
		VerifiedAt:        bindingTimestamp(),
		Source:            controldb.BindingSourceExplicitVerify,
	}
	if err := s.controlDB.UpsertVerifiedRemoteBinding(b); err != nil {
		return nil, fmt.Errorf("persist verified remote binding for %s: %w", projectName, err)
	}
	return &b, nil
}

// verifiedBinding loads the project and its verified binding. ok=false means
// the project has no verified remote: every external GitLab operation must
// refuse (fail-closed) in that case.
func (s *Server) verifiedBinding(projectName string) (*entity.Project, *controldb.VerifiedRemoteBinding, bool, error) {
	p, err := s.st.Project(projectName)
	if err != nil {
		return nil, nil, false, err
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return nil, nil, false, err
	}
	binding, ok, err := s.controlDB.VerifiedRemoteBindingFor(workspaceID, projectName)
	if err != nil {
		return nil, nil, false, err
	}
	return p, binding, ok, nil
}

// verifiedGitLabHost resolves the GitLab host from the binding's pinned
// connection — never from the client-writable RemoteConnection field — and
// cross-checks that the resolved connection matches the binding: a rebound
// or swapped connection ID cannot silently redirect platform operations at
// a different forge host.
func (s *Server) verifiedGitLabHost(ctx context.Context, projectName string) (*codehost.GitLabHost, *controldb.VerifiedRemoteBinding, error) {
	p, binding, ok, err := s.verifiedBinding(projectName)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("project %s has no verified remote binding; run the admin remote verify flow first", projectName)
	}
	host, connID, err := s.resolveGitLabHost(binding.ConnectionID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve verified connection %s: %w", binding.ConnectionID, err)
	}
	if connID != binding.ConnectionID {
		return nil, nil, fmt.Errorf("verified connection %s for project %s no longer resolves (got %s); re-run remote verify", binding.ConnectionID, projectName, connID)
	}
	_ = p
	return host, binding, nil
}

// adoptRemoteBindingAfterSync is the init-flow adoption: after a verified
// platform create (binding exists) the agent's worktree origin must match the
// binding's path before the platform writes anything else. Called from the
// workflow step hook, not from any client request.
func (s *Server) adoptRemoteBindingAfterSync(projectName string, binding *controldb.VerifiedRemoteBinding, originPath string) bool {
	if binding == nil || strings.TrimSpace(originPath) == "" {
		return false
	}
	return strings.EqualFold(binding.PathWithNamespace, strings.TrimSpace(originPath))
}

// handleProjectRemoteVerify is the admin-initiated read-only verification of
// a project's remote (P0.6-5): it looks the project's platform-recorded
// remote path up live on the forge through the chosen connection and, on a
// match, persists the verified remote binding (source explicit-verify).
// Existing projects migrated before bindings existed stay untrusted until an
// administrator runs this; until then every external GitLab operation on the
// project fails closed.
func (s *Server) handleProjectRemoteVerify(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentWorkspaceAdmin(w, r) {
		return
	}
	projectName := strings.TrimSpace(r.PathValue("name"))
	if _, err := s.st.Project(projectName); err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}
	var body struct {
		ConnectionID string `json:"connectionId"`
	}
	_ = s.readJSON(w, r, &body) // body optional; empty connection = project pin
	binding, err := s.verifyProjectRemoteBinding(r.Context(), mustWorkspaceID(r, s), projectName, body.ConnectionID, "")
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, err.Error())
			return
		}
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}
	s.auditLog(auditLogInput{
		Action:       "project.remote.verify",
		ResourceType: "project",
		ResourceID:   projectName,
		Summary:      "Verified remote binding (read-only forge lookup)",
		After: map[string]any{
			"connectionId":    binding.ConnectionID,
			"remoteProjectId": binding.RemoteProjectID,
			"path":            binding.PathWithNamespace,
			"source":          binding.Source,
		},
		Request: r,
	})
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "binding": binding})
}

// handleProjectInitRemoteVerify lets the project initialization flow (bind an
// existing remote repo) establish the verified remote binding without a
// workspace admin in the loop. The caller must manage the project (the same
// right that lets them start the initialization task), and the verification
// itself stays read-only on the forge: the clone URL supplied by the init
// form is only a path hint — authorization comes from the live lookup through
// the pinned connection, which must actually contain a repository at that
// path. Without this endpoint every bind-remote project would stay
// fail-closed for ci_ready pipeline evidence and remote adoption until an
// administrator ran the workspace-admin verify by hand.
func (s *Server) handleProjectInitRemoteVerify(w http.ResponseWriter, r *http.Request) {
	projectName := strings.TrimSpace(r.PathValue("name"))
	if !s.checkProjectManager(w, r, projectName) {
		return
	}
	p, err := s.st.Project(projectName)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}
	var body struct {
		ConnectionID string `json:"connectionId"`
		CloneURL     string `json:"cloneUrl"`
	}
	_ = s.readJSON(w, r, &body)
	hint := strings.TrimSpace(body.CloneURL)
	if hint == "" {
		hint = strings.TrimSpace(p.CloneURL)
	}
	binding, err := s.verifyProjectRemoteBinding(r.Context(), mustWorkspaceID(r, s), projectName, body.ConnectionID, hint)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, err.Error())
			return
		}
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}
	s.auditLog(auditLogInput{
		Action:       "project.remote.verify",
		ResourceType: "project",
		ResourceID:   projectName,
		Summary:      "Verified remote binding during project initialization (read-only forge lookup)",
		After: map[string]any{
			"connectionId":    binding.ConnectionID,
			"remoteProjectId": binding.RemoteProjectID,
			"path":            binding.PathWithNamespace,
			"source":          binding.Source,
		},
		Request: r,
	})
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "binding": binding})
}

// mustWorkspaceID extracts the request-scoped workspace; the verify handler
// has already authenticated the caller by the time this runs.
func mustWorkspaceID(r *http.Request, s *Server) string {
	id, err := s.currentWorkspaceID()
	if err != nil {
		return ""
	}
	_ = r
	return id
}

var _ = json.Marshal
