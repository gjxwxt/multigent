package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/secretbox"
	taskstore "github.com/multigent/multigent/internal/taskstore"
)

func newWakeupPreflightFixture(t *testing.T) (string, *controldb.SQLiteStore, taskstore.Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", "")
	t.Setenv("MULTIGENT_DATA_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".multigent", "agency.yaml"), []byte("name: Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := controldb.Open(filepath.Join(root, ".multigent", "multigent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Format(time.RFC3339)
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws", Name: "Test", Slug: "test", Root: root, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return root, db, taskstore.NewDB(root, db)
}

// Regression (systemd deployments): a manual wakeup from a shell without
// MULTIGENT_CONNECTION_ENCRYPTION_KEY used to start the cycle, fail at
// "materialize provider credentials" inside the runner, and mark the running
// task done_failed. The preflight must refuse before any task state changes.
func TestWakeupCredentialPreflightRefusesSealedProviderWithoutKey(t *testing.T) {
	root, db, ts := newWakeupPreflightFixture(t)
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "")

	// Seal WITH the key present (env-v1 envelope, as production writes them),
	// then simulate the key-less shell for the preflight itself.
	t.Setenv(secretbox.EnvKey, "test-encryption-key")
	sealed, err := secretbox.SealString("sk-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretbox.EnvKey, "")
	if err := db.UpsertModelProvider("ws", controldb.ModelProvider{
		ID:          "prov-1",
		WorkspaceID: "ws",
		Name:        "main",
		Type:        "openai",
		APIKey:      sealed,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	pending := &entity.Task{
		ID:        entity.NewTaskID(),
		Title:     "doomed task",
		Status:    entity.TaskStatusPending,
		Prompt:    "would burn",
		CreatedBy: "test",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := ts.AddTask("sample", "dev", pending); err != nil {
		t.Fatal(err)
	}

	err = wakeupCredentialPreflightError(root, db)
	if err == nil {
		t.Fatal("preflight must refuse a wakeup when sealed credentials exist but the key is absent")
	}
	if !strings.Contains(err.Error(), secretbox.EnvKey) {
		t.Fatalf("error must name the missing env var: %v", err)
	}

	// The refusal must not touch any task state.
	fresh, getErr := ts.GetTask("sample", "dev", pending.ID)
	if getErr != nil {
		t.Fatalf("task must remain readable: %v", getErr)
	}
	if fresh.Status != entity.TaskStatusPending || fresh.ArchivedAt != nil {
		t.Fatalf("task state must be untouched by the refused wakeup: status=%s archivedAt=%v", fresh.Status, fresh.ArchivedAt)
	}
	active, err := ts.ListTasks("sample", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("pending task must stay visible after refused wakeup, got %+v", active)
	}
}

func TestWakeupCredentialPreflightRefusesSealedConnectionSecretWithoutKey(t *testing.T) {
	root, db, _ := newWakeupPreflightFixture(t)
	t.Setenv(secretbox.EnvKey, "test-encryption-key")
	values, err := controldb.SealConnectionSecret(map[string]string{"token": "glpat-x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretbox.EnvKey, "")
	if err := db.UpsertConnection(controldb.Connection{
		ID:             "conn-1",
		WorkspaceID:    "ws",
		Provider:       "gitlab",
		ConnectionName: "gitlab-main",
		AuthType:       "token",
		Status:         "active",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertConnectionSecret(controldb.ConnectionSecret{
		ConnectionID: "conn-1",
		Ciphertext:   values.Ciphertext,
		Nonce:        values.Nonce,
		KeyVersion:   values.KeyVersion,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	if err := wakeupCredentialPreflightError(root, db); err == nil {
		t.Fatal("preflight must refuse a wakeup when an env-v1 connection secret exists but the key is absent")
	}
}

func TestWakeupCredentialPreflightPassesWithoutSealedCredentials(t *testing.T) {
	root, db, _ := newWakeupPreflightFixture(t)
	t.Setenv(secretbox.EnvKey, "")

	// plain-dev credentials and no connections: no decryption will be needed.
	if err := db.UpsertModelProvider("ws", controldb.ModelProvider{
		ID:          "prov-plain",
		WorkspaceID: "ws",
		Name:        "plain",
		Type:        "openai",
		APIKey:      "raw-key-in-dev",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := wakeupCredentialPreflightError(root, db); err != nil {
		t.Fatalf("preflight must not refuse an environment without sealed credentials: %v", err)
	}
}

func TestWakeupCredentialPreflightNoopWhenKeyPresent(t *testing.T) {
	root, db, _ := newWakeupPreflightFixture(t)
	t.Setenv(secretbox.EnvKey, "test-encryption-key")
	sealed, err := secretbox.SealString("sk-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModelProvider("ws", controldb.ModelProvider{
		ID:          "prov-2",
		WorkspaceID: "ws",
		Name:        "main",
		Type:        "openai",
		APIKey:      sealed,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretbox.EnvKey, "some-encryption-key")
	if err := wakeupCredentialPreflightError(root, db); err != nil {
		t.Fatalf("preflight must pass when the encryption key is set: %v", err)
	}
}

func TestSecretboxEnvelopeNeedsKeyVersions(t *testing.T) {
	cases := map[string]bool{
		"raw-key":                                         false,
		"sealed:bm90LWpzb24=":                             false, // undecodable envelope → unknown → not env-v1
		`sealed:eyJrZXlWZXJzaW9uIjoiIn0=`:                 false, // empty keyVersion = plain-dev
		`sealed:eyJrZXlWZXJzaW9uIjoiZW52LXYxIn0=`:         true,
		`sealed:eyJrZXlWZXJzaW9uIjoiZW52LXYxLXN0cmljdCJ9`: true,
	}
	for value, want := range cases {
		if got := secretboxEnvelopeNeedsKey(value); got != want {
			t.Fatalf("secretboxEnvelopeNeedsKey(%q) = %v, want %v", value, got, want)
		}
	}
}
