package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestChatopsActionSession_LifecycleAndAtomicClaim(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	ws := Workspace{
		ID:   "ws-test-chatops",
		Name: "Test WS",
		Slug: "test-ws",
		Root: dir,
	}
	if err := store.UpsertWorkspace(ws); err != nil {
		t.Fatalf("UpsertWorkspace failed: %v", err)
	}

	session := &ChatopsActionSession{
		ID:                   "cas-1",
		WorkspaceID:          ws.ID,
		Project:              "proj-1",
		TaskID:               "task-1",
		StepID:               "step-review",
		ExpectedStateVersion: 42,
		ReviewSnapshotHash:   "sha256:abcd",
		ActionType:           "approve",
		ActorMMUserID:        "mm-user-josh",
		ActorPlatformUserID:  "usr-josh",
		State:                "dialog_opened",
		ActionNonce:          "nonce-xyz",
		TokenHash:            "tokenhash-123",
		ExpiresAt:            time.Now().UTC().Add(5 * time.Minute),
	}

	// 1. Create session
	if err := store.CreateChatopsActionSession(session); err != nil {
		t.Fatalf("CreateChatopsActionSession failed: %v", err)
	}

	// 2. Query by ID
	fetched, found, err := store.ChatopsActionSessionByID(ws.ID, "cas-1")
	if err != nil || !found {
		t.Fatalf("ChatopsActionSessionByID failed: found=%v, err=%v", found, err)
	}
	if fetched.ActionNonce != "nonce-xyz" || fetched.State != "dialog_opened" {
		t.Fatalf("unexpected fetched session: %+v", fetched)
	}

	// 3. Query by Nonce
	fetchedNonce, found, err := store.ChatopsActionSessionByNonce(ws.ID, "nonce-xyz")
	if err != nil || !found {
		t.Fatalf("ChatopsActionSessionByNonce failed: found=%v, err=%v", found, err)
	}
	if fetchedNonce.ID != "cas-1" {
		t.Fatalf("unexpected fetched session: %+v", fetchedNonce)
	}

	// 4. Duplicate Nonce or TokenHash must fail (UNIQUE constraint)
	dupSession := &ChatopsActionSession{
		ID:                   "cas-2",
		WorkspaceID:          ws.ID,
		Project:              "proj-1",
		TaskID:               "task-1",
		StepID:               "step-review",
		ExpectedStateVersion: 42,
		ReviewSnapshotHash:   "sha256:abcd",
		ActionType:           "approve",
		ActorMMUserID:        "mm-user-josh",
		State:                "dialog_opened",
		ActionNonce:          "nonce-xyz", // Duplicate nonce
		TokenHash:            "tokenhash-different",
	}
	if err := store.CreateChatopsActionSession(dupSession); err == nil {
		t.Fatal("expected duplicate nonce to fail")
	}

	// 5. Atomic claim for processing
	claimed, err := store.ClaimChatopsActionSessionForProcessing(ws.ID, "cas-1")
	if err != nil || !claimed {
		t.Fatalf("expected claim to succeed: claimed=%v, err=%v", claimed, err)
	}

	// Second claim must fail (already in processing state, not dialog_opened)
	claimedAgain, err := store.ClaimChatopsActionSessionForProcessing(ws.ID, "cas-1")
	if err != nil || claimedAgain {
		t.Fatalf("second claim should return false: claimed=%v, err=%v", claimedAgain, err)
	}

	// 6. Transition to completed with resolution trace and resolved outputs
	traceJSON := `{"commit_sha":{"mode":"system_locked","source":"task.commit"}}`
	outputsJSON := `{"decision":"approved","commit_sha":"c0ffee"}`
	if err := store.CompleteChatopsActionSession(ws.ID, "cas-1", traceJSON, outputsJSON); err != nil {
		t.Fatalf("CompleteChatopsActionSession failed: %v", err)
	}
	finalSession, _, _ := store.ChatopsActionSessionByID(ws.ID, "cas-1")
	if finalSession.State != "completed" || finalSession.ResolutionTraceJSON != traceJSON || finalSession.ResolvedOutputsJSON != outputsJSON {
		t.Fatalf("expected state completed with trace and outputs, got %+v", finalSession)
	}

	// 6b. Test 1-Click Approve Lifecycle: state starts at 'issued', claimed directly to 'processing'
	sessionIssued := &ChatopsActionSession{
		ID:                   "cas-direct-approve",
		WorkspaceID:          ws.ID,
		Project:              "proj-1",
		TaskID:               "task-1",
		StepID:               "step-review",
		ExpectedStateVersion: 42,
		ReviewSnapshotHash:   "sha256:abcd",
		ActionType:           "approve",
		ActorMMUserID:        "mm-user-josh",
		ActorPlatformUserID:  "usr-josh",
		State:                "issued",
		ActionNonce:          "nonce-direct-approve",
		TokenHash:            "tokenhash-direct-approve",
		ExpiresAt:            time.Now().UTC().Add(5 * time.Minute),
	}
	if err := store.CreateChatopsActionSession(sessionIssued); err != nil {
		t.Fatalf("CreateChatopsActionSession for issued session failed: %v", err)
	}
	claimedIssued, err := store.ClaimChatopsActionSessionForProcessing(ws.ID, sessionIssued.ID)
	if err != nil || !claimedIssued {
		t.Fatalf("expected claim from 'issued' to succeed: claimed=%v, err=%v", claimedIssued, err)
	}

	// 7. Expire stale sessions
	expiredSession := &ChatopsActionSession{
		ID:                   "cas-exp",
		WorkspaceID:          ws.ID,
		Project:              "proj-1",
		TaskID:               "task-2",
		StepID:               "step-review",
		ExpectedStateVersion: 10,
		ReviewSnapshotHash:   "sha256:1111",
		ActionType:           "reject",
		ActorMMUserID:        "mm-user-josh",
		State:                "issued",
		ActionNonce:          "nonce-exp",
		TokenHash:            "tokenhash-exp",
		ExpiresAt:            time.Now().UTC().Add(-1 * time.Minute), // Already expired
	}
	if err := store.CreateChatopsActionSession(expiredSession); err != nil {
		t.Fatalf("CreateChatopsActionSession expired failed: %v", err)
	}

	if err := store.ExpireStaleChatopsActionSessions(ws.ID, time.Now().UTC()); err != nil {
		t.Fatalf("ExpireStaleChatopsActionSessions failed: %v", err)
	}
	expFetched, _, _ := store.ChatopsActionSessionByID(ws.ID, "cas-exp")
	if expFetched.State != "expired" {
		t.Fatalf("expected state expired, got %q", expFetched.State)
	}
}
