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

// GitHubConfig holds connection parameters for GitHub or a GitHub Enterprise
// API-compatible server.
type GitHubConfig struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// GitHubHost implements CodeHost for GitHub pull requests and repositories.
type GitHubHost struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewGitHubHost(cfg GitHubConfig) *GitHubHost {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if !strings.HasSuffix(baseURL, "/api/v3") && strings.Contains(baseURL, "github.example") {
		baseURL += "/api/v3"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubHost{baseURL: baseURL, token: strings.TrimSpace(cfg.Token), client: client}
}

func (g *GitHubHost) Provider() string { return "github" }

func (g *GitHubHost) CheckConnection(ctx context.Context) error {
	req, err := g.newRequest(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("github connection check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github connection check failed with status %d", resp.StatusCode)
	}
	return nil
}

type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	HTMLURL   string `json:"html_url"`
	AvatarURL string `json:"avatar_url"`
}

func (g *GitHubHost) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	var namespaces []Namespace
	var user githubUser
	if err := g.doJSON(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return nil, fmt.Errorf("get github user: %w", err)
	}
	namespaces = append(namespaces, Namespace{ID: user.ID, Name: firstNonEmptyString(user.Name, user.Login), Path: user.Login, Kind: "user", FullPath: user.Login})
	var orgs []githubUser
	if err := g.doJSON(ctx, http.MethodGet, "/user/orgs?per_page=100", nil, &orgs); err != nil {
		if err == ErrUnauthorized {
			return nil, err
		}
		return namespaces, nil
	}
	for _, org := range orgs {
		namespaces = append(namespaces, Namespace{ID: org.ID, Name: firstNonEmptyString(org.Name, org.Login), Path: org.Login, Kind: "group", FullPath: org.Login})
	}
	return namespaces, nil
}

type githubRepoResp struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	FullName      string     `json:"full_name"`
	HTMLURL       string     `json:"html_url"`
	CloneURL      string     `json:"clone_url"`
	SSHURL        string     `json:"ssh_url"`
	DefaultBranch string     `json:"default_branch"`
	Owner         githubUser `json:"owner"`
}

