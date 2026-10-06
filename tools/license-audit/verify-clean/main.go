// verify-clean is the per-batch acceptance gate for the license remediation
// (B1, execution-plan §4): given a list of files (arguments, -list file, or
// a directory), it verifies that every file has ZERO upstream blame lines
// (everything authored by gaojiaxiang). Per v3 §5 the acceptance criterion
// for migrated packages is "blame upstream lines = 0" — machine-checkable,
// not "looks right".
//
// Exit codes: 0 = all listed files clean; 1 = at least one file has
// upstream lines or is untracked/missing; 2 = tooling error.
//
// Usage:
//
//	go run ./tools/license-audit/verify-clean [-repo DIR] [-list FILES] [-json] [PATH...]
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
)

type fileVerdict struct {
	Path          string `json:"path"`
	Lines         int    `json:"lines"`
	UpstreamLines int    `json:"upstream_lines"`
	Tracked       bool   `json:"tracked"`
	Clean         bool   `json:"clean"`
	// Offenders lists the upstream (bucket, count) pairs in descending
	// count order, so a red file says who and how much.
	Offenders map[string]int `json:"offenders,omitempty"`
}

type verdict struct {
	Repo      string         `json:"repo"`
	Files     []fileVerdict  `json:"files"`
	Clean     bool           `json:"clean"`
	Summary   map[string]any `json:"summary"`
}

func main() {
	repo := flag.String("repo", ".", "repository root")
	list := flag.String("list", "", "verify only the files listed in this file (one per line)")
	jsonOut := flag.Bool("json", false, "print full JSON verdict instead of a human summary")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: verify-clean [-repo DIR] [-list FILES] [-json] [PATH...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	verdict, err := run(*repo, *list, *jsonOut, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-clean: %v\n", err)
		os.Exit(2)
	}
	if !verdict.Clean {
		os.Exit(1)
	}
}

func run(repoDir, listFile string, asJSON bool, paths []string) (*verdict, error) {
	repoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, err
	}
	files, err := resolveFiles(repoDir, listFile, paths)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to verify")
	}

	v := &verdict{Repo: repoDir, Files: []fileVerdict{}, Clean: true}
	totalLines, totalUp := 0, 0
	for _, p := range files {
		fv := verifyFile(repoDir, p)
		v.Files = append(v.Files, fv)
		totalLines += fv.Lines
		totalUp += fv.UpstreamLines
		if !fv.Clean {
			v.Clean = false
			if !asJSON {
				fmt.Fprintf(os.Stderr, "FAIL %s: %d upstream / %d lines %v\n",
					fv.Path, fv.UpstreamLines, fv.Lines, fv.Offenders)
			}
		}
	}
	v.Summary = map[string]any{
		"files":          len(v.Files),
		"total_lines":    totalLines,
		"upstream_lines": totalUp,
		"clean":          v.Clean,
	}
	if asJSON {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return v, err
		}
		fmt.Println(string(data))
	} else {
		if v.Clean {
			fmt.Printf("verify-clean: PASS — %d files, %d lines, 0 upstream\n", len(v.Files), totalLines)
		} else {
			fmt.Printf("verify-clean: FAIL — %d files, %d lines, %d upstream\n", len(v.Files), totalLines, totalUp)
		}
	}
	return v, nil
}

// verifyFile blames one file and reports whether every line is authored by
// gaojiaxiang. Untracked or missing files fail closed (clean=false).
func verifyFile(repoDir, path string) fileVerdict {
	fv := fileVerdict{Path: path, Tracked: true, Clean: false, Offenders: map[string]int{}}
	out, err := gitOutput(repoDir, "blame", "--porcelain", "--", path)
	if err != nil {
		// Fail closed: untracked/unreadable files can never pass a
		// cleanliness gate.
		fv.Offenders["<untracked-or-error>"] = 1
		return fv
	}
	type binding struct {
		self    bool
		known   bool
		unknown string // bucket name once resolved
	}
	shaAuthor := map[string]binding{}
	curSHA := ""
	curSelf := false
	curKnown := false
	curName := "other"
	for _, line := range strings.Split(out, "\n") {
		switch {
		case isBlameHeader(line):
			curSHA = line[:40]
			if b, ok := shaAuthor[curSHA]; ok {
				curSelf, curKnown, curName = b.self, b.known, b.unknown
			} else {
				zero := true
				for i := 0; i < 40; i++ {
					if line[i] != '0' {
						zero = false
						break
					}
				}
				if zero {
					curSelf, curKnown, curName = false, true, "uncommitted"
				} else {
					curSelf, curKnown, curName = false, false, "other"
				}
			}
		case strings.HasPrefix(line, "author "):
			if !curKnown {
				name := strings.TrimPrefix(line, "author ")
				curSelf = name == "gaojiaxiang"
				if curSelf {
					curName = "gaojiaxiang"
				} else {
					curName = name
				}
				curKnown = true
				shaAuthor[curSHA] = binding{self: curSelf, known: true, unknown: curName}
			}
		case strings.HasPrefix(line, "\t"):
			fv.Lines++
			if !curSelf {
				fv.UpstreamLines++
				fv.Offenders[curName]++
			}
		}
	}
	fv.Clean = fv.UpstreamLines == 0
	return fv
}

// resolveFiles resolves arguments (files or directories expanded via git
// ls-files) or a -list file into a file list.
func resolveFiles(repoDir, listFile string, args []string) ([]string, error) {
	if listFile != "" {
		f, err := os.Open(listFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var out []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l != "" && !strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
		return out, sc.Err()
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("no paths given (arguments or -list required)")
	}
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
