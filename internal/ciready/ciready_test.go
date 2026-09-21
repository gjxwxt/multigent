package ciready

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/projecttemplate"
)

func writeFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"name":"web","scripts":{"build":"vite build"}}`
	if err := os.WriteFile(filepath.Join(root, "web", "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "package-lock.json"), []byte(`{"name":"web","lockfileVersion":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := `{"version":1,"backend":{"directory":"server","command":"go run .","port":8080,"healthPath":"/api/health"}}`
	if err := os.WriteFile(filepath.Join(root, ".multigent", "runtime.json"), []byte(runtime), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func findCheck(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in %#v", name, report.Checks)
	return Check{}
}

func TestEnsureSeedsBaselineAndPasses(t *testing.T) {
	root := writeFixtureRepo(t)
	report, err := Ensure(root)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(report.Seeded) != 3 {
		t.Fatalf("expected 3 seeded files, got %#v", report.Seeded)
	}
	if report.Overall != OverallReady {
		t.Fatalf("expected ready, got %#v", report.Checks)
	}
	for _, path := range []string{".gitlab-ci.yml", "deploy/Dockerfile", "deploy/compose.yml"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Fatalf("seeded file %s: %v", path, err)
		}
	}
}

func TestEnsureIsIdempotentAndNeverOverwrites(t *testing.T) {
	root := writeFixtureRepo(t)
	custom := []byte("custom ci\n")
	if err := os.WriteFile(filepath.Join(root, ".gitlab-ci.yml"), custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(root); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := Ensure(root)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	for _, path := range second.Seeded {
		if path == ".gitlab-ci.yml" {
			t.Fatal("ensure must not seed over an existing .gitlab-ci.yml")
		}
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitlab-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(custom) {
		t.Fatalf("existing file was modified: %q", data)
	}
}

func TestCheckFlagsDriftedBaseline(t *testing.T) {
	root := writeFixtureRepo(t)
	drifted := `
stages: [test]
test:backend:
  stage: test
  script:
    - cd server && go test ./...
build:frontend:
  stage: test
  script:
    - cd web && npm run lint
package:
  stage: test
  script:
    - apk add --no-cache docker-cli
`
	if err := os.WriteFile(filepath.Join(root, ".gitlab-ci.yml"), []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Verify(root)
	if report.Overall != OverallNotReady {
		t.Fatalf("drifted repo must not be ready: %#v", report.Checks)
	}
	for name, want := range map[string]string{
		"required_jobs":     StatusFail, // build:backend/deploy missing
		"runner_tags":       StatusFail,
		"tag_only_release":  StatusFail,
		"npm_mirror":        StatusFail,
		"apk_cache":         StatusFail,
		"interruptible":     StatusFail,
		"frontend_scripts":  StatusFail, // npm run lint absent from package.json
		"health_path":       StatusFail, // compose.yml missing -> skip? see below
	} {
		got := findCheck(t, report, name)
		if name == "health_path" {
			// deploy/compose.yml is absent in this fixture: skip, not fail.
			if got.Status != StatusSkip {
				t.Fatalf("health_path: expected skip, got %#v", got)
			}
			continue
		}
		if got.Status != want {
			t.Fatalf("%s: expected %s, got %#v", name, want, got)
		}
	}
}

func TestCheckPassesOnSeededBaselineWithScripts(t *testing.T) {
	root := writeFixtureRepo(t)
	if _, err := Ensure(root); err != nil {
		t.Fatal(err)
	}
	report := Verify(root)
	if got := findCheck(t, report, "frontend_scripts"); got.Status != StatusPass {
		t.Fatalf("frontend_scripts: %#v", got)
	}
	if got := findCheck(t, report, "health_path"); got.Status != StatusPass {
		t.Fatalf("health_path: %#v", got)
	}
}

func TestVerifyReactSpringBoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if _, err := projecttemplate.Materialize(root, projecttemplate.ReactSpringBootID); err != nil {
		t.Fatalf("materialize react_spring_boot: %v", err)
	}
	report := Verify(root)
	if report.Overall != OverallReady {
		for _, c := range report.Checks {
			if c.Status == StatusFail {
				t.Errorf("failing check %s: %s", c.Name, c.Detail)
			}
		}
		t.Fatalf("expected react_spring_boot repo to be OverallReady, got %s", report.Overall)
	}
}

func TestEnsureSeedsSpringBootBaselineAndPasses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".multigent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "build.gradle"), []byte("plugins {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "server", "gradle", "wrapper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "gradle", "wrapper", "gradle-wrapper.jar"), []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := `{"name":"web","scripts":{"build":"vite build"}}`
	if err := os.WriteFile(filepath.Join(root, "web", "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "package-lock.json"), []byte(`{"name":"web","lockfileVersion":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := `{"version":1,"backend":{"directory":"server","command":"./gradlew bootRun","port":8080,"healthPath":"/api/health"}}`
	if err := os.WriteFile(filepath.Join(root, ".multigent", "runtime.json"), []byte(runtime), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Ensure(root)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(report.Seeded) != 3 {
		t.Fatalf("expected 3 seeded files, got %#v", report.Seeded)
	}
	if report.Overall != OverallReady {
		for _, c := range report.Checks {
			if c.Status == StatusFail {
				t.Errorf("check failed: %s - %s", c.Name, c.Detail)
			}
		}
		t.Fatalf("expected OverallReady, got %s", report.Overall)
	}

	ciData, err := os.ReadFile(filepath.Join(root, ".gitlab-ci.yml"))
	if err != nil {
		t.Fatalf("read seeded .gitlab-ci.yml: %v", err)
	}
	if !strings.Contains(string(ciData), "react_spring_boot") {
		t.Errorf("expected seeded .gitlab-ci.yml to be for react_spring_boot, got: %s", string(ciData))
	}
}

func TestCheckFlagsMissingLockfile(t *testing.T) {
	root := writeFixtureRepo(t)
	// Remove package-lock.json
	_ = os.Remove(filepath.Join(root, "web", "package-lock.json"))
	if _, err := Ensure(root); err != nil {
		t.Fatal(err)
	}
	report := Verify(root)
	check := findCheck(t, report, "lockfile")
	if check.Status != StatusFail {
		t.Fatalf("expected lockfile check to fail when package-lock.json is missing, got: %#v", check)
	}
	if report.Overall != OverallNotReady {
		t.Fatalf("expected report overall not_ready, got %s", report.Overall)
	}
}

func TestCheckFlagsUnexecutableGradlewOrMissingWrapper(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "build.gradle"), []byte("plugins {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// gradlew exists but not executable (0644)
	if err := os.WriteFile(filepath.Join(root, "server", "gradlew"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// missing wrapper jar
	report := Verify(root)
	check := findCheck(t, report, "build_tool_readiness")
	if check.Status != StatusFail {
		t.Fatalf("expected build_tool_readiness check to fail, got: %#v", check)
	}
	if !strings.Contains(check.Detail, "gradle-wrapper.jar is missing") || !strings.Contains(check.Detail, "not executable") {
		t.Fatalf("unexpected detail: %s", check.Detail)
	}
}

func TestProbeBuildDependencies(t *testing.T) {
	t.Run("pass when lockfile present", func(t *testing.T) {
		repo := writeFixtureRepo(t)
		checks := ProbeBuildDependencies(repo)
		var lockfile *Check
		for i := range checks {
			if checks[i].Name == "lockfile" {
				lockfile = &checks[i]
			}
		}
		if lockfile == nil || lockfile.Status != StatusPass {
			t.Fatalf("expected lockfile pass, got %#v", checks)
		}
	})

	t.Run("fail when web manifest has no lockfile", func(t *testing.T) {
		repo := writeFixtureRepo(t)
		if err := os.Remove(filepath.Join(repo, "web", "package-lock.json")); err != nil {
			t.Fatal(err)
		}
		checks := ProbeBuildDependencies(repo)
		found := false
		for _, c := range checks {
			if c.Name == "lockfile" && c.Status == StatusFail {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected lockfile fail, got %#v", checks)
		}
	})

	t.Run("skip on repo without manifests", func(t *testing.T) {
		repo := t.TempDir()
		checks := ProbeBuildDependencies(repo)
		for _, c := range checks {
			// build_tool_readiness passes vacuously when there is no gradle
			// server dir; lockfile/makefile checks skip without manifests.
			if c.Status != StatusSkip && !(c.Name == "build_tool_readiness" && c.Status == StatusPass) {
				t.Fatalf("expected skip (or vacuous pass) on empty repo, got %#v", checks)
			}
		}
	})

	t.Run("never seeds or mutates the repo", func(t *testing.T) {
		repo := writeFixtureRepo(t)
		if err := os.Remove(filepath.Join(repo, "web", "package-lock.json")); err != nil {
			t.Fatal(err)
		}
		_ = ProbeBuildDependencies(repo)
		if _, err := os.Stat(filepath.Join(repo, "web", "package-lock.json")); !os.IsNotExist(err) {
			t.Fatalf("probe must not seed lockfiles")
		}
	})
}
