package db

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Secret storage security baseline (intranet handover, 2026-09-14):
// every secret-bearing table has a plaintext dev mode keyed by the absence of
// MULTIGENT_CONNECTION_ENCRYPTION_KEY. Three shapes exist:
//
//   - connection_secrets:         "plain-dev"        (base64 JSON, no nonce)
//   - model_providers.api_key:    secretbox "plain-dev" ("sealed:" envelope)
//   - oauth_client_configs:       "dev-plain-base64" (base64 JSON, hex nonce)
//
// The audit/migration helpers below treat all three uniformly. The single
// source of truth for "is this row plaintext" is the key_version column (or
// the sealed: envelope's KeyVersion for model providers); ciphertext bytes
// are never logged or exported in plaintext form by these helpers.

// SecretPlaintextRecords is the audit summary for one storage surface.
type SecretPlaintextRecords struct {
	// Table is the storage surface: "connections", "model_providers",
	// "oauth_client_configs".
	Table string `json:"table"`
	// WorkspaceID scopes the count ("" for connection secrets, which are
	// workspace-scoped through the connections table itself).
	WorkspaceID string `json:"workspaceId,omitempty"`
	// ID identifies the record within the table (connection ID, provider ID,
	// or workspace/provider pair for OAuth rows joined with "/").
	ID string `json:"id"`
	// Name is a human-readable label that never contains secret material.
	Name string `json:"name,omitempty"`
	// KeyVersion is the stored version marker ("plain-dev",
	// "dev-plain-base64", or the sealed envelope's version).
	KeyVersion string `json:"keyVersion"`
}

// SecretsAuditReport is the full plaintext inventory across all surfaces.
type SecretsAuditReport struct {
	EncryptionKeyConfigured bool                     `json:"encryptionKeyConfigured"`
	RequireEncrypted        bool                     `json:"requireEncryptedEnv"`
	Plaintext               []SecretPlaintextRecords `json:"plaintext"`
	Encrypted               int                      `json:"encrypted"`
	Empty                   int                      `json:"empty"`
}

