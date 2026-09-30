package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// toolState is what the renderer remembers about calls in flight.
type toolState struct {
	// root is the workspace, so paths are shown relative to it, and real
	// is the same with symbolic links resolved.
	root, real string
	// calls maps a call's id to its tool and summary, so its result can be
	// drawn under it.
	calls map[string]*toolCall
	// befores are files as they were just before a tool changed them, in
	// the order the changes happened, for the diff drawn under the result.
	befores []checkpoint
	// shown are the edits whose diff a dialog already drew, by call key.
	shown map[string]bool
}

type checkpoint struct {
	path    string
	before  []byte
	existed bool
}

// Checkpoint wraps next, the session's hook for a file's content just before
// a tool changes it, so the renderer sees it too: it is what an edit's diff
// is drawn from, whatever mode approved it.
func (r *Renderer) Checkpoint(next func(path string, before []byte, existed bool)) func(string, []byte, bool) {
	return func(path string, before []byte, existed bool) {
		r.mu.Lock()
		if len(r.tools.befores) > 64 {
			r.tools.befores = r.tools.befores[1:]
		}
		r.tools.befores = append(r.tools.befores, checkpoint{path, append([]byte(nil), before...), existed})
		r.mu.Unlock()
		if next != nil {
			next(path, before, existed)
		}
	}
}

// takeBefore finds and removes the checkpoint for path.
func (r *Renderer) takeBefore(path string) (checkpoint, bool) {
	abs := path
	if !filepath.IsAbs(abs) && r.tools.root != "" {
		abs = filepath.Join(r.tools.root, abs)
	}
	abs = filepath.Clean(abs)
	for i, c := range r.tools.befores {
		if filepath.Clean(c.path) == abs || filepath.Base(c.path) == filepath.Base(abs) && strings.HasSuffix(filepath.Clean(c.path), strings.TrimPrefix(abs, filepath.VolumeName(abs))) {
			r.tools.befores = append(r.tools.befores[:i], r.tools.befores[i+1:]...)
			return c, true
		}
	}
	return checkpoint{}, false
}

// MarkDiffShown notes that a dialog drew the diff of this call, so the
// result under it does not draw it again.
func (r *Renderer) MarkDiffShown(tool string, args json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools.shown == nil {
		r.tools.shown = map[string]bool{}
	}
	r.tools.shown[tool+"\x00"+string(args)] = true
}

type toolCall struct {
	tool    string
	summary string
	args    json.RawMessage
	// asked is set when the call waited on a person: the dialog drew it.
	asked bool
}

// SetWorkspace tells the renderer the workspace root, so paths in tool
// headers, scopes and messages are shown relative to it.
func (r *Renderer) SetWorkspace(root string) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	r.mu.Lock()
	r.tools.root, r.tools.real = root, real
	r.mu.Unlock()
}

// rel shows p relative to the workspace, whether p names it by its links
// or by its real path.
func (r *Renderer) rel(p string) string {
	if out := relPath(r.tools.root, p); out != p || r.tools.real == r.tools.root {
		return out
	}
	if out := relPath(r.tools.real, p); out != p {
		return out
	}
	// A path through a link to the workspace: resolve the part that exists.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		if out := relPath(r.tools.real, filepath.Join(dir, filepath.Base(p))); !filepath.IsAbs(out) {
			return out
		}
	}
	return relPath("", p)
}

// relPath shows p relative to root when it is inside it, with ~ for the
// home directory otherwise.
func relPath(root, p string) string {
	if p == "" {
		return p
	}
	if root != "" && filepath.IsAbs(p) {
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "."
			}
			return rel
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// toolTitle is the name a tool is shown by.
func toolTitle(tool string) string {
	switch tool {
	case "bash":
		return "Bash"
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "edit":
		return "Edit"
	case "glob":
		return "Glob"
	case "grep":
		return "Grep"
	case "task":
		return "Task"
	case "web_fetch":
		return "Fetch"
	case "web_search":
		return "Search"
	}
	if tool == "" {
		return "Tool"
	}
	return strings.ToUpper(tool[:1]) + tool[1:]
}

// summarizeArgs renders the one useful detail per tool, so the line stays
// scannable.
func summarizeArgs(tool string, raw json.RawMessage) string { return summarizeArgsRel(tool, raw, nil) }

// summarizeArgsRel is summarizeArgs with paths shortened by rel.
func summarizeArgsRel(tool string, raw json.RawMessage, rel func(string) string) string {
	if rel == nil {
		rel = func(s string) string { return s }
	}
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
		return rel(str("path"))
	case "glob":
		return str("pattern")
	case "grep":
		if p := str("path"); p != "" {
			return fmt.Sprintf("%q in %s", str("pattern"), rel(p))
		}
		return fmt.Sprintf("%q", str("pattern"))
	case "bash":
		return truncate(str("command"), 120)
	case "task":
		return str("description")
	case "web_fetch":
		return str("url")
	case "web_search":
		return str("query")
	}
	return ""
}

// toolHeader is a call's one line: ● Edit(src/main.go).
func (r *Renderer) toolHeader(tool string, args json.RawMessage) string {
	s := r.s
	sum := sanitize(summarizeArgsRel(tool, args, r.rel), false)
	if sum == "" {
		return s.Accent("● ") + s.Bold(toolTitle(tool))
	}
	return s.Accent("● ") + s.Bold(toolTitle(tool)) + s.Dim("(") + sum + s.Dim(")")
}

