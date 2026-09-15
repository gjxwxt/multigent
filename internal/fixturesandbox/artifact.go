// Artifact lifecycle: freeze a seeded template database into a content-
// addressed artifact, then provision task-private copies from it. The
// artifact is immutable once frozen; a changed schema digest requires a new
// artifact (fail-closed on drift, plan §3.2 item 7).
package fixturesandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

const artifactTable = "fixture_sandbox_artifacts"

// Artifact describes one frozen template database.
type Artifact struct {
	Digest           string    `json:"digest"` // sha256 of the db file (content address)
	FixtureVersion   string    `json:"fixtureVersion"`
	Scenario         string    `json:"scenario"`
	SchemaDigest     string    `json:"schemaDigest"` // logical schema fingerprint of the source worktree
	LogicalDigest    string    `json:"logicalDigest"`// canonical export hash — the determinism identity
	SizeBytes        int64     `json:"sizeBytes"`
	Project          string    `json:"project"`
	CreatedAt        time.Time `json:"createdAt"`
}

// FreezeResult is the outcome of freezing a generator-produced database.
type FreezeResult struct {
	Artifact      Artifact
	ArtifactPath  string
}

// FreezeFile registers an already-produced database file (the generator ran
// in a disposable container; the caller passes the resulting db) as a frozen
// artifact: verify size, compute content digest + schema digest, copy into
// the artifact store, persist metadata. Idempotent: freezing identical
// content again returns the same artifact.
func (s *Store) FreezeFile(sourcePath string, project, fixtureVersion, scenario, schemaDigest string) (*FreezeResult, error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("stat generator output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("generator output is not a regular file")
	}
	if info.Size() > MaxArtifactBytes {
		return nil, fmt.Errorf("generator output %d bytes exceeds the %d byte artifact cap", info.Size(), MaxArtifactBytes)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("generator output is empty")
	}
	// A live WAL/SHM sibling means the generator left an un-checkpointed
	// database: freezing only the main file would silently lose the tail of
	// committed transactions living in the WAL. Fail closed — the generator
	// must checkpoint before its output is accepted.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(sourcePath + suffix); err == nil {
			return nil, fmt.Errorf("generator output has a live %s sibling — checkpoint the database before freezing", suffix)
		}
	}
	digest, err := fileSHA256(sourcePath)
	if err != nil {
		return nil, err
	}
	if existing, err := s.GetArtifact(digest); err == nil {
		path := s.artifactPath(digest)
		if _, err := os.Stat(path); err == nil {
			return &FreezeResult{Artifact: *existing, ArtifactPath: path}, nil
		}
		// metadata survived, file did not: fall through and re-materialize
	}
	artDir := s.ArtifactRoot()
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact dir: %w", err)
	}
	path := s.artifactPath(digest)
	if err := copyFileExclusive(sourcePath, path); err != nil {
		return nil, fmt.Errorf("materialize artifact: %w", err)
	}
	logical, err := logicalDigestOf(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("logical digest: %w", err)
	}
	artifact := Artifact{
		Digest:         digest,
		FixtureVersion: fixtureVersion,
		Scenario:       scenario,
		SchemaDigest:   schemaDigest,
		LogicalDigest:  logical,
		SizeBytes:      info.Size(),
		Project:        project,
		CreatedAt:      time.Now().UTC(),
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return nil, err
	}
	if err := s.db.UpsertRecord(artifactTable, s.workspace, []string{"artifact", digest, ""}, string(raw)); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &FreezeResult{Artifact: artifact, ArtifactPath: path}, nil
}

// GetArtifact loads artifact metadata by content digest.
func (s *Store) GetArtifact(digest string) (*Artifact, error) {
	raw, found, err := s.db.GetRecord(artifactTable, s.workspace, []string{"artifact", digest, ""})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("artifact %s not found", digest[:12])
	}
	var a Artifact
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("decode artifact: %w", err)
	}
	return &a, nil
}

func (s *Store) artifactPath(digest string) string {
	return filepath.Join(s.ArtifactRoot(), digest+".db")
}

// ProvisionResult describes a provisioned task-private database.
type ProvisionResult struct {
	Lease        *Lease
	DBPath       string // absolute path of the private db file
	EnvVar       string // env var name the preview container receives
	ArtifactDigest string
}

