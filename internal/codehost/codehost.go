package codehost

import (
	"context"
	"errors"
	"strings"
	"sync"
)

var (
	ErrNotFound     = errors.New("codehost: resource not found")
	ErrConflict     = errors.New("codehost: resource conflict")
	ErrUnauthorized = errors.New("codehost: unauthorized access")
)

// Namespace represents an account or group namespace where repositories can be created.
type Namespace struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // "user" | "group"
	FullPath string `json:"fullPath"`
}

// CreateRepoRequest specifies options when creating a new remote repository.
type CreateRepoRequest struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	NamespaceID int64  `json:"namespaceId,omitempty"`
	Visibility  string `json:"visibility"` // "private" | "internal" | "public"
	Description string `json:"description,omitempty"`
}

// Repository contains metadata about a remote repository.
type Repository struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	PathWithNamespace string `json:"pathWithNamespace"`
	WebURL            string `json:"webUrl"`
	HTTPCloneURL      string `json:"httpCloneUrl"` // clean URL without embedded token
	SSHCloneURL       string `json:"sshCloneUrl"`
	DefaultBranch     string `json:"defaultBranch"`
}

// ChangeRequest represents a Merge Request (GitLab) or Pull Request (GitHub/Gitee).
type ChangeRequest struct {
	ID           string `json:"id"` // IID or PR number (e.g. "12")
	ProjectID    string `json:"projectId"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	HeadSHA      string `json:"headSha"`
	WebURL       string `json:"webUrl"`
	State        string `json:"state"` // "opened" | "merged" | "closed"
}

// CodeHost is the provider-agnostic interface for remote code hosting platforms.
type CodeHost interface {
	// Provider returns the provider name (e.g. "gitlab", "github", "gitee").
	Provider() string

	// CheckConnection validates credential authentication and API reachability.
	CheckConnection(ctx context.Context) error

	// ListNamespaces lists personal accounts and groups where the user can create projects.
	ListNamespaces(ctx context.Context) ([]Namespace, error)

	// CreateRepository creates a new remote repository without initial README.
	CreateRepository(ctx context.Context, req CreateRepoRequest) (*Repository, error)

	// DeleteRepository removes a repository (used for rolling back failed initializations).
	DeleteRepository(ctx context.Context, projectID string) error

	// CreateOrUpdateMR creates a new MR or updates an existing open MR for the source branch.
	// If existingIID is provided, it attempts to update that MR directly.
	CreateOrUpdateMR(ctx context.Context, projectID string, existingIID string, sourceBranch, targetBranch, title, desc, headSHA string) (*ChangeRequest, error)

	// GetMR fetches a specific change request by project and MR IID.
	GetMR(ctx context.Context, projectID, mrIID string) (*ChangeRequest, error)

	// MergeMR merges an open MR, optionally using expectedHeadSHA for optimistic locking.
	MergeMR(ctx context.Context, projectID, mrIID, expectedHeadSHA, commitMessage string) error
}

// Registry manages registered CodeHost adapters.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]CodeHost
}

var globalRegistry = &Registry{
	providers: make(map[string]CodeHost),
}

// Register registers a CodeHost instance for a provider name.
func Register(provider string, host CodeHost) {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	globalRegistry.providers[strings.ToLower(strings.TrimSpace(provider))] = host
}

// Get returns the registered CodeHost for the given provider.
func Get(provider string) (CodeHost, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	h, ok := globalRegistry.providers[strings.ToLower(strings.TrimSpace(provider))]
	return h, ok
}

// EnsureValidVisibility standardizes visibility values.
func EnsureValidVisibility(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "public":
		return "public"
	case "internal":
		return "internal"
	default:
		return "private"
	}
}
