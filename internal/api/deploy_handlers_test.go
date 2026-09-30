package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigent/multigent/internal/codehost"
	controldb "github.com/multigent/multigent/internal/db"
	"github.com/multigent/multigent/internal/entity"
)

// fakeDeployGitLabServer serves the GitLab endpoints the deploy center
// touches: branches, pipeline trigger, pipeline listing by SHA, pipeline
// jobs and CI variable writes. The recorded calls double as assertions.
type fakeDeployGitLab struct {
	mu           sync.Mutex
	vars         map[string]string // CI variable writes: key → value
	branches     []codehost.BranchInfo
	pipelines    []codehost.PipelineInfo
	jobs         []codehost.PipelineJobInfo
	triggerCount int
}

func newFakeDeployGitLab() *fakeDeployGitLab {
	return &fakeDeployGitLab{
		vars: map[string]string{},
		branches: []codehost.BranchInfo{
			{Name: "main", Default: true, CommitID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CommitTitle: "main head"},
			{Name: "feature", Default: false, CommitID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CommitTitle: "feature head"},
		},
	}
}

func (f *fakeDeployGitLab) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		switch {
		case strings.HasSuffix(path, "/repository/branches"):
			f.mu.Lock()
			out := `[`
			for i, b := range f.branches {
				if i > 0 {
					out += ","
				}
				out += fmt.Sprintf(`{"name":%q,"default":%t,"commit":{"id":%q,"title":%q}}`, b.Name, b.Default, b.CommitID, b.CommitTitle)
			}
			out += `]`
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(out))
		case strings.HasSuffix(path, "/variables"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(path, "/jobs"):
			f.mu.Lock()
			out := `[`
			for i, j := range f.jobs {
				if i > 0 {
					out += ","
				}
				out += fmt.Sprintf(`{"id":%d,"name":%q,"stage":%q,"status":%q,"runner":{"description":%q}}`, j.ID, j.Name, j.Stage, j.Status, j.RunnerDescription)
			}
			out += `]`
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(out))
		case strings.HasSuffix(path, "/pipelines"):
			f.mu.Lock()
			sha := r.URL.Query().Get("sha")
			out := `[`
			n := 0
			for _, p := range f.pipelines {
				if sha != "" && p.SHA != sha {
					continue
				}
				if n > 0 {
					out += ","
				}
				out += fmt.Sprintf(`{"id":%d,"sha":%q,"ref":%q,"status":%q,"web_url":"http://gitlab/pipelines/%d"}`, p.ID, p.SHA, p.Ref, p.Status, p.ID)
				n++
			}
			out += `]`
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(out))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("POST /api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		switch {
		case strings.HasSuffix(path, "/variables"):
			_ = r.ParseForm()
			f.mu.Lock()
			f.vars[r.FormValue("key")] = r.FormValue("value")
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"key":"` + r.FormValue("key") + `"}`))
		case strings.HasSuffix(path, "/pipeline"):
			_ = r.ParseForm()
			f.mu.Lock()
			f.triggerCount++
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			pipeline := codehost.PipelineInfo{
				ID:     int64(900 + f.triggerCount),
				SHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Ref:    fmt.Sprint(body["ref"]),
				Status: "running",
			}
			f.pipelines = append(f.pipelines, pipeline)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":%d,"sha":%q,"ref":%q,"status":"running","web_url":"http://gitlab/pipelines/%d"}`, pipeline.ID, pipeline.SHA, pipeline.Ref, pipeline.ID)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("PUT /api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		if !strings.Contains(path, "/variables/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		key := path[strings.LastIndex(path, "/variables/")+len("/variables/"):]
		f.mu.Lock()
		f.vars[key] = r.FormValue("value")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"` + key + `"}`))
	})
	return mux
}

// newDeployHandlerTestServer wires the standard test Server plus a fake
// GitLab behind the verified remote binding, so handler tests exercise the
// real resolveGitLabHost path with no live forge. The acting user "owner"
// carries the project operator role the deploy write endpoints require.
func newDeployHandlerTestServer(t *testing.T) (*Server, string, *fakeDeployGitLab) {
	t.Helper()
	s, workspaceID := newConnectionGrantPolicyServer(t)
	// "owner" already exists in the base helper (linked to the sample agent);
	// grant it the project operator role the deploy write endpoints require.
	if err := s.users.UpdateUser("owner", nil, nil, nil, nil, nil, nil, nil, []projectAccess{
		{Project: "sample", Role: ProjectRoleOperator},
		{Project: "sample-b", Role: ProjectRoleOperator},
	}, nil, nil); err != nil {
		t.Fatalf("grant owner operator: %v", err)
	}
	fake := newFakeDeployGitLab()
	gitlab := httptest.NewServer(fake.handler())
	t.Cleanup(gitlab.Close)
	seedGitLabConnection(t, s, workspaceID, "conn-gitlab-deploy", gitlab.URL)
	seedVerifiedBinding(t, s, workspaceID, "sample", "conn-gitlab-deploy", "root/sample", "58")
	seedVerifiedBinding(t, s, workspaceID, "sample-b", "conn-gitlab-deploy", "root/sample-b", "59")
	if err := s.st.SaveProject("sample", &entity.Project{Name: "sample", DeployPort: 28123}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	if err := s.st.SaveProject("sample-b", &entity.Project{Name: "sample-b", DeployPort: 28124}); err != nil {
		t.Fatalf("save project b: %v", err)
	}
	return s, workspaceID, fake
}

func deployRequestForTest(t *testing.T, username string) *http.Request {
	t.Helper()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests", username, createDeployRequest{
		Branch: "main",
		SHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vars:   map[string]string{"RELEASE_CHANNEL": "stable"},
	})
	req.SetPathValue("name", "sample")
	return req
}

func TestDeployVerifyTokenRoundTripAndExpiry(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)

	token, err := s.signDeployVerifyToken("sample", "abc123", "dep-1", deployTokenTTL)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, ok := s.verifyDeployVerifyToken(token)
	if !ok {
		t.Fatal("freshly signed token must verify")
	}
	if claims.Project != "sample" || claims.SHA != "abc123" || claims.RequestID != "dep-1" {
		t.Fatalf("claims mismatch: %+v", claims)
	}

	expired, err := s.signDeployVerifyToken("sample", "abc123", "dep-1", -1*time.Second)
	if err != nil {
		t.Fatalf("sign expired: %v", err)
	}
	if _, ok := s.verifyDeployVerifyToken(expired); ok {
		t.Fatal("expired token must be rejected")
	}

	// Tampering with the payload must fail the MAC check.
	parts := strings.Split(token, ".")
	if _, ok := s.verifyDeployVerifyToken(parts[0] + "." + base64Encode([]byte("deadbeef"))); ok {
		t.Fatal("tampered token must be rejected")
	}
	// Truncated MAC must fail too.
	if _, ok := s.verifyDeployVerifyToken(parts[0] + "." + parts[1][:len(parts[1])-4]); ok {
		t.Fatal("truncated MAC must be rejected")
	}
}

func TestHandleDeployVerifyAllowsApprovedSHAAndRejectsMismatch(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)

	seed := controldb.DeployRequest{
		ID: "dep-verify-1", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "1111111111111111111111111111111111111111",
		Status: "approved", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	token, err := s.signDeployVerifyToken("sample", seed.SHA, seed.ID, deployTokenTTL)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Matching SHA over an approved request → 200 {"ok":true}.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/deploy-verify?project=sample&sha="+seed.SHA, nil)
	req.Header.Set(deployTokenHeader, token)
	s.handleDeployVerify(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("matching sha status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Token bound to a different SHA than the query → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/deploy-verify?project=sample&sha=2222222222222222222222222222222222222222", nil)
	req.Header.Set(deployTokenHeader, token)
	s.handleDeployVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched sha status=%d body=%s", rec.Code, rec.Body.String())
	}

	// No token → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/deploy-verify?project=sample&sha="+seed.SHA, nil)
	s.handleDeployVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no token status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Valid token but no approved request for the SHA → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/deploy-verify?project=sample&sha=3333333333333333333333333333333333333333", nil)
	other, err := s.signDeployVerifyToken("sample", "3333333333333333333333333333333333333333", seed.ID, deployTokenTTL)
	if err != nil {
		t.Fatalf("sign other: %v", err)
	}
	req.Header.Set(deployTokenHeader, other)
	s.handleDeployVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unapproved sha status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Terminal-success requests keep admitting the same SHA (redeploy gate).
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, seed.ID, "approved", "deploying"); err != nil {
		t.Fatalf("to deploying: %v", err)
	}
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, seed.ID, "deploying", "success"); err != nil {
		t.Fatalf("to success: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/deploy-verify?project=sample&sha="+seed.SHA, nil)
	req.Header.Set(deployTokenHeader, token)
	s.handleDeployVerify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("success-status sha must stay admitted, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateDeployRequestApprovedImmediate(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	rec := httptest.NewRecorder()
	s.handleCreateDeployRequest(rec, deployRequestForTest(t, "owner"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created controldb.DeployRequest
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Status != "approved" {
		t.Fatalf("status=%s, want approved (approvalRequired omitted defaults off)", created.Status)
	}
	if created.SHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("sha=%s", created.SHA)
	}
	if len(created.CommitSpan) != 1 || created.CommitSpan[0].SHA != created.SHA {
		t.Fatalf("first-deploy span should be [target], got %+v", created.CommitSpan)
	}
	// The ledger row must exist.
	got, found, err := s.controlDB.DeployRequestFor(workspaceID, created.ID)
	if err != nil || !found || got.Status != "approved" {
		t.Fatalf("ledger read: found=%v err=%v", found, err)
	}
	_ = fake
}

func TestHandleCreateDeployRequestPendingApproval(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)

	yes := true
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests", "owner", createDeployRequest{
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ApprovalRequired: &yes,
	})
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleCreateDeployRequest(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created controldb.DeployRequest
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Status != "pending_approval" {
		t.Fatalf("status=%s, want pending_approval", created.Status)
	}
	_ = workspaceID
}

func TestHandleCreateDeployRequestInflightConflict409(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)

	rec := httptest.NewRecorder()
	s.handleCreateDeployRequest(rec, deployRequestForTest(t, "owner"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A second in-flight request for the same project violates the partial
	// unique index → 409.
	rec = httptest.NewRecorder()
	s.handleCreateDeployRequest(rec, deployRequestForTest(t, "owner"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second create status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}

	// After the first request reaches a terminal state, creation works again.
	got, listErr := s.controlDB.ListDeployRequests(controldb.DeployRequestFilter{WorkspaceID: workspaceID, ProjectID: "sample"})
	if listErr != nil || len(got) != 1 {
		t.Fatalf("requests=%d err=%v", len(got), listErr)
	}
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, got[0].ID, "approved", "deploying"); err != nil {
		t.Fatalf("to deploying: %v", err)
	}
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, got[0].ID, "deploying", "success"); err != nil {
		t.Fatalf("to success: %v", err)
	}
	rec = httptest.NewRecorder()
	s.handleCreateDeployRequest(rec, deployRequestForTest(t, "owner"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after terminal status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateDeployRequestSensitiveVarsMaskedAndPushed(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests", "owner", createDeployRequest{
		Branch: "main",
		SHA:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vars: map[string]string{
			"RELEASE_CHANNEL": "stable",
			"DB_PASSWORD":     "hunter2",
			"API_TOKEN":       "tok-123",
			"SIGNING_KEY":     "key-456",
			"CLIENT_SECRET":   "sec-789",
		},
	})
	req.SetPathValue("name", "sample")
	rec := httptest.NewRecorder()
	s.handleCreateDeployRequest(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created controldb.DeployRequest
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Vars["RELEASE_CHANNEL"] != "stable" {
		t.Fatalf("plain var lost: %v", created.Vars)
	}
	for _, key := range []string{"DB_PASSWORD", "API_TOKEN", "SIGNING_KEY", "CLIENT_SECRET"} {
		if created.Vars[key] != deploySensitiveVarMask {
			t.Fatalf("sensitive var %s stored as %q, want mask", key, created.Vars[key])
		}
	}
	// Values must have landed in GitLab under the namespaced keys.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, key := range []string{"DB_PASSWORD", "API_TOKEN", "SIGNING_KEY", "CLIENT_SECRET"} {
		val, ok := fake.vars[deployVarCIPrefix+key]
		if !ok {
			t.Fatalf("var %s never pushed to gitlab (vars=%v)", key, fake.vars)
		}
		if val == deploySensitiveVarMask || val == "" {
			t.Fatalf("var %s pushed with masked/empty value %q", key, val)
		}
	}
	// The persisted ledger row must carry the mask too, never the secret.
	got, found, err := s.controlDB.DeployRequestFor(workspaceID, created.ID)
	if err != nil || !found {
		t.Fatalf("ledger read: %v", err)
	}
	if got.Vars["DB_PASSWORD"] != deploySensitiveVarMask {
		t.Fatalf("persisted sensitive var = %q, want mask", got.Vars["DB_PASSWORD"])
	}
}

func TestDeployRequestCASApproveRejectCancel(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)

	// Each seed parks one request at pending_approval in its OWN project:
	// the inflight partial unique index is per (workspace, project), so a
	// second pending request in "sample" would collide with one still
	// in-flight there.
	seed := func(id, project string) {
		req := controldb.DeployRequest{
			ID: id, WorkspaceID: workspaceID, ProjectID: project,
			Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Status: "pending_approval", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
		}
		if err := s.controlDB.InsertDeployRequest(req); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	call := func(handler func(w http.ResponseWriter, r *http.Request), project, id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := providerTestRequest(http.MethodPost, "/api/v1/projects/"+project+"/deploy/requests/"+id+"/x", "owner", nil)
		req.SetPathValue("name", project)
		req.SetPathValue("id", id)
		handler(rec, req)
		return rec
	}

	// approve: wrong from-status → 409.
	seed("dep-appr", "sample")
	if rec := call(s.handleTriggerDeployRequest, "sample", "dep-appr"); rec.Code != http.StatusConflict {
		t.Fatalf("trigger on pending_approval status=%d body=%s", rec.Code, rec.Body.String())
	}

	// reject: pending_approval → rejected.
	if rec := call(s.handleRejectDeployRequest, "sample", "dep-appr"); rec.Code != http.StatusOK {
		t.Fatalf("reject status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-appr"); got.Status != "rejected" {
		t.Fatalf("after reject status=%s", got.Status)
	}
	// reject again → 409 (CAS from-status no longer matches).
	if rec := call(s.handleRejectDeployRequest, "sample", "dep-appr"); rec.Code != http.StatusConflict {
		t.Fatalf("second reject status=%d body=%s", rec.Code, rec.Body.String())
	}

	// approve: pending_approval → approved → 202 via trigger chain.
	seed("dep-ok", "sample")
	if rec := call(s.handleApproveDeployRequest, "sample", "dep-ok"); rec.Code != http.StatusAccepted {
		t.Fatalf("approve status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-ok"); got.Status != "deploying" {
		t.Fatalf("after approve status=%s, want deploying", got.Status)
	}
	// approve again → 409.
	if rec := call(s.handleApproveDeployRequest, "sample", "dep-ok"); rec.Code != http.StatusConflict {
		t.Fatalf("second approve status=%d body=%s", rec.Code, rec.Body.String())
	}

	// cancel from pending_approval → cancelled; cancel again → 409.
	seed("dep-cancel", "sample-b")
	if rec := call(s.handleCancelDeployRequest, "sample-b", "dep-cancel"); rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-cancel"); got.Status != "cancelled" {
		t.Fatalf("after cancel status=%s", got.Status)
	}
	if rec := call(s.handleCancelDeployRequest, "sample-b", "dep-cancel"); rec.Code != http.StatusConflict {
		t.Fatalf("second cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	// cancel from approved → cancelled.
	seed("dep-cancel-2", "sample-b")
	if _, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, "dep-cancel-2", "pending_approval", "approved"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if rec := call(s.handleCancelDeployRequest, "sample-b", "dep-cancel-2"); rec.Code != http.StatusOK {
		t.Fatalf("cancel approved status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-cancel-2"); got.Status != "cancelled" {
		t.Fatalf("after cancel approved status=%s", got.Status)
	}
}

func TestHandleApproveDeployRequestTriggersPipelineWithGateVars(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	req := controldb.DeployRequest{
		ID: "dep-trig", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: "pending_approval", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	httpReq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests/dep-trig/approve", "owner", nil)
	httpReq.SetPathValue("name", "sample")
	httpReq.SetPathValue("id", "dep-trig")
	s.handleApproveDeployRequest(rec, httpReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("approve status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, _, err := s.controlDB.DeployRequestFor(workspaceID, "dep-trig")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != "deploying" {
		t.Fatalf("status=%s, want deploying", got.Status)
	}
	if got.PipelineID == 0 {
		t.Fatal("pipeline id not recorded")
	}
	fake.mu.Lock()
	triggered := fake.triggerCount > 0
	fake.mu.Unlock()
	if !triggered {
		t.Fatal("pipeline was not triggered against gitlab")
	}

	// A second trigger must now 409 (status is deploying, not approved).
	rec = httptest.NewRecorder()
	httpReq = providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests/dep-trig/trigger", "owner", nil)
	httpReq.SetPathValue("name", "sample")
	httpReq.SetPathValue("id", "dep-trig")
	s.handleTriggerDeployRequest(rec, httpReq)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second trigger status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRecoverActiveDeployRequestsReconcilesTerminalPipeline(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// A success pipeline and a failed pipeline, both already recorded.
	fake.mu.Lock()
	fake.pipelines = []codehost.PipelineInfo{
		{ID: 901, SHA: sha, Ref: "main", Status: "success"},
		{ID: 902, SHA: sha, Ref: "main", Status: "failed"},
	}
	fake.mu.Unlock()

	success := controldb.DeployRequest{
		ID: "dep-rec-ok", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: sha, Status: "deploying", PipelineID: 901,
		CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	// A second project: the inflight partial unique index is per (workspace,
	// project), so the two recovery seeds must not share one.
	failed := controldb.DeployRequest{
		ID: "dep-rec-fail", WorkspaceID: workspaceID, ProjectID: "sample-b",
		Branch: "main", SHA: sha, Status: "deploying", PipelineID: 902,
		CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(success); err != nil {
		t.Fatalf("seed success: %v", err)
	}
	if err := s.controlDB.InsertDeployRequest(failed); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// No deploy host configured: success recovery must still flip the status
	// with an honest unknown-health snapshot.
	s.recoverActiveDeployRequests()

	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-rec-ok"); got.Status != "success" {
		t.Fatalf("success recovery status=%s", got.Status)
	}
	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-rec-fail"); got.Status != "failed" {
		t.Fatalf("failed recovery status=%s", got.Status)
	}
}

func TestRecoverActiveDeployRequestsFailsStaleUnknownPipeline(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	// No pipeline for this SHA on the forge, and the request is older than
	// the 2h stale bound → recovery must fail it.
	sha := "cccccccccccccccccccccccccccccccccccccccc"
	stale := controldb.DeployRequest{
		ID: "dep-rec-stale", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: sha, Status: "deploying", PipelineID: 903,
		CreatedBy: "owner",
		CreatedAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
	}
	if err := s.controlDB.InsertDeployRequest(stale); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = fake

	s.recoverActiveDeployRequests()

	if got, _, _ := s.controlDB.DeployRequestFor(workspaceID, "dep-rec-stale"); got.Status != "failed" {
		t.Fatalf("stale recovery status=%s, want failed", got.Status)
	}
}

func TestDeploySensitiveVarDetection(t *testing.T) {
	cases := map[string]bool{
		"DB_PASSWORD":     true,
		"db_password":     true,
		"API_TOKEN":       true,
		"SIGNING_KEY":     true,
		"CLIENT_SECRET":   true,
		"MY_TOKEN_VALUE":  true,
		"RELEASE_CHANNEL": false,
		"DEBUG":           false,
		"KEYSTONE":        true, // contains KEY — conservative by design
		"":                false,
	}
	for key, want := range cases {
		if got := deploySensitiveVar(key); got != want {
			t.Fatalf("deploySensitiveVar(%q)=%v, want %v", key, got, want)
		}
	}
}

func TestConsoleReachableURLEnvOverride(t *testing.T) {
	s, _ := newConnectionGrantPolicyServer(t)
	t.Setenv(consoleURLEnv, "https://console.example.com/")
	if got := s.consoleReachableURL(httptest.NewRequest("GET", "http://127.0.0.1:27892/x", nil)); got != "https://console.example.com" {
		t.Fatalf("env override = %q", got)
	}
	t.Setenv(consoleURLEnv, "")
	req := httptest.NewRequest("GET", "http://127.0.0.1:27892/x", nil)
	if got := s.consoleReachableURL(req); got != "http://127.0.0.1:27892" {
		t.Fatalf("host fallback = %q", got)
	}
}

func TestGetDeployAggregateReturnsLedgerWithoutForge(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)

	seed := controldb.DeployRequest{
		ID: "dep-agg", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: "success", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/deploy", "owner", nil)
	req.SetPathValue("name", "sample")
	s.handleGetDeployAggregate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
	}
	var agg struct {
		Project      string                   `json:"project"`
		DeployPort   int                      `json:"deployPort"`
		LastDeployed *controldb.DeployRequest `json:"lastDeployed"`
		Inflight     *controldb.DeployRequest `json:"inflight"`
		Health       map[string]any           `json:"health"`
		Pipelines    []map[string]any         `json:"pipelines"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&agg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if agg.Project != "sample" || agg.DeployPort != 28123 {
		t.Fatalf("project=%s port=%d", agg.Project, agg.DeployPort)
	}
	if agg.LastDeployed == nil || agg.LastDeployed.ID != "dep-agg" {
		t.Fatalf("lastDeployed=%+v", agg.LastDeployed)
	}
	if agg.Inflight != nil {
		t.Fatalf("inflight should be nil, got %+v", agg.Inflight)
	}
	if agg.Health == nil || agg.Health["status"] != "unknown" {
		t.Fatalf("health=%v, want unknown (host not configured)", agg.Health)
	}
	if agg.Pipelines == nil || len(agg.Pipelines) != 0 {
		t.Fatalf("pipelines=%v, want empty (no forge reachability needed)", agg.Pipelines)
	}
}

