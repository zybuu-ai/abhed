package app

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
)

// panelLinger is how long a finished subagent or job stays in the panel.
const panelLinger = 2 * time.Minute

// jobRow is one piece of the conversation's work as the panel and /tasks
// list it: a subagent, or a background job of any kind.
type jobRow struct {
	ID, Parent string
	Kind       string // the agent type for a subagent, else the job's kind
	Agent      bool
	Background bool
	Title      string
	Activity   string
	Status     string // running, completed, failed, cancelled
	Reason     string
	Started    time.Time
	Ended      time.Time
	TokensIn   int
	ExitCode   *int
	Summary    string
	// Undelivered are messages sent to it that it ended before taking.
	Undelivered []string
}

func (j jobRow) running() bool { return j.Status == "running" }

// outcome is how an ended row reads: a shell that exited 0 completed, one
// that exited otherwise failed; anything else is its own status.
func (j jobRow) outcome() string {
	if j.Status != agent.ShellExited {
		return j.Status
	}
	if j.ExitCode != nil && *j.ExitCode == 0 {
		return "completed"
	}
	return "failed"
}

// workPanel feeds the dock's work panel and /tasks from the open
// conversation's work list and background registry, and says in the
// transcript when a background subagent or job finishes.
type workPanel struct {
	r      *ui.Renderer
	store  server.EventStore
	editor *ui.LineReader
	now    func() time.Time

	mu     sync.Mutex
	work   *agent.Work
	bg     *agent.Background
	mainID string
	busy   bool
	since  time.Time
	// ended is when each item was first seen ended; told, whose end was said.
	ended map[string]time.Time
	told  map[string]bool
}

func newWorkPanel(r *ui.Renderer, store server.EventStore, editor *ui.LineReader) *workPanel {
	return &workPanel{r: r, store: store, editor: editor, now: time.Now,
		ended: map[string]time.Time{}, told: map[string]bool{}}
}

// attach makes loop the conversation the panel follows.
func (p *workPanel) attach(loop *agent.Loop, id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.work, p.bg, p.mainID = loop.Work, loop.Background, id
	p.ended, p.told = map[string]time.Time{}, map[string]bool{}
}

// turn marks the conversation's own run starting or ending.
func (p *workPanel) turn(busy bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.busy = busy
	if busy {
		p.since = p.now()
	}
	p.mu.Unlock()
}

func (p *workPanel) sources() (*agent.Work, *agent.Background, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.work, p.bg, p.mainID
}

// jobs lists every subagent and background job, in the order they started.
func (p *workPanel) jobs() []jobRow {
	if p == nil {
		return nil
	}
	work, bg, _ := p.sources()
	var out []jobRow
	have := map[string]bool{}
	for _, it := range work.Items() {
		have[it.ID] = true
		out = append(out, jobRow{ID: it.ID, Parent: it.Parent, Kind: orDefault(it.AgentType, "agent"), Agent: true,
			Background: it.Background, Title: it.Title, Activity: it.Activity, Status: it.Status, Reason: it.Reason,
			Started: it.Started, Ended: it.Ended, TokensIn: it.TokensIn, Undelivered: it.Undelivered})
	}
	// Background jobs that are not subagents, such as shells, by their kind.
	for _, t := range bg.Tasks() {
		if have[t.ID] {
			continue
		}
		j := jobRow{ID: t.ID, Kind: t.Kind, Agent: t.Kind != agent.KindShell, Background: true,
			Title: t.Description, Activity: t.LastLine, Status: t.Status, Reason: t.Reason,
			Started: t.Started, ExitCode: t.ExitCode, Summary: t.Summary}
		if j.Agent {
			j.Kind = orDefault(t.AgentType, "agent")
		}
		out = append(out, j)
	}
	return out
}

// endedAt is when j ended: its own time, or when the panel first saw it ended.
func (p *workPanel) endedAt(j jobRow) time.Time {
	if !j.Ended.IsZero() {
		return j.Ended
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.ended[j.ID]
	if !ok {
		at = p.now()
		p.ended[j.ID] = at
	}
	return at
}

// rows is the panel as the dock draws it: the conversation, then each
// running or lately finished item nested under what started it.
func (p *workPanel) rows() []ui.WorkRow {
	if p == nil {
		return nil
	}
	jobs := p.jobs()
	_, _, mainID := p.sources()
	p.mu.Lock()
	main := ui.WorkRow{State: ui.WorkIdle}
	if p.busy {
		main.State, main.Elapsed = ui.WorkRunning, p.now().Sub(p.since)
	}
	p.mu.Unlock()
	main.Tokens = p.r.Usage().TokensIn
	now := p.now()
	var shown []jobRow
	for _, j := range jobs {
		if j.running() || now.Sub(p.endedAt(j)) < panelLinger {
			shown = append(shown, j)
		}
	}
	if len(shown) == 0 {
		return nil
	}
	return append([]ui.WorkRow{main}, p.tree(shown, mainID)...)
}

// tree orders jobs under their parents, depth first.
func (p *workPanel) tree(jobs []jobRow, mainID string) []ui.WorkRow {
	known := map[string]bool{}
	for _, j := range jobs {
		known[j.ID] = true
	}
	children := map[string][]jobRow{}
	for _, j := range jobs {
		parent := j.Parent
		if parent == mainID || !known[parent] {
			parent = ""
		}
		children[parent] = append(children[parent], j)
	}
	var out []ui.WorkRow
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, j := range children[parent] {
			out = append(out, p.row(j, depth))
			walk(j.ID, depth+1)
		}
	}
	walk("", 1)
	return out
}

