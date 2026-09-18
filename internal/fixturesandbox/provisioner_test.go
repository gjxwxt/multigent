package fixturesandbox

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGenerator "runs in a container" by writing the seeded db directly —
// stands in for the disposable-container execution in unit tests.
func fakeGenerator(dbContent string) ContainerGenerator {
	return func(ctx context.Context, worktreeDir, argv string, timeout time.Duration) (string, error) {
		if argv == "" {
			return "", context.DeadlineExceeded
		}
		out := filepath.Join(worktreeDir, "server", "data", "app.db")
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return "", err
		}
		db, err := sql.Open("sqlite", out)
		if err != nil {
			return "", err
		}
		defer db.Close()
		if _, err := db.Exec(dbContent); err != nil {
			return "", err
		}
		return out, nil
	}
}

// newProvisionerFixture: contract + frozen artifact + provisioner.
func newProvisionerFixture(t *testing.T, gen ContainerGenerator) (*Provisioner, *Store, string, *Contract) {
	t.Helper()
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	return NewProvisioner(s, gen), s, root, c
}

func TestProvisionForPreviewGeneratesAndFreezesOnce(t *testing.T) {
	calls := 0
	p, _, root, _ := newProvisionerFixture(t, func(ctx context.Context, worktreeDir, argv string, timeout time.Duration) (string, error) {
		calls++
		return fakeGenerator(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO items VALUES (1,'alpha'),(2,'beta');`)(ctx, worktreeDir, argv, timeout)
	})
	env, err := p.ProvisionForPreview(context.Background(), "t-gen-1", "proj", root)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], "APP_DB_PATH=") {
		t.Fatalf("unexpected env: %v", env)
	}
	dbPath := strings.TrimPrefix(env[0], "APP_DB_PATH=")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("private db: %v", err)
	}
	db, err := OpenPrivateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil || n != 2 {
		db.Close()
		t.Fatalf("rows=%d err=%v", n, err)
	}
	db.Close()
	if calls != 1 {
		t.Fatalf("generator must run exactly once, ran %d", calls)
	}
	// Second task, same schema: artifact reused, no new generator run.
	env2, err := p.ProvisionForPreview(context.Background(), "t-gen-2", "proj", root)
	if err != nil {
		t.Fatalf("provision 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("generator must not rerun for a second task, ran %d", calls)
	}
	if env2[0] == env[0] {
		t.Fatal("distinct tasks must get distinct private db paths")
	}
}

func TestProvisionFailsClosedOnGeneratorFailure(t *testing.T) {
	p, _, root, _ := newProvisionerFixture(t, func(ctx context.Context, worktreeDir, argv string, timeout time.Duration) (string, error) {
		return "", context.DeadlineExceeded
	})
	_, err := p.ProvisionForPreview(context.Background(), "t-fail", "proj", root)
	if err == nil {
		t.Fatal("generator failure must fail the preview provision")
	}
	// And no lease may be left behind.
	leases, _ := p.store.ListLeases()
	for _, l := range leases {
		if l.TaskID == "t-fail" && l.State == StateActive {
			t.Fatal("no active lease may survive a failed provision")
		}
	}
}

func TestContractCacheInvalidation(t *testing.T) {
	p, _, root, _ := newProvisionerFixture(t, fakeGenerator(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL);`))
	if _, err := p.loadContract(root); err != nil {
		t.Fatal(err)
	}
	p.InvalidateContractCache(root)
	if _, cached := p.contracts[root]; cached {
		t.Fatal("cache must be dropped")
	}
}

func TestReapExpiredReclaimsHeadlessLeases(t *testing.T) {
	p, s, root, _ := newProvisionerFixture(t, fakeGenerator(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO items VALUES (1,'x');`))
	// Simulate a headless exec lease by provisioning then backdating expiry.
	res, err := p.provision(context.Background(), "proj", "t-headless", root, KindExec, "default")
	if err != nil {
		t.Fatal(err)
	}
	// Backdate directly through the store.
	l, err := s.GetLease(res.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	l.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := s.putLease(*l); err != nil {
		t.Fatal(err)
	}
	if err := p.ReapExpired(); err != nil {
		t.Fatalf("reap: %v", err)
	}
	after, err := s.GetLease(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateRemoved {
		t.Fatalf("expired exec lease must be reclaimed, state=%s", after.State)
	}
	if _, err := os.Stat(res.DBPath); !os.IsNotExist(err) {
		t.Fatal("expired lease data dir must be gone")
	}
}

func TestProvisionRejectsUndeclaredScenario(t *testing.T) {
	p, _, root, _ := newProvisionerFixture(t, fakeGenerator(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL);`))
	_, err := p.provision(context.Background(), "proj", "t-scen", root, KindPreview, "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared scenario must be rejected, got %v", err)
	}
}

