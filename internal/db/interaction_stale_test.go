package db

import (
	"testing"
	"time"
)

func TestShouldRecoverStaleInteraction(t *testing.T) {
	now := time.Now().UTC()
	stale := func(minsAgo int) InteractionSession {
		ts := now.Add(-time.Duration(minsAgo) * time.Minute).Format(time.RFC3339)
		return InteractionSession{
			ID: "s1", SourceKind: "scheduler", LockReason: "running_task",
			Status: "active", UpdatedAt: ts, LastActivityAt: ts,
		}
	}
	cases := []struct {
		name       string
		active     InteractionSession
		sourceKind string
		reason     string
		want       bool
	}{
		{"live session blocks", stale(0), "scheduler", "running_task", false},
		{"just under window blocks", stale(1), "scheduler", "running_task", false},
		{"beyond window recovers", stale(3), "scheduler", "running_task", true},
		{"fallback to UpdatedAt", func() InteractionSession {
			s := stale(3)
			s.LastActivityAt = ""
			return s
		}(), "scheduler", "running_task", true},
		{"non-scheduler acquirer never recovers", stale(30), "feishu", "running_task", false},
		// H1 closeout (2026-09-27): a manual_run requester is a legitimate
		// same-ladder acquirer — the API's manual-start precheck admits the
		// stale scheduler session, so the spawned `multigent run` must recover
		// it identically or the ok+pid response lies again (the spawned
		// process exited "agent is busy in scheduler session").
		{"manual_run acquirer recovers stale scheduler session", stale(3), "manual_run", "running_task", true},
		{"manual_run acquirer still blocked on live session", stale(0), "manual_run", "running_task", false},
		{"manual_run acquirer with non-task reason never recovers", stale(30), "manual_run", "chat", false},
		{"different lock reason never recovers", stale(30), "scheduler", "chat", false},
		{"active session of different shape never recovers", func() InteractionSession {
			s := stale(30)
			s.SourceKind = "feishu"
			return s
		}(), "scheduler", "running_task", false},
		{"unparseable timestamps never recovers", func() InteractionSession {
			s := stale(30)
			s.LastActivityAt = "not-a-time"
			s.UpdatedAt = "not-a-time"
			return s
		}(), "scheduler", "running_task", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldRecoverStaleInteraction(tc.active, tc.sourceKind, tc.reason); got != tc.want {
				t.Fatalf("ShouldRecoverStaleInteraction = %v, want %v", got, tc.want)
			}
		})
	}
}
