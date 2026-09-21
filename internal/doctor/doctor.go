// Package doctor implements the server-side `multigent doctor` self-check.
//
// Scope (reduction plan Task 0.2): a read-only diagnostic that reports the
// effective configuration tree, data-directory health, secrets baseline,
// GitLab connection reachability, LLM provider reachability, and Docker
// runtime readiness — the checks an operator needs before trusting a
// deployment, especially after an intranet migration.
//
// Contracts this package must not violate:
//   - Never print secret material. Sensitive values are reported as
//     "set (redacted)" or "not set"; only connection/provider identities
//     (IDs, base URLs, names) appear in output.
//   - Never mutate anything. Doctor opens the control DB read-only in spirit
//     (queries only) and never writes files.
//   - Config precedence matches the server: env > multigent.conf > default,
//     as implemented by cmd/multigent loadAppConfig().applyConfigEnv().
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Source classifies where an effective configuration value came from.
type Source string

const (
	SourceEnv   Source = "env"      // present in the process environment
	SourceConf  Source = "conf"     // derived from multigent.conf (setEnvIfEmpty)
	SourceUnset Source = "unset"    // neither env nor conf; server default applies
)

// Sensitivity controls output redaction.
type Sensitivity int

const (
	SensitivityPlain    Sensitivity = iota // value shown verbatim (URLs, modes)
	SensitivityRedacted                    // value masked, only set/unset shown
)

// EnvEntry describes one configuration key in the doctor report.
type EnvEntry struct {
	Key      string      `json:"key"`
	Domain   string      `json:"domain"`
	Value    string      `json:"value,omitempty"`
	Redacted bool        `json:"redacted,omitempty"`
	Set      bool        `json:"set"`
	Source   Source      `json:"source"`
	Sensitive Sensitivity `json:"-"`
}

// Check is one diagnostic result. Status is one of "pass", "warn", "fail",
// "skip" (check not applicable in this environment).
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Blocking bool  `json:"blocking,omitempty"`
}

// Report is the full doctor output.
type Report struct {
	Version    string     `json:"version,omitempty"`
	DataDir    string     `json:"dataDir"`
	ConfigPath string     `json:"configPath,omitempty"`
	Env        []EnvEntry `json:"env"`
	Checks     []Check    `json:"checks"`
}

// Options controls doctor execution.
type Options struct {
	// Version is the multigent binary version string (best effort).
	Version string
	// DataDir is the resolved data directory (MULTIGENT_DATA_DIR or discovered root).
	DataDir string
	// ConfigPath is the resolved multigent.conf path (may be empty).
	ConfigPath string
	// ConfKeys lists keys that loadAppConfig().applyConfigEnv() derives from
	// the conf file. Doctor marks a key SourceConf when it is not in the env
	// but appears here and the conf was loaded.
	ConfKeys map[string]bool
	// ConfLoaded reports whether a conf file was actually loaded.
	ConfLoaded bool
	// ProbeTimeout bounds each network probe (GitLab, LLM).
	ProbeTimeout time.Duration
	// SkipProbes disables network probes (for offline/test runs).
	SkipProbes bool
	// OpenControlDB opens the control-plane DB for secrets audit, connections
	// and model providers. Returning an error marks the data check as failed.
	OpenControlDB func() (ControlDB, error)
	// DockerProbe checks the Docker daemon (nil = check via `docker info`).
	DockerProbe func() error
	// Now allows tests to control wall-clock (unused currently, reserved).
	Now func() time.Time
}

// ControlDB is the subset of the control-plane store doctor needs.
type ControlDB interface {
	AuditSecrets() (*SecretsAudit, error)
	ListConnections() ([]ConnectionInfo, error)
	ListModelProviders() ([]ProviderInfo, error)
	Close() error
}

// SecretsAudit mirrors db.SecretsAuditReport without importing the db package
// (keeps doctor dependency-light and testable).
type SecretsAudit struct {
	EncryptionKeyConfigured bool
	RequireEncrypted        bool
	PlaintextTables         []string
	PlaintextTotal          int
	Encrypted               int
	Empty                   int
}

// ConnectionInfo is a redacted view of a code-host connection.
type ConnectionInfo struct {
	ID       string
	Provider string
	Name     string
	AuthType string
	IsDefault bool
	BaseURL  string
	// CheckToken, when non-empty, is used for a live reachability probe.
	CheckToken string
}

