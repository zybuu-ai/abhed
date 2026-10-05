package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// The harness's own activity, drawn as dim lines in the transcript: the todo
// list, subagents starting and returning, compaction, mode and rule changes,
// hooks, and the rule that approved a call. Every field shown can carry text
// the model or an extension chose, so each goes through VisibleLine.

// spawnedInfo is what subagent.spawned says that is worth a line.
type spawnedInfo struct {
	Description string `json:"description"`
	AgentType   string `json:"agent_type"`
	Definition  string `json:"definition"`
	Session     string `json:"session"`
	Model       string `json:"model"`
	Branch      string `json:"branch"`
	Background  bool   `json:"background"`
}

// returnedInfo is what subagent.returned says that is worth a line.
type returnedInfo struct {
	Description string `json:"description"`
	Session     string `json:"session"`
	Reason      string `json:"reason"`
	Turns       int    `json:"turns"`
	TokensIn    int    `json:"tokens_in"`
	TokensOut   int    `json:"tokens_out"`
}

// compactionInfo is compaction.completed, which carries either the counts
// or why nothing was compacted.
type compactionInfo struct {
	agent.Compaction
	Error   string `json:"error"`
	Skipped string `json:"skipped"`
}

// field is untrusted text fit for one line of an activity row.
func field(s string, n int) string { return VisibleLine(truncate(s, n)) }

// activityRows is the line an activity event draws, or nil for an event that
// is not activity or says nothing worth a line. r.mu is held.
func (r *Renderer) activityRows(ev agent.Event) []string {
	s := r.s
	note := func(text string) []string { return []string{"  " + s.Yellow("◆") + " " + s.Dim(text)} }
	switch ev.Type {
	case agent.EvSubagentSpawned:
		var p spawnedInfo
		if json.Unmarshal(ev.Payload, &p) != nil {
			return nil
		}
		who := orStr(orStr(p.AgentType, p.Definition), "general")
		if r.agentTypes == nil {
			r.agentTypes = map[string]string{}
		}
		r.agentTypes[p.Session] = who
		detail := field(p.Description, 80)
		if p.Model != "" {
			detail += " · " + field(p.Model, 40)
		}
		if p.Branch != "" {
			detail += " · worktree " + field(p.Branch, 40)
		}
		return []string{"  " + s.Dim("├ ") + s.Accent(field(who, 30)) + s.Dim(" started · "+detail)}

	case agent.EvSubagentReturn:
		var p returnedInfo
		if json.Unmarshal(ev.Payload, &p) != nil {
			return nil
		}
		who := orStr(r.agentTypes[p.Session], "subagent")
		status := field(orStr(p.Reason, "ended"), 30)
		if p.Reason == string(agent.TermCompleted) {
			status = s.Green(status)
		} else {
			status = s.Yellow(status)
		}
		turns := fmt.Sprintf("%d turn%s", p.Turns, map[bool]string{true: "", false: "s"}[p.Turns == 1])
		return []string{"  " + s.Dim("└ ") + s.Accent(field(who, 30)) + s.Dim(" returned · ") + status +
			s.Dim(fmt.Sprintf(" · %s · %s tokens · %s", turns, formatCount(p.TokensIn+p.TokensOut), field(p.Description, 60)))}

	case agent.EvCompactStarted:
		var c agent.Compaction
		_ = json.Unmarshal(ev.Payload, &c)
		if c.BeforeTokens > 0 {
			return note(fmt.Sprintf("compacting the conversation · %s tokens", formatCount(c.BeforeTokens)))
		}
		return note("compacting the conversation")

	case agent.EvCompactDone:
		var c compactionInfo
		if json.Unmarshal(ev.Payload, &c) != nil {
			return nil
		}
		switch {
		case c.Error != "":
			return []string{"  " + s.Red("◆") + " " + s.Dim("compaction failed: "+field(c.Error, 120))}
		case c.Skipped != "":
			return note("compaction made no change")
		}
		return note(fmt.Sprintf("compacted · %s → %s tokens (%s)",
			formatCount(c.BeforeTokens), formatCount(c.AfterTokens), field(orStr(c.Trigger, "auto"), 10)))

	case agent.EvModeChanged:
		var m agent.ModeChanged
		// A mode carried into a new conversation restates one already shown.
		if json.Unmarshal(ev.Payload, &m) != nil || m.To == "" || m.Via == "carried" {
			return nil
		}
		parts := []string{"mode: " + field(m.From, 20) + " → " + field(m.To, 20)}
		if m.Via != "" {
			parts = append(parts, field(m.Via, 20))
		}
		if m.By != "" && m.By != agent.ByUser {
			parts = append(parts, "by "+field(m.By, 20))
		}
		return note(strings.Join(parts, " · "))

	case agent.EvPermissionChanged:
		var p agent.PermissionChanged
		if json.Unmarshal(ev.Payload, &p) != nil || p.Rule == "" {
			return nil
		}
		verb := "added"
		if p.Op == "remove" {
			verb = "removed"
		}
		text := fmt.Sprintf("%s %s rule %s: %s", field(orStr(p.Scope, "session"), 20), field(p.List, 10), verb, field(p.Rule, 120))
		if p.Op != "remove" && orStr(p.Scope, "session") == "session" {
			text += " (until /clear)"
		}
		return note(text)

	case agent.EvHookFired:
		var h agent.HookFired
		if json.Unmarshal(ev.Payload, &h) != nil {
			return nil
		}
		verdict := field(h.Verdict, 20)
		switch h.Verdict {
		case "block":
			verdict = s.Red(verdict)
		case "ask":
			verdict = s.Yellow(verdict)
		}
		return []string{"  " + s.Yellow("◆") + " " + s.Dim("hook "+field(h.Extension, 40)+" · "+field(h.Event, 30)+" · ") + verdict}

	case agent.EvActionApproved:
		// Only a configured rule's approval is drawn: a read the default
		// allows, or a person's answer, is shown well enough already.
		var m map[string]string
		if json.Unmarshal(ev.Payload, &m) != nil || m["by"] != agent.ByPolicy || m["rule"] == "" {
			return nil
		}
		return []string{"  " + s.Green("✓") + " " + s.Dim("auto: "+field(m["rule"], 120))}
	}
	return nil
}

