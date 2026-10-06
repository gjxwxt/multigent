// blame-authors is the license-remediation audit tool (B1, execution-plan §4).
//
// It runs `git blame --porcelain` over every tracked file in the repository
// (or over the paths given as arguments / listed in the file named by
// -list) and aggregates line counts per author bucket:
//
//	gaojiaxiang / chenhg5 / root / chicken / other
//
// The known author emails are resolved from git history itself (author name
// + email pairs seen in `git log --format=%an%x09%ae`), so the buckets
// follow the repository's real identities rather than hard-coded strings
// that could drift.
//
// Output is JSON on stdout, or written to the file named by -o. Binary-ish
// extensions (images, fonts, archives) are skipped — blame has no line
// semantics there. Uncommitted lines (revision 0000000000000000000000)
// bucket as "uncommitted" so uncommitted work can never silently inflate an
// author bucket (v3 §13 caveat).
//
// Usage:
//
//	go run ./tools/license-audit/blame-authors.go [-repo DIR] [-o OUT.json] [PATH...]
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Buckets are the canonical author buckets from the remediation verdict
// (docs/license-remediation-final-v3.md §1). Any author outside these maps
// to "other".
var canonicalBuckets = []string{"gaojiaxiang", "chenhg5", "root", "chicken", "other"}

// selfAuthor is the bucket treated as "own" for the upstream-line test.
const selfAuthor = "gaojiaxiang"

// binaryExt are extensions where line counts are meaningless; they are
// reported as skipped files.
var binaryExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".svg": true, ".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".pdf": true, ".zip": true, ".gz": true, ".icns": true, ".bundle": true,
	".webp": true, ".mp4": true, ".mov": true, ".bin": true,
}

type fileReport struct {
	Path    string         `json:"path"`
	Lines   int            `json:"lines"` // total blame-attributed lines
	Authors map[string]int `json:"authors"`
	// UpstreamLines is the remediation-bad bucket: every line NOT authored
	// by gaojiaxiang. Per the v3 §3 criterion discipline the pollution test
	// is "upstream lines > 0", never a ratio.
	UpstreamLines int `json:"upstream_lines"`
}

type repoReport struct {
	Repo        string         `json:"repo"`
	HeadCommit  string         `json:"head_commit"`
	GeneratedAt string         `json:"generated_at"`
	Files       []fileReport   `json:"files"`
	Totals      map[string]int `json:"totals"` // bucket -> lines (includes "uncommitted")
	Skipped     []string       `json:"skipped_binary"`
	Summary     map[string]any `json:"summary"`
}

type blameEntry struct {
	bucket      string
	uncommitted bool
}

func main() {
	repo := flag.String("repo", ".", "repository root (git worktree used for blame)")
	out := flag.String("o", "", "write JSON report to this file (default stdout)")
	list := flag.String("list", "", "audit only the files listed in this file (one per line)")
	quiet := flag.Bool("q", false, "suppress progress output on stderr")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: blame-authors [-repo DIR] [-o OUT.json] [-list FILES] [PATH...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(*repo, *out, *list, *quiet, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "blame-authors: %v\n", err)
		os.Exit(1)
	}
}

func run(repoDir, outFile, listFile string, quiet bool, extraPaths []string) error {
	repoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return err
	}
	head, err := gitOutput(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("not a git repo (%s): %w", repoDir, err)
	}
	head = strings.TrimSpace(head)

	// Resolve the author email -> bucket mapping from real history so the
	// buckets cannot drift from the repository's actual identities.
	bucketByAuthor, err := authorBuckets(repoDir)
	if err != nil {
		return err
	}

	paths, err := auditPaths(repoDir, listFile, extraPaths)
	if err != nil {
		return err
	}

	report := &repoReport{
		Repo:        repoDir,
		HeadCommit:  head,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Files:       []fileReport{},
		Skipped:     []string{},
		Totals:      map[string]int{},
	}
	totals := map[string]int{}
	for _, b := range canonicalBuckets {
		totals[b] = 0
	}
	totals["uncommitted"] = 0

	for i, p := range paths {
		if binaryExt[strings.ToLower(filepath.Ext(p))] {
			report.Skipped = append(report.Skipped, p)
			continue
		}
		fr, err := blameFile(repoDir, p, bucketByAuthor)
		if err != nil {
			// Deleted-but-listed files are skipped; anything else is a hard
			// error so the audit never passes on partial data.
			if strings.Contains(err.Error(), "no such path") ||
				strings.Contains(err.Error(), "does not exist") {
				continue
			}
			return fmt.Errorf("blame %s: %w", p, err)
		}
		report.Files = append(report.Files, *fr)
		for bucket, n := range fr.Authors {
			totals[bucket] += n
		}
		if !quiet && i%100 == 0 {
			fmt.Fprintf(os.Stderr, "\rblame-authors: %d/%d files", i, len(paths))
		}
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "\rblame-authors: %d/%d files done\n", len(paths), len(paths))
	}

	totalLines := 0
	selfLines := 0
	upstreamLines := 0
	upstreamFileCount := 0
	for _, fr := range report.Files {
		totalLines += fr.Lines
		selfLines += fr.Authors[selfAuthor]
		upstreamLines += fr.UpstreamLines
		if fr.UpstreamLines > 0 {
			upstreamFileCount++
		}
	}
	for k, v := range totals {
		report.Totals[k] = v
	}

	report.Summary = map[string]any{
		"files_audited":       len(report.Files),
		"files_skipped_bin":   len(report.Skipped),
		"total_lines":         totalLines,
		"self_lines":          selfLines,     // gaojiaxiang
		"upstream_lines":      upstreamLines, // not-gaojiaxiang (v3 §1 口径)
		"upstream_file_count": upstreamFileCount,
		"self_percent":        pct(selfLines, totalLines),
		"buckets":             totals,
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if outFile == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(outFile, data, 0o644)
}

