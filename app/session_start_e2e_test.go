package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// starts is each session.started in evs, decoded.
func starts(evs []agent.Event) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e.Type == agent.EvSessionStarted {
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			p["seq"] = float64(e.Seq)
			out = append(out, p)
		}
	}
	return out
}

const startRefused = "recording the session start"

// A session continued with -c, -r or -fork-session, in the terminal or with
// -p, records its own start after the record it goes on from, marked resumed.
func TestContinuedSessionRecordsItsStart(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-n", "codewords")
	g.ask(c, "Remember the codeword ZEBRA-41.")
	exit(c)
	id := g.sessions()[0].ID
	before := len(verified(t, g.record(), id))

	c = g.start("-c", "-mode", "accept-edits")
	g.ask(c, "What is the codeword?")
	exit(c)
	out, err := g.cmd("-r", "codewords", "-p", "And again?").CombinedOutput()
	if err != nil {
		t.Fatalf("-r -p: %v\n%s", err, out)
	}
	if strings.Contains(c.out.String()+string(out), startRefused) {
		t.Fatalf("a start was refused:\n%s\n%s", c.out.String(), out)
	}
	s := starts(verified(t, g.record(), id))
	if len(s) != 3 || s[0]["resumed"] != nil || s[0]["seq"] != float64(1) {
		t.Fatalf("starts: %v", s)
	}
	if s[1]["resumed"] != true || s[1]["through_seq"] != float64(before) || s[1]["mode"] != "accept-edits" || s[1]["headless"] != false {
		t.Fatalf("the -c start: %v", s[1])
	}
	if s[2]["resumed"] != true || s[2]["headless"] != true || s[2]["through_seq"].(float64) <= s[1]["seq"].(float64) {
		t.Fatalf("the -r -p start: %v", s[2])
	}

	// A fork is a new session: its start follows the copied conversation.
	for _, args := range [][]string{{"-r", id, "-fork-session", "-p", "Fork it"}, {"-r", id, "-fork-session"}} {
		known := map[string]bool{}
		for _, e := range g.sessions() {
			known[e.ID] = true
		}
		if args[len(args)-1] == "-fork-session" {
			c = g.start(args...)
			g.ask(c, "Fork it")
			exit(c)
			out = []byte(c.out.String())
		} else if out, err = g.cmd(args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if strings.Contains(string(out), startRefused) {
			t.Fatalf("%v: a start was refused:\n%s", args, out)
		}
		var branch string
		for _, e := range g.sessions() {
			if !known[e.ID] {
				branch = e.ID
			}
		}
		evs := verified(t, g.record(), branch)
		// The copy carries the source's starts; the fork's own comes after it.
		bs := starts(evs)
		own := bs[len(bs)-1]
		if evs[0].Type != agent.EvSessionBranched || len(bs) != len(s)+1 || own["resumed"] != true ||
			own["through_seq"] != own["seq"].(float64)-1 || own["headless"] != (args[len(args)-1] != "-fork-session") {
			t.Fatalf("%v: the fork's starts: %v\n%s", args, bs, out)
		}
	}
}

// A conversation rebuilt between turns, after /mode or /add-dir wrote to its
// record, does not record its start again.
func TestRebuildDoesNotRecordTheStartAgain(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "first")
	c.command("/mode plan", "plan")
	g.ask(c, "second")
	c.command("/mode default", "default")
	dir := realDir(t)
	c.command("/add-dir "+dir, "answer 1-")
	c.command("1", "added")
	g.ask(c, "third")
	exit(c)
	if strings.Contains(c.out.String(), startRefused) {
		t.Fatalf("a rebuild recorded the start again:\n%s", c.out.String())
	}
	id := g.sessions()[0].ID
	evs := verified(t, g.record(), id)
	if s := starts(evs); len(s) != 1 || s[0]["seq"] != float64(1) {
		t.Fatalf("starts: %v", s)
	}
	if !hasType(evs, agent.EvWorkspaceDirAdded) || !hasType(evs, agent.EvModeChanged) {
		t.Fatalf("the changes between turns are missing: %s", c.out.String())
	}
}

