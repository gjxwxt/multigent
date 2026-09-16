package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// withTestUser injects the auth identity the withTokenAuth middleware would.
func withTestUser(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, ctxUserKey, username)
}

// setupChangeRunTestServer builds a Server whose project "resproj" has a
// real git repo workspace, a task with a WorktreeDir, and an operator user.
func setupChangeRunTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, wsID := setupUserIMTestServer(t)

	// Real git repo as the task worktree + project root.
	repo := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+repo)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\n\nfunc Hi() string { return \"hi\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "baseline")

	// Project dir layout: <root>/projects/resproj with .multigent so the
	// project lock resolves through the same path the worktree manager uses.
	projectDir := filepath.Join(s.root, "projects", "resproj")
	if err := os.MkdirAll(filepath.Join(projectDir, ".multigent", "worktrees"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := s.st.SaveProject("resproj", &entity.Project{Name: "resproj", Repo: repo}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// Seed the task (AddTask writes the durable record index that
	// findTaskInProject falls back to).
	task := &entity.Task{
		ID:          "task-cr-1",
		Title:       "change run target",
		Status:      entity.TaskStatusInProgress,
		WorktreeDir: repo,
	}
	if err := s.ts.AddTask("resproj", "backend-dev", task); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// Operator user.
	_ = s.users.CreateUser("op-user", "pass123", RoleMember, "", "", "", "", "")
	_ = s.controlDB.UpsertWorkspaceMember(wsID, "op-user", WorkspaceRoleMember)
	_ = s.users.UpdateUser("op-user", nil, nil, nil, nil, nil, nil, nil, []projectAccess{{Project: "resproj", Role: ProjectRoleOperator}}, nil, nil)
	return s, repo
}

func authedReq(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		buf = *bytes.NewReader(raw)
	} else {
		buf = *bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	// withTokenAuth needs a real token; tests exercise the RBAC gates by
	// injecting the identity directly (same pattern as agent_env_policy_test).
	req = req.WithContext(withTestUser(req.Context(), "op-user"))
	rec := httptest.NewRecorder()
	s.changerunRoute(rec, req)
	return rec
}

// changerunRoute dispatches to the change-run handler the same way the mux
// pattern would, extracting path values from the request URL.
func (s *Server) changerunRoute(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/projects/"), "/")
	// /{name}/tasks/{taskId}/change-runs[/{proposalId}[/{action}]]
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("name", parts[0])
	r.SetPathValue("taskId", parts[2])
	rest := parts[4:]
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		s.handleListChangeRunProposals(w, r)
	case len(rest) == 0 && r.Method == http.MethodPost:
		s.handleCreateChangeRunProposal(w, r)
	case len(rest) == 1 && r.Method == http.MethodGet:
		r.SetPathValue("proposalId", rest[0])
		s.handleGetChangeRunProposal(w, r)
	case len(rest) == 2 && r.Method == http.MethodPost:
		r.SetPathValue("proposalId", rest[0])
		switch rest[1] {
		case "apply":
			s.handleApplyChangeRunProposal(w, r)
		case "rollback":
			s.handleRollbackChangeRunProposal(w, r)
		case "reject":
			s.handleRejectChangeRunProposal(w, r)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func TestChangeRunCreateRequiresOperator(t *testing.T) {
	s, _ := setupChangeRunTestServer(t)
	// Viewer-level access must be rejected on create.
	_ = s.users.CreateUser("viewer-user", "pass123", RoleMember, "", "", "", "", "")
	req := httptest.NewRequest("POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs",
		strings.NewReader(`{"patch":"x"}`))
	req = req.WithContext(withTestUser(req.Context(), "viewer-user"))
	rec := httptest.NewRecorder()
	s.changerunRoute(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create must 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChangeRunCreateApplyRollbackOverHTTP(t *testing.T) {
	s, repo := setupChangeRunTestServer(t)

	patch := `diff --git a/app.go b/app.go
--- a/app.go
+++ b/app.go
@@ -1,3 +1,3 @@
 package main
 
-func Hi() string { return "hi" }
+func Hi() string { return "hello" }
`
	// Create
	rec := authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs", map[string]any{
		"actor":   "copilot",
		"request": "greet differently",
		"patch":   patch,
		"paths":   []string{"app.go"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.State != "awaiting_approval" || !strings.HasPrefix(created.ID, "crp-") {
		t.Fatalf("created proposal wrong: %+v", created)
	}

	// Apply
	rec = authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs/"+created.ID+"/apply", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	content, _ := os.ReadFile(filepath.Join(repo, "app.go"))
	if !strings.Contains(string(content), `"hello"`) {
		t.Fatalf("patch not applied: %s", content)
	}

	// Rollback
	rec = authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs/"+created.ID+"/rollback", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body.String())
	}
	content, _ = os.ReadFile(filepath.Join(repo, "app.go"))
	if !strings.Contains(string(content), `"hi"`) {
		t.Fatalf("rollback did not restore: %s", content)
	}
}

func TestChangeRunListAndReject(t *testing.T) {
	s, _ := setupChangeRunTestServer(t)
	patch := "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n"
	rec := authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs", map[string]any{
		"patch": patch, "paths": []string{"app.go"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Second create for the same task conflicts (single active).
	rec = authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs", map[string]any{
		"patch": patch, "paths": []string{"app.go"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second active create must 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// List shows the active one.
	rec = authedReq(t, s, "GET", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs", nil)
	var listed struct {
		Proposals []map[string]any `json:"proposals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Proposals) != 1 || listed.Proposals[0]["id"] != created.ID {
		t.Fatalf("list mismatch: %s", rec.Body.String())
	}

	// Reject terminates it; then a new create succeeds.
	rec = authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs/"+created.ID+"/reject", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	rec = authedReq(t, s, "POST", "/api/v1/projects/resproj/tasks/task-cr-1/change-runs", map[string]any{
		"patch": patch, "paths": []string{"app.go"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after reject: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChangeRunCrossProjectProposalIs404(t *testing.T) {
	s, _ := setupChangeRunTestServer(t)

	// A second project "other" with its own repo and task.
	otherRepo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = otherRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+otherRepo)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(otherRepo, "config.go"), []byte("package other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "baseline")
	if err := s.st.SaveProject("other", &entity.Project{Name: "other", Repo: otherRepo}); err != nil {
		t.Fatal(err)
	}
	task2 := &entity.Task{ID: "task-other-1", Title: "other target", Status: entity.TaskStatusInProgress, WorktreeDir: otherRepo}
	if err := s.ts.AddTask("other", "backend-dev", task2); err != nil {
		t.Fatal(err)
	}
	_ = s.users.UpdateUser("op-user", nil, nil, nil, nil, nil, nil, nil, []projectAccess{{Project: "resproj", Role: ProjectRoleOperator}, {Project: "other", Role: ProjectRoleOperator}}, nil, nil)

	// op-user creates a proposal under project "other".
	otherPatch := "diff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -1 +1 @@\n-package other\n+package other2\n"
	rec := authedReq(t, s, "POST", "/api/v1/projects/other/tasks/task-other-1/change-runs", map[string]any{
		"patch": otherPatch, "paths": []string{"config.go"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create under other: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Now attack: apply/reject/rollback that proposal through resproj's URL.
	for _, action := range []string{"apply", "reject", "rollback"} {
		path := "/api/v1/projects/resproj/tasks/task-cr-1/change-runs/" + created.ID
		if action != "list" {
			path += "/" + action
		}
		rec := authedReq(t, s, "POST", path, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("cross-project %s must 404, got %d: %s", action, rec.Code, rec.Body.String())
		}
	}
	// The other project's proposal is untouched and its worktree unchanged.
	content, _ := os.ReadFile(filepath.Join(otherRepo, "config.go"))
	if string(content) != "package other\n" {
		t.Fatalf("cross-project apply mutated the foreign worktree: %s", content)
	}
}
