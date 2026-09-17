package previewreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// MaxPatchBytes limits turn patches to 2MB to protect against unbounded diffs.
	MaxPatchBytes = 2 * 1024 * 1024
)

// highRiskPathSubstrings are rejected outright in touched paths (deterministic blacklist):
// CI/CD, deploy, secrets, agent-identity files, and git internals.
var highRiskPathSubstrings = []string{
	".gitlab-ci", ".github/workflows", "jenkinsfile",
	"dockerfile", ".dockerignore",
	"deploy/", "deployment/", "k8s/", "helm/", "terraform/",
	".env", "credentials", "secret", "id_rsa", ".pem", ".p12", ".pfx", ".key",
	"agents.md", "claude.md", ".multigent/", ".git/",
}

// sensitivePatterns detects credentials, tokens, and private keys in added patch lines.
var sensitivePatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{
		name:    "private key header",
		pattern: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	},
	{
		name:    "OpenAI API key",
		pattern: regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`),
	},
	{
		name:    "GitHub personal access token",
		pattern: regexp.MustCompile(`gh[pousr]_[a-zA-Z0-9]{20,}`),
	},
	{
		name:    "Slack token",
		pattern: regexp.MustCompile(`xox[baprs]-[0-9a-zA-Z]{10,}`),
	},
	{
		name:    "AWS access key ID",
		pattern: regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	},
	{
		name:    "bearer token",
		pattern: regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9_\-\.]{25,}`),
	},
	{
		name:    "hardcoded secret or password assignment",
		pattern: regexp.MustCompile(`(?i)(password|passwd|secret|apikey|api_key)\s*(?::=|=|:)\s*["'][^"'\s]{8,}["']`),
	},
}

// ValidatePatchPaths checks all touched paths against the high-risk blacklist
// and rejects path traversal, root paths, or malformed entries.
func ValidatePatchPaths(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("patch touches no valid files")
	}
	for _, p := range paths {
		if isHighRiskPath(p) {
			return fmt.Errorf("path %q is high-risk and rejected by the turn security blacklist", p)
		}
	}
	return nil
}

func isHighRiskPath(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(filepath.ToSlash(strings.TrimSpace(path)), "\\", "/"))
	if normalized == "" {
		return true
	}
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, "..") {
		return true
	}
	for _, banned := range highRiskPathSubstrings {
		if strings.Contains(normalized, banned) {
			return true
		}
	}
	return false
}

// PatchTouchedPaths extracts all file paths touched by a unified diff.
// It inspects diff --git headers as well as --- and +++ lines to catch
// additions, deletions, modifications, and renames.
func PatchTouchedPaths(patch string) []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, "a/")
		p = strings.TrimPrefix(p, "b/")
		if p != "" && p != "/dev/null" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			add(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			add(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "diff --git "):
			body := strings.TrimPrefix(line, "diff --git ")
			oldPart, newPart, ok := strings.Cut(body, " ")
			if ok {
				add(oldPart)
				add(newPart)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// ScanSensitiveDiff inspects the added lines of a unified diff for sensitive
// patterns (e.g. API keys, private keys, passwords). Returns an error without
// echoing the sensitive value if a violation is detected.
func ScanSensitiveDiff(patch string) error {
	if len(patch) > MaxPatchBytes {
		return fmt.Errorf("patch size (%d bytes) exceeds maximum allowable limit (%d bytes)", len(patch), MaxPatchBytes)
	}

	lines := strings.Split(patch, "\n")
	for lineNum, line := range lines {
		// Only inspect additions (+ line), skipping the +++ header
		if strings.HasPrefix(line, "+++ ") || !strings.HasPrefix(line, "+") {
			continue
		}
		content := line[1:]
		for _, sp := range sensitivePatterns {
			if sp.pattern.MatchString(content) {
				return fmt.Errorf("patch line %d contains sensitive pattern (%s) — admission rejected", lineNum+1, sp.name)
			}
		}
	}
	return nil
}

// SanitizeDisplayDiff scrubs sensitive patterns from a patch for safe UI rendering.
func SanitizeDisplayDiff(patch string) string {
	sanitized := patch
	for _, sp := range sensitivePatterns {
		sanitized = sp.pattern.ReplaceAllString(sanitized, "[REDACTED_SECRET]")
	}
	return sanitized
}

// RedactSecrets scans arbitrary text (prompts, errors, comments) and redacts
// any sensitive credentials, tokens, API keys, or private keys.
func RedactSecrets(text string) string {
	if text == "" {
		return ""
	}
	res := text
	for _, sp := range sensitivePatterns {
		res = sp.pattern.ReplaceAllString(res, "[REDACTED_SECRET]")
	}
	return res
}

// ComputeRequestDigest computes an irreversible SHA-256 digest of a request payload.
func ComputeRequestDigest(payload string) string {
	h := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(h[:])
}
