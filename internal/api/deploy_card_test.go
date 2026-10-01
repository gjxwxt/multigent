package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
)

// newDeployCardTestRequest builds a pending_approval deploy request fixture.
func newDeployCardTestRequest(workspaceID string) controldb.DeployRequest {
	return controldb.DeployRequest{
		ID:          "dep-" + strings.Repeat("a", 10),
		WorkspaceID: workspaceID,
		ProjectID:   "sample",
		Branch:      "feature/deploy-center",
		SHA:         "0123456789abcdef0123456789abcdef01234567",
		Env:         "production",
		CommitSpan: []controldb.CommitSpanEntry{
			{SHA: "0123456789abcdef0123456789abcdef01234567", ShortSHA: "0123456", Title: "feat: deploy center batch 4", Author: "alice", CommittedAt: "2026-09-30T00:00:00Z"},
			{SHA: "fedcba9876543210fedcba9876543210fedcba98", ShortSHA: "fedcba9", Title: "fix: ledger CAS", Author: "bob", CommittedAt: "2026-09-30T01:00:00Z"},
		},
		Status:    "pending_approval",
		CreatedBy: "admin",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func TestFormatDeployApprovalAttachment_Structure(t *testing.T) {
	req := newDeployCardTestRequest("ws-card")
	tok := "signed.token.payload"

	att := FormatDeployApprovalAttachment(&req, deployCardActionApprove, tok, "http://console.example")

	if got, _ := att["title"].(string); !strings.Contains(got, "部署审批") || !strings.Contains(got, "Deploy Approval") {
		t.Fatalf("card title = %q, want bilingual deploy approval heading", got)
	}

	fields, _ := att["fields"].([]map[string]any)
	if len(fields) != 6 {
		t.Fatalf("fields = %d, want 6 (project/branch/sha/env/commits/requester)", len(fields))
	}
	joined := ""
	for _, f := range fields {
		joined += f["title"].(string) + "=" + f["value"].(string) + "\n"
	}
	for _, want := range []string{"sample", "feature/deploy-center", "0123456789ab", "2 条", "feat: deploy center batch 4", "admin", "production"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("card fields missing %q; got:\n%s", want, joined)
		}
	}

	actions, _ := att["actions"].([]map[string]any)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2 (approve + reject)", len(actions))
	}
	for i, wantAct := range []string{deployCardActionApprove, deployCardActionReject} {
		btn := actions[i]
		integration, _ := btn["integration"].(map[string]any)
		if integration == nil {
			t.Fatalf("button %d missing integration", i)
		}
		if got, _ := integration["url"].(string); !strings.HasSuffix(got, "/api/v1/im/mattermost/actions") {
			t.Fatalf("button %d integration.url = %q, want chatops actions endpoint", i, got)
		}
		// Regression for the "Action integration error" live finding: relative
		// integration URLs resolve against the MM SiteURL, so the click POSTs
		// to Mattermost itself (404). The URL must carry the absolute console
		// origin passed in.
		if got, _ := integration["url"].(string); !strings.HasPrefix(got, "http://console.example/") {
			t.Fatalf("button %d integration.url = %q, want absolute callback base origin", i, got)
		}
		ctxMap, _ := integration["context"].(map[string]any)
		if ctxMap == nil {
			t.Fatalf("button %d missing integration.context", i)
		}
		if got, _ := ctxMap["action"].(string); got != wantAct {
			t.Fatalf("button %d context.action = %q, want %q", i, got, wantAct)
		}
		if got, _ := ctxMap["action_token"].(string); got != tok {
			t.Fatalf("button %d context.action_token = %q, want signed token %q", i, got, tok)
		}
	}
}

// The deploy card buttons carry one signed token per button, and each token
// round-trips through VerifyActionToken with TaskID holding the deploy
// request id — the exact payload the chatops callback dispatches on.
func TestDeployCardToken_SignVerifyRoundTrip(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	req := newDeployCardTestRequest(workspaceID)
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest: %v", err)
	}

	expiresAt := time.Now().UTC().Add(2 * time.Hour).Unix()
	approveTok, err := s.signDeployCardToken(&req, "conn-mm-chatops-test", "chan-chatops-1", deployCardActionApprove, expiresAt)
	if err != nil {
		t.Fatalf("signDeployCardToken approve: %v", err)
	}
	rejectTok, err := s.signDeployCardToken(&req, "conn-mm-chatops-test", "chan-chatops-1", deployCardActionReject, expiresAt)
	if err != nil {
		t.Fatalf("signDeployCardToken reject: %v", err)
	}
	if approveTok == rejectTok {
		t.Fatalf("approve and reject tokens must differ (distinct action + nonce)")
	}

	payload, err := imbridge.VerifyActionToken(hmacSecret, approveTok)
	if err != nil {
		t.Fatalf("VerifyActionToken approve: %v", err)
	}
	if payload.TaskID != req.ID {
		t.Fatalf("token TaskID = %q, want deploy request id %q", payload.TaskID, req.ID)
	}
	if payload.Action != "deploy_approve" {
		t.Fatalf("token Action = %q, want deploy_approve", payload.Action)
	}
	if payload.StepID != "deploy" {
		t.Fatalf("token StepID = %q, want deploy", payload.StepID)
	}
	if payload.WorkspaceID != workspaceID || payload.ProjectID != "sample" {
		t.Fatalf("token ws/prj = %q/%q, want %s/sample", payload.WorkspaceID, payload.ProjectID, workspaceID)
	}
	if payload.ConnectionID != "conn-mm-chatops-test" {
		t.Fatalf("token ConnectionID = %q, want conn-mm-chatops-test", payload.ConnectionID)
	}

	// Wrong secret must fail closed.
	if _, err := imbridge.VerifyActionToken("other-secret", approveTok); err == nil {
		t.Fatalf("expected verification failure with wrong secret")
	}
}

