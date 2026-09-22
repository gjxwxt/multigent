package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/multigent/multigent/internal/changerun"
	"github.com/multigent/multigent/internal/gitworktree"
)

// reviewChangeRunSettingKey toggles the 30.10 approved evolution: when set to
// "true" on the workspace, an approved human review records the review commit
// as an applied Change Run proposal so the operator gains one-click atomic
// rollback from the console. The legacy direct commit/push path stays intact —
// the proposal is bookkeeping on top, never a gate, so flipping the flag off
// (or any proposal bookkeeping failure) cannot break a human-approved review.
// Per the 30.10 second adjudication the DEFAULT flip requires explicit user
// confirmation; until then the flag must be set to "true" per workspace.
const reviewChangeRunSettingKey = "enable_review_changerun_proposal"

// reviewChangeRunEnabled reads the workspace feature flag. Default off: per
// the 30.10 second adjudication the default flip requires explicit user
// confirmation.
func (s *Server) reviewChangeRunEnabled() bool {
	if s == nil || s.controlDB == nil {
		return false
	}
	value, ok, err := s.controlDB.GetSetting(reviewChangeRunSettingKey)
	if err != nil || !ok {
		return false
	}
	return strings.TrimSpace(value) == "true"
}

// recordReviewChangeRun records an already-merged review commit as an applied
// Change Run proposal. The review commit has landed between preimageSHA and
// checkpointSHA (the step-1 rollback anchor pair); we synthesize the patch
// from that SHA pair, persist a proposal, and walk it awaiting_approval →
// applying → applied so the operator gains one-click atomic rollback from the
// console. Best-effort by design: any failure here is logged and swallowed —
// the approved review itself must never fail on proposal bookkeeping.
func (s *Server) recordReviewChangeRun(workspaceID, project, taskID, preimageSHA, checkpointSHA string) {
	defer func() {
		// Proposal bookkeeping is observability, not a gate: any panic here
		// must not take down the approved review path.
		if r := recover(); r != nil {
			log.Printf("[review-changerun] panic recording proposal for task %s (project %s): %v", taskID, project, r)
		}
	}()

	if preimageSHA == "" || checkpointSHA == "" || preimageSHA == checkpointSHA {
		return
	}
	store := s.changerunStore()
	if store == nil {
		return
	}

	// The worktree content equals the checkpoint content right now (the
	// review commit just landed), so the diff and the postimage hashes both
	// come from the commit root the caller resolved.
	task, _, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		log.Printf("[review-changerun] task %s not found in project %s", taskID, project)
		return
	}
	wtDir := strings.TrimSpace(task.WorktreeDir)
	if wtDir == "" || func() bool { _, err := os.Stat(filepath.Join(wtDir, ".git")); return err != nil }() {
		wtDir = s.resolveProjectGitRoot(project)
	}
	if _, err := os.Stat(filepath.Join(wtDir, ".git")); err != nil {
		log.Printf("[review-changerun] no git worktree available for task %s (project %s)", taskID, project)
		return
	}

	patch, err := gitDiffOutput(wtDir, preimageSHA+".."+checkpointSHA)
	if err != nil {
		log.Printf("[review-changerun] diff %s..%s failed for task %s (project %s): %v", shortSHA(preimageSHA), shortSHA(checkpointSHA), taskID, project, err)
		return
	}
	if strings.TrimSpace(patch) == "" {
		return
	}
	touched := changerun.PatchTouchedPaths(patch)

	p, err := store.Create(project, taskID, "system",
		fmt.Sprintf("审核通过收编（review commit %s → %s），自动记录为已应用 proposal 以获得原子回滚能力", shortSHA(preimageSHA), shortSHA(checkpointSHA)),
		patch, patch, touched)
	if err != nil {
		log.Printf("[review-changerun] create proposal failed for task %s (project %s): %v", taskID, project, err)
		return
	}

	// Walk awaiting_approval → applying → applied. Postimage hashes come from
	// the worktree AFTER the commit landed (worktree content == checkpoint
	// content), so the hashes are the true postimage of the patch.
	if _, err := store.BeginApply(p.ID); err != nil {
		log.Printf("[review-changerun] begin apply failed for proposal %s: %v", p.ID, err)
		return
	}
	postimage, err := hashPostimage(wtDir, touched)
	if err != nil {
		log.Printf("[review-changerun] postimage hash failed for proposal %s: %v", p.ID, err)
		return
	}
	if _, err := store.MarkApplied(p.ID, postimage, checkpointSHA); err != nil {
		log.Printf("[review-changerun] mark applied failed for proposal %s: %v", p.ID, err)
		return
	}

	workspaceID2, _ := s.currentWorkspaceID()
	s.auditLog(auditLogInput{
		WorkspaceID:  workspaceID2,
		ActorType:    "system",
		ActorID:      "system",
		Action:       "review.commit_recorded_as_changerun",
		ResourceType: "change_run_proposal",
		ResourceID:   p.ID,
		Summary:      fmt.Sprintf("review commit %s..%s recorded as applied change run for task %s (project %s)", shortSHA(preimageSHA), shortSHA(checkpointSHA), taskID, project),
		After: map[string]any{
			"project":       project,
			"taskId":        taskID,
			"proposalId":    p.ID,
			"preimageSha":   preimageSHA,
			"checkpointSha": checkpointSHA,
			"touchedPaths":  touched,
		},
	})
}

// gitDiffOutput runs a bounded read-only git diff (SanitizedGitEnv, stdout
// only) — the worktree content equals the checkpoint content at this moment,
// so the diff between the two SHAs is the true patch of the review commit.
func gitDiffOutput(dir, rangeSpec string) (string, error) {
	cmd := exec.Command("git", "diff", rangeSpec)
	cmd.Dir = dir
	cmd.Env = gitworktree.SanitizedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s: %w", rangeSpec, err)
	}
	return string(out), nil
}

// hashPostimage hashes the touched files' current (post-commit) content —
// the true postimage of the synthesized patch. Deletions hash as "absent".
func hashPostimage(root string, touched []string) (map[string]string, error) {
	post := map[string]string{}
	for _, path := range touched {
		abs := filepath.Join(root, filepath.FromSlash(path))
		h, err := hashFileSha(abs)
		if os.IsNotExist(err) {
			post[path] = "absent" // deletions are a valid postimage
			continue
		}
		if err != nil {
			return post, err
		}
		post[path] = h
	}
	return post, nil
}

func hashFileSha(path string) (string, error) {
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
