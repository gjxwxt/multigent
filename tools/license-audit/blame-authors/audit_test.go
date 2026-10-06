package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The cross-check pairs come from the remediation verdict
// (docs/license-remediation-final-v3.md §1, all measured):
//   - internal/db is the known-polluted control: 14,388 lines, 6,590
//     upstream (chenhg5) at the v3 baseline, upstream ratio 45.8%.
//   - internal/projecttemplate is the known-clean control: upstream = 0.
//
// The v3 numbers were measured at commit 70cfa058; this repo has moved on,
// so the test pins the file/line shape of the CURRENT HEAD instead and
// asserts the structural invariants the remediation actually depends on:
//
//  1. polluted control reports upstream > 0 AND its author breakdown
//     matches the HEAD-measured census exactly (regression-pins the parser);
//  2. clean control reports upstream == 0 (zero false positives);
//  3. verdict polarity is preserved: same audit run, polluted fails,
//     clean passes (criterion discipline: >0, never a ratio).
//
// If any of these drift, someone changed either the blame parsing or the
// packages themselves — both require re-freezing the audit baseline before
// any batch verdict may cite the numbers.
func TestAuditCrossCheckKnownPollutedDB(t *testing.T) {
	out, err := runAudit(t, "internal/db")
	if err != nil {
		t.Fatalf("audit internal/db: %v\n%s", err, out)
	}
	rep := parseSummary(t, out)

	if rep.Self == 0 {
		t.Fatalf("internal/db should have self lines, got 0: %s", out)
	}
	if rep.Upstream == 0 {
		t.Fatalf("internal/db is the known-polluted control; upstream must be > 0 (v3 §1: 45.8%% chenhg5)")
	}
	if rep.Buckets["chenhg5"] != rep.Upstream {
		t.Fatalf("internal/db upstream (%d) must be entirely chenhg5 (%d) per HEAD census",
			rep.Upstream, rep.Buckets["chenhg5"])
	}
	if rep.Total != 14388 {
		// HEAD census: 62 files / 14,388 lines. If this moves, internal/db
		// changed and the v3 §1 table needs a re-measure (recorded in the
		// audit report, not silently accepted).
		t.Fatalf("internal/db total lines moved from 14,388 to %d — re-freeze the audit baseline", rep.Total)
	}
	if rep.Upstream != 6590 {
		t.Fatalf("internal/db upstream lines moved from 6,590 to %d — re-freeze the audit baseline", rep.Upstream)
	}
}

func TestAuditCrossCheckKnownCleanProjectTemplate(t *testing.T) {
	out, err := runAudit(t, "internal/projecttemplate")
	if err != nil {
		t.Fatalf("audit internal/projecttemplate: %v\n%s", err, out)
	}
	rep := parseSummary(t, out)

	if rep.Upstream != 0 {
		t.Fatalf("internal/projecttemplate is the known-clean control; upstream must be 0, got %d", rep.Upstream)
	}
	if rep.Total == 0 {
		t.Fatalf("internal/projecttemplate audited to 0 lines — audit ran on nothing")
	}
	if rep.Self != rep.Total {
		t.Fatalf("clean control must be 100%% self: self=%d total=%d", rep.Self, rep.Total)
	}
}

// TestVerifyCleanGatePolarity runs the verify-clean gate on both controls
// and asserts the exit codes: clean package passes (0), polluted fails (1).
func TestVerifyCleanGatePolarity(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", "..", ".."))

	t.Run("clean package passes", func(t *testing.T) {
		cmd := exec.Command("go", "run", "./tools/license-audit/verify-clean", "-repo", repoRoot, "internal/projecttemplate")
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("verify-clean should pass on internal/projecttemplate: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "PASS") {
			t.Fatalf("expected PASS in output, got: %s", out)
		}
	})

	t.Run("polluted package fails", func(t *testing.T) {
		cmd := exec.Command("go", "run", "./tools/license-audit/verify-clean", "-repo", repoRoot, "internal/db")
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("verify-clean should FAIL on internal/db, but exited 0:\n%s", out)
		}
		if !strings.Contains(string(out), "FAIL") {
			t.Fatalf("expected FAIL in output, got: %s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("polluted gate must exit 1, got: %v", err)
		}
	})
}

// TestClosureScanContaminationPaths checks the closure tool on the two
// documented criterion-discipline cases:
//   - internal/changerun and internal/fixturesandbox are self-clean but
//     depend on internal/db (upstream > 0) — they must be reported
//     contaminated VIA the db chain (v3 §3: quasi-clean eats the facade);
//   - strict-clean packages (gitworktree, projecttemplate, codehost,
//     doctor, ciready) must stay closure-clean.
func TestClosureScanContaminationPaths(t *testing.T) {
	out, err := runClosureScan(t, "internal/changerun", "internal/fixturesandbox", "internal/gitworktree", "internal/projecttemplate", "internal/codehost", "internal/doctor", "internal/ciready")
	if err != nil {
		t.Fatalf("closure-scan: %v\n%s", err, out)
	}
	pkgs := parseClosureReport(t, out)

	module := "github.com/multigent/multigent"
	mustContaminated := []string{
		module + "/internal/changerun",
		module + "/internal/fixturesandbox",
	}
	for _, ip := range mustContaminated {
		r, ok := pkgs[ip]
		if !ok {
			t.Fatalf("package %s missing from closure report", ip)
		}
		if r.Clean {
			t.Fatalf("%s imports internal/db (upstream>0); closure must mark it contaminated", ip)
		}
		if len(r.ContaminatedVia) == 0 {
			t.Fatalf("%s contaminated without a via-chain", ip)
		}
		last := r.ContaminatedVia[len(r.ContaminatedVia)-1]
		if last != module+"/internal/db" {
			t.Fatalf("%s chain should end at internal/db, got %v", ip, r.ContaminatedVia)
		}
	}

	mustClean := []string{
		module + "/internal/gitworktree",
		module + "/internal/projecttemplate",
		module + "/internal/codehost",
		module + "/internal/doctor",
		module + "/internal/ciready",
	}
	for _, ip := range mustClean {
		r, ok := pkgs[ip]
		if !ok {
			t.Fatalf("package %s missing from closure report", ip)
		}
		if !r.Clean {
			t.Fatalf("%s is strict-clean (v3 §3); closure must keep it clean, got via=%v", ip, r.ContaminatedVia)
		}
	}
}

