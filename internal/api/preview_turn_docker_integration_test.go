package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/previewreceipt"
	"github.com/multigent/multigent/internal/secretbox"
)

// TestPreviewTurn_DockerSandboxExecution_RealContainer validates the end-to-end
// preview turn lifecycle executed inside a real Docker container.
// This is an explicit opt-in integration test. It is skipped if MULTIGENT_RUN_DOCKER_INTEGRATION!=1
// or if MULTIGENT_PREVIEW_DOCKER_TEST_IMAGE is not set / not available locally.
func TestPreviewTurn_DockerSandboxExecution_RealContainer(t *testing.T) {
	if os.Getenv("MULTIGENT_RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("skipping real container integration test: MULTIGENT_RUN_DOCKER_INTEGRATION=1 not set")
	}

	testImage := strings.TrimSpace(os.Getenv("MULTIGENT_PREVIEW_DOCKER_TEST_IMAGE"))
	if testImage == "" {
		t.Skip("skipping real container integration test: MULTIGENT_PREVIEW_DOCKER_TEST_IMAGE not set")
	}

	if strings.HasSuffix(testImage, ":latest") {
		t.Fatal("MULTIGENT_PREVIEW_DOCKER_TEST_IMAGE must not use :latest tag; must specify pinned digest or concrete tag")
	}

	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("docker binary not found in PATH: %v", err)
	}

	if err := exec.Command(dockerPath, "info").Run(); err != nil {
		t.Skipf("docker daemon not responding: %v", err)
	}

	// Verify image exists locally without pulling from network
	if err := exec.Command(dockerPath, "image", "inspect", testImage).Run(); err != nil {
		t.Skipf("test image %q not present in local docker cache: %v", testImage, err)
	}

	t.Setenv(secretbox.EnvKey, "test-master-key-for-preview-32b!!")

	// 1. Setup local git repo
	gitRoot := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = gitRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Docker Tester")
	runGit("config", "user.email", "tester@docker.test")
	runGit("config", "commit.gpgSign", "false")

	appFile := filepath.Join(gitRoot, "app.js")
	if err := os.WriteFile(appFile, []byte("console.log('original');\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "app.js")
	runGit("commit", "-m", "initial commit")
	baseSHA := runGit("rev-parse", "HEAD")

	// 2. Setup Server, Store & TurnEngine
	s, workspaceID := newConnectionGrantPolicyServer(t)
	store := s.receiptStoreForWorkspace(workspaceID)
	if store == nil {
		t.Fatal("receipt store unavailable for workspace")
	}
	engine := previewreceipt.NewTurnEngine(store)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	project := "proj-docker-integration"
	taskID := "task-docker-001"

	// 3. Define deterministic, zero-network Docker runner and verify mount arguments
	dockerRunner := previewreceipt.AgentRunnerFunc(func(ctx context.Context, cloneDir string, prompt string) error {
		// Strict invariant checks:
		// cloneDir must be isolated and must not be gitRoot
		if cloneDir == gitRoot || strings.HasPrefix(cloneDir, gitRoot+string(filepath.Separator)) {
			return fmt.Errorf("isolation breach: cloneDir %s is inside gitRoot %s", cloneDir, gitRoot)
		}

		mountSpec := fmt.Sprintf("%s:/workspace:rw", cloneDir)
		dockerArgs := []string{
			"run", "--rm",
			"--network", "none",
			"-v", mountSpec,
			"-w", "/workspace",
			testImage,
			"sh", "-c", "echo \"// modified by docker agent\" >> /workspace/app.js",
		}

		// Security assertion on argv:
		for _, arg := range dockerArgs {
			if strings.Contains(arg, "/var/run/docker.sock") {
				return fmt.Errorf("security invariant violated: docker.sock mounted: %s", arg)
			}
			if strings.HasPrefix(arg, gitRoot+":") {
				return fmt.Errorf("security invariant violated: host worktree mounted directly: %s", arg)
			}
			if arg == "/:/workspace" || strings.HasPrefix(arg, "/:/") {
				return fmt.Errorf("security invariant violated: host root mounted: %s", arg)
			}
		}

		cmd := exec.CommandContext(ctx, dockerPath, dockerArgs...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("container execution failed: %w (output: %s)", err, string(out))
		}
		return nil
	})

	// 4. Execute turn inside real Docker container
	turn, err := engine.ExecuteTurn(ctx, previewreceipt.ExecuteTurnParams{
		WorkspaceID:    workspaceID,
		Project:        project,
		ProjectGitRoot: gitRoot,
		TaskID:         taskID,
		WorktreeDir:    gitRoot,
		Prompt:         "deterministic docker prompt",
		Actor:          "docker-tester",
		Runner:         dockerRunner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn in container failed: %v", err)
	}

	if turn.Status != previewreceipt.StatusCaptured {
		t.Fatalf("expected turn to be CAPTURED, got %s", turn.Status)
	}
	if !strings.Contains(turn.DisplayDiff, "+// modified by docker agent") {
		t.Fatalf("displayDiff missing expected container modification: %s", turn.DisplayDiff)
	}
	snapDir := previewreceipt.TurnSnapshotDir(gitRoot, taskID, turn.TurnID)
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("expected snapshot directory to exist: %v", err)
	}

	// 5. Review & Commit flow
	intentID := fmt.Sprintf("review-docker-%d", time.Now().UnixNano())
	committing, err := engine.PrepareCommitReceipts(ctx, previewreceipt.PrepareCommitReceiptsParams{
		Project:        project,
		ProjectGitRoot: gitRoot,
		TaskID:         taskID,
		CommitIntentID: intentID,
		PreCommitSHA:   baseSHA,
	})
	if err != nil {
		t.Fatalf("PrepareCommitReceipts failed: %v", err)
	}
	if len(committing) != 1 || committing[0].Status != previewreceipt.StatusCommitting {
		t.Fatalf("expected 1 COMMITTING receipt, got: %v", committing)
	}

	// Slot must be held by commit_intent group
	slot, activeRec, err := store.GetActiveSlot(ctx, project, taskID)
	if err != nil || slot == nil || !slot.IsCommitIntentGroup() || activeRec == nil {
		t.Fatalf("slot not held by commit_intent group: slot=%+v, active=%+v, err=%v", slot, activeRec, err)
	}

	// Apply container modifications to gitRoot and commit
	if err := os.WriteFile(appFile, []byte("console.log('original');\n// modified by docker agent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "app.js")
	commitMsg := fmt.Sprintf("chore(review): container user feedback\n\n%s: %s", previewreceipt.CommitIntentTrailerKey, intentID)
	runGit("commit", "-m", commitMsg)
	checkpointSHA := runGit("rev-parse", "HEAD")

	// Finalize commit receipts
	if err := engine.FinalizeCommitReceipts(ctx, project, taskID, committing, checkpointSHA, gitRoot); err != nil {
		t.Fatalf("FinalizeCommitReceipts failed: %v", err)
	}

	// 6. Verify terminal state
	finalRec, err := store.Get(ctx, project, taskID, turn.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if finalRec.Status != previewreceipt.StatusCommitted {
		t.Fatalf("expected receipt to be COMMITTED, got %s", finalRec.Status)
	}
	if finalRec.CommittedSHA != checkpointSHA {
		t.Fatalf("expected CommittedSHA %s, got %s", checkpointSHA, finalRec.CommittedSHA)
	}
	if finalRec.SnapshotCleanupPending {
		t.Fatal("expected SnapshotCleanupPending to be false after clean finalize")
	}

	// Snapshot must be deleted
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory should be deleted, got err: %v", err)
	}

	// Slot must be released
	slotFinal, activeFinal, _ := store.GetActiveSlot(ctx, project, taskID)
	if activeFinal != nil {
		t.Fatalf("expected active slot holder to be nil, got %+v", activeFinal)
	}
	if slotFinal != nil && slotFinal.ReceiptID != "" && slotFinal.IsCommitIntentGroup() {
		t.Fatalf("expected slot to be released, got %+v", slotFinal)
	}
}
