package codehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitLabConfig holds connection parameters for GitLab.
type GitLabConfig struct {
	BaseURL    string // e.g. "https://gitlab.com" or "http://gitlab.internal:8080"
	Token      string // Personal access token
	HTTPClient *http.Client
}

// GitLabHost implements CodeHost for GitLab.
type GitLabHost struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewGitLabHost creates a new GitLab CodeHost adapter.
func NewGitLabHost(cfg GitLabConfig) *GitLabHost {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/api/v4") {
		baseURL += "/api/v4"
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	return &GitLabHost{
		baseURL: baseURL,
		token:   strings.TrimSpace(cfg.Token),
		client:  client,
	}
}

func (g *GitLabHost) Provider() string {
	return "gitlab"
}

func (g *GitLabHost) CheckConnection(ctx context.Context) error {
	req, err := g.newRequest(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab connection check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gitlab connection check failed with status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

type gitlabNamespace struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	FullPath string `json:"full_path"`
}

func (g *GitLabHost) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	var out []Namespace
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("/namespaces?per_page=100&page=%d", page)
		req, err := g.newRequest(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list gitlab namespaces: %w", err)
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return nil, ErrUnauthorized
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("list gitlab namespaces status %d: %s", resp.StatusCode, string(b))
		}

		var raw []gitlabNamespace
		decodeErr := json.NewDecoder(resp.Body).Decode(&raw)
		nextPage := strings.TrimSpace(resp.Header.Get("X-Next-Page"))
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode gitlab namespaces: %w", decodeErr)
		}
		for _, ns := range raw {
			out = append(out, Namespace{
				ID:       ns.ID,
				Name:     ns.Name,
				Path:     ns.Path,
				Kind:     ns.Kind,
				FullPath: ns.FullPath,
			})
		}
		if nextPage == "" {
			return out, nil
		}
		next, err := strconv.Atoi(nextPage)
		if err != nil || next <= page {
			return nil, fmt.Errorf("invalid GitLab namespace pagination header %q", nextPage)
		}
		page = next - 1
	}
}

type gitlabProjectResp struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	SSHURLToRepo      string `json:"ssh_url_to_repo"`
	DefaultBranch     string `json:"default_branch"`
}

func (g *GitLabHost) CreateRepository(ctx context.Context, req CreateRepoRequest) (*Repository, error) {
	name := strings.TrimSpace(req.Name)
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = name
	}
	if name == "" {
		return nil, fmt.Errorf("repository name is required")
	}

	body := map[string]any{
		"name":                   name,
		"path":                   path,
		"visibility":             EnsureValidVisibility(req.Visibility),
		"initialize_with_readme": false,
	}
	if req.NamespaceID > 0 {
		body["namespace_id"] = req.NamespaceID
	}
	if req.Description != "" {
		body["description"] = req.Description
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := g.newRequest(ctx, http.MethodPost, "/projects", bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create gitlab project: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, ErrConflict
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create gitlab project status %d: %s", resp.StatusCode, string(b))
	}

	var p gitlabProjectResp
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode gitlab project response: %w", err)
	}

	defaultBranch := p.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	return &Repository{
		ID:                strconv.FormatInt(p.ID, 10),
		Name:              p.Name,
		PathWithNamespace: p.PathWithNamespace,
		WebURL:            p.WebURL,
		HTTPCloneURL:      p.HTTPURLToRepo,
		SSHCloneURL:       p.SSHURLToRepo,
		DefaultBranch:     defaultBranch,
	}, nil
}

