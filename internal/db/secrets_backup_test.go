package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BackupConsistent must produce a single-file snapshot that passes
// integrity_check, must refuse to overwrite an existing file (a previous
// snapshot, or anything else), and must never clobber the live DB path.
func TestBackupConsistentAndVerify(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "multigent.db")
	store, err := Open(live)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// Content worth snapshotting: an encrypted connection secret row.
	if err := os.Setenv(EnvConnectionEncryptionKey, "k"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() { os.Unsetenv(EnvConnectionEncryptionKey) })
	sealed, err := SealConnectionSecret(map[string]string{"token": "t"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	sealed.ConnectionID = "conn-bk"
	if err := store.UpsertWorkspace(Workspace{ID: "ws-1", Name: "ws-1"}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := store.UpsertConnection(Connection{ID: "conn-bk", WorkspaceID: "ws-1", Provider: "gitlab", ConnectionName: "conn-bk"}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if err := store.UpsertConnectionSecret(sealed); err != nil {
		t.Fatalf("store secret: %v", err)
	}

	dest := filepath.Join(dir, "snap.db")
	if err := store.BackupConsistent(dest); err != nil {
		t.Fatalf("BackupConsistent: %v", err)
	}
	if err := VerifyBackup(dest); err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("backup perms = %o, want 600", perm)
	}
	// The snapshot must be a real copy of the live content, loadable
	// independently of the WAL.
	backupStore, err := Open(dest)
	if err != nil {
		t.Fatalf("open backup as db: %v", err)
	}
	defer backupStore.Close()
	got, found, err := backupStore.ConnectionSecret("conn-bk")
	if err != nil || !found {
		t.Fatalf("read secret from backup: found=%v err=%v", found, err)
	}
	values, err := OpenConnectionSecret(got)
	if err != nil {
		t.Fatalf("decrypt from backup: %v", err)
	}
	if values["token"] != "t" {
		t.Fatalf("token = %q, want t", values["token"])
	}
}

func TestBackupConsistentRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "multigent.db")
	store, err := Open(live)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	dest := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(dest, []byte("previous snapshot"), 0o600); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	err = store.BackupConsistent(dest)
	if err == nil {
		t.Fatal("BackupConsistent over an existing file must fail")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("error = %v, want overwrite refusal", err)
	}
	// The pre-existing file must be untouched.
	kept, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(kept) != "previous snapshot" {
		t.Fatalf("dest contents changed: %q", kept)
	}
}

func TestBackupConsistentRejectsEmptyDest(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := store.BackupConsistent("  "); err == nil {
		t.Fatal("empty dest must be rejected")
	}
}

// VerifyBackup must reject a file that is not a SQLite database — a torn or
// foreign file must never pass as a rollback anchor.
func TestVerifyBackupRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not a database file"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if err := VerifyBackup(corrupt); err == nil {
		t.Fatal("VerifyBackup must reject a non-SQLite file")
	}
}
