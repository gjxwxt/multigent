package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/appconfig"
)

func TestBuildProbeDockerArgs(t *testing.T) {
	// Set some transport envs and model keys
	t.Setenv("NPM_CONFIG_REGISTRY", "https://nexus.corp.example/npm/")
	t.Setenv("PIP_INDEX_URL", "https://nexus.corp.example/pypi/")
	t.Setenv("OPENAI_API_KEY", "secret-openai-key")
	t.Setenv("ANTHROPIC_API_KEY", "secret-anthropic-key")
	t.Setenv("GEMINI_API_KEY", "secret-gemini-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-aws-key")

	cmdArgs := []string{"/bin/sh", "-c", "echo hello"}
	image := "ghcr.io/multigent/multigent/runtime-base:test"

	t.Run("with hostUser", func(t *testing.T) {
		args := buildProbeDockerArgs(image, "bridge", true, cmdArgs)

		// Must have basic Docker flags
		if len(args) < 5 || args[0] != "run" || args[1] != "--rm" {
			t.Fatalf("expected run --rm prefix, got: %v", args)
		}

		// Must have user flag
		hasUser := false
		for i, a := range args {
			if a == "--user" && i+1 < len(args) {
				hasUser = true
				expectedUIDGID := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
				if args[i+1] != expectedUIDGID {
					t.Errorf("expected --user %s, got %s", expectedUIDGID, args[i+1])
				}
				break
			}
		}
		if !hasUser {
			t.Error("missing --user flag when runAsHostUser is true")
		}

		// Must have network and host-gateway
		hasBridge := false
		hasGateway := false
		for _, a := range args {
			if a == "--network=bridge" {
				hasBridge = true
			}
			if a == "--add-host=host.docker.internal:host-gateway" {
				hasGateway = true
			}
		}
		if !hasBridge {
			t.Error("missing --network=bridge")
		}
		if !hasGateway {
			t.Error("missing --add-host=host.docker.internal:host-gateway")
		}

		// Must NOT have any volume mount or workspace
		for _, a := range args {
			if a == "-v" || a == "--volume" || a == "--mount" || strings.HasPrefix(a, "-v=") || strings.HasPrefix(a, "--volume=") || strings.HasPrefix(a, "--mount=") {
				t.Errorf("probe docker args must not contain volume mounts: %s", a)
			}
			if strings.Contains(strings.ToLower(a), "workspace") {
				t.Errorf("probe docker args must not contain workspace: %s", a)
			}
		}

		// Must NOT have any model API keys
		for _, a := range args {
			for _, forbidden := range []string{
				"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
				"GOOGLE_API_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID",
				"secret-openai-key", "secret-anthropic-key",
			} {
				if strings.Contains(a, forbidden) {
					t.Errorf("probe docker args leaked model credential or key %q in arg %q", forbidden, a)
				}
			}
		}

		// Must pass transport keys
		hasNPM := false
		hasPIP := false
		for i, a := range args {
			if a == "-e" && i+1 < len(args) {
				if args[i+1] == "NPM_CONFIG_REGISTRY" {
					hasNPM = true
				}
				if args[i+1] == "PIP_INDEX_URL" {
					hasPIP = true
				}
			}
		}
		if !hasNPM || !hasPIP {
			t.Errorf("probe docker args missing transport env keys: npm=%v, pip=%v", hasNPM, hasPIP)
		}

		// Must end with image and /bin/sh -c <cmd>
		last := args[len(args)-4:]
		if last[0] != image || last[1] != "/bin/sh" || last[2] != "-c" || last[3] != "echo hello" {
			t.Errorf("unexpected command tail: %v", last)
		}
	})

	t.Run("without hostUser", func(t *testing.T) {
		args := buildProbeDockerArgs(image, "host", false, cmdArgs)
		for _, a := range args {
			if a == "--user" {
				t.Errorf("did not expect --user flag when runAsHostUser is false")
			}
		}
		hasHostNetwork := false
		for _, a := range args {
			if a == "--network=host" {
				hasHostNetwork = true
			}
		}
		if !hasHostNetwork {
			t.Errorf("expected --network=host, got: %v", args)
		}
	})
}

