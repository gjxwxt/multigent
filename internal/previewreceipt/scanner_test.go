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
