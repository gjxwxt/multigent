package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controldb "github.com/multigent/multigent/internal/db"
)

func newConnectionTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	users := newTestUserStore(t)
	root := filepath.Join(t.TempDir(), "workspace")
	workspaceID := "ws-one"
	if err := users.db.UpsertWorkspace(controldb.Workspace{
		ID:        workspaceID,
		Name:      "One",
		Slug:      "one",
		Root:      root,
		CreatedAt: "2026-07-15T00:00:00Z",
	}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	for _, username := range []string{"owner", "other"} {
		if err := users.CreateUser(username, "pass123", RoleMember, "", "", "", "", ""); err != nil {
			t.Fatalf("create user %s: %v", username, err)
		}
		if err := users.db.UpsertWorkspaceMember(workspaceID, username, WorkspaceRoleMember); err != nil {
			t.Fatalf("workspace member %s: %v", username, err)
		}
	}
	return &Server{root: root, controlDB: users.db, users: users}, workspaceID
}

func TestConnectionTestCustomHTTPUsesServerSideCredential(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=secret")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": upstreamAuth,
			"ok":   true,
		})
	}))
	defer upstream.Close()

	connection := controldb.Connection{
		ID:             "conn-http",
		WorkspaceID:    workspaceID,
		Provider:       "custom-http",
		ConnectionName: "api",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthCustomCredential,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"baseUrl": upstream.URL, "apiKey": "test-token"})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/conn-http/test", strings.NewReader(`{"headers":{"Authorization":"Bearer attacker"}}`))
	req.SetPathValue("id", connection.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "owner"))
	rec := httptest.NewRecorder()

	s.handleTestConnection(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamAuth != "Bearer test-token" {
		t.Fatalf("upstream auth=%q", upstreamAuth)
	}
	body := rec.Body.String()
	if strings.Contains(body, "test-token") || strings.Contains(body, "Set-Cookie") || strings.Contains(body, "session=secret") {
		t.Fatalf("test response leaked sensitive data: %s", body)
	}
	if !strings.Contains(body, "Bearer [redacted]") {
		t.Fatalf("redacted marker missing: %s", body)
	}

	updated, found, err := s.controlDB.ConnectionByID(connection.ID)
	if err != nil || !found {
		t.Fatalf("get updated connection: found=%v err=%v", found, err)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(updated.ProfileJSON), &profile); err != nil {
		t.Fatalf("profile json: %v", err)
	}
	if profile["lastValidatedAt"] == "" {
		t.Fatalf("lastValidatedAt missing in profile: %#v", profile)
	}
	if profile["lastValidationOK"] != true {
		t.Fatalf("lastValidationOK=%#v profile=%#v", profile["lastValidationOK"], profile)
	}
	if profile["lastValidationStatus"] != float64(http.StatusOK) {
		t.Fatalf("lastValidationStatus=%#v profile=%#v", profile["lastValidationStatus"], profile)
	}
	if profile["lastValidationMessage"] != "Connection test succeeded" {
		t.Fatalf("lastValidationMessage=%#v profile=%#v", profile["lastValidationMessage"], profile)
	}
}

func TestConnectionTestRequiresManagementAccess(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-owner",
		WorkspaceID:    workspaceID,
		Provider:       "custom-http",
		ConnectionName: "api",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthCustomCredential,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}); err != nil {
		t.Fatalf("connection: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/conn-owner/test", nil)
	req.SetPathValue("id", "conn-owner")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "other"))
	rec := httptest.NewRecorder()

	s.handleTestConnection(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConnectionTestPersistsFailedHTTPValidation(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	connection := controldb.Connection{
		ID:             "conn-http-fail",
		WorkspaceID:    workspaceID,
		Provider:       "custom-http",
		ConnectionName: "api",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthCustomCredential,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"baseUrl": upstream.URL, "apiKey": "test-token"})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/conn-http-fail/test", nil)
	req.SetPathValue("id", connection.ID)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "owner"))
	rec := httptest.NewRecorder()

	s.handleTestConnection(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result testConnectionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if result.OK || result.Status != http.StatusServiceUnavailable {
		t.Fatalf("unexpected result: %#v", result)
	}

	updated, found, err := s.controlDB.ConnectionByID(connection.ID)
	if err != nil || !found {
		t.Fatalf("get updated connection: found=%v err=%v", found, err)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(updated.ProfileJSON), &profile); err != nil {
		t.Fatalf("profile json: %v", err)
	}
	if profile["lastValidatedAt"] == "" {
		t.Fatalf("lastValidatedAt missing in profile: %#v", profile)
	}
	if profile["lastValidationOK"] != false {
		t.Fatalf("lastValidationOK=%#v profile=%#v", profile["lastValidationOK"], profile)
	}
	if profile["lastValidationStatus"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("lastValidationStatus=%#v profile=%#v", profile["lastValidationStatus"], profile)
	}
	if !strings.Contains(profile["lastValidationMessage"].(string), "HTTP 503") {
		t.Fatalf("lastValidationMessage=%#v profile=%#v", profile["lastValidationMessage"], profile)
	}
}

