package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
	"github.com/multigent/multigent/internal/imbridge"
)

// newProvisionFixServer returns a server plus a Mattermost mock that records
// every channel-member invite (user_id set).
var provisionFixMockURL string

func newProvisionFixServer(t *testing.T) (*Server, string, *map[string]bool) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	invited := make(map[string]bool)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mm-fix-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]imbridge.MattermostTeam{{ID: "team-fix-1", Name: "team-main", DisplayName: "Main"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/channels":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(imbridge.MattermostChannel{ID: "chan-fix-1", Name: "proj-fixproj", DisplayName: "#proj-fixproj", Type: "P"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/members"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			invited[body["user_id"]] = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	provisionFixMockURL = mock.URL
	return s, workspaceID, &invited
}

func upsertFixConnection(t *testing.T, s *Server, workspaceID, connID, name, botID, instanceID string) {
	t.Helper()
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             connID,
		WorkspaceID:    workspaceID,
		Provider:       "mattermost",
		ConnectionName: name,
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       "bot_token",
		Status:         "active",
		ProfileJSON:    `{"botId":"` + botID + `","baseUrl":"` + provisionFixMockURL + `"}`,
		IMInstanceID:   instanceID,
	}); err != nil {
		t.Fatalf("upsert connection %s: %v", connID, err)
	}
	secret, err := sealConnectionSecret(map[string]string{"baseUrl": provisionFixMockURL, "botToken": "mm-fix-token", "appId": botID, "bridgeHmacSecret": "hmac-" + botID})
	if err != nil {
		t.Fatalf("seal secret %s: %v", connID, err)
	}
	secret.ConnectionID = connID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("upsert secret %s: %v", connID, err)
	}
}

func provisionFixProject(t *testing.T, s *Server, workspaceID, projectName string, workerIDs []string) *projectChannelProvisionResponse {
	t.Helper()
	if err := s.st.SaveProject(projectName, &entity.Project{Name: projectName}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	reqBody := projectChannelProvisionRequest{
		Provider:   "mattermost",
		Mode:       "create",
		Visibility: "private",
		WorkerIDs:  workerIDs,
	}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+projectName+"/channels/provision", "admin", reqBody)
	req.SetPathValue("name", projectName)
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp projectChannelProvisionResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &resp
}

// TestConnectionMatchesAgentHyphenNames pins FIX 1: connection names built as
// "agent-<project>-<Agent Name>" (hyphens) must match agent display names
// ("FP Dev A", spaces) after token normalization.
func TestConnectionMatchesAgentHyphenNames(t *testing.T) {
	conn := controldb.Connection{ConnectionName: "agent-fp-dedupe-FP-Dev-A"}
	if !connectionMatchesAgent(conn, "FP Dev A") {
		t.Fatal("expected hyphenated connection name to match spaced agent name")
	}
	conn2 := controldb.Connection{ConnectionName: "FP Dev B"}
	if !connectionMatchesAgent(conn2, "FP Dev B") {
		t.Fatal("expected exact-name match to hold")
	}
	if connectionMatchesAgent(controldb.Connection{ConnectionName: "agent-other-Alex"}, "FP Dev A") {
		t.Fatal("unrelated connection must not match")
	}
}

