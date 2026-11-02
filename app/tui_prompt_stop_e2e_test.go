//go:build unix

package app

import (
	"strings"
	"testing"
	"time"
)

// A message stopped with Esc while the prompt hook decides is never sent:
// not then, and not with the next message.
func TestTUIStopWhileScreenedSendsNothing(t *testing.T) {
	gate := writeScript(t, `#!/bin/bash
while IFS= read -r line; do
  case "$line" in *unscreened*) sleep 3 ;; esac
  echo '{}'
done
`)
	stub, ws := tuiWorkspace(t, `,"extensions":[{"name":"gate","command":"bash","args":[`+jsonQuote(gate)+`],"events":["user_prompt_submit"],"timeout_ms":10000}]`)
	r := startTUI(t, stub, ws, 100, 30)
	r.waitText("for shortcuts")
	r.typed("an unscreened plan\r")
	time.Sleep(500 * time.Millisecond) // the hook is deciding
	r.send("\x1b")
	time.Sleep(4 * time.Second) // the hook's answer, owed, is read and set aside
	r.typed("hello\r")
	r.waitFor("the answer to hello", true, func(s string) bool { return stub.prompt() == "hello" })
	stub.mu.Lock()
	bodies := append([]string(nil), stub.bodies...)
	stub.mu.Unlock()
	for _, b := range bodies {
		if strings.Contains(b, "unscreened") {
			t.Fatalf("a message stopped while screened reached the model:\n%s", r.capture("after"))
		}
	}
}
