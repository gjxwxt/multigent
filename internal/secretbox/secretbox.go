package secretbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	EnvKey = "MULTIGENT_CONNECTION_ENCRYPTION_KEY"

	versionPlain       = "plain-dev"
	versionEnvV1       = "env-v1"
	VersionEnvV1Strict = "env-v1-strict"
	prefix             = "sealed:"
)

type Box struct {
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce,omitempty"`
	KeyVersion string `json:"keyVersion"`
}

func SealString(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	raw := []byte(value)
	box := Box{KeyVersion: versionPlain, Ciphertext: base64.StdEncoding.EncodeToString(raw)}
	key := strings.TrimSpace(os.Getenv(EnvKey))
	if key != "" {
		sealed, err := sealEnvV1(raw, key)
		if err != nil {
			return "", err
		}
		box = sealed
	} else if os.Getenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS") != "" && requireEncryptedSecretsFlag() {
		return "", fmt.Errorf("%s is required (MULTIGENT_REQUIRE_ENCRYPTED_SECRETS is set; refusing to store provider API keys in plaintext)", EnvKey)
	}
	payload, err := json.Marshal(box)
	if err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(payload), nil
}

// requireEncryptedSecretsFlag mirrors internal/db's envFlagSet for the
// MULTIGENT_REQUIRE_ENCRYPTED_SECRETS hard gate.
func requireEncryptedSecretsFlag() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MULTIGENT_REQUIRE_ENCRYPTED_SECRETS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func sealEnvV1(raw []byte, key string) (Box, error) {
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return Box{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Box{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Box{}, err
	}
	return Box{
		Ciphertext: base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, raw, nil)),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		KeyVersion: versionEnvV1,
	}, nil
}

func OpenString(sealed string) (string, error) {
	sealed = strings.TrimSpace(sealed)
	if sealed == "" {
		return "", nil
	}
	if !strings.HasPrefix(sealed, prefix) {
		return "", fmt.Errorf("secret is not sealed")
	}
	rawBox, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		return "", err
	}
	var box Box
	if err := json.Unmarshal(rawBox, &box); err != nil {
		return "", err
	}
	switch box.KeyVersion {
	case versionPlain:
		raw, err := base64.StdEncoding.DecodeString(box.Ciphertext)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	case versionEnvV1:
		key := strings.TrimSpace(os.Getenv(EnvKey))
		if key == "" {
			return "", fmt.Errorf("%s is required to decrypt secret", EnvKey)
		}
		sum := sha256.Sum256([]byte(key))
		block, err := aes.NewCipher(sum[:])
		if err != nil {
			return "", err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return "", err
		}
		nonce, err := base64.StdEncoding.DecodeString(box.Nonce)
		if err != nil {
			return "", err
		}
		ciphertext, err := base64.StdEncoding.DecodeString(box.Ciphertext)
		if err != nil {
			return "", err
		}
		opened, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return "", err
		}
		return string(opened), nil
	default:
		return "", fmt.Errorf("unsupported secret version %q", box.KeyVersion)
	}
}

func IsSealed(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), prefix)
}

// SealBytesStrict seals arbitrary raw bytes preserving exact byte fidelity
// (no trim, no whitespace alterations, supports binary/newlines/control chars).
// It strictly requires MULTIGENT_CONNECTION_ENCRYPTION_KEY and fails closed
// (never falls back to plain-dev or unencrypted storage).
func SealBytesStrict(raw []byte) (string, error) {
	key := strings.TrimSpace(os.Getenv(EnvKey))
	if key == "" {
		return "", fmt.Errorf("%s is required for strict byte encryption (refusing plaintext)", EnvKey)
	}

	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nil, nonce, raw, nil)
	box := Box{
		KeyVersion: VersionEnvV1Strict,
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
	}

	payload, err := json.Marshal(box)
	if err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(payload), nil
}

// OpenBytesStrict decrypts a strict sealed string preserving exact raw bytes.
// It strictly rejects plain-dev and legacy secret versions.
func OpenBytesStrict(sealed string) ([]byte, error) {
	if sealed == "" {
		return nil, nil
	}
	if !strings.HasPrefix(sealed, prefix) {
		return nil, fmt.Errorf("strict secret is not sealed")
	}

	rawBox, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		return nil, fmt.Errorf("decode sealed box: %w", err)
	}
	var box Box
	if err := json.Unmarshal(rawBox, &box); err != nil {
		return nil, fmt.Errorf("unmarshal sealed box: %w", err)
	}

	if box.KeyVersion != VersionEnvV1Strict {
		return nil, fmt.Errorf("unsupported strict secret version %q (plain-dev and legacy versions not permitted)", box.KeyVersion)
	}

	key := strings.TrimSpace(os.Getenv(EnvKey))
	if key == "" {
		return nil, fmt.Errorf("%s is required to decrypt strict secret", EnvKey)
	}

	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce, err := base64.StdEncoding.DecodeString(box.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(box.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}

	opened, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt strict secret: %w", err)
	}
	return opened, nil
}

// StrictEncryptionSelfTest verifies that the encryption key is configured
// and capable of faithful round-trip encryption/decryption of arbitrary bytes.
func StrictEncryptionSelfTest() error {
	key := strings.TrimSpace(os.Getenv(EnvKey))
	if key == "" {
		return fmt.Errorf("%s is not configured", EnvKey)
	}

	// Probe with leading/trailing whitespace, CR LF, null bytes, and UTF-8 multibyte
	probe := []byte("  \t\r\n--- PROBE STRICT PATCH ---\r\n\x00\x01\x02\xff\xfe\x00中文测试  \r\n\t")
	sealed, err := SealBytesStrict(probe)
	if err != nil {
		return fmt.Errorf("seal probe: %w", err)
	}
	opened, err := OpenBytesStrict(sealed)
	if err != nil {
		return fmt.Errorf("open probe: %w", err)
	}
	if !bytes.Equal(opened, probe) {
		return fmt.Errorf("strict encryption self-test failed: byte mismatch on roundtrip")
	}
	return nil
}
