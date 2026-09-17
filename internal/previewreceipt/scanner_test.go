package previewreceipt

import (
	"strings"
	"testing"
)

func TestValidatePatchPaths(t *testing.T) {
	// Valid paths
	validPaths := []string{
		"web/src/App.tsx",
		"src/components/Header.jsx",
		"internal/api/routes.go",
		"style/main.css",
		"public/favicon.ico",
	}
	if err := ValidatePatchPaths(validPaths); err != nil {
		t.Fatalf("expected valid paths to pass, got error: %v", err)
	}

	// Empty paths
	if err := ValidatePatchPaths(nil); err == nil {
		t.Fatal("expected error for empty paths")
	}

	// High-risk and invalid paths
	bannedCases := []string{
		".gitlab-ci.yml",
		".github/workflows/ci.yml",
		"Dockerfile",
		"deploy/prod.yaml",
		".env",
		".env.production",
		"server/credentials.json",
		"id_rsa",
		"cert.pem",
		"key.p12",
		"AGENTS.md",
		"CLAUDE.md",
		".multigent/runtime.json",
		".git/config",
		"../escape.txt",
		"/root/file.txt",
		"",
	}

	for _, banned := range bannedCases {
		err := ValidatePatchPaths([]string{banned})
		if err == nil {
			t.Fatalf("expected path %q to be rejected, but it passed", banned)
		}
	}
}

func TestPatchTouchedPaths(t *testing.T) {
	patch := `diff --git a/web/src/App.tsx b/web/src/App.tsx
--- a/web/src/App.tsx
+++ b/web/src/App.tsx
@@ -1,3 +1,3 @@
-import React from 'react';
+import React, { useState } from 'react';
diff --git a/deleted.txt b/deleted.txt
--- a/deleted.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/new.txt b/new.txt
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
diff --git a/src/old.go b/src/renamed.go
similarity index 100%
rename from src/old.go
rename to src/renamed.go
`

	paths := PatchTouchedPaths(patch)
	expected := []string{
		"deleted.txt",
		"new.txt",
		"src/old.go",
		"src/renamed.go",
		"web/src/App.tsx",
	}

	if len(paths) != len(expected) {
		t.Fatalf("expected %d touched paths, got %d: %v", len(expected), len(paths), paths)
	}
	for i, p := range expected {
		if paths[i] != p {
			t.Fatalf("at index %d: expected %q, got %q", i, p, paths[i])
		}
	}
}

func TestScanSensitiveDiff(t *testing.T) {
	// Clean patch
	cleanPatch := `diff --git a/app.go b/app.go
--- a/app.go
+++ b/app.go
@@ -1 +1 @@
-var port = 8080
+var port = 8081
`
	if err := ScanSensitiveDiff(cleanPatch); err != nil {
		t.Fatalf("expected clean patch to pass, got: %v", err)
	}

	// Sensitive cases
	sensitiveCases := []struct {
		name  string
		patch string
	}{
		{
			name: "OpenAI API key",
			patch: `diff --git a/api.go b/api.go
+++ b/api.go
+apiKey := "sk-proj-abc12345678901234567890"
`,
		},
		{
			name: "GitHub personal access token",
			patch: `diff --git a/api.go b/api.go
+++ b/api.go
+token = "ghp_123456789012345678901234567890"
`,
		},
		{
			name: "Private Key",
			patch: `diff --git a/key.txt b/key.txt
+++ b/key.txt
+-----BEGIN RSA PRIVATE KEY-----
+MIIEowIBAAKCAQEA...
`,
		},
		{
			name: "Hardcoded password assignment",
			patch: `diff --git a/config.go b/config.go
+++ b/config.go
+password := "supersecret123"
`,
		},
		{
			name: "Bearer token",
			patch: `diff --git a/auth.go b/auth.go
+++ b/auth.go
+req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...")
`,
		},
		{
			name: "JWT token standalone",
			patch: `diff --git a/jwt.go b/jwt.go
+++ b/jwt.go
+jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.cTJj98m..."
`,
		},
		{
			name: "Basic auth header",
			patch: `diff --git a/auth.go b/auth.go
+++ b/auth.go
+Authorization: Basic dXNlcjpwYXNzMTIzNDU2
`,
		},
		{
			name: "Unquoted secret assignment",
			patch: `diff --git a/env.go b/env.go
+++ b/env.go
+api_key = supersecrettokenvalue123
`,
		},
		{
			name: "GitLab PAT",
			patch: `diff --git a/ci.go b/ci.go
+++ b/ci.go
+glpat-abcdefghijklmnop123
`,
		},
	}

	for _, sc := range sensitiveCases {
		err := ScanSensitiveDiff(sc.patch)
		if err == nil {
			t.Fatalf("expected case %q to fail sensitive scan, but it passed", sc.name)
		}
	}

	// Max size limit
	oversized := strings.Repeat("+a\n", (MaxPatchBytes/3)+10)
	if err := ScanSensitiveDiff(oversized); err == nil {
		t.Fatal("expected oversized patch to be rejected")
	}
}

