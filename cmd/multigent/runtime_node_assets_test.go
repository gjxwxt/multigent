package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigent/multigent/internal/assets"
	"github.com/multigent/multigent/internal/runtimeexec"
)

func TestRuntimeNodeStageTaskAssets(t *testing.T) {
	content := "# 需求\n\n## 1. 范围\n正文"
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])

	var gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/runtime-node/assets/"+sha {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(mock.Close)

	cfg := runtimeNodeConfig{ServerURL: mock.URL, Token: "node-token-1"}
	workspaceRoot := t.TempDir()
	cacheDir, err := runtimeNodeStageTaskAssets(cfg, workspaceRoot, "rtrun-1", []runtimeexec.SpecAsset{
		{Sha256: sha, DisplayName: "需求说明.md", Role: "requirement_input", Required: true},
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if gotAuth != "Bearer node-token-1" {
		t.Fatalf("node token must be presented, got %q", gotAuth)
	}
	if cacheDir != filepath.Join(workspaceRoot, ".multigent", "assets-cache", "rtrun-1") {
		t.Fatalf("cache dir layout mismatch: %s", cacheDir)
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, assets.RelPathFor(sha, "需求说明.md")))
	if err != nil || string(data) != content {
		t.Fatalf("staged bytes mismatch: err=%v data=%q", err, data)
	}
	_ = os.RemoveAll(cacheDir)
}

func TestRuntimeNodeStageTaskAssetsHashMismatchFails(t *testing.T) {
	content := "honest bytes"
	sha := strings.Repeat("a", 64) // does not match the content
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(mock.Close)

	cfg := runtimeNodeConfig{ServerURL: mock.URL, Token: "node-token-1"}
	if _, err := runtimeNodeStageTaskAssets(cfg, t.TempDir(), "rtrun-2", []runtimeexec.SpecAsset{
		{Sha256: sha, DisplayName: "doc.md"},
	}); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("downloaded bytes failing SHA verification must abort staging, got err=%v", err)
	}
}
