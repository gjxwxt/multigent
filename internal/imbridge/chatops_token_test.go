package imbridge

import (
	"testing"
	"time"
)

func TestChatopsTokens_SignAndVerify(t *testing.T) {
	secret := "test-secret-key-12345"

	// 1. Action Token
	actionPayload := ActionTokenPayload{
		WorkspaceID:          "ws-1",
		ProjectID:            "proj-1",
		TaskID:               "task-100",
		StepID:               "step-review",
		Action:               "approve",
		ChannelID:            "chan-123",
		ConnectionID:         "conn-abc",
		ExpectedStateVersion: 42,
		ReviewSnapshotHash:   "sha256:abc",
		Nonce:                "nonce-1",
		ExpiresAt:            time.Now().UTC().Add(1 * time.Hour).Unix(),
	}

	tok, err := SignActionToken(secret, actionPayload)
	if err != nil {
		t.Fatalf("SignActionToken failed: %v", err)
	}

	verified, err := VerifyActionToken(secret, tok)
	if err != nil {
		t.Fatalf("VerifyActionToken failed: %v", err)
	}
	if verified.TaskID != "task-100" || verified.ExpectedStateVersion != 42 || verified.ChannelID != "chan-123" || verified.ConnectionID != "conn-abc" {
		t.Fatalf("unexpected verified action payload: %+v", verified)
	}

	// 2. Tampered token fails
	tamperedTok := tok + "x"
	if _, err := VerifyActionToken(secret, tamperedTok); err == nil {
		t.Fatal("expected error on tampered token")
	}

	// 3. Expired token fails
	expiredPayload := actionPayload
	expiredPayload.ExpiresAt = time.Now().UTC().Add(-10 * time.Second).Unix()
	expTok, _ := SignActionToken(secret, expiredPayload)
	if _, err := VerifyActionToken(secret, expTok); err == nil {
		t.Fatal("expected error on expired token")
	}

	// 4. Dialog Token
	dialogPayload := DialogTokenPayload{
		WorkspaceID:          "ws-1",
		ProjectID:            "proj-1",
		TaskID:               "task-100",
		StepID:               "step-review",
		Action:               "reject",
		ExpectedStateVersion: 42,
		ReviewSnapshotHash:   "sha256:abc",
		ActorMMUserID:        "mm-user-josh",
		SessionID:            "cas-1",
		Nonce:                "nonce-2",
		ExpiresAt:            time.Now().UTC().Add(5 * time.Minute).Unix(),
	}

	dtok, err := SignDialogToken(secret, dialogPayload)
	if err != nil {
		t.Fatalf("SignDialogToken failed: %v", err)
	}

	verifiedDialog, err := VerifyDialogToken(secret, dtok)
	if err != nil {
		t.Fatalf("VerifyDialogToken failed: %v", err)
	}
	if verifiedDialog.ActorMMUserID != "mm-user-josh" || verifiedDialog.SessionID != "cas-1" {
		t.Fatalf("unexpected verified dialog payload: %+v", verifiedDialog)
	}

	// 5. ComputeTokenHash
	h1 := ComputeTokenHash(tok)
	h2 := ComputeTokenHash(tok)
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash mismatch or invalid length: %q vs %q", h1, h2)
	}
}
