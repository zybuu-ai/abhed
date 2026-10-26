package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/debugrelay"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// TestDebugFakeAdapter is a debug adapter the debug tests run in place of
// dlv or debugpy: it answers initialize, the engine's launch and disconnect,
// and on launch asks to run something in the editor's terminal.
func TestDebugFakeAdapter(t *testing.T) {
	if os.Getenv("ABHED_FAKE_DAP") == "" {
		t.Skip("run by the debug tests")
	}
	br := bufio.NewReader(os.Stdin)
	seq := 0
	send := func(v map[string]any) {
		seq++
		v["seq"] = seq
		b, _ := json.Marshal(v)
		_ = debugrelay.Write(os.Stdout, b)
	}
	for {
		raw, err := debugrelay.Read(br)
		if err != nil {
			os.Exit(0)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		cmd, _ := m["command"].(string)
		switch {
		case m["type"] == "request" && cmd == "initialize":
			send(map[string]any{"type": "response", "request_seq": m["seq"], "command": cmd, "success": true})
		case m["type"] == "request" && cmd == "launch":
			args, _ := m["arguments"].(map[string]any)
			fmt.Fprintf(os.Stderr, "launched %v console=%v\n", args["program"], args["console"])
			send(map[string]any{"type": "request", "command": "runInTerminal", "arguments": map[string]any{"args": []string{"/bin/sh"}}})
			send(map[string]any{"type": "response", "request_seq": m["seq"], "command": cmd, "success": true})
			send(map[string]any{"type": "event", "event": "initialized"})
		case m["type"] == "response" && cmd == "runInTerminal":
			send(map[string]any{"type": "event", "event": "output", "body": map[string]any{"output": fmt.Sprintf("runInTerminal answered success=%v", m["success"])}})
		case m["type"] == "request" && cmd == "disconnect":
			send(map[string]any{"type": "response", "request_seq": m["seq"], "command": cmd, "success": true})
			// As debugpy's adapter does: it goes when its input closes, not before.
			if os.Getenv("ABHED_FAKE_DAP_WAIT") == "" {
				os.Exit(0)
			}
		case m["type"] == "request":
			send(map[string]any{"type": "response", "request_seq": m["seq"], "command": cmd, "success": true})
		}
	}
}

// fakeDebugger runs the fake adapter for every debug session, on a tier that
// can host one, and notes the command lines it was asked to run.
func fakeDebugger(t *testing.T, missing int, env ...string) *[]string {
	t.Helper()
	var asked []string
	oldCmd, oldTier := debugCommand, debugTier
	debugTier = func(tools.Bash) string { return "container" }
	debugCommand = func(_ tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
		asked = append(asked, command)
		if missing != 0 {
			return exec.CommandContext(ctx, "/bin/sh", "-c", fmt.Sprintf("echo 'abhed-debug: missing' >&2; exit %d", missing)) // #nosec G204 -- a test's own script
		}
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDebugFakeAdapter$") // #nosec G204 -- the test binary
		c.Env = append(append(os.Environ(), "ABHED_FAKE_DAP=1"), env...)
		c.Dir = cwd
		return c
	}
	t.Cleanup(func() { debugCommand, debugTier = oldCmd, oldTier })
	return &asked
}

func debugMessages(msgs []rpcMessage, method string) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		if m.Method == method {
			var p map[string]any
			_ = json.Unmarshal(m.Params, &p)
			out = append(out, p)
		}
	}
	return out
}

