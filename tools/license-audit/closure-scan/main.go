// closure-scan is the license-remediation dependency-closure tool
// (B1, execution-plan §4; v3 §3 criterion discipline #2: per-file/per-package
// blame verdicts must never be extrapolated to the import closure).
//
// It parses the Go import graph of the target repository, runs the blame
// census (same parsing rules as blame-authors), and reports every package
// that transitively depends on a package with upstream lines > 0 — the
// pollution source set. A "clean" package that imports a polluted one is
// itself marked contaminated, with the shortest chain shown.
//
// Usage:
//
//	go run ./tools/license-audit/closure-scan [-repo DIR] [-o OUT.json] [PKG...]
//
// Without PKG arguments the whole module is scanned. Exit code is 0 when
// every requested package is closure-clean, 1 otherwise (usable as a batch
// gate), 2 on tooling error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type pkgInfo struct {
	importPath string
	dir        string // repo-relative
	imports    []string
	// upstreamLines / totalLines come from the blame census.
	upstreamLines int
	totalLines    int
}

type pkgReport struct {
	Package       string   `json:"package"`
	TotalLines    int      `json:"total_lines"`
	UpstreamLines int      `json:"upstream_lines"`
	// DirectPolluted lists this package's own imports that are polluted.
	DirectPolluted []string `json:"direct_polluted_deps,omitempty"`
	// ContaminatedVia lists the shortest dependency path from this package
	// to some polluted package (polluted package last, self first).
	ContaminatedVia []string `json:"contaminated_via,omitempty"`
	Clean           bool     `json:"clean"`
}

type scanReport struct {
	Repo     string      `json:"repo"`
	Module   string      `json:"module"`
	Packages []pkgReport `json:"packages"`
	// Polluted is the source set: packages with upstream lines > 0 by the
	// self criterion (v3 §3 discipline #1: >0, never a ratio).
	Polluted []string       `json:"polluted_packages"`
	Summary  map[string]any `json:"summary"`
}

func main() {
	repo := flag.String("repo", ".", "repository root")
	out := flag.String("o", "", "write JSON report to this file (default stdout)")
	quiet := flag.Bool("q", false, "suppress progress output on stderr")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: closure-scan [-repo DIR] [-o OUT.json] [PKG...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	clean, err := run(*repo, *out, *quiet, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "closure-scan: %v\n", err)
		os.Exit(2)
	}
	if !clean {
		os.Exit(1)
	}
}

func run(repoDir, outFile string, quiet bool, want []string) (bool, error) {
	repoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return false, err
	}
	module, err := goModulePath(repoDir)
	if err != nil {
		return false, err
	}

	// 1. Blame census per tracked directory.
	blame, err := blameCensus(repoDir, quiet)
	if err != nil {
		return false, err
	}

	// 2. Parse every in-module package's imports.
	pkgs, err := parsePackages(repoDir, module, blame, quiet)
	if err != nil {
		return false, err
	}

	// 3. Polluted set: any package with upstream lines > 0.
	polluted := map[string]bool{}
	for ip, p := range pkgs {
		if p.upstreamLines > 0 {
			polluted[ip] = true
		}
	}

	// 4. Transitive contamination with shortest-chain evidence.
	names := make([]string, 0, len(pkgs))
	for ip := range pkgs {
		names = append(names, ip)
	}
	sort.Strings(names)

	reports := make([]pkgReport, 0, len(pkgs))
	for _, ip := range names {
		p := pkgs[ip]
		r := pkgReport{
			Package:       ip,
			TotalLines:    p.totalLines,
			UpstreamLines: p.upstreamLines,
		}
		direct := []string{}
		for _, im := range p.imports {
			if polluted[im] {
				direct = append(direct, im)
			}
		}
		sort.Strings(direct)
		r.DirectPolluted = direct

		if chain, ok := shortestPollutedChain(ip, pkgs, polluted); ok {
			r.Clean = false
			r.ContaminatedVia = chain
		} else {
			r.Clean = true
		}
		reports = append(reports, r)
	}

	pollutedNames := make([]string, 0)
	for ip := range polluted {
		pollutedNames = append(pollutedNames, ip)
	}
	sort.Strings(pollutedNames)

	// 5. Exit gate: with explicit PKG arguments only they gate; otherwise
	// the whole module must be closure-clean.
	gateClean := true
	if len(want) > 0 {
		gate := map[string]bool{}
		for _, w := range want {
			gate[normalizeImport(module, w)] = true
		}
		for _, r := range reports {
			if gate[r.Package] && !r.Clean {
				gateClean = false
			}
		}
	} else {
		for _, r := range reports {
			if !r.Clean {
				gateClean = false
				break
			}
		}
	}

	report := scanReport{
		Repo:     repoDir,
		Module:   module,
		Packages: reports,
		Polluted: pollutedNames,
		Summary: map[string]any{
			"packages_scanned":   len(pkgs),
			"polluted_packages":  len(pollutedNames),
			"contaminated_total": countContaminated(reports),
			"gate_clean":         gateClean,
		},
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if outFile == "" {
		_, err = os.Stdout.Write(data)
		return gateClean, err
	}
	return gateClean, os.WriteFile(outFile, data, 0o644)
}

