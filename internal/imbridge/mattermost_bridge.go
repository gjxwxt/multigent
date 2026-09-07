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
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	controldb "github.com/multigent/multigent/internal/db"
)

// MattermostBridgeConfig configures the Mattermost WebSocket bridge.
type MattermostBridgeConfig struct {
	MultigentURL        string        // Target URL to forward events (e.g. "http://127.0.0.1:27892")
	PollInterval        time.Duration // Interval to poll controlDB for binding changes (default 30s)
	StatusAddr          string        // Local loopback address for status endpoint (default "127.0.0.1:27894")
	CatchUpMaxWindow    time.Duration // Maximum catch-up window (default 24h)
	CursorFlushInterval time.Duration // Interval to persist dirty cursors to DB (default 5s)
}

// DefaultBridgeConfig returns default bridge configuration.
func DefaultBridgeConfig() MattermostBridgeConfig {
	return MattermostBridgeConfig{
		MultigentURL:        "http://127.0.0.1:27892",
		PollInterval:        30 * time.Second,
		StatusAddr:          "127.0.0.1:27894",
		CatchUpMaxWindow:    24 * time.Hour,
		CursorFlushInterval: 5 * time.Second,
	}
}

// MattermostBridge manages WebSocket connections for all connected Mattermost bots.
type MattermostBridge struct {
	cfg              MattermostBridgeConfig
	store            controldb.Store
	httpClient       *http.Client
	supervisors      map[string]*botSupervisor // keyed by binding.ID
	unconfiguredBots map[string]string         // binding.ID -> reason (e.g. missing_hmac_secret)
	mu               sync.Mutex

	// Metrics (process in-memory)
	messagesForwarded atomic.Int64
	catchUpCount      atomic.Int64
	errorsCount       atomic.Int64
	panicCount        atomic.Int64
	lastPanic         string
	lastPanicAt       string
	panicMu           sync.Mutex
	startTime         time.Time
}

// NewMattermostBridge creates a new MattermostBridge.
func NewMattermostBridge(cfg MattermostBridgeConfig, store controldb.Store) *MattermostBridge {
	if cfg.MultigentURL == "" {
		cfg.MultigentURL = "http://127.0.0.1:27892"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.CatchUpMaxWindow <= 0 {
		cfg.CatchUpMaxWindow = 24 * time.Hour
	}
	if cfg.CursorFlushInterval <= 0 {
		cfg.CursorFlushInterval = 5 * time.Second
	}

	return &MattermostBridge{
		cfg:              cfg,
		store:            store,
		httpClient:       &http.Client{Timeout: 15 * time.Second},
		supervisors:      make(map[string]*botSupervisor),
		unconfiguredBots: make(map[string]string),
		startTime:        time.Now(),
	}
}

// Start runs the bridge until ctx is canceled.
func (b *MattermostBridge) Start(ctx context.Context) error {
	log.Printf("[mattermost-bridge] starting bridge, multigent_url=%s poll_interval=%v", b.cfg.MultigentURL, b.cfg.PollInterval)

	// Start status HTTP server if configured
	if b.cfg.StatusAddr != "" {
		go b.runStatusServer(ctx)
	}

	// Initial refresh of bot supervisors
	b.refreshSupervisors(ctx)

	pollTicker := time.NewTicker(b.cfg.PollInterval)
	defer pollTicker.Stop()

	flushTicker := time.NewTicker(b.cfg.CursorFlushInterval)
	defer flushTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[mattermost-bridge] shutting down...")
			b.stopAllSupervisors()
			return ctx.Err()
		case <-pollTicker.C:
			b.refreshSupervisors(ctx)
		case <-flushTicker.C:
			b.flushAllCursors()
		}
	}
}

func (b *MattermostBridge) recordPanic(where string, r any) {
	b.panicCount.Add(1)
	b.errorsCount.Add(1)
	b.panicMu.Lock()
	b.lastPanic = fmt.Sprintf("[%s] %v", where, r)
	b.lastPanicAt = time.Now().UTC().Format(time.RFC3339)
	b.panicMu.Unlock()
	log.Printf("[mattermost-bridge] PANIC RECOVERED in %s: %v\n%s", where, r, debug.Stack())
}

