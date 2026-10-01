package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// DialogApprover asks for approval with the dock's dialog: the call and what
// it will change, why it is asked, and numbered answers that no key pressed
// in haste can give (see dialogKey). Without a terminal it hands the
// question to Base, the line prompt.
type DialogApprover struct {
	// Base holds the session's "always" answers, and asks when there is no
	// terminal.
	Base   *Approver
	Reader *LineReader
	Render *Renderer
	// ReadFile reads a file through the session, for the diff an edit will
	// make; nil shows the edit's old and new text instead.
	ReadFile func(path string) ([]byte, error)
}

// Approve implements agent.Approver.
func (a *DialogApprover) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	if a.Reader == nil || !a.Reader.Raw() {
		return a.Base.Approve(ctx, tool, args, res)
	}
	scope := res.Offer()
	header := a.header(tool, args)
	if scope != "" && a.Base.Session.Has(scope) {
		agent.NoteAnswer(ctx, agent.Answer{By: agent.BySessionScope, Scope: scope})
		a.Reader.commitItem(&dialogRecord{spec: DialogSpec{Title: header, Choices: []Choice{{ID: "yes",
			Label: "Allowed for this session by " + a.showScope(scope)}}}})
		return true, nil
	}

	spec := a.spec(ctx, tool, args, res, header)
	id, err := a.Reader.Dialog(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil // input ended: nobody can say yes
	}
	switch id {
	case "yes":
		if res.Step == "destructive" {
			ok, err := a.confirm(ctx, args, res)
			if err != nil || !ok {
				a.Reader.Stop()
				return false, err
			}
		}
		a.noteDiff(tool, args)
		return true, nil
	case "always":
		if scope == "" {
			return false, nil // never offered: nothing to grant
		}
		a.Base.Session.Add(scope)
		agent.NoteAnswer(ctx, agent.Answer{By: agent.ByReviewer, Granted: scope})
		a.noteDiff(tool, args)
		return true, nil
	}
	// No: the turn stops, so the person can say what to do instead.
	a.Reader.Stop()
	return false, nil
}

func (a *DialogApprover) noteDiff(tool string, args json.RawMessage) {
	if a.Render != nil && (tool == "edit" || tool == "write") {
		a.Render.MarkDiffShown(tool, args)
	}
}

func (a *DialogApprover) rel(p string) string {
	if a.Render != nil {
		return a.Render.rel(p)
	}
	return p
}

func (a *DialogApprover) header(tool string, args json.RawMessage) string {
	sum := reveal(summarizeArgsRel(tool, args, a.rel))
	if tool == "bash" {
		cmd := reveal(str(args, "command"))
		sum = firstLine(cmd)
		if len([]rune(sum)) > 80 {
			sum = string([]rune(sum)[:79]) + "…"
		} else if sum != cmd {
			sum += " …" // more lines follow, shown in full below
		}
	}
	if sum == "" {
		return toolTitle(tool)
	}
	return toolTitle(tool) + "(" + sum + ")"
}

// showScope shows a scope with its path relative to the workspace:
// edit(/home/me/ws/src/a.go) reads as edit(src/a.go).
func (a *DialogApprover) showScope(scope string) string {
	open, close := strings.IndexByte(scope, '('), strings.LastIndexByte(scope, ')')
	if open < 0 || close < open {
		return reveal(scope)
	}
	inner := scope[open+1 : close]
	if filepath.IsAbs(inner) {
		inner = a.rel(inner)
	}
	return reveal(scope[:open+1] + inner + scope[close:])
}