// TestProvisionBindsEachAgentToItsOwnBot pins FIX 1 end-to-end: with 4 agents
// and 4 auto-named connections, each binding must reference its own bot (no
// alphabetical cross-assignment).
func TestProvisionBindsEachAgentToItsOwnBot(t *testing.T) {
	s, workspaceID, _ := newProvisionFixServer(t)
	projectName := "fixproj"

	type aw struct{ id, name string }
	agents := []aw{{"aw-fa", "FP Dev A"}, {"aw-fb", "FP Dev B"}, {"aw-fc", "FP Dev C"}, {"aw-fq", "FP QA"}}
	for _, a := range agents {
		if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{ID: a.id, WorkspaceID: workspaceID, Name: a.name, Status: "active"}); err != nil {
			t.Fatalf("upsert worker %s: %v", a.name, err)
		}
	}
	// Connection names mimic the real auto-naming: agent-<project>-<Agent Name with hyphens>
	upsertFixConnection(t, s, workspaceID, "conn-fa", "agent-fixproj-FP-Dev-A", "bot-user-a", "inst-fix-1")
	upsertFixConnection(t, s, workspaceID, "conn-fb", "agent-fixproj-FP-Dev-B", "bot-user-b", "inst-fix-1")
	upsertFixConnection(t, s, workspaceID, "conn-fc", "agent-fixproj-FP-Dev-C", "bot-user-c", "inst-fix-1")
	upsertFixConnection(t, s, workspaceID, "conn-fq", "agent-fixproj-FP-QA", "bot-user-q", "inst-fix-1")

	resp := provisionFixProject(t, s, workspaceID, projectName, []string{"aw-fa", "aw-fb", "aw-fc", "aw-fq"})
	if len(resp.FailedAgents) != 0 || len(resp.SkippedAgents) != 0 {
		t.Fatalf("unexpected failures: failed=%v skipped=%v", resp.FailedAgents, resp.SkippedAgents)
	}

	bindings, err := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{WorkspaceID: workspaceID, ProjectID: projectName, Provider: "mattermost", Status: "connected"})
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	got := map[string]string{}
	for _, b := range bindings {
		got[b.AgentID] = b.ExternalBotID
	}
	want := map[string]string{"FP Dev A": "bot-user-a", "FP Dev B": "bot-user-b", "FP Dev C": "bot-user-c", "FP QA": "bot-user-q"}
	for agent, bot := range want {
		if got[agent] != bot {
			t.Fatalf("agent %s bound to %q, want %q (all: %v)", agent, got[agent], bot, got)
		}
	}
}

// TestProvisionRefusesCrossedBotReuse pins FIX 2: when the only available bot
// is already bound to another (project, agent), provision must SKIP the new
// binding instead of silently reusing the bot (DEFECT-C3 guard).
func TestProvisionRefusesCrossedBotReuse(t *testing.T) {
	s, workspaceID, _ := newProvisionFixServer(t)
	if err := s.controlDB.UpsertAgentWorker(controldb.AgentWorker{ID: "aw-fa", WorkspaceID: workspaceID, Name: "FP Dev A", Status: "active"}); err != nil {
		t.Fatalf("upsert worker: %v", err)
	}
	upsertFixConnection(t, s, workspaceID, "conn-fa", "agent-fixproj-FP-Dev-A", "bot-user-a", "inst-fix-1")
	// Pre-bind bot-user-a to another project via a connected binding.
	if err := s.controlDB.UpsertAgentChannelBinding(controldb.AgentChannelBinding{
		ID: "chan-preexisting", WorkspaceID: workspaceID, ProjectID: "otherproj", AgentID: "Someone",
		Provider: "mattermost", ConnectionID: "conn-fa", ExternalBotID: "bot-user-a", Status: "connected",
	}); err != nil {
		t.Fatalf("preexisting binding: %v", err)
	}

	if err := s.st.SaveProject("fixproj2", &entity.Project{Name: "fixproj2"}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	reqBody := projectChannelProvisionRequest{Provider: "mattermost", Mode: "create", Visibility: "private", WorkerIDs: []string{"aw-fa"}}
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/fixproj2/channels/provision", "admin", reqBody)
	req.SetPathValue("name", "fixproj2")
	rr := httptest.NewRecorder()
	s.handleProvisionProjectChannel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp projectChannelProvisionResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	bindings, _ := s.controlDB.ListAgentChannelBindings(controldb.AgentChannelBindingFilter{WorkspaceID: workspaceID, ProjectID: "fixproj2", Provider: "mattermost", Status: "connected"})
	for _, b := range bindings {
		if b.ExternalBotID == "bot-user-a" {
			t.Fatal("bot already bound to another project must not be re-bound by provision")
		}
	}
	found := false
	for _, sk := range resp.SkippedAgents {
		if sk == "FP Dev A" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FP Dev A in skippedAgents, got %v", resp.SkippedAgents)
	}
}
