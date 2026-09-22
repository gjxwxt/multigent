package gitworktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// fileFingerprint hashes a file's content AND its permission mode so the QA
// baseline delta can detect same-path content changes that a path-set
// comparison would miss (file modified before the task AND again by the
// task; file committed and then modified again) plus content-identical
// permission flips (chmod +x on a script is a delivery, not noise). Symbolic
// links are fingerprinted as "symlink:<target>" so retargeting (or replacing
// a symlink with a regular file) diffs cleanly. Missing files are
// fingerprinted as "deleted" so deletions diff cleanly against the baseline.
func fileFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "deleted", nil
		}
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("readlink %s: %w", path, err)
		}
		return "symlink:" + target, nil
	}
	if !info.Mode().IsRegular() {
		// Devices, sockets, fifos: pin the mode so a swap between two
		// special types still shows up as a delta.
		return fmt.Sprintf("special:%s", info.Mode()), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	// Mode prefix (perm bits only): content hash follows, so the same bytes
	// with different permission bits produce different fingerprints.
	fmt.Fprintf(h, "mode:%o\n", uint32(info.Mode().Perm()))
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unquoteGitPath decodes a C-quoted git path (octal escapes, backslashes)
// as produced by git status --porcelain -z for non-ASCII names.
func unquoteGitPath(p string) string {
	if len(p) < 2 || p[0] != '"' || p[len(p)-1] != '"' {
		return p
	}
	p = p[1 : len(p)-1]
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+1 < len(p) && p[i+1] >= '0' && p[i+1] <= '7' {
			// Exactly three octal digits (git's C-quote format); a short or
			// non-octal tail is kept literal instead of silently decoding.
			if i+3 >= len(p) || p[i+2] < '0' || p[i+2] > '7' || p[i+3] < '0' || p[i+3] > '7' {
				b.WriteByte(p[i])
				continue
			}
			v := int(p[i+1]-'0')*64 + int(p[i+2]-'0')*8 + int(p[i+3]-'0')
			if v > 255 {
				b.WriteByte(p[i])
				continue
			}
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
