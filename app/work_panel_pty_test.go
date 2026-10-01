//go:build unix

package app

import (
	"strings"
	"testing"
	"time"
)

// since is what the CLI printed after the first n bytes.
func (r *ptyRun) since(n int) string { return r.text()[n:] }

// A background subagent gets a row under the input while it runs, with the
// hint; the arrows select it only with nothing typed, and the empty input
// then says who a message goes to.
func TestPanelRowsAndSelection(t *testing.T) {
	m := &bgModelServer{childDelay: time.Minute}
	ws := bgWorkspace(t, m.start(t), "")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("↑/↓ to select · Enter to view", 1)
	r.waitFor("general  child", 1)

	mark := len(r.text())
	r.send("abc")
	time.Sleep(200 * time.Millisecond)
	r.send("\x1b[B")
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(r.since(mark), "Message @general") {
		t.Fatalf("Down selected a row while something was typed:\n%s", r.since(mark))
	}
	r.send("\x15") // clear the line
	time.Sleep(200 * time.Millisecond)
	r.send("\x1b[B")
	r.waitFor("Message @general…", 1)
}

// Enter on a selected row opens its record read-only; Esc closes it.
func TestPanelEnterOpensRecord(t *testing.T) {
	m := &bgModelServer{childDelay: time.Minute}
	ws := bgWorkspace(t, m.start(t), "")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("general  child", 1)
	r.send("\x1b[B")
	r.waitFor("Message @general…", 1)
	r.send("\r")
	r.waitFor("general · child · read-only", 1)
	r.waitFor("\x1b[?1049h", 1)
	r.send("\x1b")
	r.waitFor("\x1b[?1049l", 1)
}

// A message typed with a running subagent selected goes to that subagent,
// as the person's message, and not to the conversation.
func TestPanelMessageGoesToSubagent(t *testing.T) {
	m := &bgModelServer{childDelay: 1500 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), `,"subagents":{"wake":"notify"}`)
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("general  child", 1)
	r.send("\x1b[B")
	r.waitFor("Message @general…", 1)
	r.send("hurry up\r")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, ok := m.prompted.Load("hurry up"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the subagent never got the message:\n%s", r.text())
		}
	}
	r.waitFor(`Agent "child" finished`, 1)
	if strings.Contains(r.text(), "has finished and cannot take messages") {
		t.Fatalf("the message went to main:\n%s", r.text())
	}
}

// A finished subagent cannot take a message: the input says so, and what
// is typed goes to the conversation.
func TestPanelFinishedRowSendsToMain(t *testing.T) {
	m := &bgModelServer{childDelay: 300 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), `,"subagents":{"wake":"notify"}`)
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor(`Agent "child" finished`, 1)
	r.waitFor("background work finished", 1)
	r.send("\x1b[B")
	r.waitFor("general can't take messages; a message goes to main", 1)
	r.send("hello there\r")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, ok := m.prompted.Load("hello there"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the conversation never got the message:\n%s", r.text())
		}
	}
}

// /tasks numbers the background work; "view" shows one and "kill" stops it,
// and /bashes is the same command.
func TestTasksNumbersViewKill(t *testing.T) {
	m := &bgModelServer{childDelay: time.Minute}
	ws := bgWorkspace(t, m.start(t), "")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("general  child", 1)
	r.send("/bashes\r")
	r.waitFor("/tasks view <n> shows one", 1)
	r.waitFor("background · running", 1)
	r.send("/tasks kill 1\r")
	r.waitFor("cancelled 1", 1)
	r.waitFor(`Agent "child" cancelled`, 1)
}

// In the line mode there is no panel: a finished background subagent is
// said in a line, and /tasks lists and shows the work.
func TestPanelLineMode(t *testing.T) {
	m := &bgModelServer{childDelay: 300 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), `,"subagents":{"wake":"notify"}`)
	d := startDumb(t, ws)
	d.send("go\r")
	d.wait(`Agent "child" finished`)
	d.send("/tasks\r")
	d.wait("/tasks view <n> shows one")
	d.send("/tasks view 1\r")
	d.wait("child result")
	if got := d.text(); strings.Contains(got, "to select") || strings.Contains(got, "\x1b[") {
		t.Fatalf("the line mode drew a panel or escapes:\n%q", got)
	}
}
