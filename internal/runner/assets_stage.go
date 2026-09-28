package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/multigent/multigent/internal/assets"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/runenv"
	"github.com/multigent/multigent/internal/runtimeexec"
	"github.com/multigent/multigent/internal/sandbox"
)

// Task-asset staging for runs. Two execution topologies share one contract:
//
//   - Control-plane-local task runs (scheduler → RunTaskWithContext): the
//     runner stages the task's bound blobs into a per-run cache dir on this
//     host and mounts it read-only (appendTaskAssetsMount).
//   - Node-dispatched runs: the control plane resolves attachments at
//     spec-build time (enqueueRuntimeTaskRun → runtimeexec.Spec.Assets) and
//     pre-renders the manifest into the prompt; the node downloads and
//     stages, then signals the cache dir via runtimeexec.RuntimeTaskAssetsDirEnv.
//
// The manifest inside the prompt always references sandbox.AssetsMount, so
// the agent sees the same paths regardless of which transport filled them.
//
// FAIL-CLOSED RULE: a task with bound assets must never start a run whose
// manifest points at nothing. Local staging failure fails the run; a node
// staging failure fails the run via the node's fail-report path. A control
// DB read failure inside the prompt renderer degrades to "no manifest"
// (matching workflowPromptContext's fail-open), which the staging/enqueue
// paths then catch as their own hard error — the two layers cannot produce
// a silently asset-less run.

// taskAssetsPromptSection renders the manifest block for the task's bound
// assets, or "" when the task has none (or the control DB cannot be read —
// staging/enqueue paths own the hard failure).
func (r *Runner) taskAssetsPromptSection(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ""
	}
	controlDB, err := controldb.OpenDefault()
	if err != nil {
		return ""
	}
	defer controlDB.Close()
	atts, err := controlDB.ListAssetAttachmentsForTask(taskID)
	if err != nil || len(atts) == 0 {
		return ""
	}
	return assets.ManifestFromAttachments(atts)
}

type stagedTaskAssets struct {
	CacheDir string
	Cleanup  func()
}

// stageLocalTaskAssets materializes the task's bound blobs into a per-run
// cache dir on this host. Returns (nil, nil) for tasks without assets.
// Fail-closed: any missing blob, hash mismatch or budget overflow aborts the
// run before the agent starts.
func (r *Runner) stageLocalTaskAssets(task *entity.Task) (*stagedTaskAssets, error) {
	if task == nil || strings.TrimSpace(task.ID) == "" {
		return nil, nil
	}
	controlDB, err := controldb.OpenDefault()
	if err != nil {
		return nil, fmt.Errorf("task assets: open control db: %w", err)
	}
	defer controlDB.Close()
	atts, err := controlDB.ListAssetAttachmentsForTask(task.ID)
	if err != nil {
		return nil, fmt.Errorf("task assets: list attachments for %s: %w", task.ID, err)
	}
	if len(atts) == 0 {
		return nil, nil
	}
	bs, err := assets.NewBlobStore()
	if err != nil {
		return nil, fmt.Errorf("task assets: resolve blob store: %w", err)
	}
	cacheDir := filepath.Join(r.root, ".multigent", "assets-cache",
		task.ID+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if _, err := assets.StageTaskAssets(controlDB, bs, task.ID, cacheDir); err != nil {
		return nil, err
	}
	return &stagedTaskAssets{
		CacheDir: cacheDir,
		Cleanup:  func() { _ = os.RemoveAll(cacheDir) },
	}, nil
}

// appendTaskAssetsMount adds the read-only assets mount when the run carries
// a staged assets dir. Docker-provider only: the manifest's /mnt/multigent/
// assets contract has no meaning for direct host execution, and a mounted lie
// is worse than a loud refusal (the direct-host callers fail closed instead).
func appendTaskAssetsMount(mounts []entity.RuntimeMount, runtimeEnv map[string]string, provider entity.SandboxProvider) []entity.RuntimeMount {
	dir := strings.TrimSpace(runtimeEnv[runtimeexec.RuntimeTaskAssetsDirEnv])
	if dir == "" || provider != entity.SandboxDocker {
		return mounts
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return mounts
	}
	for _, m := range mounts {
		if m.Target == sandbox.AssetsMount {
			return mounts
		}
	}
	return append(mounts, entity.RuntimeMount{
		Source: dir,
		Target: sandbox.AssetsMount,
		Mode:   runenv.MountModeReadOnly,
		Kind:   "context",
	})
}
