package imbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

// The exported deploy-card bridges pass through to the projection service's
// credential resolution and raw HTTP surface. This test asserts the props
// structure that reaches the Mattermost wire: channel, message root, and the
// attachments envelope the api package's FormatDeployApprovalAttachment feeds in.
func TestCreatePostWithProps_PostsAttachmentsEnvelope(t *testing.T) {
	var postCount atomic.Int32
	var received map[string]any
	mm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" && r.Method == http.MethodPost {
			postCount.Add(1)
			_ = json.NewDecoder(r.Body).Decode(&received)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"post-deploy-card-1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mm.Close()

	store, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	const ws = "ws-deploy-bridge"
	_ = store.UpsertWorkspace(controldb.Workspace{ID: ws, Name: "Deploy", Slug: "deploy"})
	_ = store.UpsertConnection(controldb.Connection{
		ID: "conn-deploy-bridge", WorkspaceID: ws, Provider: "mattermost",
		ConnectionName: "mm", AuthType: "bot_token", Status: "active",
	})
	secret, err := controldb.SealConnectionSecret(map[string]string{
		"baseUrl": mm.URL, "botToken": "tok", "bridgeHmacSecret": "hmac-deploy",
	})
	if err != nil {
		t.Fatalf("SealConnectionSecret: %v", err)
	}
	secret.ConnectionID = "conn-deploy-bridge"
	_ = store.UpsertConnectionSecret(secret)
	_ = store.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID: "b-deploy", WorkspaceID: ws, ProjectID: "proj", AgentID: "pm",
		Provider: "mattermost", ConnectionID: "conn-deploy-bridge",
		ExternalChatID: "chan-deploy", Status: "connected",
	})

	svc := NewTaskThreadProjectionService(store, nil)
	baseURL, botToken, channelID, connID, hmacSecret, err := svc.ResolveMMTarget(ws, "proj", "")
	if err != nil {
		t.Fatalf("ResolveMMTarget: %v", err)
	}
	if channelID != "chan-deploy" || connID != "conn-deploy-bridge" || hmacSecret != "hmac-deploy" {
		t.Fatalf("resolved target channel=%q conn=%q hmac=%q", channelID, connID, hmacSecret)
	}
	if botToken != "tok" {
		t.Fatalf("botToken = %q", botToken)
	}

	props := map[string]any{
		"attachments": []any{
			map[string]any{
				"title": "🚀 部署审批 (Deploy Approval)",
				"actions": []any{
					map[string]any{"id": "deploy-approve", "type": "button"},
					map[string]any{"id": "deploy-reject", "type": "button"},
				},
			},
		},
	}
	postID, err := svc.CreatePostWithProps(context.Background(), baseURL, botToken, channelID, "", "#### 🚀 部署审批待处理", props)
	if err != nil {
		t.Fatalf("CreatePostWithProps: %v", err)
	}
	if postID != "post-deploy-card-1" {
		t.Fatalf("post id = %q", postID)
	}
	if postCount.Load() != 1 {
		t.Fatalf("posts = %d, want 1", postCount.Load())
	}
	if got := received["channel_id"]; got != "chan-deploy" {
		t.Fatalf("wire channel_id = %v", got)
	}
	if got := received["message"]; !strings.Contains(got.(string), "部署审批") {
		t.Fatalf("wire message = %v", got)
	}
	wireProps, _ := received["props"].(map[string]any)
	if wireProps == nil {
		t.Fatalf("wire props missing: %v", received)
	}
	atts, _ := wireProps["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("wire attachments = %d, want 1", len(atts))
	}
	att := atts[0].(map[string]any)
	if title, _ := att["title"].(string); !strings.Contains(title, "Deploy Approval") {
		t.Fatalf("wire attachment title = %v", att["title"])
	}
	actions, _ := att["actions"].([]any)
	if len(actions) != 2 {
		t.Fatalf("wire attachment actions = %d, want 2 buttons", len(actions))
	}
}

// ArchiveDeployApprovalCard's transport: PATCH replaces props.attachments
// with the completed-state card (buttons stripped by replacement).
func TestPatchPost_ReplaceProps(t *testing.T) {
	var patched map[string]any
	mm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts/post-deploy-card-1/patch" && r.Method == http.MethodPut {
			_ = json.NewDecoder(r.Body).Decode(&patched)
			_, _ = w.Write([]byte(`{"status":"OK"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mm.Close()

	svc := NewTaskThreadProjectionService(nil, mm.Client())
	payload := map[string]any{
		"props": map[string]any{"attachments": []any{map[string]any{"title": "部署审批已完结 (已归档)"}}},
	}
	if err := svc.PatchPost(context.Background(), mm.URL, "tok", "post-deploy-card-1", payload); err != nil {
		t.Fatalf("PatchPost: %v", err)
	}
	wireProps, _ := patched["props"].(map[string]any)
	if wireProps == nil {
		t.Fatalf("patch props missing: %v", patched)
	}
	atts, _ := wireProps["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("patched attachments = %d, want 1", len(atts))
	}
	if title, _ := atts[0].(map[string]any)["title"].(string); !strings.Contains(title, "已归档") {
		t.Fatalf("patched card title = %v", atts[0])
	}
}