// Full chatops loop: pending deploy request + approve click → CAS to
// approved, audit event written, hook fired. A second click with the same
// nonce (anti-replay) is rejected.
func TestMattermostActionCallback_DeployApprove_CASAuditHookAndReplay(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	req := newDeployCardTestRequest(workspaceID)
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest: %v", err)
	}

	var hookCalls int
	var hookReqID string
	s.deployTriggerHook = func(ctx context.Context, dr *controldb.DeployRequest, actor string) error {
		hookCalls++
		hookReqID = dr.ID
		return nil
	}

	nonce := imbridge.GenerateNonce()
	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		TaskID:       req.ID, // deploy request id rides in TaskID
		StepID:       "deploy",
		Action:       "deploy_approve",
		ChannelID:    "chan-chatops-1",
		ConnectionID: "conn-mm-chatops-test",
		Nonce:        nonce,
		ExpiresAt:    time.Now().UTC().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}

	body, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		UserName:  "admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-deploy-card-1",
		Context:   mattermostActionContext{ActionToken: tok, Action: "deploy_approve"},
	})
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if text, _ := resp["ephemeral_text"].(string); !strings.Contains(text, "已批准") {
		t.Fatalf("expected approve confirmation ephemeral, got %v", resp)
	}

	// CAS: status moved pending_approval → approved.
	got, found, err := s.controlDB.DeployRequestFor(workspaceID, req.ID)
	if err != nil || !found {
		t.Fatalf("DeployRequestFor: found=%v err=%v", found, err)
	}
	if got.Status != "approved" {
		t.Fatalf("status after chatops approve = %q, want approved", got.Status)
	}

	// Hook fired with the right request.
	if hookCalls != 1 || hookReqID != req.ID {
		t.Fatalf("trigger hook calls=%d reqID=%q, want 1/%s", hookCalls, hookReqID, req.ID)
	}

	// Audit event recorded.
	events, err := s.controlDB.ListAuditEvents(controldb.AuditEventFilter{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.approve",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
	})
	if err != nil || len(events) == 0 {
		t.Fatalf("expected deploy.request.approve audit event, got %d events err=%v", len(events), err)
	}

	// Replay with the identical token+nonce must be blocked.
	rec2 := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body)))
	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	text2, _ := resp2["text"].(string)
	if !strings.Contains(text2, "请勿重复操作") {
		t.Fatalf("expected replay rejection, got %v", resp2)
	}
	if hookCalls != 1 {
		t.Fatalf("hook must not fire twice, got %d calls", hookCalls)
	}
}

// Reject path: CAS to rejected + audit; and a click on a request that is no
// longer pending (already decided on the console) must answer the 409
// "already processed" message.
func TestMattermostActionCallback_DeployReject_AndAlreadyProcessed(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	req := newDeployCardTestRequest(workspaceID)
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest: %v", err)
	}

	signDeployTok := func(action string) string {
		tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
			WorkspaceID:  workspaceID,
			ProjectID:    "sample",
			TaskID:       req.ID,
			StepID:       "deploy",
			Action:       action,
			ChannelID:    "chan-chatops-1",
			ConnectionID: "conn-mm-chatops-test",
			Nonce:        imbridge.GenerateNonce(),
			ExpiresAt:    time.Now().UTC().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatalf("SignActionToken: %v", err)
		}
		return tok
	}

	// 1. Reject advances to rejected.
	body, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-deploy-card-1",
		Context:   mattermostActionContext{ActionToken: signDeployTok("deploy_reject"), Action: "deploy_reject"},
	})
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reject expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got, found, err := s.controlDB.DeployRequestFor(workspaceID, req.ID)
	if err != nil || !found {
		t.Fatalf("DeployRequestFor: found=%v err=%v", found, err)
	}
	if got.Status != "rejected" {
		t.Fatalf("status after chatops reject = %q, want rejected", got.Status)
	}
	events, err := s.controlDB.ListAuditEvents(controldb.AuditEventFilter{
		WorkspaceID:  workspaceID,
		Action:       "deploy.request.reject",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
	})
	if err != nil || len(events) == 0 {
		t.Fatalf("expected deploy.request.reject audit event, got %d err=%v", len(events), err)
	}

	// 2. A fresh approve card click on the now-terminal request must report
	// the 409 "already processed" semantics, not corrupt the state.
	body2, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-admin",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-deploy-card-1",
		Context:   mattermostActionContext{ActionToken: signDeployTok("deploy_approve"), Action: "deploy_approve"},
	})
	rec2 := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body2)))
	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	text2, _ := resp2["text"].(string)
	if !strings.Contains(text2, "已被处理") || !strings.Contains(text2, "409") {
		t.Fatalf("expected already-processed 409 message, got %q", text2)
	}
	final, _, _ := s.controlDB.DeployRequestFor(workspaceID, req.ID)
	if final.Status != "rejected" {
		t.Fatalf("status must stay rejected, got %q", final.Status)
	}
}

