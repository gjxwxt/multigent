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
		// Third-party QA toolkits put generated scripts under tests/<layer>/,
		// which this gate already accepts — see the rejects table for the part
		// of their output that does NOT fit.
		"external qa toolkit scripts": "tests/api/auth/login.spec.ts",
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
		// A dotted artifact root is NOT the allowlisted "qa" component: the dir
		// allowlist matches path components exactly, so ".qa-agent" misses.
		// Pinned because QA toolkits hardcode this root and their case corpus /
		// spec-tasks / reports are exactly what a QA step would try to declare.
		"dotted qa root cases":      ".qa-agent/cases/auth.json",
		"dotted qa root spec-tasks": ".qa-agent/spec-tasks/auth.json",
		"dotted qa root config":     ".qa-agent/config/project.json",
		"dotted qa root report":     ".qa-agent/reports/index.html",
		"dotted qa root local env":  ".qa-agent/local/accounts.local.json",
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

// TestValidateTouchedPathFormatUniversal (S2-2.3, review round P0): the
// universal half of the contract is format + forbidden surfaces, with NO
// QA-role judgment — a delivery branch declaring real business code must
// pass this (the delivery surface flag scopes the whitelist inside
// verifyDeclared), while forbidden surfaces stay rejected everywhere.
func TestValidateTouchedPathFormatUniversal(t *testing.T) {
	accepts := map[string]string{
		"none declaration":   "none",
		"business code":      "server/store/store.go",
		"business code html": "web/src/pages/LoginPage.tsx",
		"test artifact":      "server/store/store_test.go",
		"mixed delivery":     "server/store/store.go\nserver/store/store_test.go",
	}
	for name, raw := range accepts {
		if err := ValidateTouchedPathFormat(raw); err != nil {
			t.Fatalf("%s: expected accept, got %v", name, err)
		}
	}
	rejects := map[string]string{
		"empty declaration":   "",
		"gitlab ci":           ".gitlab-ci.yml",
		"deploy dir":          "deploy/prod/patch.yaml",
		"credentials":         "server/credentials.json",
		"agent instructions":  "AGENTS.md",
		"path escape":         "../shared/lib.go",
		"absolute path":       "/etc/passwd",
		"backslash separator": `server\store\store.go`,
	}
	for name, raw := range rejects {
		if err := ValidateTouchedPathFormat(raw); err == nil {
			t.Fatalf("%s: expected reject for %q", name, raw)
		}
	}
}

// TestDeliveryFormatDiffersFromQAWhitelist (S2-2.3): the SAME declaration
// splits the two checkpoint kinds — business code passes the universal
// format but fails the linear-QA whitelist. This is the semantic that the
// eager whitelist check in checkBranchQAGate used to break.
func TestDeliveryFormatDiffersFromQAWhitelist(t *testing.T) {
	const businessDecl = "server/store/store.go"
	if err := ValidateTouchedPathFormat(businessDecl); err != nil {
		t.Fatalf("delivery checkpoint must accept business code, got %v", err)
	}
	if err := ValidateQATouchedPaths(businessDecl); err == nil {
		t.Fatal("linear QA whitelist must still reject business code")
	}
}
