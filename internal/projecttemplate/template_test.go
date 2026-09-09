package projecttemplate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		"AGENTS.md",
		"CLAUDE.md",
		"Makefile",
		"docs/architecture.md",
		"docs/api-spec.md",
		"docs/tdd-guide.md",
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

func TestMaterializeReactSpringBoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	report, err := Materialize(root, ReactSpringBootID)
	if err != nil {
		t.Fatalf("materialize spring boot: %v", err)
	}
	if report.ID != ReactSpringBootID || report.Version != ReactSpringBootVersion || len(report.Digest) != 64 {
		t.Fatalf("unexpected report: %#v", report)
	}

	expectedPaths := []string{
		".gitignore",
		".gitlab-ci.yml",
		".env.example",
		".multigent/runtime.json",
		"README.md",
		"AGENTS.md",
		"CLAUDE.md",
		"Makefile",
		"docs/architecture.md",
		"docs/api-spec.md",
		"docs/tdd-guide.md",
		"deploy/Dockerfile",
		"deploy/compose.yml",
		"server/build.gradle",
		"server/settings.gradle",
		"server/gradlew",
		"server/gradle/wrapper/gradle-wrapper.properties",
		"server/src/main/resources/application.yml",
		"server/src/main/java/com/example/app/Application.java",
		"server/src/main/java/com/example/app/config/WebConfig.java",
		"server/src/main/java/com/example/app/controller/HealthController.java",
		"server/src/main/java/com/example/app/controller/ItemController.java",
		"server/src/main/java/com/example/app/service/ItemService.java",
		"server/src/main/java/com/example/app/service/impl/ItemServiceImpl.java",
		"server/src/main/java/com/example/app/repository/ItemRepository.java",
		"server/src/main/java/com/example/app/model/HealthResponse.java",
		"server/src/main/java/com/example/app/model/Item.java",
		"server/src/main/java/com/example/app/model/CreateItemRequest.java",
		"server/src/main/java/com/example/app/exception/ApiErrorResponse.java",
		"server/src/main/java/com/example/app/exception/ResourceNotFoundException.java",
		"server/src/main/java/com/example/app/exception/GlobalExceptionHandler.java",
		"server/src/test/java/com/example/app/ApplicationTests.java",
		"server/src/test/java/com/example/app/controller/HealthControllerTest.java",
		"server/src/test/java/com/example/app/controller/ItemControllerTest.java",
		"server/src/test/java/com/example/app/service/ItemServiceTest.java",
		"server/gradle/wrapper/gradle-wrapper.jar",
		"web/package.json",
		"web/package-lock.json",
		"web/vite.config.ts",
		"web/tsconfig.json",
		"web/index.html",
		"web/src/index.css",
		"web/src/main.tsx",
		"web/src/App.tsx",
		"web/src/types/index.ts",
		"web/src/services/api.ts",
		"web/src/components/Layout.tsx",
		"web/src/components/ItemCard.tsx",
		"web/src/components/CreateItemModal.tsx",
		"web/src/pages/HomePage.tsx",
		"web/src/test/setupTests.ts",
		"web/src/test/App.test.tsx",
	}

	for _, path := range expectedPaths {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Errorf("expected generated file %s: %v", path, err)
		}
	}

	info, err := os.Stat(filepath.Join(root, "server", "gradlew"))
	if err != nil {
		t.Fatalf("stat gradlew: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("gradlew must be executable, got: %v", info.Mode())
	}

	var runtime struct {
		Version int `json:"version"`
		Backend struct {
			Directory  string `json:"directory"`
			HealthPath string `json:"healthPath"`
			Port       int    `json:"port"`
		} `json:"backend"`
	}
	raw, err := os.ReadFile(filepath.Join(root, ".multigent", "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime contract: %v", err)
	}
	if err := json.Unmarshal(raw, &runtime); err != nil {
		t.Fatalf("decode runtime contract: %v", err)
	}
	if runtime.Version != 1 || runtime.Backend.Directory != "server" || runtime.Backend.HealthPath != "/api/health" || runtime.Backend.Port != 8080 {
		t.Fatalf("unexpected runtime contract: %#v", runtime)
	}

	if _, err := Materialize(root, ReactSpringBootID); err == nil {
		t.Fatal("expected existing files to prevent overwrite")
	}
}

func TestCIBaselineFilesForTemplate(t *testing.T) {
	for _, id := range []string{ReactGoFullstackID, ReactSpringBootID} {
		files, err := CIBaselineFilesForTemplate(id)
		if err != nil {
			t.Fatalf("ci baseline for %s: %v", id, err)
		}
		if len(files) != 3 {
			t.Fatalf("expected 3 ci baseline files for %s, got %d", id, len(files))
		}
		for _, required := range ciBaselinePaths {
			if _, ok := files[required]; !ok {
				t.Errorf("expected baseline file %s in template %s", required, id)
			}
		}
	}
}

func TestReactSpringBootBlackboxLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if _, err := Materialize(root, ReactSpringBootID); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	cmdDoctor := exec.Command("make", "doctor")
	cmdDoctor.Dir = root
	if output, err := cmdDoctor.CombinedOutput(); err != nil {
		t.Fatalf("make doctor failed: %v\n%s", err, output)
	}

	cmdGradle := exec.Command("./gradlew", "--version")
	cmdGradle.Dir = filepath.Join(root, "server")
	if output, err := cmdGradle.CombinedOutput(); err != nil {
		t.Fatalf("gradlew --version failed: %v\n%s", err, output)
	}
}

func TestReactSpringBootMakeInstallFailsWithoutLockfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if _, err := Materialize(root, ReactSpringBootID); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	lockfile := filepath.Join(root, "web", "package-lock.json")
	if err := os.Remove(lockfile); err != nil {
		t.Fatalf("remove lockfile: %v", err)
	}

	cmdInstall := exec.Command("make", "install")
	cmdInstall.Dir = root
	output, err := cmdInstall.CombinedOutput()
	if err == nil {
		t.Fatalf("expected make install to fail without lockfile, but succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "web/package-lock.json is required") {
		t.Fatalf("expected error message requiring lockfile, got:\n%s", output)
	}
}

func TestReactSpringBootControlledCIIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping controlled CI integration test in short mode")
	}
	for _, bin := range []string{"make", "npm", "java"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("prerequisite binary %q not found, skipping integration test", bin)
		}
	}

	root := filepath.Join(t.TempDir(), "repo")
	if _, err := Materialize(root, ReactSpringBootID); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	cmdInstall := exec.Command("make", "install")
	cmdInstall.Dir = root
	if output, err := cmdInstall.CombinedOutput(); err != nil {
		t.Fatalf("make install failed: %v\n%s", err, output)
	}

	cmdVerify := exec.Command("make", "verify")
	cmdVerify.Dir = root
	if output, err := cmdVerify.CombinedOutput(); err != nil {
		t.Fatalf("make verify failed: %v\n%s", err, output)
	}
}
