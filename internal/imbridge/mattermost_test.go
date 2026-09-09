package imbridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMattermostProviderInfo(t *testing.T) {
	provider, ok := LookupProvider("mattermost")
	if !ok {
		t.Fatalf("mattermost provider not registered")
	}
	info := provider.Info()
	if info.ID != "mattermost" || info.SetupMode != "manual" {
		t.Fatalf("unexpected info: %#v", info)
	}
	if len(info.Fields) < 3 {
		t.Fatalf("expected at least 3 setup fields, got %d", len(info.Fields))
	}
}

func TestMattermostManualSetup(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/me" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-bot-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       "bot-12345",
			"username": "multigent-bot",
			"is_bot":   true,
			"roles":    "system_user",
		})
	}))
	defer ts.Close()

	p := mattermostProvider{}

	// 1. Missing bridgeHmacSecret must fail
	_, err := p.ManualSetup(context.Background(), ManualSetupRequest{
		Values: map[string]string{
			"baseUrl":      ts.URL,
			"botToken":     "test-bot-token",
			"commandToken": "cmd-tok-123",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "bridge HMAC secret is required") {
		t.Fatalf("expected error for missing bridgeHmacSecret, got: %v", err)
	}

	// 2. Valid setup with bridgeHmacSecret succeeds
	res, err := p.ManualSetup(context.Background(), ManualSetupRequest{
		Values: map[string]string{
			"baseUrl":          ts.URL,
			"botToken":         "test-bot-token",
			"commandToken":     "cmd-tok-123",
			"bridgeHmacSecret": "my-secret-key-123",
		},
	})
	if err != nil {
		t.Fatalf("manual setup failed: %v", err)
	}
	if res.AppID != "bot-12345" || res.ExternalBotID != "bot-12345" {
		t.Fatalf("unexpected app ID: %#v", res)
	}
	if res.SecretValues["baseUrl"] != ts.URL || res.SecretValues["botToken"] != "test-bot-token" || res.SecretValues["commandToken"] != "cmd-tok-123" || res.SecretValues["bridgeHmacSecret"] != "my-secret-key-123" {
		t.Fatalf("unexpected secret values: %#v", res.SecretValues)
	}
}

func TestMattermostParseEvent(t *testing.T) {
	p := mattermostProvider{}

	// Valid posted event from human user
	postJSON, _ := json.Marshal(map[string]any{
		"id":         "post-999",
		"create_at":  1700000000000,
		"user_id":    "usr-human",
		"channel_id": "ch-direct-1",
		"message":    "hello agent",
		"root_id":    "",
	})
	eventJSON, _ := json.Marshal(map[string]any{
		"event": "posted",
		"data": map[string]any{
			"channel_id":   "ch-direct-1",
			"channel_type": "D",
			"sender_name":  "human_user",
			"bot_id":       "bot-12345",
			"post":         string(postJSON),
		},
	})

	ev, err := p.ParseEvent(eventJSON)
	if err != nil {
		t.Fatalf("parse event: %v", err)
	}
	if !ev.IsMessage {
		t.Fatalf("expected isMessage=true")
	}
	if ev.Message.MessageID != "post-999" || ev.Message.SenderOpenID != "usr-human" || ev.Message.Text != "hello agent" {
		t.Fatalf("unexpected parsed message: %#v", ev.Message)
	}

	// Message from the bot itself should be ignored
	botPostJSON, _ := json.Marshal(map[string]any{
		"id":         "post-1000",
		"user_id":    "bot-12345",
		"channel_id": "ch-direct-1",
		"message":    "I am bot",
	})
	botEventJSON, _ := json.Marshal(map[string]any{
		"event": "posted",
		"data": map[string]any{
			"channel_id":   "ch-direct-1",
			"channel_type": "D",
			"bot_id":       "bot-12345",
			"post":         string(botPostJSON),
		},
	})
	botEv, err := p.ParseEvent(botEventJSON)
	if err != nil {
		t.Fatalf("parse bot event: %v", err)
	}
	if botEv.IsMessage {
		t.Fatalf("bot own message should not be flagged as IsMessage")
	}
}

func TestMattermostShouldHandleMessage(t *testing.T) {
	p := mattermostProvider{}
	// Direct message should be handled
	if !p.ShouldHandleMessage("", IncomingMessage{ChatType: "D", ChatID: "ch-1"}) {
		t.Fatalf("direct message type D should be handled")
	}
	if !p.ShouldHandleMessage("", IncomingMessage{ChatType: "direct", ChatID: "ch-1"}) {
		t.Fatalf("direct message type 'direct' should be handled")
	}
	// Group message without matching bound channel should be ignored
	if p.ShouldHandleMessage("", IncomingMessage{ChatType: "O", ChatID: "ch-public"}) {
		t.Fatalf("unbound channel message should be ignored")
	}
	// Matching bound channel should be handled
	if !p.ShouldHandleMessage("ch-public", IncomingMessage{ChatType: "O", ChatID: "ch-public"}) {
		t.Fatalf("bound channel message should be handled")
	}
}

func TestMattermostSendMessage(t *testing.T) {
	var receivedPost map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" {
			_ = json.NewDecoder(r.Body).Decode(&receivedPost)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-post-1"})
			return
		}
		if r.URL.Path == "/api/v4/channels/direct" {
			// Direct channel creation
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "dm-ch-999"})
			return
		}
		t.Fatalf("unexpected path: %s", r.URL.Path)
	}))
	defer ts.Close()

	p := mattermostProvider{}
	secrets := map[string]string{
		"baseUrl":  ts.URL,
		"botToken": "bot-tok",
		"appId":    "bot-id",
	}

	// Send to existing chat
	err := p.SendMessage(context.Background(), secrets, OutgoingTarget{ChatID: "ch-123"}, OutgoingMessage{Text: "test msg"})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	if receivedPost["channel_id"] != "ch-123" || receivedPost["message"] != "test msg" {
		t.Fatalf("unexpected received post: %#v", receivedPost)
	}

	// Send to user via ReceiveID (open direct channel first)
	err = p.SendMessage(context.Background(), secrets, OutgoingTarget{ReceiveID: "usr-456"}, OutgoingMessage{Text: "direct msg"})
	if err != nil {
		t.Fatalf("send direct message: %v", err)
	}
	if receivedPost["channel_id"] != "dm-ch-999" || receivedPost["message"] != "direct msg" {
		t.Fatalf("unexpected received post: %#v", receivedPost)
	}
}

