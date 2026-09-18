package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/builtins"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/store"
)

func TestPreviewSkillProfiles_ListAvailable(t *testing.T) {
	profiles := ListPreviewSkillProfiles()
	if len(profiles) < 4 {
		t.Fatalf("expected at least 4 default skill profiles, got %d", len(profiles))
	}

	foundMap := make(map[string]bool)
	for _, p := range profiles {
		foundMap[p.ID] = true
		if p.Name == "" || p.Description == "" || len(p.SkillIDs) == 0 {
			t.Errorf("profile %+v has invalid fields", p)
		}
	}

	expectedIDs := []string{"ui-polish", "a11y-remediation", "responsive-layout", "form-logic"}
	for _, exp := range expectedIDs {
		if !foundMap[exp] {
			t.Errorf("missing expected profile: %s", exp)
		}
	}
}

func TestPreviewSkillProfiles_Lookup(t *testing.T) {
	p, ok := LookupPreviewSkillProfile("ui-polish")
	if !ok || p.ID != "ui-polish" {
		t.Fatalf("expected to find ui-polish, got: ok=%v, p=%+v", ok, p)
	}

	_, ok = LookupPreviewSkillProfile("malicious-arbitrary-profile")
	if ok {
		t.Fatal("expected unknown profile lookup to return false")
	}
}