// §7.8: a Python program is debugged in the sandbox: the launch is the
// person's judged bash call, Studio's initialize reaches the adapter, the
// engine launches the program itself, Studio's own launch and the adapter's
// runInTerminal are refused, and stop ends it, each recorded.
func TestStudioDebugsAProgram(t *testing.T) {
	asked := fakeDebugger(t, 0)
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	var res struct {
		DebugID string `json:"debugId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py", "args": []string{"--x"}}, &res)
	if res.DebugID == "" || len(*asked) != 1 || !strings.Contains((*asked)[0], "debugpy.adapter") {
		t.Fatalf("launch %+v, asked %v", res, *asked)
	}
	send := func(m string) {
		r.cl.ok("_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(m)}, nil)
	}
	send(`{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"python"}}`)
	r.cl.waitFor(from, "launched", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"launched"`)
	})
	r.cl.waitFor(from, "runInTerminal refused", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/message" && strings.Contains(string(m.Params), "runInTerminal answered success=false")
	})
	send(`{"seq":2,"type":"request","command":"launch","arguments":{"program":"/etc/evil"}}`)
	r.cl.waitFor(from, "launch refused", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/message" && strings.Contains(string(m.Params), `"request_seq":2`) && strings.Contains(string(m.Params), `"success":false`)
	})
	for _, m := range debugMessages(r.cl.since(from), "_abhed/debug/message") {
		if strings.Contains(fmt.Sprint(m), "runInTerminal") && !strings.Contains(fmt.Sprint(m), "answered") {
			t.Fatalf("the adapter's runInTerminal reached Studio: %v", m)
		}
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
	r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"ended"`)
	})
	launched, _ := r.recorded(id, agent.EvDebugLaunched)
	stopped, _ := r.recorded(id, agent.EvDebugStopped)
	if len(launched) != 1 || launched[0]["language"] != "python" || !strings.Contains(launched[0]["command"].(string), "python3 "+r.ws+"/app.py --x") {
		t.Fatalf("debug.launched %v", launched)
	}
	if len(stopped) != 1 || stopped[0]["reason"] != "stopped" {
		t.Fatalf("debug.stopped %v", stopped)
	}
	reqs, actors := r.recorded(id, agent.EvActionRequested)
	found := false
	for i, q := range reqs {
		args, _ := q["args"].(map[string]any)
		found = found || q["tool"] == "bash" && actors[i] == agent.ActorUser && strings.HasPrefix(fmt.Sprint(args["command"]), "python3 ")
	}
	if !found {
		t.Fatalf("the launch is not the person's bash call: %v", reqs)
	}
	r.cl.refused(errParams, "_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(`{}`)})
}

// A deny rule on the debuggee's command line refuses the launch, and nothing starts.
func TestStudioDebugLaunchIsJudged(t *testing.T) {
	asked := fakeDebugger(t, 0)
	r := newStudioRig(t, `,"permissions":{"deny":["bash(python3 *)"]}`)
	id := r.open()
	r.write("app.py", "print('hi')\n")
	r.cl.refused(errPolicy, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"})
	if len(*asked) != 0 {
		t.Fatalf("an adapter started: %v", *asked)
	}
	// Outside the workspace, or in no language Abhed debugs, is refused as bad params.
	r.cl.refused(errParams, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "/etc/passwd"})
	r.cl.refused(errParams, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "ruby", "program": "a.rb"})
}

// A missing debugger ends the session with a problem that says what to do;
// a tier that cannot host a session refuses with one before anything runs.
func TestStudioDebugProblems(t *testing.T) {
	fakeDebugger(t, 98)
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	from := r.cl.mark()
	var res struct {
		DebugID string `json:"debugId"`
	}
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}, &res)
	st := r.cl.waitFor(from, "failed", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"failed"`)
	})
	if !strings.Contains(string(st.Params), "debugpy") || !strings.Contains(string(st.Params), "adapter-missing") {
		t.Fatalf("status %s", st.Params)
	}
	// Go needs the bundled Linux helper as its bridge; this build has none.
	bmsg := r.cl.refused(errRefused, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "go", "program": "."})
	if !strings.Contains(bmsg, "Linux build of Abhed") {
		t.Fatalf("bridge refusal %q", bmsg)
	}
	// The none tier has no sandbox to debug in (nor, on macOS, the process tier).
	debugTier = func(tools.Bash) string { return "none" }
	msg := r.cl.refused(errRefused, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"})
	if !strings.Contains(msg, "container tier") {
		t.Fatalf("tier refusal %q", msg)
	}
}

func TestDebugTierProblem(t *testing.T) {
	for tier, ok := range map[string]bool{"container": true, "vm": true, "process": runtime.GOOS == "linux", "none": false, "fence": false, "": false} {
		if (debugTierProblem(tier) == nil) != ok {
			t.Errorf("%q: problem %v", tier, debugTierProblem(tier))
		}
	}
}

// An adapter that waits for its input to close after answering disconnect,
// as debugpy's does, ends with the session: the engine closes its input, and
// the session is ended and recorded without a stop.
func TestStudioDebugEndsOnDisconnect(t *testing.T) {
	fakeDebugger(t, 0, "ABHED_FAKE_DAP_WAIT=1")
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	var res struct {
		DebugID string `json:"debugId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}, &res)
	send := func(m string) {
		r.cl.ok("_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(m)}, nil)
	}
	send(`{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"python"}}`)
	r.cl.waitFor(from, "launched", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"launched"`)
	})
	send(`{"seq":2,"type":"request","command":"disconnect","arguments":{"terminateDebuggee":true}}`)
	r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"ended"`)
	})
	stopped, _ := r.recorded(id, agent.EvDebugStopped)
	if len(stopped) != 1 || stopped[0]["reason"] != "ended" || stopped[0]["exit_code"] != float64(0) {
		t.Fatalf("debug.stopped %v", stopped)
	}
	// The session is gone: nothing more is taken for it.
	r.cl.refused(errParams, "_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(`{"seq":3,"type":"request","command":"threads"}`)})
}