func (g *GitLabHost) DeleteRepository(ctx context.Context, projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fmt.Errorf("project ID is required")
	}

	endpoint := fmt.Sprintf("/projects/%s", url.PathEscape(projectID))
	req, err := g.newRequest(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete gitlab project: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete gitlab project status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

type gitlabMRResp struct {
	ID           int64  `json:"id"`
	IID          int64  `json:"iid"`
	ProjectID    int64  `json:"project_id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	WebURL       string `json:"web_url"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SHA          string `json:"sha"`
}

func (g *GitLabHost) CreateOrUpdateMR(ctx context.Context, projectID string, existingIID string, sourceBranch, targetBranch, title, desc, headSHA string) (*ChangeRequest, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	sourceBranch = strings.TrimSpace(sourceBranch)
	targetBranch = strings.TrimSpace(targetBranch)
	if targetBranch == "" {
		targetBranch = "main"
	}

	// 1. If explicit IID given, try updating it
	if existingIID != "" {
		cr, err := g.updateMR(ctx, projectID, existingIID, title, desc)
		if err == nil {
			return cr, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}

	// 2. Search for existing open MR with source_branch
	endpointSearch := fmt.Sprintf("/projects/%s/merge_requests?source_branch=%s&target_branch=%s&state=opened",
		url.PathEscape(projectID), url.QueryEscape(sourceBranch), url.QueryEscape(targetBranch))
	searchReq, err := g.newRequest(ctx, http.MethodGet, endpointSearch, nil)
	if err != nil {
		return nil, err
	}
	searchResp, err := g.client.Do(searchReq)
	if err != nil {
		return nil, fmt.Errorf("search gitlab merge requests: %w", err)
	}
	if searchResp.StatusCode == http.StatusUnauthorized || searchResp.StatusCode == http.StatusForbidden {
		searchResp.Body.Close()
		return nil, ErrUnauthorized
	}
	if searchResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(searchResp.Body)
		searchResp.Body.Close()
		return nil, fmt.Errorf("search gitlab merge requests status %d: %s", searchResp.StatusCode, string(b))
	}
	var openMRs []gitlabMRResp
	decodeErr := json.NewDecoder(searchResp.Body).Decode(&openMRs)
	searchResp.Body.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("decode gitlab merge requests: %w", decodeErr)
	}
	if len(openMRs) > 0 {
		iidStr := strconv.FormatInt(openMRs[0].IID, 10)
		return g.updateMR(ctx, projectID, iidStr, title, desc)
	}

	// 3. Create new MR
	body := map[string]any{
		"source_branch": sourceBranch,
		"target_branch": targetBranch,
		"title":         title,
		"description":   desc,
	}
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpointCreate := fmt.Sprintf("/projects/%s/merge_requests", url.PathEscape(projectID))
	req, err := g.newRequest(ctx, http.MethodPost, endpointCreate, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create gitlab mr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, ErrConflict
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create gitlab mr status %d: %s", resp.StatusCode, string(b))
	}

	var mr gitlabMRResp
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode gitlab mr response: %w", err)
	}

	return &ChangeRequest{
		ID:           strconv.FormatInt(mr.IID, 10),
		ProjectID:    projectID,
		Title:        mr.Title,
		Description:  mr.Description,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.SHA,
		WebURL:       mr.WebURL,
		State:        mr.State,
	}, nil
}

func (g *GitLabHost) updateMR(ctx context.Context, projectID, mrIID, title, desc string) (*ChangeRequest, error) {
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}
	if desc != "" {
		body["description"] = desc
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("/projects/%s/merge_requests/%s", url.PathEscape(projectID), url.PathEscape(mrIID))
	req, err := g.newRequest(ctx, http.MethodPut, endpoint, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update gitlab mr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("update gitlab mr status %d: %s", resp.StatusCode, string(b))
	}

	var mr gitlabMRResp
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode gitlab mr response: %w", err)
	}

	return &ChangeRequest{
		ID:           strconv.FormatInt(mr.IID, 10),
		ProjectID:    projectID,
		Title:        mr.Title,
		Description:  mr.Description,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.SHA,
		WebURL:       mr.WebURL,
		State:        mr.State,
	}, nil
}

func (g *GitLabHost) GetMR(ctx context.Context, projectID, mrIID string) (*ChangeRequest, error) {
	endpoint := fmt.Sprintf("/projects/%s/merge_requests/%s", url.PathEscape(projectID), url.PathEscape(mrIID))
	req, err := g.newRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get gitlab mr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get gitlab mr status %d: %s", resp.StatusCode, string(b))
	}

	var mr gitlabMRResp
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode gitlab mr response: %w", err)
	}

	return &ChangeRequest{
		ID:           strconv.FormatInt(mr.IID, 10),
		ProjectID:    projectID,
		Title:        mr.Title,
		Description:  mr.Description,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.SHA,
		WebURL:       mr.WebURL,
		State:        mr.State,
	}, nil
}

func (g *GitLabHost) MergeMR(ctx context.Context, projectID, mrIID, expectedHeadSHA, commitMessage string) error {
	body := map[string]any{}
	if expectedHeadSHA != "" {
		body["sha"] = expectedHeadSHA // optimistic lock on head commit
	}
	if commitMessage != "" {
		body["merge_commit_message"] = commitMessage
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("/projects/%s/merge_requests/%s/merge", url.PathEscape(projectID), url.PathEscape(mrIID))
	req, err := g.newRequest(ctx, http.MethodPut, endpoint, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("merge gitlab mr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: gitlab mr cannot be merged (conflict or sha mismatch): %s", ErrConflict, string(b))
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("merge gitlab mr status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (g *GitLabHost) newRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	u := g.baseURL + endpoint
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if g.token != "" {
		req.Header.Set("PRIVATE-TOKEN", g.token)
	}
	return req, nil
}