// shortestPollutedChain BFSes from start over imports and returns the
// shortest import chain reaching a polluted package (inclusive of both
// ends). Deterministic: imports are visited in sorted order.
func shortestPollutedChain(start string, pkgs map[string]*pkgInfo, polluted map[string]bool) ([]string, bool) {
	if polluted[start] {
		return []string{start}, true
	}
	type node struct {
		ip   string
		path []string
	}
	visited := map[string]bool{start: true}
	queue := []node{{ip: start, path: []string{start}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		p := pkgs[cur.ip]
		if p == nil {
			continue
		}
		imports := append([]string{}, p.imports...)
		sort.Strings(imports)
		for _, im := range imports {
			if visited[im] {
				continue
			}
			chain := append(append([]string{}, cur.path...), im)
			if polluted[im] {
				return chain, true
			}
			visited[im] = true
			queue = append(queue, node{ip: im, path: chain})
		}
	}
	return nil, false
}

func countContaminated(reports []pkgReport) int {
	n := 0
	for _, r := range reports {
		if !r.Clean {
			n++
		}
	}
	return n
}

// blameCensus runs git blame over every tracked .go file and aggregates
// {total, upstream} line counts per package directory.
func blameCensus(repoDir string, quiet bool) (map[string][2]int, error) {
	out, err := gitOutput(repoDir, "ls-files")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, path := range strings.Split(out, "\n") {
		path = strings.TrimSpace(path)
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
	}
	byDir := map[string][2]int{} // dir -> {total, upstream}
	for i, path := range files {
		bl, err := gitOutput(repoDir, "blame", "--porcelain", "--", path)
		if err != nil {
			if strings.Contains(err.Error(), "no such path") ||
				strings.Contains(err.Error(), "does not exist") {
				continue
			}
			return nil, err
		}
		total, upstream := countBlameLines(bl)
		dir := filepath.Dir(path)
		cur := byDir[dir]
		byDir[dir] = [2]int{cur[0] + total, cur[1] + upstream}
		if !quiet && i%200 == 0 {
			fmt.Fprintf(os.Stderr, "\rclosure-scan: blame %d/%d files", i, len(files))
		}
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "\rclosure-scan: blame %d/%d files\n", len(files), len(files))
	}
	return byDir, nil
}

func countBlameLines(out string) (total, upstream int) {
	shaSelf := map[string]bool{}
	curSHA := ""
	curSelf := false
	for _, line := range splitLines(out) {
		switch {
		case isBlameHeaderShared(line):
			curSHA = line[:40]
			if self, ok := shaSelf[curSHA]; ok {
				curSelf = self
			} else {
				curSelf = false // unknown until "author " arrives; uncommitted is non-self
			}
		case strings.HasPrefix(line, "author "):
			if _, seen := shaSelf[curSHA]; !seen {
				self := strings.TrimPrefix(line, "author ") == selfAuthorName
				shaSelf[curSHA] = self
				curSelf = self
			}
		case strings.HasPrefix(line, "\t"):
			total++
			if !curSelf {
				upstream++
			}
		}
	}
	return total, upstream
}

// selfAuthorName mirrors blame-authors' self bucket. The remediation
// criterion (v3 §1) counts everything not authored by gaojiaxiang as
// upstream.
const selfAuthorName = "gaojiaxiang"

// isBlameHeaderShared reports whether line looks like a porcelain header:
// "<40-hex-sha> <orig-line> ...". Kept locally (closure-scan and
// blame-authors are standalone `go run` programs by design; a shared helper
// package would break the one-directory-per-tool layout).
func isBlameHeaderShared(line string) bool {
	if len(line) < 42 || line[40] != ' ' {
		return false
	}
	for i := 0; i < 40; i++ {
		c := line[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

// parsePackages walks the repo and parses every .go file's imports,
// resolving them to module-internal import paths where applicable.
func parsePackages(repoDir, module string, blame map[string][2]int, quiet bool) (map[string]*pkgInfo, error) {
	pkgs := map[string]*pkgInfo{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", "vendor", "node_modules", "dist", "web", "work", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		dir := filepath.Dir(rel)
		ip := module
		if dir != "." {
			ip = module + "/" + filepath.ToSlash(dir)
		}
		p := pkgs[ip]
		if p == nil {
			p = &pkgInfo{importPath: ip, dir: dir}
			if b, ok := blame[dir]; ok {
				p.totalLines, p.upstreamLines = b[0], b[1]
			}
			pkgs[ip] = p
		}
		af, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// Unparseable files (e.g. testdata): record and skip their
			// imports rather than failing the whole audit.
			fmt.Fprintf(os.Stderr, "closure-scan: parse %s: %v (skipped)\n", rel, err)
			return nil
		}
		for _, imp := range af.Imports {
			val := strings.Trim(imp.Path.Value, `"`)
			if val == "C" {
				continue
			}
			if strings.HasPrefix(val, module+"/") || val == module {
				p.addImport(val)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "closure-scan: %d module packages parsed\n", len(pkgs))
	}
	return pkgs, nil
}

func (p *pkgInfo) addImport(ip string) {
	for _, have := range p.imports {
		if have == ip {
			return
		}
	}
	p.imports = append(p.imports, ip)
}

// normalizeImport resolves a user-supplied package argument (module path,
// ./relative dir, or plain name) to the module-internal import path.
func normalizeImport(module, arg string) string {
	arg = strings.TrimSpace(arg)
	switch {
	case arg == ".":
		return module
	case strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../"):
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(arg)), "./")
		if clean == "." {
			return module
		}
		return module + "/" + clean
	case strings.HasPrefix(arg, module+"/") || arg == module:
		return arg
	default:
		return module + "/" + arg
	}
}

func goModulePath(repoDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("module directive not found in go.mod")
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
