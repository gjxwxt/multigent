package imbridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type mattermostProvider struct{}

func (mattermostProvider) Info() ProviderInfo {
	return ProviderInfo{
		ID:        "mattermost",
		Label:     "Mattermost",
		SetupMode: "manual",
		Fields: []ManualSetupField{
			{Name: "baseUrl", Label: "Mattermost Server URL", Type: "text", Required: true, Placeholder: "http://127.0.0.1:8065", Help: "URL of your Mattermost instance."},
			{Name: "botToken", Label: "Bot Access Token", Type: "password", Required: true, Placeholder: "...", Help: "Access token for the Multigent Bot user."},
			{Name: "commandToken", Label: "Slash Command Token", Type: "password", Required: false, Placeholder: "...", Help: "Token of the /bind slash command from Mattermost integrations."},
			{Name: "bridgeHmacSecret", Label: "Bridge HMAC Secret", Type: "password", Required: true, Placeholder: "...", Help: "HMAC shared secret between mattermost-bridge and Multigent (required for loopback verification)."},
		},
	}
}

func (mattermostProvider) OpenBaseURL() string { return "" }

func (mattermostProvider) BeginSetup(context.Context) (SetupBeginResponse, error) {
	return SetupBeginResponse{}, fmt.Errorf("mattermost uses manual setup")
}

func (mattermostProvider) PollSetup(context.Context, string, string) (SetupPollResponse, error) {
	return SetupPollResponse{}, fmt.Errorf("mattermost uses manual setup")
}

