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
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Renderer draws the session's events. Attached to the input dock it streams
// replies a fragment at a time and keeps a transcript; otherwise — piped
// output, -p — it writes finished lines to w.
type Renderer struct {
	w     io.Writer
	s     Style
	quiet bool

	// dock, when attached, is where everything is drawn.
	dock *dock

	mu sync.Mutex

	// Line mode: the partial line the deltas have not finished.
	streaming bool
	pending   strings.Builder
	lineSt    mdState

	// Dock mode: the reply streaming now, its transcript block, how many
	// of its rows are committed, and finished rows held in the dock until
	// there are enough to commit together.
	ms    *mdStream
	sb    *streamBlock
	shown int
	held  []string

	// lastReasoning is the most recent reasoning block, kept so /think can
	// print the one the user just saw collapsed.
	lastReasoning string

	// Reasoning is shown in full when true. Off by default: on a model that
	// reasons at length it buries the answer. /think toggles it, and a
	// summary line always appears so the reasoning is known to exist.
	Reasoning bool

	// tools tracks the calls in flight, so a result is drawn under its
	// request and a diff is shown once.
	tools toolState

	// todos is the task list as last recorded, for the dock and /todos;
	// agentTypes names each subagent by session, for its return line;
	// lastReply is the latest reply's text, for /copy.
	todos      []agent.Todo
	agentTypes map[string]string
	lastReply  string
	// lastReplyCut is set when cleaning took something out of it.
	lastReplyCut bool

	// usage is what the footer shows about the model and the session. It
	// has its own lock: the dock reads it while drawing, holding the dock's
	// lock, and the renderer takes the dock's lock while holding its own.
	usageMu sync.Mutex
	usage   Usage
}

// Usage is what the renderer has seen of the model's accounting.
type Usage struct {
	Model         string
	ContextTokens int
	ContextWindow int
	TokensIn      int
	TokensOut     int
	TokensCached  int
}

// ContextPct is how full the window was at the last call, or -1.
func (u Usage) ContextPct() int {
	if u.ContextWindow <= 0 || u.ContextTokens <= 0 {
		return -1
	}
	return min(100, u.ContextTokens*100/u.ContextWindow)
}

func NewRenderer(w io.Writer, quiet bool) *Renderer {
	return &Renderer{w: w, s: NewStyle(w), quiet: quiet}
}

// Attach draws through the dock of l, when l has one: replies stream into
// it and everything becomes part of its transcript.
func (r *Renderer) Attach(l *LineReader) {
	if l != nil && l.raw && !r.quiet {
		r.mu.Lock()
		r.dock = l.d
		r.s = l.d.st
		r.mu.Unlock()
	}
}

// Usage returns what the renderer has seen of the model's accounting.
func (r *Renderer) Usage() Usage {
	r.usageMu.Lock()
	defer r.usageMu.Unlock()
	return r.usage
}

// StartThinking shows the activity line for a turn.
func (r *Renderer) StartThinking() {
	if d := r.dock; d != nil {
		d.mu.Lock()
		if !d.act.on {
			d.act = activity{on: true, since: d.now(), verb: int(d.now().Unix()) % len(thinkingVerbs)}
		}
		d.draw()
		d.mu.Unlock()
	}
}

// StopThinking ends it, at the end of a turn or on interrupt.
func (r *Renderer) StopThinking() {
	if d := r.dock; d != nil {
		d.mu.Lock()
		d.act = activity{}
		d.draw()
		d.mu.Unlock()
	}
}

// PauseThinking reports whether the activity line is on. It never has to be
// paused for output: it lives in the dock, not on the line being written.
func (r *Renderer) PauseThinking() bool {
	if d := r.dock; d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.act.on
	}
	return false
}

// ShowLastReasoning prints the most recent reasoning block in full, and
// reports whether there was one.
func (r *Renderer) ShowLastReasoning() bool {
	r.mu.Lock()
	text := r.lastReasoning
	r.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return false
	}
	if d := r.dock; d != nil {
		d.mu.Lock()
		d.commitItem(&reasoningBlock{text: text, open: true})
		d.mu.Unlock()
		return true
	}
	fmt.Fprintf(r.w, "%s %s\n", r.s.Dim("▾"), r.s.Dim("reasoning"))
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(r.w, "  %s %s\n", r.s.Dim("│"), r.s.Dim(line))
	}
	return true
}

