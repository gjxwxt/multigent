// Package kvfacade is the stage-0 KV storage facade prototype
// (license-remediation B3; docs/license-remediation-final-v3.md §4 and
// execution-plan §4-B3).
//
// Purpose: measure the REAL cost of the facade that the six quasi-clean
// packages (previewreceipt / fixturesandbox / changerun / brownfield /
// assets / secretbox) consume from internal/db, so the v3 §9 estimate
// (800–1,200 lines) is replaced by measured numbers. This is a PROTOTYPE:
// it lives in a new package (no production code imports it), reimplements
// the semantics from scratch on modernc.org/sqlite, and its acceptance is
// its own test suite plus the concurrency-boundary tests.
//
// Scope per v3 §4:
//   - KV record CRUD + revision guards: GetRecord / UpsertRecord /
//     DeleteRecord / ListRecords / ListRecordsWithRevision /
//     UpdateRecordIfRevision / UpdateRecordIfPayloadAndRevision /
//     InsertRecordIfAbsent / RecordRevision / KVWrite / KVTxReader
//   - claim semantics (NOT a revision CAS): CommitRecordWritesGuarded(+Tx)
//     and the transition-shaped CommitTransitionGuarded(+Tx) — claim-owner
//     verification inside BEGIN IMMEDIATE, exactly the semantics of
//     internal/db/workspaces.go:523/:590
//   - workspace: UpsertWorkspace / Workspace
//
// The kv_records table shape is kept compatible with the production schema
// (table_name / workspace_id / k1 / k2 / k3 / payload / updated_at /
// revision, PK on the five key columns, monotonic revision counter bumped
// on every write) so a schema diff against internal/db is mechanical.
//
// Out of scope (annotated, not implemented — v3 §4 two special surfaces):
//   - fixturesandbox's raw SQL (Exec/Query/QueryRow): the facade exposes
//     RawQuerier below as the controlled escape hatch; full schema-level
//     parity is stage-2 work.
//   - assets' typed helpers (AssetAttachment/AssetBlob/AssetFile/...): the
//     column inventory is documented in storage-prototype-numbers.md;
//     they re-point to raw records or get implemented in stage 2.
package kvfacade

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrClaimLost is returned when the stored claim marker no longer names the
// committing claimID — another transitioner stole the claim. NOTHING from
// the losing batch persists (the transaction rolls back whole).
var ErrClaimLost = errors.New("kvfacade: claim lost: the stored claim marker no longer names this claimID")

// ClaimMarkerPrefix mirrors the production transition-claim framing
// (internal/workflow/store.go transitionClaimPrefix / internal/db
// transitionClaimMarkerPrefix). A mismatch would fail every guarded commit
// closed, so the constant is documented as cross-package lockstep.
const ClaimMarkerPrefix = "transition-claim:"

// Facade is the stage-0 storage facade over one SQLite database.
type Facade struct {
	sql *sql.DB
	// clock returns UTC now as the updated_at string; overridable in tests.
	clock func() time.Time
}

// Open opens (creating if needed) the facade database at path and applies
// the schema.
func Open(path string) (*Facade, error) {
	// Per-connection pragmas ride the DSN (modernc.org/sqlite _pragma
	// option): busy_timeout serializes writers instead of failing them,
	// WAL lets readers proceed during guarded commits.
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	dsn := path + sep + "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single-writer discipline: SQLite serializes writers; a small pool with
	// busy_timeout keeps the concurrency tests honest without flaky
	// SQLITE_BUSY between independent connections.
	db.SetMaxOpenConns(4)
	f := &Facade{sql: db, clock: func() time.Time { return time.Now().UTC() }}
	if err := f.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return f, nil
}

// OpenInMemory opens a shared-cache in-memory facade (tests).
func OpenInMemory() (*Facade, error) {
	return Open("file:kvfacade_mem:?cache=shared")
}

// Close closes the database.
func (f *Facade) Close() error { return f.sql.Close() }