func TestPreviewSkillProfiles_DigestAndTamperDetection(t *testing.T) {
	skillDir := t.TempDir()
	skillMD := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillMD, []byte("# Test Skill Content V1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	digest1, err := ComputeSkillDigest(skillDir)
	if err != nil {
		t.Fatalf("ComputeSkillDigest failed: %v", err)
	}
	if len(digest1) != 64 {
		t.Fatalf("expected 64-char hex digest, got: %s", digest1)
	}

	// Repeated computation produces same digest
	digest2, err := ComputeSkillDigest(skillDir)
	if err != nil || digest2 != digest1 {
		t.Fatalf("digest mismatch on same content: %s != %s (err: %v)", digest1, digest2, err)
	}

	// Modifying file changes digest
	if err := os.WriteFile(skillMD, []byte("# Test Skill Content V2 (Tampered)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	digest3, err := ComputeSkillDigest(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	if digest3 == digest1 {
		t.Fatal("expected digest to change after modifying SKILL.md")
	}
}

func TestPreviewSkillProfiles_ResolveGuidance_BuiltinSkills(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".multigent", "agency.yaml"), []byte("name: test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Ensure builtin skills (including modern-web-guidance and a11y-debugging) are copied
	if err := builtins.EnsureSkills(workspaceRoot); err != nil {
		t.Fatalf("EnsureSkills failed: %v", err)
	}

	dbPath := filepath.Join(workspaceRoot, ".multigent", "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st := store.NewDB(workspaceRoot, db)

	// 1. Resolve ui-polish
	guidance, err := ResolveSkillProfileGuidance(st, "ui-polish")
	if err != nil {
		t.Fatalf("ResolveSkillProfileGuidance(ui-polish) failed: %v", err)
	}
	if !strings.Contains(guidance, "Skill Profile: 视觉排版与质感调优 (ui-polish)") {
		t.Errorf("missing profile header in guidance: %s", guidance)
	}
	if !strings.Contains(guidance, "modern-web-guidance") {
		t.Errorf("missing modern-web-guidance in guidance: %s", guidance)
	}
	if !strings.Contains(guidance, "digest:") {
		t.Errorf("missing digest in guidance: %s", guidance)
	}

	// 2. Resolve a11y-remediation
	a11yGuidance, err := ResolveSkillProfileGuidance(st, "a11y-remediation")
	if err != nil {
		t.Fatalf("ResolveSkillProfileGuidance(a11y-remediation) failed: %v", err)
	}
	if !strings.Contains(a11yGuidance, "a11y-debugging") {
		t.Errorf("missing a11y-debugging in guidance: %s", a11yGuidance)
	}

	// 3. Unknown profile fails closed
	_, err = ResolveSkillProfileGuidance(st, "nonexistent-profile")
	if err == nil {
		t.Fatal("expected error for nonexistent profile, got nil")
	}
}

func TestPreviewSkillProfiles_Endpoint(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	project := "test-proj"
	taskID := "task-001"
	if err := s.st.SaveProject(project, &entity.Project{Name: project}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+project+"/tasks/"+taskID+"/preview/profiles", nil)
	req.SetPathValue("name", project)
	req.SetPathValue("taskId", taskID)
	// Inject admin user context from fixture
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "admin"))

	rec := httptest.NewRecorder()
	s.handleGetTaskPreviewProfiles(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		OK       bool                  `json:"ok"`
		Profiles []PreviewSkillProfile `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if !resp.OK || len(resp.Profiles) < 4 {
		t.Fatalf("unexpected profiles response: %+v", resp)
	}
}

func TestPreviewSkillProfiles_PromptInjectionInPreviewChat(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".multigent", "agency.yaml"), []byte("name: test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := builtins.EnsureSkills(workspaceRoot); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(workspaceRoot, ".multigent", "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st := store.NewDB(workspaceRoot, db)
	s := &Server{
		root:      workspaceRoot,
		controlDB: db,
		st:        st,
		users:     newUserStore(db),
	}

	project := "proj-preview-skill"
	taskID := "task-skill-101"
	worktreeDir := t.TempDir()

	guidance, err := ResolveSkillProfileGuidance(st, "ui-polish")
	if err != nil {
		t.Fatal(err)
	}

	prompt := s.buildPreviewChatPrompt(project, taskID, worktreeDir, "请修改标题颜色", nil, guidance)
	if !strings.Contains(prompt, "【专业技能指引 | Skill Profile: 视觉排版与质感调优 (ui-polish)】") {
		t.Fatalf("prompt missing skill profile header: %s", prompt)
	}
	if !strings.Contains(prompt, "modern-web-guidance") {
		t.Fatalf("prompt missing modern-web-guidance skill name: %s", prompt)
	}
	if !strings.Contains(prompt, "请修改标题颜色") {
		t.Fatalf("prompt missing user message: %s", prompt)
	}
}

func TestPreviewSkillProfiles_BaselineManifestMatchesBuiltins(t *testing.T) {
	tempDir := t.TempDir()
	if err := builtins.EnsureSkills(tempDir); err != nil {
		t.Fatalf("EnsureSkills failed: %v", err)
	}

	for skillID, expectedDigest := range DefaultTrustedBuiltinSkillDigests {
		skillDir := filepath.Join(tempDir, "skills", skillID)
		digest, err := ComputeSkillDigest(skillDir)
		if err != nil {
			t.Fatalf("ComputeSkillDigest for %s failed: %v", skillID, err)
		}
		if digest != expectedDigest {
			t.Errorf("manifest mismatch for skill %s: expected %s, got %s", skillID, expectedDigest, digest)
		}
	}
}

func TestPreviewSkillProfiles_TamperedSkillFailsResolution(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".multigent", "agency.yaml"), []byte("name: test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := builtins.EnsureSkills(workspaceRoot); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(workspaceRoot, ".multigent", "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st := store.NewDB(workspaceRoot, db)

	// Step A: Untouched skill resolves successfully
	guidance, err := ResolveSkillProfileGuidance(st, "ui-polish")
	if err != nil {
		t.Fatalf("expected initial resolution to succeed, got: %v", err)
	}
	if guidance == "" {
		t.Fatal("expected non-empty guidance")
	}

	// Step B: Tamper with SKILL.md -> Resolve MUST fail
	skillMD := filepath.Join(workspaceRoot, "skills", "modern-web-guidance", "SKILL.md")
	f, err := os.OpenFile(skillMD, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\n\n<!-- Tampered malicious payload -->\n")
	_ = f.Close()

	_, err = ResolveSkillProfileGuidance(st, "ui-polish")
	if err == nil {
		t.Fatal("expected tampered SKILL.md to cause ResolveSkillProfileGuidance to fail, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") || !strings.Contains(err.Error(), "tampered or untrusted") {
		t.Fatalf("expected digest mismatch error message, got: %v", err)
	}

	// Step C: Adding an unauthorized script also causes Resolve to fail (directory tampering)
	restoreOriginal := builtins.EnsureSkills(workspaceRoot)
	if restoreOriginal != nil {
		t.Fatal(restoreOriginal)
	}
	// Verify restored works
	if _, err := ResolveSkillProfileGuidance(st, "ui-polish"); err != nil {
		t.Fatalf("restored skill failed: %v", err)
	}
	// Add rogue script
	scriptDir := filepath.Join(workspaceRoot, "skills", "modern-web-guidance", "scripts")
	_ = os.MkdirAll(scriptDir, 0755)
	_ = os.WriteFile(filepath.Join(scriptDir, "rogue.sh"), []byte("#!/bin/sh\necho malicious\n"), 0755)

	_, err = ResolveSkillProfileGuidance(st, "ui-polish")
	if err == nil {
		t.Fatal("expected extra rogue script in skill dir to cause ResolveSkillProfileGuidance to fail, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch error, got: %v", err)
	}
}

func TestPreviewSkillProfiles_UntrustedSkillRejection(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".multigent", "agency.yaml"), []byte("name: test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := builtins.EnsureSkills(workspaceRoot); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(workspaceRoot, ".multigent", "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st := store.NewDB(workspaceRoot, db)

	// Temporarily override trusted digest for modern-web-guidance to wrong hash
	cleanup := SetTrustedSkillDigestForTest("modern-web-guidance", "0000000000000000000000000000000000000000000000000000000000000000")
	defer cleanup()

	_, err = ResolveSkillProfileGuidance(st, "ui-polish")
	if err == nil {
		t.Fatal("expected rejection when trusted digest does not match, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPutSkillPrompt_RequiresWorkspaceAdmin(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	// In newConnectionGrantPolicyServer fixture:
	// "admin" is WorkspaceRoleAdmin
	// "owner" is WorkspaceRoleMember (regular user without workspace admin privileges)

	// Ensure built-in skills exist in workspace
	if err := builtins.EnsureSkills(s.root); err != nil {
		t.Fatal(err)
	}

	body := promptSaveBody{
		Content: "# Malicious modified skill prompt\n",
	}
	bodyBytes, _ := json.Marshal(body)

	// 1. Regular user ("owner") attempts PUT -> Expect 403 Forbidden
	reqMember := httptest.NewRequest(http.MethodPut, "/api/v1/skills/modern-web-guidance", bytes.NewReader(bodyBytes))
	reqMember.Header.Set("Content-Type", "application/json")
	reqMember.SetPathValue("name", "modern-web-guidance")
	reqMember = reqMember.WithContext(context.WithValue(reqMember.Context(), ctxUserKey, "owner"))

	recMember := httptest.NewRecorder()
	s.handlePutSkillPrompt(recMember, reqMember)

	if recMember.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-admin user, got %d: %s", recMember.Code, recMember.Body.String())
	}
	if !strings.Contains(recMember.Body.String(), "workspace_admin_required") && !strings.Contains(recMember.Body.String(), "workspace admin access required") {
		t.Fatalf("expected workspace admin error code, got: %s", recMember.Body.String())
	}

	// 2. Workspace Admin ("admin") attempts PUT -> Expect 200 OK
	reqAdmin := httptest.NewRequest(http.MethodPut, "/api/v1/skills/modern-web-guidance", bytes.NewReader(bodyBytes))
	reqAdmin.Header.Set("Content-Type", "application/json")
	reqAdmin.SetPathValue("name", "modern-web-guidance")
	reqAdmin = reqAdmin.WithContext(context.WithValue(reqAdmin.Context(), ctxUserKey, "admin"))

	recAdmin := httptest.NewRecorder()
	s.handlePutSkillPrompt(recAdmin, reqAdmin)

	if recAdmin.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for admin user, got %d: %s", recAdmin.Code, recAdmin.Body.String())
	}
}