// RBAC: a bound user without operator role on the project must be refused
// before any CAS transition happens.
func TestMattermostActionCallback_DeployApprove_ViewerForbidden(t *testing.T) {
	s, workspaceID, _, _, _, hmacSecret := setupTestChatopsEnv(t)
	req := newDeployCardTestRequest(workspaceID)
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest: %v", err)
	}

	if err := s.users.CreateUser("deploy-viewer", "pass123", RoleMember, "", "", "", "", ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_ = s.users.UpdateUser("deploy-viewer", nil, nil, nil, nil, nil, nil, nil, []projectAccess{{Project: "sample", Role: ProjectRoleViewer}}, nil, nil)
	_ = s.controlDB.UpsertUserChannelIdentity(controldb.UserChannelIdentity{
		ID:               "ucid-deploy-viewer",
		WorkspaceID:      workspaceID,
		UserID:           "deploy-viewer",
		ChannelBindingID: "binding-chatops-1",
		Provider:         "mattermost",
		ExternalUserID:   "mm-user-viewer",
	})

	tok, err := imbridge.SignActionToken(hmacSecret, imbridge.ActionTokenPayload{
		WorkspaceID:  workspaceID,
		ProjectID:    "sample",
		TaskID:       req.ID,
		StepID:       "deploy",
		Action:       "deploy_approve",
		ChannelID:    "chan-chatops-1",
		ConnectionID: "conn-mm-chatops-test",
		Nonce:        imbridge.GenerateNonce(),
		ExpiresAt:    time.Now().UTC().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignActionToken: %v", err)
	}
	body, _ := json.Marshal(mattermostActionPayload{
		UserID:    "mm-user-viewer",
		ChannelID: "chan-chatops-1",
		PostID:    "mock-deploy-card-1",
		Context:   mattermostActionContext{ActionToken: tok, Action: "deploy_approve"},
	})
	rec := httptest.NewRecorder()
	s.handleMattermostActionCallback(rec, httptest.NewRequest(http.MethodPost, "/api/v1/im/mattermost/actions", bytes.NewReader(body)))

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errMap, _ := resp["error"].(map[string]any)
	if errMap == nil || !strings.Contains(errMap["message"].(string), "部署审批权限") {
		t.Fatalf("expected operator RBAC rejection, got %v", resp)
	}
	got, _, _ := s.controlDB.DeployRequestFor(workspaceID, req.ID)
	if got.Status != "pending_approval" {
		t.Fatalf("status must stay pending_approval after RBAC refusal, got %q", got.Status)
	}
}

// The designated-approver field only renders on pinned requests: absent on
// legacy cards, username-only when no display label was stored, "Label
// (username)" when the creation flow persisted one.
func TestFormatDeployApprovalAttachment_ApproverField(t *testing.T) {
	base := newDeployCardTestRequest("ws-card")

	// Legacy request: no approverId → no approver field (6 fields).
	att := FormatDeployApprovalAttachment(&base, deployCardActionApprove, "tok", "http://console.example")
	fields, _ := att["fields"].([]map[string]any)
	if len(fields) != 6 {
		t.Fatalf("legacy fields = %d, want 6", len(fields))
	}

	// Pinned with label.
	withLabel := base
	withLabel.Approval = map[string]any{"required": true, "state": "pending_approval", "approverId": "alex", "approverLabel": "Alex Chen"}
	att = FormatDeployApprovalAttachment(&withLabel, deployCardActionApprove, "tok", "http://console.example")
	fields, _ = att["fields"].([]map[string]any)
	if len(fields) != 7 {
		t.Fatalf("pinned fields = %d, want 7", len(fields))
	}
	last := fields[len(fields)-1]
	if last["title"] != "指定审批人 (Approver)" || last["value"] != "Alex Chen (alex)" {
		t.Fatalf("approver field = %v/%v, want label + username", last["title"], last["value"])
	}

	// Pinned without label falls back to the bare username.
	bare := base
	bare.Approval = map[string]any{"required": true, "state": "pending_approval", "approverId": "alex"}
	att = FormatDeployApprovalAttachment(&bare, deployCardActionApprove, "tok", "http://console.example")
	fields, _ = att["fields"].([]map[string]any)
	last = fields[len(fields)-1]
	if last["value"] != "alex" {
		t.Fatalf("bare approver field = %v, want username fallback", last["value"])
	}
}
