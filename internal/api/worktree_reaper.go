package api

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
)

// worktreeReaperInterval is the self-heal sweep cadence. Worktrees are only
// pure garbage once their task is terminal AND uncommitted work has been
// checkpointed, so an hourly sweep is plenty; the first sweep is delayed so a
// restart does not coincide with scheduled runs starting up.
const (
	worktreeReaperInterval = time.Hour
	worktreeReaperStartup  = 5 * time.Minute
)

// startWorktreeReaper launches the idempotent self-heal singleton: terminal
// tasks leave their worktree directories behind (only the manual cleanup
// endpoint reclaimed them before), and they accumulate — the 2026-09-22 VM
// sweep found 309MB across three done_success tasks from a single test day.
// The sweep reuses the manual cleanup endpoint's exact safe sequence: stop the
// preview (bind-mounts the worktree), checkpoint uncommitted work onto the
// task branch, then remove the worktree. Failure on one task is logged and
// skipped, never fatal.
func (s *Server) startWorktreeReaper(ctx context.Context) {
	if s == nil || s.controlDB == nil || s.ts == nil || s.worktreeMgr == nil {
		return
	}
	s.worktreeReaperOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		s.worktreeReaperCancel = cancel
		done := make(chan struct{})
		s.worktreeReaperDone = done
		go s.worktreeReaperLoop(loopCtx, done)
	})
}

func (s *Server) stopWorktreeReaper() {
	if s == nil || s.worktreeReaperCancel == nil {
		return
	}
	s.worktreeReaperCancel()
	select {
	case <-s.worktreeReaperDone:
	case <-time.After(10 * time.Second):
	}
}

func (s *Server) worktreeReaperLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(worktreeReaperStartup)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.sweepTerminalTaskWorktrees(ctx)
		timer.Reset(worktreeReaperInterval)
	}
}

// sweepTerminalTaskWorktrees walks live projects, finds worktree directories
// whose task is terminal, and reclaims them through the same fenced sequence
// the manual endpoint performs. Directories with no task record are counted
// and reported but left for explicit orphan deletion (the project-level
// orphan scan owns that surface; automatic deletion without a record to
// consult is how good data gets lost).
func (s *Server) sweepTerminalTaskWorktrees(ctx context.Context) {
	projects, err := s.st.ListProjects()
	if err != nil {
		log.Printf("[worktree-reaper] list projects: %v", err)
		return
	}
	reclaimed, bytesFreed, skipped, orphans := 0, int64(0), 0, 0
	for _, project := range projects {
		if ctx.Err() != nil {
			break
		}
		name := project.Name
		gitRoot := s.resolveProjectGitRoot(name)
		wtRoot := filepath.Join(gitRoot, ".multigent", "worktrees")
		entries, err := os.ReadDir(wtRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			taskID := e.Name()
			_, _, task, err := s.ts.FindTaskByID(taskID)
			if err != nil || task == nil {
				orphans++
				continue
			}
			if !task.Status.IsTerminal() {
				continue
			}
			wtDir := taskWorktreeDir(task, gitRoot, taskID)
			size := dirSize(wtDir)
			if err := s.reclaimTerminalWorktree(name, task, taskID, gitRoot, wtDir); err != nil {
				skipped++
				log.Printf("[worktree-reaper] task %s (project %s): skipped: %v", taskID, name, err)
				continue
			}
			reclaimed++
			bytesFreed += size
			log.Printf("[worktree-reaper] task %s (project %s): worktree reclaimed (%d bytes)", taskID, name, size)
		}
	}
	if reclaimed > 0 || orphans > 0 || skipped > 0 {
		log.Printf("[worktree-reaper] sweep: %d reclaimed (%d bytes), %d skipped, %d recordless dirs left for explicit orphan cleanup",
			reclaimed, bytesFreed, skipped, orphans)
	}
}

// reclaimTerminalWorktree mirrors handlePostTaskWorktreeCleanup's safe order:
// stop the preview first (it bind-mounts the worktree), checkpoint any
// uncommitted work onto the task branch, then remove the worktree. The
// checkpoint is best-effort in the same direction as the manual endpoint —
// but here a checkpoint FAILURE is treated as skip-not-delete, because an
// unattended sweep must never destroy unpushed work.
func (s *Server) reclaimTerminalWorktree(project string, task *entity.Task, taskID, gitRoot, wtDir string) error {
	if s.previewEngine != nil {
		if inst, ok := s.previewEngine.GetInstance(taskID); ok && inst != nil {
			if err := s.previewEngine.StopEphemeralPreview(taskID); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(wtDir); err == nil {
		sha, wasDirty, err := s.worktreeMgr.CommitWorktreeState(wtDir, "chore(reaper): checkpoint uncommitted work before worktree removal")
		if err != nil {
			return err
		}
		if wasDirty && sha != "" {
			// Uncommitted work existed; push it best-effort exactly like the
			// manual endpoint does so the checkpoint is not lost with the
			// directory. Removal proceeds: the commit is on the task branch.
			s.pushTaskBranchBestEffort(project, task, wtDir, sha)
		}
	}
	if err := s.worktreeMgr.CleanupWorktree(gitRoot, taskID); err != nil {
		// Sandbox-written root-owned files (node_modules etc.) defeat the
		// service process's rm exactly like the historical project orphans
		// did. Repair ownership once, retry the removal; if it STILL survives,
		// surface the same truthful failure the orphan delete endpoint does —
		// removal then needs elevated permissions on the host.
		_ = exec.Command("chmod", "-R", "u+rwX", wtDir).Run()
		if err := s.worktreeMgr.CleanupWorktree(gitRoot, taskID); err != nil {
			return fmt.Errorf("worktree survives ownership repair (root-owned files? remove manually with elevated permissions): %w", err)
		}
	}
	return nil
}
