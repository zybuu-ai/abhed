// Package ui renders the agent's work to a terminal.
//
// The design rules are in docs/architecture/09-ux.md. The ones that shape this
// code: show work as it happens so the user can interrupt early; one line per
// tool call, expanded only when it carries information; diffs before writes.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// ANSI codes, disabled when not writing to a terminal or when NO_COLOR is set.
type Style struct{ enabled bool }

func NewStyle(w io.Writer) Style {
	if os.Getenv("NO_COLOR") != "" {
		return Style{false}
	}
	// A writer that stands in for the terminal answers for itself. Without
	// this, wrapping os.Stdout in anything at all silently turned colour off,
	// because the check could only recognise an *os.File.
	if t, ok := w.(interface{ IsTerminal() bool }); ok {
		return Style{t.IsTerminal()}
	}
	f, isFile := w.(*os.File)
	if !isFile {
		return Style{false}
	}
	info, err := f.Stat()
	if err != nil {
		return Style{false}
	}
	return Style{(info.Mode() & os.ModeCharDevice) != 0}
}

func (s Style) wrap(code, text string) string {
	if !s.enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) Dim(t string) string    { return s.wrap("2", t) }
func (s Style) Bold(t string) string   { return s.wrap("1", t) }
func (s Style) Red(t string) string    { return s.wrap("31", t) }
func (s Style) Green(t string) string  { return s.wrap("32", t) }
func (s Style) Yellow(t string) string { return s.wrap("33", t) }
func (s Style) Blue(t string) string   { return s.wrap("34", t) }
func (s Style) Cyan(t string) string   { return s.wrap("36", t) }

// Reverse swaps foreground and background, which is how a selected row in a
// list reads as selected on every terminal theme — a colour chosen for a dark
// background disappears on a light one.
func (s Style) Reverse(t string) string { return s.wrap("7", t) }

type Renderer struct {
	w     io.Writer
	s     Style
	quiet bool

	// streaming marks a reply in progress; pending holds the partial line the
	// deltas have not finished, and table collects rows until their block ends.
	streaming bool
	pending   strings.Builder
	table     []string

	// think animates while the model works. Every write path stops it first
	// and the turn restarts it, so a frame can never land mid-line and leave
	// a spinner character stranded in the transcript.
	think *Thinking

	// lastReasoning is the most recent reasoning block, kept so /think can
	// print the one the user just saw collapsed. Toggling a flag that only
	// affects the NEXT turn is not what someone means when they ask to see the
	// reasoning in front of them.
	lastReasoning string

	// Reasoning is shown in full when true. Off by default: on a model that
	// reasons at length it buries the answer, and it is the answer the user
	// asked for. /think toggles it, and a summary line always appears so the
	// reasoning is known to exist rather than silently dropped.
	Reasoning bool
}

// flushLines renders every complete line held in the buffer.
//
// A table is the one construct that cannot be formatted a line at a time — its
// columns are only measurable once the widest row has arrived — so a run of
// table rows is held until the block ends and then rendered together.
func (r *Renderer) flushLines(final bool) {
	buf := r.pending.String()
	for {
		i := strings.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		r.emit(buf[:i])
		buf = buf[i+1:]
	}
	r.pending.Reset()
	r.pending.WriteString(buf)
	if final && buf != "" {
		r.emit(buf)
		r.pending.Reset()
	}
	if final {
		r.flushTable()
	}
}

// emit renders one finished line, buffering table rows until the block ends.
func (r *Renderer) emit(line string) {
	if isTableRow(line) || (len(r.table) > 0 && isTableDivider(line)) {
		r.table = append(r.table, line)
		return
	}
	r.flushTable()
	fmt.Fprintln(r.w, Markdown(r.s, line))
}

func (r *Renderer) flushTable() {
	if len(r.table) == 0 {
		return
	}
	fmt.Fprint(r.w, Markdown(r.s, strings.Join(r.table, "\n"))+"\n")
	r.table = nil
}

