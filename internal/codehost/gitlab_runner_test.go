package codehost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnableRunnerOnProjectBindsRunner(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/44/runners", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	if err := host.EnableRunnerOnProject(context.Background(), "44", "1"); err != nil {
		t.Fatalf("EnableRunnerOnProject failed: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v4/projects/44/runners" {
		t.Fatalf("unexpected request %s %s", gotMethod, gotPath)
	}
	if gotBody != "runner_id=1" {
		t.Fatalf("unexpected body %q", gotBody)
	}
}

func TestEnableRunnerOnProjectRejectsUnexpectedConflict(t *testing.T) {
	// Only the GitLab 400 "already been taken" body means idempotent
	// success; a bare 409 is a real error and must surface.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/44/runners", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	if err := host.EnableRunnerOnProject(context.Background(), "44", "1"); err == nil {
		t.Fatal("bare 409 must surface as an error (only 400 already-taken is idempotent)")
	}
}

func TestEnableRunnerOnProjectTreatsGitLab400TakenAsIdempotent(t *testing.T) {
	// Real-world GitLab (19.x) answers an already-bound runner with
	// 400 "Runner projects runner has already been taken", not 409 —
	// observed live during the health-probe flight (2026-09-06).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/46/runners", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Validation failed: Runner projects runner has already been taken - runner_error"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "t"})
	if err := host.EnableRunnerOnProject(context.Background(), "46", "1"); err != nil {
		t.Fatalf("400 already-taken must be idempotent success: %v", err)
	}
}

func TestEnableRunnerOnProjectRejectsMissingIDs(t *testing.T) {
	host := NewGitLabHost(GitLabConfig{BaseURL: "http://127.0.0.1:1", Token: "t"})
	if err := host.EnableRunnerOnProject(context.Background(), "", "1"); err == nil {
		t.Fatal("expected error for empty project ID")
	}
	if err := host.EnableRunnerOnProject(context.Background(), "44", ""); err == nil {
		t.Fatal("expected error for empty runner ID")
	}
}
