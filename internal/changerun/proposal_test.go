package changerun

import (
	"strings"
	"sync"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := controldb.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatalf("open controldb: %v", err)
	}
	if err := db.UpsertWorkspace(controldb.Workspace{ID: "ws-cr", Name: "CR"}); err != nil {
		t.Fatalf("upsert workspace: %v", err)
	}
	return NewStore(db, "ws-cr")
}

func TestCreateRejectsSecondActiveProposal(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("proj", "task-1", "agent-a", "fix bug", "patch", "diff", []string{"a.go"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.Create("proj", "task-1", "agent-b", "fix again", "patch2", "diff2", nil)
	if err == nil || !strings.Contains(err.Error(), "already has an active proposal") {
		t.Fatalf("second active proposal must be rejected, got: %v", err)
	}
	// A different task is unaffected.
	if _, err := s.Create("proj", "task-2", "agent-a", "fix bug", "patch", "diff", nil); err != nil {
		t.Fatalf("second task create: %v", err)
	}
}

func TestCASRejectsConcurrentDoubleTransition(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("proj", "task-1", "agent-a", "fix bug", "patch", "diff", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var wg sync.WaitGroup
	wins := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.BeginApply(p.ID); err != nil {
				wins[i] = "lost: " + err.Error()
			} else {
				wins[i] = "won"
			}
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, w := range wins {
		if w == "won" {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one BeginApply must win, got %d: %v", winners, wins)
	}
}

func TestStateTransitionsFullLifecycle(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("proj", "task-1", "agent-a", "fix bug", "patch", "diff", []string{"server/x.go"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.State != StateAwaitingApproval {
		t.Fatalf("initial state: %s", p.State)
	}
	if _, err := s.BeginApply(p.ID); err != nil {
		t.Fatalf("begin apply: %v", err)
	}
	got, err := s.MarkApplied(p.ID, map[string]string{"server/x.go": "abc123"}, "deadbeef")
	if err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	if got.State != StateApplied || got.AppliedSHA != "deadbeef" || got.Postimage["server/x.go"] != "abc123" {
		t.Fatalf("applied state wrong: %+v", got)
	}
	// Applied proposals no longer block new ones.
	if _, err := s.Create("proj", "task-1", "agent-a", "again", "p", "d", nil); err != nil {
		t.Fatalf("create after applied: %v", err)
	}
}

func TestRejectOnlyFromAwaitingApproval(t *testing.T) {
	s := newTestStore(t)
	p, _ := s.Create("proj", "task-1", "agent-a", "r", "p", "d", nil)
	if _, err := s.BeginApply(p.ID); err != nil {
		t.Fatalf("begin apply: %v", err)
	}
	if _, err := s.Reject(p.ID, "human"); err == nil {
		t.Fatal("reject from applying state must fail")
	}
}

func TestRedactionBeforePersist(t *testing.T) {
	s := newTestStore(t)
	request := "config:\n" +
		"  api_key: sk-proj-abcdefgh12345678\n" +
		"  GITHUB_TOKEN=ghp_" + strings.Repeat("x", 30) + "\n" +
		"  Authorization: Bearer eyJhbGciOi.eyJzdWIi.SflKxwRJ\n" +
		"  password: hunter2hunter2\n" +
		"  normal_line: stays\n"
	p, err := s.Create("proj", "task-1", "agent-a", request, "", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.Get(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, secret := range []string{
		"sk-proj-abcdefgh12345678",
		"ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		"eyJhbGciOi.eyJzdWIi.SflKxwRJ",
		"hunter2hunter2",
	} {
		if strings.Contains(got.Request, secret) {
			t.Fatalf("secret leaked into persisted record: %s", secret)
		}
	}
	if !strings.Contains(got.Request, "normal_line: stays") {
		t.Fatalf("non-secret content must survive: %q", got.Request)
	}
	if !strings.Contains(got.Request, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %q", got.Request)
	}
}

func TestSizeCapTruncates(t *testing.T) {
	s := newTestStore(t)
	big := strings.Repeat("a", MaxPatchBytes+100)
	p, err := s.Create("proj", "task-1", "agent-a", "r", big, "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !p.PatchCapped {
		t.Fatal("patch must be flagged capped")
	}
	if len(p.Patch) > MaxPatchBytes+64 {
		t.Fatalf("patch not truncated: %d bytes", len(p.Patch))
	}
	if !strings.HasSuffix(p.Patch, "[truncated]") {
		t.Fatalf("truncation marker missing: %q", p.Patch[len(p.Patch)-30:])
	}
}
