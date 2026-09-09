// Package ciready implements the deterministic CI/CD readiness gate used by
// project initialization. All logic is pure verification plus idempotent
// seeding of the platform CI baseline; no agent judgement is involved.
package ciready

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/multigent/multigent/internal/projecttemplate"
)

const (
	StatusPass = "pass"
	StatusFail = "fail"
	StatusSkip = "skip"

	OverallReady    = "ready"
	OverallNotReady = "not_ready"
)

// Check is one deterministic verification item.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the structured result consumed by the runtime endpoint and the
// `mga ci ready` command.
type Report struct {
	Repo    string  `json:"repo"`
	Seeded  []string `json:"seeded,omitempty"`
	Checks  []Check  `json:"checks"`
	Overall string   `json:"overall"`
}

// requiredJobs are the job ids the CI baseline contract guarantees.
var requiredJobs = []string{"lint:backend", "test:backend", "build:frontend", "build:backend", "package", "deploy"}

// Ensure seeds any missing CI baseline files (never overwriting existing
// ones) and then runs the full deterministic check.
func Ensure(repoDir string) (Report, error) {
	seeded, err := seedMissing(repoDir)
	if err != nil {
		return Report{}, err
	}
	report := Verify(repoDir)
	report.Seeded = seeded
	return report, nil
}

func seedMissing(repoDir string) ([]string, error) {
	templateID := detectTemplateID(repoDir)
	files, err := projecttemplate.CIBaselineFilesForTemplate(templateID)
	if err != nil {
		return nil, err
	}
	var seeded []string
	for _, path := range sortedKeys(files) {
		target := filepath.Join(repoDir, filepath.FromSlash(path))
		if _, err := os.Lstat(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, files[path], 0o644); err != nil {
			return nil, fmt.Errorf("seed %s: %w", path, err)
		}
		seeded = append(seeded, path)
	}
	return seeded, nil
}

func detectTemplateID(repoDir string) string {
	if raw, err := os.ReadFile(filepath.Join(repoDir, ".multigent", "runtime.json")); err == nil {
		var rt struct {
			TemplateID string `json:"templateId"`
			Backend    struct {
				Command string `json:"command"`
			} `json:"backend"`
		}
		if err := json.Unmarshal(raw, &rt); err == nil {
			if strings.TrimSpace(rt.TemplateID) != "" {
				return strings.TrimSpace(rt.TemplateID)
			}
			if strings.Contains(rt.Backend.Command, "gradle") || strings.Contains(rt.Backend.Command, "mvn") || strings.Contains(rt.Backend.Command, "java") {
				return projecttemplate.ReactSpringBootID
			}
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "server", "build.gradle")); err == nil {
		return projecttemplate.ReactSpringBootID
	}
	if _, err := os.Stat(filepath.Join(repoDir, "server", "pom.xml")); err == nil {
		return projecttemplate.ReactSpringBootID
	}
	return projecttemplate.ReactGoFullstackID
}

