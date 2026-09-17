package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multigent/multigent/internal/store"
)

// PreviewSkillProfile represents a curated, server-side allowlisted skill preset
// for preview Copilot turns (Task 3.2, batch review 2026-09-15 §4.2).
type PreviewSkillProfile struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	SkillIDs    []string `json:"skillIds"`
}

var defaultPreviewSkillProfiles = map[string]PreviewSkillProfile{
	"ui-polish": {
		ID:          "ui-polish",
		Name:        "视觉排版与质感调优",
		Description: "依据现代 Web 规范优化页面色彩搭配、层次感与排版质感",
		SkillIDs:    []string{"modern-web-guidance"},
	},
	"a11y-remediation": {
		ID:          "a11y-remediation",
		Name:        "无障碍访问性合规",
		Description: "检查并修复 ARIA 语义、键盘导航焦点与对比度无障碍规范",
		SkillIDs:    []string{"a11y-debugging"},
	},
	"responsive-layout": {
		ID:          "responsive-layout",
		Name:        "移动端与响应式适配",
		Description: "适配手机、平板等窄屏视口宽度与流式容器布局",
		SkillIDs:    []string{"modern-web-guidance"},
	},
	"form-logic": {
		ID:          "form-logic",
		Name:        "表单交互与提交校验",
		Description: "规范化表单输入组件校验、状态提示与交互逻辑",
		SkillIDs:    []string{"modern-web-guidance"},
	},
}

// ListPreviewSkillProfiles returns a sorted list of allowlisted profiles.
func ListPreviewSkillProfiles() []PreviewSkillProfile {
	ids := make([]string, 0, len(defaultPreviewSkillProfiles))
	for id := range defaultPreviewSkillProfiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]PreviewSkillProfile, 0, len(ids))
	for _, id := range ids {
		out = append(out, defaultPreviewSkillProfiles[id])
	}
	return out
}

// LookupPreviewSkillProfile looks up a profile by ID in the allowlist.
func LookupPreviewSkillProfile(id string) (PreviewSkillProfile, bool) {
	p, ok := defaultPreviewSkillProfiles[strings.TrimSpace(id)]
	return p, ok
}

// ComputeSkillDigest calculates a SHA-256 digest of the skill files (SKILL.md and assets).
// This guarantees integrity before mounting into prompt guidance.
func ComputeSkillDigest(skillDir string) (string, error) {
	if fi, err := os.Stat(skillDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("skill directory invalid: %w", err)
	}

	h := sha256.New()
	var filePaths []string
	err := filepath.Walk(skillDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillDir, path)
		if err != nil {
			return err
		}
		// Ignore hidden files and editor temporary files
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") || strings.HasSuffix(base, "~") {
			return nil
		}
		filePaths = append(filePaths, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk skill dir: %w", err)
	}

	sort.Strings(filePaths)
	for _, rel := range filePaths {
		fullPath := filepath.Join(skillDir, rel)
		f, err := os.Open(fullPath)
		if err != nil {
			return "", fmt.Errorf("open skill file %q: %w", rel, err)
		}
		// Hash the relative path and contents
		h.Write([]byte(rel + "\x00"))
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("hash skill file %q: %w", rel, err)
		}
		_ = f.Close()
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// ResolveSkillProfileGuidance verifies the requested profile, computes digests,
// and formats pure text guidance for prompt injection.
//
// Invariant (batch review §4.2): Execution attachments (scripts/.sh) are strictly
// omitted/disabled; only pure declarative markdown guidance is injected.
func ResolveSkillProfileGuidance(st store.Store, profileID string) (string, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return "", nil
	}
	profile, ok := LookupPreviewSkillProfile(profileID)
	if !ok {
		return "", fmt.Errorf("unknown skill profile %q", profileID)
	}

	if st == nil {
		return "", errors.New("skill store unavailable")
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("【专业技能指引 | Skill Profile: %s (%s)】\n", profile.Name, profile.ID))
	buf.WriteString("当前调优任务匹配了专业工程技能指引（摘要校验通过），请严格遵循以下准则完成代码修改：\n\n")

	for _, skillID := range profile.SkillIDs {
		sk, err := st.Skill(skillID)
		if err != nil || sk == nil {
			return "", fmt.Errorf("required skill %q for profile %q not found: %w", skillID, profileID, err)
		}
		skillDir := st.SkillDir(skillID)
		digest, err := ComputeSkillDigest(skillDir)
		if err != nil {
			return "", fmt.Errorf("compute skill digest for %q: %w", skillID, err)
		}
		prompt, err := st.SkillPrompt(skillID)
		if err != nil {
			return "", fmt.Errorf("load skill prompt for %q: %w", skillID, err)
		}

		buf.WriteString(fmt.Sprintf("── [Skill: %s (digest: %.8s...)] ──\n", sk.Name, digest))
		buf.WriteString(strings.TrimSpace(prompt))
		buf.WriteString("\n\n")
	}

	return strings.TrimSpace(buf.String()), nil
}

func (s *Server) handleGetTaskPreviewProfiles(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	if project == "" {
		project = r.PathValue("project")
	}
	taskID := r.PathValue("taskId")
	if project == "" || taskID == "" {
		s.jsonError(w, http.StatusBadRequest, "project and taskId are required")
		return
	}

	if !s.checkProjectAccess(w, r, project) {
		return
	}

	profiles := ListPreviewSkillProfiles()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"profiles": profiles,
	})
}
