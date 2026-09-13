package secretbox

import (
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

	versionPlain = "plain-dev"
	versionEnvV1 = "env-v1"
	prefix       = "sealed:"
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
