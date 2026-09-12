package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/projecttemplate"
	"github.com/multigent/multigent/internal/sandbox"
)

// TestPutAgentSandboxRejectsUnknownProfile proves the fail-closed contract at
// the API boundary: an unknown runtime profile is a 400, never a silent
// degradation to the base image.
func TestPutAgentSandboxRejectsUnknownProfile(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")

	req := providerTestRequest(http.MethodPut, "/api/v1/projects/sample/agents/pm/sandbox", "admin", map[string]any{
		"provider": "docker",
		"image":    "",
		"profile":  "jvm",
	})
	req.SetPathValue("name", "sample")
	req.SetPathValue("agent", "pm")
	rec := httptest.NewRecorder()
	s.handlePutAgentSandbox(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile should be 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown runtime profile") {
		t.Fatalf("error should name the invalid profile, got %s", rec.Body.String())
	}

	// The worker's persisted config must remain untouched by the rejected write.
	resolved, found, err := s.agentDirectory.ProjectWorker(workspaceID, "sample", "pm")
	if err != nil || !found {
		t.Fatalf("load worker: found=%v err=%v", found, err)
	}
	cfg := decodeAgentWorkerRuntimeConfig(resolved.Worker)
	if cfg.Sandbox != nil && cfg.Sandbox.Docker != nil && cfg.Sandbox.Docker.Profile != "" {
		t.Fatalf("rejected write must not persist a profile, got %q", cfg.Sandbox.Docker.Profile)
	}
}

