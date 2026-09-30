package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Surface is how a command or the session shows something to the person and
// asks them something. The terminal UI implements it; LineSurface is the
// fallback for piped input and dumb terminals. Callers never draw directly.
type Surface interface {
	// Append adds a block to the transcript.
	Append(Block)
	// Dialog asks one question and returns the chosen Choice's ID. It is
	// guarded: no answer, input ending or a cancelled context returns
	// ErrNoAnswer, never a choice, and a Confirm dialog's default is "no".
	Dialog(ctx context.Context, d DialogSpec) (string, error)
	// Pick chooses one item from a list (sessions, rewind points, models,
	// rules) and returns its ID, or ErrNoAnswer.
	Pick(ctx context.Context, p PickSpec) (string, error)
	// Panel shows a read-only, scrollable view (help, context, status, tasks)
	// and returns when the person closes it.
	Panel(ctx context.Context, p PanelSpec) error
	// SetStatus replaces what the footer shows.
	SetStatus(StatusModel)
	// Notify shows a short message that does not join the transcript.
	Notify(Toast)
}

// ErrNoAnswer is a Dialog or Pick that ended without the person choosing:
// input ended, the context was cancelled, or the answer never matched. The
// caller treats it as a refusal.
var ErrNoAnswer = errors.New("no answer")

// BlockKind is what a Block holds, which decides how it is drawn.
type BlockKind string

const (
	BlockNotice   BlockKind = "notice"
	BlockError    BlockKind = "error"
	BlockMarkdown BlockKind = "markdown"
	BlockTable    BlockKind = "table"
	BlockDiff     BlockKind = "diff"     // Text is a unified diff
	BlockToolOut  BlockKind = "tool_out" // Text is a tool's redacted output
)

// Block is one piece of the transcript.
type Block struct {
	Kind BlockKind
	Text string
	// Rows is a table's cells, the first row its header.
	Rows [][]string
	// Path is the file a diff or tool output is about, relative to the workspace.
	Path string
}

// DialogKind is the shape of a Dialog.
type DialogKind string

const (
	// DialogConfirm is a yes-or-no question. Without Choices it offers
	// "yes" and "no". Its default is always "no": any other is refused.
	DialogConfirm DialogKind = "confirm"
	// DialogChoice is one of the given Choices.
	DialogChoice DialogKind = "choice"
	// DialogApproval is a tool call waiting on the person. Its answers still
	// go through the approver's rules; the dialog only collects them. Its
	// default is "no" or none, so a bare Enter never approves.
	DialogApproval DialogKind = "approval"
)

// Choice is one answer a Dialog offers.
type Choice struct {
	ID    string // returned when chosen
	Label string
	// Key is a single key that picks it on a terminal, 0 for none.
	Key rune
	// Destructive choices cannot be undone. They are never the default and
	// need a second, explicit confirmation.
	Destructive bool
	// Widening choices grant more than was asked (an "always" scope, a
	// broader mode). They are never the default.
	Widening bool
}

// DialogSpec is one question.
type DialogSpec struct {
	Kind    DialogKind
	Title   string
	Body    []Block
	Choices []Choice
	// Default is the ID taken on an empty answer; "" means an answer is required.
	Default string
	// Why says who is asking and on what grounds: step · rule · reason · asked by.
	Why string
}

// Confirm choice IDs.
const (
	ChoiceYes = "yes"
	ChoiceNo  = "no"
)

// Normalized fills in a Confirm dialog's choices and default and checks the
// guard rules every Surface relies on: IDs and keys are unique, the default
// is one of the choices, and it is neither destructive nor widening. A
// Confirm dialog's default can only be "no", and an Approval's only "no" or
// none, so a bare Enter can never say yes to either.
func (d DialogSpec) Normalized() (DialogSpec, error) {
	if d.Kind == DialogConfirm {
		if len(d.Choices) == 0 {
			d.Choices = []Choice{{ID: ChoiceYes, Label: "Yes", Key: 'y'}, {ID: ChoiceNo, Label: "No", Key: 'n'}}
		}
		if d.Default == "" {
			d.Default = ChoiceNo
		}
	}
	if len(d.Choices) == 0 {
		return d, errors.New("a dialog needs at least one choice")
	}
	switch {
	case d.Kind == DialogConfirm && d.Default != ChoiceNo:
		return d, fmt.Errorf("a confirm dialog's default must be %q, not %q", ChoiceNo, d.Default)
	case d.Kind == DialogApproval && d.Default != "" && d.Default != ChoiceNo:
		return d, fmt.Errorf("an approval's default must be %q or none, not %q", ChoiceNo, d.Default)
	}
	ids, keys := map[string]bool{}, map[rune]bool{}
	var def *Choice
	for i, c := range d.Choices {
		if c.ID == "" || ids[c.ID] {
			return d, fmt.Errorf("dialog choice %d has an empty or repeated id %q", i+1, c.ID)
		}
		// A typed number is the choice's place in the list; a key or an id
		// that is a number too would pick a different choice by the same answer.
		if _, err := strconv.Atoi(c.ID); err == nil {
			return d, fmt.Errorf("dialog choice id %q is a number, which reads as a place in the list", c.ID)
		}
		if unicode.IsDigit(c.Key) {
			return d, fmt.Errorf("dialog key %q is a digit, which reads as a place in the list", c.Key)
		}
		ids[c.ID] = true
		if c.Key != 0 {
			if keys[c.Key] {
				return d, fmt.Errorf("dialog key %q is used twice", c.Key)
			}
			keys[c.Key] = true
		}
		if c.ID == d.Default {
			def = &d.Choices[i]
		}
	}
	switch {
	case d.Default == "":
	case def == nil:
		return d, fmt.Errorf("dialog default %q is not one of its choices", d.Default)
	case def.Destructive || def.Widening:
		return d, fmt.Errorf("dialog default %q widens or cannot be undone, so it cannot be the default", d.Default)
	}
	return d, nil
}

// choice is the choice with id.
func (d DialogSpec) choice(id string) (Choice, bool) {
	for _, c := range d.Choices {
		if c.ID == id {
			return c, true
		}
	}
	return Choice{}, false
}

// match finds the choice an answer names: its number in the list, its key or
// its ID.
func (d DialogSpec) match(answer string) (Choice, bool) {
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(d.Choices) {
		return d.Choices[n-1], true
	}
	for _, c := range d.Choices {
		if strings.EqualFold(answer, c.ID) || c.Key != 0 && len([]rune(answer)) == 1 && []rune(answer)[0] == c.Key {
			return c, true
		}
	}
	return Choice{}, false
}

// PickItem is one entry of a Pick.
type PickItem struct {
	ID     string
	Label  string
	Detail string // a second, dimmer column: age, branch, size
}

// PickSpec is a list to choose one entry from.
type PickSpec struct {
	Title string
	Items []PickItem
	// Default is the ID taken on an empty answer; "" means an answer is required.
	Default string
}

// PanelSpec is a read-only view.
type PanelSpec struct {
	Title string
	Body  []Block
}

// Toast is a short notice outside the transcript.
type Toast struct {
	Text string
	Warn bool
}
