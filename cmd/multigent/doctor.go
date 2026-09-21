package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/doctor"
	"github.com/multigent/multigent/internal/secretbox"
	"github.com/multigent/multigent/internal/sandbox"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var (
		jsonOut    bool
		skipProbes bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Server-side self-check: config tree, data dir, secrets, GitLab/LLM reachability, Docker",
		Long: `Read-only diagnostic for operators. Reports:

  - the effective MULTIGENT_* configuration tree (env vs multigent.conf vs unset),
    grouped by the domains in docs/config-reference-and-migration.md §2
  - data directory writability and the secrets baseline (plaintext count;
    REQUIRE_ENCRYPTED_SECRETS fail-closed readiness)
  - GitLab connection reachability (live GET /user per connection, token redacted)
  - model provider reachability (GET {base}/models, credential rejected = warning)
  - Docker daemon readiness for agent sandboxes

Exit codes: 0 = no blocking issues; 1 = blocking issues found (data dir
unusable, plaintext secrets under REQUIRE_ENCRYPTED_SECRETS, control DB
unreadable). Warnings alone do not change the exit code.

Secret material is never printed. Doctor never writes anything except a
transient write-probe file inside the data directory (removed immediately).`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDoctor(jsonOut, skipProbes)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of text")
	cmd.Flags().BoolVar(&skipProbes, "offline", false, "skip network probes (GitLab/LLM); config tree and local checks only")
	return cmd
}

func runDoctor(jsonOut bool, skipProbes bool) error {
	if _, confErr := loadAppConfig(); confErr != nil {
		// A broken conf is itself a diagnosis; keep going and surface it.
		fmt.Fprintf(os.Stderr, "doctor: config load failed: %v\n", confErr)
	}

	opts := doctor.Options{
		Version:      version,
		DataDir:      resolveDoctorDataDir(),
		ConfigPath:   activeConfigPath(),
		ConfLoaded:   strings.TrimSpace(activeConfigPath()) != "",
		ConfKeys:     confDerivedKeys(),
		SkipProbes:   skipProbes,
		OpenControlDB: openDoctorControlDB,
		DockerProbe:  sandbox.CheckDocker,
	}

	report := doctor.Run(context.Background(), opts)
	if jsonOut {
		fmt.Println(report.MarshalJSONIndented())
	} else {
		fmt.Print(report.RenderText())
	}

	blocking, _, _ := report.Summary()
	if blocking {
		os.Exit(1)
	}
	return nil
}

// resolveDoctorDataDir mirrors the server's data-dir resolution chain:
// MULTIGENT_DATA_DIR > MULTIGENT_CONTROL_DATA_DIR > --dir/cwd discovery.
func resolveDoctorDataDir() string {
	if dataDir := strings.TrimSpace(os.Getenv("MULTIGENT_DATA_DIR")); dataDir != "" {
		return dataDir
	}
	if dataDir := strings.TrimSpace(os.Getenv("MULTIGENT_CONTROL_DATA_DIR")); dataDir != "" {
		return dataDir
	}
	root, err := resolveRoot()
	if err != nil {
		return ""
	}
	return root
}

// confDerivedKeys lists every key applyConfigEnv may derive from multigent.conf.
// Doctor uses this only to label sources; a key absent from the env is "unset"
// regardless (an empty conf value contributes nothing).
func confDerivedKeys() map[string]bool {
	return map[string]bool{
		"MULTIGENT_SMTP_HOST":                true,
		"MULTIGENT_SMTP_PORT":                true,
		"MULTIGENT_SMTP_USERNAME":            true,
		"MULTIGENT_SMTP_PASSWORD":            true,
		"MULTIGENT_SMTP_FROM":                true,
		"MULTIGENT_SMTP_FROM_NAME":           true,
		"MULTIGENT_SMTP_TLS":                 true,
		"MULTIGENT_RUNTIME_IMAGE":            true,
		"MULTIGENT_RUNTIME_REGION":           true,
		"MULTIGENT_RUNTIME_PROFILE":          true,
		"NPM_CONFIG_REGISTRY":                true,
		"PIP_INDEX_URL":                      true,
		"GOPROXY":                            true,
		"GOSUMDB":                            true,
		"GOPRIVATE":                          true,
		"HTTPS_PROXY":                        true,
		"https_proxy":                        true,
		"HTTP_PROXY":                         true,
		"http_proxy":                         true,
		"NO_PROXY":                           true,
		"no_proxy":                           true,
		"MULTIGENT_ALLOW_DIRECT_HOST":        true,
		"MULTIGENT_E2B_API_URL":              true,
		"MULTIGENT_PLAYBOOK_REGISTRY_URLS":   true,
	}
}

