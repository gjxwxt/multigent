package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

// seedTaskAssetFile registers one asset_files row (plus its blob row) in the
// control DB so asset-binding validation can resolve it.
func seedTaskAssetFile(t *testing.T, s *Server, workspaceID, project, displayName string) *controldb.AssetFile {
	t.Helper()
	sum := sha256.Sum256([]byte(displayName))
	sha := hex.EncodeToString(sum[:])
	if err := s.controlDB.UpsertAssetBlob(controldb.AssetBlob{
		Sha256: sha, Size: int64(len(displayName)), Mime: "text/plain",
		StoragePath: filepath.Join(sha[:2], sha), CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	f := &controldb.AssetFile{
		WorkspaceID: workspaceID, ProjectID: project,
		DisplayName: displayName, CurrentSha: sha, CreatedBy: "tester",
	}
	if err := s.controlDB.InsertAssetFile(f); err != nil {
		t.Fatalf("insert asset file: %v", err)
	}
	return f
}

func postCreateTaskWithAssets(t *testing.T, s *Server, body postTaskBody) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := providerTestRequest(http.MethodPost, "/api/v1/projects/sample/tasks", "admin", body)
	req.SetPathValue("name", "sample")
	s.handlePostProjectTask(rec, req)
	return rec
}

func decodeCreatedTaskID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode creation response: %v (%s)", err, rec.Body.String())
	}
	return created.ID
}

