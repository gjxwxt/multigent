package api

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/multigent/multigent/internal/entity"
)

// Deploy port pool: every template-initialized project gets a stable host
// port for CI deployments. The starter's deploy/compose.yml binds
// ${APP_PORT:-8088}, so without allocation every project collides on 8088
// on the deploy host. Allocation is DB-authoritative: the platform picks
// the smallest unused port in the pool, persists it on the project, and
// mirrors it into the GitLab CI/CD variable APP_PORT (CI variables win
// over the compose .env and the yml default). No liveness probing on
// purpose: the deploy host is usually a different machine than the
// server, and a port occupied outside the platform fails loudly at
// `compose up` time anyway.

// Pool bounds stay below both OS ephemeral ranges (Linux 32768+, macOS
// 49152+) so reserved ports never race with outbound-connection port
// selection on the deploy host, and clear of the platform's own 2789x.
const (
	deployPortRangeEnv   = "MULTIGENT_DEPLOY_PORT_RANGE"
	defaultDeployPortMin = 28000
	defaultDeployPortMax = 28999
)

// deployPortMu serializes allocate+save so concurrent initializations
// cannot pick the same port.
var deployPortMu sync.Mutex

// deployPortPoolRange parses MULTIGENT_DEPLOY_PORT_RANGE ("min-max") and
// falls back to 28000-28999.
func deployPortPoolRange() (int, int) {
	raw := strings.TrimSpace(os.Getenv(deployPortRangeEnv))
	if raw == "" {
		return defaultDeployPortMin, defaultDeployPortMax
	}
	lo, hi, ok := strings.Cut(raw, "-")
	if !ok {
		log.Printf("[deploy-port] ignoring invalid %s=%q, using %d-%d", deployPortRangeEnv, raw, defaultDeployPortMin, defaultDeployPortMax)
		return defaultDeployPortMin, defaultDeployPortMax
	}
	min, errMin := strconv.Atoi(strings.TrimSpace(lo))
	max, errMax := strconv.Atoi(strings.TrimSpace(hi))
	if errMin != nil || errMax != nil || min < 1024 || max > 65535 || min > max {
		log.Printf("[deploy-port] ignoring invalid %s=%q, using %d-%d", deployPortRangeEnv, raw, defaultDeployPortMin, defaultDeployPortMax)
		return defaultDeployPortMin, defaultDeployPortMax
	}
	return min, max
}

// ensureDeployPort assigns the project a deploy port when it has none.
// Returns true when a fresh port was assigned; the caller is responsible
// for saving the project. The port only becomes durable with that save,
// so a failed initialization leaks nothing.
func (s *Server) ensureDeployPort(p *entity.Project) (bool, error) {
	if p.DeployPort != 0 {
		return false, nil
	}
	deployPortMu.Lock()
	defer deployPortMu.Unlock()
	projects, err := s.st.ListProjects()
	if err != nil {
		return false, fmt.Errorf("list projects for deploy port allocation: %w", err)
	}
	used := make(map[int]bool)
	for _, other := range projects {
		if other.Name != p.Name && other.DeployPort != 0 {
			used[other.DeployPort] = true
		}
	}
	min, max := deployPortPoolRange()
	for port := min; port <= max; port++ {
		if !used[port] {
			p.DeployPort = port
			return true, nil
		}
	}
	return false, fmt.Errorf("deploy port pool %d-%d exhausted", min, max)
}

// pushDeployPortVariable mirrors the allocated port into the GitLab CI/CD
// variable APP_PORT. Best-effort by design: the local allocation stays
// authoritative, and a failed push only means the next deploy falls back
// to the starter default, so log and move on.
func (s *Server) pushDeployPortVariable(ctx context.Context, project string, p *entity.Project) {
	if p.DeployPort == 0 || strings.TrimSpace(p.RemoteProjectID) == "" {
		return
	}
	host, _, err := s.pinnedGitLabHost(ctx, project, p)
	if err != nil {
		log.Printf("[deploy-port] %s: resolve gitlab host failed, APP_PORT=%d not pushed: %v", project, p.DeployPort, err)
		return
	}
	if err := host.SetProjectVariable(ctx, p.RemoteProjectID, "APP_PORT", strconv.Itoa(p.DeployPort)); err != nil {
		log.Printf("[deploy-port] %s: push APP_PORT=%d to gitlab failed: %v", project, p.DeployPort, err)
		return
	}
	log.Printf("[deploy-port] %s: APP_PORT=%d pushed to gitlab project %s", project, p.DeployPort, p.RemoteProjectID)
}

// defaultRunnerIDEnv names the runner the platform binds into every
// template-initialized project. Shared runners commonly disable
// run_untagged, so a project with no explicit binding stays pending forever
// — the exact deadlock the ias-auth-center pilot hit (runner tags restored
// by hand). Configure MULTIGENT_GITLAB_RUNNER_ID with the shared runner's
// numeric ID and initialization binds it best-effort alongside APP_PORT.
const defaultRunnerIDEnv = "MULTIGENT_GITLAB_RUNNER_ID"

// bindDefaultRunner attaches the configured default runner to the project's
// GitLab remote. Best-effort by design, mirroring pushDeployPortVariable:
// initialization must not fail when the remote is absent, the GitLab host is
// unreachable, or no default runner is configured — handlePutProject
// re-runs this on every remote update.
func (s *Server) bindDefaultRunner(ctx context.Context, project string, p *entity.Project) {
	runnerID := strings.TrimSpace(os.Getenv(defaultRunnerIDEnv))
	if runnerID == "" || strings.TrimSpace(p.RemoteProjectID) == "" {
		return
	}
	host, _, err := s.pinnedGitLabHost(ctx, project, p)
	if err != nil {
		log.Printf("[runner-bind] %s: resolve gitlab host failed, runner %s not bound: %v", project, runnerID, err)
		return
	}
	if err := host.EnableRunnerOnProject(ctx, p.RemoteProjectID, runnerID); err != nil {
		log.Printf("[runner-bind] %s: bind runner %s to gitlab project %s failed: %v", project, runnerID, p.RemoteProjectID, err)
		return
	}
	log.Printf("[runner-bind] %s: runner %s bound to gitlab project %s", project, runnerID, p.RemoteProjectID)
}
