package codehost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
