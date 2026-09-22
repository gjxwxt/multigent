package connector

import (
	"fmt"
	"net/url"
	"strings"
)

const DefaultGitLabInstanceURL = "https://gitlab.com"

// NormalizeGitLabInstanceURL accepts a GitLab web URL (or its /api/v4 URL)
// and returns the canonical instance root used by both HTTP actions and glab.
func NormalizeGitLabInstanceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultGitLabInstanceURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid GitLab instance URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("GitLab instance URL must use http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("GitLab instance URL must include a host")
	}
	if u.User != nil {
		return "", fmt.Errorf("GitLab instance URL must not include credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("GitLab instance URL must not include a query or fragment")
	}

	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/api/v4") {
		path = strings.TrimSuffix(path, "/api/v4")
	}
	u.Path = strings.TrimRight(path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func GitLabAPIBaseURL(instanceURL string) (string, error) {
	instanceURL, err := NormalizeGitLabInstanceURL(instanceURL)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(instanceURL, "/") + "/api/v4", nil
}
