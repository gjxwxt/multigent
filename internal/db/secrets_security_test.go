package db

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSecretsTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedPlaintextFixtures(t *testing.T, db *SQLiteStore) {
	t.Helper()
	if err := db.UpsertWorkspace(Workspace{ID: "ws", Name: "WS", Slug: "ws"}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := db.UpsertConnection(Connection{ID: "conn-1", WorkspaceID: "ws", Provider: "gitlab", ConnectionName: "gitlab-main"}); err != nil {
		t.Fatalf("connection: %v", err)
	}
	if err := db.UpsertConnectionSecret(ConnectionSecret{
		ConnectionID: "conn-1",
		Ciphertext:   base64.StdEncoding.EncodeToString([]byte(`{"apiKey":"glpat-plain"}`)),
		KeyVersion:   "plain-dev",
	}); err != nil {
		t.Fatalf("secret: %v", err)
	}
	if err := db.UpsertModelProvider("ws", ModelProvider{
		ID: "prov-1", WorkspaceID: "ws", OwnerType: "workspace", OwnerID: "ws",
		Name: "llm", Type: "openai",
		APIKey: "sealed:" + base64.StdEncoding.EncodeToString([]byte(`{"ciphertext":"`+base64.StdEncoding.EncodeToString([]byte("sk-plain"))+`"}`)),
	}); err != nil {
		t.Fatalf("model provider: %v", err)
	}
	if err := db.UpsertOAuthClientConfig(OAuthClientConfig{
		WorkspaceID: "ws", Provider: "opendesign", ClientID: "cid",
		SecretCiphertext: base64.StdEncoding.EncodeToString([]byte(`{"clientSecret":"od-plain"}`)),
		KeyVersion:       "dev-plain-base64",
	}); err != nil {
		t.Fatalf("oauth config: %v", err)
	}
}

func TestAuditSecretsClassifiesAllSurfaces(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "")
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "")
	db := newSecretsTestStore(t)
	seedPlaintextFixtures(t, db)

	report, err := db.AuditSecrets()
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(report.Plaintext) != 3 {
		t.Fatalf("plaintext count = %d (%+v), want 3", len(report.Plaintext), report.Plaintext)
	}
	tables := map[string]int{}
	for _, rec := range report.Plaintext {
		tables[rec.Table]++
		// The audit must never include secret bytes.
		if strings.Contains(rec.ID, "glpat") || strings.Contains(rec.Name, "sk-") {
			t.Fatalf("audit leaked secret material: %+v", rec)
		}
	}
	if tables["connections"] != 1 || tables["model_providers"] != 1 || tables["oauth_client_configs"] != 1 {
		t.Fatalf("per-table counts wrong: %v", tables)
	}
	if report.Encrypted != 0 {
		t.Fatalf("encrypted = %d, want 0", report.Encrypted)
	}
}