// ProvisionOptions configures one provision call.
type ProvisionOptions struct {
	Project        string
	TaskID         string
	WorktreeDir    string // current worktree, for the schema drift check
	Contract       *Contract
	Scenario       string // "" => contract default (baseline only)
	ArtifactPath   string // frozen template db
	Artifact       *Artifact
	Kind           string // KindPreview or KindExec
}

// Provision copies the frozen artifact into a task-private directory,
// verifies schema compatibility (fail-closed on drift), and records the lease.
// The copy is idempotent per task: an existing active lease for the same
// task+artifact is reused instead of duplicating data.
func (s *Store) Provision(opts ProvisionOptions) (*ProvisionResult, error) {
	if opts.Artifact == nil || opts.ArtifactPath == "" {
		return nil, fmt.Errorf("artifact and artifact path are required")
	}
	c := opts.Contract
	if c == nil {
		return nil, fmt.Errorf("contract is required")
	}
	scenario := opts.Scenario
	if scenario == "" {
		scenario = "default"
	}
	if _, ok := c.Scenario(scenario); scenario != "default" && !ok {
		return nil, fmt.Errorf("scenario %q is not declared in the contract", scenario)
	}

	// Schema drift gate (fail-closed): the artifact's schema digest must
	// match the current worktree's. A mismatch means migrations changed
	// without a new baseline — starting a preview against mismatched schema
	// would mislead everyone.
	currentDigest, err := SchemaFingerprintFor(opts.WorktreeDir, c.SchemaFingerprintPaths)
	if err != nil {
		return nil, fmt.Errorf("current schema fingerprint: %w", err)
	}
	if currentDigest != opts.Artifact.SchemaDigest {
		return nil, fmt.Errorf("schema drift: worktree fingerprint %s != artifact %s — publish a compatible fixture baseline (re-run the generator) before provisioning", currentDigest[:12], opts.Artifact.SchemaDigest[:12])
	}

	// Reuse: one active lease per (task, artifact).
	existing, err := s.activeLeaseForTask(opts.TaskID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.ArtifactDigest == opts.Artifact.Digest {
		dbPath := filepath.Join(existing.DataDir, filepath.FromSlash(existing.DBRelPath))
		if _, err := os.Stat(dbPath); err == nil {
			return &ProvisionResult{Lease: existing, DBPath: dbPath, EnvVar: existing.EnvVar, ArtifactDigest: existing.ArtifactDigest}, nil
		}
		// directory lost: fall through and rebuild under a fresh lease
	}

	// Layout: <dataRoot>/fixturesandbox/tasks/<leaseID>/<storage rel path>
	leaseDir := filepath.Join(s.DataRoot(), "lease-"+shortID())
	storageRel := filepath.Clean(strings.TrimSpace(c.Storage))
	dbPath := filepath.Join(leaseDir, storageRel)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}
	if err := copyFile(opts.ArtifactPath, dbPath); err != nil {
		_ = os.RemoveAll(leaseDir)
		return nil, fmt.Errorf("copy private db: %w", err)
	}
	// The frozen artifact never carries WAL/SHM siblings (FreezeFile refuses
	// them), so this copy starts from a clean checkpoint by construction.

	kind := opts.Kind
	if kind == "" {
		kind = KindPreview
	}
	envVar := "APP_DB_PATH"
	lease, err := s.CreateLease(kind, opts.Project, opts.TaskID, opts.Artifact.Digest, opts.Artifact.SchemaDigest, opts.Artifact.FixtureVersion, scenario, leaseDir, storageRel, envVar, DefaultLeaseTTL)
	if err != nil {
		_ = os.RemoveAll(leaseDir)
		return nil, fmt.Errorf("record lease: %w", err)
	}
	return &ProvisionResult{Lease: lease, DBPath: dbPath, EnvVar: envVar, ArtifactDigest: opts.Artifact.Digest}, nil
}

// activeLeaseForTask returns the active lease for a task, if any.
func (s *Store) activeLeaseForTask(taskID string) (*Lease, error) {
	all, err := s.ListLeases()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].TaskID == taskID && all[i].State == StateActive {
			return &all[i], nil
		}
	}
	return nil, nil
}

