// DefaultContainerGenerator is the V1 disposable-container execution of the
// contract's generator argv. It runs `docker run --rm` against the platform
// base image with ONLY the worktree mounted — no docker socket, no model
// credentials, no host environment — and a hard timeout. The seeded database
// is produced inside the container at the worktree's storage path (e.g.
// server/data/app.db), which lands on the mounted worktree directory.
package fixturesandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GeneratorMaxOutput caps the generator's captured output (abuse bound).
const GeneratorMaxOutput = 1 << 20

// generatorImageEnv / generatorImageRegionEnv mirror the sandbox package's
// runtime image selection (MULTIGENT_RUNTIME_IMAGE explicit override,
// MULTIGENT_RUNTIME_REGION=cn selects the mainland mirror) without importing
// internal/sandbox. Intranet deployments re-point these at a private registry;
// a hardcoded GHCR constant would break fixture generation the moment GHCR is
// unreachable (migration checkpoint 2026-09-15).
const (
	generatorImageEnv       = "MULTIGENT_RUNTIME_IMAGE"
	generatorImageRegionEnv = "MULTIGENT_RUNTIME_REGION"
	generatorImageDefault   = "ghcr.io/multigent/multigent/runtime-base:latest"
	generatorImageCN        = "crpi-fu3b7e7lggtmh7za.cn-hangzhou.personal.cr.aliyuncs.com/multigent/runtime-base:latest"
)

// GeneratorImage resolves the disposable image the generator runs in. It
// carries node/go toolchains so contract commands like
// `npm run db:seed:baseline` or `go run ./cmd/dbseed baseline` work out of
// the box. Resolution matches sandbox.DefaultBaseImage: explicit override,
// then region mirror, then the published GHCR default.
func GeneratorImage() string {
	if image := strings.TrimSpace(os.Getenv(generatorImageEnv)); image != "" {
		return image
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(generatorImageRegionEnv))) {
	case "cn", "china", "zh-cn", "mainland", "mainland-china":
		return generatorImageCN
	default:
		return generatorImageDefault
	}
}

// DefaultContainerGenerator implements ContainerGenerator with docker run.
func DefaultContainerGenerator(ctx context.Context, worktreeDir, argv string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// argv is a shell line from the project contract (operator-reviewed);
	// it runs inside the disposable container, not on the host.
	args := []string{
		"run", "--rm",
		"-v", worktreeDir + ":/workspace",
		"-w", "/workspace",
		"-e", "APP_DB_PATH=/workspace/server/data/app.db",
		GeneratorImage(),
		"sh", "-c", argv,
	}
	cmd := exec.CommandContext(runCtx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("generator timed out after %s", timeout)
	}
	if err != nil {
		tail := out.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return "", fmt.Errorf("generator container failed: %w — output tail: %s", err, strings.TrimSpace(tail))
	}
	return "/workspace/server/data/app.db", nil
}
