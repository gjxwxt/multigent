package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDB is a minimal ControlDB stub.
type fakeDB struct {
	audit      *SecretsAudit
	auditErr   error
	conns      []ConnectionInfo
	providers  []ProviderInfo
	closeCalls int
}

func (f *fakeDB) AuditSecrets() (*SecretsAudit, error) { return f.audit, f.auditErr }
func (f *fakeDB) ListConnections() ([]ConnectionInfo, error) {
	return f.conns, nil
}
func (f *fakeDB) ListModelProviders() ([]ProviderInfo, error) {
	return f.providers, nil
}
func (f *fakeDB) Close() error {
	f.closeCalls++
	return nil
}

func TestRunDataDirChecks(t *testing.T) {
	t.Run("missing data dir is blocking fail", func(t *testing.T) {
		r := Run(context.Background(), Options{DataDir: ""})
		check := findCheck(t, r, "data-dir")
		if check.Status != "fail" || !check.Blocking {
			t.Fatalf("expected blocking fail, got %#v", check)
		}
	})

	t.Run("writable data dir passes and probe file is removed", func(t *testing.T) {
		dir := t.TempDir()
		r := Run(context.Background(), Options{DataDir: dir})
		check := findCheck(t, r, "data-dir-writable")
		if check.Status != "pass" {
			t.Fatalf("expected pass, got %#v", check)
		}
		if _, err := os.Stat(filepath.Join(dir, ".doctor-write-probe")); !os.IsNotExist(err) {
			t.Fatalf("write probe file was not removed")
		}
	})
}

func TestRunSecretsBaseline(t *testing.T) {
	t.Run("plaintext with REQUIRE on is blocking", func(t *testing.T) {
		r := Run(context.Background(), Options{
			DataDir: t.TempDir(),
			OpenControlDB: func() (ControlDB, error) {
				return &fakeDB{audit: &SecretsAudit{PlaintextTotal: 2, PlaintextTables: []string{"model_providers[prov-1]"}, RequireEncrypted: true}}, nil
			},
		})
		check := findCheck(t, r, "secrets-baseline")
		if check.Status != "fail" || !check.Blocking {
			t.Fatalf("expected blocking fail, got %#v", check)
		}
		if !strings.Contains(check.Detail, "fail-closed") {
			t.Fatalf("detail should mention fail-closed: %q", check.Detail)
		}
	})

	t.Run("plaintext with REQUIRE off warns and reports count", func(t *testing.T) {
		r := Run(context.Background(), Options{
			DataDir: t.TempDir(),
			OpenControlDB: func() (ControlDB, error) {
				return &fakeDB{audit: &SecretsAudit{PlaintextTotal: 3, PlaintextTables: []string{"model_providers[a]", "connections[b]", "model_providers[c]"}}}, nil
			},
		})
		check := findCheck(t, r, "secrets-baseline")
		if check.Status != "warn" {
			t.Fatalf("expected warn, got %#v", check)
		}
		if !strings.Contains(check.Detail, "3 plaintext") {
			t.Fatalf("detail should carry count: %q", check.Detail)
		}
	})

	t.Run("clean baseline passes", func(t *testing.T) {
		r := Run(context.Background(), Options{
			DataDir: t.TempDir(),
			OpenControlDB: func() (ControlDB, error) {
				return &fakeDB{audit: &SecretsAudit{Encrypted: 7}}, nil
			},
		})
		check := findCheck(t, r, "secrets-baseline")
		if check.Status != "pass" {
			t.Fatalf("expected pass, got %#v", check)
		}
	})
}

func TestEnvTreeRedaction(t *testing.T) {
	t.Setenv("MULTIGENT_CONNECTION_ENCRYPTION_KEY", "super-secret-value")
	t.Setenv("MULTIGENT_DATA_DIR", "/tmp/doctor-test-data")
	t.Setenv("MULTIGENT_SERVER_ADDR", "127.0.0.1:27892")

	r := Run(context.Background(), Options{DataDir: "/tmp/doctor-test-data", SkipProbes: true})
	text := r.RenderText()
	if strings.Contains(text, "super-secret-value") {
		t.Fatalf("sensitive value leaked into text output")
	}
	marshaled := r.MarshalJSONIndented()
	if strings.Contains(marshaled, "super-secret-value") {
		t.Fatalf("sensitive value leaked into JSON output")
	}

	var keyEntry, addrEntry *EnvEntry
	for i := range r.Env {
		switch r.Env[i].Key {
		case "MULTIGENT_CONNECTION_ENCRYPTION_KEY":
			keyEntry = &r.Env[i]
		case "MULTIGENT_SERVER_ADDR":
			addrEntry = &r.Env[i]
		case "MULTIGENT_DATA_DIR":
			// also assert below
		}
	}
	if keyEntry == nil || !keyEntry.Redacted || keyEntry.Value != "set (redacted)" {
		t.Fatalf("sensitive key not redacted: %+v", keyEntry)
	}
	if addrEntry == nil || addrEntry.Value != "127.0.0.1:27892" {
		t.Fatalf("plain key value lost: %+v", addrEntry)
	}
}

