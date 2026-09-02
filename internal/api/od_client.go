package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/store"
)

// OpenDesign (OD) client. Phase 0 (docs/opendesign-integration-plan.md
// appendix A) established the real wire contract:
//
//   - BYOK is request-scoped: every run carries agentId "byok-opencode" plus
//     a byokProvider object (protocol/baseUrl/apiKey/model). The OD daemon
//     never stores model credentials, so the platform injects them per call.
//   - Project creation requires a client-minted safe id (proj_<taskID>).
//   - Run creation answers 202 with the conversation id; run status is polled
//     via GET /api/runs?projectId=.

const (
	odAgentID          = "byok-opencode"
	odDefaultModel     = "qwen3.8-27b"
	odHTTPTimeout      = 20 * time.Second
	odProviderProvider = "opendesign"
)

// ODProject is the subset of an OD project listing the design gate needs.
// OD timestamps are epoch millis (numbers); the UI sorts by its own fetched-at
// ordering, so they are not decoded here at all.
type ODProject struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DesignSystemID string `json:"designSystemId"`
}

// odByokProvider mirrors the request-scoped BYOK credentials OD accepts on
// run/chat creation. The API key lives only inside this request body — never
// in logs, task comments, or responses.
type odByokProvider struct {
	Protocol string `json:"protocol"`
	APIKey   string `json:"apiKey"`
	BaseURL  string `json:"baseUrl"`
	Model    string `json:"model"`
}

// designModelCreds carries the LLM gateway credentials the design agent runs
// against. They belong to the task agent's model account (e.g. a
// Claude-Code-compatible gateway), never to the OD daemon: OD forwards them
// to opencode as the model endpoint, so handing it the OD connection would
// make opencode call the OD daemon itself (live incident 2026-09-02).
type designModelCreds struct {
	Protocol string
	APIKey   string
	BaseURL  string
	Model    string
}

// odProviderProtocol maps a stored model-provider type to the BYOK protocol
// OD's buildOpenCodeByokProviderConfig understands. Empty when unsupported.
func odProviderProtocol(providerType string) string {
	switch strings.TrimSpace(providerType) {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	default:
		return ""
	}
}

// resolveDesignModelProvider resolves the model-account credentials for the
// task's agent. Resolution order:
//  1. agent's bound model account (AgentMeta.Provider → ProviderStore)
//  2. agent env overrides (ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN or
//     OPENAI_BASE_URL/OPENAI_API_KEY), for agents wired ad hoc
//
// The returned creds are for the model gateway only; the OD connection is
// resolved separately for the OD API calls themselves.
func (s *Server) resolveDesignModelProvider(project, agent, model string) (*designModelCreds, error) {
	if agentMeta := s.designAgentMeta(project, agent); agentMeta != nil && agentMeta.Provider != "" {
		prov, err := store.NewProviderStoreWithDB(s.root, s.controlDB).Get(agentMeta.Provider)
		if err == nil && prov != nil {
			if protocol := odProviderProtocol(prov.Type); protocol != "" && strings.TrimSpace(prov.BaseURL) != "" {
				return &designModelCreds{
					Protocol: protocol,
					APIKey:   prov.APIKey,
					BaseURL:  strings.TrimRight(strings.TrimSpace(prov.BaseURL), "/"),
					Model:    model,
				}, nil
			}
		}
	}
	// Fall back to agent-scoped env overrides (per-agent env beats provider
	// env in resolveProviderEnv, so read both layers the same way).
	if agentMeta := s.designAgentMeta(project, agent); agentMeta != nil {
		env := agentMeta.Env
		if base := strings.TrimSpace(env["ANTHROPIC_BASE_URL"]); base != "" && strings.TrimSpace(env["ANTHROPIC_AUTH_TOKEN"]+env["ANTHROPIC_API_KEY"]) != "" {
			key := env["ANTHROPIC_AUTH_TOKEN"]
			if key == "" {
				key = env["ANTHROPIC_API_KEY"]
			}
			return &designModelCreds{Protocol: "anthropic", APIKey: key, BaseURL: strings.TrimRight(base, "/"), Model: model}, nil
		}
		if base := strings.TrimSpace(env["OPENAI_BASE_URL"]); base != "" && strings.TrimSpace(env["OPENAI_API_KEY"]) != "" {
			return &designModelCreds{Protocol: "openai", APIKey: env["OPENAI_API_KEY"], BaseURL: strings.TrimRight(base, "/"), Model: model}, nil
		}
	}
	return nil, fmt.Errorf("agent %s has no resolvable model provider for the design gate", agent)
}

func (s *Server) designAgentMeta(project, agent string) *entity.AgentMeta {
	if s.st == nil || agent == "" {
		return nil
	}
	meta, err := s.st.AgentMeta(project, agent)
	if err != nil || meta == nil {
		return nil
	}
	return meta
}

// odClientAPI is the consumption-side interface the design handlers depend
// on; tests inject fakes instead of an HTTP client.
type odClientAPI interface {
	ListProjects(ctx context.Context) ([]ODProject, error)
	CreateProject(ctx context.Context, id, name, designSystemID, pendingPrompt string) error
	// StartRun takes the task agent's model-gateway creds: byokProvider must
	// point opencode at the LLM gateway (ccr-qwen etc.), never at the OD
	// daemon itself (pitfall: OD's connection baseUrl is OD, not the model).
	StartRun(ctx context.Context, projectID, message, designSystemID, model string, creds *designModelCreds) (conversationID string, err error)
	LatestRunStatus(ctx context.Context, projectID string) (runID, status string, err error)
	DeleteProject(ctx context.Context, projectID string) error
}

