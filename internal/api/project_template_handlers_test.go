package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/projecttemplate"
)

func TestInitializeProjectTemplateMaterializesAndRecordsMetadata(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	repo := filepath.Join(t.TempDir(), "new-repo")
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/initialize-template", "admin", map[string]string{
		"repo":       repo,
		"templateId": projecttemplate.ReactGoFullstackID,
	})
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleInitializeProjectTemplate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report projecttemplate.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.ID != projecttemplate.ReactGoFullstackID || report.Digest == "" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if _, err := os.Stat(filepath.Join(repo, ".multigent", "runtime.json")); err != nil {
		t.Fatalf("generated runtime contract missing: %v", err)
	}
	project, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if project.TemplateID != report.ID || project.TemplateVersion != report.Version || project.TemplateDigest != report.Digest {
		t.Fatalf("template metadata not recorded: %#v", project)
	}
}

func TestInitializeProjectTemplateDoesNotOverwriteExistingRepository(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	repo := filepath.Join(t.TempDir(), "existing-repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "keep.txt"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = s.st.SaveProject("sample", &entity.Project{Name: "sample"})
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/initialize-template", "admin", map[string]string{
		"repo":       repo,
		"templateId": projecttemplate.ReactGoFullstackID,
	})
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleInitializeProjectTemplate(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "keep.txt")); err != nil {
		t.Fatalf("existing file should remain: %v", err)
	}
}
