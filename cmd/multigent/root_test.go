package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/multigent/multigent/internal/appconfig"
	"github.com/multigent/multigent/internal/errs"
)

func TestApplyConfigEnvBridging(t *testing.T) {
	// Clean up environment variables after test
	keys := []string{
		"NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "GOPROXY", "GOSUMDB", "GOPRIVATE",
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	cfg := &appconfig.Config{
		Registries: appconfig.RegistriesConfig{
			NPM:       "https://nexus.corp.example/repository/npm-group/",
			PIP:       "https://nexus.corp.example/repository/pypi/simple/",
			Go:        "https://nexus.corp.example/repository/go/",
			GoSumDB:   "sum.corp.example",
			GoPrivate: "git.corp.example/*",
		},
		Network: appconfig.NetworkConfig{
			HTTPSProxy: "http://proxy.corp.example:3128",
			HTTPProxy:  "http://proxy.corp.example:3128",
			NoProxy:    "localhost,127.0.0.1,.corp.example",
		},
	}

	applyConfigEnv(cfg)

	if got := os.Getenv("NPM_CONFIG_REGISTRY"); got != cfg.Registries.NPM {
		t.Errorf("NPM_CONFIG_REGISTRY = %q, want %q", got, cfg.Registries.NPM)
	}
	if got := os.Getenv("PIP_INDEX_URL"); got != cfg.Registries.PIP {
		t.Errorf("PIP_INDEX_URL = %q, want %q", got, cfg.Registries.PIP)
	}
	if got := os.Getenv("GOPROXY"); got != cfg.Registries.Go {
		t.Errorf("GOPROXY = %q, want %q", got, cfg.Registries.Go)
	}
	if got := os.Getenv("GOSUMDB"); got != cfg.Registries.GoSumDB {
		t.Errorf("GOSUMDB = %q, want %q", got, cfg.Registries.GoSumDB)
	}
	if got := os.Getenv("GOPRIVATE"); got != cfg.Registries.GoPrivate {
		t.Errorf("GOPRIVATE = %q, want %q", got, cfg.Registries.GoPrivate)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != cfg.Network.HTTPSProxy {
		t.Errorf("HTTPS_PROXY = %q, want %q", got, cfg.Network.HTTPSProxy)
	}
	if got := os.Getenv("https_proxy"); got != cfg.Network.HTTPSProxy {
		t.Errorf("https_proxy = %q, want %q", got, cfg.Network.HTTPSProxy)
	}
	if got := os.Getenv("HTTP_PROXY"); got != cfg.Network.HTTPProxy {
		t.Errorf("HTTP_PROXY = %q, want %q", got, cfg.Network.HTTPProxy)
	}
	if got := os.Getenv("http_proxy"); got != cfg.Network.HTTPProxy {
		t.Errorf("http_proxy = %q, want %q", got, cfg.Network.HTTPProxy)
	}
	if got := os.Getenv("NO_PROXY"); got != cfg.Network.NoProxy {
		t.Errorf("NO_PROXY = %q, want %q", got, cfg.Network.NoProxy)
	}
	if got := os.Getenv("no_proxy"); got != cfg.Network.NoProxy {
		t.Errorf("no_proxy = %q, want %q", got, cfg.Network.NoProxy)
	}
}

func TestApplyConfigEnvNPMFallback(t *testing.T) {
	t.Setenv("NPM_CONFIG_REGISTRY", "")
	cfg := &appconfig.Config{
		Runtime: appconfig.RuntimeConfig{
			NPMRegistry: "https://registry.npmmirror.com",
		},
	}
	applyConfigEnv(cfg)
	if got := os.Getenv("NPM_CONFIG_REGISTRY"); got != "https://registry.npmmirror.com" {
		t.Errorf("NPM_CONFIG_REGISTRY = %q, want fallback https://registry.npmmirror.com", got)
	}
}

func TestApplyConfigEnvRuntimeProfileBridge(t *testing.T) {
	t.Run("empty environment bridges config profile", func(t *testing.T) {
		t.Setenv("MULTIGENT_RUNTIME_PROFILE", "")
		cfg := &appconfig.Config{
			Runtime: appconfig.RuntimeConfig{Profile: "jvm21"},
		}
		applyConfigEnv(cfg)
		if got := os.Getenv("MULTIGENT_RUNTIME_PROFILE"); got != "jvm21" {
			t.Errorf("MULTIGENT_RUNTIME_PROFILE = %q, want bridged %q", got, "jvm21")
		}
	})
	t.Run("existing environment value wins", func(t *testing.T) {
		t.Setenv("MULTIGENT_RUNTIME_PROFILE", "base")
		cfg := &appconfig.Config{
			Runtime: appconfig.RuntimeConfig{Profile: "jvm21"},
		}
		applyConfigEnv(cfg)
		if got := os.Getenv("MULTIGENT_RUNTIME_PROFILE"); got != "base" {
			t.Errorf("MULTIGENT_RUNTIME_PROFILE = %q, want preset %q to win (setEnvIfEmpty semantics)", got, "base")
		}
	})
}

func TestExitCodeForProbeExitError(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{&ProbeExitError{Code: 0, Msg: "pass"}, 0},
		{&ProbeExitError{Code: 1, Msg: "fail"}, 1},
		{&ProbeExitError{Code: 2, Msg: "auth"}, 2},
		{&ProbeExitError{Code: 3, Msg: "none"}, 3},
		// Wrapped probe errors must still resolve through errors.As.
		{fmt.Errorf("runtime probe: %w", &ProbeExitError{Code: 2, Msg: "auth"}), 2},
	}
	for _, tc := range cases {
		if got := exitCodeFor(tc.err); got != tc.want {
			t.Errorf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestExitCodeForExecExitErrorDoesNotLeakChildCode(t *testing.T) {
	// *exec.ExitError satisfies interface{ ExitCode() int } via ProcessState.
	// It must NOT be matched: child exit codes (docker 125/126/127 etc.) would
	// collide with the CLI's own documented code space (3=not found, 5=conflict).
	err := exec.Command("sh", "-c", "exit 42").Run()
	if err == nil {
		t.Fatalf("expected non-zero exit from sh")
	}
	if got := exitCodeFor(err); got != 1 {
		t.Errorf("exitCodeFor(exec.ExitError with code 42) = %d, want 1 (child codes must not leak)", got)
	}
}

func TestExitCodeForSentinels(t *testing.T) {
	if got := exitCodeFor(errs.NotFound("resource", "x")); got != 3 {
		t.Errorf("exitCodeFor(NotFound) = %d, want 3", got)
	}
	if got := exitCodeFor(errs.Conflict("resource", "x")); got != 5 {
		t.Errorf("exitCodeFor(Conflict) = %d, want 5", got)
	}
	if got := exitCodeFor(errs.Usage("x")); got != 2 {
		t.Errorf("exitCodeFor(Usage) = %d, want 2", got)
	}
	if got := exitCodeFor(errors.New("boom")); got != 1 {
		t.Errorf("exitCodeFor(generic) = %d, want 1", got)
	}
}
