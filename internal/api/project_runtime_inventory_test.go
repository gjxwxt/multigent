package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func setWorkerRuntimeConfig(t *testing.T, s *Server, workspaceID, workerID, cfg string) {
	t.Helper()
	w, ok, err := s.controlDB.AgentWorkerByID(workspaceID, workerID)
	if err != nil || !ok {
		t.Fatalf("load worker %s: ok=%v err=%v", workerID, ok, err)
	}
	w.RuntimeConfigJSON = cfg
	if err := s.controlDB.UpsertAgentWorker(w); err != nil {
		t.Fatalf("update worker %s: %v", workerID, err)
	}
}

type inventoryWorker struct {
	Worker string `json:"worker"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

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

// A worker with a pinned image does not excuse an undeclared project:
// the pin is that worker's own choice, the project-level declaration is
// still missing, so the row stays up for review.
func TestProjectRuntimeInventoryUndeclaredWithPinnedImageStillReview(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("pinned", &entity.Project{Name: "pinned"}); err != nil {
		t.Fatalf("seed pinned: %v", err)
	}
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "pinned", "frozen", "aw-frozen", "pm-pinned-frozen")
	setWorkerRuntimeConfig(t, s, workspaceID, "aw-frozen", `{"sandbox":{"provider":"docker","image":"registry.example/team/jdk-stack:21"}}`)

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
			if row["suggestedAction"] != "review" {
				t.Fatalf("undeclared project with pinned worker must still suggest review, got %v", row["suggestedAction"])
			}
			return
		}
	}
	t.Fatalf("pinned missing from inventory: %s", rec.Body.String())
}

// Projects with several members report every member's runtime observation;
// disagreement surfaces as mixed instead of an arbitrary single source.
// Output is stably sorted by worker name.
func TestProjectRuntimeInventoryMultiWorkerObservationsAndMixed(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("mixed", &entity.Project{Name: "mixed"}); err != nil {
		t.Fatalf("seed mixed: %v", err)
	}
	if err := s.st.SaveProject("agree", &entity.Project{Name: "agree", RuntimeProfile: "jvm21"}); err != nil {
		t.Fatalf("seed agree: %v", err)
	}
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "mixed", "wB", "aw-mb", "pm-mixed-b")
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "mixed", "wA", "aw-ma", "pm-mixed-a")
	setWorkerRuntimeConfig(t, s, workspaceID, "aw-ma", `{"sandbox":{"provider":"docker","docker":{"profile":"jvm21"}}}`)
	setWorkerRuntimeConfig(t, s, workspaceID, "aw-mb", `{"sandbox":{"provider":"docker","image":"registry.example/team/base:9"}}`)
	seedAgentWorkerWithIDForTest(t, s, workspaceID, "agree", "wC", "aw-wc", "pm-agree-c")
	setWorkerRuntimeConfig(t, s, workspaceID, "aw-wc", `{"sandbox":{"provider":"docker","docker":{"profile":"jvm21"}}}`)

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/runtime-inventory", "admin", nil)
	s.handleProjectRuntimeInventory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		Project         string            `json:"project"`
		EffectiveSource string            `json:"effectiveSource"`
		EffectiveValue  string            `json:"effectiveValue"`
		Workers         []inventoryWorker `json:"workers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var mixed, agree *struct {
		Project         string            `json:"project"`
		EffectiveSource string            `json:"effectiveSource"`
		EffectiveValue  string            `json:"effectiveValue"`
		Workers         []inventoryWorker `json:"workers"`
	}
	for i := range rows {
		switch rows[i].Project {
		case "mixed":
			mixed = &rows[i]
		case "agree":
			agree = &rows[i]
		}
	}
	if mixed == nil || agree == nil {
		t.Fatalf("expected both projects in inventory: %s", rec.Body.String())
	}
	if mixed.EffectiveSource != "mixed" {
		t.Fatalf("disagreeing workers must yield mixed, got %q (workers=%+v)", mixed.EffectiveSource, mixed.Workers)
	}
	if len(mixed.Workers) != 2 {
		t.Fatalf("mixed project must report both workers, got %d", len(mixed.Workers))
	}
	if !sort.SliceIsSorted(mixed.Workers, func(i, j int) bool { return mixed.Workers[i].Worker < mixed.Workers[j].Worker }) {
		t.Fatalf("worker observations must be sorted by worker name: %+v", mixed.Workers)
	}
	if mixed.Workers[0].Worker != "wA" || mixed.Workers[1].Worker != "wB" {
		t.Fatalf("unexpected worker order: %+v", mixed.Workers)
	}
	if agree.EffectiveSource != "project" {
		t.Fatalf("declared project stays authoritative, got %q", agree.EffectiveSource)
	}
	if len(agree.Workers) != 1 {
		t.Fatalf("agree project must report its one worker, got %d", len(agree.Workers))
	}
}

// A project with no worker members reports the server default, not an
// arbitrary observation attributed to nobody.
func TestProjectRuntimeInventoryNoWorkers(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("empty", &entity.Project{Name: "empty"}); err != nil {
		t.Fatalf("seed empty: %v", err)
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
		if row["project"] == "empty" {
			if row["effectiveSource"] != "server_default" {
				t.Fatalf("workerless effective source = %v, want server_default", row["effectiveSource"])
			}
			if row["suggestedAction"] != "review" {
				t.Fatalf("workerless undeclared project must suggest review, got %v", row["suggestedAction"])
			}
			return
		}
	}
	t.Fatalf("empty missing from inventory: %s", rec.Body.String())
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