func (r *Renderer) toolRequested(d *dock, a agent.ActionRequested) {
	if r.tools.calls == nil {
		r.tools.calls = map[string]*toolCall{}
	}
	c := &toolCall{tool: a.Tool, args: a.Args, summary: summarizeArgsRel(a.Tool, a.Args, r.rel), asked: a.RequiresApproval}
	r.tools.calls[a.CallID] = c
	d.act.label = "Running " + toolTitle(a.Tool)
	if a.RequiresApproval {
		d.draw() // the dialog draws the call, and keeps it in the transcript
		return
	}
	d.commitItem(&rawBlock{text: r.toolHeader(a.Tool, a.Args)})
}

func (r *Renderer) toolObserved(d *dock, o agent.Observation) {
	d.act.label = ""
	s := r.s
	c := r.tools.calls[o.CallID]
	delete(r.tools.calls, o.CallID)
	content := sanitize(o.Content, false)
	switch {
	case o.IsError:
		first, rest := splitFirst(content)
		d.commit(&resultBlock{head: s.Red("Error: " + first), body: rest, style: s.Red, headN: 6})
	case (o.Tool == "edit" || o.Tool == "write") && c != nil:
		if b := r.changeResult(o, c, content); b != nil {
			d.commit(b)
		}
	case o.Tool == "bash":
		first, rest := splitFirst(content)
		code := 0
		if o.ExitCode != nil {
			code = *o.ExitCode
		}
		b := &resultBlock{body: trimNoOutput(rest), style: func(x string) string { return x }, headN: 3, tailN: 2}
		if code != 0 {
			b.head = s.Yellow(fmt.Sprintf("Exit %d", code)) + s.Dim(strings.TrimPrefix(first, fmt.Sprintf("exit %d", code)))
			b.headN, b.tailN = 6, 4
		} else if len(b.body) == 0 {
			b.head = s.Dim("(no output) · " + strings.TrimPrefix(first, "exit 0 · "))
		}
		d.commit(b)
	default:
		if summary := observationSummary(o); summary != "" {
			d.commit(&resultBlock{head: s.Dim(summary)})
		}
	}
}

// changeResult is an edit's or a write's result: what changed, and the diff
// unless a dialog already showed it.
func (r *Renderer) changeResult(o agent.Observation, c *toolCall, content string) block {
	s := r.s
	var args struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Content    string `json:"content"`
	}
	_ = json.Unmarshal(c.args, &args)
	before, ok := r.takeBefore(args.Path)
	key := o.Tool + "\x00" + string(c.args)
	shown := r.tools.shown[key]
	delete(r.tools.shown, key)
	if !ok {
		return &resultBlock{head: s.Dim(firstLine(content))}
	}
	after := args.Content
	if o.Tool == "edit" {
		n := 1
		if args.ReplaceAll {
			n = -1
		}
		after = strings.Replace(string(before.before), args.Old, args.New, n)
		if !before.existed {
			after = args.New
		}
	}
	diff := newFileDiff(r.rel(args.Path), string(before.before), after, !before.existed)
	if shown {
		return nil // the dialog's record already shows the change
	}
	return &resultBlock{head: diff.summary(), diff: diff}
}

func splitFirst(s string) (string, []string) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[0], lines[1:]
}

func trimNoOutput(lines []string) []string {
	if len(lines) == 1 && lines[0] == "[no output]" {
		return nil
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// resultBlock is a result under its call: a first line, then the output's
// first and last lines with a count of what is between, or a diff. Ctrl-O
// shows the whole of it.
type resultBlock struct {
	head  string
	body  []string
	style func(string) string
	diff  *fileDiff
	// headN and tailN are how many lines of the body the preview keeps
	// from its start and end.
	headN, tailN int
}

func (b *resultBlock) lines(width int, s Style, expanded bool) []string {
	var out []string
	if b.head != "" {
		for i, row := range wrapWords(b.head, max(width-5, 10)) {
			lead := "     "
			if i == 0 {
				lead = "  " + s.Dim("⎿") + "  "
			}
			out = append(out, lead+row)
		}
	}
	indent := "     "
	if b.diff != nil {
		limit := 40
		if expanded {
			limit = 0
		}
		for _, row := range b.diff.rows(s, width-len(indent), limit) {
			out = append(out, indent+row)
		}
		return out
	}
	body := b.body
	style := b.style
	if style == nil {
		style = s.Dim
	}
	emit := func(lines []string) {
		for _, l := range lines {
			for _, row := range hardWrap(l, max(width-len(indent), 8)) {
				out = append(out, indent+style(row))
			}
		}
	}
	if b.head == "" && len(body) > 0 {
		// The first output line takes the ⎿ row.
		first := hardWrap(body[0], max(width-5, 8))
		out = append(out, "  "+s.Dim("⎿")+"  "+style(first[0]))
		for _, row := range first[1:] {
			out = append(out, indent+style(row))
		}
		body = body[1:]
	}
	keep := b.headN + b.tailN
	if expanded || keep == 0 || len(body) <= keep+1 {
		emit(body)
		return out
	}
	emit(body[:b.headN])
	out = append(out, indent+s.Dim(fmt.Sprintf("… +%d lines (ctrl+o to expand)", len(body)-keep)))
	emit(body[len(body)-b.tailN:])
	return out
}

func (r *Renderer) toolDenied(d *dock, m map[string]string) {
	s := r.s
	if c := r.tools.calls[m["call_id"]]; c != nil && c.asked && m["by"] != agent.BySystem && m["by"] != agent.ByPolicy {
		return // the dialog's record already says who declined it
	}
	d.commit(&rawBlock{text: "  " + s.Red("⎿  ✕ ") + s.Dim(sanitize(m["reason"], false))})
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
