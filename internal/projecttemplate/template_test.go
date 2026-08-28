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

func TestSeedPreservesRuntimeFilesAndIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(filepath.Join(root, ".multigent", "runtime-home"), 0755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, ".multigent", "runtime-home", "keep.json")
	if err := os.WriteFile(keep, []byte("platform-owned"), 0644); err != nil {
		t.Fatal(err)
	}

	first, err := Seed(root, ReactGoFullstackID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(first.Files) == 0 {
		t.Fatal("expected seeded files")
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "platform-owned" {
		t.Fatalf("runtime file was changed: %q, %v", got, err)
	}
	if _, err := Seed(root, ReactGoFullstackID); err != nil {
		t.Fatalf("idempotent seed: %v", err)
	}
}

func TestSeedRejectsConflictingFilesWithoutOverwriting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(root, "README.md")
	if err := os.WriteFile(readme, []byte("user content"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Seed(root, ReactGoFullstackID); err == nil {
		t.Fatal("expected conflicting file to be rejected")
	}
	if got, err := os.ReadFile(readme); err != nil || string(got) != "user content" {
		t.Fatalf("conflicting file was changed: %q, %v", got, err)
	}
}