func (b *MattermostBridge) runStatusServer(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		bots := make([]map[string]any, 0, len(b.supervisors))
		for _, s := range b.supervisors {
			bots = append(bots, map[string]any{
				"bindingId":   s.binding.ID,
				"workspaceId": s.workspaceID,
				"project":     s.binding.ProjectID,
				"agent":       s.binding.AgentID,
				"botId":       s.botID,
				"connected":   s.connected.Load(),
			})
		}
		unconfigured := make(map[string]string, len(b.unconfiguredBots))
		for k, v := range b.unconfiguredBots {
			unconfigured[k] = v
		}
		b.mu.Unlock()

		b.panicMu.Lock()
		lastPanic := b.lastPanic
		lastPanicAt := b.lastPanicAt
		b.panicMu.Unlock()

		statusText := "healthy"
		if len(unconfigured) > 0 {
			statusText = "degraded"
		}

		status := map[string]any{
			"status":            statusText,
			"uptime":            time.Since(b.startTime).String(),
			"messagesForwarded": b.messagesForwarded.Load(),
			"catchUpPosts":      b.catchUpCount.Load(),
			"errors":            b.errorsCount.Load(),
			"panicCount":        b.panicCount.Load(),
			"lastPanic":         lastPanic,
			"lastPanicAt":       lastPanicAt,
			"countersScope":     "process-in-memory (resets on restart)",
			"activeBots":        len(bots),
			"unconfiguredBots":  len(unconfigured),
			"unconfigured":      unconfigured,
			"bots":              bots,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})

	server := &http.Server{
		Addr:    b.cfg.StatusAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("[mattermost-bridge] status server listening on %s", b.cfg.StatusAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[mattermost-bridge] status server stopped: %v", err)
	}
}

func (b *MattermostBridge) refreshSupervisors(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.recordPanic("refreshSupervisors", r)
		}
	}()

	bindings, err := b.store.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{
		Provider: "mattermost",
		Status:   "connected",
	})
	if err != nil {
		log.Printf("[mattermost-bridge] list bindings failed: %v", err)
		b.errorsCount.Add(1)
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	activeIDs := make(map[string]bool)
	for _, binding := range bindings {
		activeIDs[binding.ID] = true
		if _, exists := b.supervisors[binding.ID]; exists {
			continue
		}

		secret, ok, err := b.store.ConnectionSecret(binding.ConnectionID)
		if err != nil || !ok {
			log.Printf("[mattermost-bridge] connection secret missing for binding %s: %v", binding.ID, err)
			b.unconfiguredBots[binding.ID] = "missing_connection_secret"
			b.errorsCount.Add(1)
			continue
		}
		values, err := openSecretValues(secret)
		if err != nil {
			log.Printf("[mattermost-bridge] open secret failed for binding %s: %v", binding.ID, err)
			b.unconfiguredBots[binding.ID] = "secret_decrypt_failed"
			b.errorsCount.Add(1)
			continue
		}

		baseURL := strings.TrimRight(strings.TrimSpace(values["baseUrl"]), "/")
		botToken := strings.TrimSpace(values["botToken"])
		hmacSecret := strings.TrimSpace(values["bridgeHmacSecret"])
		botID := strings.TrimSpace(values["appId"])
		if botID == "" {
			botID = binding.ExternalBotID
		}

		if baseURL == "" || botToken == "" || botID == "" {
			log.Printf("[mattermost-bridge] incomplete credentials for binding %s (botId=%s)", binding.ID, botID)
			b.unconfiguredBots[binding.ID] = "incomplete_credentials"
			b.errorsCount.Add(1)
			continue
		}

		if hmacSecret == "" {
			log.Printf("[mattermost-bridge] ERROR: bot %s (%s/%s, binding=%s) missing bridgeHmacSecret in connection secret; skipping supervisor", botID, binding.ProjectID, binding.AgentID, binding.ID)
			b.unconfiguredBots[binding.ID] = "missing_hmac_secret"
			b.errorsCount.Add(1)
			continue
		}
		delete(b.unconfiguredBots, binding.ID)

		subCtx, cancel := context.WithCancel(ctx)
		sup := newBotSupervisor(b, binding, baseURL, botToken, hmacSecret, botID, subCtx, cancel)
		b.supervisors[binding.ID] = sup
		go sup.run()
		log.Printf("[mattermost-bridge] started supervisor for %s/%s (botId=%s, binding=%s)", binding.ProjectID, binding.AgentID, botID, binding.ID)
	}

	// Remove supervisors for bindings that are no longer connected
	for id, sup := range b.supervisors {
		if !activeIDs[id] {
			log.Printf("[mattermost-bridge] stopping supervisor for removed binding %s", id)
			sup.stop()
			delete(b.supervisors, id)
		}
	}
	for id := range b.unconfiguredBots {
		if !activeIDs[id] {
			delete(b.unconfiguredBots, id)
		}
	}
}

