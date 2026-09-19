package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/multigent/multigent/internal/gitworktree"
)

// handleGetTaskDiff answers "what did this task actually change" for a human
// review gate. Before this endpoint existed the reviewer saw only the fields
// the authoring agent chose to declare, which means approval was signed against
// a self-report rather than against the diff.
//
// Both sides of the range are immutable commit hashes: the recorded baseline and
// completion commits, or explicit query parameters validated as hashes. Refs and
// revision syntax are rejected so a caller cannot widen the range into commits
// that belong to someone else.
func (s *Server) handleGetTaskDiff(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	task := s.projectTaskResourceGuard(w, r, project, taskID)
	if task == nil {
		return
	}
	if s.worktreeMgr == nil {
		s.jsonError(w, http.StatusServiceUnavailable, "worktree manager is unavailable on this console")
		return
	}

	base := strings.TrimSpace(r.URL.Query().Get("base"))
	if base == "" {
		base = strings.TrimSpace(task.BaseCommit)
	}
	head := strings.TrimSpace(r.URL.Query().Get("head"))
	if head == "" {
		head = strings.TrimSpace(task.CompletionCommit)
	}
	if base == "" || head == "" {
		s.jsonError(w, http.StatusConflict, "this task has no recorded baseline and completion commit yet, so there is nothing to compare")
		return
	}
	normalizedBase, err := gitworktree.ValidateCommitSHA("base", base)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	normalizedHead, err := gitworktree.ValidateCommitSHA("head", head)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	gitRoot := s.resolveProjectGitRoot(project)
	dir := taskWorktreeDir(task, gitRoot, taskID)
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
		// Completed tasks frequently have their worktree reclaimed; the commits
		// still live in the project checkout's object database.
		if _, rootErr := os.Stat(filepath.Join(gitRoot, ".git")); rootErr != nil {
			s.jsonError(w, http.StatusConflict, "project has no local git checkout to read the diff from")
			return
		}
		dir = gitRoot
	}

	diff, err := s.worktreeMgr.DiffCommits(dir, normalizedBase, normalizedHead)
	if err != nil {
		switch {
		case errors.Is(err, gitworktree.ErrUnknownCommit):
			s.jsonError(w, http.StatusNotFound, "one of the commits is not present in this repository")
		default:
			slog.Warn("task diff failed", "project", project, "task", taskID, "error", err)
			s.jsonError(w, http.StatusInternalServerError, "could not read the diff for this task")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(diff)
}
