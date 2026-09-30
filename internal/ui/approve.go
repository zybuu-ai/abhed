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
	fmt.Fprintf(a.Out, "\n%s %s %s\n", s.Yellow("●"), s.Bold(sanitize(tool, false)), s.Dim(reveal(summarizeArgs(tool, args))))
	if via := agent.PipelineOf(ctx); via != "" {
		fmt.Fprintf(a.Out, "  %s\n", s.Dim("asked by "+sanitize(via, false)))
	}
	if who := agent.SubagentOf(ctx); who != "" {
		fmt.Fprintf(a.Out, "  %s\n", s.Dim("asked by subagent: "+sanitize(who, false)))
	}
	if res.Reason != "" {
		why := sanitize(res.Reason, false)
		if res.Step != "" {
			why += " · policy step: " + res.Step
		}
		fmt.Fprintf(a.Out, "  %s\n", s.Dim(why))
	}
	if preview := a.preview(tool, args); preview != "" {
		fmt.Fprintln(a.Out, preview)
	}

	// Numbered answers only, and nothing chosen for an empty line: a letter
	// or Enter alone never approves.
	choices := []string{"yes"}
	labels := []string{"Yes"}
	if scope != "" {
		choices = append(choices, "always")
		labels = append(labels, "Yes, and don't ask again for "+reveal(scope)+" this session")
	}
	choices = append(choices, "no")
	labels = append(labels, "No")
	id, err := a.askNumbered(ctx, read, labels, choices)
	if err != nil || id == "" {
		return false, err
	}
	switch id {
	case "always":
		a.Session.Add(scope)
		agent.NoteAnswer(ctx, agent.Answer{By: agent.ByReviewer, Granted: scope})
		return true, nil
	case "yes":
		if res.Step != "destructive" {
			return true, nil
		}
		fmt.Fprintf(a.Out, "  %s\n", s.Bold("This cannot be undone. Really run it?"))
		id, err := a.askNumbered(ctx, read, []string{"No, don't run it", "Yes, run it"}, []string{"no", "yes"})
		return id == "yes", err
	}
	return false, nil
}

// askNumbered prints numbered labels and reads until a number in range is
// given, returning its id. Anything else — a letter, an empty line — asks
// again. Input ending or the context ending is no answer.
func (a *Approver) askNumbered(ctx context.Context, read func() (string, bool), labels, ids []string) (string, error) {
	s := a.Style
	for i, l := range labels {
		fmt.Fprintf(a.Out, "  %d. %s\n", i+1, l)
	}
	for {
		fmt.Fprintf(a.Out, "  %s ", s.Dim(fmt.Sprintf("answer 1-%d:", len(labels))))
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
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && n >= 1 && n <= len(ids) {
			fmt.Fprintln(a.Out)
			return ids[n-1], nil
		}
		fmt.Fprintf(a.Out, "\n  %s\n", s.Dim("answer with a number"))
	}
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