// TestTaskCreateWithAssetsBindsAtomically: bindings supplied on the creation
// request exist before any startup source can fire — the whole point of
// carrying them in-request.
func TestTaskCreateWithAssetsBindsAtomically(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	f1 := seedTaskAssetFile(t, s, workspaceID, "sample", "requirements.md")
	f2 := seedTaskAssetFile(t, s, workspaceID, "sample", "icon.png")

	rec := postCreateTaskWithAssets(t, s, postTaskBody{
		Agent: "pm", Title: "bound at creation", Prompt: "do things",
		Assignee: "sample/pm",
		Assets: []taskCreateAssetBinding{
			{FileID: f1.ID, Role: "requirement_input", Required: true},
			{FileID: f2.ID, Role: "reference", Required: false},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creation failed: %d %s", rec.Code, rec.Body.String())
	}
	taskID := decodeCreatedTaskID(t, rec)

	atts, err := s.controlDB.ListAssetAttachmentsForTask(taskID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(atts))
	}
	byRole := map[string]controldb.AssetAttachmentWithFile{}
	for _, att := range atts {
		byRole[att.Role] = att
	}
	reqAtt, ok := byRole["requirement_input"]
	if !ok || reqAtt.Required != true || reqAtt.DisplayName != "requirements.md" {
		t.Fatalf("requirement_input binding wrong: %+v", reqAtt)
	}
	refAtt, ok := byRole["reference"]
	if !ok || refAtt.Required != false || refAtt.DisplayName != "icon.png" {
		t.Fatalf("reference binding wrong: %+v", refAtt)
	}
	// The task must exist in the task store with the bindings visible.
	if _, err := s.ts.GetTask("sample", "pm", taskID); err != nil {
		t.Fatalf("task missing from store: %v", err)
	}
}

// TestTaskCreateWithAssetsEmptyRoleDefaultsToReference mirrors the standalone
// bind endpoint's default (assets_handlers.go): an omitted role resolves to
// reference, not requirement_input.
func TestTaskCreateWithAssetsEmptyRoleDefaultsToReference(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	f := seedTaskAssetFile(t, s, workspaceID, "sample", "notes.txt")

	rec := postCreateTaskWithAssets(t, s, postTaskBody{
		Agent: "pm", Title: "default role", Prompt: "do things",
		Assets: []taskCreateAssetBinding{{FileID: f.ID}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creation failed: %d %s", rec.Code, rec.Body.String())
	}
	atts, _ := s.controlDB.ListAssetAttachmentsForTask(decodeCreatedTaskID(t, rec))
	if len(atts) != 1 || atts[0].Role != controldb.AssetRoleReference {
		t.Fatalf("empty role must default to reference, got %+v", atts)
	}
}

func TestTaskCreateWithAssetsRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutator func(t *testing.T, s *Server, workspaceID string, body *postTaskBody)
	}{
		{
			name: "unknown fileId",
			mutator: func(t *testing.T, s *Server, workspaceID string, body *postTaskBody) {
				body.Assets = []taskCreateAssetBinding{{FileID: "asf-missing", Role: "reference"}}
			},
		},
		{
			name: "empty fileId",
			mutator: func(t *testing.T, s *Server, workspaceID string, body *postTaskBody) {
				body.Assets = []taskCreateAssetBinding{{FileID: "  "}}
			},
		},
		{
			name: "invalid role",
			mutator: func(t *testing.T, s *Server, workspaceID string, body *postTaskBody) {
				f := seedTaskAssetFile(t, s, workspaceID, "sample", "bad-role.txt")
				body.Assets = []taskCreateAssetBinding{{FileID: f.ID, Role: "counter_example"}}
			},
		},
		{
			name: "archived file",
			mutator: func(t *testing.T, s *Server, workspaceID string, body *postTaskBody) {
				f := seedTaskAssetFile(t, s, workspaceID, "sample", "archived.txt")
				if err := s.controlDB.ArchiveAssetFile(f.ID); err != nil {
					t.Fatalf("archive: %v", err)
				}
				body.Assets = []taskCreateAssetBinding{{FileID: f.ID, Role: "reference"}}
			},
		},
		{
			name: "file from another project",
			mutator: func(t *testing.T, s *Server, workspaceID string, body *postTaskBody) {
				other := seedTaskAssetFile(t, s, workspaceID, "other-project", "foreign.txt")
				body.Assets = []taskCreateAssetBinding{{FileID: other.ID, Role: "reference"}}
			},
		},
		{
			name: "over the attachment budget",
			mutator: func(t *testing.T, s *Server, workspaceID string, body *postTaskBody) {
				for i := 0; i < 9; i++ {
					f := seedTaskAssetFile(t, s, workspaceID, "sample", fmt.Sprintf("bulk-%d.txt", i))
					body.Assets = append(body.Assets, taskCreateAssetBinding{FileID: f.ID, Role: "reference"})
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, workspaceID := newBranchJoinHTTPServer(t)
			body := postTaskBody{Agent: "pm", Title: "rejected", Prompt: "do things"}
			tc.mutator(t, s, workspaceID, &body)

			rec := postCreateTaskWithAssets(t, s, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d %s", rec.Code, rec.Body.String())
			}
			// The task must NOT exist — the rejection left zero residue.
			tasks, err := s.ts.ListTasks("sample", "pm")
			if err != nil {
				t.Fatalf("list tasks: %v", err)
			}
			for _, task := range tasks {
				if task.Title == "rejected" {
					t.Fatalf("rejected creation left a task behind: %s", task.ID)
				}
			}
		})
	}
}

// TestTaskCreateWithAssetsDuplicateFileIDCollapses: declaring the same file
// twice updates nothing (there is nothing yet to update at creation) — it
// collapses to one binding with the FIRST declaration's semantics, and the
// second occurrence is ignored rather than rejected.
func TestTaskCreateWithAssetsDuplicateFileIDCollapses(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	f := seedTaskAssetFile(t, s, workspaceID, "sample", "dup.txt")

	rec := postCreateTaskWithAssets(t, s, postTaskBody{
		Agent: "pm", Title: "dup collapse", Prompt: "do things",
		Assets: []taskCreateAssetBinding{
			{FileID: f.ID, Role: "reference", Required: true},
			{FileID: f.ID, Role: "requirement_input"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creation failed: %d %s", rec.Code, rec.Body.String())
	}
	atts, _ := s.controlDB.ListAssetAttachmentsForTask(decodeCreatedTaskID(t, rec))
	if len(atts) != 1 {
		t.Fatalf("duplicate fileId must collapse to one binding, got %d", len(atts))
	}
	if atts[0].Role != "reference" {
		t.Fatalf("first declaration must win, got role %q", atts[0].Role)
	}
}

// TestTaskCreateWithAssetsRollbackOnInsertFailure: when the attachment insert
// itself fails mid-loop, the compensating delete must remove the task record
// — the caller's observable outcome is "creation failed", not a task without
// its assets. Simulated by a file whose blob row is missing, which passes the
// read-only validation (it only checks asset_files) but fails... actually it
// cannot fail there: InsertAssetAttachment has no FK to blobs. So the
// compensating path is exercised structurally: validation passes, insert
// succeeds, and this test pins the SUCCESS path plus asserts the rollback
// helper removes everything it should when invoked directly.
func TestTaskCreateRollbackRemovesTaskAndBindings(t *testing.T) {
	s, workspaceID := newBranchJoinHTTPServer(t)
	f := seedTaskAssetFile(t, s, workspaceID, "sample", "rollback.txt")

	// Create via the normal path, then invoke the rollback helper the way a
	// binding failure would — and assert full residue removal.
	rec := postCreateTaskWithAssets(t, s, postTaskBody{
		Agent: "pm", Title: "rollback target", Prompt: "do things",
		Assignee: "sample/pm",
		Assets:   []taskCreateAssetBinding{{FileID: f.ID, Role: "reference"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creation failed: %d %s", rec.Code, rec.Body.String())
	}
	taskID := decodeCreatedTaskID(t, rec)
	atts, _ := s.controlDB.ListAssetAttachmentsForTask(taskID)
	if len(atts) != 1 {
		t.Fatalf("precondition: one binding expected, got %d", len(atts))
	}

	task, err := s.ts.GetTask("sample", "pm", taskID)
	if err != nil || task == nil {
		t.Fatalf("precondition: task must exist: %v", err)
	}
	s.rollbackTaskCreate("sample", "pm", task, "sample/pm")

	if got, _, err := s.controlDB.AssetFile(f.ID); err != nil || got == nil {
		t.Fatalf("asset file itself must survive rollback: %v", err)
	}
	if atts2, _ := s.controlDB.ListAssetAttachmentsForTask(taskID); len(atts2) != 0 {
		t.Fatalf("rollback must remove bindings, got %d", len(atts2))
	}
	if _, err := s.ts.GetTask("sample", "pm", taskID); err == nil {
		t.Fatalf("rollback must delete the task record")
	}
}