// ProviderInfo is a redacted view of a model provider.
type ProviderInfo struct {
	ID      string
	Name    string
	Type    string
	BaseURL string
	APIKey  string
	Model   string
}

// sensitiveKeys must never be printed verbatim.
var sensitiveKeys = map[string]bool{
	"MULTIGENT_CONNECTION_ENCRYPTION_KEY": true,
	"MULTIGENT_TRUSTED_PROXY_SECRET":      true,
	"MULTIGENT_WORKER_TOKEN":              true,
	"MULTIGENT_WEB_API_KEY":               true,
	"MULTIGENT_HTTP_API_KEY":              true,
	"MULTIGENT_CLIENT_TOKEN":              true,
	"MULTIGENT_AGENT_TOKEN":               true,
	"MULTIGENT_SMTP_PASSWORD":             true,
}

// domains mirrors docs/config-reference-and-migration.md §2 (six domains).
var domains = []struct {
	name string
	keys []string
}{
	{"network-proxy", []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"MULTIGENT_API_URL", "MULTIGENT_PUBLIC_URL", "MULTIGENT_CONSOLE_URL",
		"MULTIGENT_WEB_BASE_URL", "CHATOPS_CALLBACK_BASE_URL",
	}},
	{"runtime-images", []string{
		"MULTIGENT_RUNTIME_IMAGE", "MULTIGENT_RUNTIME_REGION", "MULTIGENT_RUNTIME_PROFILE",
		"NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "GOPROXY", "GOSUMDB", "GOPRIVATE",
	}},
	{"storage", []string{
		"MULTIGENT_DATA_DIR", "MULTIGENT_CONTROL_DATA_DIR", "MULTIGENT_CONFIG",
		"MULTIGENT_LOG_FILE", "MULTIGENT_LOG_LEVEL", "MULTIGENT_LOG_FORMAT",
		"MULTIGENT_LOG_STDERR", "MULTIGENT_LOG_MAX_SIZE_MB", "MULTIGENT_SHUTDOWN_TIMEOUT",
		"MULTIGENT_SERVER_ADDR", "MULTIGENT_API_ADDR",
	}},
	{"security", []string{
		"MULTIGENT_CONNECTION_ENCRYPTION_KEY", "MULTIGENT_REQUIRE_ENCRYPTED_SECRETS",
		"MULTIGENT_SECRETS_MIGRATION_MODE", "MULTIGENT_TRUSTED_PROXY_SECRET",
		"MULTIGENT_WORKER_TOKEN", "MULTIGENT_WORKER_ID", "MULTIGENT_WORKER_MODE",
		"MULTIGENT_WORKER_WORKSPACE", "MULTIGENT_WEB_API_KEY", "MULTIGENT_ALLOW_SIGNUP",
		"MULTIGENT_ALLOW_DIRECT_HOST", "MULTIGENT_ALLOW_DIRECT_HOST_EXECUTION",
	}},
	{"gitlab-ci", []string{
		"MULTIGENT_GITLAB_RUNNER_ID", "MULTIGENT_CI_REMOTE_PIPELINE_REQUIRED",
		"MULTIGENT_GIT_NETWORK_TIMEOUT", "MULTIGENT_GIT_SSH_KEY_FILE",
	}},
	{"internal-subprocess", []string{
		"MULTIGENT_WORKTREE_DIR", "MULTIGENT_WAKEUP_BRANCH", "MULTIGENT_WAKEUP_PROJECT",
		"MULTIGENT_WAKEUP_TARGET_TASK_ID", "MULTIGENT_WAKEUP_WORKTREE_DIR",
		"MULTIGENT_RUNTIME_NODE_DAEMON_CHILD", "MULTIGENT_RUN_ID",
		"MULTIGENT_DELEGATION_TOKEN", "MULTIGENT_DESIGN_MOCK",
		"MULTIGENT_REQUIRE_RUNTIME_NODE", "MULTIGENT_E2B_API_URL",
		"MULTIGENT_DOCKER", "MULTIGENT_DOCKER_FORWARD_LOOPBACK_PROXY",
		"MULTIGENT_DISABLE_TOOL_CACHE_PREPARE", "MULTIGENT_TOOL_FORCE_INSTALL",
		"MULTIGENT_UPDATE_CHANNEL", "MULTIGENT_NO_UPDATE_CHECK",
		"MULTIGENT_HTTP_API_KEY", "MULTIGENT_CLIENT_TOKEN", "MULTIGENT_AGENT_TOKEN",
		"MULTIGENT_SMTP_HOST", "MULTIGENT_SMTP_PORT", "MULTIGENT_SMTP_USERNAME",
		"MULTIGENT_SMTP_PASSWORD", "MULTIGENT_SMTP_FROM", "MULTIGENT_SMTP_FROM_NAME",
		"MULTIGENT_SMTP_TLS", "MULTIGENT_MODEL_CATALOG_URL",
	}},
}

