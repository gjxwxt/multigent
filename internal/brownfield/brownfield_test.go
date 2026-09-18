package brownfield

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigent/multigent/internal/preview"
	"github.com/multigent/multigent/internal/projecttemplate"
	"github.com/multigent/multigent/internal/sandbox"
)

func TestDetectAndEvaluate_RealProjectTemplates(t *testing.T) {
	// 1. Test against materialized react_go_fullstack
	tempGo := t.TempDir()
	if _, err := projecttemplate.Materialize(tempGo, projecttemplate.ReactGoFullstackID); err != nil {
		t.Fatalf("Materialize react_go_fullstack: %v", err)
	}

	reportGo, err := Detect(tempGo)
	if err != nil {
		t.Fatalf("Detect react_go_fullstack: %v", err)
	}
	if len(reportGo.Services) < 2 {
		t.Fatalf("expected at least 2 services in react_go_fullstack, got %d", len(reportGo.Services))
	}
	if reportGo.RecommendedProfile != sandbox.ProfileBase {
		t.Fatalf("expected profile base, got %s", reportGo.RecommendedProfile)
	}

	resGo := Evaluate(reportGo)
	if resGo.Status != StatusReady {
		t.Fatalf("expected react_go_fullstack to be ready, got %s, issues: %+v", resGo.Status, resGo.Issues)
	}

	// Conformance assertion against preview.LoadRuntimeSpec
	dotMgGo := filepath.Join(tempGo, ".multigent")
	if err := os.MkdirAll(dotMgGo, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotMgGo, "runtime.json"), []byte(resGo.SynthesizedRuntimeRaw), 0644); err != nil {
		t.Fatal(err)
	}
	specGo, err := preview.LoadRuntimeSpec(tempGo)
	if err != nil {
		t.Fatalf("conformance check failed: preview.LoadRuntimeSpec returned error: %v", err)
	}
	if specGo.Version != 1 {
		t.Fatalf("expected spec version 1, got %d", specGo.Version)
	}
	if specGo.Frontend == nil || specGo.Backend == nil {
		t.Fatalf("expected both frontend and backend in synthesized spec, got: %+v", specGo)
	}

	// 2. Test against materialized react_spring_boot
	tempSpring := t.TempDir()
	if _, err := projecttemplate.Materialize(tempSpring, projecttemplate.ReactSpringBootID); err != nil {
		t.Fatalf("Materialize react_spring_boot: %v", err)
	}

	reportSpring, err := Detect(tempSpring)
	if err != nil {
		t.Fatalf("Detect react_spring_boot: %v", err)
	}
	if reportSpring.RecommendedProfile != sandbox.ProfileJVM21 {
		t.Fatalf("expected profile jvm21 for Spring Boot, got %s", reportSpring.RecommendedProfile)
	}

	resSpring := Evaluate(reportSpring)
	if resSpring.Status != StatusReady {
		t.Fatalf("expected react_spring_boot to be ready, got %s, issues: %+v", resSpring.Status, resSpring.Issues)
	}

	// Conformance assertion
	dotMgSpring := filepath.Join(tempSpring, ".multigent")
	if err := os.MkdirAll(dotMgSpring, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotMgSpring, "runtime.json"), []byte(resSpring.SynthesizedRuntimeRaw), 0644); err != nil {
		t.Fatal(err)
	}
	specSpring, err := preview.LoadRuntimeSpec(tempSpring)
	if err != nil {
		t.Fatalf("conformance check failed for spring boot: %v", err)
	}
	if specSpring.Version != 1 {
		t.Fatalf("expected spec version 1, got %d", specSpring.Version)
	}
}