func (b *MattermostBridge) flushAllCursors() {
	defer func() {
		if r := recover(); r != nil {
			b.recordPanic("flushAllCursors", r)
		}
	}()

	b.mu.Lock()
	supervisors := make([]*botSupervisor, 0, len(b.supervisors))
	for _, s := range b.supervisors {
		supervisors = append(supervisors, s)
	}
	b.mu.Unlock()

	for _, s := range supervisors {
		s.flushCursors()
	}
}

func (b *MattermostBridge) stopAllSupervisors() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sup := range b.supervisors {
		sup.stop()
		delete(b.supervisors, id)
	}
}

// botSupervisor manages the WS connection and catch-up sync for one bot.
type botSupervisor struct {
	bridge           *MattermostBridge
	binding          controldb.AgentChannelBinding
	workspaceID      string
	baseURL          string
	botToken         string
	bridgeHmacSecret string
	botID            string
	ctx              context.Context
	cancel           context.CancelFunc

	connected     atomic.Bool
	seenPosts     *postIDCache
	knownChannels map[string]bool
	channelsMu    sync.RWMutex
	cursors       map[string]int64 // channel_id -> last_post_create_at (ms)
	cursorsDirty  bool
	cursorsMu     sync.Mutex
}

func newBotSupervisor(
	bridge *MattermostBridge,
	binding controldb.AgentChannelBinding,
	baseURL, botToken, bridgeHmacSecret, botID string,
	ctx context.Context,
	cancel context.CancelFunc,
) *botSupervisor {
	return &botSupervisor{
		bridge:           bridge,
		binding:          binding,
		workspaceID:      binding.WorkspaceID,
		baseURL:          baseURL,
		botToken:         botToken,
		bridgeHmacSecret: bridgeHmacSecret,
		botID:            botID,
		ctx:              ctx,
		cancel:           cancel,
		seenPosts:        newPostIDCache(10000),
		knownChannels:    make(map[string]bool),
		cursors:          make(map[string]int64),
	}
}

func (s *botSupervisor) run() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[mattermost-bridge:%s] supervisor panic recovered: %v", s.botID, r)
		}
		s.flushCursors()
	}()

	backoff := 2 * time.Second
	for s.ctx.Err() == nil {
		started := time.Now()
		err := s.connectAndServe()
		s.connected.Store(false)

		if s.ctx.Err() != nil {
			return
		}

		// If connection lasted > 30s, reset backoff
		if time.Since(started) > 30*time.Second {
			backoff = 2 * time.Second
		}

		log.Printf("[mattermost-bridge:%s] connection closed: %v; reconnecting in %v", s.botID, err, backoff)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 5*time.Minute {
				backoff *= 2
				if backoff > 5*time.Minute {
					backoff = 5 * time.Minute
				}
			}
		}
	}
}

func (s *botSupervisor) stop() {
	s.cancel()
	s.flushCursors()
}

func (s *botSupervisor) connectAndServe() error {
	wsURL, err := toWebSocketURL(s.baseURL)
	if err != nil {
		return err
	}

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.botToken)

	conn, _, err := dialer.DialContext(s.ctx, wsURL, header)
	if err != nil {
		return fmt.Errorf("dial ws %s: %w", wsURL, err)
	}
	defer conn.Close()

	// Send authentication challenge
	authPayload, _ := json.Marshal(map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data": map[string]string{
			"token": s.botToken,
		},
	})
	if err := conn.WriteMessage(websocket.TextMessage, authPayload); err != nil {
		return fmt.Errorf("send auth challenge: %w", err)
	}

	s.connected.Store(true)
	log.Printf("[mattermost-bridge:%s] WebSocket connected to %s", s.botID, wsURL)

	// Step 1: Discover known channels (from user_channel_identities + users/me/channels)
	s.discoverChannels()

	// Step 2: Perform Catch-up sync on all known channels
	s.performCatchUp()

	// Step 3: Event read loop
	for s.ctx.Err() == nil {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read ws message: %w", err)
		}
		if msgType != websocket.TextMessage {
			continue
		}

		s.handleWebSocketMessage(data)
	}
	return nil
}

