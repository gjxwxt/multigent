package projecttemplate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMaterializeReactGoFullstack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	report, err := Materialize(root, ReactGoFullstackID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if report.ID != ReactGoFullstackID || report.Version != ReactGoFullstackVersion || len(report.Digest) != 64 {
		t.Fatalf("unexpected report: %#v", report)
	}
	for _, path := range []string{
		".gitignore",
		".env.example",
		".multigent/runtime.json",
		"README.md",
		"Makefile",
		"web/package.json",
		"web/src/App.tsx",
		"server/go.mod",
		"server/main.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Fatalf("expected generated file %s: %v", path, err)
		}
	}
	var runtime struct {
		Version int `json:"version"`
		Backend struct {
			Directory  string `json:"directory"`
			HealthPath string `json:"healthPath"`
		} `json:"backend"`
	}
	raw, err := os.ReadFile(filepath.Join(root, ".multigent", "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime contract: %v", err)
	}
	if err := json.Unmarshal(raw, &runtime); err != nil {
		t.Fatalf("decode runtime contract: %v", err)
	}
	if runtime.Version != 1 || runtime.Backend.Directory != "server" || runtime.Backend.HealthPath != "/api/health" {
		t.Fatalf("unexpected runtime contract: %#v", runtime)
	}
	if _, err := Materialize(root, ReactGoFullstackID); err == nil {
		t.Fatal("expected existing files to prevent overwrite")
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = filepath.Join(root, "server")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated Go server does not test: %v\n%s", err, output)
	}
}

func TestMaterializeRejectsUnsupportedTemplate(t *testing.T) {
	if _, err := Materialize(filepath.Join(t.TempDir(), "repo"), "unknown"); err == nil {
		t.Fatal("expected unsupported template error")
	}
}
