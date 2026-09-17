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

	"github.com/multigent/multigent/internal/agentdir"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/previewreceipt"
	"github.com/multigent/multigent/internal/secretbox"
	"github.com/multigent/multigent/internal/store"
	"github.com/multigent/multigent/internal/taskstore"
)

// TestPreviewTurn_DockerSandboxExecution_RealContainer validates the end-to-end
// preview turn lifecycle executed through the REAL production runner chain:
// previewDefaultAgentRunner -> multigent exec -> runner.Runner -> runenv.DockerProvider -> sandbox.BuildArgs -> real Docker container.
//
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

	// 1. Compile real multigent binary for the test
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	multigentBin := filepath.Join(binDir, "multigent")
	buildCmd := exec.Command("go", "build", "-o", multigentBin, "./cmd/multigent")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile multigent binary: %v (%s)", err, string(out))
	}

	// 2. Setup Server with shared SQLite control DB
	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	t.Setenv("MULTIGENT_CONTROL_DATA_DIR", workspaceRoot)
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".multigent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".multigent", "agency.yaml"), []byte("name: docker-test-workspace\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(workspaceRoot, ".multigent", "multigent.db")
	db, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := store.NewDB(workspaceRoot, db)
	ts := taskstore.NewDB(workspaceRoot, db)
	s := &Server{
		root:            workspaceRoot,
		controlDB:       db,
		st:              st,
		ts:              ts,
		users:           newUserStore(db),
		agentDirectory:  agentdir.New(db),
		previewSessions: make(map[string]*previewChatSession),
		sched: &SchedulerManager{
			binPath: multigentBin,
			root:    workspaceRoot,
		},
	}
	s.triggers = newTriggerManager(workspaceRoot, multigentBin, ts, s.controlDB)

	workspaceID, err := s.currentWorkspaceID()
	if err != nil {
		t.Fatalf("workspace id: %v", err)
	}
	if err := s.controlDB.UpsertWorkspace(controldb.Workspace{
		ID:   workspaceID,
		Name: "Docker Test Workspace",
		Slug: "docker-test-workspace",
		Root: workspaceRoot,
	}); err != nil {
		t.Fatalf("workspace: %v", err)
	}

	project := "proj-docker-integration"
	taskID := "task-docker-001"
	if err := st.SaveProject(project, &entity.Project{Name: project}); err != nil {
		t.Fatalf("save project: %v", err)
	}

	// 3. Setup local git repo
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

	// 4. Configure agent with isolated Docker sandbox
	agentName := "docker-dev"
	agentMeta := &entity.AgentMeta{
		Name:       agentName,
		Project:    project,
		Model:      entity.ModelGenericCLI,
		RunCommand: "sh -c 'echo \"// modified by docker agent\" >> /workspace/app.js'",
		Sandbox: &entity.SandboxConfig{
			Provider: entity.SandboxDocker,
			Docker: &entity.DockerSandboxConfig{
				Image:       testImage,
				NetworkMode: "none",
			},
		},
	}

	now := time.Now().UTC().Format(time.RFC3339)
	runtimeCfgJSON := encodeAgentWorkerRuntimeConfig(agentWorkerRuntimeConfig{
		Sandbox:    agentMeta.Sandbox,
		RunCommand: agentMeta.RunCommand,
	})
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
		ID:                "aw-" + agentName,
		WorkspaceID:       workspaceID,
		Name:              agentName,
		DisplayName:       agentName,
		Model:             "custom",
		Status:            "available",
		RuntimeConfigJSON: runtimeCfgJSON,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("upsert agent worker: %v", err)
	}
	if err := s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
		ID:          "pm-" + agentName,
		WorkspaceID: workspaceID,
		ProjectID:   project,
		MemberType:  "agent_worker",
		MemberID:    "aw-" + agentName,
		Title:       agentName,
		Role:        "developer",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("upsert project membership: %v", err)
	}

	// 5. Test fail-closed validation of sandbox configuration
	t.Run("FailClosed_ExtraVolumesRejected", func(t *testing.T) {
		badAgent := "bad-extra-vol"
		badMeta := &entity.AgentMeta{
			Name:       badAgent,
			Project:    project,
			Model:      entity.ModelGenericCLI,
			RunCommand: "echo test",
			Sandbox: &entity.SandboxConfig{
				Provider: entity.SandboxDocker,
				Docker: &entity.DockerSandboxConfig{
					Image:        testImage,
					ExtraVolumes: []string{"/tmp:/tmp"},
				},
			},
		}
		_ = s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
			ID:                "aw-" + badAgent,
			WorkspaceID:       workspaceID,
			Name:              badAgent,
			Model:             "custom",
			RuntimeConfigJSON: encodeAgentWorkerRuntimeConfig(agentWorkerRuntimeConfig{Sandbox: badMeta.Sandbox, RunCommand: badMeta.RunCommand}),
			CreatedAt:         now,
			UpdatedAt:         now,
		})
		_ = s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
			ID:          "pm-" + badAgent,
			WorkspaceID: workspaceID,
			ProjectID:   project,
			MemberType:  "agent_worker",
			MemberID:    "aw-" + badAgent,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		badRunner := s.newPreviewAgentRunner(workspaceID, project, badAgent, "http://127.0.0.1:27892")
		err := badRunner.RunAgent(context.Background(), t.TempDir(), "test prompt")
		if err == nil || !strings.Contains(err.Error(), "rejects ExtraVolumes") {
			t.Fatalf("expected error rejecting ExtraVolumes, got: %v", err)
		}
	})

	t.Run("FailClosed_DockerSocketRejected", func(t *testing.T) {
		badAgent := "bad-docker-sock"
		badMeta := &entity.AgentMeta{
			Name:       badAgent,
			Project:    project,
			Model:      entity.ModelGenericCLI,
			RunCommand: "echo test",
			Sandbox: &entity.SandboxConfig{
				Provider: entity.SandboxDocker,
				Docker: &entity.DockerSandboxConfig{
					Image:        testImage,
					ExtraVolumes: []string{"/var/run/docker.sock:/var/run/docker.sock"},
				},
			},
		}
		_ = s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
			ID:                "aw-" + badAgent,
			WorkspaceID:       workspaceID,
			Name:              badAgent,
			Model:             "custom",
			RuntimeConfigJSON: encodeAgentWorkerRuntimeConfig(agentWorkerRuntimeConfig{Sandbox: badMeta.Sandbox, RunCommand: badMeta.RunCommand}),
			CreatedAt:         now,
			UpdatedAt:         now,
		})
		_ = s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
			ID:          "pm-" + badAgent,
			WorkspaceID: workspaceID,
			ProjectID:   project,
			MemberType:  "agent_worker",
			MemberID:    "aw-" + badAgent,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		badRunner := s.newPreviewAgentRunner(workspaceID, project, badAgent, "http://127.0.0.1:27892")
		err := badRunner.RunAgent(context.Background(), t.TempDir(), "test prompt")
		if err == nil || !strings.Contains(err.Error(), "strictly forbids Docker socket") {
			t.Fatalf("expected error rejecting Docker socket, got: %v", err)
		}
	})

	t.Run("FailClosed_CredentialMountsRejected", func(t *testing.T) {
		badAgent := "bad-cred-mounts"
		badMeta := &entity.AgentMeta{
			Name:       badAgent,
			Project:    project,
			Model:      entity.ModelGenericCLI,
			RunCommand: "echo test",
			Sandbox: &entity.SandboxConfig{
				Provider: entity.SandboxDocker,
				Docker: &entity.DockerSandboxConfig{
					Image:            testImage,
					CredentialMounts: []string{"/host:/root/.cred"},
				},
			},
		}
		_ = s.controlDB.UpsertAgentWorker(controldb.AgentWorker{
			ID:                "aw-" + badAgent,
			WorkspaceID:       workspaceID,
			Name:              badAgent,
			Model:             "custom",
			RuntimeConfigJSON: encodeAgentWorkerRuntimeConfig(agentWorkerRuntimeConfig{Sandbox: badMeta.Sandbox, RunCommand: badMeta.RunCommand}),
			CreatedAt:         now,
			UpdatedAt:         now,
		})
		_ = s.controlDB.UpsertProjectMembership(controldb.ProjectMembership{
			ID:          "pm-" + badAgent,
			WorkspaceID: workspaceID,
			ProjectID:   project,
			MemberType:  "agent_worker",
			MemberID:    "aw-" + badAgent,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		badRunner := s.newPreviewAgentRunner(workspaceID, project, badAgent, "http://127.0.0.1:27892")
		err := badRunner.RunAgent(context.Background(), t.TempDir(), "test prompt")
		if err == nil || !strings.Contains(err.Error(), "rejects CredentialMounts") {
			t.Fatalf("expected error rejecting CredentialMounts, got: %v", err)
		}
	})

	// 6. Execute turn through the REAL production runner chain
	store := s.receiptStoreForWorkspace(workspaceID)
	if store == nil {
		t.Fatal("receipt store unavailable for workspace")
	}
	engine := previewreceipt.NewTurnEngine(store)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Real production preview runner
	prodRunner := s.newPreviewAgentRunner(workspaceID, project, agentName, "http://127.0.0.1:27892")

	turn, err := engine.ExecuteTurn(ctx, previewreceipt.ExecuteTurnParams{
		WorkspaceID:    workspaceID,
		Project:        project,
		ProjectGitRoot: gitRoot,
		TaskID:         taskID,
		WorktreeDir:    gitRoot,
		Prompt:         "deterministic docker prompt",
		Actor:          "docker-tester",
		Runner:         prodRunner,
	})
	if err != nil {
		t.Fatalf("ExecuteTurn through production runner failed: %v", err)
	}

	if turn.Status != previewreceipt.StatusCaptured {
		t.Fatalf("expected turn to be CAPTURED, got %s", turn.Status)
	}
	if !strings.Contains(turn.DisplayDiff, "+// modified by docker agent") {
		t.Fatalf("displayDiff missing expected container modification: %s", turn.DisplayDiff)
	}

	// 7. Verify snapshot directory exists under .multigent/turns/<taskID>/<turnID>/snapshot
	snapDir := previewreceipt.TurnSnapshotDir(gitRoot, taskID, turn.TurnID)
	if _, err := os.Stat(snapDir); err != nil {
		t.Fatalf("expected snapshot directory to exist at %s: %v", snapDir, err)
	}
	snapshotAppFile := filepath.Join(snapDir, "app.js")
	if snapContent, err := os.ReadFile(snapshotAppFile); err != nil || !strings.Contains(string(snapContent), "// modified by docker agent") {
		t.Fatalf("snapshot app.js missing container modification: %v (content: %s)", err, string(snapContent))
	}

	// 8. Review & Commit flow
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

	// 9. Verify terminal state
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