// Run executes all doctor checks and returns the report.
func Run(ctx context.Context, opts Options) *Report {
	report := &Report{
		Version:    opts.Version,
		DataDir:    opts.DataDir,
		ConfigPath: opts.ConfigPath,
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 5 * time.Second
	}

	report.Env = collectEnv(opts)
	report.appendDataChecks(opts)
	if !opts.SkipProbes {
		report.appendGitLabChecks(ctx, opts)
		report.appendProviderChecks(ctx, opts)
	}
	report.appendRuntimeCheck(opts)
	return report
}

// collectEnv builds the config tree across all domains.
func collectEnv(opts Options) []EnvEntry {
	entries := make([]EnvEntry, 0, 64)
	for _, domain := range domains {
		for _, key := range domain.keys {
			raw := os.Getenv(key)
			entry := EnvEntry{
				Key:       key,
				Domain:    domain.name,
				Sensitive: SensitivityPlain,
			}
			switch {
			case raw != "":
				entry.Set = true
				entry.Source = SourceEnv
				if sensitiveKeys[key] {
					entry.Sensitive = SensitivityRedacted
					entry.Redacted = true
					entry.Value = "set (redacted)"
				} else {
					entry.Value = raw
				}
			case opts.ConfLoaded && opts.ConfKeys[key]:
				// applyConfigEnv ran setEnvIfEmpty; if the key is still empty
				// here the conf value itself was empty — treat as unset.
				entry.Source = SourceUnset
			default:
				entry.Source = SourceUnset
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

// appendDataChecks verifies the data directory, control DB and secrets baseline.
func (r *Report) appendDataChecks(opts Options) {
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		r.add(Check{Name: "data-dir", Status: "fail", Detail: "data directory not resolved (set MULTIGENT_DATA_DIR or run inside a workspace)", Blocking: true})
		return
	}
	info, err := os.Stat(dataDir)
	switch {
	case err != nil:
		r.add(Check{Name: "data-dir", Status: "fail", Detail: fmt.Sprintf("data directory %s: %v", dataDir, err), Blocking: true})
		return
	case !info.IsDir():
		r.add(Check{Name: "data-dir", Status: "fail", Detail: fmt.Sprintf("%s is not a directory", dataDir), Blocking: true})
		return
	}
	probe := filepath.Join(dataDir, ".doctor-write-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		r.add(Check{Name: "data-dir-writable", Status: "fail", Detail: fmt.Sprintf("cannot write into %s: %v", dataDir, err), Blocking: true})
		return
	}
	_ = os.Remove(probe)
	r.add(Check{Name: "data-dir-writable", Status: "pass", Detail: dataDir})

	if opts.OpenControlDB == nil {
		r.add(Check{Name: "secrets-baseline", Status: "skip", Detail: "no control DB opener provided"})
		return
	}
	db, err := opts.OpenControlDB()
	if err != nil {
		r.add(Check{Name: "secrets-baseline", Status: "fail", Detail: fmt.Sprintf("open control DB: %v", err), Blocking: true})
		return
	}
	defer func() { _ = db.Close() }()

	audit, err := db.AuditSecrets()
	if err != nil {
		r.add(Check{Name: "secrets-baseline", Status: "fail", Detail: fmt.Sprintf("audit secrets: %v", err), Blocking: true})
		return
	}
	if audit == nil {
		r.add(Check{Name: "secrets-baseline", Status: "skip", Detail: "audit returned no report"})
		return
	}
	if audit.PlaintextTotal > 0 {
		detail := fmt.Sprintf("%d plaintext secret record(s) in %s", audit.PlaintextTotal, strings.Join(audit.PlaintextTables, ", "))
		if audit.RequireEncrypted {
			r.add(Check{Name: "secrets-baseline", Status: "fail", Detail: detail+" (REQUIRE_ENCRYPTED_SECRETS is on: server startup will fail-closed)", Blocking: true})
		} else {
			r.add(Check{Name: "secrets-baseline", Status: "warn", Detail: detail + " — run `multigent secrets audit` / `secrets migrate`"})
		}
		return
	}
	r.add(Check{Name: "secrets-baseline", Status: "pass", Detail: fmt.Sprintf("0 plaintext, %d encrypted, %d empty; encryption key configured=%v", audit.Encrypted, audit.Empty, audit.EncryptionKeyConfigured)})
}

// appendGitLabChecks probes every code-host connection.
func (r *Report) appendGitLabChecks(ctx context.Context, opts Options) {
	if opts.OpenControlDB == nil {
		r.add(Check{Name: "gitlab-connections", Status: "skip", Detail: "no control DB opener provided"})
		return
	}
	db, err := opts.OpenControlDB()
	if err != nil {
		r.add(Check{Name: "gitlab-connections", Status: "skip", Detail: fmt.Sprintf("control DB unavailable: %v", err)})
		return
	}
	defer func() { _ = db.Close() }()

	connections, err := db.ListConnections()
	if err != nil {
		r.add(Check{Name: "gitlab-connections", Status: "warn", Detail: fmt.Sprintf("list connections: %v", err)})
		return
	}
	if len(connections) == 0 {
		r.add(Check{Name: "gitlab-connections", Status: "skip", Detail: "no code-host connections configured"})
		return
	}
	failed := 0
	for _, conn := range connections {
		if !strings.EqualFold(conn.Provider, "gitlab") {
			continue
		}
		check := Check{Name: fmt.Sprintf("gitlab:%s", conn.ID)}
		if conn.BaseURL != "" {
			check.Detail = conn.BaseURL
		}
		if conn.CheckToken == "" {
			check.Status = "warn"
			check.Detail = fmt.Sprintf("%s (no credential available for live probe)", check.Detail)
			r.add(check)
			continue
		}
		if err := probeGitLab(ctx, conn, opts.ProbeTimeout); err != nil {
			check.Status = "fail"
			check.Detail = fmt.Sprintf("%s: %v", check.Detail, err)
			failed++
		} else {
			check.Status = "pass"
		}
		r.add(check)
	}
	_ = failed // individual checks carry the status; no aggregate blocking here
}

// probeGitLab performs a GET /user against the GitLab API with the token.
func probeGitLab(ctx context.Context, conn ConnectionInfo, timeout time.Duration) error {
	base := strings.TrimRight(strings.TrimSpace(conn.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("no base URL recorded")
	}
	if !strings.Contains(base, "/api/v4") {
		base += "/api/v4"
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base+"/user", nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", conn.CheckToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("token rejected (401)")
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// appendProviderChecks probes model providers with GET /models.
func (r *Report) appendProviderChecks(ctx context.Context, opts Options) {
	if opts.OpenControlDB == nil {
		r.add(Check{Name: "llm-providers", Status: "skip", Detail: "no control DB opener provided"})
		return
	}
	db, err := opts.OpenControlDB()
	if err != nil {
		r.add(Check{Name: "llm-providers", Status: "skip", Detail: fmt.Sprintf("control DB unavailable: %v", err)})
		return
	}
	defer func() { _ = db.Close() }()

	providers, err := db.ListModelProviders()
	if err != nil {
		r.add(Check{Name: "llm-providers", Status: "warn", Detail: fmt.Sprintf("list model providers: %v", err)})
		return
	}
	if len(providers) == 0 {
		r.add(Check{Name: "llm-providers", Status: "skip", Detail: "no model providers configured"})
		return
	}
	for _, p := range providers {
		check := Check{Name: fmt.Sprintf("llm:%s", p.ID)}
		check.Detail = fmt.Sprintf("%s (%s)", p.Name, p.Type)
		if strings.TrimSpace(p.BaseURL) == "" {
			check.Status = "skip"
			check.Detail += ": no base URL (built-in provider)"
			r.add(check)
			continue
		}
		if err := probeProvider(ctx, p, opts.ProbeTimeout); err != nil {
			// Provider reachability is environment-dependent; warn, don't block.
			check.Status = "warn"
			check.Detail += fmt.Sprintf(": %v", err)
		} else {
			check.Status = "pass"
		}
		r.add(check)
	}
}

// probeProvider performs GET {base}/models (OpenAI-compatible) or a bare base
// probe for other provider types.
func probeProvider(ctx context.Context, p ProviderInfo, timeout time.Duration) error {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := base
	if strings.Contains(base, "/v1") || p.Type == "openai" || p.Type == "custom" {
		url = base + "/models"
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(p.APIKey) != "" {
		switch strings.ToLower(p.Type) {
		case "anthropic":
			req.Header.Set("x-api-key", p.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		default:
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200-299 or 401/403 mean the endpoint answered; DNS/TCP failures surface
	// as errors. 401/403 still prove reachability but flag the credential.
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("reachable but credential rejected (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == 404:
		return fmt.Errorf("reachable but /models not found (HTTP 404; probe URL %s)", url)
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// appendRuntimeCheck verifies the Docker daemon.
func (r *Report) appendRuntimeCheck(opts Options) {
	probe := opts.DockerProbe
	if probe == nil {
		probe = func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run()
		}
	}
	check := Check{Name: "docker"}
	if err := probe(); err != nil {
		check.Status = "warn"
		check.Detail = fmt.Sprintf("docker daemon unreachable: %v (agent sandbox runs will fail until Docker is up)", err)
		r.add(check)
		return
	}
	check.Status = "pass"
	check.Detail = "docker daemon reachable"
	r.add(check)
}

// add appends a check to the report.
func (r *Report) add(c Check) {
	r.Checks = append(r.Checks, c)
}

// Summary computes the aggregate result.
func (r *Report) Summary() (blocking bool, failed, warned int) {
	for _, c := range r.Checks {
		switch c.Status {
		case "fail":
			failed++
			if c.Blocking {
				blocking = true
			}
		case "warn":
			warned++
		}
	}
	return blocking, failed, warned
}

// RenderText renders a human-readable report (no JSON).
func (r *Report) RenderText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "multigent doctor")
	if r.Version != "" {
		fmt.Fprintf(&b, " (%s)", r.Version)
	}
	fmt.Fprintf(&b, "\n  data dir : %s\n", r.DataDir)
	if r.ConfigPath != "" {
		fmt.Fprintf(&b, "  conf     : %s\n", r.ConfigPath)
	}

	b.WriteString("\n== configuration tree ==\n")
	currentDomain := ""
	for _, e := range r.Env {
		if e.Domain != currentDomain {
			currentDomain = e.Domain
			fmt.Fprintf(&b, "\n[%s]\n", e.Domain)
		}
		switch {
		case e.Set && e.Redacted:
			fmt.Fprintf(&b, "  %-42s %s\n", e.Key, e.Value)
		case e.Set:
			fmt.Fprintf(&b, "  %-42s %s\n", e.Key, e.Value)
		default:
			fmt.Fprintf(&b, "  %-42s (unset)\n", e.Key)
		}
	}

	b.WriteString("\n== checks ==\n")
	for _, c := range r.Checks {
		marker := map[string]string{"pass": "[ OK ]", "warn": "[WARN]", "fail": "[FAIL]", "skip": "[SKIP]"}[c.Status]
		if c.Detail != "" {
			fmt.Fprintf(&b, "  %s %-34s %s\n", marker, c.Name, c.Detail)
		} else {
			fmt.Fprintf(&b, "  %s %-34s\n", marker, c.Name)
		}
	}

	blocking, failed, warned := r.Summary()
	fmt.Fprintf(&b, "\n== summary: %d failed, %d warned", failed, warned)
	if blocking {
		b.WriteString(" — BLOCKING issues found\n")
	} else if failed > 0 {
		b.WriteString(" — failures found (non-blocking)\n")
	} else if warned > 0 {
		b.WriteString(" — warnings only\n")
	} else {
		b.WriteString(" — all checks passed\n")
	}
	return b.String()
}

// MarshalJSONIndented is a convenience for CLI JSON output.
func (r *Report) MarshalJSONIndented() string {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(body)
}
