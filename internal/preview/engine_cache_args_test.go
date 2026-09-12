package preview

import (
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/sandbox"
)

// Regression: preview containers mounted npm/Go cache volumes but never set
// the cache env vars, so the mounts sat unused and every preview re-downloaded
// dependencies (p15 canary §9, GPT review gate 5). Every cache volume must be
// paired with the env var that points the toolchain at it.
func TestPreviewDockerBaseArgsCacheEnvsMatchVolumes(t *testing.T) {
	started := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	args := previewDockerBaseArgs(previewDockerBaseArgsInput{
		ContainerName: "multigent-preview-t-test",
		Port:          28099,
		ProjectName:   "proj",
		TaskID:        "t-test",
		ProjectType:   "fullstack",
		WorktreeDir:   "/tmp/wt",
		StartedAt:     started,
		ExpiresAt:     started.Add(30 * time.Minute),
		ReadOnly:      false,
	})

	joined := strings.Join(args, "\x00")
	pairs := []struct{ volume, env string }{
		{"multigent-npm-cache:" + sandbox.HostUserCacheHome + "/npm", "npm_config_cache=" + sandbox.HostUserCacheHome + "/npm"},
		{"multigent-go-cache:" + sandbox.HostUserCacheHome + "/go/pkg/mod", "GOMODCACHE=" + sandbox.HostUserCacheHome + "/go/pkg/mod"},
		{"multigent-go-build-cache:" + sandbox.HostUserCacheHome + "/go-build", "GOCACHE=" + sandbox.HostUserCacheHome + "/go-build"},
	}
	for _, p := range pairs {
		if !strings.Contains(joined, p.volume) {
			t.Fatalf("missing cache volume %q in args", p.volume)
		}
		if !strings.Contains(joined, "-e\x00"+p.env) {
			t.Fatalf("cache volume %q has no matching env %q", p.volume, p.env)
		}
	}
	if !strings.Contains(joined, "GOPATH="+sandbox.HostUserCacheHome+"/go") {
		t.Fatalf("missing GOPATH env (go install/cache writability depends on it)")
	}
	// GOPATH root must coincide with the mod-cache parent so both point into
	// the same mounted volume.
	if !strings.Contains(joined, "-v\x00multigent-go-cache:"+sandbox.HostUserCacheHome+"/go/pkg/mod") {
		t.Fatalf("go mod cache volume missing or misplaced")
	}
}