func TestConnectionTestEnforcesConnectionActionPolicy(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer upstream.Close()

	connection := controldb.Connection{
		ID:             "conn-policy-test",
		WorkspaceID:    workspaceID,
		Provider:       "custom-http",
		ConnectionName: "api",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthCustomCredential,
		Status:         "active",
		ProfileJSON:    `{"allowedActionMethods":["GET"],"allowedActionEndpoints":["/safe/*"]}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"baseUrl": upstream.URL, "apiKey": "test-token"})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	blockedReq := httptest.NewRequest(http.MethodPost, "/api/v1/connections/conn-policy-test/test", strings.NewReader(`{"method":"POST","endpoint":"/safe/items","body":{"ok":true}}`))
	blockedReq.SetPathValue("id", connection.ID)
	blockedReq.Header.Set("Content-Type", "application/json")
	blockedReq = blockedReq.WithContext(context.WithValue(blockedReq.Context(), ctxUserKey, "owner"))
	blockedRec := httptest.NewRecorder()
	s.handleTestConnection(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusBadRequest {
		t.Fatalf("blocked status=%d body=%s", blockedRec.Code, blockedRec.Body.String())
	}

	allowedReq := httptest.NewRequest(http.MethodPost, "/api/v1/connections/conn-policy-test/test", strings.NewReader(`{"method":"GET","endpoint":"/safe/items"}`))
	allowedReq.SetPathValue("id", connection.ID)
	allowedReq.Header.Set("Content-Type", "application/json")
	allowedReq = allowedReq.WithContext(context.WithValue(allowedReq.Context(), ctxUserKey, "owner"))
	allowedRec := httptest.NewRecorder()
	s.handleTestConnection(allowedRec, allowedReq)
	if allowedRec.Code != http.StatusOK {
		t.Fatalf("allowed status=%d body=%s", allowedRec.Code, allowedRec.Body.String())
	}
	if upstreamHits != 1 {
		t.Fatalf("upstream hits=%d", upstreamHits)
	}
}

func TestConnectionHealthCheckRunsOnlyEnabledDueConnections(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer upstream.Close()

	dueProfile := `{"healthCheckEnabled":true,"healthCheckIntervalMinutes":5,"nextHealthCheckAt":"2026-07-14T00:00:00Z"}`
	disabledProfile := `{"healthCheckEnabled":false,"nextHealthCheckAt":"2026-07-14T00:00:00Z"}`
	futureProfile := `{"healthCheckEnabled":true,"healthCheckIntervalMinutes":5,"nextHealthCheckAt":"2999-01-01T00:00:00Z"}`
	for _, tc := range []struct {
		id      string
		profile string
	}{
		{"conn-due", dueProfile},
		{"conn-disabled", disabledProfile},
		{"conn-future", futureProfile},
	} {
		connection := controldb.Connection{
			ID:             tc.id,
			WorkspaceID:    workspaceID,
			Provider:       "custom-http",
			ConnectionName: tc.id,
			OwnerType:      ConnectionOwnerUser,
			OwnerID:        "owner",
			AuthType:       ConnectionAuthCustomCredential,
			Status:         "active",
			ProfileJSON:    tc.profile,
			CreatedBy:      "owner",
			CreatedAt:      "2026-07-15T00:00:00Z",
			UpdatedAt:      "2026-07-15T00:00:00Z",
		}
		if err := s.controlDB.UpsertConnection(connection); err != nil {
			t.Fatalf("connection %s: %v", tc.id, err)
		}
		secret, err := sealConnectionSecret(map[string]string{"baseUrl": upstream.URL, "apiKey": "token-" + tc.id})
		if err != nil {
			t.Fatalf("seal secret %s: %v", tc.id, err)
		}
		secret.ConnectionID = tc.id
		if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
			t.Fatalf("secret %s: %v", tc.id, err)
		}
	}

	resp := s.runConnectionHealthChecks(context.Background(), connectionHealthCheckOptions{WorkspaceID: workspaceID, Limit: 10})
	if resp.Checked != 1 || resp.Skipped != 2 {
		t.Fatalf("health response=%#v", resp)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d", hits)
	}
	updated, found, err := s.controlDB.ConnectionByID("conn-due")
	if err != nil || !found {
		t.Fatalf("updated connection found=%v err=%v", found, err)
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(updated.ProfileJSON), &profile); err != nil {
		t.Fatalf("profile json: %v", err)
	}
	if profile["lastHealthCheckAt"] == "" || profile["nextHealthCheckAt"] == "" {
		t.Fatalf("health timestamps missing: %#v", profile)
	}
	if profile["lastValidationOK"] != true {
		t.Fatalf("lastValidationOK=%#v profile=%#v", profile["lastValidationOK"], profile)
	}
}

func TestRunConnectionHealthChecksRequiresWorkspaceAdmin(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	if err := s.controlDB.UpsertWorkspaceMember(workspaceID, "owner", WorkspaceRoleAdmin); err != nil {
		t.Fatalf("promote owner: %v", err)
	}
	if err := s.controlDB.UpsertConnection(controldb.Connection{
		ID:             "conn-health",
		WorkspaceID:    workspaceID,
		Provider:       "custom-http",
		ConnectionName: "api",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthCustomCredential,
		Status:         "active",
		ProfileJSON:    `{"healthCheckEnabled":true}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}); err != nil {
		t.Fatalf("connection: %v", err)
	}

	memberReq := providerTestRequest(http.MethodPost, "/api/v1/connections/health-check", "other", runConnectionHealthChecksRequest{Force: true})
	memberRec := httptest.NewRecorder()
	s.handleRunConnectionHealthChecks(memberRec, memberReq)
	if memberRec.Code != http.StatusForbidden {
		t.Fatalf("member status=%d body=%s", memberRec.Code, memberRec.Body.String())
	}

	adminReq := providerTestRequest(http.MethodPost, "/api/v1/connections/health-check", "owner", runConnectionHealthChecksRequest{Force: true, Limit: 5})
	adminRec := httptest.NewRecorder()
	s.handleRunConnectionHealthChecks(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", adminRec.Code, adminRec.Body.String())
	}
}

