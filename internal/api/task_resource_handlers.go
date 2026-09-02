package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
	"github.com/multigent/multigent/internal/preview"
)

// previewEngineAPI is the slice of the preview engine the API server depends
// on. Keeping it an interface lets resource tests fake the engine instead of
// shelling docker, without dragging the whole Engine type into server.go.
// SeedInstanceForTest is part of the engine's public test surface.
type previewEngineAPI interface {
	GetInstance(taskID string) (*preview.PreviewInstance, bool)
	StartEphemeralPreview(ctx context.Context, taskID, projectName, worktreeDir string) (*preview.PreviewInstance, error)
	StartSnapshotPreview(ctx context.Context, taskID, projectName, worktreeDir string) (*preview.PreviewInstance, error)
	StopEphemeralPreview(taskID string) error
	Reconcile(ctx context.Context) error
	SeedInstanceForTest(inst *preview.PreviewInstance)
}

// compile-time proof that *preview.Engine satisfies the consumption surface.
var _ previewEngineAPI = (*preview.Engine)(nil)

type taskWorktreeResource struct {
	Exists          bool   `json:"exists"`
	Branch          string `json:"branch,omitempty"`
	DirtyFiles      int    `json:"dirtyFiles,omitempty"`
	UnpushedCommits int    `json:"unpushedCommits,omitempty"`
	DiskBytes       *int64 `json:"diskBytes,omitempty"`
	DiskTimedOut    bool   `json:"diskTimedOut,omitempty"`
}

