package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// sealAESGCM is the shared AES-GCM sealing primitive for the audit/migration
// helpers. nonceHex selects the hex nonce spelling used by OAuth rows;
// connection/model-provider envelopes use base64.
func sealAESGCM(raw []byte, key, keyVersion string, nonceHex bool) (ConnectionSecret, error) {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return ConnectionSecret{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ConnectionSecret{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return ConnectionSecret{}, err
	}
	sealed := gcm.Seal(nil, nonce, raw, nil)
	nonceText := base64.StdEncoding.EncodeToString(nonce)
	if nonceHex {
		nonceText = hex.EncodeToString(nonce)
	}
	return ConnectionSecret{
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
		Nonce:      nonceText,
		KeyVersion: keyVersion,
	}, nil
}
