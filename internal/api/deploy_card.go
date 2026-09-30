package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/imbridge"
)

// Deploy-center approval cards (deploy center batch 4).
//
// When a deploy request enters pending_approval, PostDeployApprovalCard posts
// an interactive Mattermost card with [批准][驳回] buttons. The buttons carry
// signed action tokens routed through the existing chatops callback endpoint
// (/api/v1/im/mattermost/actions), which reuses the full verification chain
// (HMAC → user verification → identity binding → anti-replay nonce) before
// advancing the deploy request via CAS.
//
// Contract notes carried over from the human-review card:
//   - the action verb lives in integration.context.action while ONE token is
//     signed per card whose payload Action matches the button (the callback
//     handler enforces context.action == tokenData.Action, so approve/reject
//     each need their own token — same shape as FormatHumanReviewAttachment);
//   - ActionTokenPayload.TaskID (tsk) carries the deploy request id and
//     StepID is the fixed "deploy" step marker;
//   - ExpectedStateVersion/ReviewSnapshotHash are workflow-run concepts that
//     do not exist on deploy requests, so they stay at zero values and the
//     CAS protection is the deploy request Status column itself
//     (UpdateDeployRequestStatus from='pending_approval').
//
// Everything here is best-effort: card failures are logged and returned as
// errors to callers that can surface them, but they never block the deploy
// request lifecycle.

// deployCardStepID is the fixed StepID stamped into deploy approval action
// tokens. It distinguishes deploy cards from workflow review cards in logs
// and session rows; the callback dispatches on Action, not on this value.
const deployCardStepID = "deploy"

// deployCardActions are the two verbs the chatops callback recognizes.
const (
	deployCardActionApprove = "deploy_approve"
	deployCardActionReject  = "deploy_reject"
)

// FormatDeployApprovalAttachment builds the Mattermost attachment for a
// pending-approval deploy card. token is the pre-signed action token for the
// button identified by action (deploy_approve | deploy_reject); the same
// signed shape is used for both buttons, mirroring
// imbridge.FormatHumanReviewAttachment.
func FormatDeployApprovalAttachment(req *controldb.DeployRequest, action, token string) map[string]any {
	fields := []map[string]any{
		{"title": "项目 (Project)", "value": deployCardValue(req.ProjectID), "short": true},
		{"title": "分支 (Branch)", "value": deployCardValue(req.Branch), "short": true},
		{"title": "SHA", "value": deployCardShortSHA(req.SHA), "short": true},
		{"title": "环境 (Env)", "value": deployCardEnvLabel(req.Env), "short": true},
		{"title": "提交 (Commits)", "value": deployCommitSpanSummary(req.CommitSpan), "short": false},
		{"title": "发起人 (Requested By)", "value": deployCardValue(req.CreatedBy), "short": true},
	}

	return map[string]any{
		"color": "#f59e0b",
		"title": "🚀 部署审批 (Deploy Approval)",
		"text":  "请核对以下部署单信息，选择审批操作：",
		"fields": fields,
		"actions": []map[string]any{
			deployCardButton("deploy-approve", "✅ 批准部署 (Approve)", "success", deployCardActionApprove, token),
			deployCardButton("deploy-reject", "⛔ 驳回 (Reject)", "danger", deployCardActionReject, token),
		},
	}
}

// deployCardButton renders one interactive button. The action verb is embedded
// both in the signed token payload and in integration.context.action — the
// callback handler rejects clicks where the two disagree (tamper check).
func deployCardButton(id, name, style, action, token string) map[string]any {
	return map[string]any{
		"id":    id,
		"name":  name,
		"type":  "button",
		"style": style,
		"integration": map[string]any{
			"url": "/api/v1/im/mattermost/actions",
			"context": map[string]any{
				"action_token": token,
				"action":       action,
			},
		},
	}
}

func deployCardValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "(未提供)"
	}
	return v
}

func deployCardEnvLabel(env string) string {
	env = strings.TrimSpace(env)
	if env == "" {
		return "production"
	}
	return env
}

func deployCardShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "(未提供)"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// deployCommitSpanSummary renders "N 条：first title" — the span length plus
// the first commit title as a quick orientation line.
func deployCommitSpanSummary(span []controldb.CommitSpanEntry) string {
	if len(span) == 0 {
		return "(无提交记录)"
	}
	title := strings.TrimSpace(span[0].Title)
	if title == "" {
		title = deployCardShortSHA(span[0].SHA)
	}
	if len(title) > 80 {
		title = title[:80] + "…"
	}
	return fmt.Sprintf("%d 条 (共 %s): %s", len(span), deployCardShortSHA(span[len(span)-1].SHA), title)
}