func (g *GitHubHost) CreateRepository(ctx context.Context, req CreateRepoRequest) (*Repository, error) {
	name := strings.TrimSpace(req.Path)
	if name == "" {
		name = strings.TrimSpace(req.Name)
	}
	if name == "" {
		return nil, fmt.Errorf("repository name is required")
	}
	body := map[string]any{
		"name":        name,
		"description": req.Description,
		"private":     !strings.EqualFold(EnsureValidVisibility(req.Visibility), "public"),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var repo githubRepoResp
	if err := g.doJSON(ctx, http.MethodPost, "/user/repos", bytes.NewReader(data), &repo); err != nil {
		if strings.Contains(err.Error(), "status 422") {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("create github repository: %w", err)
	}
	return g.toRepository(repo), nil
}

func (g *GitHubHost) toRepository(repo githubRepoResp) *Repository {
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	return &Repository{
		ID: strconv.FormatInt(repo.ID, 10), Name: repo.Name, PathWithNamespace: repo.FullName,
		WebURL: repo.HTMLURL, HTTPCloneURL: repo.CloneURL, SSHCloneURL: repo.SSHURL, DefaultBranch: branch,
	}
}

func (g *GitHubHost) DeleteRepository(ctx context.Context, projectID string) error {
	if strings.TrimSpace(projectID) == "" {
		return fmt.Errorf("project ID is required")
	}
	return g.doJSON(ctx, http.MethodDelete, repoPath(projectID), nil, nil)
}

type githubPRResp struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Merged bool `json:"merged"`
}

func (g *GitHubHost) CreateOrUpdateMR(ctx context.Context, projectID, existingIID, sourceBranch, targetBranch, title, desc, headSHA string) (*ChangeRequest, error) {
	projectID = strings.TrimSpace(projectID)
	sourceBranch = strings.TrimSpace(sourceBranch)
	targetBranch = strings.TrimSpace(targetBranch)
	if projectID == "" || sourceBranch == "" {
		return nil, fmt.Errorf("github project, source branch and target branch are required")
	}
	if targetBranch == "" {
		targetBranch = "main"
	}
	if existingIID != "" {
		cr, err := g.updatePR(ctx, projectID, existingIID, title, desc)
		if err == nil {
			return cr, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	owner := strings.SplitN(projectID, "/", 2)[0]
	var open []githubPRResp
	query := "/pulls?state=open&base=" + url.QueryEscape(targetBranch) + "&head=" + url.QueryEscape(owner+":"+sourceBranch)
	if err := g.doJSON(ctx, http.MethodGet, repoPath(projectID)+query, nil, &open); err != nil {
		return nil, fmt.Errorf("search github pull requests: %w", err)
	}
	if len(open) > 0 {
		return g.updatePR(ctx, projectID, strconv.Itoa(open[0].Number), title, desc)
	}
	body, err := json.Marshal(map[string]string{"head": sourceBranch, "base": targetBranch, "title": title, "body": desc})
	if err != nil {
		return nil, err
	}
	var pr githubPRResp
	if err := g.doJSON(ctx, http.MethodPost, repoPath(projectID)+"/pulls", bytes.NewReader(body), &pr); err != nil {
		return nil, fmt.Errorf("create github pull request: %w", err)
	}
	return toChangeRequest(projectID, pr), nil
}

func (g *GitHubHost) updatePR(ctx context.Context, projectID, number, title, desc string) (*ChangeRequest, error) {
	body, err := json.Marshal(map[string]string{"title": title, "body": desc})
	if err != nil {
		return nil, err
	}
	var pr githubPRResp
	if err := g.doJSON(ctx, http.MethodPatch, repoPath(projectID)+"/pulls/"+url.PathEscape(number), bytes.NewReader(body), &pr); err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("update github pull request: %w", err)
	}
	return toChangeRequest(projectID, pr), nil
}

func (g *GitHubHost) GetMR(ctx context.Context, projectID, mrIID string) (*ChangeRequest, error) {
	var pr githubPRResp
	if err := g.doJSON(ctx, http.MethodGet, repoPath(projectID)+"/pulls/"+url.PathEscape(mrIID), nil, &pr); err != nil {
		if strings.Contains(err.Error(), "status 404") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get github pull request: %w", err)
	}
	return toChangeRequest(projectID, pr), nil
}

func (g *GitHubHost) MergeMR(ctx context.Context, projectID, mrIID, expectedHeadSHA, commitMessage string) error {
	body := map[string]string{"merge_method": "merge"}
	if expectedHeadSHA != "" {
		body["sha"] = expectedHeadSHA
	}
	if commitMessage != "" {
		body["commit_message"] = commitMessage
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var result struct {
		Merged bool `json:"merged"`
	}
	if err := g.doJSON(ctx, http.MethodPut, repoPath(projectID)+"/pulls/"+url.PathEscape(mrIID)+"/merge", bytes.NewReader(data), &result); err != nil {
		return fmt.Errorf("merge github pull request: %w", err)
	}
	if !result.Merged {
		return fmt.Errorf("%w: github pull request was not merged", ErrConflict)
	}
	return nil
}

func toChangeRequest(projectID string, pr githubPRResp) *ChangeRequest {
	state := pr.State
	if pr.Merged {
		state = "merged"
	}
	return &ChangeRequest{ID: strconv.Itoa(pr.Number), ProjectID: projectID, Title: pr.Title, Description: pr.Body, SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref, HeadSHA: pr.Head.SHA, WebURL: pr.HTMLURL, State: state}
}

func repoPath(projectID string) string {
	return "/repos/" + url.PathEscape(strings.TrimSpace(projectID))
}

func (g *GitHubHost) newRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (g *GitHubHost) doJSON(ctx context.Context, method, endpoint string, body io.Reader, result any) error {
	req, err := g.newRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github API status %d", resp.StatusCode)
	}
	if result == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decode github API response: %w", err)
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
