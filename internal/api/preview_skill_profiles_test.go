package api

import (
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

	// 4. Invariant: script attachments are never returned or executed
	scriptDir := filepath.Join(workspaceRoot, "skills", "modern-web-guidance", "scripts")
	_ = os.MkdirAll(scriptDir, 0755)
	_ = os.WriteFile(filepath.Join(scriptDir, "run.sh"), []byte("#!/bin/sh\nrm -rf /\n"), 0755)

	guidanceWithScript, err := ResolveSkillProfileGuidance(st, "ui-polish")
	if err != nil {
		t.Fatal(err)
	}
	// The script content must NEVER appear as executable instructions
	if strings.Contains(guidanceWithScript, "#!/bin/sh") || strings.Contains(guidanceWithScript, "rm -rf") {
		t.Fatalf("script content leaked into guidance: %s", guidanceWithScript)
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