func TestProvisionForPreviewContractLessWorktree(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir() // empty directory, no .multigent/fixtures.json
	p := NewProvisioner(s, nil)
	env, err := p.ProvisionForPreview(context.Background(), "t-empty", "proj", root)
	if err != nil {
		t.Fatalf("contract-less project must return nil error, got %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("contract-less project must return empty env, got %v", env)
	}

	st, err := p.TaskStatus(context.Background(), "t-empty", "proj", root)
	if err != nil {
		t.Fatalf("TaskStatus error: %v", err)
	}
	if st.HasContract {
		t.Fatalf("expected HasContract=false for empty worktree")
	}
}

func writeContractWithScenarios(t *testing.T, root string) *Contract {
	t.Helper()
	dir := filepath.Join(root, ".multigent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{
  "version": 1,
  "engine": "sqlite",
  "storage": "server/data/app.db",
  "defaultFixtureVersion": "1.0.0",
  "schemaFingerprintPaths": ["server/migrations/*.sql"],
  "generator": {"command": "npm run db:seed:baseline", "timeoutSeconds": 60},
  "scenarios": {
    "default": {"description": "Standard baseline data", "command": "npm run db:seed:scenario -- --name default"},
    "edge_cases": {"description": "Edge case records", "command": "npm run db:seed:scenario -- --name edge_cases"}
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "fixtures.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	mig := filepath.Join(root, "server", "migrations")
	if err := os.MkdirAll(mig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mig, "0001_baseline.sql"), []byte("CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadContract(root)
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return c
}

func TestProvisionerTaskStatusAndReset(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	writeContractWithScenarios(t, root)
	gen := fakeGenerator(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO items (id, label) VALUES (1, 'initial');`)
	p := NewProvisioner(s, gen)

	// Before provisioning: HasContract=true, activeScenario=default, no lease
	status, err := p.TaskStatus(context.Background(), "t-task-1", "proj", root)
	if err != nil {
		t.Fatalf("TaskStatus: %v", err)
	}
	if !status.HasContract {
		t.Fatalf("expected HasContract=true")
	}
	if status.ActiveScenario != "default" || status.LeaseID != "" {
		t.Fatalf("unexpected unprovisioned status: %+v", status)
	}
	if len(status.AvailableScenarios) < 2 {
		t.Fatalf("expected at least 2 scenarios, got %d", len(status.AvailableScenarios))
	}

	// Provision
	env, err := p.ProvisionForPreview(context.Background(), "t-task-1", "proj", root)
	if err != nil {
		t.Fatalf("ProvisionForPreview: %v", err)
	}
	if len(env) != 1 {
		t.Fatalf("expected 1 env var, got %v", env)
	}

	// Status after provisioning: lease present
	status, err = p.TaskStatus(context.Background(), "t-task-1", "proj", root)
	if err != nil {
		t.Fatalf("TaskStatus after provision: %v", err)
	}
	if status.LeaseID == "" || status.State != StateActive || status.ResetCount != 0 {
		t.Fatalf("unexpected status after provision: %+v", status)
	}

	// Mutate the private DB
	dbPath := strings.TrimPrefix(env[0], "APP_DB_PATH=")
	db, err := OpenPrivateDB(dbPath)
	if err != nil {
		t.Fatalf("open private db: %v", err)
	}
	db.Close()
	// Overwrite with empty
	if err := os.WriteFile(dbPath, []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("corrupt private db: %v", err)
	}

	// ResetTask
	res, err := p.ResetTask(context.Background(), "t-task-1", "proj", root)
	if err != nil {
		t.Fatalf("ResetTask: %v", err)
	}
	if res.Lease.ResetCount != 1 {
		t.Fatalf("expected ResetCount=1, got %d", res.Lease.ResetCount)
	}

	// Verify database was restored
	dbRestored, err := OpenPrivateDB(res.DBPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer dbRestored.Close()
	var label string
	if err := dbRestored.QueryRow("SELECT label FROM items WHERE id = 1").Scan(&label); err != nil {
		t.Fatalf("query restored item: %v", err)
	}
	if label != "initial" {
		t.Fatalf("expected item label 'initial', got %q", label)
	}

	// Verify status reports resetCount=1
	status, err = p.TaskStatus(context.Background(), "t-task-1", "proj", root)
	if err != nil {
		t.Fatalf("TaskStatus after reset: %v", err)
	}
	if status.ResetCount != 1 {
		t.Fatalf("expected status.ResetCount=1, got %d", status.ResetCount)
	}
}

func TestProvisionerSwitchScenario(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	writeContractWithScenarios(t, root)
	gen := func(ctx context.Context, worktreeDir, argv string, timeout time.Duration) (string, error) {
		out := filepath.Join(worktreeDir, "server", "data", "app.db")
		_ = os.Remove(out)
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		db, err := sql.Open("sqlite", out)
		if err != nil {
			return "", err
		}
		defer db.Close()
		sqlStmt := `CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO items (id, label) VALUES (1, 'default-label');`
		if strings.Contains(argv, "edge_cases") {
			sqlStmt = `CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO items (id, label) VALUES (1, 'edge-label');`
		}
		if _, err := db.Exec(sqlStmt); err != nil {
			return "", err
		}
		return out, nil
	}
	p := NewProvisioner(s, gen)

	// Provision default scenario
	res1, err := p.ProvisionForPreview(context.Background(), "t-task-sc", "proj", root)
	if err != nil {
		t.Fatalf("provision default: %v", err)
	}
	if len(res1) != 1 {
		t.Fatalf("expected 1 env var")
	}

	// Switch to edge_cases
	res2, err := p.SwitchScenario(context.Background(), "t-task-sc", "proj", root, "edge_cases")
	if err != nil {
		t.Fatalf("switch to edge_cases: %v", err)
	}
	if res2.Lease.Scenario != "edge_cases" {
		t.Fatalf("expected scenario edge_cases, got %s", res2.Lease.Scenario)
	}

	// Verify edge_cases data
	db, err := OpenPrivateDB(res2.DBPath)
	if err != nil {
		t.Fatalf("open edge_cases db: %v", err)
	}
	defer db.Close()
	var label string
	if err := db.QueryRow("SELECT label FROM items WHERE id = 1").Scan(&label); err != nil {
		t.Fatalf("query edge item: %v", err)
	}
	if label != "edge-label" {
		t.Fatalf("expected 'edge-label', got %q", label)
	}

	// Reject undeclared scenario
	if _, err := p.SwitchScenario(context.Background(), "t-task-sc", "proj", root, "nonexistent"); err == nil {
		t.Fatalf("expected error switching to nonexistent scenario")
	}
}