type odClient struct {
	s *Server
}

var _ odClientAPI = (*odClient)(nil)

func (s *Server) defaultODClient() odClientAPI {
	if s.designClient != nil {
		return s.designClient
	}
	return &odClient{s: s}
}

// designConnectionConfig resolves the workspace's opendesign connection:
// base URL from ProfileJSON (or secret "baseUrl"), API key from the
// encrypted secret values. Same read pattern as resolveCodeHost.
type designConnectionConfig struct {
	BaseURL string
	APIKey  string
	ConnID  string
}

func (s *Server) resolveDesignConnection() (*designConnectionConfig, error) {
	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		return nil, err
	}
	connections, err := s.controlDB.ListConnections(controldb.ConnectionFilter{
		WorkspaceID: workspaceID,
		Provider:    odProviderProvider,
	})
	if err != nil {
		return nil, fmt.Errorf("list opendesign connections: %w", err)
	}
	if len(connections) == 0 {
		return nil, fmt.Errorf("no opendesign connection configured in this workspace")
	}
	conn := connections[0]

	values := map[string]string{}
	secret, ok, err := s.controlDB.ConnectionSecret(conn.ID)
	if err != nil {
		return nil, fmt.Errorf("read connection secret: %w", err)
	}
	if ok {
		opened, err := openConnectionSecret(secret)
		if err != nil {
			return nil, fmt.Errorf("decrypt connection secret: %w", err)
		}
		values = opened
	}

	baseURL := strings.TrimSpace(values["baseUrl"])
	if baseURL == "" {
		profile := map[string]any{}
		_ = json.Unmarshal([]byte(conn.ProfileJSON), &profile)
		if v, ok := profile["baseUrl"].(string); ok {
			baseURL = strings.TrimSpace(v)
		}
	}
	if baseURL == "" {
		return nil, fmt.Errorf("opendesign connection %s has no baseUrl", conn.ID)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	apiKey := strings.TrimSpace(values["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("opendesign connection %s has no apiKey", conn.ID)
	}
	return &designConnectionConfig{BaseURL: baseURL, APIKey: apiKey, ConnID: conn.ID}, nil
}

// odDo performs an authenticated OD API call and decodes the JSON response.
func (s *Server) odDo(ctx context.Context, method, path string, body any, out any) error {
	cfg, err := s.resolveDesignConnection()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode od request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build od request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The OD token is injected here and never logged; error paths below only
	// carry status codes and redacted bodies.
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: odHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("od service unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read od response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.ReplaceAll(string(raw), cfg.APIKey, "[redacted]")
		return &odAPIError{Status: resp.StatusCode, Detail: detail}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode od response: %w", err)
		}
	}
	return nil
}

type odAPIError struct {
	Status int
	Detail string
}

func (e *odAPIError) Error() string {
	detail := e.Detail
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return fmt.Sprintf("od api %d: %s", e.Status, detail)
}

func (c *odClient) ListProjects(ctx context.Context) ([]ODProject, error) {
	var out struct {
		Projects []ODProject `json:"projects"`
	}
	if err := c.s.odDo(ctx, http.MethodGet, "/api/projects", nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func (c *odClient) CreateProject(ctx context.Context, id, name, designSystemID, pendingPrompt string) error {
	body := map[string]any{"id": id, "name": name}
	if designSystemID != "" {
		body["designSystemId"] = designSystemID
	}
	if pendingPrompt != "" {
		body["pendingPrompt"] = pendingPrompt
	}
	return c.s.odDo(ctx, http.MethodPost, "/api/projects", body, nil)
}

func (c *odClient) StartRun(ctx context.Context, projectID, message, designSystemID, model string, creds *designModelCreds) (string, error) {
	if model == "" {
		model = odDefaultModel
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" {
		return "", fmt.Errorf("design model credentials unresolved for run on %s", projectID)
	}
	body := map[string]any{
		"projectId": projectID,
		"agentId":   odAgentID,
		"message":   message,
		// OD validates BYOK completeness against the TOP-LEVEL model field
		// (meta.model); byokProvider.model alone fails validation.
		"model": model,
		"byokProvider": odByokProvider{
			Protocol: creds.Protocol,
			APIKey:   creds.APIKey,
			BaseURL:  creds.BaseURL,
			Model:    model,
		},
	}
	if designSystemID != "" {
		body["designSystemId"] = designSystemID
	}
	var out struct {
		ConversationID string `json:"conversationId"`
		RunID          string `json:"runId"`
		ID             string `json:"id"`
	}
	if err := c.s.odDo(ctx, http.MethodPost, "/api/runs", body, &out); err != nil {
		return "", err
	}
	if out.ConversationID != "" {
		return out.ConversationID, nil
	}
	return "", nil
}

func (c *odClient) LatestRunStatus(ctx context.Context, projectID string) (string, string, error) {
	var out struct {
		Runs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := c.s.odDo(ctx, http.MethodGet, "/api/runs?projectId="+projectID, nil, &out); err != nil {
		return "", "", err
	}
	if len(out.Runs) == 0 {
		return "", "none", nil
	}
	return out.Runs[0].ID, out.Runs[0].Status, nil
}

func (c *odClient) DeleteProject(ctx context.Context, projectID string) error {
	return c.s.odDo(ctx, http.MethodDelete, "/api/projects/"+projectID, nil, nil)
}
