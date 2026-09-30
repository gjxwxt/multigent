package codehost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitLabHostCheckConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "my-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v4/user" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":1,"username":"alice"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{
		BaseURL: srv.URL,
		Token:   "my-token",
	})

	if err := host.CheckConnection(context.Background()); err != nil {
		t.Fatalf("CheckConnection failed: %v", err)
	}
}

func TestGitLabHostListNamespaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/namespaces" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[
				{"id":10,"name":"Alice","path":"alice","kind":"user","full_path":"alice"},
				{"id":20,"name":"Core Team","path":"core-team","kind":"group","full_path":"core-team"}
			]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	ns, err := host.ListNamespaces(context.Background())
	if err != nil {
		t.Fatalf("ListNamespaces failed: %v", err)
	}
	if len(ns) != 2 {
		t.Fatalf("expected 2 namespaces, got %d", len(ns))
	}
	if ns[1].Name != "Core Team" || ns[1].Kind != "group" {
		t.Errorf("unexpected namespace: %+v", ns[1])
	}
}

func TestGitLabHostListNamespacesPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/namespaces" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":10,"name":"Alice","path":"alice","kind":"user","full_path":"alice"}]`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":20,"name":"Core Team","path":"core-team","kind":"group","full_path":"core-team"}]`))
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	ns, err := host.ListNamespaces(context.Background())
	if err != nil {
		t.Fatalf("ListNamespaces failed: %v", err)
	}
	if len(ns) != 2 || ns[0].ID != 10 || ns[1].ID != 20 {
		t.Fatalf("unexpected paginated namespaces: %+v", ns)
	}
}

func TestGitLabHostCreateRepository(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/projects" && r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "my-app" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id": 42,
				"name": "my-app",
				"path_with_namespace": "team/my-app",
				"web_url": "https://gitlab.example.com/team/my-app",
				"http_url_to_repo": "https://gitlab.example.com/team/my-app.git",
				"ssh_url_to_repo": "git@gitlab.example.com:team/my-app.git",
				"default_branch": "main"
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	repo, err := host.CreateRepository(context.Background(), CreateRepoRequest{
		Name:        "my-app",
		NamespaceID: 10,
		Visibility:  "private",
	})
	if err != nil {
		t.Fatalf("CreateRepository failed: %v", err)
	}
	if repo.ID != "42" || repo.HTTPCloneURL != "https://gitlab.example.com/team/my-app.git" {
		t.Errorf("unexpected repo: %+v", repo)
	}
}

func TestGitLabHostNormalizesLocalCloneURLWithoutCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id": 42,
			"name": "my-app",
			"path_with_namespace": "root/my-app",
			"web_url": "http://localhost:8083/root/my-app",
			"http_url_to_repo": "http://localhost:8083/root/my-app.git",
			"ssh_url_to_repo": "ssh://git@localhost:2224/root/my-app.git"
		}`))
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "secret-token"})
	repo, err := host.CreateRepository(context.Background(), CreateRepoRequest{Name: "my-app"})
	if err != nil {
		t.Fatalf("CreateRepository failed: %v", err)
	}
	if repo.HTTPCloneURL != "http://host.docker.internal:8083/root/my-app.git" {
		t.Fatalf("unexpected normalized clone URL: %q", repo.HTTPCloneURL)
	}
	body, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret-token") || strings.Contains(string(body), "authenticatedCloneUrl") {
		t.Fatalf("repository response must not expose credentials: %s", body)
	}
}

func TestGitLabHostCreateAndMergeMR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/projects/42/merge_requests" && r.Method == http.MethodGet:
			// No existing open MR
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/api/v4/projects/42/merge_requests" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id": 101,
				"iid": 7,
				"project_id": 42,
				"title": "feat: add search",
				"description": "details",
				"state": "opened",
				"source_branch": "feature/t-1",
				"target_branch": "main",
				"sha": "abc1234",
				"web_url": "https://gitlab.example.com/team/my-app/-/merge_requests/7"
			}`))
		case r.URL.Path == "/api/v4/projects/42/merge_requests/7/merge" && r.Method == http.MethodPut:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["sha"] != "abc1234" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"Branch has changed"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"state":"merged"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	mr, err := host.CreateOrUpdateMR(context.Background(), "42", "", "feature/t-1", "main", "feat: add search", "details", "abc1234")
	if err != nil {
		t.Fatalf("CreateOrUpdateMR failed: %v", err)
	}
	if mr.ID != "7" || mr.HeadSHA != "abc1234" || mr.WebURL == "" {
		t.Errorf("unexpected MR: %+v", mr)
	}

	// Test Merge with matching SHA
	if err := host.MergeMR(context.Background(), "42", "7", "abc1234", "merge commit"); err != nil {
		t.Fatalf("MergeMR failed: %v", err)
	}

	// Test Merge with mismatched SHA
	if err := host.MergeMR(context.Background(), "42", "7", "wrong-sha", "merge commit"); err == nil {
		t.Fatalf("expected MergeMR with wrong sha to fail")
	}
}

func TestGitLabHostNormalizesCloneURLWithBaseURLHost(t *testing.T) {
	host := NewGitLabHost(GitLabConfig{BaseURL: "http://192.168.139.3:8083", Token: "secret"})
	repo := host.toRepository(gitlabProjectResp{
		ID:            123,
		Name:          "demo-repo",
		HTTPURLToRepo: "http://localhost:8083/root/demo-repo.git",
	})
	if repo.HTTPCloneURL != "http://192.168.139.3:8083/root/demo-repo.git" {
		t.Fatalf("expected clone URL rewritten to baseURL host, got: %q", repo.HTTPCloneURL)
	}
}

