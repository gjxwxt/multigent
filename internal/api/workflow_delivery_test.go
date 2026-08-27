package api

import (
	"testing"

	"github.com/multigent/multigent/internal/entity"
)

func TestUpdateTaskRemoteMR(t *testing.T) {
	task := &entity.Task{}
	if !updateTaskRemoteMR(task, map[string]string{
		"pr_url":    "https://gitlab.example.test/team/app/-/merge_requests/12",
		"pr_number": "12",
		"head_sha":  "abc123",
	}) {
		t.Fatal("expected remote MR state to change")
	}
	if task.RemoteMRIID != "12" || task.RemoteMRURL == "" || task.RemoteMRHeadSHA != "abc123" || task.RemoteMRState != "opened" {
		t.Fatalf("unexpected remote MR state: %+v", task)
	}
}

func TestUpdateTaskRemoteMRIgnoresLocalFallback(t *testing.T) {
	task := &entity.Task{}
	if updateTaskRemoteMR(task, map[string]string{
		"pr_url":    "branch: feature/task-1",
		"pr_number": "task-1",
	}) {
		t.Fatal("local branch fallback must not be recorded as a remote MR")
	}
	if task.RemoteMRIID != "" || task.RemoteMRURL != "" || task.RemoteMRState != "" {
		t.Fatalf("local fallback polluted remote MR state: %+v", task)
	}
}
