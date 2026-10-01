package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// Approver prompts the user to approve a tool call.
//
// Never "the agent wants to edit auth.go — allow?". The prompt shows exactly
// what changes, because an approval the user cannot evaluate is theatre
// (docs §09).
type Approver struct {
	In      io.Reader
	Out     io.Writer
	Style   Style
	Session *AllowList
	// Prepare, when set, brackets an interactive approval. It is called once
	// before the prompt is drawn and returns a reader for the user's decision
	// and a cleanup run once a decision is made.
	//
	// Interactive mode uses it to (1) pause the thinking indicator so the
	// prompt is not overwritten frame by frame, and (2) read the answer through
	// the single stdin reader the line editor already owns — a single keypress
	// in raw mode rather than a full line. Reading In here
	// instead would open a second reader racing the editor for each keystroke
	// and wait for a "\n" raw mode never sends (Enter is "\r"). read returns
	// ok=false when input ended or was cancelled, which is treated as a refusal.
	Prepare func(ctx context.Context) (read func() (string, bool), cleanup func())

	// reader is the lazily-created line reader for the non-interactive path.
	reader *bufio.Reader
}

// AllowList holds scopes the user approved with "always" during this session.
type AllowList struct {
	scopes map[string]bool
}

func NewAllowList() *AllowList { return &AllowList{scopes: map[string]bool{}} }

func (a *AllowList) Add(scope string) { a.scopes[scope] = true }

func (a *AllowList) Has(scope string) bool { return a.scopes[scope] }

// Reset forgets every scope, for a new session: a scope lasts one session.
func (a *AllowList) Reset() { clear(a.scopes) }

func NewApprover(out io.Writer) *Approver {
	return &Approver{In: os.Stdin, Out: out, Style: NewStyle(out), Session: NewAllowList()}
}

func (a *Approver) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	scope := res.Offer()
	if scope != "" && a.Session.Has(scope) {
		agent.NoteAnswer(ctx, agent.Answer{By: agent.BySessionScope, Scope: scope})
		return true, nil
	}

	// Establish the reader before drawing the prompt: Prepare pauses the
	// thinking indicator, so drawing after it means the prompt is not written
	// under an animation that erases it a moment later.
	read := a.readAnswer
	if a.Prepare != nil {
		r, cleanup := a.Prepare(ctx)
		read = r
		defer cleanup()
	}

	s := a.Style
	// Everything shown comes from the model or the configuration: the
	// command, the path and the diff are revealed — hidden characters shown
	// as marked escapes — and the rest keeps text only, so what is approved
	// is exactly what will run.
	fmt.Fprintf(a.Out, "\n%s %s %s\n", s.Yellow("●"), s.Bold(VisibleLine(tool)), s.Dim(VisibleLine(summarizeArgs(tool, args))))
	if via := agent.PipelineOf(ctx); via != "" {
		fmt.Fprintf(a.Out, "  %s\n", s.Dim("asked by "+VisibleLine(via)))
	}
	if who := agent.SubagentOf(ctx); who != "" {
		fmt.Fprintf(a.Out, "  %s\n", s.Dim("asked by subagent: "+VisibleLine(who)))
	}
	if res.Reason != "" {
		why := VisibleLine(res.Reason)
		if res.Step != "" {
			why += " · policy step: " + res.Step
		}
		fmt.Fprintf(a.Out, "  %s\n", s.Dim(why))
	}
	if preview := a.preview(tool, args); preview != "" {
		fmt.Fprintln(a.Out, preview)
	}

	// Numbered answers only, and nothing chosen for an empty line: a letter
	// or Enter alone never approves. The spec is checked as every dialog is.
	choices := []Choice{{ID: ChoiceYes, Label: "Yes"}}
	if scope != "" {
		choices = append(choices, Choice{ID: "always", Label: "Yes, and don't ask again for " + reveal(scope) + " this session", Widening: true})
	}
	choices = append(choices, Choice{ID: ChoiceNo, Label: "No"})
	if approvalHidden(ctx, tool, args, res) {
		fmt.Fprintf(a.Out, "  %s\n", s.Yellow(HiddenWarning))
	}
	id, err := a.askNumbered(ctx, read, DialogSpec{Kind: DialogApproval, Title: tool, Choices: choices})
	if err != nil || id == "" {
		return false, err
	}
	// Each answer names the ask it answered, so the line says what it approved.
	answered := func(how string) {
		fmt.Fprintf(a.Out, "  %s\n", s.Dim(how+": "+askedWhat(ctx, tool, args)))
	}
	switch id {
	case "always":
		a.Session.Add(scope)
		agent.NoteAnswer(ctx, agent.Answer{By: agent.ByReviewer, Granted: scope})
		answered("always allowed " + reveal(scope))
		return true, nil
	case ChoiceYes:
		if res.Step == "destructive" {
			fmt.Fprintf(a.Out, "  %s\n", s.Bold("This cannot be undone. Really run it?"))
			id, err := a.askNumbered(ctx, read, DialogSpec{Kind: DialogConfirm, Title: "This cannot be undone",
				Choices: []Choice{{ID: ChoiceNo, Label: "No, don't run it"}, {ID: ChoiceYes, Label: "Yes, run it", Destructive: true}}})
			if id != ChoiceYes {
				answered("rejected")
				return false, err
			}
		}
		answered("accepted")
		return true, nil
	}
	answered("rejected")
	return false, nil
}