func (f *Facade) migrate() error {
	stmts := []string{
		// kv_records: same shape as internal/db/migrations.go (production).
		`CREATE TABLE IF NOT EXISTS kv_records (
	table_name TEXT NOT NULL,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	k1 TEXT NOT NULL DEFAULT '',
	k2 TEXT NOT NULL DEFAULT '',
	k3 TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	revision INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (table_name, workspace_id, k1, k2, k3)
)`,
		`CREATE INDEX IF NOT EXISTS idx_kv_records_lookup ON kv_records(table_name, workspace_id, k1, k2)`,
		`CREATE TABLE IF NOT EXISTS workspaces (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	slug TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	root TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT '',
	last_opened_at TEXT NOT NULL DEFAULT ''
)`,
		// RunRepository minimal-case tables (transition + membership +
		// worker slices). Column sets mirror the production entities the
		// workflow store actually touches (internal/db/db.go).
		`CREATE TABLE IF NOT EXISTS project_memberships (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL,
	project_id TEXT NOT NULL,
	member_type TEXT NOT NULL,
	member_id TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL DEFAULT '',
	permissions_json TEXT NOT NULL DEFAULT '[]',
	auto_pick_tasks INTEGER NOT NULL DEFAULT 0,
	attention_enabled INTEGER NOT NULL DEFAULT 0,
	priority_weight REAL NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
)`,
		`CREATE TABLE IF NOT EXISTS agent_workers (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL,
	name TEXT NOT NULL,
	display_name TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	profile_prompt TEXT NOT NULL DEFAULT '',
	avatar TEXT NOT NULL DEFAULT '',
	team TEXT NOT NULL DEFAULT '',
	role TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	primary_session_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
)`,
	}
	for _, s := range stmts {
		if _, err := f.sql.Exec(s); err != nil {
			return fmt.Errorf("kvfacade migrate: %w", err)
		}
	}
	return nil
}

// Workspace is the facade's workspace record (production Workspace shape,
// trimmed to the fields the four quasi-clean packages actually pass
// through: everything except audit-only metadata).
type Workspace struct {
	ID           string
	Name         string
	Slug         string
	Description  string
	Root         string
	CreatedBy    string
	CreatedAt    string
	UpdatedAt    string
	LastOpenedAt string
}

// UpsertWorkspace inserts or updates a workspace row.
func (f *Facade) UpsertWorkspace(w Workspace) error {
	now := f.clock().Format(time.RFC3339)
	if w.CreatedAt == "" {
		w.CreatedAt = now
	}
	if w.UpdatedAt == "" {
		w.UpdatedAt = now
	}
	_, err := f.sql.Exec(`INSERT INTO workspaces (
	id, name, slug, description, root, created_by, created_at, updated_at, last_opened_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name = excluded.name,
	slug = excluded.slug,
	description = excluded.description,
	root = excluded.root,
	created_by = CASE WHEN excluded.created_by != '' THEN excluded.created_by ELSE workspaces.created_by END,
	updated_at = excluded.updated_at,
	last_opened_at = CASE WHEN excluded.last_opened_at != '' THEN excluded.last_opened_at ELSE workspaces.last_opened_at END`,
		w.ID, w.Name, w.Slug, w.Description, w.Root, w.CreatedBy, w.CreatedAt, w.UpdatedAt, w.LastOpenedAt)
	return err
}

// Workspace fetches one workspace by id; found=false when absent.
func (f *Facade) Workspace(id string) (Workspace, bool, error) {
	var w Workspace
	err := f.sql.QueryRow(`SELECT id, name, slug, description, root, created_by, created_at, updated_at, last_opened_at
FROM workspaces WHERE id = ?`, id).
		Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Root, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.LastOpenedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, err
	}
	return w, true, nil
}

// normalizeKey pads/truncates a record key to the three-column kv_records
// key (k1/k2/k3), same discipline as internal/db normalizeKey.
func normalizeKey(key []string) (string, string, string) {
	var out [3]string
	for i := 0; i < len(key) && i < 3; i++ {
		out[i] = key[i]
	}
	return out[0], out[1], out[2]
}

// UpsertRecord writes one kv_records row, bumping the monotonic revision.
func (f *Facade) UpsertRecord(table, workspaceID string, key []string, payload string) error {
	k1, k2, k3 := normalizeKey(key)
	_, err := f.sql.Exec(`INSERT INTO kv_records (table_name, workspace_id, k1, k2, k3, payload, updated_at, revision)
VALUES (?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(table_name, workspace_id, k1, k2, k3) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at, revision = kv_records.revision + 1`,
		table, workspaceID, k1, k2, k3, payload, f.clock().Format(time.RFC3339))
	return err
}

