package hawkeye

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// The editor's completions and next edits show their counts and answer
// times, renames finished from the file and those left to the person.
func TestEditorStatsShown(t *testing.T) {
	r := Report{Editor: []EditorStats{{Seq: 9, Feature: "next_edit", CompletionSummary: agent.CompletionSummary{Model: "coder",
		Requests: 3, Served: 6, Local: 4, LocalDropped: 1, LatencyP50Ms: 120, LatencyP90Ms: 300, LatencyMaxMs: 400}}}}
	text := Text(r)
	for _, want := range []string{"editor    #9 next_edit coder: 3 sent, 6 served", "4 from the file", "1 rename(s) left to the person", "120 ms median"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	html, err := HTML(r)
	if err != nil || !strings.Contains(html, "3 · 6 · 4 · 1") || !strings.Contains(html, "120 ms · 300 ms · 400 ms") {
		t.Fatalf("html: %v", err)
	}
}