func (p *workPanel) row(j jobRow, depth int) ui.WorkRow {
	r := ui.WorkRow{ID: j.ID, Depth: depth, Kind: j.Kind, Title: j.Title, Tokens: j.TokensIn,
		Target: j.Agent && j.running()}
	switch j.outcome() {
	case "running":
		r.State, r.Activity = ui.WorkRunning, j.Activity
		r.Elapsed = p.now().Sub(j.Started)
	case "completed":
		r.State = ui.WorkDone
	case "failed":
		r.State = ui.WorkFailed
	default:
		r.State = ui.WorkCancelled
	}
	if !j.running() && !j.Started.IsZero() {
		r.Elapsed = p.endedAt(j).Sub(j.Started)
	}
	return r
}

// send hands text to a running subagent as the person's message; one that
// cannot take it is said, and the dock sends it to the conversation.
func (p *workPanel) send(id, text string) bool {
	work, _, _ := p.sources()
	if err := work.Steer(id, text); err != nil {
		name := "the task"
		if it, ok := work.Item(id); ok {
			name = "@" + orDefault(it.AgentType, "agent")
		}
		fmt.Printf("  %s\n", p.r.Style().Dim(name+" has finished and cannot take messages; sent to main"))
		return false
	}
	return true
}

// view shows a subagent's or job's record, read-only, until it is closed.
func (p *workPanel) view(id string) {
	var j jobRow
	found := false
	for _, x := range p.jobs() {
		if x.ID == id {
			j, found = x, true
		}
	}
	if !found {
		return
	}
	title := fmt.Sprintf("%s · %s · read-only", j.Kind, sanitizeLine(j.Title))
	_ = p.editor.Panel(context.Background(), ui.PanelSpec{Title: title,
		Body: []ui.Block{{Kind: ui.BlockToolOut, Text: p.recordText(j)}}})
}

// recordText is a job's record as the line renderer draws it: its calls,
// their results and its messages; a job with no record says what is known.
func (p *workPanel) recordText(j jobRow) string {
	var b bytes.Buffer
	if j.Agent && p.store != nil {
		if evs, err := p.store.Events(j.ID); err == nil && len(evs) > 0 {
			r := ui.NewRenderer(&b, false)
			for _, ev := range evs {
				r.Event(ev)
			}
			return strings.TrimRight(b.String(), "\n")
		}
	}
	fmt.Fprintf(&b, "%s: %s\nstatus: %s", j.Kind, j.Title, j.Status)
	if j.ExitCode != nil {
		fmt.Fprintf(&b, " (exit code %d)", *j.ExitCode)
	}
	if j.Activity != "" {
		fmt.Fprintf(&b, "\nlast output: %s", j.Activity)
	}
	if j.Summary != "" {
		fmt.Fprintf(&b, "\n\n%s", j.Summary)
	}
	return b.String()
}

// sanitizeLine keeps a title to one line.
func sanitizeLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// watch says, once each, when a background subagent or job ends, until
// stop is closed. It reads what the panel reads, so it works the same in
// the line mode, where there is no panel.
func (p *workPanel) watch(stop <-chan struct{}) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.announce()
		}
	}
}

// announce prints a notice for each background item that has ended since
// the last look; a closing session's ends are said by the session.
func (p *workPanel) announce() {
	for _, j := range p.jobs() {
		if j.running() {
			continue
		}
		p.mu.Lock()
		told := p.told[j.ID]
		p.told[j.ID] = true
		p.mu.Unlock()
		if told || j.Reason == string(agent.TermSessionClosed) {
			continue
		}
		for _, m := range j.Undelivered {
			fmt.Printf("  %s\n", p.r.Style().Dim(fmt.Sprintf("@%s ended before taking your message: %q", j.Kind, m)))
		}
		if !j.Background {
			continue
		}
		fmt.Printf("%s\n", finishNotice(p.r.Style(), j, p.endedAt(j).Sub(j.Started)))
	}
}

// finishNotice is the line a finished background item leaves:
//
//	● Agent "review the fix" finished · 5m 27s
//	● Background task "go test ./..." completed (exit code 0)
func finishNotice(s ui.Style, j jobRow, took time.Duration) string {
	mark := s.Green("●")
	switch j.outcome() {
	case "completed":
	case "failed":
		mark = s.Red("✕")
	default:
		mark = s.Dim("○")
	}
	title := sanitizeLine(j.Title)
	if !j.Agent {
		line := fmt.Sprintf("Background task %q %s", title, j.Status)
		if j.ExitCode != nil {
			line += fmt.Sprintf(" (exit code %d)", *j.ExitCode)
		}
		return mark + " " + line
	}
	verb := "finished"
	if j.Status != "completed" {
		verb = j.Status
	}
	return mark + " " + fmt.Sprintf("Agent %q %s", title, verb) + s.Dim(" · "+elapsedText(took))
}

// elapsedText is a duration as the panel shows it: 48s, 5m 27s, 1h 02m.
func elapsedText(d time.Duration) string {
	sec := int(max(d, 0).Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh %02dm", sec/3600, sec%3600/60)
}
