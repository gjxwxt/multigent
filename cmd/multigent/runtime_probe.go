package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/multigent/multigent/internal/appconfig"
	"github.com/multigent/multigent/internal/sandbox"
	"github.com/spf13/cobra"
)

// ProbeExitError represents an exit code for the probe command.
type ProbeExitError struct {
	Code int
	Msg  string
}

func (e *ProbeExitError) Error() string {
	return e.Msg
}

func (e *ProbeExitError) ExitCode() int {
	return e.Code
}

// ProbeResult records the outcome of probing a single registry source.
type ProbeResult struct {
	Source     string `json:"source"`
	Registry   string `json:"registry"`
	Target     string `json:"target"`
	Status     string `json:"status"` // PASS, AUTH_REQUIRED, FAIL
	Duration   string `json:"duration"`
	DurationMS int64  `json:"duration_ms"`
	Summary    string `json:"summary"`
}

// ProbeReport wraps the overall probe results and exit code.
type ProbeReport struct {
	ExitCode int           `json:"exit_code"`
	Results  []ProbeResult `json:"results"`
}

type probeTask struct {
	Source      string
	Registry    string
	Target      string
	CommandArgs []string
}

type probeRunnerFunc func(ctx context.Context, args []string) ([]byte, error)

var probeRunner probeRunnerFunc = func(ctx context.Context, args []string) ([]byte, error) {
	cmd := sandbox.DockerCommandContext(ctx, args...)
	return cmd.CombinedOutput()
}

var safeTargetRegex = regexp.MustCompile(`^[a-zA-Z0-9_@.~/+=^:-]+$`)

func validateProbeTarget(source, target string) error {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return fmt.Errorf("%s probe target cannot be empty", source)
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("invalid %s probe target %q: leading dash not allowed", source, target)
	}
	if !safeTargetRegex.MatchString(trimmed) {
		return fmt.Errorf("invalid %s probe target %q: contains unsafe or illegal characters", source, target)
	}
	return nil
}

var userInfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)([^/\s@]+@)`)

func redactUserInfo(s string) string {
	return userInfoPattern.ReplaceAllString(s, "${1}<redacted>@")
}

func isAuthRequired(output string) bool {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "401") || strings.Contains(lower, "403") {
		if strings.Contains(lower, "unauthorized") ||
			strings.Contains(lower, "forbidden") ||
			strings.Contains(lower, "e401") ||
			strings.Contains(lower, "e403") ||
			strings.Contains(lower, "client error") ||
			strings.Contains(lower, "server response") ||
			strings.Contains(lower, "http") ||
			strings.Contains(lower, "status") {
			return true
		}
	}
	if strings.Contains(lower, "authentication required") ||
		strings.Contains(lower, "unable to authenticate") ||
		strings.Contains(lower, "not authorized") ||
		strings.Contains(lower, "authorization required") ||
		strings.Contains(lower, "credentials required") {
		return true
	}
	return false
}

func extractRelevantErrorLine(output string) string {
	lines := strings.Split(output, "\n")
	var candidates []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			candidates = append(candidates, trimmed)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	for _, l := range candidates {
		lower := strings.ToLower(l)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "401") ||
			strings.Contains(lower, "403") ||
			strings.Contains(lower, "unauthorized") ||
			strings.Contains(lower, "forbidden") ||
			strings.Contains(lower, "fatal") {
			return l
		}
	}
	return candidates[len(candidates)-1]
}

func classifyProbeOutput(output string, exitCode int) (string, string) {
	redacted := redactUserInfo(strings.TrimSpace(output))
	if exitCode == 0 {
		firstLine := strings.TrimSpace(strings.Split(redacted, "\n")[0])
		if firstLine == "" {
			firstLine = "ok"
		}
		return "PASS", firstLine
	}
	if isAuthRequired(output) {
		firstLine := extractRelevantErrorLine(redacted)
		if firstLine == "" {
			firstLine = "authentication required (401/403 or credentials needed)"
		}
		return "AUTH_REQUIRED", firstLine
	}
	firstLine := extractRelevantErrorLine(redacted)
	if firstLine == "" {
		firstLine = "command failed"
	}
	return "FAIL", firstLine
}

func calculateProbeExitCode(results []ProbeResult) int {
	if len(results) == 0 {
		return 3
	}
	hasFail := false
	hasAuthRequired := false
	for _, r := range results {
		if r.Status == "FAIL" {
			hasFail = true
		} else if r.Status == "AUTH_REQUIRED" {
			hasAuthRequired = true
		}
	}
	if hasFail {
		return 1
	}
	if hasAuthRequired {
		return 2
	}
	return 0
}

func buildProbeDockerArgs(image, network string, runAsHostUser bool, cmdArgs []string) []string {
	network = strings.TrimSpace(network)
	if network == "" {
		network = "bridge"
	}
	args := []string{"run", "--rm"}
	if runAsHostUser {
		args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	}
	args = append(args, fmt.Sprintf("--network=%s", network), "--add-host=host.docker.internal:host-gateway")
	args = append(args, sandbox.TransportDockerArgs()...)
	args = append(args, "-e", "HOME=/tmp/multigent-probe")
	args = append(args, image)
	args = append(args, cmdArgs...)
	return args
}

func resolveProbeTasks(cfg *appconfig.Config, npmPkg, pipPkg, goMod, sourceFilter string) ([]probeTask, error) {
	filterSet := make(map[string]bool)
	if strings.TrimSpace(sourceFilter) != "" {
		for _, s := range strings.Split(sourceFilter, ",") {
			st := strings.ToLower(strings.TrimSpace(s))
			if st != "" {
				filterSet[st] = true
			}
		}
	}

	shouldProbe := func(name string) bool {
		if len(filterSet) == 0 {
			return true
		}
		return filterSet[name]
	}

	if shouldProbe("apt") && len(filterSet) > 0 {
		return nil, fmt.Errorf("apt probe is not supported in P0: managed enterprise APT sources and certificates are introduced in P1 (corp.sources image)")
	}

	var tasks []probeTask

	// 1. npm
	if shouldProbe("npm") {
		npmReg := ""
		if cfg != nil {
			npmReg = cfg.Registries.NPM
			if npmReg == "" {
				npmReg = cfg.Runtime.NPMRegistry
			}
		}
		if npmReg == "" {
			npmReg = os.Getenv("NPM_CONFIG_REGISTRY")
		}
		if npmReg != "" {
			pkg := strings.TrimSpace(npmPkg)
			if pkg == "" {
				pkg = "npm"
			}
			if err := validateProbeTarget("npm", pkg); err != nil {
				return nil, err
			}
			tasks = append(tasks, probeTask{
				Source:   "npm",
				Registry: npmReg,
				Target:   pkg,
				CommandArgs: []string{
					"/bin/sh", "-c",
					"mkdir -p /tmp/multigent-probe && cd /tmp/multigent-probe && exec npm view -- \"$1\" version",
					"_", pkg,
				},
			})
		}
	}

	// 2. pip
	if shouldProbe("pip") {
		pipReg := ""
		if cfg != nil {
			pipReg = cfg.Registries.PIP
		}
		if pipReg == "" {
			pipReg = os.Getenv("PIP_INDEX_URL")
		}
		if pipReg != "" {
			pkg := strings.TrimSpace(pipPkg)
			if pkg == "" {
				pkg = "pip"
			}
			if err := validateProbeTarget("pip", pkg); err != nil {
				return nil, err
			}
			tasks = append(tasks, probeTask{
				Source:   "pip",
				Registry: pipReg,
				Target:   pkg,
				CommandArgs: []string{
					"/bin/sh", "-c",
					"mkdir -p /tmp/multigent-probe && cd /tmp/multigent-probe && exec pip download --no-deps --dest /tmp -- \"$1\"",
					"_", pkg,
				},
			})
		}
	}

	// 3. go
	if shouldProbe("go") {
		goProxy := ""
		if cfg != nil {
			goProxy = cfg.Registries.Go
		}
		if goProxy == "" {
			goProxy = os.Getenv("GOPROXY")
		}
		if goProxy != "" {
			mod := strings.TrimSpace(goMod)
			if mod == "" {
				mod = "golang.org/x/mod@v0.24.0"
			}
			targetMod := mod
			if !strings.Contains(targetMod, "@") {
				targetMod += "@latest"
			}
			if err := validateProbeTarget("go", targetMod); err != nil {
				return nil, err
			}
			tasks = append(tasks, probeTask{
				Source:   "go",
				Registry: goProxy,
				Target:   mod,
				CommandArgs: []string{
					"/bin/sh", "-c",
					"mkdir -p /tmp/multigent-probe/go-probe && cd /tmp/multigent-probe/go-probe && ([ -f go.mod ] || go mod init probe >/dev/null 2>&1 || true) && exec go mod download -- \"$1\"",
					"_", targetMod,
				},
			})
		}
	}

	return tasks, nil
}

func printProbeResultsText(results []ProbeResult) {
	if len(results) == 0 {
		fmt.Fprintln(os.Stdout, "multigent runtime probe: no configured registries to probe")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tSTATUS\tREGISTRY\tTARGET\tDURATION\tSUMMARY")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Source,
			r.Status,
			r.Registry,
			r.Target,
			r.Duration,
			r.Summary)
	}
	_ = w.Flush()
}

func printProbeResultsJSON(report ProbeReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(data, '\n'))
	return err
}

func newRuntimeProbeCmd() *cobra.Command {
	var (
		npmPackage string
		pipPackage string
		goModule   string
		timeout    time.Duration
		format     string
		image      string
		source     string
		network    string
	)

	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Probe configured registry and proxy transport links in an ephemeral sandbox container",
		Long: `Probe configured registry and proxy transport links using real CLI clients
