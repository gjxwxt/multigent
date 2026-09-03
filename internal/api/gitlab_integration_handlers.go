package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
)

func (s *Server) resolveGitLabHost(connectionID string) (*codehost.GitLabHost, string, error) {
	host, connID, err := s.resolveCodeHost("gitlab", connectionID)
	if err != nil {
		return nil, "", err
	}
	gitlab, ok := host.(*codehost.GitLabHost)
	if !ok {
		return nil, "", fmt.Errorf("resolved code host is not GitLab")
	}
	return gitlab, connID, nil
}

func (s *Server) resolveGitHubHost(connectionID string) (*codehost.GitHubHost, string, error) {
	host, connID, err := s.resolveCodeHost("github", connectionID)
	if err != nil {
		return nil, "", err
	}
	github, ok := host.(*codehost.GitHubHost)
	if !ok {
		return nil, "", fmt.Errorf("resolved code host is not GitHub")
	}
	return github, connID, nil
}

func (s *Server) resolveCodeHost(provider, connectionID string) (codehost.CodeHost, string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "gitlab" && provider != "github" {
		return nil, "", fmt.Errorf("unsupported code host provider %q", provider)
	}
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return nil, "", err
	}

	connections, err := s.controlDB.ListConnections(controldb.ConnectionFilter{
		WorkspaceID: workspaceID,
		Provider:    provider,
	})
	if err != nil || len(connections) == 0 {
		return nil, "", fmt.Errorf("no %s connection configured in this workspace", provider)
	}

	// Connection selection: explicit ID wins; otherwise the provider's
	// marked default; otherwise the newest-updated connection as a final
	// fallback for legacy workspaces that never picked one.
	conn := connections[0]
	for _, c := range connections {
		if c.IsDefault {
			conn = c
			break
		}
	}
	if connectionID != "" {
		found := false
		for _, c := range connections {
			if c.ID == connectionID {
				conn = c
				found = true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("connection %s not found", connectionID)
		}
	}

	values := map[string]string{}
	secret, ok, err := s.controlDB.ConnectionSecret(conn.ID)
	if err != nil {
		return nil, "", fmt.Errorf("read connection secret: %w", err)
	}
	if ok {
		opened, err := openConnectionSecret(secret)
		if err != nil {
			return nil, "", fmt.Errorf("decrypt connection secret: %w", err)
		}
		values = opened
	}
	if strings.TrimSpace(values["apiKey"]) == "" && conn.AuthType == ConnectionAuthOAuth2 {
		if token, tokenErr := s.oauthAccessTokenForConnection(conn, values); tokenErr == nil {
			values["apiKey"] = token
		} else {
			return nil, "", fmt.Errorf("resolve %s OAuth token: %w", provider, tokenErr)
		}
	}

	baseURL := strings.TrimSpace(values["baseUrl"])
	if baseURL == "" {
		profile := map[string]any{}
		_ = json.Unmarshal([]byte(conn.ProfileJSON), &profile)
		if v, ok := profile["baseUrl"].(string); ok {
			baseURL = strings.TrimSpace(v)
		}
	}
	token := strings.TrimSpace(values["apiKey"])
	if provider == "gitlab" {
		if baseURL == "" {
			baseURL = "https://gitlab.com"
		}
		return codehost.NewGitLabHost(codehost.GitLabConfig{BaseURL: baseURL, Token: token}), conn.ID, nil
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return codehost.NewGitHubHost(codehost.GitHubConfig{BaseURL: baseURL, Token: token}), conn.ID, nil
}

func (s *Server) handleGitLabStatus(w http.ResponseWriter, r *http.Request) {
	host, connID, err := s.resolveGitLabHost(r.URL.Query().Get("connectionId"))
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connected": false,
			"error":     err.Error(),
		})
		return
	}

	ctx := r.Context()
	if err := host.CheckConnection(ctx); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connected":    false,
			"connectionId": connID,
			"error":        err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"connected":    true,
		"connectionId": connID,
		"provider":     "gitlab",
	})
}

func (s *Server) handleGitLabNamespaces(w http.ResponseWriter, r *http.Request) {
	host, _, err := s.resolveGitLabHost(r.URL.Query().Get("connectionId"))
	if err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}

	namespaces, err := host.ListNamespaces(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"namespaces": namespaces,
	})
}

type gitlabCreateProjectReq struct {
	ConnectionID string `json:"connectionId"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	NamespaceID  int64  `json:"namespaceId"`
	Visibility   string `json:"visibility"`
	Description  string `json:"description"`
}

func (s *Server) handleGitLabCreateProject(w http.ResponseWriter, r *http.Request) {
	var body gitlabCreateProjectReq
	if err := s.readJSON(w, r, &body); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeInvalidJSON, "invalid JSON")
		return
	}

	host, connID, err := s.resolveGitLabHost(body.ConnectionID)
	if err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}

	repo, err := host.CreateRepository(r.Context(), codehost.CreateRepoRequest{
		Name:        body.Name,
		Path:        body.Path,
		NamespaceID: body.NamespaceID,
		Visibility:  body.Visibility,
		Description: body.Description,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"connectionId": connID,
		"repository":   repo,
	})
}

type mergeTaskMRReq struct {
	CommitMessage string `json:"commitMessage"`
}

func (s *Server) handleMergeTaskMR(w http.ResponseWriter, r *http.Request) {
	projectName := r.PathValue("name")
	taskID := r.PathValue("id")

	p, err := s.st.Project(projectName)
	if err != nil {
		if isNotFoundErr(err) {
			s.jsonErrorCode(w, http.StatusNotFound, ErrCodeProjectNotFound, "project not found")
			return
		}
		s.serverError(w, err)
		return
	}

	var body mergeTaskMRReq
	_ = s.readJSON(w, r, &body)

	// Fetch task
	task, agentName, err := s.findTaskInProject(projectName, taskID)
	if err != nil {
		s.jsonErrorCode(w, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("task %s not found: %v", taskID, err))
		return
	}

	if err := s.prepareTaskDelivery(r, projectName, task, body.CommitMessage); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.ts.PersistTask(projectName, agentName, task); err != nil {
		s.serverError(w, fmt.Errorf("persist task delivery state: %w", err))
		return
	}
	s.cleanupTaskDeliveryArtifacts(projectName, taskID)

	mode := "local"
	response := map[string]any{
		"ok":     true,
		"mode":   mode,
		"mrIid":  task.RemoteMRIID,
		"status": "merged",
	}
	if p.RemoteProvider == "gitlab" || p.RemoteProvider == "github" {
		mode = p.RemoteProvider + "_remote"
		response["mode"] = mode
	} else {
		// Keep the legacy response field for local callers while the task
		// metadata remains the source of truth for both delivery modes.
		response["mergedSha"] = task.RemoteMRHeadSHA
	}

	_ = json.NewEncoder(w).Encode(response)
}