func TestRedactUserInfo(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "https://user:password@registry.example.com/npm/",
			want:  "https://<redacted>@registry.example.com/npm/",
		},
		{
			input: "http://token@10.0.0.1:8080/pypi/simple",
			want:  "http://<redacted>@10.0.0.1:8080/pypi/simple",
		},
		{
			input: "git://user:pass123@git.corp.example/repo.git",
			want:  "git://<redacted>@git.corp.example/repo.git",
		},
		{
			input: "npm error code E401 https://alice:secret@nexus.example.com/npm/",
			want:  "npm error code E401 https://<redacted>@nexus.example.com/npm/",
		},
		{
			input: "https://nexus.corp.example/repository/npm/",
			want:  "https://nexus.corp.example/repository/npm/",
		},
		{
			input: "user@example.com is an email, not a URL with userinfo",
			want:  "user@example.com is an email, not a URL with userinfo",
		},
	}

	for _, tt := range tests {
		got := redactUserInfo(tt.input)
		if got != tt.want {
			t.Errorf("redactUserInfo(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestClassifyProbeOutput(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		exitCode    int
		wantStatus  string
		wantSummary string
	}{
		{
			name:        "exit 0 with version",
			output:      "10.9.0\n",
			exitCode:    0,
			wantStatus:  "PASS",
			wantSummary: "10.9.0",
		},
		{
			name:        "exit 0 empty output",
			output:      "",
			exitCode:    0,
			wantStatus:  "PASS",
			wantSummary: "ok",
		},
		{
			name:        "npm E401",
			output:      "npm notice\nnpm error code E401\nnpm error 401 Unauthorized - GET https://user:pass@nexus.example.com/npm/",
			exitCode:    1,
			wantStatus:  "AUTH_REQUIRED",
			wantSummary: "npm error code E401",
		},
		{
			name:        "pip 403 Forbidden",
			output:      "HTTP error 403 Forbidden\nAuthentication credentials required",
			exitCode:    1,
			wantStatus:  "AUTH_REQUIRED",
			wantSummary: "HTTP error 403 Forbidden",
		},
		{
			name:        "go 401 Unauthorized",
			output:      "go: module golang.org/x/mod: reading https://proxy.example.com/golang.org/x/mod/@v/list: 401 Unauthorized",
			exitCode:    1,
			wantStatus:  "AUTH_REQUIRED",
			wantSummary: "go: module golang.org/x/mod: reading https://proxy.example.com/golang.org/x/mod/@v/list: 401 Unauthorized",
		},
		{
			name:        "generic connection failure",
			output:      "dial tcp 10.0.0.1:443: connect: connection refused",
			exitCode:    1,
			wantStatus:  "FAIL",
			wantSummary: "dial tcp 10.0.0.1:443: connect: connection refused",
		},
		{
			name:        "generic failure with redacted secret URL",
			output:      "fatal: unable to access 'https://token123@git.example.com': Failed to connect",
			exitCode:    1,
			wantStatus:  "FAIL",
			wantSummary: "fatal: unable to access 'https://<redacted>@git.example.com': Failed to connect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, summary := classifyProbeOutput(tt.output, tt.exitCode)
			if status != tt.wantStatus {
				t.Errorf("classifyProbeOutput() status = %q, want %q", status, tt.wantStatus)
			}
			if summary != tt.wantSummary {
				t.Errorf("classifyProbeOutput() summary = %q, want %q", summary, tt.wantSummary)
			}
		})
	}
}