// openDoctorControlDB opens the control-plane DB and adapts it to the doctor
// interface, performing credential decryption for GitLab probes (redacted in
// output by the doctor package).
func openDoctorControlDB() (doctor.ControlDB, error) {
	control, err := db.OpenDefault()
	if err != nil {
		return nil, fmt.Errorf("open control DB: %w", err)
	}
	return &doctorControlDB{db: control}, nil
}

// doctorControlDB adapts db.SQLiteStore to doctor.ControlDB. Connection and
// provider views are redacted to the fields doctor is allowed to see.
type doctorControlDB struct {
	db *db.SQLiteStore
}

func (a *doctorControlDB) AuditSecrets() (*doctor.SecretsAudit, error) {
	report, err := a.db.AuditSecrets()
	if err != nil {
		return nil, err
	}
	out := &doctor.SecretsAudit{
		EncryptionKeyConfigured: report.EncryptionKeyConfigured,
		RequireEncrypted:        report.RequireEncrypted,
		Encrypted:               report.Encrypted,
		Empty:                   report.Empty,
	}
	for _, table := range report.Plaintext {
		out.PlaintextTables = append(out.PlaintextTables, fmt.Sprintf("%s[%s]", table.Table, table.ID))
		out.PlaintextTotal++
	}
	return out, nil
}

func (a *doctorControlDB) ListConnections() ([]doctor.ConnectionInfo, error) {
	rows, err := a.db.ListConnections(db.ConnectionFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]doctor.ConnectionInfo, 0, len(rows))
	for _, row := range rows {
		info := doctor.ConnectionInfo{
			ID:        row.ID,
			Provider:  row.Provider,
			Name:      row.ConnectionName,
			AuthType:  row.AuthType,
			BaseURL:   profileBaseURL(row.ProfileJSON),
		}
		// Credential resolution mirrors the server path: decrypt the stored
		// secret; the secret values carry both the token and (typically) the
		// baseUrl. Failure to decrypt is reported as an unreachable connection
		// rather than crashing doctor.
		if secret, found, secretErr := a.db.ConnectionSecret(row.ID); secretErr == nil && found {
			if values, openErr := db.OpenConnectionSecret(secret); openErr == nil {
				info.CheckToken = strings.TrimSpace(values["apiKey"])
				if strings.TrimSpace(info.BaseURL) == "" {
					info.BaseURL = strings.TrimSpace(values["baseUrl"])
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (a *doctorControlDB) ListModelProviders() ([]doctor.ProviderInfo, error) {
	workspaces, err := a.db.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	var out []doctor.ProviderInfo
	for _, workspace := range workspaces {
		rows, err := a.db.ListModelProviders(workspace.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			info := doctor.ProviderInfo{
				ID:      row.ID,
				Name:    row.Name,
				Type:    row.Type,
				BaseURL: row.BaseURL,
				Model:   row.Model,
			}
			if strings.TrimSpace(row.APIKey) != "" {
				if opened, openErr := secretbox.OpenString(row.APIKey); openErr == nil {
					info.APIKey = opened
				}
			}
			out = append(out, info)
		}
	}
	return out, nil
}

func (a *doctorControlDB) Close() error {
	return a.db.Close()
}

// profileBaseURL extracts baseUrl from a connection profile JSON.
func profileBaseURL(profileJSON string) string {
	if strings.TrimSpace(profileJSON) == "" {
		return ""
	}
	var profile struct {
		BaseURL string `json:"baseUrl"`
	}
	if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
		return ""
	}
	return strings.TrimSpace(profile.BaseURL)
}