// Verify runs every deterministic verification item against a repository.
func Verify(repoDir string) Report {
	report := Report{Repo: repoDir}
	add := func(name, status, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail})
	}

	raw, err := os.ReadFile(filepath.Join(repoDir, ".gitlab-ci.yml"))
	var missing []string
	if err != nil {
		missing = append(missing, ".gitlab-ci.yml")
	}
	for _, path := range []string{"deploy/Dockerfile", "deploy/compose.yml"} {
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(path))); err != nil {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		add("baseline_files", StatusFail, "missing: "+strings.Join(missing, ", "))
	} else {
		add("baseline_files", StatusPass, "")
	}
	checkPrerequisites(repoDir, add)
	if err != nil {
		report.Overall = OverallNotReady
		return report
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		add("ci_yaml_parses", StatusFail, err.Error())
		report.Overall = OverallNotReady
		return report
	}
	add("ci_yaml_parses", StatusPass, "")

	jobs := ciJobs(doc)

	var absent []string
	for _, id := range requiredJobs {
		if _, ok := jobs[id]; !ok {
			absent = append(absent, id)
		}
	}
	if len(absent) > 0 {
		add("required_jobs", StatusFail, "missing jobs: "+strings.Join(absent, ", "))
	} else {
		add("required_jobs", StatusPass, "")
	}

	var untagged []string
	for id, spec := range jobs {
		if !jobHasTag(resolveSpec(doc, spec), "docker") {
			untagged = append(untagged, id)
		}
	}
	sort.Strings(untagged)
	if len(untagged) > 0 {
		add("runner_tags", StatusFail, "jobs without tags [docker]: "+strings.Join(untagged, ", "))
	} else {
		add("runner_tags", StatusPass, "")
	}

	var unguarded []string
	for _, id := range []string{"package", "deploy"} {
		spec, ok := jobs[id]
		if !ok || !ruleGatedOnTag(spec) {
			unguarded = append(unguarded, id)
		}
	}
	if len(unguarded) > 0 {
		add("tag_only_release", StatusFail, "release jobs must be gated on $CI_COMMIT_TAG: "+strings.Join(unguarded, ", "))
	} else {
		add("tag_only_release", StatusPass, "")
	}

	code := strings.Join(codeLines(string(raw)), "\n")
	if strings.Contains(code, "registry.npmmirror.com") {
		add("npm_mirror", StatusPass, "")
	} else {
		add("npm_mirror", StatusFail, "frontend jobs must install through registry.npmmirror.com (cold npm ci is 5-7min otherwise)")
	}

	var apkLines []string
	for _, line := range strings.Split(code, "\n") {
		if strings.Contains(line, "apk add") {
			apkLines = append(apkLines, line)
		}
	}
	switch {
	case len(apkLines) == 0:
		add("apk_cache", StatusSkip, "no apk usage")
	case anyContains(apkLines, "--no-cache"):
		add("apk_cache", StatusFail, "apk add --no-cache re-downloads docker CLI on every job (use --cache-dir /cache/apk)")
	case allContain(apkLines, "--cache-dir"):
		add("apk_cache", StatusPass, "")
	default:
		add("apk_cache", StatusFail, "apk add must use --cache-dir /cache/apk so the runner host volume is reused")
	}

	if def, ok := doc["default"].(map[string]any); ok && fmt.Sprint(def["interruptible"]) == "true" {
		add("interruptible", StatusPass, "")
	} else {
		add("interruptible", StatusFail, "default.interruptible must be true")
	}

	checkFrontendScripts(repoDir, jobs, add)
	checkHealthPath(repoDir, add)

	report.Overall = OverallReady
	for _, c := range report.Checks {
		if c.Status == StatusFail {
			report.Overall = OverallNotReady
			break
		}
	}
	return report
}

// ciJobs extracts job definitions, skipping reserved top-level keys and
// hidden templates (".frontend" style anchors are still validated via extends
// indirectly through the raw-text checks above).
func ciJobs(doc map[string]any) map[string]map[string]any {
	reserved := map[string]bool{"workflow": true, "default": true, "variables": true, "stages": true}
	jobs := map[string]map[string]any{}
	for key, value := range doc {
		if reserved[key] || strings.HasPrefix(key, ".") {
			continue
		}
		if spec, ok := value.(map[string]any); ok {
			jobs[key] = spec
		}
	}
	return jobs
}

// resolveSpec merges the templates named by `extends` under the job spec
// (job keys win, later extends entries win over earlier ones) so inherited
// fields like tags are visible to the checks.
func resolveSpec(doc map[string]any, spec map[string]any) map[string]any {
	extends, ok := spec["extends"]
	if !ok {
		return spec
	}
	var names []string
	switch value := extends.(type) {
	case string:
		names = []string{value}
	case []any:
		for _, item := range value {
			names = append(names, fmt.Sprint(item))
		}
	}
	merged := map[string]any{}
	for _, name := range names {
		if tpl, ok := doc[name].(map[string]any); ok {
			for k, v := range resolveSpec(doc, tpl) {
				merged[k] = v
			}
		}
	}
	for k, v := range spec {
		merged[k] = v
	}
	return merged
}