func str(args json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// spec builds the approval dialog.
func (a *DialogApprover) spec(ctx context.Context, tool string, args json.RawMessage, res policy.Result, header string) DialogSpec {
	var why []string
	if approvalHidden(ctx, tool, args, res) {
		why = append(why, HiddenWarning)
	}
	if via := agent.PipelineOf(ctx); via != "" {
		why = append(why, "asked by "+VisibleLine(via))
	}
	if who := agent.SubagentOf(ctx); who != "" {
		why = append(why, "asked by subagent: "+VisibleLine(who))
	}
	reason := VisibleLine(res.Reason)
	if res.Step != "" {
		reason = strings.TrimPrefix(reason+" · policy step: "+res.Step, " · ")
	}
	if reason != "" {
		why = append(why, reason)
	}

	var body []Block
	question := fmt.Sprintf("Allow %s?", toolTitle(tool))
	switch tool {
	case "bash":
		body = append(body, viewBlock(&commandBlock{command: reveal(str(args, "command"))}))
		question = "Run this command?"
	case "edit", "write":
		path := str(args, "path")
		if d := a.proposed(tool, args); d != nil {
			body = append(body, viewBlock(&resultBlock{head: d.summary(), diff: d}))
		}
		switch {
		case tool == "write" && a.missing(path):
			question = "Create " + reveal(a.rel(path)) + "?"
		case tool == "write":
			question = "Overwrite " + reveal(a.rel(path)) + "?"
		default:
			question = "Make this edit to " + reveal(a.rel(path)) + "?"
		}
	default:
		if raw := strings.TrimSpace(string(args)); raw != "" && raw != "{}" {
			body = append(body, viewBlock(&commandBlock{command: reveal(raw), plain: true}))
		}
	}

	choices := []Choice{{ID: "yes", Label: "Yes"}}
	if scope := res.Offer(); scope != "" {
		choices = append(choices, Choice{ID: "always", Label: "Yes, and don't ask again for " + a.showScope(scope) + " this session", Widening: true})
	}
	choices = append(choices, Choice{ID: "no", Label: "No, and tell Abhed what to do instead (esc)"})
	scope := a.showScope(res.Offer())
	return DialogSpec{
		Kind:    DialogApproval,
		Title:   header,
		Why:     strings.Join(why, "\n"),
		Body:    body,
		Ask:     question,
		Choices: choices,
		Cancel:  "no",
		Outcome: func(id string) string {
			switch id {
			case "yes":
				return "Approved"
			case "always":
				return "Approved · allowed for this session: " + scope
			}
			return "Declined"
		},
	}
}

// confirm is the second question a destructive command needs, with No as
// the answer Enter gives.
func (a *DialogApprover) confirm(ctx context.Context, args json.RawMessage, res policy.Result) (bool, error) {
	id, err := a.Reader.Dialog(ctx, DialogSpec{
		Kind:  DialogConfirm,
		Title: "This cannot be undone",
		Why:   VisibleLine(res.Reason),
		Body:  []Block{viewBlock(&commandBlock{command: reveal(str(args, "command"))})},
		Ask:   "Really run it?",
		Choices: []Choice{
			{ID: "no", Label: "No, don't run it"},
			{ID: "yes", Label: "Yes, run it", Destructive: true},
		},
		Default: "no",
		Cancel:  "no",
		Outcome: func(id string) string {
			if id == "yes" {
				return "Confirmed"
			}
			return "Not run"
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	return id == "yes", nil
}

func (a *DialogApprover) missing(path string) bool {
	if a.ReadFile == nil {
		return false
	}
	_, err := a.ReadFile(path)
	return err != nil
}

// proposed is the diff an edit or write would make to the file as it is now.
func (a *DialogApprover) proposed(tool string, args json.RawMessage) *fileDiff {
	var p struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Content    string `json:"content"`
	}
	if json.Unmarshal(args, &p) != nil {
		return nil
	}
	var before []byte
	existed := false
	if a.ReadFile != nil {
		if b, err := a.ReadFile(p.Path); err == nil {
			before, existed = b, true
		}
	}
	rel := a.rel(p.Path)
	if tool == "write" {
		return newFileDiff(rel, string(before), p.Content, !existed)
	}
	switch {
	case !existed && p.Old == "":
		return newFileDiff(rel, "", p.New, true)
	case !existed || !strings.Contains(string(before), p.Old):
		// Not readable, or not matching: show the change as given.
		return newFileDiff(rel, p.Old, p.New, false)
	}
	n := 1
	if p.ReplaceAll {
		n = -1
	}
	return newFileDiff(rel, string(before), strings.Replace(string(before), p.Old, p.New, n), false)
}

// commandBlock is a command shown whole: every line, wrapped, never cut.
type commandBlock struct {
	command string
	plain   bool
}

func (b *commandBlock) lines(width int, s Style, _ bool) []string {
	var out []string
	for i, l := range strings.Split(b.command, "\n") {
		lead := "$ "
		if i > 0 || b.plain {
			lead = "  "
		}
		for j, row := range hardWrap(l, max(width-2, 8)) {
			if j > 0 {
				lead = "  "
			}
			out = append(out, s.Dim(lead)+s.Bold(row))
		}
	}
	return out
}

// Stop asks the session to stop the running turn, as Esc does.
func (l *LineReader) Stop() {
	if !l.raw {
		return
	}
	select {
	case l.d.stops <- struct{}{}:
	default:
	}
}

// commitItem draws a finished item in the transcript.
func (l *LineReader) commitItem(b block) {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	l.d.commitItem(b)
}