func TestConnectionHealthIntervalIsClamped(t *testing.T) {
	if got := healthConnectionInterval(map[string]any{"healthCheckIntervalMinutes": float64(1)}); got != minConnectionHealthCheckInterval {
		t.Fatalf("min clamp=%s", got)
	}
	if got := healthConnectionInterval(map[string]any{"healthCheckIntervalMinutes": float64(60 * 24 * 40)}); got != maxConnectionHealthCheckInterval {
		t.Fatalf("max clamp=%s", got)
	}
	if got := healthConnectionInterval(map[string]any{"healthCheckIntervalMinutes": float64(30)}); got != 30*time.Minute {
		t.Fatalf("interval=%s", got)
	}
}

func TestDingTalkBotConnectionTestUsesScopedWebhookEndpoint(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	connection := controldb.Connection{
		ID:             "conn-dingtalk",
		WorkspaceID:    workspaceID,
		Provider:       "dingtalk_bot",
		ConnectionName: "alerts",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"apiKey": "ding-token"})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	actionReq := runtimeActionProxyRequest{}
	applyDefaultConnectionTestRequest(connection.Provider, &actionReq)
	if actionReq.Endpoint != "/robot/send" || actionReq.Method != http.MethodPost || !strings.Contains(string(actionReq.Body), "Multigent connection test") {
		t.Fatalf("default DingTalk test request=%#v body=%s", actionReq, string(actionReq.Body))
	}

	_, err = s.testHTTPConnection(httptest.NewRequest(http.MethodPost, "/", nil), connection, testConnectionRequest{
		Endpoint: "/not-supported",
		Method:   http.MethodPost,
		Body:     json.RawMessage(`{"msgtype":"text","text":{"content":"test"}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "only supports /robot/send") {
		t.Fatalf("expected unsupported endpoint error, got %v", err)
	}
}

func TestCloudflareConnectionTestUsesTokenVerifyEndpoint(t *testing.T) {
	actionReq := runtimeActionProxyRequest{}
	applyDefaultConnectionTestRequest("cloudflare", &actionReq)
	if actionReq.Endpoint != "/user/tokens/verify" || actionReq.Method != http.MethodGet {
		t.Fatalf("default Cloudflare test request=%#v", actionReq)
	}
}

func TestGitLabReadOnlyConnectionTestChecksRepositoryOverGitHTTP(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	const secretToken = "test-project-token-do-not-return"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%q, want GET", r.Method)
		}
		if r.URL.Path != "/root/smoke-s2d4-mirror.git/info/refs" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.URL.Query().Get("service"); got != "git-upload-pack" {
			t.Errorf("service=%q", got)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "oauth2" || password != secretToken {
			t.Errorf("unexpected basic auth username=%q present=%v", username, ok)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte("# service=git-upload-pack\n0000"))
	}))
	defer upstream.Close()

	connection := controldb.Connection{
		ID:             "conn-gitlab-readonly",
		WorkspaceID:    workspaceID,
		Provider:       "gitlab",
		ConnectionName: "smoke-qa-readonly",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{"repositoryPath":"root/smoke-s2d4-mirror"}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"baseUrl": upstream.URL, "apiKey": secretToken})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/conn-gitlab-readonly/test", nil)
	req.SetPathValue("id", connection.ID)
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "owner"))
	rec := httptest.NewRecorder()
	s.handleTestConnection(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result testConnectionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if !result.OK || result.Status != http.StatusOK {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(rec.Body.String(), secretToken) {
		t.Fatalf("test response leaked credential: %s", rec.Body.String())
	}
}

func TestGitLabReadOnlyConnectionTestExplainsUserAPI403(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			t.Errorf("path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"insufficient_scope"}`))
	}))
	defer upstream.Close()

	connection := controldb.Connection{
		ID:             "conn-gitlab-no-path",
		WorkspaceID:    workspaceID,
		Provider:       "gitlab",
		ConnectionName: "smoke-qa-readonly",
		OwnerType:      ConnectionOwnerWorkspace,
		OwnerID:        workspaceID,
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{"baseUrl": upstream.URL, "apiKey": "test-token"})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	result, err := s.testConnection(httptest.NewRequest(http.MethodPost, "/test", nil), connection, testConnectionRequest{})
	if err != nil {
		t.Fatalf("test connection: %v", err)
	}
	if result.Status != http.StatusForbidden || !strings.Contains(result.Message, "read_repository-only token can read repositories but cannot access /user") || !strings.Contains(result.Message, "repositoryPath") {
		t.Fatalf("unexpected diagnostic: %#v", result)
	}
}

