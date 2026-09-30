package imbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Exported bridges for the deploy-center approval cards (deploy center batch
// 4). The task-thread projection service owns Mattermost credential
// resolution and the raw HTTP surface; these wrappers expose exactly what the
// api package's deploy_card.go needs — resolve target, create a post with
// props, patch an existing post — without duplicating connection/secret logic
// or widening the internal API further. They are thin pass-throughs on purpose.

// ResolveMMTarget resolves the Mattermost target for a workspace/project:
// base URL, bot token, channel (project binding → workspace fallback),
// connection id and the card-signing HMAC secret. Exported so the deploy
// approval card flow can sign action tokens with the same secret the
// callback endpoint will verify against.
func (s *TaskThreadProjectionService) ResolveMMTarget(workspaceID, projectID, preferredChannelID string) (baseURL, botToken, channelID, connectionID, hmacSecret string, err error) {
	if s == nil {
		return "", "", "", "", "", fmt.Errorf("task thread projection service unavailable")
	}
	return s.resolveMMTarget(workspaceID, projectID, preferredChannelID)
}

// CreatePostWithProps posts a raw message with interactive props (buttons)
// into a Mattermost channel and returns the created post id.
func (s *TaskThreadProjectionService) CreatePostWithProps(ctx context.Context, baseURL, botToken, channelID, rootID, message string, props map[string]any) (string, error) {
	if s == nil {
		return "", fmt.Errorf("task thread projection service unavailable")
	}
	return s.createPostWithProps(ctx, baseURL, botToken, channelID, rootID, message, props)
}

// PatchPost PATCHes an existing Mattermost post (PUT /api/v4/posts/{id}/patch)
// with the given payload. Used to archive interactive cards: replace the
// message and strip the action buttons once a decision has been recorded.
func (s *TaskThreadProjectionService) PatchPost(ctx context.Context, baseURL, botToken, postID string, payload map[string]any) error {
	if s == nil {
		return fmt.Errorf("task thread projection service unavailable")
	}
	baseURL = strings.TrimRight(baseURL, "/")
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/api/v4/posts/"+postID+"/patch", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json")

	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mattermost patch post (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}