// Reset rebuilds a lease's private db from its frozen artifact:
// CAS active->resetting, wipe directory, re-copy from artifact, CAS back.
// The artifact is immutable, so reset cannot drift.
func (s *Store) Reset(leaseID string) (*ProvisionResult, error) {
	l, err := s.BeginReset(leaseID)
	if err != nil {
		return nil, err
	}
	rollback := func(failErr error) (*ProvisionResult, error) {
		_, _ = s.FailReset(leaseID)
		return nil, failErr
	}
	if _, err := s.GetArtifact(l.ArtifactDigest); err != nil {
		return rollback(fmt.Errorf("load artifact for reset: %w", err))
	}
	artifactPath := s.artifactPath(l.ArtifactDigest)
	if _, err := os.Stat(artifactPath); err != nil {
		return rollback(fmt.Errorf("artifact file missing: %w", err))
	}
	dbPath := filepath.Join(l.DataDir, filepath.FromSlash(l.DBRelPath))
	if err := os.RemoveAll(l.DataDir); err != nil {
		return rollback(fmt.Errorf("wipe sandbox dir: %w", err))
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return rollback(fmt.Errorf("recreate sandbox dir: %w", err))
	}
	if err := copyFile(artifactPath, dbPath); err != nil {
		return rollback(fmt.Errorf("re-copy private db: %w", err))
	}
	if _, err := s.CompleteReset(leaseID, l.ArtifactDigest, l.TemplateSchemaDigest); err != nil {
		return nil, fmt.Errorf("complete reset: %w (directory rebuilt; lease stuck in resetting)", err)
	}
	return &ProvisionResult{Lease: l, DBPath: dbPath, EnvVar: l.EnvVar, ArtifactDigest: l.ArtifactDigest}, nil
}

// Reclaim tears down one expired lease: CAS active->reclaiming, then remove
// the directory and mark removed. The caller is responsible for stopping any
// container that mounts the directory BEFORE calling this (plan §3.2 item 9).
func (s *Store) Reclaim(leaseID string) error {
	l, err := s.BeginReclaim(leaseID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(l.DataDir); err != nil {
		// Directory removal failure must not strand the lease in reclaiming
		// forever: return it to active so the next reaper pass retries.
		_, _ = s.transition(leaseID, StateReclaiming, StateActive, nil)
		return fmt.Errorf("remove sandbox dir: %w", err)
	}
	if _, err := s.CompleteReclaim(leaseID); err != nil {
		return fmt.Errorf("complete reclaim: %w", err)
	}
	return nil
}

// ReclaimOrphanDirs removes task data directories that no active/resetting
// lease references (restart recovery: leases are durable, dirs can outlive
// their rows or vice versa). Returns the removed directory names.
func (s *Store) ReclaimOrphanDirs() ([]string, error) {
	entries, err := os.ReadDir(s.DataRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	leases, err := s.ListLeases()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, l := range leases {
		if l.State == StateRemoved {
			continue
		}
		known[filepath.Base(l.DataDir)] = true
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.DataRoot(), e.Name())); err == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed, nil
}

// --- helpers ---

func shortID() string {
	var b [8]byte
	if _, err := cryptoRandRead(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// logicalDigestOf opens a copied database read-only and hashes its canonical
// export (see export.go — the normalized identity, not file bytes).
func logicalDigestOf(path string) (string, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	export, err := canonicalExport(db)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(export))
	return hex.EncodeToString(sum[:]), nil
}

// copyFile copies src to dst, creating dst's parent directory.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// copyFileExclusive fails when dst already exists (artifact materialization
// must never overwrite another writer's file — content addressing makes
// collision benign, but exclusivity surfaces bugs early).
func copyFileExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		// Same content by construction (content-addressed name); verify size
		// matches and treat as done.
		seen, statErr := os.Stat(dst)
		if statErr != nil {
			return statErr
		}
		src_info, statErr2 := os.Stat(src)
		if statErr2 != nil {
			return statErr2
		}
		if seen.Size() == src_info.Size() {
			return nil
		}
		return fmt.Errorf("artifact %s exists with mismatched size", filepath.Base(dst))
	}
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
