package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// script answers with its lines in order, then reports input ended.
type script []string

func (s *script) Await(context.Context) (string, bool) {
	if len(*s) == 0 {
		return "", false
	}
	line := (*s)[0]
	*s = (*s)[1:]
	return line, true
}

func lineSurface(answers ...string) (*LineSurface, *bytes.Buffer) {
	var out bytes.Buffer
	sc := script(answers)
	return NewLineSurface(&out, Style{}, &sc), &out
}

var approval = DialogSpec{
	Kind:  DialogApproval,
	Title: "Run rm -rf build?",
	Choices: []Choice{
		{ID: "once", Label: "Yes", Key: 'y'},
		{ID: "always", Label: "Yes, always bash(rm *)", Key: 'a', Widening: true},
		{ID: "no", Label: "No", Key: 'n'},
	},
	Default: "no",
	Why:     "step default · no rule · asked by main",
}

// The guard: nothing but a typed answer picks a choice, and nothing typed by
// accident widens or destroys.
func TestLineDialogNeverApprovesByItself(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		spec    DialogSpec
		answers []string
		want    string
		err     error
	}{
		{"input ended", approval, nil, "", ErrNoAnswer},
		{"empty takes the safe default", approval, []string{""}, "no", nil},
		{"confirm defaults to no", DialogSpec{Kind: DialogConfirm, Title: "Switch to auto?"}, []string{""}, ChoiceNo, nil},
		{"confirm takes y", DialogSpec{Kind: DialogConfirm}, []string{"y"}, ChoiceYes, nil},
		{"no default needs an answer, then input ends", DialogSpec{Kind: DialogChoice, Choices: approval.Choices}, []string{"", ""}, "", ErrNoAnswer},
		{"by number", approval, []string{"1"}, "once", nil},
		{"by key", approval, []string{"a"}, "always", nil},
		{"by id", approval, []string{"NO"}, "no", nil},
		{"garbage three times", approval, []string{"yes please", "0", "4"}, "", ErrNoAnswer},
		{"garbage then a number", approval, []string{"sure", "3"}, "no", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, _ := lineSurface(c.answers...)
			got, err := l.Dialog(ctx, c.spec)
			if got != c.want || !errors.Is(err, c.err) {
				t.Fatalf("got %q, %v; want %q, %v", got, err, c.want, c.err)
			}
		})
	}
}

// A destructive choice needs a second, explicit yes; anything else falls back
// to the safe default or no answer.
func TestLineDialogConfirmsDestructiveChoices(t *testing.T) {
	spec := DialogSpec{Kind: DialogChoice, Title: "Restore?", Choices: []Choice{
		{ID: "restore", Label: "Restore code and conversation", Destructive: true},
		{ID: "cancel", Label: "Cancel"},
	}, Default: "cancel"}
	for _, c := range []struct {
		answers []string
		want    string
		err     error
	}{
		{[]string{"1", "yes"}, "restore", nil},
		{[]string{"1", "YES"}, "restore", nil},
		{[]string{"1", ""}, "cancel", nil},
		{[]string{"1", "y"}, "cancel", nil},
		{[]string{"1"}, "cancel", nil},
	} {
		l, out := lineSurface(c.answers...)
		got, err := l.Dialog(context.Background(), spec)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%q: got %q, %v", c.answers, got, err)
		}
		if !strings.Contains(out.String(), "type yes to confirm") {
			t.Errorf("%q: no confirmation was asked:\n%s", c.answers, out)
		}
	}
	noDefault := spec
	noDefault.Default = ""
	l, _ := lineSurface("1", "no")
	if got, err := l.Dialog(context.Background(), noDefault); got != "" || !errors.Is(err, ErrNoAnswer) {
		t.Errorf("an unconfirmed destructive choice with no default gave %q, %v", got, err)
	}
}

// A dialog that would make a widening or destructive choice the default is
// refused before anything is shown.
func TestDialogSpecRefusesUnsafeDefaults(t *testing.T) {
	for _, d := range []DialogSpec{
		{Kind: DialogChoice, Choices: approval.Choices, Default: "always"},
		{Kind: DialogChoice, Choices: []Choice{{ID: "wipe", Destructive: true}}, Default: "wipe"},
		{Kind: DialogChoice, Choices: approval.Choices, Default: "missing"},
		{Kind: DialogChoice},
		{Kind: DialogChoice, Choices: []Choice{{ID: "a"}, {ID: "a"}}},
		{Kind: DialogChoice, Choices: []Choice{{ID: "a", Key: 'x'}, {ID: "b", Key: 'x'}}},
		{Kind: DialogConfirm, Choices: []Choice{{ID: ChoiceYes, Widening: true}, {ID: ChoiceNo}}, Default: ChoiceYes},
	} {
		l, out := lineSurface("1")
		if got, err := l.Dialog(context.Background(), d); err == nil || got != "" {
			t.Errorf("%+v was asked: %q", d, got)
		}
		if out.Len() != 0 {
			t.Errorf("an invalid dialog drew %q", out)
		}
	}
}