// authorBuckets maps author names to canonical bucket names using real git
// history. The four known author names map to themselves; anything else is
// "other".
func authorBuckets(repoDir string) (map[string]string, error) {
	out, err := gitOutput(repoDir, "log", "--format=%an%x09%ae", "--all")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		bucket := "other"
		switch name {
		case "gaojiaxiang", "chenhg5", "root", "chicken":
			bucket = name
		}
		m[name] = bucket
	}
	return m, nil
}

// blameFile blames one file and buckets every content line. blame
// --porcelain emits, per blamed line: a header "<sha> <orig> [<result>]",
// then metadata for that sha (author, author-mail, ...), then the content
// line prefixed with \t. Commit metadata for a sha is only repeated when the
// sha changes, so we cache sha -> bucket on first sight and reuse it.
func blameFile(repoDir, path string, buckets map[string]string) (*fileReport, error) {
	out, err := gitOutput(repoDir, "blame", "--porcelain", "--", path)
	if err != nil {
		return nil, err
	}
	return parseBlamePorcelain(path, buckets, out), nil
}

func parseBlamePorcelain(path string, buckets map[string]string, out string) *fileReport {
	fr := &fileReport{Path: path, Authors: map[string]int{}}
	for _, b := range canonicalBuckets {
		fr.Authors[b] = 0
	}
	zeroSHA := "0000000000000000000000000000000000000000"
	shaBucket := map[string]string{}
	curBucket := "other"
	curSHA := ""

	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case isBlameHeader(line):
			curSHA = line[:40]
			if b, ok := shaBucket[curSHA]; ok {
				curBucket = b
			} else if curSHA == zeroSHA {
				curBucket = "uncommitted"
			} else {
				curBucket = "" // unknown until the "author " line arrives
			}
		case strings.HasPrefix(line, "author "):
			if curBucket == "" {
				name := strings.TrimPrefix(line, "author ")
				b := buckets[name]
				if b == "" {
					b = "other"
				}
				shaBucket[curSHA] = b
				curBucket = b
			}
		case strings.HasPrefix(line, "\t"):
			bucket := curBucket
			if bucket == "" {
				bucket = "other"
			}
			if curSHA == zeroSHA {
				bucket = "uncommitted"
			}
			fr.Lines++
			fr.Authors[bucket]++
			if bucket != selfAuthor {
				fr.UpstreamLines++
			}
		}
	}
	return fr
}

// isBlameHeader reports whether line looks like a porcelain header:
// "<40-hex-sha> <orig-line> ..." (optionally " (num)" trailing and a result
// line for boundary entries).
func isBlameHeader(line string) bool {
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

// auditPaths resolves the file set: explicit args, or a -list file, or all
// tracked files (git ls-files).
func auditPaths(repoDir, listFile string, extra []string) ([]string, error) {
	if listFile != "" {
		data, err := os.ReadFile(listFile)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimSpace(l)
			if l != "" && !strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
		return out, nil
	}
	if len(extra) > 0 {
		return expandDirs(repoDir, extra)
	}
	out, err := gitOutput(repoDir, "ls-files")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// expandDirs resolves each argument: an existing directory becomes every
// tracked file under it; anything else is passed through as a path
// (existing file, glob, or a path that blame itself will reject).
func expandDirs(repoDir string, args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		if info, err := os.Stat(filepath.Join(repoDir, a)); err == nil && info.IsDir() {
			ls, err := gitOutput(repoDir, "ls-files", "--", a)
			if err != nil {
				return nil, err
			}
			for _, l := range strings.Split(ls, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					out = append(out, l)
				}
			}
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func pct(part, total int) string {
	if total == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", float64(part)/float64(total)*100)
}
