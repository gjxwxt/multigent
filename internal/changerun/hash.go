package changerun

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// hashFile returns the sha256 of a file's content ("absent" is handled by
// callers — a missing file is an error here).
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256Hex hashes a byte slice.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