// Without an answer source, or with the context gone, nothing is chosen.
func TestLineDialogWithoutInput(t *testing.T) {
	l := NewLineSurface(&bytes.Buffer{}, Style{}, nil)
	if got, err := l.Dialog(context.Background(), approval); got != "" || !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("nil input gave %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l2, _ := lineSurface("1")
	if got, err := l2.Dialog(ctx, approval); got != "" || !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a cancelled context gave %q, %v", got, err)
	}
}

// Through the session's Prompter: a delivered line answers, and closed input
// is no answer rather than a hang.
func TestLineDialogThroughThePrompter(t *testing.T) {
	p := NewPrompter()
	l := NewLineSurface(&bytes.Buffer{}, Style{}, p)
	done := make(chan string)
	go func() {
		id, _ := l.Dialog(context.Background(), approval)
		done <- id
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !p.Deliver("1") {
		if time.Now().After(deadline) {
			t.Fatal("the dialog never waited on the prompter")
		}
		time.Sleep(time.Millisecond)
	}
	if id := <-done; id != "once" {
		t.Fatalf("got %q", id)
	}
	p.Close()
	if id, err := l.Dialog(context.Background(), approval); id != "" || !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("after input closed: %q, %v", id, err)
	}
}

// Text from a model or a file cannot drive the terminal through a dialog.
func TestLineSurfaceStripsControlSequences(t *testing.T) {
	l, out := lineSurface("")
	_, _ = l.Dialog(context.Background(), DialogSpec{Kind: DialogConfirm,
		Title: "plan\x1b]52;c;ZXZpbA==\x07",
		Body:  []Block{{Kind: BlockMarkdown, Text: "step one\x1b[2J\nstep two"}}})
	l.Append(Block{Kind: BlockToolOut, Text: "\x1b]0;title\x07done"})
	l.Notify(Toast{Text: "\x9b31mhi"})
	for _, bad := range []string{"\x1b", "\x07", "\x9b"} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("%q reached the terminal: %q", bad, out)
		}
	}
	if !strings.Contains(out.String(), "step two") {
		t.Fatalf("the body was lost: %q", out)
	}
}

func TestLinePick(t *testing.T) {
	items := []PickItem{{ID: "s-1", Label: "fix tests", Detail: "2h"}, {ID: "s-2", Label: "docs"}}
	for _, c := range []struct {
		answers []string
		def     string
		want    string
		err     error
	}{
		{[]string{"2"}, "", "s-2", nil},
		{[]string{"s-1"}, "", "s-1", nil},
		{[]string{""}, "s-1", "s-1", nil},
		{[]string{"", "", ""}, "", "", ErrNoAnswer},
		{nil, "s-1", "", ErrNoAnswer},
	} {
		l, _ := lineSurface(c.answers...)
		got, err := l.Pick(context.Background(), PickSpec{Title: "Resume", Items: items, Default: c.def})
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%q: got %q, %v", c.answers, got, err)
		}
	}
	l, _ := lineSurface("1")
	if _, err := l.Pick(context.Background(), PickSpec{}); !errors.Is(err, ErrNoAnswer) {
		t.Error("an empty pick chose something")
	}
}

func TestLineSurfaceBlocksAndStatus(t *testing.T) {
	l, out := lineSurface()
	l.Append(Block{Kind: BlockTable, Rows: [][]string{{"rule", "layer"}, {"bash(go test*)", "session"}}})
	l.Append(Block{Kind: BlockDiff, Path: "a.go", Text: "-old\n+new"})
	l.Append(Block{Kind: BlockError, Text: "refused"})
	_ = l.Panel(context.Background(), PanelSpec{Title: "Status", Body: []Block{{Kind: BlockNotice, Text: "mode default"}}})
	for _, want := range []string{"rule            layer", "bash(go test*)  session", "a.go", "-old", "+new", "✕ refused", "Status", "mode default"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	l.SetStatus(StatusModel{Mode: "plan"})
	if l.Status().Mode != "plan" {
		t.Error("the status was not kept")
	}
}