func TestSanitizeDisplayDiff(t *testing.T) {
	patch := `+apiKey := "sk-1234567890123456789012345"`
	sanitized := SanitizeDisplayDiff(patch)
	if strings.Contains(sanitized, "sk-1234567890123456789012345") {
		t.Fatalf("expected secret to be redacted, got: %s", sanitized)
	}
	if !strings.Contains(sanitized, "[REDACTED_SECRET]") {
		t.Fatalf("expected [REDACTED_SECRET] placeholder, got: %s", sanitized)
	}
}

func TestRedactSecretsAndCapAndRedact(t *testing.T) {
	cases := []struct {
		input       string
		mustNotHave string
		mustHave    string
	}{
		{
			input:       "Here is the token: ghp_123456789012345678901234567890 in log",
			mustNotHave: "ghp_123456789012345678901234567890",
			mustHave:    "[REDACTED_SECRET]",
		},
		{
			input:       "Use OpenAI key sk-1234567890123456789012345 here",
			mustNotHave: "sk-1234567890123456789012345",
			mustHave:    "[REDACTED_SECRET]",
		},
		{
			input:       "Auth: Authorization: Basic dXNlcjpwYXNzMTIzNDU2",
			mustNotHave: "dXNlcjpwYXNzMTIzNDU2",
			mustHave:    "[REDACTED_SECRET]",
		},
		{
			input:       "Proxy-Authorization: Basic dXNlcjpwYXNzMTIzNDU2",
			mustNotHave: "dXNlcjpwYXNzMTIzNDU2",
			mustHave:    "[REDACTED_SECRET]",
		},
		{
			input:       "Config has api_key = supersecretvalue123 and password: myhiddenpassword123",
			mustNotHave: "supersecretvalue123",
			mustHave:    "[REDACTED_SECRET]",
		},
		{
			input:       "Token is eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.cTJj98m45678901234567890",
			mustNotHave: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.cTJj98m45678901234567890",
			mustHave:    "[REDACTED_SECRET]",
		},
		{
			input:       "Gitlab token glpat-12345678901234567890 in config",
			mustNotHave: "glpat-12345678901234567890",
			mustHave:    "[REDACTED_SECRET]",
		},
	}

	for _, c := range cases {
		redacted := RedactSecrets(c.input)
		if strings.Contains(redacted, c.mustNotHave) {
			t.Fatalf("RedactSecrets leaked %q: %s", c.mustNotHave, redacted)
		}
		if !strings.Contains(redacted, c.mustHave) {
			t.Fatalf("RedactSecrets missing %q: %s", c.mustHave, redacted)
		}
	}

	// Test CapAndRedact length capping
	longInput := "test " + strings.Repeat("A", 5000)
	capped := CapAndRedact(longInput, 100)
	if len(capped) > 150 {
		t.Fatalf("expected capped output, got len %d", len(capped))
	}
	if !strings.Contains(capped, "… [truncated]") {
		t.Fatalf("expected truncation marker, got: %s", capped)
	}
}

