//go:build unix

package app

import (
	"strings"
	"testing"
	"time"
)

// At the prompt, a background result is drawn when it arrives, with no
// task running: the subscription is the conversation's, not a turn's.
func TestCLIIdleNoticeRendered(t *testing.T) {
	m := &bgModelServer{childDelay: 800 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), "")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("still running", 1)
	r.waitFor("background: child finished (completed", 1)
	r.waitFor("background work finished", 1)
}

// Ctrl-C at the prompt with background tasks running warns first, and a
// second within two seconds cancels them.
func TestCLIDoubleCtrlCAtPromptCancelsChildren(t *testing.T) {
	m := &bgModelServer{childDelay: time.Minute}
	ws := bgWorkspace(t, m.start(t), "")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("still running", 1)
	time.Sleep(200 * time.Millisecond)
	r.send("\x03")
	r.waitFor("Ctrl-C again within 2 s to cancel them", 1)
	r.send("\x03")
	r.waitFor("cancelled 1 background task(s)", 1)
	r.waitFor("background: child finished (cancelled", 1)
}

// Leaving the session cancels its background tasks and says how many.
func TestCLIExitCancelsAsSessionClosed(t *testing.T) {
	m := &bgModelServer{childDelay: time.Minute}
	ws := bgWorkspace(t, m.start(t), "")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("still running", 1)
	r.send("/tasks\r")
	r.waitFor("running", 2)
	r.send("/exit\r")
	r.waitFor("cancelling 1 background task(s)", 1)
	r.waitFor("1 background task(s) ended as the session closed", 1)
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("did not exit:\n%s", r.text())
	}
}

// With wake auto, an idle result starts a wake run through the same driver
// as a task, while nothing is typed.
func TestCLIAutoWake(t *testing.T) {
	m := &bgModelServer{childDelay: 800 * time.Millisecond}
	ws := bgWorkspace(t, m.start(t), `,"subagents":{"wake":"auto"}`)
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Type a task", 1)
	r.send("go\r")
	r.waitFor("woke to act on background results", 1)
	r.waitFor("noted", 1)
	if strings.Count(r.text(), "woke to act") != 1 {
		t.Fatalf("woke more than once:\n%s", r.text())
	}
}