func TestCalculateProbeExitCode(t *testing.T) {
	tests := []struct {
		name    string
		results []ProbeResult
		want    int
	}{
		{
			name:    "empty results",
			results: nil,
			want:    3,
		},
		{
			name: "all pass",
			results: []ProbeResult{
				{Source: "npm", Status: "PASS"},
				{Source: "pip", Status: "PASS"},
			},
			want: 0,
		},
		{
			name: "has fail",
			results: []ProbeResult{
				{Source: "npm", Status: "PASS"},
				{Source: "pip", Status: "FAIL"},
			},
			want: 1,
		},
		{
			name: "has auth required no fail",
			results: []ProbeResult{
				{Source: "npm", Status: "PASS"},
				{Source: "pip", Status: "AUTH_REQUIRED"},
			},
			want: 2,
		},
		{
			name: "has both fail and auth required (fail priority)",
			results: []ProbeResult{
				{Source: "npm", Status: "AUTH_REQUIRED"},
				{Source: "pip", Status: "FAIL"},
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateProbeExitCode(tt.results)
			if got != tt.want {
				t.Errorf("calculateProbeExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveProbeTasks(t *testing.T) {
	cfg := &appconfig.Config{
		Registries: appconfig.RegistriesConfig{
			NPM: "https://nexus.corp.example/npm/",
			PIP: "https://nexus.corp.example/pypi/",
			Go:  "https://nexus.corp.example/go/",
		},
	}

	t.Run("all configured", func(t *testing.T) {
		tasks, err := resolveProbeTasks(cfg, "", "", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tasks) != 3 {
			t.Fatalf("expected 3 tasks, got %d", len(tasks))
		}
		if tasks[0].Source != "npm" || tasks[0].Target != "npm" {
			t.Errorf("task 0 unexpected: %+v", tasks[0])
		}
		if tasks[1].Source != "pip" || tasks[1].Target != "pip" {
			t.Errorf("task 1 unexpected: %+v", tasks[1])
		}
		if tasks[2].Source != "go" || tasks[2].Target != "golang.org/x/mod@v0.24.0" {
			t.Errorf("task 2 unexpected: %+v", tasks[2])
		}
	})

	t.Run("custom packages", func(t *testing.T) {
		tasks, err := resolveProbeTasks(cfg, "express", "requests", "example.com/mod@v1.0", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tasks) != 3 {
			t.Fatalf("expected 3 tasks, got %d", len(tasks))
		}
		if tasks[0].Target != "express" || len(tasks[0].CommandArgs) != 5 || tasks[0].CommandArgs[4] != "express" {
			t.Errorf("npm task unexpected: %+v", tasks[0])
		}
		if tasks[1].Target != "requests" || len(tasks[1].CommandArgs) != 5 || tasks[1].CommandArgs[4] != "requests" {
			t.Errorf("pip task unexpected: %+v", tasks[1])
		}
		if tasks[2].Target != "example.com/mod@v1.0" || len(tasks[2].CommandArgs) != 5 || tasks[2].CommandArgs[4] != "example.com/mod@v1.0" {
			t.Errorf("go task unexpected: %+v", tasks[2])
		}
	})

	t.Run("filter by source", func(t *testing.T) {
		tasks, err := resolveProbeTasks(cfg, "", "", "", "npm,go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d", len(tasks))
		}
		if tasks[0].Source != "npm" || tasks[1].Source != "go" {
			t.Errorf("unexpected filtered tasks: %+v", tasks)
		}
	})

	t.Run("unconfigured sources skipped", func(t *testing.T) {
		emptyCfg := &appconfig.Config{}
		tasks, err := resolveProbeTasks(emptyCfg, "", "", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tasks) != 0 {
			t.Errorf("expected 0 tasks for empty config, got %d", len(tasks))
		}
	})
}

func TestResolveProbeTasksShellInjectionPrevention(t *testing.T) {
	cfg := &appconfig.Config{
		Registries: appconfig.RegistriesConfig{
			NPM: "https://nexus.corp.example/npm/",
			PIP: "https://nexus.corp.example/pypi/",
			Go:  "https://nexus.corp.example/go/",
		},
	}

	injectionTargets := []string{
		"pkg; rm -rf /",
		"pkg && touch /tmp/pwned",
		"pkg | cat /etc/passwd",
		"pkg`whoami`",
		"pkg$(whoami)",
		"pkg\nrm -rf /",
		"-leading-dash",
		"--flag-injection",
		"pkg with spaces",
		"pkg'quote",
		"pkg\"doublequote",
	}

	for _, bad := range injectionTargets {
		t.Run("npm:"+bad, func(t *testing.T) {
			_, err := resolveProbeTasks(cfg, bad, "pip", "golang.org/x/mod@v0.24.0", "npm")
			if err == nil {
				t.Fatalf("expected error for malicious npm target %q, got nil", bad)
			}
		})
		t.Run("pip:"+bad, func(t *testing.T) {
			_, err := resolveProbeTasks(cfg, "npm", bad, "golang.org/x/mod@v0.24.0", "pip")
			if err == nil {
				t.Fatalf("expected error for malicious pip target %q, got nil", bad)
			}
		})
		t.Run("go:"+bad, func(t *testing.T) {
			_, err := resolveProbeTasks(cfg, "npm", "pip", bad, "go")
			if err == nil {
				t.Fatalf("expected error for malicious go target %q, got nil", bad)
			}
		})
	}
}

func TestProbeTaskPositionalArguments(t *testing.T) {
	cfg := &appconfig.Config{
		Registries: appconfig.RegistriesConfig{
			NPM: "https://nexus.corp.example/npm/",
		},
	}
	tasks, err := resolveProbeTasks(cfg, "lodash", "", "", "npm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	args := tasks[0].CommandArgs
	if len(args) != 5 || args[0] != "/bin/sh" || args[1] != "-c" || args[3] != "_" || args[4] != "lodash" {
		t.Fatalf("unexpected CommandArgs structure: %v", args)
	}
	if strings.Contains(args[2], "lodash") {
		t.Fatalf("script body should use \"$1\" rather than interpolating target name: %s", args[2])
	}
}

func TestProbeFlagsAndDefaults(t *testing.T) {
	cmd := newRuntimeProbeCmd()

	flags := []struct {
		name         string
		defaultValue string
	}{
		{"npm-package", "npm"},
		{"pip-package", "pip"},
		{"go-module", "golang.org/x/mod@v0.24.0"},
		{"format", "text"},
		{"network", "bridge"},
	}

	for _, f := range flags {
		flag := cmd.Flags().Lookup(f.name)
		if flag == nil {
			t.Errorf("missing flag --%s", f.name)
			continue
		}
		if flag.DefValue != f.defaultValue {
			t.Errorf("flag --%s default = %q, want %q", f.name, flag.DefValue, f.defaultValue)
		}
	}
}

func TestPrintProbeResultsJSON(t *testing.T) {
	report := ProbeReport{
		ExitCode: 0,
		Results: []ProbeResult{
			{
				Source:     "npm",
				Registry:   "https://nexus.corp.example/npm/",
				Target:     "npm",
				Status:     "PASS",
				Duration:   "1.2s",
				DurationMS: 1200,
				Summary:    "10.9.0",
			},
		},
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed ProbeReport
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if parsed.ExitCode != 0 || len(parsed.Results) != 1 {
		t.Errorf("parsed unexpected: %+v", parsed)
	}
	if parsed.Results[0].Source != "npm" || parsed.Results[0].Status != "PASS" {
		t.Errorf("parsed.Results unexpected: %+v", parsed.Results)
	}
}

func TestMockProbeRunnerExecution(t *testing.T) {
	origRunner := probeRunner
	defer func() { probeRunner = origRunner }()

	probeRunner = func(ctx context.Context, args []string) ([]byte, error) {
		for _, a := range args {
			if strings.Contains(a, "npm view") {
				return []byte("10.9.0\n"), nil
			}
			if strings.Contains(a, "pip download") {
				return []byte("HTTP 401 Unauthorized\n"), errors.New("exit 1")
			}
		}
		return []byte("ok"), nil
	}

	cfg := &appconfig.Config{
		Registries: appconfig.RegistriesConfig{
			NPM: "https://nexus.corp.example/npm/",
			PIP: "https://nexus.corp.example/pypi/",
		},
	}

	tasks, err := resolveProbeTasks(cfg, "npm", "pip", "", "")
	if err != nil {
		t.Fatalf("resolveProbeTasks: %v", err)
	}
	var results []ProbeResult

	for _, task := range tasks {
		dockerArgs := buildProbeDockerArgs("test-image", "bridge", false, task.CommandArgs)
		start := time.Now()
		out, err := probeRunner(context.Background(), dockerArgs)
		elapsed := time.Since(start)

		exitCode := 0
		if err != nil {
			exitCode = 1
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
	if exitCode != 2 {
		t.Errorf("expected exitCode 2 (AUTH_REQUIRED), got %d", exitCode)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Status != "PASS" || results[0].Summary != "10.9.0" {
		t.Errorf("npm result unexpected: %+v", results[0])
	}
	if results[1].Status != "AUTH_REQUIRED" || !strings.Contains(results[1].Summary, "401") {
		t.Errorf("pip result unexpected: %+v", results[1])
	}
}

func TestRuntimeProbeCmdEmptyConfigExitsWithCode3(t *testing.T) {
	t.Setenv("NPM_CONFIG_REGISTRY", "")
	t.Setenv("PIP_INDEX_URL", "")
	t.Setenv("GOPROXY", "")
	t.Setenv("MULTIGENT_CONFIG", "")

	loadedConfig = &appconfig.Config{}
	defer func() { loadedConfig = nil }()

	cmd := newRuntimeProbeCmd()
	err := cmd.RunE(cmd, []string{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var exitErr *ProbeExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ProbeExitError, got %T: %v", err, err)
	}
	if exitErr.Code != 3 {
		t.Fatalf("expected exit code 3, got %d", exitErr.Code)
	}
}