// todoBlock is the agent's task list as it stood at one todo.updated.
type todoBlock struct {
	items []agent.Todo
	// lead is drawn before the first row: the result mark under a call, or
	// a heading when the list is shown on its own.
	lead string
}

// todoMax is how many items a list shows before "… N more"; Ctrl-O shows all.
const todoMax = 12

func (b *todoBlock) lines(width int, s Style, expanded bool) []string {
	var out []string
	items := b.items
	more := 0
	if !expanded && len(items) > todoMax {
		more = len(items) - todoMax
		items = items[:todoMax]
	}
	room := max(width-7, 10)
	for i, it := range items {
		lead := "     "
		if i == 0 {
			lead = "  " + s.Dim("⎿") + "  "
		}
		out = append(out, lead+todoRow(s, it, room))
	}
	if more > 0 {
		out = append(out, "     "+s.Dim(fmt.Sprintf("… %d more (ctrl+o to expand)", more)))
	}
	if len(out) == 0 {
		out = append(out, "  "+s.Dim("⎿")+"  "+s.Dim("(the list is empty)"))
	}
	if b.lead != "" {
		out = append([]string{b.lead}, out...)
	}
	return out
}

// todoRow is one item: a box for its status and its text, cut to room.
func todoRow(s Style, it agent.Todo, room int) string {
	text := truncateWidth(VisibleLine(it.Text), room)
	switch it.Status {
	case "done":
		return s.Green("☒") + " " + s.Dim(s.Strike(text))
	case "in_progress":
		return s.Accent("◼") + " " + s.Bold(text)
	case "cancelled":
		return s.Dim("☐ " + s.Strike(text) + " (cancelled)")
	}
	return s.Dim("☐") + " " + text
}

// todoSummary is the list in one line, for the dock: done of total and the
// item being worked on. "" when nothing is left to do.
func todoSummary(items []agent.Todo) (string, bool) {
	done, open := 0, 0
	current := ""
	for _, it := range items {
		switch it.Status {
		case "done":
			done++
		case "cancelled":
		default:
			open++
			if it.Status == "in_progress" && current == "" {
				current = it.Text
			}
		}
	}
	if open == 0 {
		return "", false
	}
	line := fmt.Sprintf("%d/%d done", done, done+open)
	if current != "" {
		line += " · " + VisibleLine(current)
	}
	return line, true
}

// todoEvent draws a todo.updated: the whole list under the call that made
// it, and kept for the dock and /todos. r.mu is held.
func (r *Renderer) todoEvent(ev agent.Event) *todoBlock {
	var l agent.TodoList
	if json.Unmarshal(ev.Payload, &l) != nil {
		return nil
	}
	r.todos = append([]agent.Todo(nil), l.Items...)
	return &todoBlock{items: r.todos}
}

// Todos is the agent's task list as last recorded.
func (r *Renderer) Todos() []agent.Todo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Todo(nil), r.todos...)
}

// ShowTodos prints the task list on its own, for /todos, and reports whether
// there was one.
func (r *Renderer) ShowTodos() bool {
	r.mu.Lock()
	items := append([]agent.Todo(nil), r.todos...)
	r.mu.Unlock()
	if len(items) == 0 {
		return false
	}
	head := r.s.Accent("● ") + r.s.Bold("Todos")
	if sum, open := todoSummary(items); open {
		head += r.s.Dim(" · " + truncate(sum, 60))
	}
	b := &todoBlock{items: items, lead: head}
	if d := r.dock; d != nil {
		d.mu.Lock()
		d.commitItem(b)
		d.mu.Unlock()
		return true
	}
	for _, row := range b.lines(100, r.s, true) {
		fmt.Fprintln(r.w, row)
	}
	return true
}

// LastReply is the text of the agent's most recent reply, cleaned of
// anything a terminal would act on, or "".
func (r *Renderer) LastReply() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReply
}

// LastReplyCut reports whether cleaning the last reply took characters out
// of it, so a copy of it is not the text the model sent.
func (r *Renderer) LastReplyCut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReplyCut
}

// toggleTodos is Ctrl-T: the whole task list above the input, or its summary.
func (d *dock) toggleTodos() {
	if _, open := todoSummary(d.todos); !open {
		d.flash("no open todos")
		return
	}
	d.todosOpen = !d.todosOpen
}

// todoRows is the task list above the input while any of it is open: one
// summary line, or every item after Ctrl-T.
func (d *dock) todoRows(w int) []string {
	sum, open := todoSummary(d.todos)
	if !open {
		return nil
	}
	s := d.st
	if !d.todosOpen {
		return []string{truncateWidth("  "+s.Accent("◼")+" "+s.Dim("Todos "+sum+" · ctrl+t to list"), w)}
	}
	b := &todoBlock{items: d.todos, lead: "  " + s.Accent("◼") + " " + s.Dim("Todos "+sum+" · ctrl+t to hide")}
	var out []string
	for _, row := range b.lines(w, s, false) {
		out = append(out, truncateWidth(row, w))
	}
	return out
}