// TestPutAgentWorkerSandboxRejectsUnknownProfile covers the workspace-level
// agent worker sandbox endpoint with the same fail-closed rule.
func TestPutAgentWorkerSandboxRejectsUnknownProfile(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")
	resolved, found, err := s.agentDirectory.ProjectWorker(workspaceID, "sample", "pm")
	if err != nil || !found {
		t.Fatalf("load worker: found=%v err=%v", found, err)
	}
	workerID := resolved.Worker.ID

	req := providerTestRequest(http.MethodPut, "/api/v1/agents/"+workerID+"/sandbox", "admin", map[string]any{
		"provider": "docker",
		"profile":  "node22",
	})
	req.SetPathValue("id", workerID)
	rec := httptest.NewRecorder()
	s.handlePutAgentWorkerSandbox(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile should be 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestTemplateProfileWritesProjectNotWorker is the cross-project pollution
// regression: initializing the Spring Boot template must declare jvm21 on the
// PROJECT (per-project authority) and leave the shared agent worker's sandbox
// config untouched, so the same worker joined into a base project keeps
// resolving the base image.
func TestTemplateProfileWritesProjectNotWorker(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("sample-sb", &entity.Project{Name: "sample-sb"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	seedAgentWorkerForTest(t, s, workspaceID, "sample-sb", "pm")
	repo := t.TempDir()
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

	project, err := s.st.Project("sample-sb")
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if project.RuntimeProfile != sandbox.ProfileJVM21 {
		t.Fatalf("spring boot template should declare project-level jvm21, got %q", project.RuntimeProfile)
	}

	resolved, found, err := s.agentDirectory.ProjectWorker(workspaceID, "sample-sb", "pm")
	if err != nil || !found {
		t.Fatalf("load worker: found=%v err=%v", found, err)
	}
	cfg := decodeAgentWorkerRuntimeConfig(resolved.Worker)
	if cfg.Sandbox != nil && cfg.Sandbox.Docker != nil && cfg.Sandbox.Docker.Profile != "" {
		t.Fatalf("shared worker must not receive the template profile (cross-project pollution), got %q", cfg.Sandbox.Docker.Profile)
	}
}

// TestResolveProjectRuntimePerProject proves the preview runtime resolution is
// per project: a jvm21 project resolves a jvm21-family image while a base
// project in the same workspace resolves the base image — with the identical
// server environment for both.
func TestResolveProjectRuntimePerProject(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	t.Setenv(sandbox.EnvRuntimeProfile, "")
	t.Setenv(sandbox.EnvRuntimeImage, "")

	if err := s.st.SaveProject("base-proj", &entity.Project{Name: "base-proj"}); err != nil {
		t.Fatalf("save base project: %v", err)
	}
	if err := s.st.SaveProject("jvm-proj", &entity.Project{Name: "jvm-proj", RuntimeProfile: sandbox.ProfileJVM21}); err != nil {
		t.Fatalf("save jvm project: %v", err)
	}

	baseSel, err := s.resolveTaskPreviewRuntime("base-proj", "no-such-task")
	if err != nil {
		t.Fatalf("resolve base project: %v", err)
	}
	if baseSel.Profile != sandbox.ProfileBase || strings.Contains(baseSel.ImageRef, "runtime-jvm21") {
		t.Fatalf("base project should resolve base runtime, got %+v", baseSel)
	}

	jvmSel, err := s.resolveTaskPreviewRuntime("jvm-proj", "no-such-task")
	if err != nil {
		t.Fatalf("resolve jvm project: %v", err)
	}
	if jvmSel.Profile != sandbox.ProfileJVM21 || !strings.Contains(jvmSel.ImageRef, "runtime-jvm21") {
		t.Fatalf("jvm21 project should resolve a jvm21-family image, got %+v", jvmSel)
	}
}

// TestResolveTaskPreviewRuntimeFollowsTaskAssignee exercises the real preview
// resolution path for a persisted task: task.Assignee →
// taskExecutingAgentRuntime → resolveTaskPreviewRuntime. Unlike
// TestResolveProjectRuntimePerProject (which resolves on project defaults via
// a nonexistent task), these cases seed a real task record plus its executing
// agent worker, so the assignee's sandbox preferences actually participate in
// the decision.
func TestResolveTaskPreviewRuntimeFollowsTaskAssignee(t *testing.T) {
	t.Setenv(sandbox.EnvRuntimeProfile, "")
	t.Setenv(sandbox.EnvRuntimeImage, "")

	// seedAssignedTask persists the project (with its declared runtime
	// profile), an agent worker with the requested sandbox RuntimeConfigJSON,
	// and a task assigned to that worker.
	seedAssignedTask := func(t *testing.T, s *Server, workspaceID, project, agent, taskID, projectProfile, sandboxJSON string) {
		t.Helper()
		if err := s.st.SaveProject(project, &entity.Project{Name: project, RuntimeProfile: projectProfile}); err != nil {
			t.Fatalf("save project %s: %v", project, err)
		}
		seedAgentWorkerForTest(t, s, workspaceID, project, agent)
		if sandboxJSON != "" {
			resolved, found, err := s.agentDirectory.ProjectWorker(workspaceID, project, agent)
			if err != nil || !found {
				t.Fatalf("load worker: found=%v err=%v", found, err)
			}
			worker := resolved.Worker
			worker.RuntimeConfigJSON = sandboxJSON
			if err := s.controlDB.UpsertAgentWorker(worker); err != nil {
				t.Fatalf("apply worker sandbox config: %v", err)
			}
		}
		task := &entity.Task{
			ID:           taskID,
			Title:        "runtime resolution",
			Assignee:     project + "/" + agent,
			AssigneeType: "agent_worker",
			Status:       entity.TaskStatusDoneSuccess,
			Prompt:       "resolve runtime",
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		if err := s.ts.AddTask(project, agent, task); err != nil {
			t.Fatalf("add task: %v", err)
		}
	}

	t.Run("base project honors agent jvm21 preference", func(t *testing.T) {
		s, workspaceID := newConnectionGrantPolicyServer(t)
		seedAssignedTask(t, s, workspaceID, "pref-base", "dev", "t-pref-1", "",
			`{"sandbox":{"provider":"docker","docker":{"profile":"jvm21"}}}`)
		sel, err := s.resolveTaskPreviewRuntime("pref-base", "t-pref-1")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if sel.Profile != sandbox.ProfileJVM21 || !strings.Contains(sel.ImageRef, "runtime-jvm21") {
			t.Fatalf("agent jvm21 preference should apply in a project that declares no profile, got %+v", sel)
		}
	})

	t.Run("jvm21 project overrides agent base preference", func(t *testing.T) {
		s, workspaceID := newConnectionGrantPolicyServer(t)
		seedAssignedTask(t, s, workspaceID, "auth-jvm", "dev", "t-auth-1", sandbox.ProfileJVM21,
			`{"sandbox":{"provider":"docker","docker":{"profile":"base"}}}`)
		sel, err := s.resolveTaskPreviewRuntime("auth-jvm", "t-auth-1")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if sel.Profile != sandbox.ProfileJVM21 || !strings.Contains(sel.ImageRef, "runtime-jvm21") {
			t.Fatalf("project jvm21 must override the agent's base preference, got %+v", sel)
		}
	})

	t.Run("explicit agent image pins preview to the same image", func(t *testing.T) {
		s, workspaceID := newConnectionGrantPolicyServer(t)
		const pinned = "harbor.example.com/platform/jdk-stack:21"
		seedAssignedTask(t, s, workspaceID, "img-jvm", "dev", "t-img-1", sandbox.ProfileJVM21,
			`{"sandbox":{"provider":"docker","image":"`+pinned+`"}}`)
		sel, err := s.resolveTaskPreviewRuntime("img-jvm", "t-img-1")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if sel.ImageRef != pinned {
			t.Fatalf("preview must run the agent's explicitly pinned image, got %q", sel.ImageRef)
		}
		if sel.Profile != sandbox.ProfileJVM21 {
			t.Fatalf("project profile should be recorded in the selection, got %q", sel.Profile)
		}
		// The engine renders JAVA_TOOL_OPTIONS from this recorded profile
		// (asserted in internal/preview: TestProfilePreviewEnvJTOFromSelection);
		// here the contract is that the pinned image does NOT erase the
		// project's declared profile from the selection.
	})
}

// The per-project preview image/env rendering contract lives in
// internal/preview/engine_test.go (TestPreviewImageResolution,
// TestProfilePreviewEnv); the api package asserts its side of the contract —
// that a jvm21 project resolves a jvm21-family runtime for preview start.

// TestAgentWorkerSandboxBodyRoundTrip keeps the JSON contract documented: a
// valid profile persists on the worker's docker sandbox config.
func TestAgentWorkerSandboxProfileRoundTrip(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	seedAgentWorkerForTest(t, s, workspaceID, "sample", "pm")
	resolved, found, err := s.agentDirectory.ProjectWorker(workspaceID, "sample", "pm")
	if err != nil || !found {
		t.Fatalf("load worker: found=%v err=%v", found, err)
	}
	workerID := resolved.Worker.ID

	req := providerTestRequest(http.MethodPut, "/api/v1/agents/"+workerID+"/sandbox", "admin", map[string]any{
		"provider": "docker",
		"profile":  "JVM21", // case-insensitive normalization
	})
	req.SetPathValue("id", workerID)
	rec := httptest.NewRecorder()
	s.handlePutAgentWorkerSandbox(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid profile should be accepted, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated, found, err := s.agentDirectory.ProjectWorker(workspaceID, "sample", "pm")
	if err != nil || !found {
		t.Fatalf("reload worker: found=%v err=%v", found, err)
	}
	cfg := decodeAgentWorkerRuntimeConfig(updated.Worker)
	if cfg.Sandbox == nil || cfg.Sandbox.Docker == nil || cfg.Sandbox.Docker.Profile != "JVM21" {
		t.Fatalf("profile should persist on the worker docker config, got %+v", cfg.Sandbox)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(updated.Worker.RuntimeConfigJSON), &raw); err != nil {
		t.Fatalf("decode runtime config: %v", err)
	}
}
