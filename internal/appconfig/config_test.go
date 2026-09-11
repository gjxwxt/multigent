package appconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[workspace]
dir = "/tmp/multigent"

[server]
addr = "0.0.0.0:8080"

[auth]
api_key = "secret"

[smtp]
host = "smtp.example.com"
port = 465
from = "noreply@example.com"
tls = "implicit"

[logging]
file = "/tmp/multigent.log"
level = "debug"
format = "json"
max_size_mb = 20
stderr = false

[runtime]
image = "registry.example.com/multigent/runtime-base:latest"
region = "cn"
npm_registry = "https://registry.npmmirror.com"

[sandbox]
allow_direct_host = false

[sandbox.e2b]
api_url = "http://127.0.0.1:49999"

[playbooks]
registry_urls = [
  "file:///tmp/playbooks.json",
  "https://example.com/registry.json",
]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Dir != "/tmp/multigent" || cfg.Server.Addr != "0.0.0.0:8080" || cfg.Auth.APIKey != "secret" {
		t.Fatalf("basic config not loaded: %#v", cfg)
	}
	if cfg.SMTP.Port != 465 || cfg.SMTP.TLS != "implicit" {
		t.Fatalf("smtp config not loaded: %#v", cfg.SMTP)
	}
	if cfg.Logging.Stderr == nil || *cfg.Logging.Stderr {
		t.Fatalf("stderr bool not loaded: %#v", cfg.Logging)
	}
	if cfg.Runtime.Image != "registry.example.com/multigent/runtime-base:latest" || cfg.Runtime.Region != "cn" || cfg.Runtime.NPMRegistry != "https://registry.npmmirror.com" {
		t.Fatalf("runtime config not loaded: %#v", cfg.Runtime)
	}
	if cfg.Sandbox.E2B.APIURL == "" {
		t.Fatalf("e2b api url missing")
	}
	if cfg.Sandbox.AllowDirectHost == nil || *cfg.Sandbox.AllowDirectHost {
		t.Fatalf("sandbox allow_direct_host not loaded: %#v", cfg.Sandbox.AllowDirectHost)
	}
	if len(cfg.Playbooks.RegistryURLs) != 2 || cfg.Playbooks.RegistryURLs[0] != "file:///tmp/playbooks.json" || cfg.Playbooks.RegistryURLs[1] != "https://example.com/registry.json" {
		t.Fatalf("playbook registry urls not loaded: %#v", cfg.Playbooks.RegistryURLs)
	}
}

func TestLoadConfigRegistriesAndNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config_registries.toml")
	body := `
[registries]
npm = "https://nexus.corp.example/repository/npm-group/"
pip = "https://nexus.corp.example/repository/pypi/simple/"
go = "https://nexus.corp.example/repository/go/"
go_sumdb = "sum.corp.example"
go_private = "git.corp.example/*"

[network]
https_proxy = "http://proxy.corp.example:3128"
http_proxy = "http://proxy.corp.example:3128"
no_proxy = "localhost,127.0.0.1,.corp.example"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load registries & network: %v", err)
	}
	if cfg.Registries.NPM != "https://nexus.corp.example/repository/npm-group/" {
		t.Errorf("Registries.NPM = %q", cfg.Registries.NPM)
	}
	if cfg.Registries.PIP != "https://nexus.corp.example/repository/pypi/simple/" {
		t.Errorf("Registries.PIP = %q", cfg.Registries.PIP)
	}
	if cfg.Registries.Go != "https://nexus.corp.example/repository/go/" {
		t.Errorf("Registries.Go = %q", cfg.Registries.Go)
	}
	if cfg.Registries.GoSumDB != "sum.corp.example" {
		t.Errorf("Registries.GoSumDB = %q", cfg.Registries.GoSumDB)
	}
	if cfg.Registries.GoPrivate != "git.corp.example/*" {
		t.Errorf("Registries.GoPrivate = %q", cfg.Registries.GoPrivate)
	}
	if cfg.Network.HTTPSProxy != "http://proxy.corp.example:3128" {
		t.Errorf("Network.HTTPSProxy = %q", cfg.Network.HTTPSProxy)
	}
	if cfg.Network.HTTPProxy != "http://proxy.corp.example:3128" {
		t.Errorf("Network.HTTPProxy = %q", cfg.Network.HTTPProxy)
	}
	if cfg.Network.NoProxy != "localhost,127.0.0.1,.corp.example" {
		t.Errorf("Network.NoProxy = %q", cfg.Network.NoProxy)
	}
}

func TestLoadConfigUnknownSectionFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config_unknown_sec.toml")
	body := `
[unknown_section]
foo = "bar"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error on unknown section, got nil")
	}
}

func TestLoadConfigUnknownKeyInEachSectionFails(t *testing.T) {
	sections := []string{
		"workspace", "server", "auth", "smtp", "logging",
		"runtime", "sandbox", "sandbox.e2b", "playbooks",
		"registries", "network",
	}
	for _, sec := range sections {
		t.Run(sec, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config_bad_key.toml")
			body := fmt.Sprintf("[%s]\nunknown_test_key_xyz = \"val\"\n", sec)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for unknown key in section [%s], got nil", sec)
			}
		})
	}
}

func TestLoadConfigValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{
			name: "registry not https",
			toml: "[registries]\nnpm = \"http://nexus.corp.example/\"\n",
		},
		{
			name: "registry contains userinfo",
			toml: "[registries]\nnpm = \"https://user:pass@nexus.corp.example/\"\n",
		},
		{
			name: "registry contains query",
			toml: "[registries]\npip = \"https://nexus.corp.example/?foo=bar\"\n",
		},
		{
			name: "go registry contains direct",
			toml: "[registries]\ngo = \"https://nexus.corp.example/,direct\"\ngo_sumdb = \"sum.corp.example\"\n",
		},
		{
			name: "go registry missing go_sumdb",
			toml: "[registries]\ngo = \"https://nexus.corp.example/\"\n",
		},
		{
			name: "go_sumdb is off",
			toml: "[registries]\ngo = \"https://nexus.corp.example/\"\ngo_sumdb = \"off\"\n",
		},
		{
			name: "proxy contains userinfo",
			toml: "[network]\nhttps_proxy = \"http://user:pass@proxy.corp.example:3128\"\n",
		},
		{
			name: "registry missing host",
			toml: "[registries]\nnpm = \"https:///path/only\"\n",
		},
		{
			name: "proxy missing host",
			toml: "[network]\nhttps_proxy = \"http:///path/only\"\n",
		},
		{
			name: "go registry token is off",
			toml: "[registries]\ngo = \"off\"\ngo_sumdb = \"sum.corp.example\"\n",
		},
		{
			name: "go registry token is direct",
			toml: "[registries]\ngo = \"direct\"\ngo_sumdb = \"sum.corp.example\"\n",
		},
		{
			name: "go registry comma separated with direct token",
			toml: "[registries]\ngo = \"https://proxy1.corp.example/go,direct\"\ngo_sumdb = \"sum.corp.example\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config_err.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}

func TestLoadConfigValidDirectInPathAndMultiProxy(t *testing.T) {
	toml := `
[registries]
go = "https://nexus.corp.example/repository/go-direct-proxy/,https://mirror.corp.example/go/"
go_sumdb = "sum.corp.example"
`
	path := filepath.Join(t.TempDir(), "config_ok.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error for valid go proxies: %v", err)
	}
	if cfg.Registries.Go != "https://nexus.corp.example/repository/go-direct-proxy/,https://mirror.corp.example/go/" {
		t.Fatalf("unexpected go registry value: %s", cfg.Registries.Go)
	}
}
