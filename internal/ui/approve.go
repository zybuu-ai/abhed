package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

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
	// in raw mode, like Claude Code, rather than a full line. Reading In here
	// instead would open a second reader racing the editor for each keystroke
	// and wait for a "\n" raw mode never sends (Enter is "\r"). read returns
	// ok=false when input ended or was cancelled, which is treated as a refusal.
	Prepare func(ctx context.Context) (read func() (string, bool), cleanup func())

	// reader is the lazily-created line reader for the non-interactive path.
	reader *bufio.Reader
}

// Prompter routes a typed line to a pending approval.
//
// The interactive loop runs one reader on stdin (the line editor) and consumes
// its lines in a select loop. An approval that read stdin itself would fight
// that reader for bytes. Instead the approver parks on Await, the reader loop
// hands the next line to Deliver, and there is still only one reader.
type Prompter struct {
	mu      sync.Mutex
	waiting chan string
	stop    chan struct{}
	once    sync.Once
}

// NewPrompter returns a Prompter ready to route approval input.
func NewPrompter() *Prompter { return &Prompter{stop: make(chan struct{})} }

// Await blocks until a line is delivered, the context is cancelled, or input
// ends. ok is false in the latter two cases.
func (p *Prompter) Await(ctx context.Context) (string, bool) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiting = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.waiting = nil
		p.mu.Unlock()
	}()
	select {
	case s := <-ch:
		return s, true
	case <-ctx.Done():
		return "", false
	case <-p.stop:
		return "", false
	}
}

// Deliver hands a line to a pending Await. It reports whether one was waiting,
// so the caller knows to consume the line rather than treat it as steering.
func (p *Prompter) Deliver(line string) bool {
	p.mu.Lock()
	ch := p.waiting
	p.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- line:
		return true
	default:
		return false
	}
}

// Close reports that input has ended, so any current or future Await refuses
// rather than blocking forever.
func (p *Prompter) Close() { p.once.Do(func() { close(p.stop) }) }

// AllowList holds scopes the user approved with "always" during this session.
type AllowList struct {
	scopes map[string]bool
}

func NewAllowList() *AllowList { return &AllowList{scopes: map[string]bool{}} }

func (a *AllowList) Add(scope string) { a.scopes[scope] = true }

func (a *AllowList) Has(scope string) bool { return a.scopes[scope] }

func NewApprover(out io.Writer) *Approver {
	return &Approver{In: os.Stdin, Out: out, Style: NewStyle(out), Session: NewAllowList()}
}

func (a *Approver) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	if res.Scope != "" && a.Session.Has(res.Scope) {
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
	fmt.Fprintf(a.Out, "\n%s %s %s\n", s.Yellow("●"), s.Bold(tool), s.Dim(summarizeArgs(tool, args)))
	if res.Reason != "" {
		fmt.Fprintf(a.Out, "  %s\n", s.Dim(res.Reason))
	}

	if preview := a.preview(tool, args); preview != "" {
		fmt.Fprintln(a.Out, preview)
	}

	options := "[a]ccept  [r]eject"
	if res.Scope != "" {
		options += fmt.Sprintf("  [A]lways allow %s", s.Dim(res.Scope))
	}
	fmt.Fprintf(a.Out, "  %s ", options)

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}

		line, ok := read()
		if !ok {
			// Input ended or was cancelled. A cancelled context is an error the
			// loop must see; an ended input is a refusal, not an error.
			if err := ctx.Err(); err != nil {
				return false, err
			}
			// EOF (piped input, no TTY): refuse rather than silently proceeding.
			fmt.Fprintln(a.Out)
			return false, nil
		}
		switch strings.TrimSpace(line) {
		case "a", "y", "":
			fmt.Fprintln(a.Out)
			return true, nil
		case "r", "n":
			fmt.Fprintln(a.Out)
			return false, nil
		case "A":
			if res.Scope != "" {
				a.Session.Add(res.Scope)
				fmt.Fprintln(a.Out)
				return true, nil
			}
			fmt.Fprintf(a.Out, "\n  no scope available; [a]ccept or [r]eject: ")
		default:
			// An unrecognised key just re-shows the choices. In raw mode a
			// single keypress arrives with no echo, so without this a stray key
			// looks like nothing happened.
			fmt.Fprintf(a.Out, "\n  %s ", options)
		}
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
			fmt.Fprintf(&b, "  %s\n", s.Red("- "+line))
		}
		for _, line := range strings.Split(strings.TrimRight(updated, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", s.Green("+ "+line))
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
			fmt.Fprintf(&b, "  %s\n", s.Green("+ "+line))
		}
		if len(lines) > 15 {
			fmt.Fprintf(&b, "  %s\n", s.Dim(fmt.Sprintf("... %d more lines", len(lines)-15)))
		}
		return strings.TrimRight(b.String(), "\n")

	case "bash":
		return fmt.Sprintf("  %s", s.Dim("$ "+str("command")))
	}
	return ""
}
