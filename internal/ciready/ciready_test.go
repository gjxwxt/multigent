package ciready

import (
	"os"
	"path/filepath"
	"testing"
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
