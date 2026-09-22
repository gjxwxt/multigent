package workflow

// Control-plane persistence for QA baselines (fix round S2-2, reviewer P0):
// the baseline document captured at worktree materialization is stored in
// kv_records (table "qa_baselines", key: project/taskID) — the control
// plane's own storage, which sandboxed agents have NO write path to. The
// copy inside the worktree is a recovery/tamper-canary aid only; the gate
// trusts exclusively what this store returns.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/multigent/multigent/internal/gitworktree"
)

// qaBaselineTable is the kv_records namespace for baseline documents. The
// kv layer is schema-agnostic (table name + composite key + payload), so no
// migration is needed; the table name keeps the records isolated from
// workflow state.
const qaBaselineTable = "qa_baselines"

// CaptureQABaselineRecord fingerprints the freshly materialized worktree
// and persists the document to the control plane. It must run at the
// protected moment (right after `git worktree add`, before the agent runs)
// with project/taskID identifying the OWNING task.
//
// Ordering guarantee: the worktree-side recovery copy and the capture
// manifest are written ONLY after the control-plane upload has succeeded,
// so a failed upload can never leave a "baseline existed here" canary
// behind a missing baseline (that combination is exactly the tamper case
// the gate fails closed on). On upload failure the worktree copy is
// removed again and an error is returned; the gate then stays on the
// legacy absolute surface, strictly fail-closed as before S2-1.
func (s *Store) CaptureQABaselineRecord(project, taskID, worktreeDir string) error {
	if s == nil || s.db == nil {
		// A store without a control plane cannot capture at all; make sure a
		// stale worktree-side copy (e.g. from a previous attempt) never
		// survives without its authoritative record.
		if s != nil {
			_ = s.DeleteQABaselineRecordFiles(worktreeDir)
		}
		return fmt.Errorf("qa baseline capture requires a control-plane store")
	}
	if strings.TrimSpace(project) == "" || strings.TrimSpace(taskID) == "" || strings.TrimSpace(worktreeDir) == "" {
		return fmt.Errorf("qa baseline capture requires project, taskID and worktreeDir")
	}
	baseline, err := gitworktree.CaptureQABaseline(worktreeDir)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	if err := s.db.UpsertRecord(qaBaselineTable, s.workspaceID, []string{project, taskID}, string(payload)); err != nil {
		// Roll the recovery copy back out so no half-trusted state remains.
		_ = s.DeleteQABaselineRecordFiles(worktreeDir)
		return fmt.Errorf("persist qa baseline: %w", err)
	}
	if err := gitworktree.WriteQABaselineManifest(worktreeDir); err != nil {
		// The control plane already has the authoritative document; the
		// manifest only weakens tamper DETECTION for this worktree. Do not
		// undo the upload, but surface the anomaly.
		return fmt.Errorf("qa baseline persisted, but manifest write failed: %w", err)
	}
	return nil
}

// LoadQABaselinePayload implements the gitworktree.QABaselineLookup
// contract: fetch the authoritative baseline document for a task from the
// control plane. found=false is a CLEAN miss (legacy worktree); a stored
// payload that fails to decode as a string is an error.
func (s *Store) LoadQABaselinePayload(project, taskID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("qa baseline lookup requires a control-plane store")
	}
	payload, ok, err := s.db.GetRecord(qaBaselineTable, s.workspaceID, []string{project, taskID})
	if err != nil {
		return "", false, err
	}
	return payload, ok, nil
}

// QABaselineLookupAdapter exposes the store's lookup as the plain function
// the gitworktree gate helper expects (avoids exporting the *Store type
// into gitworktree).
func (s *Store) QABaselineLookupAdapter() gitworktree.QABaselineLookup {
	return s.LoadQABaselinePayload
}

// DeleteQABaselineRecord removes the control-plane baseline document (task
// worktree cleanup path). The worktree-side copy dies with the worktree.
func (s *Store) DeleteQABaselineRecord(project, taskID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("qa baseline delete requires a control-plane store")
	}
	return s.db.DeleteRecord(qaBaselineTable, s.workspaceID, []string{project, taskID})
}

// DeleteQABaselineRecordFiles removes the worktree-side recovery copy and
// manifest (used on capture rollback; the cleanup path removes the whole
// worktree anyway).
func (s *Store) DeleteQABaselineRecordFiles(worktreeDir string) error {
	if strings.TrimSpace(worktreeDir) == "" {
		return nil
	}
	manifestErr := os.Remove(gitworktree.QABaselineManifestPath(worktreeDir))
	baselineErr := os.Remove(gitworktree.QABaselinePath(worktreeDir))
	if manifestErr != nil && !os.IsNotExist(manifestErr) {
		return manifestErr
	}
	if baselineErr != nil && !os.IsNotExist(baselineErr) {
		return baselineErr
	}
	return nil
}