func (r *Renderer) Style() Style { return r.s }

// Event renders one event.
func (r *Renderer) Event(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Type {
	case agent.EvModelCall:
		var c agent.ModelCall
		if json.Unmarshal(ev.Payload, &c) == nil && c.Error == "" {
			r.usageMu.Lock()
			defer r.usageMu.Unlock()
			// A call outside the conversation costs tokens but says nothing of its context.
			if c.Model != "" && c.Purpose == "" {
				r.usage.Model = c.Model
			}
			if c.Purpose == "" {
				r.usage.ContextTokens = c.TokensIn
			}
			if c.ContextWindow > 0 {
				r.usage.ContextWindow = c.ContextWindow
			}
			r.usage.TokensIn += c.TokensIn
			r.usage.TokensOut += c.TokensOut
			r.usage.TokensCached += c.TokensCached
		}
		return
	case agent.EvModelSwitched:
		var m agent.ModelSwitched
		if json.Unmarshal(ev.Payload, &m) == nil && m.Model != "" {
			r.usageMu.Lock()
			r.usage.Model = m.Model
			r.usageMu.Unlock()
		}
	case agent.EvAgentMessage:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) == nil && strings.TrimSpace(m.Text) != "" {
			r.lastReply = sanitize(m.Text, false)
			r.lastReplyCut = r.lastReply != m.Text
		}
	case agent.EvSessionEnded:
		var e agent.SessionEnded
		if json.Unmarshal(ev.Payload, &e) == nil && e.ContextTokens > 0 {
			r.usageMu.Lock()
			r.usage.ContextTokens = e.ContextTokens
			if e.ContextWindow > 0 {
				r.usage.ContextWindow = e.ContextWindow
			}
			r.usageMu.Unlock()
		}
	}
	if r.dock != nil {
		r.dockEvent(ev)
		return
	}
	r.lineEvent(ev)
}

// dockEvent draws an event through the dock. r.mu is held.
func (r *Renderer) dockEvent(ev agent.Event) {
	d := r.dock
	d.mu.Lock()
	defer d.mu.Unlock()
	s := r.s
	switch ev.Type {
	case agent.EvAgentDelta:
		var dl agent.Delta
		if json.Unmarshal(ev.Payload, &dl) != nil || dl.Text == "" {
			return
		}
		r.feed(d, sanitize(dl.Text, false))

	case agent.EvAgentMessage:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil {
			r.endReply(d, "")
			return
		}
		r.endReply(d, sanitize(m.Text, false))

	case agent.EvAgentReasoningDelta:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) == nil {
			d.act.label = "Thinking"
			d.act.tokens += estimateTokens(m.Text)
			d.draw()
		}

	case agent.EvAgentReasoning:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil || strings.TrimSpace(m.Text) == "" {
			return
		}
		r.endReply(d, "")
		text := sanitize(strings.TrimSpace(m.Text), false)
		r.lastReasoning = text
		d.act.label = ""
		d.commitItem(&reasoningBlock{text: text, open: r.Reasoning})

	case agent.EvUserMessage:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil || m.QueueID == "" {
			return // the prompt itself was drawn where it was typed
		}
		// A message typed during the turn, now taken by the agent: it leaves
		// the queue and joins the transcript where it applies.
		r.endReply(d, "")
		d.dequeue(m.Text)
		d.commitItem(&promptBlock{text: sanitize(m.Text, false), prompt: d.promptText()})

	case agent.EvActionRequested:
		var a agent.ActionRequested
		if json.Unmarshal(ev.Payload, &a) != nil {
			return
		}
		r.endReply(d, "")
		r.toolRequested(d, a)

	case agent.EvObservation:
		var o agent.Observation
		if json.Unmarshal(ev.Payload, &o) != nil {
			return
		}
		r.toolObserved(d, o)

	case agent.EvActionDenied:
		var m map[string]string
		if json.Unmarshal(ev.Payload, &m) == nil {
			r.toolDenied(d, m)
		}

	case agent.EvForked:
		var f agent.Forked
		if json.Unmarshal(ev.Payload, &f) == nil {
			r.endReply(d, "")
			d.commitItem(&rawBlock{text: s.Dim(fmt.Sprintf("── forked at step %d; the steps after it, above, were abandoned ──", f.ThroughSeq))})
		}

	case agent.EvSubagentNotice:
		var n agent.Notice
		if json.Unmarshal(ev.Payload, &n) != nil {
			return
		}
		r.endReply(d, "")
		d.commit(&rawBlock{text: "  " + s.Yellow("◆") + " " + s.Dim(noticeText(n))})

	case agent.EvSessionWoken:
		d.commit(&rawBlock{text: "  " + s.Yellow("◆") + " " + s.Dim("woke to act on background results")})

	case agent.EvTodoUpdated:
		if b := r.todoEvent(ev); b != nil {
			r.endReply(d, "")
			d.todos = r.todos
			d.commit(b)
		}

	case agent.EvSuggestionOffered:
		var p agent.SuggestionOffered
		if json.Unmarshal(ev.Payload, &p) == nil {
			d.offerNext(p.Text)
		}

	case agent.EvSessionEnded:
		var e agent.SessionEnded
		if json.Unmarshal(ev.Payload, &e) != nil {
			return
		}
		r.endReply(d, "")
		if e.Settled {
			d.commit(&rawBlock{text: "  " + s.Yellow("◆") + " " + s.Dim("background work finished")})
			return
		}
		if e.Background > 0 {
			d.commit(&rawBlock{text: "  " + s.Dim(fmt.Sprintf("%d background task(s) still running; /tasks lists them", e.Background))})
		}
		if why := endedText(e.Reason, e.Detail); why != "" {
			d.commit(&rawBlock{text: "  " + s.Yellow("⎿ ") + why})
		}

	default:
		if rows := r.activityRows(ev); rows != nil {
			r.endReply(d, "")
			d.commit(&rowsBlock{rows: rows})
		}
	}
}