func (mattermostProvider) ManualSetup(ctx context.Context, req ManualSetupRequest) (ManualSetupResult, error) {
	values := normalizeManualValues(req.Values)
	baseURL := strings.TrimRight(strings.TrimSpace(values["baseUrl"]), "/")
	if baseURL == "" {
		return ManualSetupResult{}, fmt.Errorf("mattermost server URL is required")
	}
	token := strings.TrimSpace(values["botToken"])
	if token == "" {
		return ManualSetupResult{}, fmt.Errorf("mattermost bot token is required")
	}
	hmacSecret := strings.TrimSpace(values["bridgeHmacSecret"])
	if hmacSecret == "" {
		return ManualSetupResult{}, fmt.Errorf("bridge HMAC secret is required")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v4/users/me", nil)
	if err != nil {
		return ManualSetupResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return ManualSetupResult{}, fmt.Errorf("mattermost users/me: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ManualSetupResult{}, fmt.Errorf("mattermost token verification failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
		Roles    string `json:"roles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ManualSetupResult{}, fmt.Errorf("decode mattermost users/me response: %w", err)
	}
	if parsed.ID == "" {
		return ManualSetupResult{}, fmt.Errorf("mattermost user id not returned")
	}

	appID := parsed.ID
	return ManualSetupResult{
		Provider:      "mattermost",
		BaseURL:       baseURL,
		AuthType:      "bot_token",
		AppID:         appID,
		ExternalBotID: parsed.ID,
		SecretValues: map[string]string{
			"baseUrl":          baseURL,
			"botToken":         token,
			"commandToken":     strings.TrimSpace(values["commandToken"]),
			"bridgeHmacSecret": strings.TrimSpace(values["bridgeHmacSecret"]),
			"appId":            appID,
		},
		Profile: map[string]any{
			"botId":    parsed.ID,
			"username": parsed.Username,
			"isBot":    parsed.IsBot,
			"roles":    parsed.Roles,
			"baseUrl":  baseURL,
		},
	}, nil
}

func (mattermostProvider) ExtractEncryptedPayload([]byte) (string, bool) { return "", false }
func (mattermostProvider) DecryptEvent(string, string) ([]byte, error)   { return nil, nil }

func (mattermostProvider) ParseEvent(raw []byte) (ParsedEvent, error) {
	var ev struct {
		Event string `json:"event"`
		Data  struct {
			ChannelID   string `json:"channel_id"`
			ChannelType string `json:"channel_type"`
			TeamID      string `json:"team_id"`
			Post        string `json:"post"`
			SenderName  string `json:"sender_name"`
			BotID       string `json:"bot_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ParsedEvent{}, err
	}
	if ev.Event != "posted" || ev.Data.Post == "" {
		return ParsedEvent{}, nil
	}
	var post struct {
		ID        string `json:"id"`
		CreateAt  int64  `json:"create_at"`
		UserID    string `json:"user_id"`
		ChannelID string `json:"channel_id"`
		Message   string `json:"message"`
		RootID    string `json:"root_id"`
	}
	if err := json.Unmarshal([]byte(ev.Data.Post), &post); err != nil {
		return ParsedEvent{}, err
	}
	if post.UserID == "" || (ev.Data.BotID != "" && post.UserID == ev.Data.BotID) {
		return ParsedEvent{}, nil
	}
	return ParsedEvent{
		AppID:     ev.Data.BotID,
		IsMessage: true,
		Message: IncomingMessage{
			MessageID:    post.ID,
			ChatID:       post.ChannelID,
			ChatType:     ev.Data.ChannelType,
			RootID:       post.RootID,
			SenderOpenID: post.UserID,
			SenderUserID: post.UserID,
			Text:         post.Message,
			RawContent:   post.Message,
		},
	}, nil
}

func (mattermostProvider) ShouldHandleMessage(boundChatID string, message IncomingMessage) bool {
	if strings.EqualFold(message.ChatType, "D") || strings.EqualFold(message.ChatType, "direct") {
		return true
	}
	if strings.TrimSpace(boundChatID) != "" && strings.TrimSpace(boundChatID) == strings.TrimSpace(message.ChatID) {
		return true
	}
	return false
}

func (p mattermostProvider) SendText(ctx context.Context, secrets map[string]string, target OutgoingTarget, text string) error {
	return p.SendMessage(ctx, secrets, target, OutgoingMessage{Format: "text", Text: text})
}

func (mattermostProvider) ReplyText(ctx context.Context, secrets map[string]string, message IncomingMessage, text string) error {
	baseURL := strings.TrimRight(secrets["baseUrl"], "/")
	token := secrets["botToken"]
	if baseURL == "" || token == "" {
		return fmt.Errorf("mattermost credentials missing")
	}
	payload := map[string]any{
		"channel_id": message.ChatID,
		"message":    text,
	}
	if message.RootID != "" {
		payload["root_id"] = message.RootID
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v4/posts", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mattermost reply failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (mattermostProvider) SendMessage(ctx context.Context, secrets map[string]string, target OutgoingTarget, message OutgoingMessage) error {
	baseURL := strings.TrimRight(secrets["baseUrl"], "/")
	token := secrets["botToken"]
	botUserID := secrets["appId"]
	if baseURL == "" || token == "" {
		return fmt.Errorf("mattermost credentials missing")
	}

	channelID := strings.TrimSpace(target.ChatID)
	// If ChatID is empty but ReceiveID (external user_id) is provided, open a direct channel
	if channelID == "" && strings.TrimSpace(target.ReceiveID) != "" {
		if botUserID == "" {
			return fmt.Errorf("mattermost bot user id (appId) is required to open direct channel")
		}
		directBody, _ := json.Marshal([]string{botUserID, strings.TrimSpace(target.ReceiveID)})
		dReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v4/channels/direct", bytes.NewReader(directBody))
		if err != nil {
			return fmt.Errorf("create direct channel request: %w", err)
		}
		dReq.Header.Set("Authorization", "Bearer "+token)
		dReq.Header.Set("Content-Type", "application/json")
		dResp, err := http.DefaultClient.Do(dReq)
		if err != nil {
			return fmt.Errorf("mattermost open direct channel: %w", err)
		}
		defer dResp.Body.Close()
		if dResp.StatusCode < 200 || dResp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(dResp.Body, 1024))
			return fmt.Errorf("mattermost open direct channel failed (%d): %s", dResp.StatusCode, string(b))
		}
		var dChannel struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(dResp.Body).Decode(&dChannel); err != nil {
			return fmt.Errorf("decode direct channel response: %w", err)
		}
		channelID = dChannel.ID
	}

	if channelID == "" {
		return fmt.Errorf("mattermost channel id is required")
	}

	payload := map[string]any{
		"channel_id": channelID,
		"message":    message.Text,
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v4/posts", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mattermost send post failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (mattermostProvider) VerifyForwardedEvent(r *http.Request, rawBody []byte, secrets map[string]string) error {
	// B1: loopback check using ONLY r.RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("mattermost forwarded events must originate from loopback")
	}

	// M3: when secrets is nil (no matching channel binding found yet in caller),
	// loopback check passes and we return nil so the caller can return its
	// dedicated "channel binding not found" diagnostic without being swallowed.
	if secrets == nil {
		return nil
	}

	secret := strings.TrimSpace(secrets["bridgeHmacSecret"])
	if secret == "" {
		log.Printf("[im:mattermost] bridge HMAC verification rejected: bridgeHmacSecret not configured for connection")
		return fmt.Errorf("bridge HMAC secret not configured")
	}
	sig := r.Header.Get("X-Mattermost-Forward-Signature")
	tsStr := r.Header.Get("X-Mattermost-Forward-Timestamp")
	if sig == "" || tsStr == "" {
		return fmt.Errorf("missing signature headers")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return fmt.Errorf("timestamp expired or invalid")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsStr + "."))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}
