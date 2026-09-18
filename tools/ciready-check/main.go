package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/multigent/multigent/internal/ciready"
)

func main() {
	seedFlag := flag.Bool("seed", false, "Seed missing CI baseline files before verifying")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [--seed] <target-repo-dir>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	targetDir, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid path %s: %v\n", flag.Arg(0), err)
		os.Exit(2)
	}

	var report ciready.Report
	if *seedFlag {
		report, err = ciready.Ensure(targetDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ensure failed: %v\n", err)
			os.Exit(1)
		}
	} else {
		report = ciready.Verify(targetDir)
	}

	fmt.Printf("\n=== CI/CD Readiness Report: %s (Overall: %s) ===\n", report.Repo, report.Overall)
	if len(report.Seeded) > 0 {
		fmt.Println("Seeded missing files:")
		for _, f := range report.Seeded {
			fmt.Printf("  + %s\n", f)
		}
	}
	hasFail := false
	for _, c := range report.Checks {
		statusMark := "✓"
		if c.Status == ciready.StatusFail {
			statusMark = "✗"
			hasFail = true
		} else if c.Status == ciready.StatusSkip {
			statusMark = "-"
		}
		detail := ""
		if c.Detail != "" {
			detail = " -> " + c.Detail
		}
		fmt.Printf("[%s] %-22s : %s%s\n", statusMark, c.Name, c.Status, detail)
	}
	if hasFail || report.Overall != ciready.OverallReady {
		os.Exit(1)
	}
}
