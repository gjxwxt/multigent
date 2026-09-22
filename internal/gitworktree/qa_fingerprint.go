package gitworktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// fileFingerprint hashes a file's content so the QA baseline delta can
// detect same-path content changes that a path-set comparison would miss
// (file modified before the task AND again by the task; file committed and
// then modified again). Missing files are fingerprinted as "deleted" so
// deletions diff cleanly against the baseline.
func fileFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "deleted", nil
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unquoteGitPath decodes a C-quoted git path (octal escapes, backslashes)
// as produced by git status --porcelain -z for non-ASCII names.
func unquoteGitPath(p string) string {
	p = p[1 : len(p)-1]
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) && p[i+1] >= '0' && p[i+1] <= '7' {
			var v int
			_, _ = fmt.Sscanf(p[i+1:i+4], "%o", &v)
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		if p[i] == '\\' && i+1 < len(p) {
			i++
			b.WriteByte(p[i])
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}
