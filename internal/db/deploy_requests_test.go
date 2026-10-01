package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func openDeployRequestTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestDeployRequestInsertAndRoundTrip(t *testing.T) {
	store := openDeployRequestTestStore(t)

	req := DeployRequest{
		ID:          "dep-" + strings.Repeat("a", 12),
		WorkspaceID: "ws-1",
		ProjectID:   "proj-1",
		Branch:      "feature/deploy-center",
		SHA:         "abcdef1234567890abcdef1234567890abcdef12",
		Env:         "production",
		Vars: map[string]string{
			"APP_PORT":  "8080",
			"IMAGE_TAG": "v1.2.3",
		},
		CommitSpan: []CommitSpanEntry{
			{
				SHA:         "abcdef1234567890abcdef1234567890abcdef12",
				ShortSHA:    "abcdef1",
				Title:       "feat: deploy center phase 1",
				Author:      "alice",
				CommittedAt: "2026-09-30T01:02:03Z",
			},
			{
				SHA:         "1111111234567890abcdef1234567890abcdef1",
				ShortSHA:    "1111111",
				Title:       "chore: bump deps",
				Author:      "bob",
				CommittedAt: "2026-09-29T01:02:03Z",
			},
		},
		Approval: map[string]any{
			"approver":    "josh",
			"approved_at": "2026-09-30T08:00:00Z",
			"comment":     "ship it",
		},
		Status:    "pending_approval",
		CreatedBy: "usr-alice",
		CreatedAt: "2026-09-30T07:00:00Z",
	}
	if err := store.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest failed: %v", err)
	}

	got, found, err := store.DeployRequestFor("ws-1", req.ID)
	if err != nil {
		t.Fatalf("DeployRequestFor failed: %v", err)
	}
	if !found {
		t.Fatalf("DeployRequestFor: request not found")
	}
	if got.ID != req.ID || got.WorkspaceID != "ws-1" || got.ProjectID != "proj-1" {
		t.Fatalf("identity fields mismatch: %+v", got)
	}
	if got.Branch != req.Branch || got.SHA != req.SHA || got.Env != "production" {
		t.Fatalf("basic fields mismatch: %+v", got)
	}
	if got.Status != "pending_approval" || got.CreatedBy != "usr-alice" {
		t.Fatalf("status/created_by mismatch: %+v", got)
	}
	if got.CreatedAt != req.CreatedAt || got.StartedAt != "" || got.FinishedAt != "" {
		t.Fatalf("timestamps mismatch: %+v", got)
	}
	if len(got.Vars) != 2 || got.Vars["APP_PORT"] != "8080" || got.Vars["IMAGE_TAG"] != "v1.2.3" {
		t.Fatalf("vars round-trip mismatch: %+v", got.Vars)
	}
	if len(got.CommitSpan) != 2 {
		t.Fatalf("commit span length = %d, want 2", len(got.CommitSpan))
	}
	span := got.CommitSpan[0]
	if span.SHA != req.CommitSpan[0].SHA || span.ShortSHA != "abcdef1" ||
		span.Title != "feat: deploy center phase 1" || span.Author != "alice" ||
		span.CommittedAt != "2026-09-30T01:02:03Z" {
		t.Fatalf("commit span entry mismatch: %+v", span)
	}
	if got.Approval["approver"] != "josh" || got.Approval["comment"] != "ship it" {
		t.Fatalf("approval round-trip mismatch: %+v", got.Approval)
	}
	if got.Health != nil {
		t.Fatalf("health should be nil when stored empty, got %+v", got.Health)
	}

	// Missing id must be fail-closed: (nil, false, nil).
	missing, found, err := store.DeployRequestFor("ws-1", "dep-does-not-exist")
	if err != nil || found || missing != nil {
		t.Fatalf("DeployRequestFor missing request = (%v, %v, %v), want (nil, false, nil)", missing, found, err)
	}
}

