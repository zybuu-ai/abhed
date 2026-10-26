package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/debugrelay"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
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
		case m["type"] == "request" && cmd == "initialize" && os.Getenv("ABHED_FAKE_DAP_JUNK") != "":
			// A header line with no end, as a debuggee writing to the adapter's
			// stdout could send; then it stays, holding its pipe.
			_, _ = os.Stdout.WriteString("Content-Length: 2" + strings.Repeat("x", 4096))
			select {}
		case m["type"] == "request" && cmd == "initialize":
			send(map[string]any{"type": "response", "request_seq": m["seq"], "command": cmd, "success": true})
		case m["type"] == "request" && cmd == "launch" && os.Getenv("ABHED_FAKE_DAP_FAIL") != "":
			// As Delve does: a short message, and the reason in body.error.
			send(map[string]any{"type": "response", "request_seq": m["seq"], "command": cmd, "success": false, "message": "Failed to launch",
				"body": map[string]any{"error": map[string]any{"id": 3000, "format": "Failed to launch: {e}", "variables": map[string]string{"e": "Go version go1.27 is too new for this Delve"}}}})
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
	closed, open, allow := tools.Isolation{}, tools.Isolation{Network: true}, tools.Isolation{Allowlist: true}
	for _, c := range []struct {
		tier string
		iso  tools.Isolation
		ok   bool
	}{
		{"container", open, true}, {"vm", open, true},
		{"process", closed, runtime.GOOS == "linux"}, {"process", allow, runtime.GOOS == "linux"},
		// Open, bubblewrap shares the host's network namespace: refused everywhere.
		{"process", open, false},
		{"none", closed, false}, {"fence", closed, false}, {"", closed, false},
	} {
		if (debugTierProblem(c.tier, c.iso) == nil) != c.ok {
			t.Errorf("%q %+v: problem %v", c.tier, c.iso, debugTierProblem(c.tier, c.iso))
		}
	}
	if runtime.GOOS == "linux" {
		if p := debugTierProblem("process", open); p == nil || !strings.Contains(p.Message, "network closed or allowlisted") || len(p.Steps) == 0 {
			t.Fatalf("open network on the Linux process tier: %+v", p)
		}
	}
}

// withSupervisor gives the session's loop a judge.
func withSupervisor(t *testing.T, r *studioRig, id string, g *monitor.Guard) {
	t.Helper()
	s := r.cl.conn.session(id)
	if s == nil || !s.inner {
		t.Fatal("no inner session")
	}
	s.parts.Loop.Monitor = g
}

func judging(d policy.Decision, code string, err error) monitor.Func {
	return func(context.Context, monitor.Case) (monitor.Verdict, error) {
		if err != nil {
			return monitor.Verdict{}, err
		}
		return monitor.Verdict{Decision: d, Code: code, Rationale: "the judge's reason"}, nil
	}
}

