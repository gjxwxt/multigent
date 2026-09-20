package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// The replaced design decided "does this project have a GitLab remote?" from the
// project record's remoteProvider / remoteConnection. Those are client-writable
// and prove nothing (see the P0.6 note in ci_ready_handlers.go), so a project can
// claim a remote it does not have and the console card would then disagree with
// the CI gate that blocks on the verified binding. handleProject must answer from
// the binding table and keep echoing the display fields only for the settings UI.
func TestProjectDetailReportsVerifiedBindingIndependentlyOfDisplayFields(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	if err := s.st.SaveProject("lied-project", &entity.Project{
		Name:             "lied-project",
		RemoteProvider:   "gitlab",
		RemoteConnection: "conn-attacker",
		RemoteProjectID:  "999",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	get := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/lied-project", nil)
		req.SetPathValue("name", "lied-project")
		req = req.WithContext(withTestUser(req.Context(), "admin"))
		rec := httptest.NewRecorder()
		s.handleProject(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	unbound := get()
	if unbound["remoteProvider"] != "gitlab" {
		t.Fatal("display field must still be echoed so the settings form can render it")
	}
	if verified, ok := unbound["remoteBindingVerified"]; !ok || verified != false {
		t.Fatalf("display fields alone must never read as verified, got %#v (present=%v)", unbound["remoteBindingVerified"], ok)
	}

	if err := s.controlDB.UpsertVerifiedRemoteBinding(controldb.VerifiedRemoteBinding{
		WorkspaceID:       workspaceID,
		ProjectID:         "lied-project",
		Provider:          "gitlab",
		ConnectionID:      "conn-gitlab",
		RemoteProjectID:   "42",
		PathWithNamespace: "gao/react-components-ci",
		VerifiedAt:        "2026-09-20T00:00:00Z",
		Source:            controldb.BindingSourceExplicitVerify,
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	bound := get()
	if bound["remoteBindingVerified"] != true {
		t.Fatalf("verified binding must read true, got %#v", bound["remoteBindingVerified"])
	}
	// The namespace, not an id, is what a human recognizes on a remote screen.
	if bound["remoteBindingNamespace"] != "gao/react-components-ci" {
		t.Fatalf("remoteBindingNamespace = %#v, want the bound path", bound["remoteBindingNamespace"])
	}
}
