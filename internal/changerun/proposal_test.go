package changerun

import (
	"encoding/json"
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

// Round-19 P1: slot and proposal rows are written in ONE guarded
// transaction. Three properties are pinned:
//  1. concurrent Creates still serialize (exactly one winner);
//  2. a ghost slot (holder missing or terminal) is repaired on sight — the
//     next Create reclaims it instead of erroring forever;
//  3. terminal transitions clear the slot atomically with the state change
//     (a terminal proposal never coexists with an occupied slot).
func TestConcurrentCreatesExactlyOneWinsTransactionally(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	type result struct {
		id  string
		err string
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := s.Create("proj", "task-tx", "agent", "req", "patch", "diff", nil)
			if err != nil {
				results <- result{err: err.Error()}
				return
			}
			results <- result{id: p.ID}
		}(i)
	}
	wg.Wait()
	close(results)
	var winners, losers int
	for r := range results {
		if r.id != "" {
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != n-1 {
		t.Fatalf("exactly one create must win, got %d winners / %d losers", winners, losers)
	}
}

func TestGhostSlotIsRepairedOnSight(t *testing.T) {
	s := newTestStore(t)
	// Forge a ghost slot: holder proposal row does not exist.
	p1, err := s.Create("proj", "task-ghost", "agent", "req", "patch", "diff", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate legacy crash debris: point the slot at a nonexistent proposal
	// AND remove the holder row entirely (the crash lost the proposal write).
	if err := s.db.DeleteRecord(proposalTable, s.workspace, proposalKey(p1.ID)); err != nil {
		t.Fatal(err)
	}
	ghost := taskSlot{ProposalID: "crp-doesnotexist", UpdatedAt: "now"}
	raw, _ := json.Marshal(ghost)
	if err := s.db.UpsertRecord(proposalTable, s.workspace, taskSlotKey("proj", "task-ghost"), string(raw)); err != nil {
		t.Fatal(err)
	}
	p2, err := s.Create("proj", "task-ghost", "agent", "req2", "patch2", "diff2", nil)
	if err != nil {
		t.Fatalf("ghost slot must be reclaimed by the next create, got: %v", err)
	}
	if p2.ID == p1.ID {
		t.Fatal("reclaimed create must mint a new proposal")
	}
	// Also repair when the holder exists but is TERMINAL.
	if _, err := s.Reject(p2.ID, "op"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// After a terminal transition the slot must already be free — create again.
	if _, err := s.Create("proj", "task-ghost", "agent", "req3", "patch3", "diff3", nil); err != nil {
		t.Fatalf("post-terminal create must succeed, got: %v", err)
	}
}

func TestTerminalTransitionClearsSlotAtomically(t *testing.T) {
	s := newTestStore(t)
	p, err := s.Create("proj", "task-term", "agent", "req", "patch", "diff", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Reject(p.ID, "op"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// The slot must read EMPTY right after the terminal transition.
	recs, err := s.db.ListRecordsWithRevision(proposalTable, s.workspace, taskSlotKey("proj", "task-term"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("slot row missing: %v", err)
	}
	var held taskSlot
	if err := json.Unmarshal([]byte(recs[0].Payload), &held); err != nil {
		t.Fatal(err)
	}
	if held.ProposalID != "" {
		t.Fatalf("terminal proposal must not keep holding the slot, holder=%s", held.ProposalID)
	}
}