// signDeployCardToken signs one action token for the deploy card. The token
// TTL matches the human-review card (2h); ExpectedStateVersion and
// ReviewSnapshotHash stay zero — deploy requests CAS on Status instead.
func (s *Server) signDeployCardToken(req *controldb.DeployRequest, connectionID, channelID, action string, expiresAt int64) (string, error) {
	secret, err := s.deployCardSigningSecret(req.WorkspaceID, req.ProjectID)
	if err != nil {
		return "", err
	}
	return imbridge.SignActionToken(secret, imbridge.ActionTokenPayload{
		WorkspaceID:  req.WorkspaceID,
		ProjectID:    req.ProjectID,
		TaskID:       req.ID, // carries the deploy request id (see package docs)
		StepID:       deployCardStepID,
		Action:       action,
		ChannelID:    channelID,
		ConnectionID: connectionID,
		Nonce:        imbridge.GenerateNonce(),
		ExpiresAt:    expiresAt,
	})
}

// deployCardSigningSecret resolves the Mattermost connection's HMAC secret —
// the same material verifyMattermostActionToken will verify against.
func (s *Server) deployCardSigningSecret(workspaceID, projectID string) (string, error) {
	if s.threadProjections == nil {
		return "", errors.New("mattermost projection service unavailable")
	}
	_, _, _, _, hmacSecret, err := s.threadProjections.ResolveMMTarget(workspaceID, projectID, "")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(hmacSecret) == "" {
		return "", errors.New("mattermost signing secret unavailable")
	}
	return hmacSecret, nil
}

// PostDeployApprovalCard posts the pending-approval deploy card into the
// project's Mattermost channel (project binding → workspace fallback, same
// resolution as task threads). Best-effort by contract: errors are returned
// for logging, never abort the deploy request lifecycle. Returns the created
// post id so callers can archive the card later.
func (s *Server) PostDeployApprovalCard(ctx context.Context, req *controldb.DeployRequest) (string, error) {
	if req == nil {
		return "", errors.New("deploy request is nil")
	}
	if s.threadProjections == nil {
		return "", errors.New("mattermost projection service unavailable")
	}
	baseURL, botToken, channelID, connectionID, _, err := s.threadProjections.ResolveMMTarget(req.WorkspaceID, req.ProjectID, "")
	if err != nil {
		return "", fmt.Errorf("resolve mattermost target: %w", err)
	}
	if strings.TrimSpace(channelID) == "" {
		return "", errors.New("no mattermost channel bound for deploy approval card")
	}

	expiresAt := time.Now().UTC().Add(2 * time.Hour).Unix()
	// Both buttons embed the action verb in context; each carries its own
	// signed token whose payload Action must equal the context action or the
	// callback rejects the click as tampered. Same-per-button signing matches
	// FormatHumanReviewAttachment's per-button createBtn.
	approveTok, err := s.signDeployCardToken(req, connectionID, channelID, deployCardActionApprove, expiresAt)
	if err != nil {
		return "", fmt.Errorf("sign deploy approve token: %w", err)
	}
	rejectTok, err := s.signDeployCardToken(req, connectionID, channelID, deployCardActionReject, expiresAt)
	if err != nil {
		return "", fmt.Errorf("sign deploy reject token: %w", err)
	}

	attachment := FormatDeployApprovalAttachment(req, deployCardActionApprove, approveTok)
	actions, _ := attachment["actions"].([]map[string]any)
	if len(actions) == 2 {
		actions[1]["integration"] = map[string]any{
			"url": "/api/v1/im/mattermost/actions",
			"context": map[string]any{
				"action_token": rejectTok,
				"action":       deployCardActionReject,
			},
		}
	}

	props := map[string]any{"attachments": []any{attachment}}
	message := fmt.Sprintf("#### 🚀 部署审批待处理: %s @ %s", req.Branch, deployCardShortSHA(req.SHA))
	postID, err := s.threadProjections.CreatePostWithProps(ctx, baseURL, botToken, channelID, "", message, props)
	if err != nil {
		return "", fmt.Errorf("post deploy approval card: %w", err)
	}
	log.Printf("[deploy-card] approval card posted request=%s project=%s post_id=%s", req.ID, req.ProjectID, postID)
	return postID, nil
}