func TestGitLabHostListBranches(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is the decoded form: "root%2Fmy-app" arrives as "root/my-app".
		if r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/root/my-app/repository/branches" {
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[
				{"name":"main","default":true,"commit":{"id":"aaa111","title":"init project"}},
				{"name":"feature/deploy","default":false,"commit":{"id":"bbb222","title":"feat: deploy step"}}
			]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	branches, err := host.ListBranches(context.Background(), "root/my-app")
	if err != nil {
		t.Fatalf("ListBranches failed: %v", err)
	}
	if gotQuery != "per_page=100" {
		t.Errorf("expected per_page=100 query, got %q", gotQuery)
	}
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	if branches[0].Name != "main" || !branches[0].Default || branches[0].CommitID != "aaa111" || branches[0].CommitTitle != "init project" {
		t.Errorf("unexpected branch[0]: %+v", branches[0])
	}
	if branches[1].Name != "feature/deploy" || branches[1].Default || branches[1].CommitID != "bbb222" || branches[1].CommitTitle != "feat: deploy step" {
		t.Errorf("unexpected branch[1]: %+v", branches[1])
	}
}

func TestGitLabHostListBranchesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	if _, err := host.ListBranches(context.Background(), "99999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGitLabHostTriggerPipeline(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":777,"sha":"abc1234","ref":"main","status":"created","source":"api","web_url":"https://gitlab.example.com/team/my-app/-/pipelines/777"}`))
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	pipe, err := host.TriggerPipeline(context.Background(), "42", "main", map[string]string{
		"ZETA":    "last",
		"APP_ENV": "staging",
		"ALPHA":   "first",
	})
	if err != nil {
		t.Fatalf("TriggerPipeline failed: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}

	var body struct {
		Ref       string `json:"ref"`
		Variables []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("decode request body %q: %v", gotBody, err)
	}
	if body.Ref != "main" {
		t.Errorf("expected ref main, got %q", body.Ref)
	}
	if len(body.Variables) != 3 {
		t.Fatalf("expected 3 variables, got %d: %s", len(body.Variables), gotBody)
	}
	// Variables must be sorted by key for a deterministic request body.
	if body.Variables[0].Key != "ALPHA" || body.Variables[0].Value != "first" {
		t.Errorf("variables[0] not first sorted key: %+v", body.Variables[0])
	}
	if body.Variables[1].Key != "APP_ENV" || body.Variables[1].Value != "staging" {
		t.Errorf("variables[1] not second sorted key: %+v", body.Variables[1])
	}
	if body.Variables[2].Key != "ZETA" || body.Variables[2].Value != "last" {
		t.Errorf("variables[2] not third sorted key: %+v", body.Variables[2])
	}
	if pipe == nil || pipe.ID != 777 || pipe.SHA != "abc1234" || pipe.Ref != "main" || pipe.Status != "created" {
		t.Errorf("unexpected pipeline: %+v", pipe)
	}
}

func TestGitLabHostTriggerPipelinePropagatesErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"ref not found"}`))
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	_, err := host.TriggerPipeline(context.Background(), "42", "missing-branch", nil)
	if err == nil {
		t.Fatalf("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "ref not found") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected error to carry status and GitLab message, got: %v", err)
	}
}

func TestGitLabHostListRecentPipelines(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/42/pipelines" {
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[
				{"id":2,"sha":"def5678","ref":"main","status":"success","source":"push","web_url":"https://gitlab.example.com/p/2"},
				{"id":1,"sha":"abc1234","ref":"feature/x","status":"failed","source":"web","web_url":"https://gitlab.example.com/p/1"}
			]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	pipelines, err := host.ListRecentPipelines(context.Background(), "42", 0)
	if err != nil {
		t.Fatalf("ListRecentPipelines failed: %v", err)
	}
	if gotQuery != "per_page=20" {
		t.Errorf("expected default per_page=20, got %q", gotQuery)
	}
	if len(pipelines) != 2 {
		t.Fatalf("expected 2 pipelines, got %d", len(pipelines))
	}
	if pipelines[0].ID != 2 || pipelines[0].Status != "success" || pipelines[0].SHA != "def5678" || pipelines[0].Ref != "main" || pipelines[0].WebURL != "https://gitlab.example.com/p/2" {
		t.Errorf("unexpected pipeline[0]: %+v", pipelines[0])
	}
	if pipelines[1].ID != 1 || pipelines[1].Status != "failed" || pipelines[1].Source != "web" {
		t.Errorf("unexpected pipeline[1]: %+v", pipelines[1])
	}
}

func TestGitLabHostListRecentPipelinesClampsPerPage(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	if _, err := host.ListRecentPipelines(context.Background(), "42", 500); err != nil {
		t.Fatalf("ListRecentPipelines failed: %v", err)
	}
	if gotQuery != "per_page=20" {
		t.Errorf("expected perPage>100 to clamp to default 20, got %q", gotQuery)
	}
}

func TestGitLabHostPipelineJobsMapsRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/42/pipelines/777/jobs" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[
				{"id":11,"name":"build","stage":"build","status":"success","runner":{"id":9,"description":"shared-runner-01"}},
				{"id":12,"name":"deploy","stage":"deploy","status":"pending"}
			]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	jobs, err := host.PipelineJobs(context.Background(), "42", 777)
	if err != nil {
		t.Fatalf("PipelineJobs failed: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	if jobs[0].RunnerDescription != "shared-runner-01" {
		t.Errorf("expected runner description filled, got %q", jobs[0].RunnerDescription)
	}
	if jobs[1].RunnerDescription != "" {
		t.Errorf("expected empty runner description when runner absent, got %q", jobs[1].RunnerDescription)
	}
}