// discoverChannels discovers target channels to monitor/catch-up.
func (s *botSupervisor) discoverChannels() {
	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()

	// 1. Check user_channel_identities in controlDB for bound externalChatIDs
	if s.bridge.store != nil {
		identities, err := s.bridge.store.ListUserChannelIdentities(controldb.UserChannelIdentityFilter{
			WorkspaceID:      s.workspaceID,
			ChannelBindingID: s.binding.ID,
			Provider:         "mattermost",
		})
		if err == nil {
			for _, id := range identities {
				chatID := strings.TrimSpace(id.ExternalChatID)
				if chatID != "" {
					s.knownChannels[chatID] = true
				}
			}
		}
	}

	// 2. Best-effort query GET /api/v4/users/me/channels
	httpReq, err := http.NewRequestWithContext(s.ctx, http.MethodGet, s.baseURL+"/api/v4/users/me/channels", nil)
	if err == nil {
		httpReq.Header.Set("Authorization", "Bearer "+s.botToken)
		resp, err := s.bridge.httpClient.Do(httpReq)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var channels []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				}
				if json.NewDecoder(resp.Body).Decode(&channels) == nil {
					for _, ch := range channels {
						if strings.EqualFold(ch.Type, "D") || ch.Type == "" {
							s.knownChannels[ch.ID] = true
						}
					}
				}
			}
		}
	}
}

// performCatchUp pulls missed posts since the last recorded cursor.
func (s *botSupervisor) performCatchUp() {
	s.channelsMu.RLock()
	channels := make([]string, 0, len(s.knownChannels))
	for ch := range s.knownChannels {
		channels = append(channels, ch)
	}
	s.channelsMu.RUnlock()

	nowMs := time.Now().UnixMilli()
	maxWindowMs := int64(s.bridge.cfg.CatchUpMaxWindow / time.Millisecond)
	minAllowedCursor := nowMs - maxWindowMs

	for _, chID := range channels {
		cursor := s.getCursor(chID)
		if cursor <= 0 {
			// If no cursor recorded yet, initialize to now - 24h to avoid infinite replay
			cursor = minAllowedCursor
			s.setCursor(chID, cursor)
		} else if cursor < minAllowedCursor {
			log.Printf("[mattermost-bridge:%s] channel %s cursor %d is older than 24h window; capping catch-up", s.botID, chID, cursor)
			cursor = minAllowedCursor
		}

		s.catchUpChannel(chID, cursor)
	}
}

type postItem struct {
	ID        string `json:"id"`
	CreateAt  int64  `json:"create_at"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	RootID    string `json:"root_id"`
}

func (s *botSupervisor) catchUpChannel(channelID string, since int64) {
	reqURL := fmt.Sprintf("%s/api/v4/channels/%s/posts?since=%d", s.baseURL, channelID, since)
	httpReq, err := http.NewRequestWithContext(s.ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.botToken)

	resp, err := s.bridge.httpClient.Do(httpReq)
	if err != nil {
		log.Printf("[mattermost-bridge:%s] catch-up request failed for channel %s: %v", s.botID, channelID, err)
		s.bridge.errorsCount.Add(1)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[mattermost-bridge:%s] catch-up returned status %d for channel %s", s.botID, resp.StatusCode, channelID)
		s.bridge.errorsCount.Add(1)
		return
	}

	var data struct {
		Order []string            `json:"order"`
		Posts map[string]postItem `json:"posts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("[mattermost-bridge:%s] decode catch-up response failed: %v", s.botID, err)
		return
	}

	if len(data.Posts) == 0 {
		return
	}

	// Collect posts and sort chronologically ascending (oldest first)
	items := make([]postItem, 0, len(data.Posts))
	for _, p := range data.Posts {
		if p.ID != "" && p.UserID != s.botID {
			items = append(items, p)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreateAt < items[j].CreateAt
	})

	for _, item := range items {
		if s.seenPosts.containsOrAdd(item.ID) {
			continue
		}

		if err := s.forwardPost(item, "D"); err != nil {
			log.Printf("[mattermost-bridge:%s] forward catch-up post %s failed: %v", s.botID, item.ID, err)
			s.bridge.errorsCount.Add(1)
		} else {
			s.bridge.catchUpCount.Add(1)
			s.bridge.messagesForwarded.Add(1)
			s.advanceCursor(item.ChannelID, item.CreateAt)
		}
	}
}

