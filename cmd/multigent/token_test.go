package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GPT review Q5: a warning is not a rejection. When the resolved control DB
// does not exist, admin-token must FAIL WITHOUT creating the file (or its
// parent directories) — OpenDefault would happily mint an empty store with a
// fresh jwt_secret whose tokens the running server always rejects.
func TestAdminTokenMissingControlDBFailsWithoutCreating(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("MULTIGENT_DATA_DIR", dataDir)
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", "")

	cmd := newAdminTokenCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(nil)
	cmd.SetErr(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("admin-token against a missing control DB must fail")
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "never creates") {
		t.Fatalf("error must name the missing DB and the no-create contract, got: %v", err)
	}
	dbPath := filepath.Join(dataDir, ".multigent", "multigent.db")
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("control DB must NOT be created by admin-token, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, ".multigent")); !os.IsNotExist(statErr) {
		t.Fatalf("parent directory must NOT be created either, stat err=%v", statErr)
	}
}

// With the env pointing at a directory that exists but holds no DB, the error
// must still be the no-create hard fail (not a silent fresh-DB mint).
func TestAdminTokenEmptyDataDirAlsoFails(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("MULTIGENT_DATA_DIR", dataDir)

	cmd := newAdminTokenCmd()
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("admin-token against an empty data dir must fail")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error must name the missing DB, got: %v", err)
	}
}
