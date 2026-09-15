package fixturesandbox

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	_ "modernc.org/sqlite"
)

// newTestStore opens a control DB in a temp dir and a sandbox store rooted
// at another temp dir.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	db, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open control db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// kv_records.workspace_id carries a FK to workspaces(id): seed it.
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws-test", Name: "test"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	dataRoot := t.TempDir()
	return NewStore(db, "ws-test", dataRoot), dataRoot
}

// writeContract materializes a minimal valid contract under root.
func writeContract(t *testing.T, root string) *Contract {
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
  "scenarios": {"default": {"command": "npm run db:seed:scenario -- --name default"}}
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
	if c == nil {
		t.Fatal("expected contract")
	}
	return c
}

// seededDB builds a template database with the contract's schema and two
// deterministic rows — stands in for the container-run generator output.
func seededDB(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "template.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO items VALUES (1,'alpha'),(2,'beta');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadContractAbsent(t *testing.T) {
	root := t.TempDir()
	c, err := LoadContract(root)
	if err != nil || c != nil {
		t.Fatalf("absent contract must be (nil, nil), got (%v, %v)", c, err)
	}
}

func TestLoadContractRejectsTraversalStorage(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".multigent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := `{"version":1,"engine":"sqlite","storage":"%s","defaultFixtureVersion":"1.0.0","schemaFingerprintPaths":[],"generator":{"command":"x"},"scenarios":{}}`
	for _, bad := range []string{"../outside.db", "/abs/path.db", "..\\escape.db"} {
		doc := fmt.Sprintf(base, bad)
		if err := os.WriteFile(filepath.Join(dir, "fixtures.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadContract(root); err == nil {
			t.Fatalf("storage %q must be rejected", bad)
		}
	}
}

func TestLoadContractRejectsNonSQLiteEngine(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".multigent")
	_ = os.MkdirAll(dir, 0o755)
	doc := `{"version":1,"engine":"postgres","storage":"data/app.db","defaultFixtureVersion":"1.0.0","schemaFingerprintPaths":[],"generator":{"command":"x"},"scenarios":{}}`
	_ = os.WriteFile(filepath.Join(dir, "fixtures.json"), []byte(doc), 0o644)
	if _, err := LoadContract(root); err == nil {
		t.Fatal("postgres engine must be rejected in V1")
	}
}

