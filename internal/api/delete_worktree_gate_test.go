package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

// Regression (P0.5-C, review 2026-09-14): cleanupProjectWorktrees used to
// swallow per-worktree failures — a DELETE could report success while managed
// worktrees (task code, potentially unpushed) stayed on disk. A surviving
// worktree must now fail the whole teardown with records kept.
func TestDeleteProjectFailsWhenWorktreeSurvives(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := os.MkdirAll(s.st.ProjectDir("sample"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	wt := filepath.Join(wsDir, ".multigent", "worktrees", "t-stuck")
	if err := os.MkdirAll(filepath.Join(wt, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(wt, "deep", "locked.txt")
	if err := os.WriteFile(locked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("chflags", "uchg", locked).Run(); err != nil {
		t.Skipf("immutable-flag blocker unavailable on this OS: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", locked).Run() })

	req := providerTestRequest(http.MethodDelete, "/api/v1/projects/sample", "admin", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleDeleteProject(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("delete must not succeed while a managed worktree survives")
	}
	if _, err := s.st.Project("sample"); err != nil {
		t.Fatalf("project record must survive failed worktree cleanup: %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive (records kept): %v", err)
	}
}

// A clean delete must still pass with worktrees present: normal worktrees are
// removed, the gate only trips on survivors.
func TestDeleteProjectSucceedsCleaningNormalWorktrees(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := os.MkdirAll(s.st.ProjectDir("sample"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	for _, id := range []string{"t-a", "t-b"} {
		if err := os.MkdirAll(filepath.Join(wsDir, ".multigent", "worktrees", id, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	req := providerTestRequest(http.MethodDelete, "/api/v1/projects/sample", "admin", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleDeleteProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(s.st.ProjectDir("sample")); !os.IsNotExist(err) {
		t.Fatalf("project dir survived: %v", err)
	}
}

// External-repo project: the project's Repo points outside the platform data
// root. Deletion removes only the platform-owned project dir and the managed
// worktrees; the external repository itself must be untouched.
func TestDeleteProjectExternalRepoNotTouched(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	external := filepath.Join(t.TempDir(), "external-repo")
	if err := os.MkdirAll(filepath.Join(external, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "CODE.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A managed worktree inside the external repo's .multigent/worktrees.
	wt := filepath.Join(external, ".multigent", "worktrees", "t-ext")
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample", Repo: external}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := os.MkdirAll(s.st.ProjectDir("sample"), 0o755); err != nil {
		t.Fatal(err)
	}

	req := providerTestRequest(http.MethodDelete, "/api/v1/projects/sample", "admin", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleDeleteProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	code, err := os.ReadFile(filepath.Join(external, "CODE.txt"))
	if err != nil {
		t.Fatalf("external repo damaged: %v", err)
	}
	if string(code) != "precious" {
		t.Fatalf("external repo content changed: %q", code)
	}
	if _, err := os.Stat(filepath.Join(external, ".git")); err != nil {
		t.Fatalf("external repo .git must survive: %v", err)
	}
	if _, err := os.Stat(s.st.ProjectDir("sample")); !os.IsNotExist(err) {
		t.Fatalf("platform project dir survived: %v", err)
	}
}
