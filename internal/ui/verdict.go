package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// maxShownClaims bounds the claims drawn under an answer; /evidence lists all.
const maxShownClaims = 6

// ClaimMark is a claim's verdict as one character: ✓ verified, ? unverified,
// ✗ contradicted.
func ClaimMark(s Style, verdict string) string {
	switch verdict {
	case agent.ClaimVerified:
		return s.Green("✓")
	case agent.ClaimContradicted:
		return s.Red("✗")
	}
	return s.Yellow("?")
}

// VerdictRows are the lines drawn under a checked answer: the footer, then
// each contradicted claim struck through with the evidence against it, then
// the unverified claims about the session's work.
// hint adds where to look further, for a terminal that has /evidence. An
// answer with nothing in it to check draws nothing.
func VerdictRows(s Style, v agent.AnswerVerdict, hint bool) []string {
	if len(v.Claims) == 0 && v.Contract == nil && v.Revision == "" {
		return nil
	}
	var rows []string
	// The reply streamed before it was checked: a contradiction is said in a
	// line no one can read past, before anything else.
	if v.Contradicted > 0 {
		what := fmt.Sprintf("%d statements in the reply above are", v.Contradicted)
		if v.Contradicted == 1 {
			what = "1 statement in the reply above is"
		}
		if v.Revision != "" {
			rows = append(rows, "  "+s.Bold(s.Red("✗ NOT FACT: "+what+" contradicted by this session's record. The agent is asked to correct it.")))
		} else {
			rows = append(rows, "  "+s.Bold(s.Red("✗ NOT FACT: "+what+" contradicted by this session's record; struck through below.")))
		}
	} else if v.Revision != "" {
		rows = append(rows, "  "+s.Yellow("↻ ")+s.Dim("Some of this could not be confirmed from the session's record; the agent is asked to revise it once."))
	}
	mark := s.Green("✓")
	switch {
	case v.Contradicted > 0 || (v.Contract != nil && !v.Contract.OK):
		mark = s.Red("✗")
	case v.Unverified > v.Unchecked():
		mark = s.Yellow("?")
	}
	foot := VisibleLine(v.Footer())
	if hint {
		foot += " · /evidence N shows the record behind claim N"
	}
	rows = append(rows, "  "+mark+" "+s.Dim(foot))
	shown := 0
	for _, c := range v.Claims {
		if c.Verdict != agent.ClaimContradicted || shown >= maxShownClaims {
			continue
		}
		shown++
		why := c.Reason
		if c.Conflict != nil && c.Conflict.Excerpt != "" {
			at := ""
			if c.Conflict.Seq > 0 {
				at = fmt.Sprintf("#%d: ", c.Conflict.Seq)
			}
			why = at + c.Conflict.Excerpt
		}
		rows = append(rows, fmt.Sprintf("    %s %d %s", s.Red("✗"), c.N, s.Strike(VisibleLine(clip(c.Text, 140)))),
			"      "+s.Dim("record: "+VisibleLine(clip(why, 160))))
	}
	for _, c := range v.Claims {
		if c.Verdict != agent.ClaimUnverified || !c.LoadBearing || shown >= maxShownClaims {
			continue
		}
		shown++
		rows = append(rows, fmt.Sprintf("    %s %d %s", s.Yellow("?"), c.N, s.Dim(VisibleLine(clip(c.Text, 100))+" — "+VisibleLine(clip(c.Reason, 120)))))
	}
	if c := v.Contract; c != nil && !c.OK {
		for _, r := range c.Require {
			if !r.OK {
				rows = append(rows, "    "+s.Red("✗")+" "+s.Dim("required `"+VisibleLine(clip(r.Command, 60))+"`: "+VisibleLine(clip(r.Reason, 120))))
			}
		}
		if len(c.Protected) > 0 {
			rows = append(rows, "    "+s.Red("✗")+" "+s.Dim("protected files changed: "+VisibleLine(clip(strings.Join(c.Protected, ", "), 160))))
		}
	}
	return rows
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// verdictOf reads an answer.verdict event.
func verdictOf(ev agent.Event) (agent.AnswerVerdict, bool) {
	var v agent.AnswerVerdict
	if ev.Type != agent.EvAnswerVerdict || json.Unmarshal(ev.Payload, &v) != nil {
		return v, false
	}
	return v, true
}

// LastVerdict is the latest answer.verdict drawn, for /evidence.
func (r *Renderer) LastVerdict() (agent.AnswerVerdict, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verdict == nil {
		return agent.AnswerVerdict{}, false
	}
	return *r.verdict, true
}

// hiddenCall reports a call the transcript does not draw: the answer tool,
// whose text is drawn as the reply.
func hiddenCall(tool string) bool { return tool == agent.AnswerToolName }