func TestMattermostVerifyForwardedEvent(t *testing.T) {
	p := mattermostProvider{}
	secrets := map[string]string{
		"bridgeHmacSecret": "my-secret-key",
	}

	// 1. Non-loopback request MUST fail (B1 invariant)
	reqNonLoopback := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqNonLoopback.RemoteAddr = "192.168.1.50:54321"
	if err := p.VerifyForwardedEvent(reqNonLoopback, []byte("test"), secrets); err == nil {
		t.Fatalf("expected non-loopback RemoteAddr to fail")
	}

	// 2. Loopback request with valid HMAC
	rawBody := []byte(`{"event":"posted"}`)
	nowTs := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte("my-secret-key"))
	mac.Write([]byte(nowTs + "."))
	mac.Write(rawBody)
	sig := hex.EncodeToString(mac.Sum(nil))

	reqLoopback := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqLoopback.RemoteAddr = "127.0.0.1:45678"
	reqLoopback.Header.Set("X-Mattermost-Forward-Signature", sig)
	reqLoopback.Header.Set("X-Mattermost-Forward-Timestamp", nowTs)

	if err := p.VerifyForwardedEvent(reqLoopback, rawBody, secrets); err != nil {
		t.Fatalf("valid loopback HMAC failed: %v", err)
	}

	// 3. Loopback request with invalid HMAC
	reqBadSig := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqBadSig.RemoteAddr = "127.0.0.1:45678"
	reqBadSig.Header.Set("X-Mattermost-Forward-Signature", "wrong-sig")
	reqBadSig.Header.Set("X-Mattermost-Forward-Timestamp", nowTs)
	if err := p.VerifyForwardedEvent(reqBadSig, rawBody, secrets); err == nil {
		t.Fatalf("expected invalid signature to fail")
	}

	// 4. Expired timestamp (> 5 minutes)
	oldTs := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	oldMac := hmac.New(sha256.New, []byte("my-secret-key"))
	oldMac.Write([]byte(oldTs + "."))
	oldMac.Write(rawBody)
	oldSig := hex.EncodeToString(oldMac.Sum(nil))

	reqExpired := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqExpired.RemoteAddr = "127.0.0.1:45678"
	reqExpired.Header.Set("X-Mattermost-Forward-Signature", oldSig)
	reqExpired.Header.Set("X-Mattermost-Forward-Timestamp", oldTs)
	if err := p.VerifyForwardedEvent(reqExpired, rawBody, secrets); err == nil {
		t.Fatalf("expected expired timestamp to fail")
	}

	// 5. Fail-closed: missing bridgeHmacSecret in secrets must fail
	emptySecrets := map[string]string{}
	reqNoSecret := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqNoSecret.RemoteAddr = "127.0.0.1:45678"
	reqNoSecret.Header.Set("X-Mattermost-Forward-Signature", sig)
	reqNoSecret.Header.Set("X-Mattermost-Forward-Timestamp", nowTs)
	if err := p.VerifyForwardedEvent(reqNoSecret, rawBody, emptySecrets); err == nil || !strings.Contains(err.Error(), "bridge HMAC secret not configured") {
		t.Fatalf("expected fail-closed error for empty secret, got: %v", err)
	}

	// 6. M3: nil secrets (caller has no matching binding) passes loopback check
	reqNilSecrets := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqNilSecrets.RemoteAddr = "127.0.0.1:45678"
	if err := p.VerifyForwardedEvent(reqNilSecrets, rawBody, nil); err != nil {
		t.Fatalf("expected nil secrets with loopback to return nil (preserve diagnostic), got: %v", err)
	}
	// But non-loopback with nil secrets still fails
	reqNilNonLoopback := httptest.NewRequest(http.MethodPost, "/events", nil)
	reqNilNonLoopback.RemoteAddr = "192.168.1.50:54321"
	if err := p.VerifyForwardedEvent(reqNilNonLoopback, rawBody, nil); err == nil {
		t.Fatalf("expected non-loopback with nil secrets to fail")
	}
}