// ArchiveDeployApprovalCard patches the card post to a completed state:
// buttons are stripped (the patch replaces props.attachments wholesale) and
// a settled summary line replaces the interactive card. Best-effort: a failed
// archival is logged and returned but must never block the deploy lifecycle.
func (s *Server) ArchiveDeployApprovalCard(ctx context.Context, req *controldb.DeployRequest, postID, statusText string) error {
	if req == nil || strings.TrimSpace(postID) == "" {
		return nil
	}
	if s.threadProjections == nil {
		return errors.New("mattermost projection service unavailable")
	}
	baseURL, botToken, _, _, _, err := s.threadProjections.ResolveMMTarget(req.WorkspaceID, req.ProjectID, "")
	if err != nil {
		return fmt.Errorf("resolve mattermost target: %w", err)
	}
	payload := map[string]any{
		"props": map[string]any{
			"attachments": []any{
				map[string]any{
					"color": "#10b981",
					"title": "部署审批已完结 (已归档)",
					"text":  statusText,
				},
			},
		},
	}
	if err := s.threadProjections.PatchPost(ctx, baseURL, botToken, postID, payload); err != nil {
		return fmt.Errorf("archive deploy approval card: %w", err)
	}
	return nil
}

// approveDeployRequestFromChatops advances a deploy request on a verified
// Mattermost approve click: CAS pending_approval → approved, audit event,
// card archival, then pipeline trigger. The trigger is the same shared body
// the approve REST endpoint uses (triggerDeployPipelineNow); it is invoked
// through the deployTriggerHook seam so batch wiring stays swappable in
// tests. Returns (advanced, httpStatus, err): advanced=false with a 409-
// flavoured message when the request was already processed.
func (s *Server) approveDeployRequestFromChatops(ctx context.Context, req *controldb.DeployRequest, actorUsername string) error {
	moved, err := s.controlDB.UpdateDeployRequestStatus(req.WorkspaceID, req.ID, "pending_approval", "approved")
	if err != nil {
		return fmt.Errorf("approve deploy request: %w", err)
	}
	if !moved {
		return errDeployRequestAlreadyProcessed
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  req.WorkspaceID,
		ActorType:    "user",
		ActorID:      actorUsername,
		Action:       "deploy.request.approve",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
		Summary:      fmt.Sprintf("deploy request %s approved via mattermost chatops by %s", req.ID, actorUsername),
	})

	if s.deployTriggerHook != nil {
		if err := s.deployTriggerHook(ctx, req, actorUsername); err != nil {
			log.Printf("[deploy-card] trigger hook for %s failed: %v", req.ID, err)
			return fmt.Errorf("trigger deploy pipeline: %w", err)
		}
	}
	return nil
}

// rejectDeployRequestFromChatops moves a deploy request to rejected on a
// verified Mattermost reject click. CAS-only; terminal state.
func (s *Server) rejectDeployRequestFromChatops(ctx context.Context, req *controldb.DeployRequest, actorUsername string) error {
	moved, err := s.controlDB.UpdateDeployRequestStatus(req.WorkspaceID, req.ID, "pending_approval", "rejected")
	if err != nil {
		return fmt.Errorf("reject deploy request: %w", err)
	}
	if !moved {
		return errDeployRequestAlreadyProcessed
	}

	s.auditLog(auditLogInput{
		WorkspaceID:  req.WorkspaceID,
		ActorType:    "user",
		ActorID:      actorUsername,
		Action:       "deploy.request.reject",
		ResourceType: "deploy_request",
		ResourceID:   req.ID,
		Summary:      fmt.Sprintf("deploy request %s rejected via mattermost chatops by %s", req.ID, actorUsername),
	})

	return nil
}

// errDeployRequestAlreadyProcessed maps to the 409-semantics chatops reply
// when the CAS loses a race (card clicked twice, or decided on the console).
var errDeployRequestAlreadyProcessed = errors.New("该部署单已被处理 (deploy request already processed)")

// triggerApprovedDeployRequest is the chatops-side pipeline trigger. It wraps
// the shared triggerDeployPipelineNow body from deploy_handlers.go so the
// approve path and the REST endpoint stay on one code path. The Server field
// deployTriggerHook is the seam: batch wiring assigns it after construction
// (nil = no-op, best-effort), and tests inject a stub there.
func (s *Server) triggerApprovedDeployRequest(ctx context.Context, req *controldb.DeployRequest, actorUsername string) error {
	if req == nil {
		return errors.New("deploy request is nil")
	}
	// The shared trigger body needs a request for audit attribution and the
	// reachable console URL; a minimal synthetic request is enough because the
	// chatops path never reads its body or query.
	r := &http.Request{Host: "chatops.internal", Header: make(http.Header)}
	r = r.WithContext(ctx)
	rec := &nopResponseWriter{}
	s.triggerDeployPipelineNow(rec, r, req.ProjectID, req.WorkspaceID, req)
	return nil
}

// nopResponseWriter swallows the JSON response of the shared trigger body —
// the chatops path surfaces outcomes via audit events and the card update.
type nopResponseWriter struct {
	header http.Header
	code   int
}

func (w *nopResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *nopResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *nopResponseWriter) WriteHeader(code int) { w.code = code }