func TestGitLabRepositoryPathProfileUpdatePreservesCredential(t *testing.T) {
	s, workspaceID := newConnectionTestServer(t)
	connection := controldb.Connection{
		ID:             "conn-gitlab-readonly-profile",
		WorkspaceID:    workspaceID,
		Provider:       "gitlab",
		ConnectionName: "smoke-qa-readonly",
		OwnerType:      ConnectionOwnerUser,
		OwnerID:        "owner",
		AuthType:       ConnectionAuthAPIKey,
		Status:         "active",
		ProfileJSON:    `{"baseUrl":"http://gitlab.example.test"}`,
		CreatedBy:      "owner",
		CreatedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:00Z",
	}
	if err := s.controlDB.UpsertConnection(connection); err != nil {
		t.Fatalf("connection: %v", err)
	}
	secret, err := sealConnectionSecret(map[string]string{
		"baseUrl": "http://gitlab.example.test",
		"apiKey":  "credential-must-stay-server-side",
	})
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	secret.ConnectionID = connection.ID
	if err := s.controlDB.UpsertConnectionSecret(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/connections/conn-gitlab-readonly-profile", strings.NewReader(`{"profile":{"repositoryPath":"root/smoke-s2d4-mirror"}}`))
	req.SetPathValue("id", connection.ID)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, "owner"))
	rec := httptest.NewRecorder()
	s.handleUpdateConnection(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "credential-must-stay-server-side") {
		t.Fatalf("update response leaked credential: %s", rec.Body.String())
	}
	updated, found, err := s.controlDB.ConnectionByID(connection.ID)
	if err != nil || !found {
		t.Fatalf("updated connection found=%v err=%v", found, err)
	}
	profile := connectionProfileMap(updated)
	if profile["repositoryPath"] != "root/smoke-s2d4-mirror" {
		t.Fatalf("repositoryPath=%#v", profile["repositoryPath"])
	}
	updatedSecret, found, err := s.controlDB.ConnectionSecret(connection.ID)
	if err != nil || !found {
		t.Fatalf("updated secret found=%v err=%v", found, err)
	}
	values, err := openConnectionSecret(updatedSecret)
	if err != nil {
		t.Fatalf("open secret: %v", err)
	}
	if values["apiKey"] != "credential-must-stay-server-side" || values["baseUrl"] != "http://gitlab.example.test" {
		t.Fatalf("profile-only update changed stored credential values")
	}
}

func TestNormalizeGitLabRepositoryPath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "nested group", input: "/root/subgroup/project/", want: "root/subgroup/project"},
		{name: "git suffix", input: "root/project.git", want: "root/project"},
		{name: "missing project", input: "root", wantErr: true},
		{name: "path traversal", input: "root/../project", wantErr: true},
		{name: "query injection", input: "root/project?service=bad", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeGitLabRepositoryPath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize path: %v", err)
			}
			if got != tt.want {
				t.Fatalf("path=%q, want %q", got, tt.want)
			}
		})
	}
}