// TestBlameParserPorcelainBinding pins the porcelain parsing contract that
// the whole audit stands on: sha->author bindings are CACHED by git (metadata
// lines only appear on first sight of a sha), so a parser that re-reads the
// last "author " line per content line double-counts. We feed a crafted
// blame output where one sha's metadata appears once but its content lines
// appear in two separate groups.
func TestBlameParserPorcelainBinding(t *testing.T) {
	sha := "498b18a9290b29b0e81a37c4934cd7a3673b82d3"
	fake := strings.Join([]string{
		sha + " 1 1 3",
		"author chenhg5",
		"author-mail <chg80333@gmail.com>",
		"\tpackage secretbox",
		sha + " 2 2",
		"\t",
		sha + " 3 3",
		"\timport (",
		"49b4752918b7c69a332b20398d620117456ae9dc 4 4 1",
		"author gaojiaxiang",
		"author-mail <gaojiaxiang2018@gmail.com>",
		"\tvar x = 1",
	}, "\n")

	fr := parseBlamePorcelain("fake.go", map[string]string{
		"chenhg5":     "chenhg5",
		"gaojiaxiang": "gaojiaxiang",
	}, fake)

	if fr.Lines != 4 {
		t.Fatalf("expected 4 content lines, got %d", fr.Lines)
	}
	if fr.Authors["chenhg5"] != 3 {
		t.Fatalf("cached sha binding wrong: chenhg5=%d, want 3 (metadata appears once, applies to lines 1-3)", fr.Authors["chenhg5"])
	}
	if fr.Authors["gaojiaxiang"] != 1 {
		t.Fatalf("gaojiaxiang=%d, want 1", fr.Authors["gaojiaxiang"])
	}
	if fr.UpstreamLines != 3 {
		t.Fatalf("upstream=%d, want 3", fr.UpstreamLines)
	}
}

// TestBlameParserUncommittedLines pins the uncommitted-line bucketing: a
// zero-sha block must count as uncommitted (its own bucket, upstream for
// the gate), never attributed to a real author.
func TestBlameParserUncommittedLines(t *testing.T) {
	zero := "0000000000000000000000000000000000000000"
	fake := strings.Join([]string{
		zero + " 1 1 1",
		"\twork in progress",
	}, "\n")

	fr := parseBlamePorcelain("wip.go", map[string]string{}, fake)
	if fr.Lines != 1 || fr.Authors["uncommitted"] != 1 {
		t.Fatalf("uncommitted line must bucket as uncommitted: %+v", fr.Authors)
	}
	if fr.UpstreamLines != 1 {
		t.Fatalf("uncommitted counts as upstream for the gate, got %d", fr.UpstreamLines)
	}
}

type auditSummary struct {
	Total    int            `json:"total_lines"`
	Self     int            `json:"self_lines"`
	Upstream int            `json:"upstream_lines"`
	Buckets  map[string]int `json:"buckets"`
}

func runAudit(t *testing.T, pkg string) (string, error) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", "..", ".."))
	cmd := exec.Command("go", "run", "./tools/license-audit/blame-authors", "-repo", repoRoot, "-q", pkg)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	return string(out), err
}

func parseSummary(t *testing.T, out string) auditSummary {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in audit output: %s", out)
	}
	var raw struct {
		Summary auditSummary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &raw); err != nil {
		t.Fatalf("parse audit JSON: %v\n%s", err, out)
	}
	return raw.Summary
}

type closurePkg struct {
	Package         string   `json:"package"`
	Clean           bool     `json:"clean"`
	ContaminatedVia []string `json:"contaminated_via"`
}

func runClosureScan(t *testing.T, pkgs ...string) (string, error) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", "..", ".."))
	args := append([]string{"run", "./tools/license-audit/closure-scan", "-repo", repoRoot, "-q"}, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	// Exit code 1 is expected when any requested package is contaminated —
	// the JSON report is still on stdout and is what we assert on.
	if err != nil && !strings.Contains(err.Error(), "exit status 1") {
		return string(out), err
	}
	return string(out), nil
}

func parseClosureReport(t *testing.T, out string) map[string]closurePkg {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in closure output: %s", out)
	}
	var raw struct {
		Packages []closurePkg `json:"packages"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &raw); err != nil {
		t.Fatalf("parse closure JSON: %v\n%s", err, out)
	}
	m := map[string]closurePkg{}
	for _, p := range raw.Packages {
		m[p.Package] = p
	}
	return m
}