// feed streams a fragment of the reply.
func (r *Renderer) feed(d *dock, text string) {
	if r.ms == nil {
		r.ms = &mdStream{}
		r.sb = &streamBlock{}
		r.shown = 0
		d.streaming = true
		d.act.label = ""
		d.separate()
	}
	d.act.tokens += estimateTokens(text)
	final, live := r.ms.feed(r.s, text, d.streamWidth())
	// Finished rows wait in the dock, where drawing one more is a few bytes,
	// and are committed a batch at a time: each commit redraws the dock
	// under them, so committing row by row cost more than the text.
	r.held = append(r.held, final...)
	var commit []string
	if len(r.held)+len(live) > d.liveRows() {
		commit, r.held = r.held, nil
	}
	d.streamRows(r.sb, r.leadFrom(commit, r.shown), r.leadFrom(append(append([]string(nil), r.held...), live...), r.shown+len(commit)))
	r.shown += len(commit)
}

// endReply finishes the reply streaming now, if any: its last rows are
// committed, and its transcript block keeps the source so a resize can lay
// it out again. With no stream, text is the whole reply, drawn at once.
func (r *Renderer) endReply(d *dock, text string) {
	if r.ms == nil {
		if strings.TrimSpace(text) != "" {
			d.commitItem(&mdBlock{src: text, lead: r.s.Accent("● ")})
		}
		return
	}
	rest := append(append([]string(nil), r.held...), r.ms.end(r.s, d.streamWidth())...)
	r.held = nil
	d.streamRows(r.sb, r.leadFrom(rest, r.shown), nil)
	src := text
	if src == "" {
		src = r.ms.text()
	}
	d.tr.replace(r.sb, &mdBlock{src: src, lead: r.s.Accent("● ")})
	r.ms, r.sb, r.shown, r.held = nil, nil, 0, nil
	d.streaming = false
}

// leadFrom indents rows of the reply, the reply's first row marked.
func (r *Renderer) leadFrom(rows []string, first int) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		switch {
		case first+i == 0:
			out[i] = r.s.Accent("● ") + row
		case row == "":
			out[i] = ""
		default:
			out[i] = "  " + row
		}
	}
	return out
}

// estimateTokens is the activity line's running count: about four
// characters a token, and at least one a fragment.
func estimateTokens(s string) int { return max(1, len(s)/4) }