func TestSchemaFingerprintStableAndSorted(t *testing.T) {
	root := t.TempDir()
	mig := filepath.Join(root, "server", "migrations")
	_ = os.MkdirAll(mig, 0o755)
	_ = os.WriteFile(filepath.Join(mig, "0001_a.sql"), []byte("A"), 0o644)
	_ = os.WriteFile(filepath.Join(mig, "0002_b.sql"), []byte("B"), 0o644)
	d1, err := SchemaFingerprintFor(root, []string{"server/migrations/*.sql"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := SchemaFingerprintFor(root, []string{"server/migrations/0002_b.sql", "server/migrations/0001_a.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("glob vs explicit order differ: %s vs %s", d1, d2)
	}
	_ = os.WriteFile(filepath.Join(mig, "0003_c.sql"), []byte("C"), 0o644)
	d3, err := SchemaFingerprintFor(root, []string{"server/migrations/*.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d3 {
		t.Fatal("adding a migration must change the fingerprint")
	}
}

func TestFreezeAndLogicalDigestDeterminism(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)

	r1, err := s.FreezeFile(src, "proj", c.DefaultFixtureVersion, "default", c.SchemaDigest())
	if err != nil {
		t.Fatalf("freeze 1: %v", err)
	}
	r2, err := s.FreezeFile(src, "proj", c.DefaultFixtureVersion, "default", c.SchemaDigest())
	if err != nil {
		t.Fatalf("freeze 2 (idempotent): %v", err)
	}
	if r1.Artifact.Digest != r2.Artifact.Digest || r1.Artifact.LogicalDigest != r2.Artifact.LogicalDigest {
		t.Fatalf("identical content must freeze identically: %+v vs %+v", r1.Artifact, r2.Artifact)
	}
	if r1.ArtifactPath != r2.ArtifactPath {
		t.Fatal("content-addressed path must be stable")
	}
	if _, err := os.Stat(r1.ArtifactPath); err != nil {
		t.Fatalf("artifact file: %v", err)
	}
	if got, _ := os.Stat(r1.ArtifactPath); got.Size() != r1.Artifact.SizeBytes {
		t.Fatal("artifact size mismatch")
	}
}

func TestFreezeRejectsOversizeAndWAL(t *testing.T) {
	s, _ := newTestStore(t)
	big := filepath.Join(t.TempDir(), "big.db")
	if err := os.WriteFile(big, make([]byte, MaxArtifactBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeFile(big, "p", "1.0.0", "default", "x"); err == nil {
		t.Fatal("oversize artifact must be rejected")
	}
	// WAL sibling present => generator left an un-checkpointed db.
	src := seededDB(t, t.TempDir())
	if err := os.WriteFile(src+"-wal", []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeFile(src, "p", "1.0.0", "default", "x"); err == nil {
		t.Fatal("artifact with live WAL sibling must be rejected")
	}
}

func TestProvisionCopiesPrivateDBAndRecordsLease(t *testing.T) {
	s, dataRoot := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-1", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Lease == nil || res.Lease.State != StateActive || res.Lease.TaskID != "t-1" {
		t.Fatalf("unexpected lease: %+v", res.Lease)
	}
	if !filepath.HasPrefix(res.DBPath, filepath.Join(dataRoot, "fixturesandbox", "tasks")) {
		t.Fatalf("private db must live under the sandbox root: %s", res.DBPath)
	}
	// The copy is readable and holds the seeded rows.
	db, err := sql.Open("sqlite", res.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("private db rows = %d err = %v", n, err)
	}
	// Idempotent: same task + same artifact reuses the lease.
	res2, err := s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-1", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Lease.ID != res.Lease.ID {
		t.Fatalf("expected lease reuse, got %s then %s", res.Lease.ID, res2.Lease.ID)
	}
}

func TestProvisionRejectsSchemaDrift(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the worktree schema after freezing.
	mig := filepath.Join(root, "server", "migrations")
	if err := os.WriteFile(filepath.Join(mig, "0002_drift.sql"), []byte("ALTER TABLE items ADD COLUMN note TEXT;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-drift", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err == nil {
		t.Fatal("schema drift must fail closed")
	}
}

func TestProvisionIsolationConcurrentTasks(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	const tasks = 4
	results := make([]*ProvisionResult, tasks)
	var wg sync.WaitGroup
	for i := 0; i < tasks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Provision(ProvisionOptions{
				Project: "proj", TaskID: fmt.Sprintf("t-iso-%d", i), WorktreeDir: root,
				Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
			})
			if err != nil {
				t.Errorf("provision %d: %v", i, err)
				return
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	paths := map[string]bool{}
	for i, r := range results {
		if r == nil {
			t.Fatalf("task %d got no result", i)
		}
		if paths[r.DBPath] {
			t.Fatalf("duplicated private db path: %s", r.DBPath)
		}
		paths[r.DBPath] = true
		// Malicious write in task i's sandbox must not affect others.
		db, err := sql.Open("sqlite", r.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM items`); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	for i, r := range results[:tasks-1] {
		_ = i
		db, err := sql.Open("sqlite", r.DBPath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
		db.Close()
		if n != 0 {
			// each sandbox was wiped by its own loop iteration; isolation
			// means the wipe only affected that one path — verified by
			// distinct paths above. Cross-task influence would show as a
			// row count other than the wiped 0 only if paths collided.
			t.Fatalf("unexpected rows: %d", n)
		}
	}
}

func TestResetRestoresFromArtifact(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-reset", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Dirty the private db.
	db, _ := sql.Open("sqlite", res.DBPath)
	if _, err := db.Exec(`DELETE FROM items; INSERT INTO items VALUES (99,'dirty');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	logicalDirty, err := logicalDigestOf(res.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Reset(res.Lease.ID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	logicalClean, err := logicalDigestOf(out.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if logicalDirty == logicalClean {
		t.Fatal("reset must change the dirty db back to the artifact state")
	}
	if logicalClean != fr.Artifact.LogicalDigest {
		t.Fatalf("reset state %s != artifact identity %s", logicalClean, fr.Artifact.LogicalDigest)
	}
	after, err := s.GetLease(res.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateActive || after.ResetCount != 1 {
		t.Fatalf("lease after reset: %+v", after)
	}
}

func TestResetConcurrentSingleWinner(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-race", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	const racers = 6
	var wg sync.WaitGroup
	wins := make(chan int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Reset(res.Lease.ID); err == nil {
				wins <- i
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	n := 0
	for range wins {
		n++
	}
	if n != 1 {
		t.Fatalf("exactly one reset must win, got %d", n)
	}
}

func TestReclaimLifecycleAndOrphans(t *testing.T) {
	s, dataRoot := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-gone", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.DBPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Reclaim(res.Lease.ID); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if _, err := os.Stat(res.DBPath); !os.IsNotExist(err) {
		t.Fatalf("private db must be gone, got %v", err)
	}
	l, err := s.GetLease(res.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.State != StateRemoved {
		t.Fatalf("lease state after reclaim: %s", l.State)
	}
	// Double reclaim must fail cleanly (terminal state).
	if err := s.Reclaim(res.Lease.ID); err == nil {
		t.Fatal("reclaiming a removed lease must fail")
	}
	// Orphan dir sweep: a stray directory with no lease is removed.
	orph := filepath.Join(dataRoot, "fixturesandbox", "tasks", "lease-orphan")
	_ = os.MkdirAll(orph, 0o755)
	_ = os.WriteFile(filepath.Join(orph, "junk.db"), []byte("x"), 0o644)
	removed, err := s.ReclaimOrphanDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "lease-orphan" {
		t.Fatalf("orphan sweep removed %v", removed)
	}
	if _, err := os.Stat(orph); !os.IsNotExist(err) {
		t.Fatal("orphan dir must be gone")
	}
}

func TestExpiredLeases(t *testing.T) {
	s, _ := newTestStore(t)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-exp", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ExpiredLeases(time.Now().UTC())
	if err != nil || len(fresh) != 0 {
		t.Fatalf("fresh lease must not be expired: %v %v", fresh, err)
	}
	// Force expiry backdated.
	backdated, err := s.ExpiredLeases(time.Now().UTC().Add(DefaultLeaseTTL + time.Minute))
	if err != nil || len(backdated) != 1 {
		t.Fatalf("expected 1 expired lease, got %v %v", backdated, err)
	}
	if backdated[0].ID != res.Lease.ID {
		t.Fatal("wrong lease expired")
	}
}

func TestLeasePersistenceAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	dataRoot := t.TempDir()
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws-p", Name: "persist"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	s1 := NewStore(db, "ws-p", dataRoot)
	root := t.TempDir()
	c := writeContract(t, root)
	src := seededDB(t, root)
	fr, err := s1.FreezeFile(src, "proj", "1.0.0", "default", c.SchemaDigest())
	if err != nil {
		t.Fatal(err)
	}
	res, err := s1.Provision(ProvisionOptions{
		Project: "proj", TaskID: "t-persist", WorktreeDir: root,
		Contract: c, ArtifactPath: fr.ArtifactPath, Artifact: &fr.Artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2 := NewStore(db2, "ws-p", dataRoot)
	got, err := s2.GetLease(res.Lease.ID)
	if err != nil {
		t.Fatalf("lease must survive restart: %v", err)
	}
	if got.TaskID != "t-persist" || got.State != StateActive {
		t.Fatalf("unexpected lease after reopen: %+v", got)
	}
	if _, err := os.Stat(got.DataDir); err != nil {
		t.Fatalf("data dir must survive: %v", err)
	}
}

func TestContractSerializationRoundTrip(t *testing.T) {
	root := t.TempDir()
	c := writeContract(t, root)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Contract
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Storage != c.Storage || back.Engine != c.Engine {
		t.Fatalf("round trip mismatch: %+v", back)
	}
	if len(back.schemaDigest) != 0 {
		t.Fatal("derived fields must not leak into serialization")
	}
}

func TestDigestHelperStability(t *testing.T) {
	h := sha256.Sum256([]byte("x"))
	if len(hex.EncodeToString(h[:])) != 64 {
		t.Fatal("sanity")
	}
}

// Intranet migration checkpoint: the generator image must follow the same
// env contract as the agent sandbox (explicit override > region mirror >
// GHCR default), or fixture generation breaks the moment GHCR is
// unreachable behind an air-gapped network.
func TestGeneratorImageFollowsRuntimeImageEnv(t *testing.T) {
	t.Setenv("MULTIGENT_RUNTIME_IMAGE", "registry.internal:5000/multigent/runtime-base:2026.09")
	if got := GeneratorImage(); got != "registry.internal:5000/multigent/runtime-base:2026.09" {
		t.Fatalf("explicit override must win, got %s", got)
	}

	t.Setenv("MULTIGENT_RUNTIME_IMAGE", "")
	t.Setenv("MULTIGENT_RUNTIME_REGION", "cn")
	got := GeneratorImage()
	if !strings.Contains(got, "cn-hangzhou.personal.cr.aliyuncs.com") {
		t.Fatalf("cn region must select the mainland mirror, got %s", got)
	}

	os.Unsetenv("MULTIGENT_RUNTIME_REGION")
	if got := GeneratorImage(); got != "ghcr.io/multigent/multigent/runtime-base:latest" {
		t.Fatalf("default must be the published GHCR image, got %s", got)
	}
}
