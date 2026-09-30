//go:build unix

package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// dockCount counts the input docks on screen: rows that are only a rule.
func ruleRows(screen string) int {
	n := 0
	for _, l := range strings.Split(screen, "\n") {
		if t := strings.TrimSpace(l); t != "" && strings.Trim(t, "─") == "" {
			n++
		}
	}
	return n
}

// A resize, down to 40 columns and back up, leaves exactly one dock — no
// copy of the old one above the new — and the transcript rewrapped for the
// new width.
func TestTUIResizeLeavesNoGhost(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 60, 24)
	r.send("please long\r")
	r.waitText("commodo consequat.")
	r.quiet(300 * time.Millisecond)
	r.typed("half-typed prompt")
	for _, size := range [][2]int{{40, 20}, {100, 30}, {45, 16}, {80, 24}} {
		r.resize(size[0], size[1])
		time.Sleep(150 * time.Millisecond)
		r.quiet(150 * time.Millisecond)
		screen := r.term.Text()
		if n := ruleRows(screen); n != 2 {
			t.Fatalf("at %dx%d the screen has %d rule rows, want the dock's 2:\n%s", size[0], size[1], n, r.term.Dump())
		}
		if strings.Count(screen, "shift+tab") != 1 || !strings.Contains(screen, "half-typed prompt") {
			t.Fatalf("at %dx%d the dock is wrong:\n%s", size[0], size[1], r.term.Dump())
		}
		for _, l := range r.term.Lines() {
			if w := len([]rune(l)); w > size[0] {
				t.Fatalf("a row is wider than %d: %q", size[0], l)
			}
		}
		t.Log(r.capture(fmt.Sprintf("after resizing to %dx%d", size[0], size[1])))
	}
	r.send("\r")
	r.waitText("You said: half-typed prompt")
}

// A resize storm while a reply streams leaves the reply whole and one dock.
func TestTUIResizeDuringAStream(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 24)
	r.send("please slow\r")
	r.waitScreen("word3")
	for i := 0; i < 12; i++ {
		r.resize(40+i*5, 18+i%4)
		time.Sleep(25 * time.Millisecond)
	}
	r.resize(70, 24)
	r.waitText("word59")
	r.quiet(300 * time.Millisecond)
	if n := ruleRows(r.term.Text()); n != 2 {
		t.Fatalf("%d rule rows after the storm:\n%s", n, r.term.Dump())
	}
	all := strings.Join(strings.Fields(r.term.All()), " ")
	for i := 0; i < 60; i++ {
		w := fmt.Sprintf("word%d", i)
		if !strings.Contains(all, w+" ") && !strings.HasSuffix(all, w) && !strings.Contains(all, w+"\n") {
			t.Fatalf("%s is missing after the storm:\n%s", w, r.term.All())
		}
	}
}

// At 40 columns everything is usable: the banner keeps its shape, the
// footer fits, a dialog's choices are whole.
func TestTUIFortyColumns(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 40, 24)
	for _, l := range r.term.Lines() {
		if len([]rune(l)) > 40 {
			t.Fatalf("row wider than 40: %q", l)
		}
	}
	if strings.Contains(r.term.Text(), "\n"+strings.Repeat(" ", 3)+"var/folders") {
		t.Fatalf("the workspace path wrapped under the mark:\n%s", r.term.Dump())
	}
	r.send("please edit\r")
	r.waitScreen("Make this edit")
	flat := strings.Join(strings.Fields(strings.ReplaceAll(r.term.Text(), "│", "")), "")
	if !strings.Contains(flat, "don'taskagainforedit(hello.txt)thissession") {
		t.Fatalf("the scope is cut at 40 columns:\n%s", r.term.Dump())
	}
	t.Log(r.capture("approval at 40 columns"))
}
