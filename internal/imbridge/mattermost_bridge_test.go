package imbridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

func TestPostIDCache(t *testing.T) {
	c := newPostIDCache(3)

	if c.containsOrAdd("") {
		t.Fatal("empty id should return false")
	}

	if c.containsOrAdd("post-1") {
		t.Fatal("first insert should return false (not seen)")
	}
	if !c.containsOrAdd("post-1") {
		t.Fatal("second check should return true (seen)")
	}

	if c.containsOrAdd("post-2") {
		t.Fatal("first insert post-2 should return false")
	}
	if c.containsOrAdd("post-3") {
		t.Fatal("first insert post-3 should return false")
	}

	// Cache now has [post-1, post-2, post-3]
	// Adding post-4 should evict post-1
	if c.containsOrAdd("post-4") {
		t.Fatal("first insert post-4 should return false")
	}
	if c.containsOrAdd("post-1") {
		t.Fatal("post-1 should have been evicted, so should return false")
	}
	if !c.containsOrAdd("post-4") {
		t.Fatal("post-4 should be present")
	}
}

func TestToWebSocketURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"http://127.0.0.1:8065", "ws://127.0.0.1:8065/api/v4/websocket"},
		{"http://127.0.0.1:8065/", "ws://127.0.0.1:8065/api/v4/websocket"},
		{"https://mattermost.example.com", "wss://mattermost.example.com/api/v4/websocket"},
		{"https://mattermost.example.com/sub", "wss://mattermost.example.com/sub/api/v4/websocket"},
	}
	for _, tc := range cases {
		got, err := toWebSocketURL(tc.input)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("for %s got %s, want %s", tc.input, got, tc.want)
		}
	}
}

