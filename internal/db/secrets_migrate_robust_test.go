package db

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Migration robustness drills (intranet handover): interruption, concurrent
// writers, corrupt plaintext rows, and restore-from-backup must all preserve
// the invariant that secrets are never lost and the run is re-runnable.

func migrateFixture(t *testing.T) (*SQLiteStore, string) {
	t.Helper()
	dir := t.TempDir()
	live := filepath.Join(dir, "multigent.db")
	store, err := Open(live)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	t.Setenv(EnvConnectionEncryptionKey, "robust-key")
	if err := store.UpsertWorkspace(Workspace{ID: "ws-m", Name: "ws-m"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	for i, name := range []string{"c1", "c2", "c3"} {
		if err := store.UpsertConnection(Connection{ID: name, WorkspaceID: "ws-m", Provider: "gitlab", ConnectionName: name}); err != nil {
			t.Fatalf("seed connection %s: %v", name, err)
		}
		plain := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"token":"t%d"}`, i)))
		if err := store.UpsertConnectionSecret(ConnectionSecret{ConnectionID: name, Ciphertext: plain, KeyVersion: "plain-dev"}); err != nil {
			t.Fatalf("seed secret %s: %v", name, err)
		}
	}
	return store, live
}

// Interruption: a per-record write failure mid-run must leave the untouched
// records still plaintext (re-runnable) and the failed ones reported, never
// deleted or half-rewritten into an unreadable state.
func TestMigrateSecretsInterruptionLeavesReRunnableState(t *testing.T) {
	store, _ := migrateFixture(t)

	// Corrupt one row so its apply() fails mid-run: the other two must still
	// migrate and the failed one must stay exactly as it was.
	corrupt := base64.StdEncoding.EncodeToString([]byte("not json at all"))
	if _, err := store.sql.Exec(`UPDATE connection_secrets SET ciphertext = ? WHERE connection_id = 'c2'`, corrupt); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	report, err := store.MigrateSecrets()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if report.ReEncrypted != 2 {
		t.Fatalf("re-encrypted = %d, want 2", report.ReEncrypted)
	}
	if len(report.Failed) != 1 || report.Failed[0].ID != "c2" {
		t.Fatalf("failed = %+v, want exactly c2", report.Failed)
	}

	// The failed row keeps its original (corrupt) plaintext bytes: untouched,
	// identifiable, and safe to retry or hand-fix.
	secret, found, err := store.ConnectionSecret("c2")
	if err != nil || !found {
		t.Fatalf("c2 must survive: found=%v err=%v", found, err)
	}
	if secret.KeyVersion != "plain-dev" {
		t.Fatalf("c2 key_version = %q, want unchanged plain-dev", secret.KeyVersion)
	}

	// Re-run after "fixing" c2: successful records are not re-touched.
	fixed := base64.StdEncoding.EncodeToString([]byte(`{"token":"t1"}`))
	if _, err := store.sql.Exec(`UPDATE connection_secrets SET ciphertext = ? WHERE connection_id = 'c2'`, fixed); err != nil {
		t.Fatalf("fix row: %v", err)
	}
	retry, err := store.MigrateSecrets()
	if err != nil {
		t.Fatalf("retry migrate: %v", err)
	}
	if retry.ReEncrypted != 1 || len(retry.Failed) != 0 {
		t.Fatalf("retry report = %+v, want exactly c2 migrated", retry)
	}
	remaining, err := store.CountPlaintextSecretRecords()
	if err != nil || remaining != 0 {
		t.Fatalf("plaintext after retry = %d err=%v, want 0", remaining, err)
	}
}

// Background writes that landed BEFORE the migration (the realistic
// maintenance-window case: the service was writing until it was stopped).
// NOTE (P0.6-6, honest scoping): this is NOT a concurrent-migration test —
// the writer completes before MigrateSecrets starts. Migration is only
// sanctioned inside a maintenance window with the service quiesced; no
// online-concurrent-safety claim is made or tested here. The test pins the
// "migration catches up on everything written before it ran" invariant.
func TestMigrateSecretsCoversPriorBackgroundWrites(t *testing.T) {
	store, _ := migrateFixture(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			name := fmt.Sprintf("bg%d", i)
			_ = store.UpsertConnection(Connection{ID: name, WorkspaceID: "ws-m", Provider: "gitlab", ConnectionName: name})
			_ = store.UpsertConnectionSecretForID(name, ConnectionSecret{
				Ciphertext: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"token":"bg%d"}`, i))),
				KeyVersion: "plain-dev",
			})
		}
	}()
	wg.Wait()

	report, err := store.MigrateSecrets()
	if err != nil {
		t.Fatalf("migrate after background writes: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if report.ReEncrypted != 23 { // 3 fixtures + 20 background rows
		t.Fatalf("re-encrypted = %d, want 23", report.ReEncrypted)
	}
	remaining, err := store.CountPlaintextSecretRecords()
	if err != nil || remaining != 0 {
		t.Fatalf("plaintext remaining = %d err=%v, want 0", remaining, err)
	}
}

// Backup restore: a VACUUM INTO snapshot taken before migration must restore
// a readable pre-migration state (the rollback anchor).
func TestMigrateSecretsRestoreFromBackup(t *testing.T) {
	store, live := migrateFixture(t)

	backup := filepath.Join(t.TempDir(), "rollback.db")
	if err := store.BackupConsistent(backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := VerifyBackup(backup); err != nil {
		t.Fatalf("verify backup: %v", err)
	}

	if _, err := store.MigrateSecrets(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	remaining, err := store.CountPlaintextSecretRecords()
	if err != nil || remaining != 0 {
		t.Fatalf("post-migrate plaintext = %d err=%v, want 0", remaining, err)
	}

	// Simulate the rollback: replace the live DB file with the snapshot and
	// reopen. The plaintext rows must be readable again (old format, with the
	// key present or not — plaintext reads need no key).
	if err := store.Close(); err != nil {
		t.Fatalf("close live: %v", err)
	}
	if err := os.Remove(live); err != nil {
		t.Fatalf("remove live: %v", err)
	}
	restored, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if err := os.WriteFile(live, restored, 0o600); err != nil {
		t.Fatalf("restore backup over live: %v", err)
	}
	rollback, err := Open(live)
	if err != nil {
		t.Fatalf("reopen restored db: %v", err)
	}
	defer rollback.Close()

	secret, found, err := rollback.ConnectionSecret("c1")
	if err != nil || !found {
		t.Fatalf("read c1 after rollback: found=%v err=%v", found, err)
	}
	raw, err := base64.StdEncoding.DecodeString(secret.Ciphertext)
	if err != nil {
		t.Fatalf("decode plaintext after rollback: %v", err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse plaintext after rollback: %v", err)
	}
	if values["token"] != "t0" {
		t.Fatalf("token = %q, want t0", values["token"])
	}
	if secret.KeyVersion != "plain-dev" {
		t.Fatalf("key_version = %q, want plain-dev after rollback", secret.KeyVersion)
	}
}