// AuditSecrets inventories every secret-bearing record and classifies it as
// plaintext, encrypted, or empty. It never decrypts anything and never
// returns secret material.
func (db *SQLiteStore) AuditSecrets() (*SecretsAuditReport, error) {
	report := &SecretsAuditReport{
		EncryptionKeyConfigured: strings.TrimSpace(os.Getenv(EnvConnectionEncryptionKey)) != "",
		RequireEncrypted:        envFlagSet(EnvRequireEncryptedSecrets),
	}
	// connection_secrets join connections for workspace/name labels.
	rows, err := db.sql.Query(`SELECT s.connection_id, s.key_version, c.workspace_id, c.connection_name
		FROM connection_secrets s LEFT JOIN connections c ON c.id = s.connection_id`)
	if err != nil {
		return nil, fmt.Errorf("audit connection secrets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, keyVersion, workspaceID, name string
		if err := rows.Scan(&id, &keyVersion, &workspaceID, &name); err != nil {
			return nil, err
		}
		report.classify("connections", id, workspaceID, name, keyVersion)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// model_providers: api_key carries the sealed envelope.
	mpRows, err := db.sql.Query(`SELECT id, workspace_id, name, api_key FROM model_providers`)
	if err != nil {
		return nil, fmt.Errorf("audit model providers: %w", err)
	}
	defer mpRows.Close()
	for mpRows.Next() {
		var id, workspaceID, name, apiKey string
		if err := mpRows.Scan(&id, &workspaceID, &name, &apiKey); err != nil {
			return nil, err
		}
		if strings.TrimSpace(apiKey) == "" {
			report.Empty++
			continue
		}
		version := secretboxEnvelopeVersion(apiKey)
		report.classify("model_providers", id, workspaceID, name, version)
	}
	if err := mpRows.Err(); err != nil {
		return nil, err
	}

	// oauth_client_configs: one row per workspace+provider.
	oaRows, err := db.sql.Query(`SELECT workspace_id, provider, key_version FROM oauth_client_configs`)
	if err != nil {
		return nil, fmt.Errorf("audit oauth client configs: %w", err)
	}
	defer oaRows.Close()
	for oaRows.Next() {
		var workspaceID, provider, keyVersion string
		if err := oaRows.Scan(&workspaceID, &provider, &keyVersion); err != nil {
			return nil, err
		}
		report.classify("oauth_client_configs", workspaceID+"/"+provider, workspaceID, provider, keyVersion)
	}
	if err := oaRows.Err(); err != nil {
		return nil, err
	}
	return report, nil
}

func (r *SecretsAuditReport) classify(table, id, workspaceID, name, keyVersion string) {
	switch secretStorageMode(keyVersion) {
	case secretModePlaintext:
		r.Plaintext = append(r.Plaintext, SecretPlaintextRecords{
			Table: table, WorkspaceID: workspaceID, ID: id, Name: name, KeyVersion: keyVersion,
		})
	case secretModeEncrypted:
		r.Encrypted++
	default:
		// Unknown version markers count as plaintext: an unrecognized scheme
		// must fail the audit loudly rather than pass as encrypted.
		r.Plaintext = append(r.Plaintext, SecretPlaintextRecords{
			Table: table, WorkspaceID: workspaceID, ID: id, Name: name, KeyVersion: keyVersion,
		})
	}
}

const (
	secretModePlaintext = "plaintext"
	secretModeEncrypted = "encrypted"
	secretModeEmpty     = "empty"
)

func secretStorageMode(keyVersion string) string {
	switch strings.TrimSpace(keyVersion) {
	case "plain-dev", "dev-plain-base64", "":
		return secretModePlaintext
	case "env-v1", "aes-gcm-sha256-env":
		return secretModeEncrypted
	default:
		return "unknown"
	}
}

// secretboxEnvelopeVersion reads the KeyVersion out of a "sealed:" envelope
// without decrypting. Values outside the envelope format are reported as
// unknown (audited as plaintext).
func secretboxEnvelopeVersion(value string) string {
	const prefix = "sealed:"
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, prefix) {
		return "raw"
	}
	rawBox, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, prefix))
	if err != nil {
		return "unknown"
	}
	var box struct {
		KeyVersion string `json:"keyVersion"`
	}
	if err := json.Unmarshal(rawBox, &box); err != nil {
		return "unknown"
	}
	if strings.TrimSpace(box.KeyVersion) == "" {
		return "plain-dev"
	}
	return box.KeyVersion
}

// SecretsMigrationResult reports one migrated record.
type SecretsMigrationResult struct {
	Table       string `json:"table"`
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	From        string `json:"from"`
	To          string `json:"to"`
}

// SecretsMigrateReport is the outcome of an --apply migration run.
type SecretsMigrateReport struct {
	BackupPath  string                  `json:"backupPath,omitempty"`
	ReEncrypted int                     `json:"reEncrypted"`
	Failed      []SecretsMigrationError `json:"failed,omitempty"`
	Details     []SecretsMigrationResult `json:"details,omitempty"`
}

