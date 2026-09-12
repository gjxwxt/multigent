package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

func TestDeleteRoleTeamAndProjectRequireWorkspaceAdmin(t *testing.T) {
	s, workspaceID := newConnectionGrantPolicyServer(t)
	grantProjectRoleForTest(t, s, workspaceID, "member", ProjectRoleManager)
	if err := s.st.SaveTeam("engineering", &entity.Team{Name: "engineering"}); err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := s.st.SaveRole("engineering", "backend", &entity.Role{Name: "backend"}); err != nil {
		t.Fatalf("role: %v", err)
	}

	memberRoleRec := httptest.NewRecorder()
	memberRoleReq := providerTestRequest(http.MethodDelete, "/api/v1/teams/engineering/roles/backend", "member", nil)
	memberRoleReq.SetPathValue("team", "engineering")
	memberRoleReq.SetPathValue("role", "backend")
	s.handleDeleteRole(memberRoleRec, memberRoleReq)
	if memberRoleRec.Code != http.StatusForbidden {
		t.Fatalf("member delete role status=%d body=%s", memberRoleRec.Code, memberRoleRec.Body.String())
	}

	adminRoleRec := httptest.NewRecorder()
	adminRoleReq := providerTestRequest(http.MethodDelete, "/api/v1/teams/engineering/roles/backend", "admin", nil)
	adminRoleReq.SetPathValue("team", "engineering")
	adminRoleReq.SetPathValue("role", "backend")
	s.handleDeleteRole(adminRoleRec, adminRoleReq)
	if adminRoleRec.Code != http.StatusOK {
		t.Fatalf("admin delete role status=%d body=%s", adminRoleRec.Code, adminRoleRec.Body.String())
	}
	if _, err := s.st.Role("engineering", "backend"); err == nil {
		t.Fatalf("role still exists")
	}

	memberTeamRec := httptest.NewRecorder()
	memberTeamReq := providerTestRequest(http.MethodDelete, "/api/v1/teams/engineering", "member", nil)
	memberTeamReq.SetPathValue("teamPath", "engineering")
	s.handleDeleteTeam(memberTeamRec, memberTeamReq)
	if memberTeamRec.Code != http.StatusForbidden {
		t.Fatalf("member delete team status=%d body=%s", memberTeamRec.Code, memberTeamRec.Body.String())
	}

	adminTeamRec := httptest.NewRecorder()
	adminTeamReq := providerTestRequest(http.MethodDelete, "/api/v1/teams/engineering", "admin", nil)
	adminTeamReq.SetPathValue("teamPath", "engineering")
	s.handleDeleteTeam(adminTeamRec, adminTeamReq)
	if adminTeamRec.Code != http.StatusOK {
		t.Fatalf("admin delete team status=%d body=%s", adminTeamRec.Code, adminTeamRec.Body.String())
	}
	if _, err := s.st.Team("engineering"); err == nil {
		t.Fatalf("team still exists")
	}

	conn := controldb.Connection{
		ID:             "conn-sample-1",
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: "Test MM Conn",
		Status:         "active",
		CreatedBy:      "admin",
	}
	if err := s.controlDB.UpsertConnection(conn); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	if err := s.controlDB.UpsertProjectChannelLink(controldb.ProjectChannelLink{
		ID:          "link-sample-1",
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
		Provider:    "mattermost",
		ChannelID:   "chan-sample-1",
		ChannelName: "sample-channel",
	}); err != nil {
		t.Fatalf("upsert project channel link: %v", err)
	}
	if err := s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "bind-sample-1",
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		AgentID:      "agent-sample-1",
		ConnectionID: "conn-sample-1",
		Provider:     "mattermost",
		Status:       "connected",
	}); err != nil {
		t.Fatalf("upsert agent channel binding: %v", err)
	}

	memberProjectRec := httptest.NewRecorder()
	memberProjectReq := providerTestRequest(http.MethodDelete, "/api/v1/projects/sample", "member", nil)
	memberProjectReq.SetPathValue("name", "sample")
	s.handleDeleteProject(memberProjectRec, memberProjectReq)
	if memberProjectRec.Code != http.StatusForbidden {
		t.Fatalf("member delete project status=%d body=%s", memberProjectRec.Code, memberProjectRec.Body.String())
	}

	adminProjectRec := httptest.NewRecorder()
	adminProjectReq := providerTestRequest(http.MethodDelete, "/api/v1/projects/sample", "admin", nil)
	adminProjectReq.SetPathValue("name", "sample")
	s.handleDeleteProject(adminProjectRec, adminProjectReq)
	if adminProjectRec.Code != http.StatusOK {
		t.Fatalf("admin delete project status=%d body=%s", adminProjectRec.Code, adminProjectRec.Body.String())
	}
	if _, err := s.st.Project("sample"); err == nil {
		t.Fatalf("project still exists")
	}
	memberships, err := s.controlDB.ListProjectMemberships(controldb.ProjectMembershipFilter{
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
	})
	if err != nil {
		t.Fatalf("project memberships after delete: %v", err)
	}
	if len(memberships) != 0 {
		t.Fatalf("project memberships after delete len=%d", len(memberships))
	}
	links, err := s.controlDB.ListProjectChannelLinks(workspaceID, "sample")
	if err != nil {
		t.Fatalf("project channel links after delete: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("project channel links after delete len=%d", len(links))
	}
	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
	})
	if err != nil {
		t.Fatalf("agent channel bindings after delete: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("agent channel bindings after delete len=%d", len(bindings))
	}
}

