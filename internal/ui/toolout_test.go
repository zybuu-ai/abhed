package ui

import (
	"encoding/json"
	"testing"
)

// A tasks call reads as one line: how many start at once and what each is for.
func TestTasksSummary(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[{"description":"review src","prompt":"x"},{"prompt":"write   the tests for pkg/store"}],"isolation":"worktree"}`)
	got := summarizeArgs("tasks", raw)
	if got != "2 in parallel, each in its own worktree: review src · write the tests for pkg/store" {
		t.Fatalf("got %q", got)
	}
	if toolTitle("tasks") != "Tasks" || summarizeArgs("tasks", json.RawMessage(`{}`)) != "" {
		t.Fatal("an empty tasks call has a summary, or the title is wrong")
	}
}