func TestAuditSecretsSeesEncryptedRows(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "test-key-32-bytes-aaaaaaaaaaaaaaa")
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "")
	db := newSecretsTestStore(t)
	if err := db.UpsertWorkspace(Workspace{ID: "ws", Name: "WS", Slug: "ws"}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := db.UpsertConnection(Connection{ID: "conn-enc", WorkspaceID: "ws", Provider: "gitlab"}); err != nil {
		t.Fatalf("connection: %v", err)
	}
	sealed, err := SealConnectionSecret(map[string]string{"apiKey": "glpat-x"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed.KeyVersion != "env-v1" {
		t.Fatalf("key version = %q, want env-v1", sealed.KeyVersion)
	}
	sealed.ConnectionID = "conn-enc"
	if err := db.UpsertConnectionSecret(sealed); err != nil {
		t.Fatalf("upsert secret: %v", err)
	}

	report, err := db.AuditSecrets()
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(report.Plaintext) != 0 || report.Encrypted != 1 {
		t.Fatalf("audit = plaintext:%d encrypted:%d, want 0/1", len(report.Plaintext), report.Encrypted)
	}
}

func TestMigrateSecretsReSealsAllSurfaces(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "migrate-key-32-bytes-bbbbbbbbbbb")
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "")
	db := newSecretsTestStore(t)
	seedPlaintextFixtures(t, db)

	report, err := db.MigrateSecrets()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if report.ReEncrypted != 3 || len(report.Failed) != 0 {
		t.Fatalf("migrate result = %+v, want 3 re-encrypted 0 failed", report)
	}

	// Post-conditions: zero plaintext, and every record still decrypts to its
	// original value through the normal open paths.
	after, err := db.AuditSecrets()
	if err != nil {
		t.Fatalf("post audit: %v", err)
	}
	if len(after.Plaintext) != 0 || after.Encrypted != 3 {
		t.Fatalf("post audit = plaintext:%d encrypted:%d, want 0/3", len(after.Plaintext), after.Encrypted)
	}
	secret, found, err := db.ConnectionSecret("conn-1")
	if err != nil || !found {
		t.Fatalf("reload secret: found=%v err=%v", found, err)
	}
	values, err := OpenConnectionSecret(secret)
	if err != nil {
		t.Fatalf("open migrated secret: %v", err)
	}
	if values["apiKey"] != "glpat-plain" {
		t.Fatalf("migrated connection secret roundtrip failed: %v", values)
	}
	provider, found, err := db.ModelProviderByID("ws", "prov-1")
	if err != nil || !found {
		t.Fatalf("reload provider: found=%v err=%v", found, err)
	}
	if got := secretboxEnvelopeVersion(provider.APIKey); got != "env-v1" {
		t.Fatalf("provider api key version = %q, want env-v1", got)
	}
	cfg, found, err := db.OAuthClientConfigByProvider("ws", "opendesign")
	if err != nil || !found {
		t.Fatalf("reload oauth config: found=%v err=%v", found, err)
	}
	if cfg.KeyVersion != "aes-gcm-sha256-env" {
		t.Fatalf("oauth key version = %q, want aes-gcm-sha256-env", cfg.KeyVersion)
	}
}

func TestMigrateSecretsRequiresKey(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "")
	db := newSecretsTestStore(t)
	if _, err := db.MigrateSecrets(); err == nil || !strings.Contains(err.Error(), "MULTIGENT_CONNECTION_ENCRYPTION_KEY") {
		t.Fatalf("migrate without key must fail naming the env, got %v", err)
	}
}

func TestMigrateSecretsIsIdempotent(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "idem-key-32-bytes-cccccccccccccc")
	db := newSecretsTestStore(t)
	seedPlaintextFixtures(t, db)
	if _, err := db.MigrateSecrets(); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	second, err := db.MigrateSecrets()
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if second.ReEncrypted != 0 {
		t.Fatalf("second migrate re-encrypted %d records, want 0", second.ReEncrypted)
	}
}

func TestRequireEncryptedSecretsBlocksPlaintextConnectionSeal(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "")
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "1")
	if _, err := SealConnectionSecret(map[string]string{"apiKey": "x"}); err == nil || !strings.Contains(err.Error(), "MULTIGENT_REQUIRE_ENCRYPTED_SECRETS") {
		t.Fatalf("seal must refuse plaintext under the hard gate, got %v", err)
	}
	// Without the gate the historical dev fallback still works.
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "")
	sealed, err := SealConnectionSecret(map[string]string{"apiKey": "x"})
	if err != nil || sealed.KeyVersion != "plain-dev" {
		t.Fatalf("dev fallback broken: %v %q", err, sealed.KeyVersion)
	}
}

func TestEnvFlagSet(t *testing.T) {
	cases := map[string]bool{"1": true, "true": true, "YES": true, "on": true, "": false, "0": false, "false": false}
	for value, want := range cases {
		t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", value)
		if got := RequireEncryptedSecrets(); got != want {
			t.Fatalf("RequireEncryptedSecrets(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestMain(m *testing.M) {
	// Keep the developer environment honest: these tests must run without a
	// host-provided master key unless they set one themselves.
	if os.Getenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY") != "" && os.Getenv("SECRETS_TEST_ALLOW_HOST_KEY") == "" {
		os.Unsetenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY")
	}
	os.Exit(m.Run())
}

var _ = json.Marshal
