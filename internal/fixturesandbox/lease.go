// Lease persistence: every sandbox data directory belongs to a lease row in
// kv_records (table "fixture_sandbox_leases") carrying its state machine.
// State transitions are revision-CAS'd so concurrent console processes
// sharing the control DB cannot double-reset or double-reclaim.
package fixturesandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

// Lease states.
const (
	StateActive     = "active"     // provisioned and attached to a task/preview
	StateResetting  = "resetting"  // exclusive: a reset is rebuilding the directory
	StateReclaiming = "reclaiming" // exclusive: the reaper is tearing it down
	StateRemoved    = "removed"    // terminal: directory deleted, row kept for audit
)

// LeaseKind distinguishes preview-attached sandboxes from headless QA runs
// (plan §3.2 item 3: both are first-class leases with IDs and expiries).
const (
	KindPreview = "preview"
	KindExec    = "exec"
)

// DefaultLeaseTTL mirrors the preview engine's 30-minute lease; the reaper
// runs on the same cadence.
const DefaultLeaseTTL = 30 * time.Minute

// MaxArtifactBytes caps a frozen template artifact (plan §6 gate: <10MB).
const MaxArtifactBytes = 10 << 20

const leaseTable = "fixture_sandbox_leases"

// Lease is the persisted sandbox lease record.
type Lease struct {
	ID                       string    `json:"id"`
	Kind                     string    `json:"kind"`
	WorkspaceID              string    `json:"workspaceId"`
	Project                  string    `json:"project"`
	TaskID                   string    `json:"taskId"`
	State                    string    `json:"state"`
	ArtifactDigest           string    `json:"artifactDigest"`
	TemplateSchemaDigest     string    `json:"templateSchemaDigest"`
	FixtureVersion           string    `json:"fixtureVersion"`
	Scenario                 string    `json:"scenario"`
	DataDir                  string    `json:"dataDir"`
	DBRelPath                string    `json:"dbRelPath"`
	EnvVar                   string    `json:"envVar"`
	ExpiresAt                time.Time `json:"expiresAt"`
	CreatedAt                time.Time `json:"createdAt"`
	UpdatedAt                time.Time `json:"updatedAt"`
	ResetCount               int       `json:"resetCount"`
}

// Store wraps the control DB for lease + artifact persistence.
type Store struct {
	db         controldb.Store
	workspace  string
	dataRoot   string // MULTIGENT_DATA_DIR-style root for artifact + data dirs
}

// NewStore builds a sandbox store over the control DB for one workspace.
// dataRoot is the platform data directory; artifacts and private DBs live
// under <dataRoot>/fixturesandbox/.
func NewStore(db controldb.Store, workspaceID, dataRoot string) *Store {
	return &Store{db: db, workspace: workspaceID, dataRoot: dataRoot}
}

func newLeaseID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("lease-%d", time.Now().UnixNano())
	}
	return "fsb-" + hex.EncodeToString(b[:])[:20]
}

func leaseKey(leaseID string) []string { return []string{"lease", leaseID, ""} }

func (s *Store) putLease(l Lease) error {
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return s.db.UpsertRecord(leaseTable, s.workspace, leaseKey(l.ID), string(raw))
}

// CreateLease inserts a fresh active lease. dataDir must already be resolved
// by the caller (provisioning decides the layout).
func (s *Store) CreateLease(kind, project, taskID, artifactDigest, schemaDigest, fixtureVersion, scenario, dataDir, dbRelPath, envVar string, ttl time.Duration) (*Lease, error) {
	now := time.Now().UTC()
	l := &Lease{
		ID:                   newLeaseID(),
		Kind:                 kind,
		WorkspaceID:          s.workspace,
		Project:              project,
		TaskID:               taskID,
		State:                StateActive,
		ArtifactDigest:       artifactDigest,
		TemplateSchemaDigest: schemaDigest,
		FixtureVersion:       fixtureVersion,
		Scenario:             scenario,
		DataDir:              dataDir,
		DBRelPath:            dbRelPath,
		EnvVar:               envVar,
		ExpiresAt:            now.Add(ttl),
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := s.putLease(*l); err != nil {
		return nil, err
	}
	return l, nil
}

// GetLease loads one lease by ID.
func (s *Store) GetLease(leaseID string) (*Lease, error) {
	raw, found, err := s.db.GetRecord(leaseTable, s.workspace, leaseKey(leaseID))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("lease %s not found", leaseID)
	}
	var l Lease
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return nil, fmt.Errorf("decode lease %s: %w", leaseID, err)
	}
	return &l, nil
}

// ListLeases returns every lease in the workspace (all states).
func (s *Store) ListLeases() ([]Lease, error) {
	recs, err := s.db.ListRecords(leaseTable, s.workspace, []string{"lease"})
	if err != nil {
		return nil, err
	}
	out := make([]Lease, 0, len(recs))
	for _, rec := range recs {
		var l Lease
		if err := json.Unmarshal([]byte(rec.Payload), &l); err != nil {
			continue // corrupt rows are skipped for listing; CAS paths fail loudly
		}
		out = append(out, l)
	}
	return out, nil
}