type taskPreviewResource struct {
	Status    string    `json:"status"`
	Port      int       `json:"port,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	ReadOnly  bool      `json:"readOnly,omitempty"`
}

type taskResourcesResponse struct {
	Worktree   taskWorktreeResource `json:"worktree"`
	Preview    *taskPreviewResource `json:"preview,omitempty"`
	Locked     bool                 `json:"locked"`
	LockReason string               `json:"lockReason,omitempty"`
}

// projectTaskResourceGuard resolves the task behind a project-scoped resource
// endpoint, enforcing project access on the way. It writes the error response
// itself and returns nil when access is denied.
func (s *Server) projectTaskResourceGuard(w http.ResponseWriter, r *http.Request, project, taskID string) *entity.Task {
	if !s.checkProjectAccess(w, r, project) {
		return nil
	}
	taskProject, _, task, err := s.ts.FindTaskByID(taskID)
	if err != nil || task == nil {
		s.jsonError(w, http.StatusNotFound, "task not found")
		return nil
	}
	if strings.TrimSpace(taskProject) != "" && taskProject != project {
		s.jsonError(w, http.StatusNotFound, "task not found in this project")
		return nil
	}
	return task
}

// taskWorktreeDir resolves the worktree directory for a task without creating
// anything: the recorded path when it still exists, else the conventional
// per-task path under the project root.
func taskWorktreeDir(task *entity.Task, gitRoot, taskID string) string {
	if task.WorktreeDir != "" {
		if st, err := os.Stat(task.WorktreeDir); err == nil && st.IsDir() {
			return task.WorktreeDir
		}
	}
	return gitworktree.WorktreeDir(gitRoot, taskID)
}

// countGitLines counts non-empty porcelain lines from one read-only git
// command; display only, cleanup re-derives state under the project lock.
func countGitLines(dir string, args ...string) int {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func countUnpushedCommits(worktreeDir, baseBranch string) int {
	ref := "origin/" + strings.TrimPrefix(strings.TrimSpace(baseBranch), "origin/")
	if strings.TrimSpace(baseBranch) == "" {
		ref = "origin/main"
	}
	return countGitLines(worktreeDir, "rev-list", "--count", ref+"..HEAD")
}

// worktreeDiskBytes measures the worktree size with a hard timeout so a huge
// node_modules cannot stall the popover. Timed-out measurements surface as
// DiskTimedOut instead of a wrong number.
func worktreeDiskBytes(dir string) (*int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "du", "-sk", dir)
	out, err := cmd.Output()
	if err != nil {
		return nil, true
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return nil, true
	}
	var kb int64
	if _, err := fmt.Sscanf(fields[0], "%d", &kb); err != nil {
		return nil, true
	}
	bytes := kb * 1024
	return &bytes, false
}

func statDir(path string) (os.FileInfo, bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	return st, st.IsDir(), nil
}

// pushTaskBranchBestEffort pushes the checkpoint commit so cleanup cannot be
// the last thing that ever saw it. Failure is loud (task comment) but never
// blocks or reverts the cleanup — the commit stays on the local branch ref.
func (s *Server) pushTaskBranchBestEffort(project string, task *entity.Task, worktreeDir, sha string) {
	branchName := strings.TrimSpace(task.BranchName)
	if branchName == "" && s.worktreeMgr != nil {
		branchName, _ = s.worktreeMgr.CheckedOutBranch(worktreeDir)
	}
	if branchName == "" {
		return
	}
	remoteCmd := exec.Command("git", "remote", "get-url", "origin")
	remoteCmd.Dir = worktreeDir
	remoteOut, err := remoteCmd.Output()
	if err != nil || len(strings.TrimSpace(string(remoteOut))) == 0 {
		return
	}
	pushCmd := exec.Command("git", "push", "origin", branchName)
	pushCmd.Dir = worktreeDir
	pushCmd.Env = gitworktree.PushNetworkEnv()
	if out, err := pushCmd.CombinedOutput(); err != nil {
		log.Printf("[resource-cleanup] push checkpoint %s to %s failed for task %s: %v (%s)",
			shortSHA(sha), branchName, task.ID, err, gitworktree.RedactGitOutput(strings.TrimSpace(string(out))))
		if s.ts != nil {
			_ = s.ts.AddComment(project, task.Assignee, &entity.TaskComment{
				ID:        entity.NewCommentID(),
				TaskID:    task.ID,
				Author:    "system",
				Body:      fmt.Sprintf("⚠️ 清理 worktree 时的检查点提交 `%s` 推送到分支 `%s` 失败: %v（本地分支已保留）", shortSHA(sha), branchName, err),
				CreatedAt: time.Now().UTC(),
			})
		}
	}
}

// taskHasPreview reports whether a live preview instance exists for the task
// (engine memory; the reaper/Reconcile keep it convergent with docker).
func (s *Server) taskHasPreview(taskID string) bool {
	if s.previewEngine == nil {
		return false
	}
	inst, ok := s.previewEngine.GetInstance(taskID)
	return ok && inst != nil
}

// taskHasWorktree reports whether the task's worktree directory exists on
// disk. One cheap stat per listed task — no git calls in the list path.
func (s *Server) taskHasWorktree(task *entity.Task, project string) bool {
	if s.worktreeMgr == nil {
		return false
	}
	gitRoot := s.resolveProjectGitRoot(project)
	dir := taskWorktreeDir(task, gitRoot, task.ID)
	if dir == "" {
		return false
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return true
	}
	return false
}

// handleGetTaskResources reports what disk/container resources a task still
// holds: its worktree (git state + size) and its live preview instance.
func (s *Server) handleGetTaskResources(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	task := s.projectTaskResourceGuard(w, r, project, taskID)
	if task == nil {
		return
	}

	resp := taskResourcesResponse{}
	if !task.Status.IsTerminal() {
		resp.Locked = true
		resp.LockReason = fmt.Sprintf("task is %s; cancel or archive it before cleanup", task.Status)
	}

	gitRoot := s.resolveProjectGitRoot(project)
	worktreeDir := taskWorktreeDir(task, gitRoot, taskID)
	if _, isDir, err := statDir(worktreeDir); err == nil && isDir {
		wt := taskWorktreeResource{Exists: true}
		wt.Branch, _ = s.worktreeMgr.CheckedOutBranch(worktreeDir)
		wt.DirtyFiles = countGitLines(worktreeDir, "status", "--porcelain")
		wt.UnpushedCommits = countUnpushedCommits(worktreeDir, task.BaseBranch)
		bytes, timedOut := worktreeDiskBytes(worktreeDir)
		wt.DiskBytes = bytes
		wt.DiskTimedOut = timedOut
		resp.Worktree = wt
	}

	if inst, ok := s.previewEngine.GetInstance(taskID); ok && inst != nil {
		resp.Preview = &taskPreviewResource{
			Status:    inst.Status,
			Port:      inst.Port,
			ExpiresAt: inst.ExpiresAt,
			ReadOnly:  inst.ReadOnly,
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handlePostTaskWorktreeCleanup reclaims a task's preview container and
// worktree directory. Branches and task lifecycle fields are untouched — the
// checkpoint commit keeps any uncommitted work on the task branch. Order is
// fixed: stop preview first (its bind-mount roots on the worktree), then
// checkpoint, then remove; every failure aborts before the destructive step.
// Dirty state is re-derived here under the project lock — values shown by
// the GET endpoint are display-only (TOCTOU).
func (s *Server) handlePostTaskWorktreeCleanup(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	taskID := strings.TrimSpace(r.PathValue("taskId"))
	task := s.projectTaskResourceGuard(w, r, project, taskID)
	if task == nil {
		return
	}

	// Fail-closed: only explicitly terminal tasks may lose their worktree.
	if !task.Status.IsTerminal() {
		s.jsonError(w, http.StatusConflict, fmt.Sprintf("task %s is %s; cancel or archive it before cleaning up its worktree", taskID, task.Status))
		return
	}

	gitRoot := s.resolveProjectGitRoot(project)
	worktreeDir := taskWorktreeDir(task, gitRoot, taskID)
	_, worktreeExists, statErr := statDir(worktreeDir)

	// 1. Stop the preview container first — it bind-mounts the worktree.
	if inst, instOk := s.previewEngine.GetInstance(taskID); instOk && inst != nil {
		if err := s.previewEngine.StopEphemeralPreview(taskID); err != nil {
			s.jsonError(w, http.StatusConflict, fmt.Sprintf("stop preview failed, worktree untouched: %v", err))
			return
		}
	}

	// 2. Fold uncommitted work into a checkpoint commit on the task branch.
	var checkpointSHA string
	if worktreeExists {
		sha, wasDirty, err := s.worktreeMgr.CommitWorktreeState(worktreeDir, "chore(cleanup): checkpoint uncommitted work before worktree removal")
		if err != nil {
			s.jsonError(w, http.StatusInternalServerError, fmt.Sprintf("checkpoint commit failed, worktree untouched: %v", err))
			return
		}
		if wasDirty {
			checkpointSHA = sha
		}
	}

	// 3. Remove the worktree directory itself (no-op when it never existed).
	if statErr == nil || worktreeExists {
		if err := s.worktreeMgr.CleanupWorktree(gitRoot, taskID); err != nil {
			s.jsonError(w, http.StatusInternalServerError, fmt.Sprintf("worktree removal failed: %v", err))
			return
		}
	}

	if checkpointSHA != "" {
		s.pushTaskBranchBestEffort(project, task, worktreeDir, checkpointSHA)
		log.Printf("[resource-cleanup] task %s (project %s): checkpoint %s committed before worktree removal", taskID, project, shortSHA(checkpointSHA))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cleaned":       true,
		"checkpointSHA": checkpointSHA,
	})
}
