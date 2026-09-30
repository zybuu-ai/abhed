package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The terminal is the session's Surface: blocks join the transcript.
func TestTerminalSurfaceAppend(t *testing.T) {
	g := newRig(t, 60, 20)
	var s Surface = g.lr
	s.Append(Block{Kind: BlockError, Text: "no such provider"})
	s.Append(Block{Kind: BlockMarkdown, Text: "**bold** and `code`"})
	s.Append(Block{Kind: BlockDiff, Path: "a.go", Text: "-old\n+new"})
	g.waitText("new")
	all := g.term.All()
	for _, want := range []string{"✕ no such provider", "bold and code", "a.go", "-old", "+new"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "**") {
		t.Errorf("markdown not rendered:\n%s", all)
	}
}

// A Confirm dialog's default is No, and a cancelled one is no answer.
func TestTerminalSurfaceConfirm(t *testing.T) {
	dr := openDialog(t, DialogSpec{Kind: DialogConfirm, Title: "Switch to auto mode?", Ask: "Switch?"})
	dr.clock.advance(time.Second)
	dr.key("\r")
	if id, _ := dr.answered(); id != ChoiceNo {
		t.Fatalf("Enter on a confirm gave %q", id)
	}

	g := newRig(t, 60, 20)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := g.lr.Dialog(ctx, DialogSpec{Kind: DialogConfirm, Title: "t", Ask: "Sure?"})
		errc <- err
	}()
	g.waitText("Sure?")
	cancel()
	if err := <-errc; !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("cancelled dialog: %v", err)
	}
	// A default that widens is refused before anything is drawn.
	if _, err := g.lr.Dialog(context.Background(), DialogSpec{Kind: DialogChoice, Title: "x",
		Choices: []Choice{{ID: "all", Label: "all", Widening: true}}, Default: "all"}); err == nil {
		t.Fatal("a widening default was accepted")
	}
}

// A pick is chosen with the arrows and Enter; Esc is no answer.
func TestTerminalSurfacePick(t *testing.T) {
	g := newRig(t, 60, 16)
	clock := newFakeClock()
	g.lr.d.mu.Lock()
	g.lr.d.now = clock.Now
	g.lr.d.mu.Unlock()
	var items []PickItem
	for i := 0; i < 30; i++ {
		items = append(items, PickItem{ID: string(rune('a'+i%26)) + string(rune('0'+i/26)), Label: "session " + string(rune('a'+i%26))})
	}
	got := make(chan string, 1)
	go func() {
		id, err := g.lr.Pick(context.Background(), PickSpec{Title: "Resume which session?", Items: items})
		if err != nil {
			id = err.Error()
		}
		got <- id
	}()
	g.waitText("Resume which session?")
	if !strings.Contains(g.term.Text(), "↓") {
		t.Fatalf("a long list is not windowed:\n%s", g.term.Dump())
	}
	clock.advance(time.Second)
	g.keys("\x1b[B")
	g.settle()
	clock.advance(time.Second)
	g.keys("\r")
	if id := <-got; id != "b0" {
		t.Fatalf("picked %q", id)
	}
}

// A panel is the full-screen view; it returns when closed.
func TestTerminalSurfacePanel(t *testing.T) {
	g := newRig(t, 60, 16)
	done := make(chan error, 1)
	go func() {
		done <- g.lr.Panel(context.Background(), PanelSpec{Title: "Status", Body: []Block{{Kind: BlockNotice, Text: "model stub-1"}}})
	}()
	g.waitText("model stub-1")
	if !g.term.Mode("?1049") {
		t.Fatal("the panel is not on the alternate screen")
	}
	g.keys("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the panel did not close")
	}
}
