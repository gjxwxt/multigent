// Change Run API (Task 3.1 subfeature 3): proposals against a task worktree
// — create, read, apply, rollback, reject. Security posture per the standing
// red lines: routes live on the main (token-authenticated) mux — never
// publicMux; every handler checks project access, and every state-changing
// handler requires project OPERATOR. Apply/rollback shell out to git only
// through the changerun engine, which runs sanitized git under the
// cross-process project lock.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/changerun"
	"github.com/multigent/multigent/internal/fixturesandbox"
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
// worktree manager and review commits hold. Scope binds the engine to the
// URL's (project, task): proposals from other projects/tasks surface as 404,
// never as cross-target applies or state changes (round-18 P0-1). The
// validator executes the project's ChangeRunVerifyCommands inside a
// disposable container — project argv never runs on the host (round-18
// P0-3, security red line).
func (s *Server) changeRunEngine(worktreeDir, project, taskID string) *changerun.ApplyEngine {
	cloneParent := filepath.Join(os.TempDir(), "multigent-changerun")
	_ = os.MkdirAll(cloneParent, 0o755)
	engine := &changerun.ApplyEngine{
		WorktreeDir: worktreeDir,
		ProjectRoot: gitworktree.ProjectRootForWorktree(worktreeDir),
		CloneParent: cloneParent,
		Scope: changerun.Scope{
			Project: project,
			TaskID:  taskID,
		},
	}
	if p, err := s.st.Project(project); err == nil && p != nil && len(p.ChangeRunVerifyCommands) > 0 {
		engine.Validator = &changerun.ContainerValidator{
			Opts: changerun.ContainerVerifyOptions{
				Commands: p.ChangeRunVerifyCommands,
				Image:    fixturesandbox.GeneratorImage(),
			},
			RunContainer: runChangeRunVerifyContainer,
		}
	}
	return engine
}

// runChangeRunVerifyContainer executes one verification command inside a
// disposable container with dir mounted at /workspace (the fixturesandbox
// generator's execution shape). Output is combined; caller redacts.
func runChangeRunVerifyContainer(ctx context.Context, dir, image, workdir, argv string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{
		"run", "--rm",
		"-v", dir + ":/workspace",
		"-w", workdir,
		image,
		"sh", "-c", argv,
	}
	cmd := exec.CommandContext(runCtx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("timed out after %s", timeout)
	}
	return out.String(), err
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

// handleGetChangeRunProposal returns one proposal. A proposal from another
// project/task renders as 404 (scope indistinguishable from missing).
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
	res, err := s.changeRunEngine(worktreeDir, project, taskID).Apply(s.changerunStore(), r.PathValue("proposalId"), s.currentUser(r).Username, r.Context())
	if err != nil {
		var nf *changerun.NotFoundError
		if errors.As(err, &nf) {
			s.jsonError(w, http.StatusNotFound, "proposal not found")
			return
		}
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
	p, err := s.changeRunEngine(worktreeDir, project, taskID).Rollback(s.changerunStore(), r.PathValue("proposalId"))
	if err != nil {
		var nf *changerun.NotFoundError
		if errors.As(err, &nf) {
			s.jsonError(w, http.StatusNotFound, "proposal not found")
			return
		}
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
	proposalID := r.PathValue("proposalId")
	// Ownership precheck BEFORE the state change: a foreign proposal must
	// render as 404 without its state ever being touched (round-18 P0-1).
	stored, err := s.changerunStore().Get(proposalID)
	if err != nil || stored.Project != project || stored.TaskID != taskID {
		s.jsonError(w, http.StatusNotFound, "proposal not found")
		return
	}
	p, err := s.changerunStore().Reject(proposalID, s.currentUser(r).Username)
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