func (r *Renderer) endStream() {
	r.streaming = false
	r.pending.Reset()
	r.table = nil
}

func NewRenderer(w io.Writer, quiet bool) *Renderer {
	r := &Renderer{w: w, s: NewStyle(w), quiet: quiet}
	r.think = NewThinking(w, r.s)
	return r
}

// StartThinking begins the indicator for a turn. The renderer stops it before
// any output, so a caller only has to start it once per turn.
func (r *Renderer) StartThinking() {
	if !r.quiet {
		r.think.Start()
	}
}

// StopThinking ends it, at the end of a turn or on interrupt.
func (r *Renderer) StopThinking() { r.think.Stop() }

// ShowLastReasoning prints the most recent reasoning block in full, and
// reports whether there was one. This is what /think shows immediately,
// rather than only affecting turns that have not happened yet.
func (r *Renderer) ShowLastReasoning() bool {
	if strings.TrimSpace(r.lastReasoning) == "" {
		return false
	}
	fmt.Fprintf(r.w, "%s %s\n", r.s.Dim("▾"), r.s.Dim("reasoning"))
	for _, line := range strings.Split(r.lastReasoning, "\n") {
		fmt.Fprintf(r.w, "  %s %s\n", r.s.Dim("│"), r.s.Dim(line))
	}
	return true
}

// PauseThinking clears the indicator so a caller can write a line, reporting
// whether it was running so the caller can restart it.
func (r *Renderer) PauseThinking() bool { return r.pause() }

// pause clears the indicator before writing. Returns whether it was running,
// so a caller that wants it back can restart it.
func (r *Renderer) pause() bool {
	if r.think.Active() {
		r.think.Stop()
		return true
	}
	return false
}

func (r *Renderer) Style() Style { return r.s }