func noticeText(n agent.Notice) string {
	turns := ""
	if n.Turns > 0 {
		turns = fmt.Sprintf(", %d turn%s", n.Turns, map[bool]string{true: "", false: "s"}[n.Turns == 1])
	}
	return fmt.Sprintf("result of %q added to the conversation (%s%s)",
		sanitize(orStr(n.Description, n.TaskID), false), sanitize(n.Status, false), turns)
}

// endedText says, in words, why a turn ended when it was not by finishing.
func endedText(reason agent.TerminalReason, detail string) string {
	switch reason {
	case agent.TermCompleted, agent.TermWakeLimit, "":
		return ""
	case agent.TermUserInterrupt:
		switch detail {
		case agent.InterruptKept:
			return "Interrupted · background shells kept · tell Abhed what to do instead"
		case agent.InterruptStopped:
			return "Interrupted · background shells stopped · tell Abhed what to do instead"
		}
		return "Interrupted · tell Abhed what to do instead"
	case agent.TermMaxTurns:
		return "Stopped at the turn limit (max_turns)"
	case agent.TermMaxBudget:
		return "Stopped at the token budget (max_budget)"
	case agent.TermRetryExhausted:
		return "The model kept failing; gave up after retrying (retry_exhausted)"
	case agent.TermStalled:
		return "The model produced nothing, repeatedly (stalled)"
	}
	return "ended: " + string(reason)
}

// reasoningBlock is the model's thinking: one line, or the whole of it when
// opened with /think or in the Ctrl-O view.
type reasoningBlock struct {
	text string
	open bool
}

func (b *reasoningBlock) lines(width int, s Style, expanded bool) []string {
	n := len(strings.Fields(b.text))
	if !b.open && !expanded {
		return []string{s.Dim(fmt.Sprintf("✻ Thought · %d words · ctrl+o to expand", n))}
	}
	out := []string{s.Dim("✻ Thinking")}
	for _, l := range strings.Split(b.text, "\n") {
		for _, row := range wrapWords(l, max(width-4, 10)) {
			out = append(out, "  "+s.Dim(s.Italic(row)))
		}
	}
	return out
}

// streamBlock is a reply's rows while it streams; the finished reply
// replaces it in the transcript with its source.
type streamBlock struct{ rows []string }

func (b *streamBlock) lines(width int, _ Style, _ bool) []string {
	var out []string
	for _, r := range b.rows {
		out = append(out, hardWrap(r, width)...)
	}
	return out
}