func TestGetDeployPreviewStateLightweight(t *testing.T) {
	s, workspaceID, _ := newDeployHandlerTestServer(t)

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/deploy/preview-state", "owner", nil)
	req.SetPathValue("name", "sample")
	s.handleGetDeployPreviewState(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview-state status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		LastDeployed *controldb.DeployRequest `json:"lastDeployed"`
		Inflight     *controldb.DeployRequest `json:"inflight"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.LastDeployed != nil || out.Inflight != nil {
		t.Fatalf("empty ledger should give nils, got %+v", out)
	}

	seed := controldb.DeployRequest{
		ID: "dep-ps", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: "approved", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec = httptest.NewRecorder()
	req = providerTestRequest(http.MethodGet, "/api/v1/projects/sample/deploy/preview-state", "owner", nil)
	req.SetPathValue("name", "sample")
	s.handleGetDeployPreviewState(rec, req)
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if out.Inflight == nil || out.Inflight.ID != "dep-ps" {
		t.Fatalf("inflight=%+v", out.Inflight)
	}
}

func TestHandleGetDeployBranchesProxy(t *testing.T) {
	s, _, fake := newDeployHandlerTestServer(t)

	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodGet, "/api/v1/projects/sample/deploy/branches", "owner", nil)
	req.SetPathValue("name", "sample")
	s.handleGetDeployBranches(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("branches status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fake.mu.Lock()
	want := len(fake.branches)
	fake.mu.Unlock()
	if len(out) != want {
		t.Fatalf("branches=%d, want %d", len(out), want)
	}
	if out[0]["name"] != "main" || out[0]["isDefault"] != true {
		t.Fatalf("first branch=%v", out[0])
	}
}

var _ = context.Background

// Regression for the adversarial review P1-1: the trigger chain used to
// discard the CAS boolean, so a cancelled request (or a second concurrent
// approver) could still fire a pipeline. The CAS must be authoritative: a
// lost approved→deploying transition refuses with 409 and never touches
// GitLab.
func TestDeployTriggerRefusesWhenCASLostCancelRace(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	req := controldb.DeployRequest{
		ID: "dep-race-cancel", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: "approved", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulate the cancel racing ahead: approved → cancelled wins before the
	// trigger handler runs.
	if moved, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "approved", "cancelled"); err != nil || !moved {
		t.Fatalf("pre-cancel: moved=%v err=%v", moved, err)
	}

	rec := httptest.NewRecorder()
	hreq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests/dep-race-cancel/trigger", "owner", nil)
	hreq.SetPathValue("name", "sample")
	hreq.SetPathValue("id", req.ID)
	s.handleTriggerDeployRequest(rec, hreq)

	if rec.Code != http.StatusConflict {
		t.Fatalf("trigger after cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	triggered := fake.triggerCount
	fake.mu.Unlock()
	if triggered != 0 {
		t.Fatalf("pipeline fired %d times for a cancelled request", triggered)
	}
}

// Regression for the adversarial review P1-1 (double trigger): when the
// approved→deploying CAS loses (another actor already moved the row), the
// shared trigger body must 409 and not sign a token or fire a pipeline.
func TestDeployTriggerRefusesWhenCASLostDoubleTrigger(t *testing.T) {
	s, workspaceID, fake := newDeployHandlerTestServer(t)

	req := controldb.DeployRequest{
		ID: "dep-race-double", WorkspaceID: workspaceID, ProjectID: "sample",
		Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status: "approved", CreatedBy: "owner", CreatedAt: nowUTCAPI(),
	}
	if err := s.controlDB.InsertDeployRequest(req); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// First approver wins the approved→deploying CAS.
	if moved, err := s.controlDB.UpdateDeployRequestStatus(workspaceID, req.ID, "approved", "deploying"); err != nil || !moved {
		t.Fatalf("first trigger CAS: moved=%v err=%v", moved, err)
	}

	rec := httptest.NewRecorder()
	hreq := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/deploy/requests/dep-race-double/trigger", "owner", nil)
	hreq.SetPathValue("name", "sample")
	hreq.SetPathValue("id", req.ID)
	s.handleTriggerDeployRequest(rec, hreq)

	if rec.Code != http.StatusConflict {
		t.Fatalf("second trigger status=%d body=%s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	triggered := fake.triggerCount
	fake.mu.Unlock()
	if triggered != 0 {
		t.Fatalf("second trigger fired a pipeline (count=%d)", triggered)
	}
}

// Regression for the adversarial review P1-2: the chatops trigger path builds
// the console URL without a real request; it must fall back to
// MULTIGENT_API_URL (set from the real listen address) instead of the
// synthetic "chatops.internal" Host.
func TestConsoleReachableURLPrefersEnvOverSyntheticHost(t *testing.T) {
	t.Setenv("MULTIGENT_API_URL", "http://192.168.139.231:27892")
	s := &Server{}
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/internal", nil)
	got := s.consoleReachableURL(r)
	if got != "http://192.168.139.231:27892" {
		t.Fatalf("console URL = %q, want MULTIGENT_API_URL value", got)
	}

	t.Setenv("MULTIGENT_API_URL", "")
	got = s.consoleReachableURL(r)
	if got != "http://"+r.Host {
		t.Fatalf("console URL = %q, want request host fallback %q", got, r.Host)
	}
}

// Regression for the adversarial review P1-3: DeployRequest JSON keys must be
// camelCase end-to-end — the console types declare createdBy/createdAt/
// pipelineId/finishedAt and there is no key-transform layer in the web client.
func TestDeployRequestJSONKeysAreCamelCase(t *testing.T) {
	req := controldb.DeployRequest{
		ID: "dep-json", WorkspaceID: "ws", ProjectID: "sample", Branch: "main",
		SHA: "abc", Status: "approved", PipelineID: 42, CreatedBy: "owner",
		CreatedAt: "2026-10-01T00:00:00Z",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"\"createdBy\"", "\"createdAt\"", "\"pipelineId\"", "\"projectId\"", "\"commitSpan\""} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("json missing %s: %s", key, b)
		}
	}
	for _, stale := range []string{"\"created_by\"", "\"created_at\"", "\"pipeline_id\"", "\"project_id\""} {
		if strings.Contains(string(b), stale) {
			t.Fatalf("json still has snake_case %s: %s", stale, b)
		}
	}
}
