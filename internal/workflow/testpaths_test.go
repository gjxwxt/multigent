package workflow

import "testing"

func TestValidateQATouchedPathsAccepts(t *testing.T) {
	cases := map[string]string{
		"none declaration":           "none",
		"NONE case-insensitive":      "NONE",
		"go test file":               "server/store/store_test.go",
		"ts spec":                    "web/src/pages/LoginPage.test.tsx",
		"jest dir":                   "web/src/__tests__/login.tsx",
		"testdata":                   "server/store/testdata/orders.json",
		"fixtures dir":               "web/cypress/fixtures/users.json",
		"e2e":                        "web/e2e/checkout.spec.ts",
		"python test":                "server/tests/test_api.py",
		"probe script":               "qa/probes/health_check.sh",
		"rust tests":                 "server/src/tests.rs",
		"multiple lines with blanks": "server/store/store_test.go\n\nweb/e2e/checkout.spec.ts\n",
		"go external test package":   "store_test/helpers_test.go",
	}
	for name, raw := range cases {
		if err := ValidateQATouchedPaths(raw); err != nil {
			t.Fatalf("%s: expected accept, got %v", name, err)
		}
	}
}

func TestValidateQATouchedPathsRejects(t *testing.T) {
	cases := map[string]string{
		"empty declaration":    "",
		"business code":        "server/store/store.go",
		"business code nested": "web/src/components/Login.tsx",
		"gitlab ci":            ".gitlab-ci.yml",
		"github workflow":      ".github/workflows/build.yml",
		"dockerfile":           "Dockerfile",
		"dockerfile nested":    "server/Dockerfile",
		"deploy dir":           "deploy/prod/patch.yaml",
		"env file":             "server/.env.production",
		"credentials":          "server/credentials.json",
		"pem key":              "certs/tls.pem",
		"agent instructions":   "AGENTS.md",
		"multigent contract":   ".multigent/fixtures.json",
		"git internals":        ".git/hooks/pre-commit",
		"path escape":          "../shared/lib.go",
		"absolute path":        "/etc/passwd",
		"backslash separator":  `server\store\store_test.go`,
		"only blank lines":     "\n\n",
	}
	for name, raw := range cases {
		if err := ValidateQATouchedPaths(raw); err == nil {
			t.Fatalf("%s: expected reject for %q", name, raw)
		}
	}
}

func TestValidateQATouchedPathsReportsLineNumbers(t *testing.T) {
	raw := "server/store/store_test.go\nserver/store/store.go"
	err := ValidateQATouchedPaths(raw)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !contains(err.Error(), "line 2") {
		t.Fatalf("error must point at the offending line: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