func TestDeployRequestCASUpdate(t *testing.T) {
	store := openDeployRequestTestStore(t)

	req := DeployRequest{
		ID:          "dep-cas-1",
		WorkspaceID: "ws-cas",
		ProjectID:   "proj-cas",
		Branch:      "main",
		SHA:         "sha-cas-1",
		Status:      "pending_approval",
		CreatedBy:   "usr-test",
	}
	if err := store.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest failed: %v", err)
	}

	// Wrong `from` status must not transition and must return false.
	ok, err := store.UpdateDeployRequestStatus("ws-cas", "dep-cas-1", "deploying", "approved")
	if err != nil {
		t.Fatalf("UpdateDeployRequestStatus (mismatch) returned error: %v", err)
	}
	if ok {
		t.Fatalf("UpdateDeployRequestStatus with wrong from status must return false")
	}
	got, found, err := store.DeployRequestFor("ws-cas", "dep-cas-1")
	if err != nil || !found {
		t.Fatalf("DeployRequestFor failed: found=%v err=%v", found, err)
	}
	if got.Status != "pending_approval" {
		t.Fatalf("status changed on failed CAS: %q", got.Status)
	}

	// Correct `from` status transitions and returns true.
	ok, err = store.UpdateDeployRequestStatus("ws-cas", "dep-cas-1", "pending_approval", "approved")
	if err != nil {
		t.Fatalf("UpdateDeployRequestStatus failed: %v", err)
	}
	if !ok {
		t.Fatalf("UpdateDeployRequestStatus with matching from status must return true")
	}
	got, _, err = store.DeployRequestFor("ws-cas", "dep-cas-1")
	if err != nil {
		t.Fatalf("DeployRequestFor failed: %v", err)
	}
	if got.Status != "approved" {
		t.Fatalf("status after CAS = %q, want approved", got.Status)
	}

	// Replay of the same transition fails (from no longer matches).
	ok, err = store.UpdateDeployRequestStatus("ws-cas", "dep-cas-1", "pending_approval", "approved")
	if err != nil || ok {
		t.Fatalf("replayed CAS must be (false, nil), got (%v, %v)", ok, err)
	}
}

func TestDeployRequestInflightMutualExclusion(t *testing.T) {
	store := openDeployRequestTestStore(t)

	first := DeployRequest{
		ID:          "dep-mutex-1",
		WorkspaceID: "ws-mutex",
		ProjectID:   "proj-mutex",
		Branch:      "main",
		SHA:         "sha-mutex-1",
		Status:      "deploying",
		CreatedBy:   "usr-test",
	}
	if err := store.InsertDeployRequest(first); err != nil {
		t.Fatalf("InsertDeployRequest first failed: %v", err)
	}

	// Same workspace+project with another in-flight status must violate the
	// partial unique index.
	conflict := DeployRequest{
		ID:          "dep-mutex-2",
		WorkspaceID: "ws-mutex",
		ProjectID:   "proj-mutex",
		Branch:      "main",
		SHA:         "sha-mutex-2",
		Status:      "approved",
		CreatedBy:   "usr-test",
	}
	if err := store.InsertDeployRequest(conflict); err == nil {
		t.Fatalf("insert of second in-flight request must violate partial unique index, got nil error")
	}

	// A different project in the same workspace is not restricted.
	otherProject := DeployRequest{
		ID:          "dep-mutex-3",
		WorkspaceID: "ws-mutex",
		ProjectID:   "proj-other",
		Branch:      "main",
		SHA:         "sha-mutex-3",
		Status:      "approved",
		CreatedBy:   "usr-test",
	}
	if err := store.InsertDeployRequest(otherProject); err != nil {
		t.Fatalf("insert into different project must succeed, got: %v", err)
	}

	// After the first request reaches a terminal status the slot frees up.
	ok, err := store.UpdateDeployRequestStatus("ws-mutex", "dep-mutex-1", "deploying", "success")
	if err != nil || !ok {
		t.Fatalf("terminal transition failed: ok=%v err=%v", ok, err)
	}
	if err := store.InsertDeployRequest(conflict); err != nil {
		t.Fatalf("insert after terminal status must succeed, got: %v", err)
	}
}

