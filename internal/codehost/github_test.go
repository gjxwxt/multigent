package codehost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubHostPullRequestLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/repos/acme/app/pulls" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]githubPRResp{})
			return
		}
		switch {
		case r.URL.Path == "/repos/acme/app/pulls" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(githubPRResp{Number: 12, Title: "title", State: "open", HTMLURL: "https://github.test/acme/app/pull/12", Head: struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}{Ref: "feature/t-1", SHA: "abc"}, Base: struct {
				Ref string `json:"ref"`
			}{Ref: "main"}})
		case r.URL.Path == "/repos/acme/app/pulls/12" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(githubPRResp{Number: 12, Title: "title", State: "open", Head: struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}{Ref: "feature/t-1", SHA: "abc"}, Base: struct {
				Ref string `json:"ref"`
			}{Ref: "main"}})
		case r.URL.Path == "/repos/acme/app/pulls/12/merge" && r.Method == http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	host := NewGitHubHost(GitHubConfig{BaseURL: server.URL, Token: "secret"})
	pr, err := host.CreateOrUpdateMR(context.Background(), "acme/app", "", "feature/t-1", "main", "title", "body", "abc")
	if err != nil {
		t.Fatalf("CreateOrUpdateMR: %v", err)
	}
	if pr.ID != "12" || pr.HeadSHA != "abc" {
		t.Fatalf("unexpected PR: %#v", pr)
	}
	got, err := host.GetMR(context.Background(), "acme/app", "12")
	if err != nil || got.SourceBranch != "feature/t-1" {
		t.Fatalf("GetMR: %#v, %v", got, err)
	}
	if err := host.MergeMR(context.Background(), "acme/app", "12", "abc", "merge"); err != nil {
		t.Fatalf("MergeMR: %v", err)
	}
}