// The supervisor may refuse a launch: refused as policy, no adapter
// started, and its verdict in the record for the person's call. A judge
// that cannot answer under a fail-closed setting refuses too.
func TestStudioDebugLaunchSupervisorRefuses(t *testing.T) {
	for _, g := range []*monitor.Guard{
		{Monitor: judging(policy.Deny, "out-of-remit", nil)},
		{Monitor: judging("", "", errors.New("judge down")), OnUnavailable: policy.Deny},
	} {
		asked := fakeDebugger(t, 0)
		r := newStudioRig(t, "")
		id := r.open()
		r.write("app.py", "print('hi')\n")
		withSupervisor(t, r, id, g)
		r.cl.refused(errPolicy, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"})
		if len(*asked) != 0 {
			t.Fatalf("an adapter started: %v", *asked)
		}
		verdicts, _ := r.recorded(id, agent.EvMonitorVerdict)
		if len(verdicts) != 1 || !strings.HasPrefix(fmt.Sprint(verdicts[0]["call_id"]), "u") || verdicts[0]["after"] != "deny" {
			t.Fatalf("verdicts %v", verdicts)
		}
		if launched, _ := r.recorded(id, agent.EvDebugLaunched); len(launched) != 0 {
			t.Fatalf("debug.launched %v", launched)
		}
	}
}

// A supervisor's ask is put to the person as a permission request; the
// launch goes ahead only on Allow, and a Deny or no answer refuses it.
func TestStudioDebugLaunchSupervisorAsksThePerson(t *testing.T) {
	for _, c := range []struct {
		answer string // optionId prefix, or "" to cancel
		ok     bool
	}{{"once:", true}, {"reject:", false}, {"", false}} {
		asked := fakeDebugger(t, 0)
		r := newStudioRig(t, "")
		id := r.open()
		r.write("app.py", "print('hi')\n")
		withSupervisor(t, r, id, &monitor.Guard{Monitor: judging(policy.Ask, "unknown-program", nil)})
		var perms []string
		r.cl.answering(func(method string, params json.RawMessage) any {
			if method != "session/request_permission" {
				return map[string]any{}
			}
			var p struct {
				ToolCall struct {
					ToolCallID string `json:"toolCallId"`
					Title      string `json:"title"`
				} `json:"toolCall"`
			}
			_ = json.Unmarshal(params, &p)
			perms = append(perms, p.ToolCall.Title)
			if c.answer == "" {
				return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
			}
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": c.answer + p.ToolCall.ToolCallID}}
		})
		params := map[string]any{"sessionId": id, "language": "python", "program": "app.py"}
		if c.ok {
			r.cl.ok("_abhed/debug/launch", params, nil)
		} else {
			r.cl.refused(errPolicy, "_abhed/debug/launch", params)
		}
		if len(perms) != 1 || !strings.Contains(perms[0], "debug: python3 ") {
			t.Fatalf("%q: permission requests %v", c.answer, perms)
		}
		if (len(*asked) == 1) != c.ok {
			t.Fatalf("%q: adapters %v", c.answer, *asked)
		}
		approved, _ := r.recorded(id, agent.EvActionApproved)
		if c.ok && (len(approved) != 1 || approved[0]["confirmed"] != "true" || approved[0]["step"] != "monitor") {
			t.Fatalf("approval %v", approved)
		}
		if c.ok {
			r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
		}
	}
}

// Parallel launches cannot pass the bound while one is being judged.
func TestDebugSlotsAreReserved(t *testing.T) {
	c := &acpConn{}
	for i := range maxDebugs {
		if !c.reserveDebug() {
			t.Fatalf("slot %d refused", i)
		}
	}
	if c.reserveDebug() {
		t.Fatal("a fifth slot was given")
	}
	c.releaseDebug()
	if !c.reserveDebug() {
		t.Fatal("a released slot was not given again")
	}
}

// Studio's sends reach the adapter in the order they were sent, without
// waiting for each reply; a session closed with its session says closed.
func TestStudioDebugSendsInOrderAndCloses(t *testing.T) {
	fakeDebugger(t, 0, "ABHED_FAKE_DAP_WAIT=1")
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	var res struct {
		DebugID string `json:"debugId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}, &res)
	for i := 1; i <= 30; i++ {
		raw, _ := json.Marshal(map[string]any{"debugId": res.DebugID, "message": json.RawMessage(fmt.Sprintf(`{"seq":%d,"type":"request","command":"threads"}`, i))})
		r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf(`"s%d"`, i)), Method: "_abhed/debug/send", Params: raw})
	}
	r.cl.waitFor(from, "the last answer", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/message" && strings.Contains(string(m.Params), `"request_seq":30`)
	})
	next := 1
	for _, m := range debugMessages(r.cl.since(from), "_abhed/debug/message") {
		msg, _ := m["message"].(map[string]any)
		if msg["command"] == "threads" {
			if int(msg["request_seq"].(float64)) != next {
				t.Fatalf("answer %v came out of order, wanted %d", msg["request_seq"], next)
			}
			next++
		}
	}
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	stopped, _ := r.recorded(id, agent.EvDebugStopped)
	if len(stopped) != 1 || stopped[0]["reason"] != "closed" {
		t.Fatalf("debug.stopped %v", stopped)
	}
}

// A launch Delve refuses says why: the status carries body.error, filled in.
func TestStudioDebugFailedLaunchSaysWhy(t *testing.T) {
	fakeDebugger(t, 0, "ABHED_FAKE_DAP_FAIL=1")
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	var res struct {
		DebugID string `json:"debugId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}, &res)
	r.cl.ok("_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(`{"seq":1,"type":"request","command":"initialize"}`)}, nil)
	st := r.cl.waitFor(from, "failed", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"failed"`)
	})
	if !strings.Contains(string(st.Params), "Go version go1.27 is too new for this Delve") {
		t.Fatalf("status %s", st.Params)
	}
	r.cl.ok("_abhed/debug/stop", map[string]any{"debugId": res.DebugID}, nil)
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

// Output the relay cannot read ends the session at once, as §7.8 says,
// rather than leaving the adapter blocked on a pipe nobody reads.
func TestStudioDebugUnreadableOutputEndsTheSession(t *testing.T) {
	fakeDebugger(t, 0, "ABHED_FAKE_DAP_JUNK=1")
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	var res struct {
		DebugID string `json:"debugId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}, &res)
	r.cl.ok("_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(`{"seq":1,"type":"request","command":"initialize"}`)}, nil)
	st := r.cl.waitFor(from, "failed", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"failed"`)
	})
	if !strings.Contains(string(st.Params), "could not be read") {
		t.Fatalf("status %s", st.Params)
	}
	stopped, _ := r.recorded(id, agent.EvDebugStopped)
	if len(stopped) != 1 || stopped[0]["reason"] != "failed" {
		t.Fatalf("debug.stopped %v", stopped)
	}
}

// Every send is answered, those queued as the session ends included.
func TestStudioDebugEverySendIsAnswered(t *testing.T) {
	fakeDebugger(t, 0, "ABHED_FAKE_DAP_WAIT=1")
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	var res struct {
		DebugID string `json:"debugId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"}, &res)
	const n = 40
	for i := 1; i <= n; i++ {
		raw, _ := json.Marshal(map[string]any{"debugId": res.DebugID, "message": json.RawMessage(fmt.Sprintf(`{"seq":%d,"type":"request","command":"threads"}`, i))})
		r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf(`"q%d"`, i)), Method: "_abhed/debug/send", Params: raw})
		if i == n/2 {
			stop, _ := json.Marshal(map[string]any{"debugId": res.DebugID})
			r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"stop"`), Method: "_abhed/debug/stop", Params: stop})
		}
	}
	for i := 1; i <= n; i++ {
		want := fmt.Sprintf(`"q%d"`, i)
		r.cl.waitFor(from, "the answer to send "+want, func(m rpcMessage) bool { return m.Method == "" && string(m.ID) == want })
	}
}

// A session closed while the supervisor's ask waits does not start the
// launch once the person answers.
func TestStudioDebugClosedWhileAskingDoesNotStart(t *testing.T) {
	asked := fakeDebugger(t, 0)
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.py", "print('hi')\n")
	withSupervisor(t, r, id, &monitor.Guard{Monitor: judging(policy.Ask, "unknown-program", nil)})
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method != "session/request_permission" {
			return map[string]any{}
		}
		var p struct {
			ToolCall struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"toolCall"`
		}
		_ = json.Unmarshal(params, &p)
		r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "once:" + p.ToolCall.ToolCallID}}
	})
	r.cl.refused(errRefused, "_abhed/debug/launch", map[string]any{"sessionId": id, "language": "python", "program": "app.py"})
	if len(*asked) != 0 {
		t.Fatalf("an adapter started after its session closed: %v", *asked)
	}
}
