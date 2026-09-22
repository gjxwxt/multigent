package connector

import "testing"

func TestNormalizeGitLabInstanceURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantAPI string
	}{
		{name: "default", want: "https://gitlab.com", wantAPI: "https://gitlab.com/api/v4"},
		{name: "self managed", input: "https://gitlab.example.test/", want: "https://gitlab.example.test", wantAPI: "https://gitlab.example.test/api/v4"},
		{name: "api url", input: "https://gitlab.example.test/api/v4/", want: "https://gitlab.example.test", wantAPI: "https://gitlab.example.test/api/v4"},
		{name: "relative url install", input: "https://example.test/gitlab/", want: "https://example.test/gitlab", wantAPI: "https://example.test/gitlab/api/v4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeGitLabInstanceURL(tt.input)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if got != tt.want {
				t.Fatalf("instance URL=%q, want %q", got, tt.want)
			}
			gotAPI, err := GitLabAPIBaseURL(tt.input)
			if err != nil {
				t.Fatalf("api URL: %v", err)
			}
			if gotAPI != tt.wantAPI {
				t.Fatalf("API URL=%q, want %q", gotAPI, tt.wantAPI)
			}
		})
	}
}

func TestNormalizeGitLabInstanceURLRejectsUnsafeValues(t *testing.T) {
	for _, input := range []string{
		"gitlab.example.test",
		"file:///tmp/gitlab",
		"https://user:pass@gitlab.example.test",
		"https://gitlab.example.test?token=secret",
		"https://gitlab.example.test/#settings",
	} {
		if _, err := NormalizeGitLabInstanceURL(input); err == nil {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
}
