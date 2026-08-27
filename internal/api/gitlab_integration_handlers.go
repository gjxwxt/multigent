package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/gitworktree"
)

func (s *Server) resolveGitLabHost(connectionID string) (*codehost.GitLabHost, string, error) {
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return nil, "", err
	}

	connections, err := s.controlDB.ListConnections(controldb.ConnectionFilter{
		WorkspaceID: workspaceID,
		Provider:    "gitlab",
	})
	if err != nil || len(connections) == 0 {
		return nil, "", fmt.Errorf("no GitLab connection configured in this workspace")
	}

	conn := connections[0]
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

	baseURL := strings.TrimSpace(values["baseUrl"])
	if baseURL == "" {
		profile := map[string]any{}
		_ = json.Unmarshal([]byte(conn.ProfileJSON), &profile)
		if v, ok := profile["baseUrl"].(string); ok {
			baseURL = strings.TrimSpace(v)
		}
	}
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	token := strings.TrimSpace(values["apiKey"])

	host := codehost.NewGitLabHost(codehost.GitLabConfig{
		BaseURL: baseURL,
		Token:   token,
	})
	return host, conn.ID, nil
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

	// 1. Remote GitLab Mode
	if p.RemoteProvider == "gitlab" && p.RemoteProjectID != "" && task.RemoteMRIID != "" {
		host, _, err := s.resolveGitLabHost(p.RemoteConnection)
		if err != nil {
			s.serverError(w, fmt.Errorf("resolve gitlab connection: %w", err))
			return
		}

		err = host.MergeMR(r.Context(), p.RemoteProjectID, task.RemoteMRIID, task.RemoteMRHeadSHA, body.CommitMessage)
		if err != nil {
			s.serverError(w, fmt.Errorf("merge gitlab mr %s: %w", task.RemoteMRIID, err))
			return
		}

		// Pull merged changes into local workspace
		mgr := gitworktree.NewManager()
		_ = mgr.SyncMain(p.Repo, p.DefaultBranch)

		task.RemoteMRState = "merged"
		_ = s.ts.PersistTask(projectName, agentName, task)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"mode":   "gitlab_remote",
			"mrIid":  task.RemoteMRIID,
			"status": "merged",
		})
		return
	}

	// 2. Pure Local Worktree Mode
	mgr := gitworktree.NewManager()
	branchToMerge := task.BranchName
	if branchToMerge == "" {
		branchToMerge = fmt.Sprintf("feature/%s", task.ID)
	}

	targetBranch := p.DefaultBranch
	if targetBranch == "" {
		targetBranch = "main"
	}

	mergedSHA, err := mgr.MergeBranchLocally(p.Repo, targetBranch, branchToMerge, body.CommitMessage)
	if err != nil {
		s.serverError(w, fmt.Errorf("local merge failed: %w", err))
		return
	}

	task.RemoteMRState = "merged"
	task.RemoteMRHeadSHA = mergedSHA
	_ = s.ts.PersistTask(projectName, agentName, task)

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"mode":      "local",
		"mergedSha": mergedSHA,
		"status":    "merged",
	})
}