// transition swaps the lease state via payload+revision CAS and returns the
// fresh lease row. expectState selects the transition; the payload witness is
// the full lease JSON the caller observed.
func (s *Store) transition(leaseID, expectState, newState string, mutate func(*Lease)) (*Lease, error) {
	recs, err := s.db.ListRecordsWithRevision(leaseTable, s.workspace, leaseKey(leaseID))
	if err != nil {
		return nil, err
	}
	if len(recs) != 1 {
		return nil, fmt.Errorf("lease %s: %d rows", leaseID, len(recs))
	}
	var current Lease
	if err := json.Unmarshal([]byte(recs[0].Payload), &current); err != nil {
		return nil, fmt.Errorf("decode lease %s: %w", leaseID, err)
	}
	if current.State != expectState {
		return nil, fmt.Errorf("lease %s in state %s, expected %s (lost race)", leaseID, current.State, expectState)
	}
	next := current
	next.State = newState
	next.UpdatedAt = time.Now().UTC()
	if mutate != nil {
		mutate(&next)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	swapped, err := s.db.UpdateRecordIfPayloadAndRevision(leaseTable, s.workspace, leaseKey(leaseID), string(raw), recs[0].Payload, recs[0].Revision)
	if err != nil {
		return nil, err
	}
	if !swapped {
		return nil, fmt.Errorf("lease %s: lost state transition race (%s -> %s)", leaseID, expectState, newState)
	}
	return &next, nil
}

// BeginReset claims the exclusive resetting state (CAS active -> resetting).
func (s *Store) BeginReset(leaseID string) (*Lease, error) {
	return s.transition(leaseID, StateActive, StateResetting, nil)
}

// CompleteReset publishes the rebuilt directory back to active.
func (s *Store) CompleteReset(leaseID string, artifactDigest, schemaDigest string) (*Lease, error) {
	return s.transition(leaseID, StateResetting, StateActive, func(l *Lease) {
		l.ArtifactDigest = artifactDigest
		l.TemplateSchemaDigest = schemaDigest
		l.ResetCount++
		l.ExpiresAt = time.Now().UTC().Add(DefaultLeaseTTL)
	})
}

// FailReset returns a stuck resetting lease to active without extending it
// (the directory may or may not have been rebuilt; provisioning re-verifies).
func (s *Store) FailReset(leaseID string) (*Lease, error) {
	return s.transition(leaseID, StateResetting, StateActive, nil)
}

// BeginReclaim claims the lease for teardown (CAS active -> reclaiming).
func (s *Store) BeginReclaim(leaseID string) (*Lease, error) {
	return s.transition(leaseID, StateActive, StateReclaiming, nil)
}

// CompleteReclaim marks the lease removed (terminal; row kept for audit).
func (s *Store) CompleteReclaim(leaseID string) (*Lease, error) {
	l, err := s.transition(leaseID, StateReclaiming, StateRemoved, nil)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// Renew extends an active lease's expiry (idempotent, keeps state).
func (s *Store) Renew(leaseID string, ttl time.Duration) (*Lease, error) {
	recs, err := s.db.ListRecordsWithRevision(leaseTable, s.workspace, leaseKey(leaseID))
	if err != nil {
		return nil, err
	}
	if len(recs) != 1 {
		return nil, fmt.Errorf("lease %s: %d rows", leaseID, len(recs))
	}
	var current Lease
	if err := json.Unmarshal([]byte(recs[0].Payload), &current); err != nil {
		return nil, err
	}
	if current.State != StateActive {
		return nil, fmt.Errorf("lease %s in state %s, cannot renew", leaseID, current.State)
	}
	current.ExpiresAt = time.Now().UTC().Add(ttl)
	current.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	swapped, err := s.db.UpdateRecordIfPayloadAndRevision(leaseTable, s.workspace, leaseKey(leaseID), string(raw), recs[0].Payload, recs[0].Revision)
	if err != nil {
		return nil, err
	}
	if !swapped {
		return nil, fmt.Errorf("lease %s: lost renewal race", leaseID)
	}
	return &current, nil
}

// ExpiredLeases lists active leases past their expiry at the given instant.
func (s *Store) ExpiredLeases(now time.Time) ([]Lease, error) {
	all, err := s.ListLeases()
	if err != nil {
		return nil, err
	}
	var out []Lease
	for _, l := range all {
		if l.State == StateActive && !l.ExpiresAt.IsZero() && !now.Before(l.ExpiresAt) {
			out = append(out, l)
		}
	}
	return out, nil
}

// ArtifactRoot is the controlled directory holding frozen template artifacts.
func (s *Store) ArtifactRoot() string {
	return strings.TrimRight(s.dataRoot, "/") + "/fixturesandbox/artifacts"
}

// DataRoot is the controlled directory holding task-private sandbox dirs.
func (s *Store) DataRoot() string {
	return strings.TrimRight(s.dataRoot, "/") + "/fixturesandbox/tasks"
}