// Event renders one event. Tool calls get a single line; failures expand.
func (r *Renderer) Event(ev agent.Event) {
	switch ev.Type {
	case agent.EvAgentDelta:
		// Stream the reply as it arrives. A cold local model can take thirty
		// seconds to its first token; text that appears as it is written makes
		// the same wall time feel responsive, and silence until the end feels
		// like a hang.
		//
		// Formatting is applied a line at a time, as each line completes. The
		// alternative — printing raw and reprinting formatted at the end —
		// needs to erase what it wrote, which stops working the moment the
		// answer is longer than the window and the draft scrolls out of reach.
		// A line is the largest unit that can be formatted without waiting for
		// what comes after it.
		if r.quiet {
			return
		}
		var d agent.Delta
		if json.Unmarshal(ev.Payload, &d) != nil || d.Text == "" {
			return
		}
		if !r.streaming {
			r.pause() // the answer has started; the indicator has done its job
			fmt.Fprint(r.w, "\n")
			r.streaming = true
		}
		r.pending.WriteString(d.Text)
		r.flushLines(false)

	case agent.EvAgentMessage:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil {
			r.endStream()
			return
		}
		if r.streaming {
			// The deltas already showed this. Flush whatever is left of the
			// final line and stop, rather than printing the whole answer twice.
			r.flushLines(true)
			fmt.Fprint(r.w, "\n")
			r.endStream()
			return
		}
		if strings.TrimSpace(m.Text) != "" {
			fmt.Fprintf(r.w, "\n%s\n", Markdown(r.s, m.Text))
		}

	case agent.EvAgentReasoning:
		// The model's thinking. The CLI dropped this entirely while the console
		// showed it, so the same session looked like it reasoned in one place
		// and not the other.
		//
		// Collapsed to a word count by default and expanded by /think: on a
		// model that reasons at length, printing it in full buries the answer
		// the user actually asked for.
		if r.quiet {
			return
		}
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil || strings.TrimSpace(m.Text) == "" {
			return
		}
		r.pause()
		text := strings.TrimSpace(m.Text)
		r.lastReasoning = text
		if !r.Reasoning {
			fmt.Fprintf(r.w, "%s %s\n",
				r.s.Dim("▸"),
				r.s.Dim(fmt.Sprintf("reasoning · %d words · type /think to expand", len(strings.Fields(text)))))
			return
		}
		fmt.Fprintf(r.w, "%s %s\n", r.s.Dim("▾"), r.s.Dim("reasoning"))
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(r.w, "  %s %s\n", r.s.Dim("│"), r.s.Dim(line))
		}

	case agent.EvActionRequested:
		if r.quiet {
			return
		}
		var a agent.ActionRequested
		if json.Unmarshal(ev.Payload, &a) != nil {
			return
		}
		r.pause()
		fmt.Fprintf(r.w, "%s %s %s\n",
			r.s.Cyan("●"), r.s.Bold(a.Tool), r.s.Dim(summarizeArgs(a.Tool, a.Args)))

	case agent.EvObservation:
		var o agent.Observation
		if json.Unmarshal(ev.Payload, &o) != nil {
			return
		}
		r.pause()
		// Errors always show; successful output stays collapsed unless it is
		// the kind of result the user needs to see.
		if o.IsError {
			for _, line := range firstLines(o.Content, 8) {
				fmt.Fprintf(r.w, "  %s %s\n", r.s.Red("│"), line)
			}
			return
		}
		if r.quiet {
			return
		}
		if o.ExitCode != nil && *o.ExitCode != 0 {
			for _, line := range firstLines(o.Content, 12) {
				fmt.Fprintf(r.w, "  %s %s\n", r.s.Yellow("│"), line)
			}
			return
		}
		if summary := observationSummary(o); summary != "" {
			fmt.Fprintf(r.w, "  %s %s\n", r.s.Dim("└"), r.s.Dim(summary))
		}

	case agent.EvActionDenied:
		r.pause()
		var m map[string]string
		if json.Unmarshal(ev.Payload, &m) == nil {
			fmt.Fprintf(r.w, "  %s %s\n", r.s.Red("✕"), r.s.Dim(m["reason"]))
		}

	case agent.EvSessionEnded:
		r.StopThinking()
		var e agent.SessionEnded
		if json.Unmarshal(ev.Payload, &e) != nil || r.quiet {
			return
		}
		if e.Reason != agent.TermCompleted {
			fmt.Fprintf(r.w, "\n%s %s\n", r.s.Yellow("!"), r.s.Dim("ended: "+string(e.Reason)))
		}
	}
}

// summarizeArgs renders the one useful detail per tool, so the line stays
// scannable. Verbosity is the default failure mode of agent CLIs.
func summarizeArgs(tool string, raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	str := func(k string) string {
		if v, found := m[k]; found {
			if s, isStr := v.(string); isStr {
				return s
			}
		}
		return ""
	}
	switch tool {
	case "read", "write", "edit":
		return str("path")
	case "glob":
		return str("pattern")
	case "grep":
		if p := str("path"); p != "" {
			return fmt.Sprintf("%q in %s", str("pattern"), p)
		}
		return fmt.Sprintf("%q", str("pattern"))
	case "bash":
		if d := str("description"); d != "" {
			return d
		}
		return truncate(str("command"), 60)
	case "task":
		return str("description")
	}
	return ""
}

func observationSummary(o agent.Observation) string {
	switch o.Tool {
	case "glob", "grep":
		n := strings.Count(strings.TrimSpace(o.Content), "\n") + 1
		if strings.HasPrefix(o.Content, "[no files") || strings.HasPrefix(o.Content, "No matches") {
			return "no results"
		}
		return fmt.Sprintf("%d line(s)", n)
	case "read":
		return fmt.Sprintf("%d line(s)", strings.Count(o.Content, "\n"))
	case "edit", "write":
		return firstLine(o.Content)
	case "bash":
		if o.ExitCode != nil {
			return fmt.Sprintf("exit %d", *o.ExitCode)
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("... %d more lines", len(lines)-n))
	}
	return lines
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