// Regression (P1, review 2026-09-14): a successful DELETE must leave no
// project directory behind. The p16 soak found worktree dirs (~200MB) plus
// historical orphans surviving a 200 response because RemoveAll failures were
// swallowed.
func TestDeleteProjectRemovesPhysicalDirectory(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	// Seed a git workspace with an orphan worktree whose .git is a real
	// directory (independent-clone semantics — invisible to git worktree
	// prune) plus a regular file outside worktrees.
	wsDir := filepath.Join(s.st.ProjectDir("sample"), "workspace")
	if err := os.MkdirAll(filepath.Join(wsDir, ".multigent", "worktrees", "t-orphan", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := providerTestRequest(http.MethodDelete, "/api/v1/projects/sample", "admin", nil)
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleDeleteProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(s.st.ProjectDir("sample")); !os.IsNotExist(err) {
		t.Fatalf("project directory survived delete: %v", err)
	}
	if _, err := s.st.Project("sample"); err == nil {
		t.Fatalf("project record still exists")
	}
}

// A deletion whose physical teardown fails must keep the project record —
// verbatim, every field — and report the failure. destroyProjectArtifacts is
// purely physical, so a failed teardown must not have touched the DB at all:
// no record drop, no empty-shell restore. The production failure mode is
// root-owned files left by sandbox containers; in-process we simulate with
// the macOS immutable flag (the only portable-enough unlink blocker that also
// defeats the chmod walk). On Linux CI this skips — the gate itself is
// exercised on the VM acceptance pass.
func TestDeleteProjectKeepsRecordsWhenTeardownFails(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	seeded := &entity.Project{
		Name:             "sample",
		Description:      "field preservation probe",
		Repo:             "/tmp/sample-repo",
		RemoteProvider:   "gitlab",
		RemoteConnection: "conn-gitlab",
		RemoteProjectID:  "58",
		RemoteURL:        "https://gitlab.example/gao/sample",
		CloneURL:         "https://gitlab.example/gao/sample.git",
		DefaultBranch:    "main",
		RuntimeProfile:   "jvm21",
		DeployPort:       30123,
	}
	if err := s.st.SaveProject("sample", seeded); err != nil {
		t.Fatalf("reseed project: %v", err)
	}
	// dbStore.SaveProject persists to the control DB only; the physical dir
	// appears when files are written into it, so create it here.
	if err := os.MkdirAll(s.st.ProjectDir("sample"), 0o755); err != nil {
		t.Fatal(err)
	}

	locked := filepath.Join(s.st.ProjectDir("sample"), "locked.txt")
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
		t.Fatalf("delete must not succeed while the physical directory survives")
	}
	p, err := s.st.Project("sample")
	if err != nil {
		t.Fatalf("project record must survive failed teardown: %v", err)
	}
	if p.Description != seeded.Description || p.Repo != seeded.Repo ||
		p.RemoteProvider != seeded.RemoteProvider || p.RemoteConnection != seeded.RemoteConnection ||
		p.RemoteProjectID != seeded.RemoteProjectID || p.RemoteURL != seeded.RemoteURL ||
		p.CloneURL != seeded.CloneURL || p.DefaultBranch != seeded.DefaultBranch ||
		p.RuntimeProfile != seeded.RuntimeProfile || p.DeployPort != seeded.DeployPort {
		t.Fatalf("failed teardown must preserve the project record verbatim, got %+v", p)
	}
}

// The orphan scan lists on-disk project directories that no longer have a
// record, and the delete endpoint removes exactly the named orphan while
// refusing anything that still has a record.
func TestProjectOrphansScanAndDelete(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	// Orphan: directory without a project record (pre-gate failed delete).
	orphanDir := filepath.Join(s.st.ProjectDir("."), "legacy-orphan")
	if err := os.MkdirAll(filepath.Join(orphanDir, "workspace", ".multigent", "worktrees", "t-old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Live project stays invisible to the scan.
	liveDir := filepath.Join(s.st.ProjectDir("."), "sample")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	scanRec := httptest.NewRecorder()
	scanReq := providerTestRequest(http.MethodGet, "/api/v1/projects/orphans", "admin", nil)
	s.handleProjectOrphans(scanRec, scanReq)
	if scanRec.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", scanRec.Code, scanRec.Body.String())
	}
	var scanResp struct {
		Orphans []struct {
			Name string `json:"name"`
		} `json:"orphans"`
	}
	if err := json.NewDecoder(scanRec.Body).Decode(&scanResp); err != nil {
		t.Fatalf("decode scan: %v", err)
	}
	var found bool
	for _, o := range scanResp.Orphans {
		if o.Name == "legacy-orphan" {
			found = true
		}
		if o.Name == "sample" {
			t.Fatalf("live project must not appear as orphan")
		}
	}
	if !found {
		t.Fatalf("orphan dir not reported: %s", scanRec.Body.String())
	}

	// Deleting a LIVE project through the orphan endpoint is refused.
	liveReq := providerTestRequest(http.MethodDelete, "/api/v1/projects/orphans/sample", "admin", nil)
	liveReq.SetPathValue("name", "sample")
	liveRec := httptest.NewRecorder()
	s.handleProjectOrphansDelete(liveRec, liveReq)
	if liveRec.Code != http.StatusConflict {
		t.Fatalf("orphan delete on live project must 409, got %d", liveRec.Code)
	}

	// Deleting the orphan succeeds and removes the tree.
	delReq := providerTestRequest(http.MethodDelete, "/api/v1/projects/orphans/legacy-orphan", "admin", nil)
	delReq.SetPathValue("name", "legacy-orphan")
	delRec := httptest.NewRecorder()
	s.handleProjectOrphansDelete(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("orphan delete status=%d body=%s", delRec.Code, delRec.Body.String())
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("orphan dir survived: %v", err)
	}
}

// Orphan endpoint hardening: names are validated (no traversal, no absolute
// paths), the scan never leaks absolute host paths, and malformed names are
// rejected before any filesystem work.
func TestProjectOrphansRejectsUnsafeNamesAndPaths(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	for _, name := range []string{"..", "a/b", ".hidden"} {
		req := providerTestRequest(http.MethodDelete, "/api/v1/projects/orphans/"+name, "admin", nil)
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		s.handleProjectOrphansDelete(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("orphan delete name %q: status %d, want 400", name, rec.Code)
		}
	}

	orphanDir := filepath.Join(s.st.ProjectDir("."), "legacy-orphan")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(orphanDir) })

	scanRec := httptest.NewRecorder()
	scanReq := providerTestRequest(http.MethodGet, "/api/v1/projects/orphans", "admin", nil)
	s.handleProjectOrphans(scanRec, scanReq)
	if scanRec.Code != http.StatusOK {
		t.Fatalf("scan status=%d body=%s", scanRec.Code, scanRec.Body.String())
	}
	if strings.Contains(scanRec.Body.String(), s.st.ProjectDir(".")) {
		t.Fatalf("scan response must not leak absolute host paths: %s", scanRec.Body.String())
	}
}