(npm, pip, go) inside an ephemeral container.

The probe container runs with the same non-root UID/GID as agent sandboxes,
with zero workspace mounts, zero credentials, and zero model API keys.
Only explicit registries configured via config file or environment variables are probed;
unconfigured registries are skipped.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAppConfig()
			if err != nil {
				return err
			}

			tasks, err := resolveProbeTasks(cfg, npmPackage, pipPackage, goModule, source)
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				if strings.EqualFold(strings.TrimSpace(format), "json") {
					_ = printProbeResultsJSON(ProbeReport{ExitCode: 3, Results: []ProbeResult{}})
				}
				return &ProbeExitError{Code: 3, Msg: "multigent runtime probe: no configured registries to probe"}
			}

			if err := sandbox.CheckDocker(); err != nil {
				return fmt.Errorf("docker check failed: %w", err)
			}

			probeImage := strings.TrimSpace(image)
			if probeImage == "" {
				probeImage = os.Getenv(sandbox.EnvRuntimeImage)
			}
			if probeImage == "" && cfg != nil {
				probeImage = cfg.Runtime.Image
			}
			if probeImage == "" {
				probeImage = sandbox.DefaultBaseImage()
			}

			ctx := context.Background()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			hostUser := sandbox.RunAsHostUser(nil)
			var results []ProbeResult

			for _, task := range tasks {
				dockerArgs := buildProbeDockerArgs(probeImage, network, hostUser, task.CommandArgs)
				start := time.Now()
				out, err := probeRunner(ctx, dockerArgs)
				elapsed := time.Since(start)

				if ctx.Err() == context.DeadlineExceeded {
					results = append(results, ProbeResult{
						Source:     task.Source,
						Registry:   redactUserInfo(task.Registry),
						Target:     task.Target,
						Status:     "FAIL",
						Duration:   elapsed.Round(time.Millisecond).String(),
						DurationMS: elapsed.Milliseconds(),
						Summary:    fmt.Sprintf("probe timed out after %s", timeout.String()),
					})
					break
				}

				exitCode := 0
				if err != nil {
					var exitErr *exec.ExitError
					if errors.As(err, &exitErr) {
						exitCode = exitErr.ExitCode()
					} else {
						exitCode = 1
					}
				}

				status, summary := classifyProbeOutput(string(out), exitCode)
				results = append(results, ProbeResult{
					Source:     task.Source,
					Registry:   redactUserInfo(task.Registry),
					Target:     task.Target,
					Status:     status,
					Duration:   elapsed.Round(time.Millisecond).String(),
					DurationMS: elapsed.Milliseconds(),
					Summary:    summary,
				})
			}

			exitCode := calculateProbeExitCode(results)
			report := ProbeReport{
				ExitCode: exitCode,
				Results:  results,
			}

			if strings.EqualFold(strings.TrimSpace(format), "json") {
				if err := printProbeResultsJSON(report); err != nil {
					return err
				}
			} else {
				printProbeResultsText(results)
			}

			if exitCode != 0 {
				var msg string
				switch exitCode {
				case 1:
					msg = "runtime probe: one or more sources failed"
				case 2:
					msg = "runtime probe: authentication required for one or more sources"
				case 3:
					msg = "runtime probe: no configured registries to probe"
				default:
					msg = fmt.Sprintf("runtime probe: exited with code %d", exitCode)
				}
				return &ProbeExitError{Code: exitCode, Msg: msg}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&npmPackage, "npm-package", "npm", "npm package to probe (default: npm)")
	cmd.Flags().StringVar(&pipPackage, "pip-package", "pip", "pip package to probe (default: pip)")
	cmd.Flags().StringVar(&goModule, "go-module", "golang.org/x/mod@v0.24.0", "Go module to probe with version (default: golang.org/x/mod@v0.24.0)")
	cmd.Flags().DurationVar(&timeout, "timeout", 120*time.Second, "overall timeout for the probe (default: 120s)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json (default: text)")
	cmd.Flags().StringVar(&image, "image", "", "runtime Docker image to use (default: resolved runtime image)")
	cmd.Flags().StringVar(&source, "source", "", "comma-separated sources to probe (npm,pip,go; default: all configured)")
	cmd.Flags().StringVar(&network, "network", "bridge", "Docker network mode for probe container (default: bridge)")

	return cmd
}