func TestGitLabProbe(t *testing.T) {
	t.Run("live probe success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("PRIVATE-TOKEN") != "token-123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		db := &fakeDB{conns: []ConnectionInfo{{
			ID: "conn-1", Provider: "gitlab", Name: "local", BaseURL: srv.URL, CheckToken: "token-123",
		}}}
		r := Run(context.Background(), Options{
			DataDir:      t.TempDir(),
			OpenControlDB: func() (ControlDB, error) { return db, nil },
		})
		check := findCheck(t, r, "gitlab:conn-1")
		if check.Status != "pass" {
			t.Fatalf("expected pass, got %#v", check)
		}
	})

	t.Run("bad credential fails the check", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		db := &fakeDB{conns: []ConnectionInfo{{
			ID: "conn-2", Provider: "gitlab", BaseURL: srv.URL, CheckToken: "wrong",
		}}}
		r := Run(context.Background(), Options{
			DataDir:      t.TempDir(),
			OpenControlDB: func() (ControlDB, error) { return db, nil },
		})
		check := findCheck(t, r, "gitlab:conn-2")
		if check.Status != "fail" {
			t.Fatalf("expected fail, got %#v", check)
		}
		if !strings.Contains(check.Detail, "401") {
			t.Fatalf("detail should carry HTTP status: %q", check.Detail)
		}
	})

	t.Run("missing token warns instead of probing", func(t *testing.T) {
		db := &fakeDB{conns: []ConnectionInfo{{
			ID: "conn-3", Provider: "gitlab", BaseURL: "http://gitlab.internal",
		}}}
		r := Run(context.Background(), Options{
			DataDir:      t.TempDir(),
			OpenControlDB: func() (ControlDB, error) { return db, nil },
		})
		check := findCheck(t, r, "gitlab:conn-3")
		if check.Status != "warn" {
			t.Fatalf("expected warn, got %#v", check)
		}
	})
}

func TestProviderProbe(t *testing.T) {
	t.Run("openai-compatible /models reachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		db := &fakeDB{providers: []ProviderInfo{{ID: "prov-1", Name: "relay", Type: "openai", BaseURL: srv.URL, APIKey: "sk-x"}}}
		r := Run(context.Background(), Options{
			DataDir:      t.TempDir(),
			OpenControlDB: func() (ControlDB, error) { return db, nil },
		})
		check := findCheck(t, r, "llm:prov-1")
		if check.Status != "pass" {
			t.Fatalf("expected pass, got %#v", check)
		}
	})

	t.Run("provider outage is warn not fail", func(t *testing.T) {
		db := &fakeDB{providers: []ProviderInfo{{ID: "prov-2", Name: "down", Type: "openai", BaseURL: "http://127.0.0.1:1"}}}
		r := Run(context.Background(), Options{
			DataDir:      t.TempDir(),
			ProbeTimeout: 500 * time.Millisecond,
			OpenControlDB: func() (ControlDB, error) { return db, nil },
		})
		check := findCheck(t, r, "llm:prov-2")
		if check.Status != "warn" {
			t.Fatalf("expected warn, got %#v", check)
		}
	})
}

func TestDockerCheck(t *testing.T) {
	t.Run("docker failure is warn", func(t *testing.T) {
		r := Run(context.Background(), Options{
			DataDir:     t.TempDir(),
			SkipProbes:  true,
			DockerProbe: func() error { return fmt.Errorf("cannot connect to the Docker daemon") },
		})
		check := findCheck(t, r, "docker")
		if check.Status != "warn" {
			t.Fatalf("expected warn, got %#v", check)
		}
	})
	t.Run("docker pass", func(t *testing.T) {
		r := Run(context.Background(), Options{
			DataDir:     t.TempDir(),
			SkipProbes:  true,
			DockerProbe: func() error { return nil },
		})
		check := findCheck(t, r, "docker")
		if check.Status != "pass" {
			t.Fatalf("expected pass, got %#v", check)
		}
	})
}

func TestSummary(t *testing.T) {
	r := &Report{Checks: []Check{
		{Name: "a", Status: "pass"},
		{Name: "b", Status: "warn"},
		{Name: "c", Status: "fail", Blocking: true},
	}}
	blocking, failed, warned := r.Summary()
	if !blocking || failed != 1 || warned != 1 {
		t.Fatalf("summary = %v/%d/%d, want true/1/1", blocking, failed, warned)
	}
}

func findCheck(t *testing.T, r *Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in report", name)
	return Check{}
}