// InsertRecordIfAbsent writes only when no row exists at the key; inserted
// reports whether THIS call claimed the absence.
func (f *Facade) InsertRecordIfAbsent(table, workspaceID string, key []string, payload string) (bool, error) {
	k1, k2, k3 := normalizeKey(key)
	res, err := f.sql.Exec(`INSERT OR IGNORE INTO kv_records (table_name, workspace_id, k1, k2, k3, payload, updated_at, revision)
VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
		table, workspaceID, k1, k2, k3, payload, f.clock().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetRecord reads one record's payload.
func (f *Facade) GetRecord(table, workspaceID string, key []string) (string, bool, error) {
	k1, k2, k3 := normalizeKey(key)
	var payload string
	err := f.sql.QueryRow(`SELECT payload FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`,
		table, workspaceID, k1, k2, k3).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return payload, true, nil
}

// RecordRevision returns the row's monotonic revision token.
func (f *Facade) RecordRevision(table, workspaceID string, key []string) (string, bool, error) {
	k1, k2, k3 := normalizeKey(key)
	var rev int64
	err := f.sql.QueryRow(`SELECT revision FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`,
		table, workspaceID, k1, k2, k3).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strconv.FormatInt(rev, 10), true, nil
}

// RecordWithRevision is one row plus its revision counter.
type RecordWithRevision struct {
	Key      []string
	Payload  string
	Revision string
}

// ListRecordsWithRevision lists rows under a key prefix (≤3 segments),
// payload and revision in one SELECT.
func (f *Facade) ListRecordsWithRevision(table, workspaceID string, keyPrefix []string) ([]RecordWithRevision, error) {
	if len(keyPrefix) > 3 {
		return nil, fmt.Errorf("record key prefix too long")
	}
	q := `SELECT k1, k2, k3, payload, revision FROM kv_records WHERE table_name = ? AND workspace_id = ?`
	args := []any{table, workspaceID}
	for i, seg := range keyPrefix {
		q += fmt.Sprintf(` AND k%d = ?`, i+1)
		args = append(args, seg)
	}
	q += ` ORDER BY k1 ASC, k2 ASC, k3 ASC`
	rows, err := f.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordWithRevision
	for rows.Next() {
		var k1, k2, k3, payload string
		var rev int64
		if err := rows.Scan(&k1, &k2, &k3, &payload, &rev); err != nil {
			return nil, err
		}
		out = append(out, RecordWithRevision{
			Key:      []string{k1, k2, k3},
			Payload:  payload,
			Revision: strconv.FormatInt(rev, 10),
		})
	}
	return out, rows.Err()
}

// Record is one kv_records row without revision.
type Record struct {
	Key     []string
	Payload string
}

// ListRecords lists rows under a key prefix.
func (f *Facade) ListRecords(table, workspaceID string, keyPrefix []string) ([]Record, error) {
	if len(keyPrefix) > 3 {
		return nil, fmt.Errorf("record key prefix too long")
	}
	q := `SELECT k1, k2, k3, payload FROM kv_records WHERE table_name = ? AND workspace_id = ?`
	args := []any{table, workspaceID}
	for i, seg := range keyPrefix {
		q += fmt.Sprintf(` AND k%d = ?`, i+1)
		args = append(args, seg)
	}
	q += ` ORDER BY k1 ASC, k2 ASC, k3 ASC`
	rows, err := f.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var k1, k2, k3, payload string
		if err := rows.Scan(&k1, &k2, &k3, &payload); err != nil {
			return nil, err
		}
		out = append(out, Record{Key: []string{k1, k2, k3}, Payload: payload})
	}
	return out, rows.Err()
}

// DeleteRecord removes one row.
func (f *Facade) DeleteRecord(table, workspaceID string, key []string) error {
	k1, k2, k3 := normalizeKey(key)
	_, err := f.sql.Exec(`DELETE FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`,
		table, workspaceID, k1, k2, k3)
	return err
}

// UpdateRecordIfRevision is the payload-blind CAS: writes newPayload only
// when the stored revision equals expectRevision (the monotonic counter,
// never the second-precision timestamp).
func (f *Facade) UpdateRecordIfRevision(table, workspaceID string, key []string, newPayload, expectRevision string) (bool, error) {
	k1, k2, k3 := normalizeKey(key)
	res, err := f.sql.Exec(`UPDATE kv_records SET payload = ?, updated_at = ?, revision = revision + 1
WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ? AND revision = ?`,
		newPayload, f.clock().Format(time.RFC3339), table, workspaceID, k1, k2, k3, expectRevision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateRecordIfPayloadAndRevision is the payload-witnessing CAS: writes
// only when BOTH the stored payload equals expectPayload AND the revision
// equals expectRevision. The workflow claim gate needs the payload witness —
// a revision-only CAS cannot distinguish "the row I observed" from "a claim
// marker someone else wrote" when two claimants observed the same revision.
func (f *Facade) UpdateRecordIfPayloadAndRevision(table, workspaceID string, key []string, newPayload, expectPayload, expectRevision string) (bool, error) {
	k1, k2, k3 := normalizeKey(key)
	res, err := f.sql.Exec(`UPDATE kv_records SET payload = ?, updated_at = ?, revision = revision + 1
WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ? AND payload = ? AND revision = ?`,
		newPayload, f.clock().Format(time.RFC3339), table, workspaceID, k1, k2, k3, expectPayload, expectRevision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// KVWrite is one record write inside a guarded batch.
type KVWrite struct {
	Table     string
	Workspace string
	Key       []string
	Payload   string
}

// KVTxReader is the deliberately read-only handle passed into guards and
// plan functions. Read-only by design (antigravity round-19 verdict #2):
// the guard runs on the connection holding the IMMEDIATE lock, so any pool
// borrow would wait on that very lock and deadlock. Borrowing a pool
// connection inside a guard is a compile error via this interface.
type KVTxReader interface {
	GetRecord(table, workspaceID string, key []string) (payload string, found bool, err error)
	GetRecordWithRevision(table, workspaceID string, key []string) (payload string, revision int64, found bool, err error)
}

// verifyClaimOwner reports whether payload is a claim marker whose embedded
// claimID equals expectClaimID. Framing mirrors production:
// "transition-claim:<len(claimID)>:<claimID><rest>".
func verifyClaimOwner(payload, expectClaimID string) bool {
	rest, ok := strings.CutPrefix(payload, ClaimMarkerPrefix)
	if !ok {
		return false
	}
	lenStr, restAfterLen, ok := strings.Cut(rest, ":")
	if !ok {
		return false
	}
	idLen, err := strconv.Atoi(lenStr)
	if err != nil || idLen <= 0 || idLen > 128 || len(restAfterLen) < idLen {
		return false
	}
	return restAfterLen[:idLen] == expectClaimID
}

// claimConn is a dedicated *sql.Conn running an explicit BEGIN IMMEDIATE
// transaction. The write lock is held from before the claim read through
// COMMIT — check-then-write batches are serialized against every other
// writer, including other processes on the same database file.
type claimConn struct {
	conn *sql.Conn
}

func (f *Facade) beginImmediate(ctx context.Context) (*claimConn, error) {
	conn, err := f.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, err
	}
	return &claimConn{conn: conn}, nil
}

func (c *claimConn) commit(ctx context.Context) error {
	_, err := c.conn.ExecContext(ctx, "COMMIT")
	c.conn.Close()
	return err
}

func (c *claimConn) rollback(ctx context.Context) {
	_, _ = c.conn.ExecContext(ctx, "ROLLBACK")
	c.conn.Close()
}

func (c *claimConn) writeBatch(ctx context.Context, workspaceID string, writes []KVWrite) error {
	if len(writes) == 0 {
		return fmt.Errorf("kvfacade: guarded commit requires at least one write")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, w := range writes {
		k1, k2, k3 := normalizeKey(w.Key)
		ws := w.Workspace
		if ws == "" {
			ws = workspaceID
		}
		if _, err := c.conn.ExecContext(ctx, `INSERT INTO kv_records (table_name, workspace_id, k1, k2, k3, payload, updated_at, revision)
VALUES (?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(table_name, workspace_id, k1, k2, k3) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at, revision = kv_records.revision + 1`,
			w.Table, ws, k1, k2, k3, w.Payload, now); err != nil {
			return err
		}
	}
	return nil
}

// txReader is the KVTxReader bound to the claim connection.
type txReader struct {
	conn *sql.Conn
}

func (t txReader) GetRecord(table, workspaceID string, key []string) (string, bool, error) {
	payload, _, found, err := t.GetRecordWithRevision(table, workspaceID, key)
	return payload, found, err
}

func (t txReader) GetRecordWithRevision(table, workspaceID string, key []string) (string, int64, bool, error) {
	k1, k2, k3 := normalizeKey(key)
	var payload string
	var rev int64
	err := t.conn.QueryRowContext(context.Background(),
		`SELECT payload, revision FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`,
		table, workspaceID, k1, k2, k3).Scan(&payload, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return payload, rev, true, nil
}

// CommitRecordWritesGuarded commits writes as one atomic batch with an
// optional guard: the guard runs INSIDE the BEGIN IMMEDIATE transaction
// before any write and may read through KVTxReader; a non-nil error aborts
// the whole batch. check-then-write batches are race-free because the
// IMMEDIATE lock is already held.
func (f *Facade) CommitRecordWritesGuarded(workspaceID string, guard func(tx KVTxReader) error, writes []KVWrite) error {
	return f.CommitRecordWritesGuardedTx(workspaceID, func(tx KVTxReader) ([]KVWrite, error) {
		if guard != nil {
			if err := guard(tx); err != nil {
				return nil, err
			}
		}
		return writes, nil
	})
}

// CommitRecordWritesGuardedTx computes the write batch INSIDE the
// transaction: plan(tx) reads through the claim connection and returns the
// writes that must land atomically. A non-nil error from plan aborts
// everything.
func (f *Facade) CommitRecordWritesGuardedTx(workspaceID string, plan func(tx KVTxReader) ([]KVWrite, error)) error {
	if plan == nil {
		return fmt.Errorf("kvfacade: record commit requires a non-nil plan")
	}
	ctx := context.Background()
	c, err := f.beginImmediate(ctx)
	if err != nil {
		return err
	}
	writes, err := plan(txReader{conn: c.conn})
	if err != nil {
		c.rollback(ctx)
		return err
	}
	if err := c.writeBatch(ctx, workspaceID, writes); err != nil {
		c.rollback(ctx)
		return err
	}
	return c.commit(ctx)
}

// CommitTransitionGuarded is the claim-owner-checked transition commit:
// the workflow_runs row's payload must still be THIS claimant's marker
// (claim prefix + embedded claimID == expectClaimID); anything else aborts
// with ErrClaimLost and nothing persists. On success the batch lands
// atomically — the final run write replaces the marker with the
// post-transition payload, which releases the claim.
func (f *Facade) CommitTransitionGuarded(workspaceID, runTable string, runKey []string, expectClaimID string, writes []KVWrite) error {
	return f.CommitTransitionGuardedTx(workspaceID, runTable, runKey, expectClaimID, func(tx KVTxReader) ([]KVWrite, error) {
		return writes, nil
	})
}

// CommitTransitionGuardedTx re-verifies the claim owner and computes the
// write batch inside the same IMMEDIATE transaction.
func (f *Facade) CommitTransitionGuardedTx(workspaceID, runTable string, runKey []string, expectClaimID string, plan func(tx KVTxReader) ([]KVWrite, error)) error {
	if plan == nil {
		return fmt.Errorf("kvfacade: transition commit requires a non-nil plan")
	}
	ctx := context.Background()
	c, err := f.beginImmediate(ctx)
	if err != nil {
		return err
	}
	k1, k2, k3 := normalizeKey(runKey)
	var payload string
	err = c.conn.QueryRowContext(ctx,
		`SELECT payload FROM kv_records WHERE table_name = ? AND workspace_id = ? AND k1 = ? AND k2 = ? AND k3 = ?`,
		runTable, workspaceID, k1, k2, k3).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		c.rollback(ctx)
		return fmt.Errorf("%w: run record missing", ErrClaimLost)
	}
	if err != nil {
		c.rollback(ctx)
		return err
	}
	if !verifyClaimOwner(payload, expectClaimID) {
		c.rollback(ctx)
		return ErrClaimLost
	}
	writes, err := plan(txReader{conn: c.conn})
	if err != nil {
		c.rollback(ctx)
		return err
	}
	if err := c.writeBatch(ctx, workspaceID, writes); err != nil {
		c.rollback(ctx)
		return err
	}
	return c.commit(ctx)
}

// ---- RunRepository minimal-case slices (transition + membership + worker) ----

// ProjectMembership is the membership row the workflow scheduler reads.
type ProjectMembership struct {
	ID               string
	WorkspaceID      string
	ProjectID        string
	MemberType       string
	MemberID         string
	Role             string
	Title            string
	Prompt           string
	PermissionsJSON  string
	AutoPickTasks    bool
	AttentionEnabled bool
	PriorityWeight   float64
	CreatedAt        string
	UpdatedAt        string
}

// ProjectMembershipFilter selects memberships.
type ProjectMembershipFilter struct {
	WorkspaceID string
	ProjectID   string
	MemberType  string
	MemberID    string
}

// UpsertProjectMembership inserts or updates one membership row.
func (f *Facade) UpsertProjectMembership(m ProjectMembership) error {
	now := f.clock().Format(time.RFC3339)
	if m.CreatedAt == "" {
		m.CreatedAt = now
	}
	if m.UpdatedAt == "" {
		m.UpdatedAt = now
	}
	auto, attn := 0, 0
	if m.AutoPickTasks {
		auto = 1
	}
	if m.AttentionEnabled {
		attn = 1
	}
	_, err := f.sql.Exec(`INSERT INTO project_memberships (
	id, workspace_id, project_id, member_type, member_id, role, title, prompt,
	permissions_json, auto_pick_tasks, attention_enabled, priority_weight, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	role = excluded.role, title = excluded.title, prompt = excluded.prompt,
	permissions_json = excluded.permissions_json, auto_pick_tasks = excluded.auto_pick_tasks,
	attention_enabled = excluded.attention_enabled, priority_weight = excluded.priority_weight,
	updated_at = excluded.updated_at`,
		m.ID, m.WorkspaceID, m.ProjectID, m.MemberType, m.MemberID, m.Role, m.Title, m.Prompt,
		m.PermissionsJSON, auto, attn, m.PriorityWeight, m.CreatedAt, m.UpdatedAt)
	return err
}

// ListProjectMemberships lists memberships matching the filter.
func (f *Facade) ListProjectMemberships(filter ProjectMembershipFilter) ([]ProjectMembership, error) {
	q := `SELECT id, workspace_id, project_id, member_type, member_id, role, title, prompt,
	permissions_json, auto_pick_tasks, attention_enabled, priority_weight, created_at, updated_at
FROM project_memberships WHERE 1=1`
	var args []any
	if filter.WorkspaceID != "" {
		q += ` AND workspace_id = ?`
		args = append(args, filter.WorkspaceID)
	}
	if filter.ProjectID != "" {
		q += ` AND project_id = ?`
		args = append(args, filter.ProjectID)
	}
	if filter.MemberType != "" {
		q += ` AND member_type = ?`
		args = append(args, filter.MemberType)
	}
	if filter.MemberID != "" {
		q += ` AND member_id = ?`
		args = append(args, filter.MemberID)
	}
	q += ` ORDER BY priority_weight DESC, created_at ASC`
	rows, err := f.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectMembership
	for rows.Next() {
		var m ProjectMembership
		var auto, attn int
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.ProjectID, &m.MemberType, &m.MemberID,
			&m.Role, &m.Title, &m.Prompt, &m.PermissionsJSON, &auto, &attn, &m.PriorityWeight,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.AutoPickTasks = auto != 0
		m.AttentionEnabled = attn != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// AgentWorker is the worker row the workflow scheduler reads.
type AgentWorker struct {
	ID              string
	WorkspaceID     string
	Name            string
	DisplayName     string
	Description     string
	ProfilePrompt   string
	Avatar          string
	Team            string
	Role            string
	Status          string
	Model           string
	PrimarySession  string
	CreatedAt       string
	UpdatedAt       string
}

// UpsertAgentWorker inserts or updates one worker row.
func (f *Facade) UpsertAgentWorker(w AgentWorker) error {
	now := f.clock().Format(time.RFC3339)
	if w.CreatedAt == "" {
		w.CreatedAt = now
	}
	if w.UpdatedAt == "" {
		w.UpdatedAt = now
	}
	_, err := f.sql.Exec(`INSERT INTO agent_workers (
	id, workspace_id, name, display_name, description, profile_prompt, avatar,
	team, role, status, model, primary_session_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	display_name = excluded.display_name, description = excluded.description,
	profile_prompt = excluded.profile_prompt, avatar = excluded.avatar,
	team = excluded.team, role = excluded.role, status = excluded.status,
	model = excluded.model, primary_session_id = excluded.primary_session_id,
	updated_at = excluded.updated_at`,
		w.ID, w.WorkspaceID, w.Name, w.DisplayName, w.Description, w.ProfilePrompt, w.Avatar,
		w.Team, w.Role, w.Status, w.Model, w.PrimarySession, w.CreatedAt, w.UpdatedAt)
	return err
}

// AgentWorkerByID fetches one worker; found=false when absent.
func (f *Facade) AgentWorkerByID(workspaceID, id string) (AgentWorker, bool, error) {
	var w AgentWorker
	err := f.sql.QueryRow(`SELECT id, workspace_id, name, display_name, description, profile_prompt, avatar,
	team, role, status, model, primary_session_id, created_at, updated_at
FROM agent_workers WHERE workspace_id = ? AND id = ?`, workspaceID, id).
		Scan(&w.ID, &w.WorkspaceID, &w.Name, &w.DisplayName, &w.Description, &w.ProfilePrompt, &w.Avatar,
			&w.Team, &w.Role, &w.Status, &w.Model, &w.PrimarySession, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentWorker{}, false, nil
	}
	if err != nil {
		return AgentWorker{}, false, err
	}
	return w, true, nil
}

// runRepository is the narrow-interface sketch for stage 2: the workflow
// engine's actual db surface (measured: ListRecords x7, UpsertRecord x6,
// ListRecordsWithRevision x4, GetRecord x4, UpdateRecordIfPayloadAndRevision
// x2, DeleteRecord x2, UpdateRecordIfRevision / RecordRevision /
// InsertRecordIfAbsent / ListProjectMemberships / CommitTransitionGuarded /
// CommitTransitionGuardedTx x1 each, plus AgentWorkerByID) collapses to
// this interface. It is deliberately NOT a copy of db.Store.
type RunRepository interface {
	// KV record surface (the facade itself implements this shape).
	UpsertRecord(table, workspaceID string, key []string, payload string) error
	InsertRecordIfAbsent(table, workspaceID string, key []string, payload string) (bool, error)
	GetRecord(table, workspaceID string, key []string) (string, bool, error)
	RecordRevision(table, workspaceID string, key []string) (string, bool, error)
	ListRecords(table, workspaceID string, keyPrefix []string) ([]Record, error)
	ListRecordsWithRevision(table, workspaceID string, keyPrefix []string) ([]RecordWithRevision, error)
	UpdateRecordIfRevision(table, workspaceID string, key []string, newPayload, expectRevision string) (bool, error)
	UpdateRecordIfPayloadAndRevision(table, workspaceID string, key []string, newPayload, expectPayload, expectRevision string) (bool, error)
	DeleteRecord(table, workspaceID string, key []string) error
	CommitTransitionGuarded(workspaceID, runTable string, runKey []string, expectClaimID string, writes []KVWrite) error
	CommitTransitionGuardedTx(workspaceID, runTable string, runKey []string, expectClaimID string, plan func(tx KVTxReader) ([]KVWrite, error)) error

	// Membership + worker slices.
	ListProjectMemberships(filter ProjectMembershipFilter) ([]ProjectMembership, error)
	UpsertProjectMembership(m ProjectMembership) error
	AgentWorkerByID(workspaceID, id string) (AgentWorker, bool, error)
	UpsertAgentWorker(w AgentWorker) error
}

// compile-time assertion: *Facade implements the RunRepository sketch.
var _ RunRepository = (*Facade)(nil)

// RawQuerier is the controlled escape hatch for fixturesandbox's raw SQL
// (v3 §4 special surface #1). Schema-level coupling stays explicit: any
// caller holding this is outside the facade's contract and must be
// inventoried in the migration ledger.
type RawQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Raw returns the escape hatch. Deliberately a method, not an exported
// field, so the audit can grep every escape-hatch call site.
func (f *Facade) Raw() RawQuerier { return f.sql }
