package secretbox

import (
	"strings"
	"testing"
)

func TestSealStringWithoutEnvKeyDoesNotStoreRawValue(t *testing.T) {
	t.Setenv(EnvKey, "")

	sealed, err := SealString("sk-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !IsSealed(sealed) {
		t.Fatalf("secret should be sealed: %q", sealed)
	}
	if strings.Contains(sealed, "sk-secret") {
		t.Fatalf("sealed value leaked raw secret: %q", sealed)
	}

	opened, err := OpenString(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != "sk-secret" {
		t.Fatalf("opened=%q", opened)
	}
}

func TestSealStringWithEnvKeyEncryptsAndOpensValue(t *testing.T) {
	t.Setenv(EnvKey, "test-encryption-key")

	sealed, err := SealString("sk-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !IsSealed(sealed) {
		t.Fatalf("secret should be sealed: %q", sealed)
	}
	if strings.Contains(sealed, "sk-secret") || strings.Contains(sealed, "plain-dev") {
		t.Fatalf("sealed value should not expose plaintext metadata: %q", sealed)
	}

	opened, err := OpenString(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != "sk-secret" {
		t.Fatalf("opened=%q", opened)
	}
}

func TestOpenStringRejectsPlaintext(t *testing.T) {
	if _, err := OpenString("sk-secret"); err == nil {
		t.Fatal("expected plaintext secret to be rejected")
	}
}

func TestSealStringRefusesPlaintextUnderHardGate(t *testing.T) {
	t.Setenv(EnvKey, "")
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "1")
	if _, err := SealString("sk-secret"); err == nil || !strings.Contains(err.Error(), "MULTIGENT_REQUIRE_ENCRYPTED_SECRETS") {
		t.Fatalf("SealString must refuse plaintext under the hard gate, got %v", err)
	}
	t.Setenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS", "")
	sealed, err := SealString("sk-secret")
	if err != nil {
		t.Fatalf("dev fallback broken: %v", err)
	}
	opened, err := OpenString(sealed)
	if err != nil || opened != "sk-secret" {
		t.Fatalf("dev fallback roundtrip failed: %v %q", err, opened)
	}
}
