package preview

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProjectType(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "preview-detect-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Case 1: Empty dir -> CLI
	if pt := DetectProjectType(tempDir); pt != ProjectTypeCLI {
		t.Fatalf("expected CLI for empty dir, got %v", pt)
	}

	// Case 2: Frontend only (package.json)
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"name":"frontend"}`), 0644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if pt := DetectProjectType(tempDir); pt != ProjectTypeFrontend {
		t.Fatalf("expected Frontend for package.json, got %v", pt)
	}

	// Case 3: Backend only (go.mod)
	os.Remove(filepath.Join(tempDir, "package.json"))
	if err := os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte(`module myapp`), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if pt := DetectProjectType(tempDir); pt != ProjectTypeBackend {
		t.Fatalf("expected Backend for go.mod, got %v", pt)
	}

	// Case 4: Fullstack (both frontend package.json and root go.mod)
	if err := os.MkdirAll(filepath.Join(tempDir, "frontend"), 0755); err != nil {
		t.Fatalf("mkdir frontend: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "frontend", "package.json"), []byte(`{"name":"web"}`), 0644); err != nil {
		t.Fatalf("write frontend package.json: %v", err)
	}
	if pt := DetectProjectType(tempDir); pt != ProjectTypeFullstack {
		t.Fatalf("expected Fullstack for frontend+backend, got %v", pt)
	}
}