// A -p run records each hook that blocks as hook.fired, as the terminal does.
func TestHeadlessRecordsHookFired(t *testing.T) {
	g := newSessRig(t)
	guard := writeScript(t, `#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"tool_call"'*) echo '{"block":true,"reason":"no writes here"}' ;;
    *) echo '{}' ;;
  esac
done
`)
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + g.url +
		`","model":"m","context_window":8192}}},"extensions":[{"name":"guard","command":"bash","args":[` + jsonQuote(guard) + `],"events":["tool_call"]}]}`
	if err := os.WriteFile(filepath.Join(g.ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := g.cmd("-mode", "accept-edits", "-p", "write a.txt=hi").CombinedOutput()
	if _, err := os.Stat(filepath.Join(g.ws, "a.txt")); err == nil {
		t.Fatalf("the hook's block did not hold:\n%s", out)
	}
	evs := verified(t, g.record(), g.sessions()[0].ID)
	fired := 0
	for _, e := range evs {
		if e.Type == agent.EvHookFired {
			var f agent.HookFired
			_ = json.Unmarshal(e.Payload, &f)
			if f.Extension != "guard" || f.Event != "tool_call" || f.Verdict != "block" {
				t.Fatalf("hook.fired: %+v", f)
			}
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("hook.fired recorded %d times:\n%s", fired, out)
	}
}

// modeChangesIn is each recorded mode.changed as from>to/via.
func modeChangesIn(evs []agent.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == agent.EvModeChanged {
			var m agent.ModeChanged
			_ = json.Unmarshal(e.Payload, &m)
			out = append(out, m.From+">"+m.To+"/"+m.Via)
		}
	}
	return out
}

// A run that continues a record in another mode, or with another system
// prompt, says so in the record: its start, then the mode change. A copy
// resumed from an exported file does the same.
func TestContinuedRunRecordsItsModeAndPrompt(t *testing.T) {
	g := newSessRig(t)
	c := g.start("-mode", "accept-edits")
	g.ask(c, "Remember the codeword ZEBRA-41.")
	c.command("/export s.jsonl", "wrote ")
	exit(c)
	id := g.sessions()[0].ID

	out, err := g.cmd("-c", "-mode", "bypass", "-append-system-prompt", "be brief", "-p", "What is it?").CombinedOutput()
	if err != nil || strings.Contains(string(out), startRefused) {
		t.Fatalf("-c -p: %v\n%s", err, out)
	}
	c = g.start("-c")
	g.ask(c, "And now?")
	exit(c)
	evs := verified(t, g.record(), id)
	s := starts(evs)
	if len(s) != 3 || s[1]["mode"] != "bypass" || s[1]["system_prompt_appended_sha256"] == nil || s[2]["mode"] != "default" {
		t.Fatalf("starts: %v", s)
	}
	if got := strings.Join(modeChangesIn(evs), " "); got != "accept-edits>bypass/flag bypass>default/config" {
		t.Fatalf("mode changes: %s", got)
	}
	// Each change follows the start of the run that made it.
	for i, e := range evs {
		if e.Type == agent.EvModeChanged && evs[i-1].Type != agent.EvSessionStarted {
			t.Fatalf("a mode change at %d does not follow its run's start", e.Seq)
		}
	}

	known := map[string]bool{id: true}
	out, err = g.cmd("-r", filepath.Join(g.ws, "s.jsonl"), "-p", "From the file").CombinedOutput()
	if err != nil || strings.Contains(string(out), startRefused) {
		t.Fatalf("-r file -p: %v\n%s", err, out)
	}
	for _, e := range g.sessions() {
		if !known[e.ID] {
			bs := starts(verified(t, g.record(), e.ID))
			if own := bs[len(bs)-1]; own["resumed"] != true || own["headless"] != true {
				t.Fatalf("the copy's starts: %v", bs)
			}
			return
		}
	}
	t.Fatalf("-r file made no session:\n%s", out)
}

// A rule added between turns does not make the next turn record its start again.
func TestPermissionRuleBetweenTurnsKeepsOneStart(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "first")
	c.command("/permissions deny bash(curl *)", "rule added")
	g.ask(c, "second")
	exit(c)
	if strings.Contains(c.out.String(), startRefused) {
		t.Fatalf("the start was recorded again:\n%s", c.out.String())
	}
	if s := starts(verified(t, g.record(), g.sessions()[0].ID)); len(s) != 1 {
		t.Fatalf("starts: %v", s)
	}
}