func TestHasApprovedDeployRequestForSHA(t *testing.T) {
	store := openDeployRequestTestStore(t)

	// pending_approval is NOT an approved state.
	if err := store.InsertDeployRequest(DeployRequest{
		ID:          "dep-sha-1",
		WorkspaceID: "ws-sha",
		ProjectID:   "proj-sha",
		Branch:      "main",
		SHA:         "sha-hit",
		Status:      "pending_approval",
		CreatedBy:   "usr-test",
	}); err != nil {
		t.Fatalf("InsertDeployRequest failed: %v", err)
	}
	hit, err := store.HasApprovedDeployRequestForSHA("ws-sha", "proj-sha", "sha-hit")
	if err != nil {
		t.Fatalf("HasApprovedDeployRequestForSHA failed: %v", err)
	}
	if hit {
		t.Fatalf("pending_approval must not count as approved")
	}

	// Walk the lifecycle: approved, deploying and success all count as hits.
	transitions := []struct{ from, to string }{
		{"pending_approval", "approved"},
		{"approved", "deploying"},
		{"deploying", "success"},
	}
	for _, tr := range transitions {
		ok, err := store.UpdateDeployRequestStatus("ws-sha", "dep-sha-1", tr.from, tr.to)
		if err != nil || !ok {
			t.Fatalf("transition %s->%s failed: ok=%v err=%v", tr.from, tr.to, ok, err)
		}
		hit, err := store.HasApprovedDeployRequestForSHA("ws-sha", "proj-sha", "sha-hit")
		if err != nil {
			t.Fatalf("HasApprovedDeployRequestForSHA failed: %v", err)
		}
		if !hit {
			t.Fatalf("status %q must hit for its sha", tr.to)
		}
	}

	// A failed request never counts.
	if err := store.InsertDeployRequest(DeployRequest{
		ID:          "dep-sha-2",
		WorkspaceID: "ws-sha",
		ProjectID:   "proj-sha",
		Branch:      "main",
		SHA:         "sha-failed",
		Status:      "failed",
		CreatedBy:   "usr-test",
	}); err != nil {
		t.Fatalf("InsertDeployRequest failed: %v", err)
	}
	hit, err = store.HasApprovedDeployRequestForSHA("ws-sha", "proj-sha", "sha-failed")
	if err != nil || hit {
		t.Fatalf("failed status must miss, got hit=%v err=%v", hit, err)
	}

	// Unknown SHA and unknown project must be a clean miss.
	hit, err = store.HasApprovedDeployRequestForSHA("ws-sha", "proj-sha", "sha-unknown")
	if err != nil || hit {
		t.Fatalf("unknown sha must miss, got hit=%v err=%v", hit, err)
	}
	hit, err = store.HasApprovedDeployRequestForSHA("ws-sha", "proj-other", "sha-hit")
	if err != nil || hit {
		t.Fatalf("unknown project must miss, got hit=%v err=%v", hit, err)
	}
}

