// Change Run API (Task 3.1 subfeature 3): proposals against a task worktree
// — create, read, apply, rollback, reject. Security posture per the standing
// red lines: routes live on the main (token-authenticated) mux — never
// publicMux; every handler checks project access, and every state-changing
// handler requires project OPERATOR. Apply/rollback shell out to git only
// through the changerun engine, which runs sanitized git under the
// cross-process project lock.
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/multigent/multigent/internal/changerun"
	"github.com/multigent/multigent/internal/gitworktree"
)

// changerunStore resolves the proposal store for the current workspace.
func (s *Server) changerunStore() *changerun.Store {
	return changerun.NewStore(s.controlDB, s.currentWorkspaceIDValue(nil))
}

// resolveChangeRunTask checks project access, locates the task, and returns
// the patch target (worktree dir + project root). It writes the error
// response itself when returning false.
func (s *Server) resolveChangeRunTask(w http.ResponseWriter, r *http.Request, project, taskID string) (worktreeDir, projectRoot string, ok bool) {
	if !s.checkProjectAccess(w, r, project) {
		return "", "", false
	}
	task, _, err := s.findTaskInProject(project, taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return "", "", false
	}
	worktreeDir = s.resolveTaskWorktreeDir(project, taskID)
	if _, statErr := os.Stat(filepath.Join(worktreeDir, ".git")); statErr != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, "task has no git worktree to change-run against")
		return "", "", false
	}
	return worktreeDir, s.st.ProjectDir(project), true
}

// changeRunEngine builds an ApplyEngine for a target. The engine locks on
// the worktree-derived project root so it serializes with the same lock the
// worktree manager and review commits hold.
func (s *Server) changeRunEngine(worktreeDir string) *changerun.ApplyEngine {
	cloneParent := filepath.Join(os.TempDir(), "multigent-changerun")
	_ = os.MkdirAll(cloneParent, 0o755)
	return &changerun.ApplyEngine{
		WorktreeDir: worktreeDir,
		ProjectRoot: gitworktree.ProjectRootForWorktree(worktreeDir),
		CloneParent: cloneParent,
	}
}

type createChangeRunProposalRequest struct {
	Actor   string   `json:"actor"`
	Request string   `json:"request"`
	Patch   string   `json:"patch"`
	Diff    string   `json:"diff"`
	Paths   []string `json:"paths"`
}

// handleCreateChangeRunProposal persists a new proposal (operator gate).
func (s *Server) handleCreateChangeRunProposal(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if _, _, ok := s.resolveChangeRunTask(w, r, project, taskID); !ok {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	var req createChangeRunProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Patch) == "" && strings.TrimSpace(req.Diff) == "" {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, "patch or diff is required")
		return
	}
	if err := changerun.ValidatePaths(req.Paths); err != nil {
		s.jsonErrorCode(w, http.StatusBadRequest, ErrCodeValidationFailed, err.Error())
		return
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = s.currentUser(r).Username
	}
	p, err := s.changerunStore().Create(project, taskID, actor, req.Request, req.Patch, req.Diff, req.Paths)
	if err != nil {
		s.jsonErrorCode(w, http.StatusConflict, ErrCodeValidationFailed, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(p)
}

// handleGetChangeRunProposal returns one proposal.
func (s *Server) handleGetChangeRunProposal(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	p, err := s.changerunStore().Get(r.PathValue("proposalId"))
	if err != nil || p.Project != project || p.TaskID != taskID {
		s.jsonError(w, http.StatusNotFound, "proposal not found")
		return
	}
	_ = json.NewEncoder(w).Encode(p)
}

// handleListChangeRunProposals lists the task's active proposal (if any).
func (s *Server) handleListChangeRunProposals(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if !s.checkProjectAccess(w, r, project) {
		return
	}
	active, err := s.changerunStore().ActiveForTask(project, taskID)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	proposals := []*changerun.Proposal{}
	if active != nil {
		proposals = append(proposals, active)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"proposals": proposals})
}

// handleApplyChangeRunProposal runs validate-clone-apply (operator gate).
func (s *Server) handleApplyChangeRunProposal(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	worktreeDir, _, ok := s.resolveChangeRunTask(w, r, project, taskID)
	if !ok {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	res, err := s.changeRunEngine(worktreeDir).Apply(s.changerunStore(), r.PathValue("proposalId"), s.currentUser(r).Username)
	if err != nil {
		status := http.StatusConflict
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), " rows") {
			status = http.StatusNotFound
		}
		s.jsonErrorCode(w, status, ErrCodeValidationFailed, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// handleRollbackChangeRunProposal surgically restores touched paths (operator).
func (s *Server) handleRollbackChangeRunProposal(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	worktreeDir, _, ok := s.resolveChangeRunTask(w, r, project, taskID)
	if !ok {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	p, err := s.changeRunEngine(worktreeDir).Rollback(s.changerunStore(), r.PathValue("proposalId"))
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), " rows") {
			status = http.StatusNotFound
		}
		s.jsonErrorCode(w, status, ErrCodeValidationFailed, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(p)
}

// handleRejectChangeRunProposal terminates a proposal without applying (operator).
func (s *Server) handleRejectChangeRunProposal(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := r.PathValue("taskId")
	if _, _, ok := s.resolveChangeRunTask(w, r, project, taskID); !ok {
		return
	}
	if !s.checkProjectOperator(w, r, project) {
		return
	}
	p, err := s.changerunStore().Reject(r.PathValue("proposalId"), s.currentUser(r).Username)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), " rows") {
			status = http.StatusNotFound
		}
		s.jsonErrorCode(w, status, ErrCodeValidationFailed, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(p)
}
