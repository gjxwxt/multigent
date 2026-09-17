package secretbox

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

func TestSealBytesStrictFidelity(t *testing.T) {
	t.Setenv(EnvKey, "strict-test-key-32-bytes-secure!")

	testCases := [][]byte{
		[]byte("simple text"),
		[]byte("   \t\r\n leading and trailing whitespace \r\n\t   "),
		[]byte("diff --git a/file b/file\n--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file"),
		[]byte("\x00\x01\x02\xfe\xff\x00\x00binary\x00nulls\x00"),
		bytes.Repeat([]byte{0x42, 0x00, 0x0a, 0x0d}, 1024), // 4KB mixed with newlines and nulls
	}

	for i, tc := range testCases {
		sealed, err := SealBytesStrict(tc)
		if err != nil {
			t.Fatalf("case %d: SealBytesStrict failed: %v", i, err)
		}
		if !IsSealed(sealed) {
			t.Fatalf("case %d: expected sealed prefix, got %q", i, sealed)
		}

		opened, err := OpenBytesStrict(sealed)
		if err != nil {
			t.Fatalf("case %d: OpenBytesStrict failed: %v", i, err)
		}
		if !bytes.Equal(opened, tc) {
			t.Fatalf("case %d: byte fidelity mismatch:\nexpected: %v\ngot:      %v", i, tc, opened)
		}
	}
}

func TestSealBytesStrictRequiresKey(t *testing.T) {
	t.Setenv(EnvKey, "")

	_, err := SealBytesStrict([]byte("unencrypted patch"))
	if err == nil {
		t.Fatal("expected SealBytesStrict to fail without encryption key")
	}
	if !strings.Contains(err.Error(), EnvKey) {
		t.Fatalf("expected error mentioning %s, got %v", EnvKey, err)
	}
}

func TestOpenBytesStrictRejectsPlainDev(t *testing.T) {
	t.Setenv(EnvKey, "strict-test-key")

	// Create a plain-dev box manually
	plainBox := Box{
		KeyVersion: versionPlain,
		Ciphertext: "dGVzdA==", // base64("test")
	}
	payload, _ := json.Marshal(plainBox)
	plainSealed := prefix + base64.StdEncoding.EncodeToString(payload)

	_, err := OpenBytesStrict(plainSealed)
	if err == nil {
		t.Fatal("expected OpenBytesStrict to reject plain-dev box")
	}
	if !strings.Contains(err.Error(), "unsupported strict secret version") {
		t.Fatalf("expected unsupported version error, got %v", err)
	}
}

func TestStrictEncryptionSelfTest(t *testing.T) {
	// Without key
	t.Setenv(EnvKey, "")
	if err := StrictEncryptionSelfTest(); err == nil {
		t.Fatal("expected StrictEncryptionSelfTest to fail when key is unset")
	}

	// With key
	t.Setenv(EnvKey, "my-super-secret-master-key")
	if err := StrictEncryptionSelfTest(); err != nil {
		t.Fatalf("expected StrictEncryptionSelfTest to succeed, got: %v", err)
	}
}
