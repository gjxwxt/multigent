package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/gitworktree"
)

// diffTestRepo builds a two-commit repository and returns its path plus the two
// commit hashes, so the endpoint is exercised against real git objects rather
// than a mocked manager.
func diffTestRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@multigent.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@multigent.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package main\n\nfunc handle() error { return nil }\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git("add", "handler.go")
	git("commit", "-q", "-m", "baseline")
	base = git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package main\n\nimport \"errors\"\n\nfunc handle() error { return errors.New(\"boom\") }\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git("add", "handler.go")
	git("commit", "-q", "-m", "agent work")
	head = git("rev-parse", "HEAD")
	return dir, base, head
}

func seedDiffTask(t *testing.T, s *Server, workspaceID string, mutate func(*entity.Task)) *entity.Task {
	t.Helper()
	seedSampleAgentsForTest(t, s, workspaceID)
	s.worktreeMgr = newTestWorktreeManager(t)
	now := time.Now().UTC()
	task := &entity.Task{
		ID: "task-diff-1", Title: "Diff me", Status: entity.TaskStatusAwaitingConfirmation,
		Priority: 2, Assignee: "pm", CreatedAt: now, UpdatedAt: now,
	}
	mutate(task)
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return task
}

func getTaskDiff(t *testing.T, s *Server, project, taskID, query string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/projects/" + project + "/tasks/" + taskID + "/diff"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("name", project)
	req.SetPathValue("taskId", taskID)
	if authenticated {
		req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))
	}
	rec := httptest.NewRecorder()
	s.handleGetTaskDiff(rec, req)
	return rec
}

func TestTaskDiffEndpointReturnsRealDiffForRecordedCommits(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	dir, base, head := diffTestRepo(t)
	task := seedDiffTask(t, s, workspaceID, func(tk *entity.Task) {
		tk.WorktreeDir = dir
		tk.BaseCommit = base
		tk.CompletionCommit = head
	})

	rec := getTaskDiff(t, s, "sample", task.ID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var diff gitworktree.Diff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if diff.Base != base || diff.Head != head {
		t.Fatalf("range not derived from the task's recorded commits: %#v", diff)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "handler.go" {
		t.Fatalf("unexpected files: %#v", diff.Files)
	}
	if !strings.Contains(diff.Patch, `+func handle() error { return errors.New("boom") }`) {
		t.Fatalf("patch does not contain the agent's change:\n%s", diff.Patch)
	}
}

func TestTaskDiffEndpointRejectsRefsAndRevisionSyntax(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	dir, base, head := diffTestRepo(t)
	task := seedDiffTask(t, s, workspaceID, func(tk *entity.Task) {
		tk.WorktreeDir = dir
		tk.BaseCommit = base
		tk.CompletionCommit = head
	})

	for _, query := range []string{
		"base=main&head=" + head,
		"base=" + base + "&head=HEAD",
		"base=" + base + "%5E&head=" + head,
		"base=--output%3D%2Ftmp%2Fpwn&head=" + head,
		"base=" + head + "&head=" + base + ".." + head,
	} {
		rec := getTaskDiff(t, s, "sample", task.ID, query, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status=%d, want 400 body=%s", query, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); strings.Contains(body, "func handle") || strings.Contains(body, "@@") {
			t.Fatalf("query %q returned diff content despite rejection: %s", query, body)
		}
	}
}

func TestTaskDiffEndpointRequiresBothRecordedCommits(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	dir, _, head := diffTestRepo(t)
	task := seedDiffTask(t, s, workspaceID, func(tk *entity.Task) {
		tk.WorktreeDir = dir
		tk.CompletionCommit = head // no baseline recorded
	})
	rec := getTaskDiff(t, s, "sample", task.ID, "", true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskDiffEndpointUnknownCommitIsNotFoundNotCrash(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	dir, base, head := diffTestRepo(t)
	task := seedDiffTask(t, s, workspaceID, func(tk *entity.Task) {
		tk.WorktreeDir = dir
		tk.BaseCommit = base
		tk.CompletionCommit = head
	})
	missing := strings.Repeat("ab", 20)
	rec := getTaskDiff(t, s, "sample", task.ID, "base="+missing+"&head="+head, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// A token that names a task in another project must not read that task's diff.
func TestTaskDiffEndpointScopesToTheTaskOwningProject(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	dir, base, head := diffTestRepo(t)
	task := seedDiffTask(t, s, workspaceID, func(tk *entity.Task) {
		tk.WorktreeDir = dir
		tk.BaseCommit = base
		tk.CompletionCommit = head
	})
	rec := getTaskDiff(t, s, "other-project", task.ID, "", true)
	if rec.Code == http.StatusOK {
		t.Fatalf("cross-project lookup returned a diff: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// The boundary is the router, not the handler: a side-effect-free read still
// must not be reachable without a token (security invariant #1 — publicMux has
// no authentication at all).
func TestTaskDiffEndpointRequiresAuthentication(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	dir, base, head := diffTestRepo(t)
	task := seedDiffTask(t, s, workspaceID, func(tk *entity.Task) {
		tk.WorktreeDir = dir
		tk.BaseCommit = base
		tk.CompletionCommit = head
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/sample/tasks/"+task.ID+"/diff", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", rec.Code, rec.Body.String())
	}
}
