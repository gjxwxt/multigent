package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/projecttemplate"
	workflowstore "github.com/multigent/multigent/internal/workflow"
)

func TestInitializeProjectTemplateMaterializesAndRecordsMetadata(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")
	repo := filepath.Join(t.TempDir(), "new-repo")
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/initialize-template", "admin", map[string]string{
		"repo":       repo,
		"templateId": projecttemplate.ReactGoFullstackID,
		"agent":      "pm",
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
	if _, err := os.Stat(filepath.Join(repo, "web", "src", "App.tsx")); err != nil {
		t.Fatalf("template repo was not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.st.AgentDir("sample", "pm"), "web", "src", "App.tsx")); err == nil {
		t.Fatalf("agent directory should not duplicate template files when repo is distinct: %v", err)
	}
	project, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if project.TemplateID != report.ID || project.TemplateVersion != report.Version || project.TemplateDigest != report.Digest {
		t.Fatalf("template metadata not recorded: %#v", project)
	}
}

func TestInitializeProjectTemplateReactSpringBoot(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("sample-sb", &entity.Project{Name: "sample-sb"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	seedAgentWorkerForTest(t, s, workspaceID, "sample-sb", "pm")
	repo := filepath.Join(t.TempDir(), "new-spring-repo")
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample-sb/initialize-template", "admin", map[string]string{
		"repo":       repo,
		"templateId": projecttemplate.ReactSpringBootID,
		"agent":      "pm",
	})
	req.SetPathValue("name", "sample-sb")
	rec := httptest.NewRecorder()
	s.handleInitializeProjectTemplate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report projecttemplate.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.ID != projecttemplate.ReactSpringBootID || report.Digest == "" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if _, err := os.Stat(filepath.Join(repo, ".multigent", "runtime.json")); err != nil {
		t.Fatalf("generated runtime contract missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "server", "build.gradle")); err != nil {
		t.Fatalf("server build.gradle missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "docs", "architecture.md")); err != nil {
		t.Fatalf("docs/architecture.md missing: %v", err)
	}
	project, err := s.st.Project("sample-sb")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if project.TemplateID != report.ID || project.TemplateVersion != report.Version || project.TemplateDigest != report.Digest {
		t.Fatalf("template metadata not recorded: %#v", project)
	}
}

func TestInitializeProjectTemplateKeepsRemoteURLIntact(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")
	remoteURL := "http://git.example.test:8083/root/sample.git"
	t.Cleanup(func() {
		_ = os.RemoveAll("http:")
	})
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/initialize-template", "admin", map[string]string{
		"repo":       remoteURL,
		"templateId": projecttemplate.ReactGoFullstackID,
	})
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleInitializeProjectTemplate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	project, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if project.Repo != remoteURL {
		t.Fatalf("remote URL mangled by path cleaning: %q", project.Repo)
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

func TestGetProjectInitializationReturnsLatestDurableRun(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")
	if err := workflowstore.NewStore(s.controlDB, workspaceID).EnsureProjectInitializationDefinition(); err != nil {
		t.Fatalf("ensure initialization workflow: %v", err)
	}

	now := time.Now().UTC()
	task := &entity.Task{
		ID:        "t-init-status",
		Title:     "Initialize sample",
		Prompt:    "initialization_request: mode=create_new",
		Type:      entity.TaskTypeChore,
		Priority:  3,
		Assignee:  "sample/pm",
		Status:    entity.TaskStatusInProgress,
		Labels:    []string{"project-initialization"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.ts.AddTask("sample", "pm", task); err != nil {
		t.Fatalf("add initialization task: %v", err)
	}
	wfStore := workflowstore.NewStore(s.controlDB, workspaceID)
	if _, _, err := wfStore.StartRun("sample", task.ID, workflowstore.ProjectInitializationWorkflowID, map[string]entity.WorkflowActorBinding{
		"project-initializer": {Type: "agent", ID: "pm"},
	}); err != nil {
		t.Fatalf("start initialization workflow: %v", err)
	}

	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/initialization", "admin", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleGetProjectInitialization(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got projectInitializationStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got.Status != "active" || got.Task == nil || got.Task.ID != task.ID {
		t.Fatalf("unexpected initialization status: %#v", got)
	}
}

func TestCreateProjectInitializationTaskSetsWorktreeDirToTargetRepo(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")

	customWorkspace := filepath.Join(t.TempDir(), "sample-workspace")
	body := map[string]any{
		"agent":       "pm",
		"title":       "Initialize Project",
		"prompt":      "initialization_request: mode=create_new",
		"type":        "chore",
		"labels":      []string{"project-initialization"},
		"vars": map[string]string{
			"initialization_repo": customWorkspace,
		},
	}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", body)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handlePostProjectTask(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	taskID, _ := created["id"].(string)
	if taskID == "" {
		t.Fatalf("no task id in response: %#v", created)
	}
	task, err := s.ts.GetTask("sample", "pm", taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.WorktreeDir != customWorkspace {
		t.Fatalf("expected task.WorktreeDir=%q, got %q", customWorkspace, task.WorktreeDir)
	}
	if _, err := os.Stat(customWorkspace); err != nil {
		t.Fatalf("target workspace directory was not created: %v", err)
	}
}