func TestDeployRequestListFilterAndLimit(t *testing.T) {
	store := openDeployRequestTestStore(t)

	rows := []DeployRequest{
		{ID: "dep-list-1", WorkspaceID: "ws-list", ProjectID: "proj-a", Branch: "main", SHA: "s1", Status: "success", CreatedBy: "u", CreatedAt: "2026-09-01T00:00:00Z"},
		{ID: "dep-list-2", WorkspaceID: "ws-list", ProjectID: "proj-a", Branch: "main", SHA: "s2", Status: "failed", CreatedBy: "u", CreatedAt: "2026-09-02T00:00:00Z"},
		{ID: "dep-list-3", WorkspaceID: "ws-list", ProjectID: "proj-a", Branch: "main", SHA: "s3", Status: "success", CreatedBy: "u", CreatedAt: "2026-09-03T00:00:00Z"},
		{ID: "dep-list-4", WorkspaceID: "ws-list", ProjectID: "proj-b", Branch: "main", SHA: "s4", Status: "success", CreatedBy: "u", CreatedAt: "2026-09-04T00:00:00Z"},
		{ID: "dep-list-5", WorkspaceID: "ws-other", ProjectID: "proj-a", Branch: "main", SHA: "s5", Status: "success", CreatedBy: "u", CreatedAt: "2026-09-05T00:00:00Z"},
	}
	for _, r := range rows {
		if err := store.InsertDeployRequest(r); err != nil {
			t.Fatalf("InsertDeployRequest %s failed: %v", r.ID, err)
		}
	}

	// Workspace + project filter, newest first.
	list, err := store.ListDeployRequests(DeployRequestFilter{WorkspaceID: "ws-list", ProjectID: "proj-a"})
	if err != nil {
		t.Fatalf("ListDeployRequests failed: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d rows, want 3", len(list))
	}
	wantOrder := []string{"dep-list-3", "dep-list-2", "dep-list-1"}
	for i, id := range wantOrder {
		if list[i].ID != id {
			t.Fatalf("row %d = %s, want %s", i, list[i].ID, id)
		}
	}

	// Status filter on top of project filter.
	list, err = store.ListDeployRequests(DeployRequestFilter{WorkspaceID: "ws-list", ProjectID: "proj-a", Status: "success"})
	if err != nil {
		t.Fatalf("ListDeployRequests failed: %v", err)
	}
	if len(list) != 2 || list[0].ID != "dep-list-3" || list[1].ID != "dep-list-1" {
		t.Fatalf("status filter mismatch: %+v", list)
	}

	// Limit.
	list, err = store.ListDeployRequests(DeployRequestFilter{WorkspaceID: "ws-list", ProjectID: "proj-a", Limit: 1})
	if err != nil {
		t.Fatalf("ListDeployRequests failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != "dep-list-3" {
		t.Fatalf("limit mismatch: %+v", list)
	}
}

func TestLatestSucceededDeployRequest(t *testing.T) {
	store := openDeployRequestTestStore(t)

	none, found, err := store.LatestSucceededDeployRequest("ws-latest", "proj-latest")
	if err != nil || found || none != nil {
		t.Fatalf("empty project must be (nil, false, nil), got (%v, %v, %v)", none, found, err)
	}

	rows := []DeployRequest{
		{ID: "dep-latest-1", WorkspaceID: "ws-latest", ProjectID: "proj-latest", Branch: "main", SHA: "s1", Status: "success", CreatedBy: "u", CreatedAt: "2026-09-01T00:00:00Z"},
		{ID: "dep-latest-2", WorkspaceID: "ws-latest", ProjectID: "proj-latest", Branch: "main", SHA: "s2", Status: "failed", CreatedBy: "u", CreatedAt: "2026-09-02T00:00:00Z"},
		{ID: "dep-latest-3", WorkspaceID: "ws-latest", ProjectID: "proj-latest", Branch: "main", SHA: "s3", Status: "success", CreatedBy: "u", CreatedAt: "2026-09-03T00:00:00Z"},
	}
	for _, r := range rows {
		if err := store.InsertDeployRequest(r); err != nil {
			t.Fatalf("InsertDeployRequest %s failed: %v", r.ID, err)
		}
	}

	got, found, err := store.LatestSucceededDeployRequest("ws-latest", "proj-latest")
	if err != nil {
		t.Fatalf("LatestSucceededDeployRequest failed: %v", err)
	}
	if !found {
		t.Fatalf("LatestSucceededDeployRequest: not found")
	}
	if got.ID != "dep-latest-3" {
		t.Fatalf("latest success = %s, want dep-latest-3", got.ID)
	}
	if got.SHA != "s3" {
		t.Fatalf("latest success sha = %s, want s3", got.SHA)
	}
}

func TestDeployRequestPipelineHealthAndDeployingList(t *testing.T) {
	store := openDeployRequestTestStore(t)

	req := DeployRequest{
		ID:          "dep-ops-1",
		WorkspaceID: "ws-ops",
		ProjectID:   "proj-ops",
		Branch:      "main",
		SHA:         "sha-ops-1",
		Status:      "pending_approval",
		CreatedBy:   "usr-test",
	}
	if err := store.InsertDeployRequest(req); err != nil {
		t.Fatalf("InsertDeployRequest failed: %v", err)
	}

	if err := store.SetDeployRequestPipeline("ws-ops", "dep-ops-1", 4242); err != nil {
		t.Fatalf("SetDeployRequestPipeline failed: %v", err)
	}
	health := map[string]any{"url": "https://example.invalid/healthz", "checks": 3}
	if err := store.SetDeployRequestHealth("ws-ops", "dep-ops-1", health); err != nil {
		t.Fatalf("SetDeployRequestHealth failed: %v", err)
	}
	got, found, err := store.DeployRequestFor("ws-ops", "dep-ops-1")
	if err != nil || !found {
		t.Fatalf("DeployRequestFor failed: found=%v err=%v", found, err)
	}
	if got.PipelineID != 4242 {
		t.Fatalf("PipelineID = %d, want 4242", got.PipelineID)
	}
	if got.Health["url"] != "https://example.invalid/healthz" {
		t.Fatalf("health round-trip mismatch: %+v", got.Health)
	}

	// Not deploying yet: the self-healing scan must not see it.
	deploying, err := store.ListDeployingDeployRequests()
	if err != nil {
		t.Fatalf("ListDeployingDeployRequests failed: %v", err)
	}
	for _, r := range deploying {
		if r.ID == "dep-ops-1" {
			t.Fatalf("pending request must not appear in deploying list")
		}
	}

	if ok, err := store.UpdateDeployRequestStatus("ws-ops", "dep-ops-1", "pending_approval", "deploying"); err != nil || !ok {
		t.Fatalf("transition to deploying failed: ok=%v err=%v", ok, err)
	}
	deploying, err = store.ListDeployingDeployRequests()
	if err != nil {
		t.Fatalf("ListDeployingDeployRequests failed: %v", err)
	}
	foundInList := false
	for _, r := range deploying {
		if r.ID == "dep-ops-1" {
			foundInList = true
			if r.WorkspaceID != "ws-ops" {
				t.Fatalf("deploying row workspace mismatch: %+v", r)
			}
		}
	}
	if !foundInList {
		t.Fatalf("deploying request must appear in ListDeployingDeployRequests")
	}
}