func TestCatchUpChronologicalSortingAndHMAC(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	store, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var forwardedPosts []postItem
	var forwardedHeaders []http.Header
	var mu sync.Mutex

	// Mock Multigent Server
	multigentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		forwardedHeaders = append(forwardedHeaders, r.Header.Clone())

		// Verify HMAC signature
		sig := r.Header.Get("X-Mattermost-Forward-Signature")
		tsStr := r.Header.Get("X-Mattermost-Forward-Timestamp")
		mac := hmac.New(sha256.New, []byte("test-secret"))
		mac.Write([]byte(tsStr + "."))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if sig != expected {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		var ev struct {
			Event string `json:"event"`
			Data  struct {
				Post  string `json:"post"`
				BotID string `json:"bot_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var p postItem
		_ = json.Unmarshal([]byte(ev.Data.Post), &p)
		forwardedPosts = append(forwardedPosts, p)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer multigentSrv.Close()

	// Mock Mattermost Server
	mmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/posts") {
			sinceStr := r.URL.Query().Get("since")
			since, _ := strconv.ParseInt(sinceStr, 10, 64)

			// Return 3 posts:
			// post-3 (newest, create_at 3000)
			// post-1 (oldest, create_at 1000)
			// post-2 (middle, create_at 2000)
			// post-bot (authored by bot, should be skipped)
			resp := map[string]any{
				"order": []string{"post-3", "post-2", "post-bot", "post-1"},
				"posts": map[string]any{
					"post-3": postItem{ID: "post-3", CreateAt: 3000, ChannelID: "ch-1", UserID: "user-1", Message: "msg-3"},
					"post-1": postItem{ID: "post-1", CreateAt: 1000, ChannelID: "ch-1", UserID: "user-1", Message: "msg-1"},
					"post-2": postItem{ID: "post-2", CreateAt: 2000, ChannelID: "ch-1", UserID: "user-1", Message: "msg-2"},
					"post-bot": postItem{ID: "post-bot", CreateAt: 2500, ChannelID: "ch-1", UserID: "bot-1", Message: "msg-bot"},
				},
			}
			if since >= 3000 {
				resp = map[string]any{"order": []string{}, "posts": map[string]any{}}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mmSrv.Close()

	bridge := NewMattermostBridge(MattermostBridgeConfig{
		MultigentURL: multigentSrv.URL,
	}, store)

	sup := newBotSupervisor(
		bridge,
		controldb.AgentChannelBinding{ID: "bind-1", WorkspaceID: "ws-test"},
		mmSrv.URL,
		"bot-token",
		"test-secret",
		"bot-1",
		context.Background(),
		func() {},
	)

	// Catch-up channel
	sup.catchUpChannel("ch-1", 500)

	mu.Lock()
	defer mu.Unlock()

	// Verify exactly 3 posts forwarded (bot-bot was skipped)
	if len(forwardedPosts) != 3 {
		t.Fatalf("expected 3 forwarded posts, got %d", len(forwardedPosts))
	}

	// Verify strictly ascending chronological order: post-1 (1000), post-2 (2000), post-3 (3000)
	if forwardedPosts[0].ID != "post-1" || forwardedPosts[1].ID != "post-2" || forwardedPosts[2].ID != "post-3" {
		t.Fatalf("posts not in chronological ascending order: %#v", forwardedPosts)
	}

	// Verify cursor was advanced to 3000
	if sup.getCursor("ch-1") != 3000 {
		t.Fatalf("expected cursor 3000, got %d", sup.getCursor("ch-1"))
	}
}

func TestCursorDBPersistenceAndRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	store, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.UpsertWorkspace(controldb.Workspace{ID: "ws-a", Name: "Workspace A", Slug: "ws-a", Root: "/tmp/ws-a"}); err != nil {
		t.Fatalf("upsert ws-a: %v", err)
	}
	if err := store.UpsertWorkspace(controldb.Workspace{ID: "ws-b", Name: "Workspace B", Slug: "ws-b", Root: "/tmp/ws-b"}); err != nil {
		t.Fatalf("upsert ws-b: %v", err)
	}

	bridge := NewMattermostBridge(MattermostBridgeConfig{}, store)

	// Workspace A supervisor
	supA := newBotSupervisor(
		bridge,
		controldb.AgentChannelBinding{ID: "bind-a", WorkspaceID: "ws-a"},
		"http://127.0.0.1:8065",
		"tok-a",
		"sec-a",
		"bot-a",
		context.Background(),
		func() {},
	)

	// Workspace B supervisor (same channel ID "ch-shared", different workspace)
	supB := newBotSupervisor(
		bridge,
		controldb.AgentChannelBinding{ID: "bind-b", WorkspaceID: "ws-b"},
		"http://127.0.0.1:8065",
		"tok-b",
		"sec-b",
		"bot-b",
		context.Background(),
		func() {},
	)

	supA.advanceCursor("ch-shared", 123456)
	supB.advanceCursor("ch-shared", 654321)

	// Flush cursors to DB
	supA.flushCursors()
	supB.flushCursors()

	// Simulate bridge restart: create new supervisors and load cursors from DB
	restartedSupA := newBotSupervisor(
		bridge,
		controldb.AgentChannelBinding{ID: "bind-a", WorkspaceID: "ws-a"},
		"http://127.0.0.1:8065",
		"tok-a",
		"sec-a",
		"bot-a",
		context.Background(),
		func() {},
	)
	restartedSupB := newBotSupervisor(
		bridge,
		controldb.AgentChannelBinding{ID: "bind-b", WorkspaceID: "ws-b"},
		"http://127.0.0.1:8065",
		"tok-b",
		"sec-b",
		"bot-b",
		context.Background(),
		func() {},
	)

	curA := restartedSupA.getCursor("ch-shared")
	curB := restartedSupB.getCursor("ch-shared")

	if curA != 123456 {
		t.Fatalf("expected Workspace A cursor 123456, got %d", curA)
	}
	if curB != 654321 {
		t.Fatalf("expected Workspace B cursor 654321, got %d", curB)
	}
}

func TestCatchUpMaxWindowCapping(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	store, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var requestedSince int64
	var requestedMu sync.Mutex

	mmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedMu.Lock()
		sinceStr := r.URL.Query().Get("since")
		requestedSince, _ = strconv.ParseInt(sinceStr, 10, 64)
		requestedMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"order": []string{}, "posts": map[string]any{}})
	}))
	defer mmSrv.Close()

	bridge := NewMattermostBridge(MattermostBridgeConfig{
		CatchUpMaxWindow: 12 * time.Hour,
	}, store)

	sup := newBotSupervisor(
		bridge,
		controldb.AgentChannelBinding{ID: "bind-1", WorkspaceID: "ws-test"},
		mmSrv.URL,
		"bot-tok",
		"sec",
		"bot-1",
		context.Background(),
		func() {},
	)

	// Set cursor to 48 hours ago (exceeding 12h window)
	fortyEightHoursAgo := time.Now().Add(-48 * time.Hour).UnixMilli()
	sup.setCursor("ch-1", fortyEightHoursAgo)

	// Register channel
	sup.channelsMu.Lock()
	sup.knownChannels["ch-1"] = true
	sup.channelsMu.Unlock()

	sup.performCatchUp()

	requestedMu.Lock()
	defer requestedMu.Unlock()

	expectedMinSince := time.Now().Add(-13 * time.Hour).UnixMilli()
	expectedMaxSince := time.Now().Add(-11 * time.Hour).UnixMilli()

	if requestedSince < expectedMinSince || requestedSince > expectedMaxSince {
		t.Fatalf("expected capped since around 12h ago (%d..%d), got %d", expectedMinSince, expectedMaxSince, requestedSince)
	}
}

func TestRefreshSupervisorsTracksMissingHMACSecret(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_hmac_check.db")
	store, err := controldb.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.UpsertWorkspace(controldb.Workspace{ID: "ws-1", Name: "WS 1", Slug: "ws-1", Root: "/tmp/ws-1"}); err != nil {
		t.Fatal(err)
	}
	sec, err := controldb.SealConnectionSecret(map[string]string{
		"baseUrl":          "http://127.0.0.1:8065",
		"botToken":         "tok-1",
		"bridgeHmacSecret": "", // MISSING
		"appId":            "bot-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	sec.ConnectionID = "conn-1"
	if err := store.UpsertConnection(controldb.Connection{
		ID:             "conn-1",
		WorkspaceID:    "ws-1",
		Provider:       "mattermost",
		ConnectionName: "conn-1",
		OwnerType:      "workspace",
		OwnerID:        "ws-1",
		Status:         "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConnectionSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID:           "bind-missing-hmac",
		WorkspaceID:  "ws-1",
		Provider:     "mattermost",
		ConnectionID: "conn-1",
		Status:       "connected",
	}); err != nil {
		t.Fatal(err)
	}

	bridge := NewMattermostBridge(MattermostBridgeConfig{}, store)
	bridge.refreshSupervisors(context.Background())

	bridge.mu.Lock()
	defer bridge.mu.Unlock()

	reason, ok := bridge.unconfiguredBots["bind-missing-hmac"]
	if !ok || reason != "missing_hmac_secret" {
		t.Fatalf("expected unconfigured reason 'missing_hmac_secret', got %q", reason)
	}
}

type panicMockStore struct {
	controldb.Store
}

func (p panicMockStore) ListAgentChannelBindings(filter controldb.AgentChannelBindingFilter) ([]controldb.AgentChannelBinding, error) {
	panic("simulated modernc.org/sqlite nil pointer dereference")
}

func TestMattermostBridge_PanicRecoveryAndStatus(t *testing.T) {
	bridge := NewMattermostBridge(MattermostBridgeConfig{}, panicMockStore{})

	// Calling refreshSupervisors should NOT panic the caller
	bridge.refreshSupervisors(context.Background())

	if bridge.panicCount.Load() != 1 {
		t.Fatalf("expected panicCount 1, got %d", bridge.panicCount.Load())
	}
	if bridge.errorsCount.Load() != 1 {
		t.Fatalf("expected errorsCount 1, got %d", bridge.errorsCount.Load())
	}
	if !strings.Contains(bridge.lastPanic, "simulated modernc.org/sqlite") {
		t.Fatalf("expected lastPanic to contain simulated error, got %q", bridge.lastPanic)
	}
	if bridge.lastPanicAt == "" {
		t.Fatalf("expected lastPanicAt to be set")
	}

	// Also verify flushAllCursors handles panics cleanly
	bridge.flushAllCursors()
	// No panic, cursors flushed
}


