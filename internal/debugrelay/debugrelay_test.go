package debugrelay

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlan(t *testing.T) {
	ws := "/w"
	s, err := Plan(Request{Language: Go, Program: "cmd/app", Args: []string{"-v", "a b"}, Env: map[string]string{"B": "2", "A": "x y"}}, ws, "/opt/abhed-dapbridge-linux-arm64")
	if err != nil {
		t.Fatal(err)
	}
	if s.Display != `A='x y' B=2 dlv debug /w/cmd/app -- -v 'a b'` {
		t.Errorf("display %q", s.Display)
	}
	if s.Launch["mode"] != "debug" || s.Launch["program"] != "/w/cmd/app" || s.Launch["console"] != "internalConsole" || s.Launch["request"] != "launch" {
		t.Errorf("launch %v", s.Launch)
	}
	if !strings.Contains(s.Command, "dlv dap --listen") || !strings.Contains(s.Command, "exec /opt/abhed-dapbridge-linux-arm64 ") {
		t.Errorf("command %q", s.Command)
	}
	s, err = Plan(Request{Language: Python, Test: "tests/test_a.py", Cwd: "sub"}, ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Display != "python3 -m pytest tests/test_a.py" || s.Launch["module"] != "pytest" || s.Cwd != "/w/sub" || !strings.Contains(s.Command, "debugpy.adapter") {
		t.Errorf("python %+v", s)
	}
	for _, bad := range []Request{
		{Language: Go, Program: "../x"}, {Language: Python, Program: "/etc/passwd"}, {Language: Go}, {Language: Go, Program: "a", Test: "b"},
		{Language: "ruby", Program: "a"}, {Language: Python, Program: "a", Cwd: "/tmp"}, {Language: Python, Program: "-c"},
		{Language: Python, Program: "a", Env: map[string]string{"BAD NAME": "1"}},
	} {
		var p *Problem
		if _, err := Plan(bad, ws, "/b"); !errors.As(err, &p) || p.Kind != KindArgs {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	var p *Problem
	if _, err := Plan(Request{Language: Go, Program: "a"}, ws, ""); !errors.As(err, &p) || p.Kind != KindBridge {
		t.Errorf("no bridge: %v", err)
	}
}

// fakeAdapter is the adapter side of a relay: what the engine wrote to it,
// and a way to answer.
type fakeAdapter struct {
	toAdapter  *io.PipeWriter
	fromEngine *bufio.Reader
	adapterOut *io.PipeWriter
	editorGot  chan json.RawMessage
	statusGot  chan string
	pumpDone   chan error
	relay      *Relay
}

func newFake(t *testing.T) *fakeAdapter {
	t.Helper()
	inR, inW := io.Pipe()   // engine -> adapter
	outR, outW := io.Pipe() // adapter -> engine
	f := &fakeAdapter{toAdapter: inW, fromEngine: bufio.NewReader(inR), adapterOut: outW,
		editorGot: make(chan json.RawMessage, 16), statusGot: make(chan string, 16), pumpDone: make(chan error, 1)}
	f.relay = New(inW, map[string]any{"request": "launch", "program": "/w/a"}, func(m json.RawMessage) { f.editorGot <- m },
		func(state, msg string) { f.statusGot <- state + ":" + msg })
	go func() { f.pumpDone <- f.relay.Pump(outR) }()
	t.Cleanup(func() { _ = outW.Close(); _ = inW.Close() })
	return f
}

func (f *fakeAdapter) read(t *testing.T) map[string]any {
	t.Helper()
	ch := make(chan json.RawMessage, 1)
	go func() {
		m, err := Read(f.fromEngine)
		if err != nil {
			close(ch)
			return
		}
		ch <- m
	}()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("the adapter's input ended")
		}
		var v map[string]any
		_ = json.Unmarshal(m, &v)
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the adapter")
	}
	return nil
}

func (f *fakeAdapter) send(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := Write(f.adapterOut, b); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeAdapter) editor(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-f.editorGot:
		var v map[string]any
		_ = json.Unmarshal(m, &v)
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the editor")
	}
	return nil
}