// SecretsMigrationError records one record that could not be migrated. The
// migration continues past individual failures so a single corrupt row does
// not strand the rest; the error names the record, never the secret bytes.
type SecretsMigrationError struct {
	Table string `json:"table"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

// MigrateSecrets re-encrypts every plaintext record with the current
// MULTIGENT_CONNECTION_ENCRYPTION_KEY. It refuses to run without the key and
// refuses when nothing is plaintext (no-op safety). Decryption failures are
// recorded per-record and do not abort the run: an unreadable plaintext row
// is left untouched for manual handling rather than silently dropped.
func (db *SQLiteStore) MigrateSecrets() (*SecretsMigrateReport, error) {
	if strings.TrimSpace(os.Getenv(EnvConnectionEncryptionKey)) == "" {
		return nil, fmt.Errorf("%s must be set to migrate secrets", EnvConnectionEncryptionKey)
	}
	report := &SecretsMigrateReport{}

	// connection_secrets: decrypt raw base64 JSON, re-seal env-v1.
	connRows, err := db.sql.Query(`SELECT s.connection_id, s.ciphertext FROM connection_secrets s
		WHERE COALESCE(s.key_version, '') IN ('', 'plain-dev')`)
	if err != nil {
		return nil, fmt.Errorf("scan connection secrets: %w", err)
	}
	type pendingSecret struct {
		table, id, workspaceID, from string
		apply                        func() error
	}
	var pending []pendingSecret
	for connRows.Next() {
		var id, ciphertext string
		if err := connRows.Scan(&id, &ciphertext); err != nil {
			connRows.Close()
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(ciphertext)
		if err != nil {
			report.Failed = append(report.Failed, SecretsMigrationError{Table: "connections", ID: id, Error: "decode plaintext: " + err.Error()})
			continue
		}
		var values map[string]string
		if err := json.Unmarshal(raw, &values); err != nil {
			report.Failed = append(report.Failed, SecretsMigrationError{Table: "connections", ID: id, Error: "parse plaintext: " + err.Error()})
			continue
		}
		secret := id
		capturedValues := values
		pending = append(pending, pendingSecret{
			table: "connections", id: id, from: "plain-dev",
			apply: func() error {
				sealed, err := SealConnectionSecret(capturedValues)
				if err != nil {
					return err
				}
				if err := db.UpsertConnectionSecretForID(secret, sealed); err != nil {
					return err
				}
				return nil
			},
		})
	}
	connRows.Close()
	if err := connRows.Err(); err != nil {
		return nil, err
	}

	// model_providers: open sealed envelope, re-seal with current env.
	mpRows, err := db.sql.Query(`SELECT id, api_key FROM model_providers`)
	if err != nil {
		return nil, fmt.Errorf("scan model providers: %w", err)
	}
	for mpRows.Next() {
		var id, apiKey string
		if err := mpRows.Scan(&id, &apiKey); err != nil {
			mpRows.Close()
			return nil, err
		}
		if strings.TrimSpace(apiKey) == "" {
			continue
		}
		version := secretboxEnvelopeVersion(apiKey)
		if secretStorageMode(version) != secretModePlaintext {
			continue
		}
		providerID := id
		plainValue := apiKey
		pending = append(pending, pendingSecret{
			table: "model_providers", id: id, from: version,
			apply: func() error {
				// Decrypt the plaintext envelope (base64 JSON with the secret
				// under "ciphertext", itself base64 of the raw key).
				raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(plainValue), "sealed:"))
				if err != nil {
					return fmt.Errorf("decode envelope: %w", err)
				}
				var box struct {
					Ciphertext string `json:"ciphertext"`
				}
				if err := json.Unmarshal(raw, &box); err != nil {
					return fmt.Errorf("parse envelope: %w", err)
				}
				plain, err := base64.StdEncoding.DecodeString(box.Ciphertext)
				if err != nil {
					return fmt.Errorf("decode plaintext: %w", err)
				}
				key := strings.TrimSpace(os.Getenv(EnvConnectionEncryptionKey))
				if key == "" {
					return fmt.Errorf("%s is required to seal secrets", EnvConnectionEncryptionKey)
				}
				sealed, err := sealAESGCM(plain, key, "env-v1", false)
				if err != nil {
					return err
				}
				env, err := json.Marshal(struct {
					Ciphertext string `json:"ciphertext"`
					Nonce      string `json:"nonce,omitempty"`
					KeyVersion string `json:"keyVersion"`
				}{Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, KeyVersion: sealed.KeyVersion})
				if err != nil {
					return err
				}
				_, err = db.sql.Exec(`UPDATE model_providers SET api_key = ?, updated_at = ? WHERE id = ?`,
					"sealed:"+base64.StdEncoding.EncodeToString(env), nowUTC(), providerID)
				return err
			},
		})
	}
	mpRows.Close()
	if err := mpRows.Err(); err != nil {
		return nil, err
	}

	// oauth_client_configs: dev-plain-base64 → aes-gcm-sha256-env.
	oaRows, err := db.sql.Query(`SELECT workspace_id, provider, secret_ciphertext FROM oauth_client_configs
		WHERE COALESCE(key_version, '') IN ('', 'dev-plain-base64')`)
	if err != nil {
		return nil, fmt.Errorf("scan oauth client configs: %w", err)
	}
	for oaRows.Next() {
		var workspaceID, provider, ciphertext string
		if err := oaRows.Scan(&workspaceID, &provider, &ciphertext); err != nil {
			oaRows.Close()
			return nil, err
		}
		key := workspaceID + "/" + provider
		pending = append(pending, pendingSecret{
			table: "oauth_client_configs", id: key, workspaceID: workspaceID, from: "dev-plain-base64",
			apply: func() error {
				raw, err := base64.StdEncoding.DecodeString(ciphertext)
				if err != nil {
					return fmt.Errorf("decode plaintext: %w", err)
				}
				var payload map[string]string
				if err := json.Unmarshal(raw, &payload); err != nil {
					return fmt.Errorf("parse plaintext: %w", err)
				}
				sealed, err := sealOAuthClientSecretValue(payload["clientSecret"])
				if err != nil {
					return err
				}
				_, err = db.sql.Exec(`UPDATE oauth_client_configs SET secret_ciphertext = ?, nonce = ?, key_version = 'aes-gcm-sha256-env', updated_at = ? WHERE workspace_id = ? AND provider = ?`,
					sealed.Ciphertext, sealed.Nonce, nowUTC(), workspaceID, provider)
				return err
			},
		})
	}
	oaRows.Close()
	if err := oaRows.Err(); err != nil {
		return nil, err
	}

	if len(pending) == 0 {
		return report, nil
	}
	for _, item := range pending {
		if err := item.apply(); err != nil {
			report.Failed = append(report.Failed, SecretsMigrationError{Table: item.table, ID: item.id, Error: err.Error()})
			continue
		}
		report.ReEncrypted++
		report.Details = append(report.Details, SecretsMigrationResult{
			Table: item.table, ID: item.id, WorkspaceID: item.workspaceID, From: item.from, To: "encrypted",
		})
	}
	return report, nil
}

// UpsertConnectionSecretForID writes a re-sealed secret back under its
// existing connection ID without touching the connection row itself.
func (db *SQLiteStore) UpsertConnectionSecretForID(connectionID string, secret ConnectionSecret) error {
	secret.ConnectionID = connectionID
	if secret.UpdatedAt == "" {
		secret.UpdatedAt = nowUTC()
	}
	_, err := db.sql.Exec(`INSERT INTO connection_secrets (connection_id, ciphertext, nonce, key_version, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(connection_id) DO UPDATE SET
		ciphertext = excluded.ciphertext,
		nonce = excluded.nonce,
		key_version = excluded.key_version,
		updated_at = excluded.updated_at`,
		secret.ConnectionID, secret.Ciphertext, secret.Nonce, secret.KeyVersion, secret.UpdatedAt)
	return err
}

// CountPlaintextSecretRecords returns the number of plaintext secret records
// across all surfaces. Cheap enough for the startup gate.
func (db *SQLiteStore) CountPlaintextSecretRecords() (int, error) {
	report, err := db.AuditSecrets()
	if err != nil {
		return 0, err
	}
	return len(report.Plaintext), nil
}

// EnvRequireEncryptedSecrets turns the plaintext fallback into a hard error
// for every seal path (connections, model providers, OAuth clients). Failure
// is loud and at write time — the deploy cannot silently accumulate new
// plaintext rows.
const EnvRequireEncryptedSecrets = "MULTIGENT_REQUIRE_ENCRYPTED_SECRETS"

// EnvConnectionEncryptionKey is the shared master key env (declared here once
// for the audit/migration helpers; the seal paths read it directly).
const EnvConnectionEncryptionKey = "MULTIGENT_CONNECTION_ENCRYPTION_KEY"

func envFlagSet(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// RequireEncryptedSecrets reports whether the hard gate is enabled.
func RequireEncryptedSecrets() bool {
	return envFlagSet(EnvRequireEncryptedSecrets)
}

// sealOAuthClientSecretValue re-seals an OAuth client secret into the
// aes-gcm-sha256-env shape used by oauth_client_configs.
func sealOAuthClientSecretValue(value string) (ConnectionSecret, error) {
	key := strings.TrimSpace(os.Getenv(EnvConnectionEncryptionKey))
	if key == "" {
		return ConnectionSecret{}, fmt.Errorf("%s is required to seal secrets", EnvConnectionEncryptionKey)
	}
	raw, err := json.Marshal(map[string]string{"clientSecret": value})
	if err != nil {
		return ConnectionSecret{}, err
	}
	return sealAESGCM(raw, key, "aes-gcm-sha256-env", true)
}
