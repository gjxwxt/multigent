package entity

import (
	"encoding/json"
	"testing"
)

func TestTaskUnmarshalJSONTreatsEmptyOptionalTimesAsNil(t *testing.T) {
	raw := []byte(`{"ID":"t-legacy","CreatedAt":"2026-09-09T15:58:54Z","UpdatedAt":"2026-09-09T16:00:00Z","StartedAt":"","FinishedAt":"","ArchivedAt":"","DueDate":"","NotBefore":""}`)
	var task Task
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("unmarshal legacy task: %v", err)
	}
	if task.ID != "t-legacy" {
		t.Fatalf("task ID = %q, want t-legacy", task.ID)
	}
	if task.StartedAt != nil || task.FinishedAt != nil || task.ArchivedAt != nil || task.DueDate != nil || task.NotBefore != nil {
		t.Fatalf("empty optional timestamps should decode as nil: %+v", task)
	}
}

func TestTaskUnmarshalJSONPreservesOptionalTimes(t *testing.T) {
	raw := []byte(`{"ID":"t-current","CreatedAt":"2026-09-09T15:58:54Z","UpdatedAt":"2026-09-09T16:00:00Z","StartedAt":"2026-09-09T16:01:00Z","FinishedAt":null}`)
	var task Task
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("unmarshal current task: %v", err)
	}
	if task.StartedAt == nil || task.StartedAt.IsZero() {
		t.Fatalf("started timestamp was not preserved: %+v", task.StartedAt)
	}
	if task.FinishedAt != nil {
		t.Fatalf("null finished timestamp should remain nil: %+v", task.FinishedAt)
	}
}