func TestSanitizeMattermostChannelName(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"#proj-order-center", "proj-order-center"},
		{"proj-order-center", "proj-order-center"},
		{"Project 123!", "project-123"},
		{"--test--channel--", "test-channel"},
		{"a", "proj-a"},
		{"", "proj-chat"},
		{"UPPER_case-Name", "upper_case-name"},
	}
	for _, c := range cases {
		got := SanitizeMattermostChannelName(c.input)
		if got != c.expected {
			t.Errorf("SanitizeMattermostChannelName(%q) = %q, expected %q", c.input, got, c.expected)
		}
	}
}

func TestMattermostClient_Operations(t *testing.T) {
	createCalled := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]MattermostTeam{
				{ID: "team-1", Name: "myteam", DisplayName: "My Team"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			if !createCalled {
				createCalled = true
				var ch MattermostChannel
				_ = json.NewDecoder(r.Body).Decode(&ch)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(MattermostChannel{
					ID:          "chan-123",
					TeamID:      ch.TeamID,
					Name:        ch.Name,
					DisplayName: ch.DisplayName,
					Type:        ch.Type,
				})
			} else {
				// Second call simulates channel already exists
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"id":"store.sql_channel.saved.create.found.app_error","message":"A channel with that name already exists"}`))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/teams/team-1/channels/name/"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(MattermostChannel{
				ID:          "chan-123",
				TeamID:      "team-1",
				Name:        "proj-test",
				DisplayName: "Project Test",
				Type:        "P",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := NewMattermostClient(ts.URL, "bot-token-xyz", ts.Client())
	ctx := context.Background()

	// 1. Get teams
	teams, err := client.GetMyTeams(ctx)
	if err != nil || len(teams) != 1 || teams[0].ID != "team-1" {
		t.Fatalf("unexpected teams result: %v, %v", teams, err)
	}

	// 2. Create channel (fresh)
	created, err := client.CreateChannel(ctx, MattermostChannel{
		TeamID:      "team-1",
		Name:        "proj-test",
		DisplayName: "Project Test",
		Type:        "P",
	})
	if err != nil || created.ID != "chan-123" {
		t.Fatalf("unexpected created channel: %v, %v", created, err)
	}

	// 3. Create channel (already exists -> returns ErrChannelAlreadyExists)
	_, err = client.CreateChannel(ctx, MattermostChannel{
		TeamID:      "team-1",
		Name:        "proj-test",
		DisplayName: "Project Test",
		Type:        "P",
	})
	if !errors.Is(err, ErrChannelAlreadyExists) {
		t.Fatalf("expected ErrChannelAlreadyExists, got %v", err)
	}

	// 4. Add user to team
	if err := client.AddUserToTeam(ctx, "team-1", "user-456"); err != nil {
		t.Fatalf("AddUserToTeam failed: %v", err)
	}

	// 5. Add user to channel
	if err := client.AddUserToChannel(ctx, "chan-123", "user-456"); err != nil {
		t.Fatalf("AddUserToChannel failed: %v", err)
	}
}
