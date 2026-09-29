package imbridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeMattermostUsersMe(t *testing.T, botID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       botID,
			"username": botID + "-user",
			"is_bot":   true,
			"roles":    "system_user",
		})
	}))
}

// TestGenerateBridgeHmacSecretStrength checks the generated secret is 32 random
// bytes hex-encoded and that repeated calls never collide.
func TestGenerateBridgeHmacSecretStrength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := generateBridgeHmacSecret()
		if err != nil {
			t.Fatalf("generateBridgeHmacSecret: %v", err)
		}
		raw, err := hex.DecodeString(s)
		if err != nil {
			t.Fatalf("not hex: %v", err)
		}
		if len(raw) != 32 {
			t.Fatalf("expected 32 bytes, got %d", len(raw))
		}
		if seen[s] {
			t.Fatalf("duplicate secret generated: %s", s)
		}
		seen[s] = true
	}
}

// TestManualSetupAutoGeneratesBridgeHmacSecret drives ManualSetup with an empty
// bridgeHmacSecret against a fake Mattermost and asserts the stored secret is a
// fresh 256-bit hex string (not empty, not an error).
func TestManualSetupAutoGeneratesBridgeHmacSecret(t *testing.T) {
	srv := newFakeMattermostUsersMe(t, "botautogen1")
	defer srv.Close()
	p := mattermostProvider{}
	res, err := p.ManualSetup(context.Background(), ManualSetupRequest{Values: map[string]string{
		"baseUrl":  srv.URL,
		"botToken": "tok",
	}})
	if err != nil {
		t.Fatalf("ManualSetup: %v", err)
	}
	got := strings.TrimSpace(res.SecretValues["bridgeHmacSecret"])
	if got == "" {
		t.Fatal("expected auto-generated bridgeHmacSecret, got empty")
	}
	raw, err := hex.DecodeString(got)
	if err != nil || len(raw) != 32 {
		t.Fatalf("expected 32-byte hex secret, got %q (err=%v)", got, err)
	}
}

// TestManualSetupRespectsExplicitBridgeHmacSecret asserts a caller-provided
// secret survives (rotation/back-compat path).
func TestManualSetupRespectsExplicitBridgeHmacSecret(t *testing.T) {
	srv := newFakeMattermostUsersMe(t, "botexplicit1")
	defer srv.Close()
	p := mattermostProvider{}
	res, err := p.ManualSetup(context.Background(), ManualSetupRequest{Values: map[string]string{
		"baseUrl":          srv.URL,
		"botToken":         "tok",
		"bridgeHmacSecret": "my-rotation-managed-secret",
	}})
	if err != nil {
		t.Fatalf("ManualSetup: %v", err)
	}
	if res.SecretValues["bridgeHmacSecret"] != "my-rotation-managed-secret" {
		t.Fatalf("explicit secret not respected: %q", res.SecretValues["bridgeHmacSecret"])
	}
}

// TestMattermostHmacFieldOptional pins the field metadata: required=false so
// the console renders it without the mandatory marker.
func TestMattermostHmacFieldOptional(t *testing.T) {
	p, ok := LookupProvider("mattermost")
	if !ok {
		t.Fatal("mattermost provider not registered")
	}
	for _, f := range p.Info().Fields {
		if f.Name == "bridgeHmacSecret" && f.Required {
			t.Fatal("bridgeHmacSecret should not be required (auto-generated when empty)")
		}
	}
}
