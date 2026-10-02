package api

import "testing"

func TestReviewFindingSeverityMapping(t *testing.T) {
	cases := map[string]int{
		"P0": 0, "p1": 1, "P2": 2, "p3": 3,
		"high": 0, "High": 0, "critical": 0,
		"medium": 1, "major": 1,
		"low": 3, "minor": 3,
		"": 2, "unknown": 2, "p9": 2,
	}
	for in, want := range cases {
		if got := reviewFindingSeverity(in); got != want {
			t.Errorf("reviewFindingSeverity(%q) = %d, want %d", in, got, want)
		}
	}
}
