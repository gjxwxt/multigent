package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

// ---- parseDeployVerifySpec (untrusted model output validation) ----

func TestParseDeployVerifySpecAcceptsValidSpec(t *testing.T) {
	raw := `{"checks":[
		{"name":"健康","method":"GET","path":"/api/health","expectedStatus":200},
		{"name":"首页","method":"GET","path":"/","expectedStatus":200,"contains":"<title"}
	]}`
	spec, err := parseDeployVerifySpec(raw)
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	if len(spec.Checks) != 2 || spec.Checks[0].Path != "/api/health" || spec.Checks[1].Contains != "<title" {
		t.Fatalf("spec fields wrong: %+v", spec)
	}
}

func TestParseDeployVerifySpecToleratesFence(t *testing.T) {
	raw := "```json\n{\"checks\":[{\"path\":\"/\",\"expectedStatus\":200}]}\n```"
	spec, err := parseDeployVerifySpec(raw)
	if err != nil {
		t.Fatalf("fenced spec rejected: %v", err)
	}
	if len(spec.Checks) != 1 || spec.Checks[0].Method != "GET" || spec.Checks[0].Name != "check-1" {
		t.Fatalf("defaults not applied: %+v", spec)
	}
}

func TestParseDeployVerifySpecRejectsHostOverride(t *testing.T) {
	for _, path := range []string{"http://evil.example.com/x", "//evil.example.com/x", "evil", ""} {
		raw := `{"checks":[{"name":"bad","method":"GET","path":"` + path + `","expectedStatus":200}]}`
		if _, err := parseDeployVerifySpec(raw); err == nil {
			t.Fatalf("path %q must be rejected (host red line)", path)
		}
	}
}

func TestParseDeployVerifySpecRejectsBadMethodAndStatus(t *testing.T) {
	raw := `{"checks":[{"name":"del","method":"DELETE","path":"/x","expectedStatus":200}]}`
	if _, err := parseDeployVerifySpec(raw); err == nil {
		t.Fatal("DELETE must be rejected")
	}
	raw = `{"checks":[{"name":"ok","method":"GET","path":"/x","expectedStatus":500}]}`
	if _, err := parseDeployVerifySpec(raw); err == nil {
		t.Fatal("expectedStatus 500 must be rejected")
	}
	raw = `{"checks":[]}`
	if _, err := parseDeployVerifySpec(raw); err == nil {
		t.Fatal("empty spec must be rejected")
	}
}

func TestParseDeployVerifySpecStripsDangerousHeaders(t *testing.T) {
	raw := `{"checks":[{"name":"h","method":"GET","path":"/","expectedStatus":200,
		"headers":{"Host":"evil","Authorization":"Bearer x","Cookie":"y","X-Ok":"1"}}]}`
	spec, err := parseDeployVerifySpec(raw)
	if err != nil {
		t.Fatalf("valid headers rejected: %v", err)
	}
	h := spec.Checks[0].Headers
	if _, bad := h["Host"]; bad {
		t.Fatal("Host header must be stripped")
	}
	if _, bad := h["Authorization"]; bad {
		t.Fatal("Authorization header must be stripped")
	}
	if _, bad := h["Cookie"]; bad {
		t.Fatal("Cookie header must be stripped")
	}
	if h["X-Ok"] != "1" {
		t.Fatal("benign header must survive")
	}
}

func TestParseDeployVerifySpecCapsChecks(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"checks":[`)
	for i := 0; i < deployVerifySpecMax+5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"c","method":"GET","path":"/x","expectedStatus":200}`)
	}
	b.WriteString(`]}`)
	spec, err := parseDeployVerifySpec(b.String())
	if err != nil {
		t.Fatalf("oversized spec rejected instead of capped: %v", err)
	}
	if len(spec.Checks) != deployVerifySpecMax {
		t.Fatalf("want capped %d, got %d", deployVerifySpecMax, len(spec.Checks))
	}
}

// ---- deterministic executor ----

func TestRunDeployVerificationExecutesChecks(t *testing.T) {
	// Local fake app: /ok → 200 with body, /bad → 500, /missing → 404.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The executor builds URLs from MULTIGENT_DEPLOY_HOST:port; point the env
	// at the test server.
	t.Setenv(deployHostEnv, strings.TrimPrefix(srv.URL, "http://"))

	s, workspaceID := newConnectionGrantPolicyServer(t)
	_ = workspaceID
	// Use the executor directly with a base override through the env: parse
	// host:port from the test server URL.
	spec := &deployVerifySpec{Checks: []deployVerifyCheck{
		{Name: "health", Method: "GET", Path: "/api/health", ExpectedStatus: 200, Contains: `"ok"`},
		{Name: "broken", Method: "GET", Path: "/broken", ExpectedStatus: 200},
		{Name: "missing", Method: "GET", Path: "/missing", ExpectedStatus: 200},
	}}
	client := &http.Client{Timeout: deployVerifyHTTPTimeout}
	ctx := httptest.NewRequest("POST", "/", nil).Context()

	var passed int
	for _, c := range spec.Checks {
		entry := s.executeDeployCheck(ctx, client, srv.URL, c)
		if entry.Passed {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("only health check should pass, got %d passing", passed)
	}
	_ = controldb.DeployRequest{}
}

// ---- route registration ----

func TestDeployVerificationRoutesRegistered(t *testing.T) {
	s, _, _ := newDeployHandlerTestServer(t)
	handler := s.Handler()
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/projects/sample/deploy/requests/dep-x/verification"},
		{http.MethodPost, "/api/v1/projects/sample/deploy/requests/dep-x/verification/run"},
		{http.MethodPost, "/api/v1/projects/sample/deploy/requests/dep-x/verification/generate"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("route %s %s not registered", tc.method, tc.path)
		}
	}
}
