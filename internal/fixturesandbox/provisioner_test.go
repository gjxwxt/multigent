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
}

