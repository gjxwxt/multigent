package codehost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitLabHostSetProjectVariableCreates(t *testing.T) {
	var gotKey, gotValue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/42/variables" {
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			gotKey, gotValue = r.FormValue("key"), r.FormValue("value")
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "tok"})
	if err := host.SetProjectVariable(context.Background(), "42", "APP_PORT", "88000"); err != nil {
		t.Fatalf("SetProjectVariable: %v", err)
	}
	if gotKey != "APP_PORT" || gotValue != "88000" {
		t.Fatalf("server saw key=%q value=%q", gotKey, gotValue)
	}
}

func TestGitLabHostSetProjectVariableUpdatesOnDuplicate(t *testing.T) {
	var updatedValue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/42/variables":
			// GitLab rejects duplicate keys with 400.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":{"key":["has already been taken"]}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v4/projects/42/variables/APP_PORT":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			updatedValue = r.FormValue("value")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "tok"})
	if err := host.SetProjectVariable(context.Background(), "42", "APP_PORT", "88007"); err != nil {
		t.Fatalf("SetProjectVariable update path: %v", err)
	}
	if updatedValue != "88007" {
		t.Fatalf("PUT value=%q, want 88007", updatedValue)
	}
}

func TestGitLabHostSetProjectVariablePropagatesHardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 Forbidden"}`))
	}))
	defer srv.Close()

	host := NewGitLabHost(GitLabConfig{BaseURL: srv.URL, Token: "bad"})
	if err := host.SetProjectVariable(context.Background(), "42", "APP_PORT", "88000"); err == nil {
		t.Fatal("expected error on 403")
	}
}