// lineEvent is the event drawn as plain lines, for output that is not the
// dock.
func (r *Renderer) lineEvent(ev agent.Event) {
	s := r.s
	switch ev.Type {
	case agent.EvAgentDelta:
		if r.quiet {
			return
		}
		var dl agent.Delta
		if json.Unmarshal(ev.Payload, &dl) != nil || dl.Text == "" {
			return
		}
		if !r.streaming {
			fmt.Fprint(r.w, "\n")
			r.streaming = true
			r.lineSt = mdState{}
		}
		r.pending.WriteString(sanitize(dl.Text, false))
		r.flushLines(false)

	case agent.EvAgentMessage:
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil {
			r.endStream()
			return
		}
		if r.streaming {
			r.flushLines(true)
			fmt.Fprint(r.w, "\n")
			r.endStream()
			return
		}
		if strings.TrimSpace(m.Text) != "" {
			fmt.Fprintf(r.w, "\n%s\n", Markdown(r.s, sanitize(m.Text, false)))
		}

	case agent.EvAgentReasoning:
		if r.quiet {
			return
		}
		var m agent.Message
		if json.Unmarshal(ev.Payload, &m) != nil || strings.TrimSpace(m.Text) == "" {
			return
		}
		text := strings.TrimSpace(sanitize(m.Text, false))
		r.lastReasoning = text
		if !r.Reasoning {
			fmt.Fprintf(r.w, "%s %s\n", s.Dim("▸"),
				s.Dim(fmt.Sprintf("reasoning · %d words · type /think to expand", len(strings.Fields(text)))))
			return
		}
		fmt.Fprintf(r.w, "%s %s\n", s.Dim("▾"), s.Dim("reasoning"))
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(r.w, "  %s %s\n", s.Dim("│"), s.Dim(line))
		}

	case agent.EvActionRequested:
		if r.quiet {
			return
		}
		var a agent.ActionRequested
		if json.Unmarshal(ev.Payload, &a) != nil {
			return
		}
		fmt.Fprintf(r.w, "%s %s %s\n", s.Accent("●"), s.Bold(VisibleLine(a.Tool)), s.Dim(VisibleLine(summarizeArgs(a.Tool, a.Args))))

	case agent.EvObservation:
		var o agent.Observation
		if json.Unmarshal(ev.Payload, &o) != nil {
			return
		}
		if o.IsError {
			for _, line := range firstLines(o.Content, 8) {
				fmt.Fprintf(r.w, "  %s %s\n", s.Red("│"), VisibleLine(line))
			}
			return
		}
		if r.quiet {
			return
		}
		if o.ExitCode != nil && *o.ExitCode != 0 {
			for _, line := range firstLines(o.Content, 12) {
				fmt.Fprintf(r.w, "  %s %s\n", s.Yellow("│"), VisibleLine(line))
			}
			return
		}
		if summary := observationSummary(o); summary != "" {
			fmt.Fprintf(r.w, "  %s %s\n", s.Dim("└"), s.Dim(VisibleLine(summary)))
		}

	case agent.EvActionDenied:
		var m map[string]string
		if json.Unmarshal(ev.Payload, &m) == nil {
			fmt.Fprintf(r.w, "  %s %s\n", s.Red("✕"), s.Dim(VisibleLine(m["reason"])))
		}

	case agent.EvForked:
		var f agent.Forked
		if json.Unmarshal(ev.Payload, &f) == nil && !r.quiet {
			fmt.Fprintf(r.w, "\n%s\n", s.Dim(fmt.Sprintf("── forked at step %d; the steps after it, above, were abandoned ──", f.ThroughSeq)))
		}

	case agent.EvSubagentNotice:
		var n agent.Notice
		if json.Unmarshal(ev.Payload, &n) != nil || r.quiet {
			return
		}
		fmt.Fprintf(r.w, "  %s %s\n", s.Yellow("◆"), s.Dim(noticeText(n)))

	case agent.EvSessionWoken:
		if !r.quiet {
			fmt.Fprintf(r.w, "  %s %s\n", s.Yellow("◆"), s.Dim("woke to act on background results"))
		}

	case agent.EvTodoUpdated:
		if b := r.todoEvent(ev); b != nil && !r.quiet {
			r.emitRows(b.lines(100, s, false))
		}

	case agent.EvSessionEnded:
		var e agent.SessionEnded
		if json.Unmarshal(ev.Payload, &e) != nil || r.quiet {
			return
		}
		if e.Settled {
			fmt.Fprintf(r.w, "  %s %s\n", s.Yellow("◆"), s.Dim("background work finished"))
			return
		}
		if e.Background > 0 {
			fmt.Fprintf(r.w, "  %s\n", s.Dim(fmt.Sprintf("%d background task(s) still running; /tasks lists them", e.Background)))
		}
		if e.Reason != agent.TermCompleted {
			fmt.Fprintf(r.w, "\n%s %s\n", s.Yellow("!"), s.Dim("ended: "+string(e.Reason)))
		}

	default:
		if rows := r.activityRows(ev); rows != nil && !r.quiet {
			r.emitRows(rows)
		}
	}
}

// flushLines renders every complete line held in the buffer, with the
// markdown state carried from line to line so a fence stays a fence.
func (r *Renderer) flushLines(final bool) {
	buf := r.pending.String()
	for {
		i := strings.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		r.emitRows(r.lineSt.line(r.s, buf[:i], 0))
		buf = buf[i+1:]
	}
	r.pending.Reset()
	r.pending.WriteString(buf)
	if final {
		if buf != "" {
			r.emitRows(r.lineSt.line(r.s, buf, 0))
			r.pending.Reset()
		}
		r.emitRows(r.lineSt.flush(r.s, 0))
	}
}

func (r *Renderer) emitRows(rows []string) {
	for _, row := range rows {
		fmt.Fprintln(r.w, row)
	}
}

func (r *Renderer) endStream() {
	r.streaming = false
	r.pending.Reset()
	r.lineSt = mdState{}
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// truncate counts runes, so a cut never splits a character into stray bytes.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
