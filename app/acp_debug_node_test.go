package app

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// §7.8: a real Node program debugged through the engine, in the session's
// own sandbox as it is (no container, no loopback, no consent): the
// breakpoint stops it, an expression is read, it runs to its end, and the
// launch is the person's judged call, recorded with loopback off.
func TestStudioDebugsNodeForReal(t *testing.T) {
	out, err := exec.Command("node", "-p", "process.versions.node").Output()
	if err != nil {
		t.Skip("no node")
	}
	v := strings.Split(strings.TrimSpace(string(out)), ".")
	if maj, _ := strconv.Atoi(v[0]); maj < 21 {
		t.Skip("node is older than this test needs")
	}
	r := newStudioRig(t, "")
	id := r.open()
	r.write("app.js", "let a = 1;\nlet b = 2;\nconsole.log('sum', a + b);\n")
	from := r.cl.mark()
	defer func() {
		if t.Failed() {
			for _, m := range r.cl.since(from) {
				t.Logf("%s %s", m.Method, m.Params)
			}
		}
	}()
	var res struct {
		DebugID string `json:"debugId"`
	}
	r.cl.ok("_abhed/debug/launch", map[string]any{"sessionId": id, "language": "node", "program": "app.js"}, &res)
	seq := 0
	send := func(command string, args any) int {
		seq++
		m, _ := json.Marshal(map[string]any{"seq": seq, "type": "request", "command": command, "arguments": args})
		r.cl.ok("_abhed/debug/send", map[string]any{"debugId": res.DebugID, "message": json.RawMessage(m)}, nil)
		return seq
	}
	adapter := func(what string, ok func(map[string]any) bool) map[string]any {
		var got map[string]any
		r.cl.waitFor(from, what, func(m rpcMessage) bool {
			if m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"failed"`) {
				t.Fatalf("the session failed: %s", m.Params)
			}
			if m.Method != "_abhed/debug/message" {
				return false
			}
			var p struct {
				Message map[string]any `json:"message"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if ok(p.Message) {
				got = p.Message
				return true
			}
			return false
		})
		return got
	}
	reply := func(n int) map[string]any {
		m := adapter("response "+strconv.Itoa(n), func(m map[string]any) bool { return m["type"] == "response" && m["request_seq"] == float64(n) })
		if m["success"] != true {
			t.Fatalf("request %d failed: %v", n, m)
		}
		b, _ := m["body"].(map[string]any)
		return b
	}
	event := func(name string) map[string]any {
		m := adapter(name, func(m map[string]any) bool { return m["type"] == "event" && m["event"] == name })
		b, _ := m["body"].(map[string]any)
		return b
	}
	reply(send("initialize", map[string]any{"adapterID": "node", "linesStartAt1": true, "pathFormat": "path"}))
	event("initialized")
	app := r.ws + "/app.js"
	reply(send("setBreakpoints", map[string]any{"source": map[string]any{"path": app}, "breakpoints": []any{map[string]any{"line": 3}}}))
	reply(send("configurationDone", nil))
	stop := event("stopped")
	st := reply(send("stackTrace", map[string]any{"threadId": stop["threadId"]}))
	top := st["stackFrames"].([]any)[0].(map[string]any)
	if src, _ := top["source"].(map[string]any); src["path"] != app || top["line"] != float64(3) {
		t.Fatalf("stopped at %v", top)
	}
	if ev := reply(send("evaluate", map[string]any{"expression": "a + b", "frameId": top["id"], "context": "watch"})); ev["result"] != "3" {
		t.Fatalf("a + b = %v", ev)
	}
	reply(send("continue", map[string]any{"threadId": stop["threadId"]}))
	adapter("the program's output", func(m map[string]any) bool {
		b, _ := m["body"].(map[string]any)
		o, _ := b["output"].(string)
		return m["event"] == "output" && strings.Contains(o, "sum 3")
	})
	event("terminated")
	send("disconnect", map[string]any{})
	r.cl.waitFor(from, "ended", func(m rpcMessage) bool {
		return m.Method == "_abhed/debug/status" && strings.Contains(string(m.Params), `"ended"`)
	})
	launched, _ := r.recorded(id, agent.EvDebugLaunched)
	if len(launched) != 1 || launched[0]["language"] != "node" || launched[0]["command"] != "node "+app || launched[0]["loopback"] == true {
		t.Fatalf("debug.launched %v", launched)
	}
	if stopped, _ := r.recorded(id, agent.EvDebugStopped); len(stopped) != 1 || stopped[0]["reason"] != "ended" {
		t.Fatalf("debug.stopped %v", stopped)
	}
}