// codeLines drops comment-only lines so keyword scans never match prose.
func codeLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func anyContains(lines []string, needle string) bool {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func allContain(lines []string, needle string) bool {
	for _, line := range lines {
		if !strings.Contains(line, needle) {
			return false
		}
	}
	return true
}

func jobHasTag(spec map[string]any, tag string) bool {
	tags, ok := spec["tags"].([]any)
	if !ok {
		return false
	}
	for _, item := range tags {
		if strings.TrimSpace(fmt.Sprint(item)) == tag {
			return true
		}
	}
	return false
}

func ruleGatedOnTag(spec map[string]any) bool {
	rules, ok := spec["rules"].([]any)
	if !ok {
		return false
	}
	for _, item := range rules {
		rule, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.Contains(fmt.Sprint(rule["if"]), "$CI_COMMIT_TAG") {
			return true
		}
	}
	return false
}

var npmRunPattern = regexp.MustCompile(`npm run ([A-Za-z0-9:_-]+)`)

func checkFrontendScripts(repoDir string, jobs map[string]map[string]any, add func(string, string, string)) {
	manifestPath := filepath.Join(repoDir, "web", "package.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		add("frontend_scripts", StatusSkip, "web/package.json not found")
		return
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		add("frontend_scripts", StatusFail, "web/package.json is not valid JSON: "+err.Error())
		return
	}
	var unknown []string
	for _, id := range sortedJobIDs(jobs) {
		for _, line := range scriptLines(jobs[id]) {
			for _, match := range npmRunPattern.FindAllStringSubmatch(line, -1) {
				if _, ok := manifest.Scripts[match[1]]; !ok {
					unknown = append(unknown, id+" -> npm run "+match[1])
				}
			}
		}
	}
	if len(unknown) > 0 {
		add("frontend_scripts", StatusFail, "scripts referenced by CI but absent from web/package.json: "+strings.Join(unknown, ", "))
	} else {
		add("frontend_scripts", StatusPass, "")
	}
}

func checkHealthPath(repoDir string, add func(string, string, string)) {
	runtimeRaw, err := os.ReadFile(filepath.Join(repoDir, ".multigent", "runtime.json"))
	if err != nil {
		add("health_path", StatusSkip, ".multigent/runtime.json not found")
		return
	}
	composeRaw, err := os.ReadFile(filepath.Join(repoDir, "deploy", "compose.yml"))
	if err != nil {
		add("health_path", StatusSkip, "deploy/compose.yml not found")
		return
	}
	var runtime struct {
		Backend struct {
			HealthPath string `json:"healthPath"`
		} `json:"backend"`
	}
	if err := json.Unmarshal(runtimeRaw, &runtime); err != nil || strings.TrimSpace(runtime.Backend.HealthPath) == "" {
		add("health_path", StatusSkip, "no backend healthPath declared")
		return
	}
	if strings.Contains(string(composeRaw), runtime.Backend.HealthPath) {
		add("health_path", StatusPass, "")
	} else {
		add("health_path", StatusFail, "deploy/compose.yml healthcheck does not probe the runtime contract path "+runtime.Backend.HealthPath)
	}
}

func checkPrerequisites(repoDir string, add func(string, string, string)) {
	if _, err := os.Stat(filepath.Join(repoDir, "web", "package.json")); err == nil {
		if _, err := os.Stat(filepath.Join(repoDir, "web", "package-lock.json")); err != nil {
			add("lockfile", StatusFail, "web/package-lock.json is required for deterministic npm ci")
		} else {
			add("lockfile", StatusPass, "")
		}
	} else {
		add("lockfile", StatusSkip, "no web/package.json")
	}

	if _, err := os.Stat(filepath.Join(repoDir, "server", "build.gradle")); err == nil {
		var gradleIssues []string
		wrapperJar := filepath.Join(repoDir, "server", "gradle", "wrapper", "gradle-wrapper.jar")
		if _, err := os.Stat(wrapperJar); err != nil {
			gradleIssues = append(gradleIssues, "server/gradle/wrapper/gradle-wrapper.jar is missing")
		}
		gradlewPath := filepath.Join(repoDir, "server", "gradlew")
		if info, err := os.Stat(gradlewPath); err != nil {
			gradleIssues = append(gradleIssues, "server/gradlew is missing")
		} else if info.Mode()&0111 == 0 {
			gradleIssues = append(gradleIssues, "server/gradlew is not executable")
		}
		if len(gradleIssues) > 0 {
			add("build_tool_readiness", StatusFail, strings.Join(gradleIssues, "; "))
		} else {
			add("build_tool_readiness", StatusPass, "")
		}
	} else {
		add("build_tool_readiness", StatusPass, "")
	}

	makefilePath := filepath.Join(repoDir, "Makefile")
	if raw, err := os.ReadFile(makefilePath); err == nil {
		content := string(raw)
		var missingTargets []string
		for _, target := range []string{"install", "verify"} {
			pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `\s*:`)
			if !pattern.MatchString(content) {
				missingTargets = append(missingTargets, target)
			}
		}
		if len(missingTargets) > 0 {
			add("makefile_targets", StatusFail, "Makefile missing required targets: "+strings.Join(missingTargets, ", "))
		} else {
			add("makefile_targets", StatusPass, "")
		}
	} else {
		add("makefile_targets", StatusSkip, "no Makefile")
	}
}

func scriptLines(spec map[string]any) []string {
	var lines []string
	switch value := spec["script"].(type) {
	case []any:
		for _, item := range value {
			lines = append(lines, fmt.Sprint(item))
		}
	case string:
		lines = append(lines, value)
	}
	return lines
}

func sortedJobIDs(jobs map[string]map[string]any) []string {
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