// askNumbered prints spec's numbered choices and reads until a number in
// range is given, returning its id. Anything else — a letter, an empty line —
// asks again. Input ending or the context ending is no answer.
func (a *Approver) askNumbered(ctx context.Context, read func() (string, bool), spec DialogSpec) (string, error) {
	spec, err := spec.Normalized()
	if err != nil {
		return "", err
	}
	s := a.Style
	for i, c := range spec.Choices {
		fmt.Fprintf(a.Out, "  %d. %s\n", i+1, c.Label)
	}
	for {
		fmt.Fprintf(a.Out, "  %s ", s.Dim(fmt.Sprintf("answer 1-%d:", len(spec.Choices))))
		if err := ctx.Err(); err != nil {
			fmt.Fprintln(a.Out)
			return "", err
		}
		line, ok := read()
		if !ok {
			fmt.Fprintln(a.Out)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return "", nil // input ended: nobody can say yes
		}
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n >= 1 && n <= len(spec.Choices) {
			fmt.Fprintln(a.Out)
			return spec.Choices[n-1].ID, nil
		}
		fmt.Fprintf(a.Out, "\n  %s\n", s.Dim("answer with a number"))
	}
}

// HiddenWarning is said above an approval whose call carries characters that
// do not print as themselves; they are shown as ⟨…⟩ escapes.
const HiddenWarning = "⚠ this call has hidden characters, shown as ⟨…⟩ escapes; check what it will do before approving"

// approvalHidden reports whether anything an approval is about carries a
// hidden character: every key and string in the arguments, a JSON string
// decoded, the reason, the scope, and who asked.
func approvalHidden(ctx context.Context, tool string, args json.RawMessage, res policy.Result) bool {
	for _, s := range []string{tool, res.Reason, res.Offer(), agent.PipelineOf(ctx), agent.SubagentOf(ctx)} {
		if HasHidden(s) {
			return true
		}
	}
	var v any
	if json.Unmarshal(args, &v) != nil {
		return HasHidden(string(args))
	}
	return valueHidden(v, 0)
}

func valueHidden(v any, depth int) bool {
	switch x := v.(type) {
	case string:
		if HasHidden(x) {
			return true
		}
		// A JSON string, such as a manifest, is read once decoded too.
		var inner any
		if depth < 4 && strings.ContainsAny(x, "{[") && json.Unmarshal([]byte(x), &inner) == nil {
			return valueHidden(inner, depth+1)
		}
	case map[string]any:
		for k, e := range x {
			if HasHidden(k) || valueHidden(e, depth) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if valueHidden(e, depth) {
				return true
			}
		}
	}
	return false
}

// readAnswer is the default line reader for tests and non-interactive callers.
// Interactive mode replaces it via Prepare. ok is false when input ended.
func (a *Approver) readAnswer() (string, bool) {
	if a.reader == nil {
		a.reader = bufio.NewReader(a.In)
	}
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return "", false
	}
	return line, true
}

// preview renders what the action will actually do.
func (a *Approver) preview(tool string, raw json.RawMessage) string {
	s := a.Style
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	str := func(k string) string {
		if v, found := m[k]; found {
			if v, isStr := v.(string); isStr {
				return v
			}
		}
		return ""
	}

	switch tool {
	case "edit":
		old, updated := str("old_string"), str("new_string")
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimRight(old, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", s.Red("- "+reveal(line)))
		}
		for _, line := range strings.Split(strings.TrimRight(updated, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", s.Green("+ "+reveal(line)))
		}
		return strings.TrimRight(b.String(), "\n")

	case "write":
		content := str("content")
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		var b strings.Builder
		shown := lines
		if len(lines) > 15 {
			shown = lines[:15]
		}
		for _, line := range shown {
			fmt.Fprintf(&b, "  %s\n", s.Green("+ "+reveal(line)))
		}
		if len(lines) > 15 {
			fmt.Fprintf(&b, "  %s\n", s.Dim(fmt.Sprintf("... %d more lines", len(lines)-15)))
		}
		return strings.TrimRight(b.String(), "\n")

	case "bash":
		return fmt.Sprintf("  %s", s.Dim("$ "+reveal(str("command"))))
	}
	return ""
}

// askedWhat names an ask for the line that records its answer: the tool, what
// it acts on, and the subagent that asked, if one did.
func askedWhat(ctx context.Context, tool string, args json.RawMessage) string {
	what := strings.TrimSpace(VisibleLine(tool) + " " + VisibleLine(summarizeArgs(tool, args)))
	if who := agent.SubagentOf(ctx); who != "" {
		what += " (subagent " + VisibleLine(who) + ")"
	}
	return what
}
