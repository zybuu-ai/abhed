package agent

import (
	"strings"
	"testing"
)

// #22: the relevance check flags only the plain case, and never on a
// question with nothing to hold an answer to.
func TestOffTopic(t *testing.T) {
	for _, c := range []struct {
		q, a string
		off  bool
	}{
		// The r2 monorepo misread: the answer explained something else.
		{`where is 'Describe what to build' defined, and which file registers the Abhed chat view container?`,
			"A new chat session type is selected in chatModes.ts by the mode picker, which reads the session's kind.", true},
		{`where is 'Describe what to build' defined?`,
			"It is the placeholder in src/vs/chat/chatModes.ts:716: `Describe what to build`.", false},
		{"what does src/utils/text.py do?", "text.py provides Normalize and chunk.", false},
		{"why does TestFooBar fail?", "The weather is fine today.", true},
		{"summarise the retrieval pipeline and its ranking stages", "Retrieval runs in two stages: BM25, then a reranker.", false},
		{"summarise the retrieval pipeline and its ranking stages", "Hello! How can I help you today?", true},
		{"say hi", "Hi!", false},                // too few words to judge
		{"/explain foo.py", "Anything.", false}, // a command's text is not the question
		{"fix it", "", false},
		// The real-model check: a question that names a tool is about the
		// work, not the tool; the answer gave the uptime.
		{"Call the ssh_connect tool to connect to app1 first, then show me the uptime of app1.",
			"app1 has been up for 3 days, 4 hours, with a load average of 0.12.", false},
		{"Call the ssh_connect tool to connect to app1 first, then show me the uptime of app1.",
			"Hello! How can I help you today?", true},
	} {
		why := offTopic(c.q, c.a, "ssh", "ssh_connect", "read", "grep")
		if (why != "") != c.off {
			t.Errorf("%q / %q: %q", c.q, c.a, why)
		}
	}
}

// The verdict carries the note and the footer says so; the outcome is not
// touched by it.
func TestRelevanceIsOnlyANote(t *testing.T) {
	v := AnswerVerdict{Verified: 1, Outcome: ClaimVerified, Relevance: RelevanceOffTopic}
	if !strings.Contains(v.Footer(), "may not address the question") {
		t.Fatal(v.Footer())
	}
}
