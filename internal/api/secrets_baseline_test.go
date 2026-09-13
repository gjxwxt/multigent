package api

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// baselineServer builds a Server whose controlDB points at a fresh SQLite
// file store, suitable for exercising the startup secret baseline gate.
func baselineServer(t *testing.T) *Server {
	t.Helper()
	store, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open control db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return &Server{controlDB: store}
}

func requireGateEnv(t *testing.T, require, migrationMode bool) {
	t.Helper()
	t.Setenv(controldb.EnvRequireEncryptedSecrets, "")
	t.Setenv("MULTIGENT_SECRETS_MIGRATION_MODE", "")
	if require {
		t.Setenv(controldb.EnvRequireEncryptedSecrets, "1")
	}
	if migrationMode {
		t.Setenv("MULTIGENT_SECRETS_MIGRATION_MODE", "1")
	}
}

func seedPlaintextConnection(t *testing.T, store *controldb.SQLiteStore) {
	t.Helper()
	if err := store.UpsertWorkspace(controldb.Workspace{ID: "ws-b", Name: "ws-b"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := store.UpsertConnection(controldb.Connection{ID: "conn-plain", WorkspaceID: "ws-b", Provider: "gitlab", ConnectionName: "conn-plain"}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	secret := controldb.ConnectionSecret{
		ConnectionID: "conn-plain",
		Ciphertext:   base64Of(`{"token":"t"}`),
		KeyVersion:   "plain-dev",
	}
	if err := store.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("seed plaintext secret: %v", err)
	}
}

// REQUIRE=1 with existing plaintext records must fail closed: the process
// exits 1 rather than serving with plaintext secrets on disk.
func TestSecretsBaselineRequireGateFailsClosedOnPlaintext(t *testing.T) {
	s := baselineServer(t)
	seedPlaintextConnection(t, s.controlDB.(*controldb.SQLiteStore))
	requireGateEnv(t, true, false)

	err := s.EnforceSecretsBaseline()
	if err == nil {
		t.Fatal("REQUIRE=1 with plaintext records must return a startup error (fail-closed)")
	}
}

// The same DB without the REQUIRE gate stays visibility-only: no exit.
func TestSecretsBaselineWarnOnlyWithoutRequire(t *testing.T) {
	s := baselineServer(t)
	seedPlaintextConnection(t, s.controlDB.(*controldb.SQLiteStore))
	requireGateEnv(t, false, false)

	if err := s.EnforceSecretsBaseline(); err != nil {
		t.Fatalf("baseline audit without REQUIRE must not fail startup: %v", err)
	}
}

// Migration mode is the single sanctioned exemption: the migrate/verify run
// must be able to read old-format rows with REQUIRE armed.
func TestSecretsBaselineMigrationModeAllowsPlaintext(t *testing.T) {
	s := baselineServer(t)
	seedPlaintextConnection(t, s.controlDB.(*controldb.SQLiteStore))
	requireGateEnv(t, true, true)

	if err := s.EnforceSecretsBaseline(); err != nil {
		t.Fatalf("migration mode must exempt the startup gate: %v", err)
	}
}

// A fully encrypted store passes the armed gate cleanly.
func TestSecretsBaselineRequireGatePassesEncryptedStore(t *testing.T) {
	s := baselineServer(t)
	store := s.controlDB.(*controldb.SQLiteStore)
	requireGateEnv(t, true, false)
	t.Setenv(controldb.EnvConnectionEncryptionKey, "baseline-key")
	t.Cleanup(func() { os.Unsetenv(controldb.EnvConnectionEncryptionKey) })
	seedPlaintextConnection(t, store)

	report, err := store.MigrateSecrets()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if report.ReEncrypted != 1 || len(report.Failed) != 0 {
		t.Fatalf("migrate report = %+v, want 1 re-encrypted, 0 failed", report)
	}
	remaining, err := store.CountPlaintextSecretRecords()
	if err != nil || remaining != 0 {
		t.Fatalf("plaintext remaining = %d err=%v, want 0", remaining, err)
	}

	if err := s.EnforceSecretsBaseline(); err != nil {
		t.Fatalf("armed gate must pass after migration: %v", err)
	}
}