func (s *botSupervisor) handleWebSocketMessage(data []byte) {
	var ev struct {
		Event string `json:"event"`
		Data  struct {
			ChannelID   string `json:"channel_id"`
			ChannelType string `json:"channel_type"`
			Post        string `json:"post"`
			SenderName  string `json:"sender_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}

	if ev.Event != "posted" || ev.Data.Post == "" {
		return
	}

	var post postItem
	if err := json.Unmarshal([]byte(ev.Data.Post), &post); err != nil {
		return
	}

	// Add channel to known channels
	s.channelsMu.Lock()
	s.knownChannels[post.ChannelID] = true
	s.channelsMu.Unlock()

	// Ignore messages from the bot itself
	if post.UserID == "" || post.UserID == s.botID {
		return
	}

	// Deduplicate against cache
	if s.seenPosts.containsOrAdd(post.ID) {
		return
	}

	channelType := ev.Data.ChannelType
	if channelType == "" {
		channelType = "D"
	}

	if err := s.forwardPost(post, channelType); err != nil {
		log.Printf("[mattermost-bridge:%s] forward ws post %s failed: %v", s.botID, post.ID, err)
		s.bridge.errorsCount.Add(1)
	} else {
		s.bridge.messagesForwarded.Add(1)
		s.advanceCursor(post.ChannelID, post.CreateAt)
	}
}

func (s *botSupervisor) forwardPost(post postItem, channelType string) error {
	postRaw, err := json.Marshal(post)
	if err != nil {
		return err
	}

	evPayload := map[string]any{
		"event": "posted",
		"data": map[string]any{
			"channel_id":   post.ChannelID,
			"channel_type": channelType,
			"post":         string(postRaw),
			"bot_id":       s.botID, // C3: inject bot_id for accurate AppID routing
		},
	}
	body, err := json.Marshal(evPayload)
	if err != nil {
		return err
	}

	endpoint := strings.TrimRight(s.bridge.cfg.MultigentURL, "/") + "/api/v1/im/mattermost/events"
	httpReq, err := http.NewRequestWithContext(s.ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Sign payload with HMAC if secret is configured
	if s.bridgeHmacSecret != "" {
		ts := time.Now().Unix()
		tsStr := strconv.FormatInt(ts, 10)
		mac := hmac.New(sha256.New, []byte(s.bridgeHmacSecret))
		mac.Write([]byte(tsStr + "."))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))

		httpReq.Header.Set("X-Mattermost-Forward-Timestamp", tsStr)
		httpReq.Header.Set("X-Mattermost-Forward-Signature", sig)
	}

	resp, err := s.bridge.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post forward event: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("multigent returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (s *botSupervisor) getCursor(channelID string) int64 {
	s.cursorsMu.Lock()
	defer s.cursorsMu.Unlock()
	if c, ok := s.cursors[channelID]; ok {
		return c
	}
	// Load from DB: kv_records (table="mattermost_cursors", workspace_id, k1=channel_id)
	if s.bridge.store != nil {
		val, ok, err := s.bridge.store.GetRecord("mattermost_cursors", s.workspaceID, []string{channelID})
		if err == nil && ok && val != "" {
			if ts, err := strconv.ParseInt(val, 10, 64); err == nil {
				s.cursors[channelID] = ts
				return ts
			}
		}
	}
	return 0
}

func (s *botSupervisor) setCursor(channelID string, cursor int64) {
	s.cursorsMu.Lock()
	defer s.cursorsMu.Unlock()
	s.cursors[channelID] = cursor
	s.cursorsDirty = true
}

func (s *botSupervisor) advanceCursor(channelID string, createAt int64) {
	s.cursorsMu.Lock()
	defer s.cursorsMu.Unlock()
	if createAt > s.cursors[channelID] {
		s.cursors[channelID] = createAt
		s.cursorsDirty = true
	}
}

func (s *botSupervisor) flushCursors() {
	s.cursorsMu.Lock()
	if !s.cursorsDirty || s.bridge.store == nil {
		s.cursorsMu.Unlock()
		return
	}
	snapshot := make(map[string]int64, len(s.cursors))
	for k, v := range s.cursors {
		snapshot[k] = v
	}
	s.cursorsDirty = false
	s.cursorsMu.Unlock()

	for chID, ts := range snapshot {
		if err := s.bridge.store.UpsertRecord("mattermost_cursors", s.workspaceID, []string{chID}, strconv.FormatInt(ts, 10)); err != nil {
			log.Printf("[mattermost-bridge:%s] upsert cursor failed for channel %s: %v", s.botID, chID, err)
		}
	}
}

func toWebSocketURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v4/websocket"
	return u.String(), nil
}

func openSecretValues(secret controldb.ConnectionSecret) (map[string]string, error) {
	return controldb.OpenConnectionSecret(secret)
}

// postIDCache is an LRU-like thread-safe cache for recently seen post IDs.
type postIDCache struct {
	maxSize int
	order   []string
	seen    map[string]struct{}
	mu      sync.Mutex
}

func newPostIDCache(maxSize int) *postIDCache {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &postIDCache{
		maxSize: maxSize,
		order:   make([]string, 0, maxSize),
		seen:    make(map[string]struct{}, maxSize),
	}
}

func (c *postIDCache) containsOrAdd(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[id]; ok {
		return true
	}
	if len(c.order) >= c.maxSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.seen, oldest)
	}
	c.order = append(c.order, id)
	c.seen[id] = struct{}{}
	return false
}
