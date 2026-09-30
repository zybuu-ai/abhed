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
	// root is the workspace, so paths are shown relative to it.
	root string
	// calls maps a call's id to its tool and summary, so its result can be
	// drawn under it.
	calls map[string]*toolCall
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
	r.mu.Lock()
	r.tools.root = root
	r.mu.Unlock()
}

func (r *Renderer) rel(p string) string { return relPath(r.tools.root, p) }

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
	content := sanitize(o.Content, false)
	switch {
	case o.IsError:
		d.commit(&rawBlock{text: resultRows(s, s.Red("Error: "+firstLine(content)), restLines(content, 6), s.Red)})
	case o.ExitCode != nil && *o.ExitCode != 0:
		d.commit(&rawBlock{text: resultRows(s, s.Yellow(fmt.Sprintf("Exit %d", *o.ExitCode)), firstLines(content, 8), s.Dim)})
	default:
		if summary := observationSummary(o); summary != "" {
			d.commit(&rawBlock{text: "  " + s.Dim("⎿  "+summary)})
		}
	}
	delete(r.tools.calls, o.CallID)
}

func (r *Renderer) toolDenied(d *dock, m map[string]string) {
	s := r.s
	if c := r.tools.calls[m["call_id"]]; c != nil && c.asked && m["by"] != agent.BySystem && m["by"] != agent.ByPolicy {
		return // the dialog's record already says who declined it
	}
	d.commit(&rawBlock{text: "  " + s.Red("⎿  ✕ ") + s.Dim(sanitize(m["reason"], false))})
}

// resultRows lays out a result under its call: a first line, then more.
func resultRows(s Style, head string, more []string, style func(string) string) string {
	var b strings.Builder
	b.WriteString("  " + s.Dim("⎿  ") + head)
	for _, l := range more {
		b.WriteString("\n     " + style(l))
	}
	return b.String()
}

func restLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= 1 {
		return nil
	}
	lines = lines[1:]
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… +%d lines", len(lines)-n))
	}
	return lines
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