// The editor's initialize goes through; the engine sends its own launch once
// it is answered and keeps the answer to itself; the editor's launch, attach
// and a restart that changes the launch are refused to it; the adapter's
// runInTerminal is answered with an error and never reaches the editor.
func TestRelayOwnsTheLaunch(t *testing.T) {
	f := newFake(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = f.relay.FromEditor(json.RawMessage(`{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"go"}}`))
	}()
	if m := f.read(t); m["command"] != "initialize" {
		t.Fatalf("adapter got %v", m)
	}
	wg.Wait()
	f.send(t, map[string]any{"seq": 1, "type": "response", "request_seq": 1, "command": "initialize", "success": true})
	if m := f.editor(t); m["command"] != "initialize" {
		t.Fatalf("editor got %v", m)
	}
	l := f.read(t)
	args, _ := l["arguments"].(map[string]any)
	if l["command"] != "launch" || args["program"] != "/w/a" || l["seq"].(float64) < engineSeq {
		t.Fatalf("launch %v", l)
	}
	f.send(t, map[string]any{"seq": 2, "type": "response", "request_seq": l["seq"], "command": "launch", "success": true})
	if s := <-f.statusGot; s != "launched:" {
		t.Fatalf("status %q", s)
	}

	for _, raw := range []string{
		`{"seq":2,"type":"request","command":"launch","arguments":{"program":"/etc/evil"}}`,
		`{"seq":3,"type":"request","command":"attach","arguments":{"processId":1}}`,
		`{"seq":4,"type":"request","command":"restart","arguments":{"arguments":{"program":"/x"}}}`,
	} {
		if err := f.relay.FromEditor(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		if m := f.editor(t); m["success"] != false || m["type"] != "response" {
			t.Fatalf("%s: editor got %v", raw, m)
		}
	}
	if err := f.relay.FromEditor(json.RawMessage(`{"seq":5,"type":"response","request_seq":9,"command":"runInTerminal","success":true}`)); err == nil {
		t.Fatal("an editor response was taken")
	}

	f.send(t, map[string]any{"seq": 3, "type": "request", "command": "runInTerminal", "arguments": map[string]any{"args": []string{"/bin/sh"}}})
	if m := f.read(t); m["type"] != "response" || m["success"] != false || m["command"] != "runInTerminal" {
		t.Fatalf("runInTerminal answered %v", m)
	}
	if s := <-f.statusGot; !strings.HasPrefix(s, "note:") {
		t.Fatalf("status %q", s)
	}
	// An ordinary event passes to the editor.
	f.send(t, map[string]any{"seq": 4, "type": "event", "event": "stopped"})
	if m := f.editor(t); m["event"] != "stopped" {
		t.Fatalf("editor got %v", m)
	}
	select {
	case m := <-f.editorGot:
		t.Fatalf("the editor also got %s", m)
	default:
	}
}

func TestReadRefusesHugeAndHeaderless(t *testing.T) {
	if _, err := Read(bufio.NewReader(strings.NewReader("Content-Length: 99999999999\r\n\r\n"))); err == nil {
		t.Fatal("a huge message was taken")
	}
	if _, err := Read(bufio.NewReader(strings.NewReader("X: 1\r\n\r\n{}"))); err == nil {
		t.Fatal("a message with no length was taken")
	}
	m, err := Read(bufio.NewReader(strings.NewReader("Content-Length: 2\r\n\r\n{}")))
	if err != nil || string(m) != "{}" {
		t.Fatalf("%q %v", m, err)
	}
}

func TestMissingAdapter(t *testing.T) {
	if p := MissingAdapter(exitNoDelve, ""); p == nil || p.Kind != KindAdapter || len(p.Steps) == 0 {
		t.Fatalf("%+v", p)
	}
	if MissingAdapter(0, "") != nil || MissingAdapter(1, "") != nil {
		t.Fatal("an ordinary exit is a problem")
	}
}