func TestEvaluate_NodeMissingLockfile_Blocking(t *testing.T) {
	tempDir := t.TempDir()
	pkgJSON := `{"name":"sample","scripts":{"dev":"vite","build":"vite build"}}`
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Detect(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	res := Evaluate(report)

	if res.Status != StatusNotReady {
		t.Fatalf("expected missing lockfile to be StatusNotReady, got %s", res.Status)
	}

	foundLockfileIssue := false
	for _, issue := range res.Issues {
		if issue.Code == "missing_lockfile" && issue.Severity == SeverityBlocking {
			foundLockfileIssue = true
		}
	}
	if !foundLockfileIssue {
		t.Fatalf("expected blocking missing_lockfile issue, got: %+v", res.Issues)
	}

	// Now add lockfile and verify it becomes ready
	if err := os.WriteFile(filepath.Join(tempDir, "package-lock.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	report2, _ := Detect(tempDir)
	res2 := Evaluate(report2)
	if res2.Status != StatusReady {
		t.Fatalf("expected ready after adding package-lock.json, got %s, issues: %+v", res2.Status, res2.Issues)
	}
}

func TestEvaluate_GoMissingGoSum_Blocking(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Detect(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	res := Evaluate(report)

	if res.Status != StatusNotReady {
		t.Fatalf("expected missing go.sum to be StatusNotReady, got %s", res.Status)
	}

	// Add go.sum
	if err := os.WriteFile(filepath.Join(tempDir, "go.sum"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	report2, _ := Detect(tempDir)
	res2 := Evaluate(report2)
	if res2.Status != StatusReady {
		t.Fatalf("expected ready after adding go.sum, got %s, issues: %+v", res2.Status, res2.Issues)
	}
}

func TestEvaluate_JavaWrapperExecutableCheck(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "build.gradle"), []byte("plugins { id 'java' }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Missing wrapper
	report, err := Detect(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	res := Evaluate(report)
	if res.Status != StatusNotReady {
		t.Fatalf("expected missing wrapper to fail, got %s", res.Status)
	}

	// 2. Non-executable wrapper
	gradlewPath := filepath.Join(tempDir, "gradlew")
	if err := os.WriteFile(gradlewPath, []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report2, _ := Detect(tempDir)
	res2 := Evaluate(report2)
	if res2.Status != StatusNotReady {
		t.Fatalf("expected non-executable wrapper to fail, got %s", res2.Status)
	}

	// 3. Executable wrapper
	if err := os.Chmod(gradlewPath, 0755); err != nil {
		t.Fatal(err)
	}
	report3, _ := Detect(tempDir)
	res3 := Evaluate(report3)
	if res3.Status != StatusReady {
		t.Fatalf("expected ready with executable wrapper, got %s, issues: %+v", res3.Status, res3.Issues)
	}
}

func TestEvaluate_PythonTieredDeterminism_WarningNotBlocking(t *testing.T) {
	tempDir := t.TempDir()
	// requirements.txt without hashes
	if err := os.WriteFile(filepath.Join(tempDir, "requirements.txt"), []byte("flask==3.0.0\nrequests>=2.31.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Detect(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	res := Evaluate(report)

	// Amendment 4: requirements.txt without hashes should be a WARNING, NOT BLOCKING
	if res.Status != StatusReady {
		t.Fatalf("expected Python with requirements.txt to be StatusReady with warning, got %s, issues: %+v", res.Status, res.Issues)
	}

	foundWarning := false
	for _, issue := range res.Issues {
		if issue.Code == "weak_determinism_python" && issue.Severity == SeverityWarning {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("expected weak_determinism_python warning, got: %+v", res.Issues)
	}
}

func TestEvaluate_PythonPyprojectWithoutLockfile_Blocking(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "pyproject.toml"), []byte("[project]\nname = 'test'\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Detect(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	res := Evaluate(report)
	if res.Status != StatusNotReady {
		t.Fatalf("expected pyproject without lockfile to be StatusNotReady, got %s", res.Status)
	}

	// Add poetry.lock
	if err := os.WriteFile(filepath.Join(tempDir, "poetry.lock"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	report2, _ := Detect(tempDir)
	res2 := Evaluate(report2)
	if res2.Status != StatusReady {
		t.Fatalf("expected ready with poetry.lock, got %s, issues: %+v", res2.Status, res2.Issues)
	}
}

func TestSynthesizeRuntimeSpec_Conformance(t *testing.T) {
	report := ScanReport{
		Services: []ServiceDetection{
			{
				Name:           "frontend",
				Directory:      "web",
				TechStack:      TechStackNode,
				PackageManager: PkgNPM,
				HasLockfile:    true,
				Scripts: map[string]string{
					"dev": "vite",
				},
				DetectedPort: 5173,
				HealthPath:   "/",
			},
			{
				Name:           "backend",
				Directory:      "server",
				TechStack:      TechStackGo,
				PackageManager: PkgGoMod,
				HasLockfile:    true,
				DetectedPort:   8080,
				HealthPath:     "/api/health",
			},
		},
	}

	spec, rawJSON, err := SynthesizeRuntimeSpec(report)
	if err != nil {
		t.Fatalf("SynthesizeRuntimeSpec: %v", err)
	}

	tempDir := t.TempDir()
	dotMg := filepath.Join(tempDir, ".multigent")
	if err := os.MkdirAll(dotMg, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotMg, "runtime.json"), []byte(rawJSON), 0644); err != nil {
		t.Fatal(err)
	}

	loaded, err := preview.LoadRuntimeSpec(tempDir)
	if err != nil {
		t.Fatalf("preview.LoadRuntimeSpec failed on synthesized spec: %v", err)
	}

	if loaded.Version != spec.Version {
		t.Fatalf("expected version %d, got %d", spec.Version, loaded.Version)
	}
	if loaded.Frontend.Directory != "web" || loaded.Backend.Directory != "server" {
		t.Fatalf("unexpected directories: frontend=%s backend=%s", loaded.Frontend.Directory, loaded.Backend.Directory)
	}
}
