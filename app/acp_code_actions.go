package app

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/lsprelay"
)

// Code actions on a range (studio-acp-contract.md §7.13): Explain, Fix and
// Add tests. Each comes back as the prompt Studio sends with session/prompt,
// so it runs as any prompt does, under the session's policy; this method
// runs nothing and reads nothing of the file.

func init() {
	liveFeatures = append(liveFeatures, "codeActions")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/codeActions": (*acpConn).codeActions,
	})
}

// Code action bounds: the editor's diagnostics taken, and each one's text.
const (
	codeActionDiagnostics = 20
	codeActionMessage     = 240
	codeActionMaxLine     = 10_000_000
)

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// codeAction is one action Studio may offer: its prompt is session/prompt's.
type codeAction struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Title       string            `json:"title"`
	IsPreferred bool              `json:"isPreferred,omitempty"`
	Prompt      []json.RawMessage `json:"prompt"`
}

func (c *acpConn) codeActions(msg rpcMessage) {
	var p struct {
		URI         string   `json:"uri"`
		Range       lspRange `json:"range"`
		Diagnostics []struct {
			Range    lspRange `json:"range"`
			Severity int      `json:"severity"`
			Message  string   `json:"message"`
			Source   string   `json:"source"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil || p.URI == "" {
		c.reply(msg.ID, nil, refusal(errParams, "uri and range are required"))
		return
	}
	r := p.Range
	if r.Start.Line < 0 || r.Start.Character < 0 || r.End.Line < r.Start.Line || r.End.Line > codeActionMaxLine ||
		r.End.Line == r.Start.Line && r.End.Character < r.Start.Character {
		c.reply(msg.ID, nil, refusal(errParams, "the range is not a range of the document"))
		return
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if s.readOnly != "" {
		c.reply(msg.ID, nil, refusal(errRecord, "%s; fork it to go on", s.readOnly))
		return
	}
	// A file the agent may not read gets no actions: each would only be refused.
	path, why := sendable(p.URI, s.roots())
	if why == "" && readDenied(parts.Loop.Policy, path) {
		why = "a rule keeps this file from being read"
	}
	if why != "" {
		c.reply(msg.ID, nil, refusal(errRefused, "no code actions here: %s", why))
		return
	}
	rel := parts.Session.Rel(path)
	where := strconv.Quote(rel)
	first, last := r.Start.Line+1, r.End.Line+1
	// A range that ends at the start of a line does not take that line.
	if last > first && r.End.Character == 0 {
		last--
	}
	lines := fmt.Sprintf("line %d of %s", first, where)
	if last > first {
		lines = fmt.Sprintf("lines %d-%d of %s", first, last, where)
	}

	var reported []string
	firstMessage := ""
	for _, d := range p.Diagnostics {
		if len(reported) == codeActionDiagnostics {
			break
		}
		// Only those on the range: the rest are another place's problem.
		if d.Range.End.Line < r.Start.Line || d.Range.Start.Line > r.End.Line {
			continue
		}
		sev := map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}[d.Severity]
		line := fmt.Sprintf("%s:%d:%d: %s: %s", rel, d.Range.Start.Line+1, d.Range.Start.Character+1, orDefault(sev, "error"), clip(d.Message, codeActionMessage))
		if d.Source != "" {
			line += " (" + clip(d.Source, 40) + ")"
		}
		if firstMessage == "" {
			firstMessage = clip(d.Message, 60)
		}
		reported = append(reported, line)
	}
	text := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return b
	}
	// The editor's diagnostics go as an attachment: data for the agent, never the person's words.
	var diagBlock json.RawMessage
	if len(reported) > 0 {
		diagBlock, _ = json.Marshal(map[string]any{"type": "resource", "resource": map[string]string{
			"uri": lsprelay.URI(path) + "#diagnostics", "mimeType": "text/plain",
			"text": "The editor's diagnostics on this range:\n" + strings.Join(reported, "\n")}})
	}

	fixTitle := "Fix with Abhed"
	fixPrompt := "Fix the problem in " + lines + ". Read the file first, make the smallest change that fixes it, and check it the way the project checks its code."
	if len(reported) > 0 {
		fixTitle = "Fix with Abhed: " + firstMessage
		fixPrompt = "Fix the problems the editor reports in " + lines + " (attached). Read the file first, make the smallest change that fixes them, and check it the way the project checks its code."
	}
	explainPrompt := "Explain what " + lines + " does and why, and anything surprising in it. Read the file first. Change nothing."
	if len(reported) > 0 {
		explainPrompt += " Explain the editor's diagnostics on it too (attached)."
	}
	withDiag := func(blocks ...json.RawMessage) []json.RawMessage {
		if diagBlock != nil {
			blocks = append(blocks, diagBlock)
		}
		return blocks
	}
	actions := []codeAction{
		{ID: "explain", Kind: "abhed.explain", Title: "Explain with Abhed", Prompt: withDiag(text(explainPrompt))},
		{ID: "fix", Kind: "quickfix.abhed", Title: fixTitle, IsPreferred: len(reported) > 0, Prompt: withDiag(text(fixPrompt))},
	}
	if !testFile(rel) {
		actions = append(actions, codeAction{ID: "tests", Kind: "source.abhed.addTests", Title: "Add tests with Abhed",
			Prompt: []json.RawMessage{text("Add tests for the code in " + lines + ", where and in the style the project keeps its tests. Read the file and its existing tests first, then run the new tests.")}})
	}
	c.reply(msg.ID, map[string]any{"actions": actions}, nil)
}

// testFile reports whether rel is itself a test, which Add tests is not offered for.
func testFile(rel string) bool {
	base := strings.ToLower(rel[strings.LastIndexAny(rel, `/\`)+1:])
	for _, suffix := range []string{"_test.go", "_test.py", ".test.ts", ".test.tsx", ".test.js", ".test.jsx", ".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")
}

// clip is s on one line, cut to n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
