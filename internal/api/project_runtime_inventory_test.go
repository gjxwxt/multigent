package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func TestProjectRuntimeInventoryListsUndeclaredAsUnknown(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("legacy", &entity.Project{Name: "legacy"}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := s.st.SaveProject("jvmproj", &entity.Project{Name: "jvmproj", RuntimeProfile: "jvm21"}); err != nil {
		t.Fatalf("seed jvmproj: %v", err)
	}
	// A worker with an agent-level profile preference joined to the legacy
	// project: the inventory must surface it as the current effective source
	// without pretending the project declared anything.
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "legacy", "picky", "aw-picky", "pm-legacy-picky")
	picky, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-picky")
	if err != nil || !ok {
		t.Fatalf("load worker: ok=%v err=%v", ok, err)
	}
	picky.RuntimeConfigJSON = `{"sandbox":{"provider":"docker","docker":{"profile":"jvm21"}}}`
	if err := s.controlDB.UpsertAgentWorker(picky); err != nil {
		t.Fatalf("update worker: %v", err)
	}

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/runtime-inventory", "admin", nil)
	s.handleProjectRuntimeInventory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		Project         string `json:"project"`
		DeclaredProfile string `json:"declaredProfile"`
		EffectiveSource string `json:"effectiveSource"`
		EffectiveValue  string `json:"effectiveValue"`
		Action          string `json:"suggestedAction"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	byName := map[string]*struct {
		Project         string `json:"project"`
		DeclaredProfile string `json:"declaredProfile"`
		EffectiveSource string `json:"effectiveSource"`
		EffectiveValue  string `json:"effectiveValue"`
		Action          string `json:"suggestedAction"`
	}{}
	for i := range rows {
		byName[rows[i].Project] = &rows[i]
	}

	jvm := byName["jvmproj"]
	if jvm == nil {
		t.Fatalf("jvmproj missing from inventory: %s", rec.Body.String())
	}
	if jvm.DeclaredProfile != "jvm21" || jvm.EffectiveSource != "project" || jvm.EffectiveValue != "jvm21" {
		t.Fatalf("jvmproj row wrong: %+v", *jvm)
	}
	if jvm.Action != "none" {
		t.Fatalf("declared project should need no action, got %q", jvm.Action)
	}

	legacy := byName["legacy"]
	if legacy == nil {
		t.Fatalf("legacy missing from inventory: %s", rec.Body.String())
	}
	if legacy.DeclaredProfile != "" {
		t.Fatalf("legacy must stay undeclared, got %q", legacy.DeclaredProfile)
	}
	if legacy.EffectiveSource != "agent" || legacy.EffectiveValue != "jvm21" {
		t.Fatalf("legacy effective runtime must come from agent preference, got source=%q value=%q", legacy.EffectiveSource, legacy.EffectiveValue)
	}
	if legacy.Action != "review" {
		t.Fatalf("undeclared project must suggest review, got %q", legacy.Action)
	}
}

func TestProjectRuntimeInventoryAgentWithoutPreferenceIsServerDefault(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("plain", &entity.Project{Name: "plain"}); err != nil {
		t.Fatalf("seed plain: %v", err)
	}
	seedAgentWorkerForTest(t, s, workspaceID, "plain", "dev")

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/runtime-inventory", "admin", nil)
	s.handleProjectRuntimeInventory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, row := range rows {
		if row["project"] == "plain" {
			if row["effectiveSource"] != "server_default" {
				t.Fatalf("plain effective source = %v, want server_default", row["effectiveSource"])
			}
			if row["suggestedAction"] != "review" {
				t.Fatalf("undeclared project must suggest review, got %v", row["suggestedAction"])
			}
			return
		}
	}
	t.Fatalf("plain missing from inventory: %s", rec.Body.String())
}

func TestProjectRuntimeInventoryAgentExplicitImageMarkedPinned(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("pinned", &entity.Project{Name: "pinned"}); err != nil {
		t.Fatalf("seed pinned: %v", err)
	}
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "pinned", "frozen", "aw-frozen", "pm-pinned-frozen")
	frozen, ok, err := s.controlDB.AgentWorkerByID(workspaceID, "aw-frozen")
	if err != nil || !ok {
		t.Fatalf("load worker: ok=%v err=%v", ok, err)
	}
	frozen.RuntimeConfigJSON = `{"sandbox":{"provider":"docker","image":"registry.example/team/jdk-stack:21"}}`
	if err := s.controlDB.UpsertAgentWorker(frozen); err != nil {
		t.Fatalf("update worker: %v", err)
	}

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/runtime-inventory", "admin", nil)
	s.handleProjectRuntimeInventory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, row := range rows {
		if row["project"] == "pinned" {
			if row["effectiveSource"] != "agent_image" {
				t.Fatalf("pinned effective source = %v, want agent_image", row["effectiveSource"])
			}
			// A pinned image wins anyway: backfill suggestion must not claim
			// the profile would change runtime behavior for this worker.
			if row["suggestedAction"] != "none" {
				t.Fatalf("pinned-image project should suggest none, got %v", row["suggestedAction"])
			}
			return
		}
	}
	t.Fatalf("pinned missing from inventory: %s", rec.Body.String())
}

func TestPutProjectProfileChangeWritesAuditEvent(t *testing.T) {
	s, workspaceID := newRuntimeProfileServer(t)
	_ = workspaceID

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPut, "/api/v1/projects/legacy", "admin", map[string]any{
		"runtimeProfile": "jvm21",
	})
	req.SetPathValue("name", "legacy")
	s.handlePutProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put legacy status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A no-op update (same value) must not emit a second audit event.
	rec2 := httptest.NewRecorder()
	req2 := providerTestRequest(http.MethodPut, "/api/v1/projects/legacy", "admin", map[string]any{
		"runtimeProfile": "jvm21",
	})
	req2.SetPathValue("name", "legacy")
	s.handlePutProject(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("put same value status=%d body=%s", rec2.Code, rec2.Body.String())
	}

	events, err := s.controlDB.ListAuditEvents(controldb.AuditEventFilter{
		WorkspaceID:  workspaceID,
		ResourceType: "project",
		ResourceID:   "legacy",
	})
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 audit event for one real change, got %d", len(events))
	}
	if events[0].Action != "project.runtime_profile.update" {
		t.Fatalf("audit action = %q", events[0].Action)
	}
	var after struct {
		RuntimeProfile string `json:"runtimeProfile"`
	}
	if err := json.Unmarshal([]byte(events[0].AfterJSON), &after); err != nil {
		t.Fatalf("decode AfterJSON %q: %v", events[0].AfterJSON, err)
	}
	if after.RuntimeProfile != "jvm21" {
		t.Fatalf("audit after.runtimeProfile = %q, want jvm21", after.RuntimeProfile)
	}
}
