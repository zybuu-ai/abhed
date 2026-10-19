package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/evidence", Args: "[N | #seq]", Help: "the checks on the last answer, or the record behind claim N",
		Group: "status", Order: 174, MidTurn: true, ReadOnly: true, Run: slashEvidence})
}

// evidenceLines bounds how much of one tool result /evidence shows.
const evidenceLines = 40

// slashEvidence is /evidence: with no argument, every claim of the last
// checked answer and its verdict; with N, claim N and the record entries
// behind it; with #seq, that entry of the record.
func slashEvidence(_ context.Context, e *cmdEnv, args []string) (bool, error) {
	if e.st == nil || e.st.store == nil || e.st.sessionID == "" {
		return false, errors.New("there is no session record to read")
	}
	events, err := e.st.store.Events(e.st.sessionID)
	if err != nil {
		return false, fmt.Errorf("could not read the record: %w", err)
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "#") {
		seq, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
		if err != nil {
			return false, fmt.Errorf("%s is not a record entry; give #seq, such as #12", args[0])
		}
		e.ui.Append(ui.Block{Kind: ui.BlockToolOut, Text: recordEntry(events, seq)})
		return false, nil
	}
	v, ok := lastVerdictIn(events)
	if !ok {
		return false, errors.New("no answer has been checked in this session yet; answers are checked when verify is label or strict")
	}
	if len(args) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s (%s)\n", v.Footer(), v.Source)
		for _, c := range v.Claims {
			fmt.Fprintf(&b, "%d %s %s\n    %s\n", c.N, mark(c.Verdict), ui.VisibleLine(c.Text), ui.VisibleLine(c.Reason))
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: strings.TrimRight(b.String(), "\n")})
		return false, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > len(v.Claims) {
		return false, fmt.Errorf("the last answer has %d claim(s); give a number from 1 to %d, or #seq for a record entry", len(v.Claims), max(1, len(v.Claims)))
	}
	c := v.Claims[n-1]
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s %s\n%s: %s\n", c.N, mark(c.Verdict), ui.VisibleLine(c.Text), c.Verdict, ui.VisibleLine(c.Reason))
	if len(c.Cited) > 0 {
		fmt.Fprintf(&b, "cited: %s\n", ui.VisibleLine(strings.Join(c.Cited, ", ")))
	}
	if len(c.Checked) > 0 {
		fmt.Fprintf(&b, "checked: %s\n", ui.VisibleLine(strings.Join(c.Checked, "; ")))
	}
	if c.Critic != "" {
		fmt.Fprintf(&b, "critic: %s\n", ui.VisibleLine(c.Critic))
	}
	seqs := append([]int64(nil), c.Evidence...)
	if c.Conflict != nil && c.Conflict.Seq > 0 {
		seqs = append(seqs, c.Conflict.Seq)
	} else if c.Conflict != nil {
		fmt.Fprintf(&b, "record: %s\n", ui.VisibleLine(c.Conflict.Excerpt))
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: strings.TrimRight(b.String(), "\n")})
	for _, seq := range seqs {
		e.ui.Append(ui.Block{Kind: ui.BlockToolOut, Text: recordEntry(events, seq)})
	}
	return false, nil
}

func mark(verdict string) string {
	switch verdict {
	case agent.ClaimVerified:
		return "✓"
	case agent.ClaimContradicted:
		return "✗"
	}
	return "?"
}

func lastVerdictIn(events []agent.Event) (agent.AnswerVerdict, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == agent.EvAnswerVerdict {
			var v agent.AnswerVerdict
			if json.Unmarshal(events[i].Payload, &v) == nil {
				return v, true
			}
		}
	}
	return agent.AnswerVerdict{}, false
}

// recordEntry shows one entry of the record: a tool result with the call
// that made it, or any other event's payload. The record is already redacted.
func recordEntry(events []agent.Event, seq int64) string {
	calls := map[string]agent.ActionRequested{}
	for _, ev := range events {
		if ev.Type == agent.EvActionRequested {
			var a agent.ActionRequested
			if json.Unmarshal(ev.Payload, &a) == nil {
				calls[a.CallID] = a
			}
		}
		if ev.Seq != seq {
			continue
		}
		if ev.Type == agent.EvObservation {
			var o agent.Observation
			if json.Unmarshal(ev.Payload, &o) != nil {
				break
			}
			head := fmt.Sprintf("#%d %s", seq, o.Tool)
			if a, ok := calls[o.CallID]; ok {
				head += " " + ui.VisibleLine(clipArgs(a.Args))
				if a.Via != "" {
					head += " (" + a.Via + ")"
				}
			}
			if o.ExitCode != nil {
				head += fmt.Sprintf(" · exit %d", *o.ExitCode)
			}
			lines := strings.Split(strings.TrimRight(o.Content, "\n"), "\n")
			more := ""
			if len(lines) > evidenceLines {
				more = fmt.Sprintf("\n… %d more line(s); recall call_id %s for the rest", len(lines)-evidenceLines, o.CallID)
				lines = lines[:evidenceLines]
			}
			return head + "\n" + ui.VisibleOutput(strings.Join(lines, "\n")) + more
		}
		return fmt.Sprintf("#%d %s\n%s", seq, ev.Type, ui.VisibleOutput(string(ev.Payload)))
	}
	return fmt.Sprintf("#%d is not in this session's record", seq)
}

func clipArgs(raw json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(raw, &a)
	s := a.Command
	if s == "" {
		s = a.Path
	}
	if s == "" {
		s = string(raw)
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:119]) + "…"
	}
	return s
}
