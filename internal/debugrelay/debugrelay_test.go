package debugrelay

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlan(t *testing.T) {
	ws := "/w"
	s, err := Plan(Request{Language: Go, Program: "cmd/app", Args: []string{"-v", "a b"}, Env: map[string]string{"B": "2", "A": "x y"}}, ws, "/opt/abhed/abhed-linux-arm64 __abhed_dap_bridge", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Display != `A='x y' B=2 dlv debug /w/cmd/app -- -v 'a b'` {
		t.Errorf("display %q", s.Display)
	}
	if s.Launch["mode"] != "debug" || s.Launch["program"] != "/w/cmd/app" || s.Launch["console"] != "internalConsole" || s.Launch["request"] != "launch" {
		t.Errorf("launch %v", s.Launch)
	}
	if !strings.Contains(s.Command, "dlv dap --check-go-version=false --listen") || !strings.Contains(s.Command, "exec /opt/abhed/abhed-linux-arm64 __abhed_dap_bridge ") {
		t.Errorf("command %q", s.Command)
	}
	s, err = Plan(Request{Language: Python, Test: "tests/test_a.py", Cwd: "sub"}, ws, "", "")
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
		if _, err := Plan(bad, ws, "/b", ""); !errors.As(err, &p) || p.Kind != KindArgs {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	var p *Problem
	if _, err := Plan(Request{Language: Go, Program: "a"}, ws, "", ""); !errors.As(err, &p) || p.Kind != KindBridge {
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

// The session is over when the adapter answers disconnect, or reports the
// program ended after a terminate; a program that just ends is not.
func TestRelayEndsOnDisconnectOrTerminate(t *testing.T) {
	f := newFake(t)
	ended := make(chan struct{}, 4)
	f.relay.OnEnd(func() { ended <- struct{}{} })
	f.send(t, map[string]any{"seq": 1, "type": "event", "event": "exited", "body": map[string]any{"exitCode": 0}})
	f.editor(t)
	select {
	case <-ended:
		t.Fatal("an exited event alone ended the session")
	default:
	}
	go func() { _ = f.relay.FromEditor(json.RawMessage(`{"seq":7,"type":"request","command":"terminate"}`)) }()
	if m := f.read(t); m["command"] != "terminate" {
		t.Fatalf("adapter got %v", m)
	}
	f.send(t, map[string]any{"seq": 2, "type": "event", "event": "terminated"})
	f.editor(t)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("terminate then terminated did not end the session")
	}
	// Once only, however many ends follow.
	f.send(t, map[string]any{"seq": 3, "type": "response", "request_seq": 8, "command": "disconnect", "success": true})
	f.editor(t)
	select {
	case <-ended:
		t.Fatal("the session ended twice")
	case <-time.After(100 * time.Millisecond):
	}

	g := newFake(t)
	gEnded := make(chan struct{}, 1)
	g.relay.OnEnd(func() { gEnded <- struct{}{} })
	g.send(t, map[string]any{"seq": 1, "type": "response", "request_seq": 3, "command": "disconnect", "success": true})
	g.editor(t)
	select {
	case <-gEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("a disconnect answer did not end the session")
	}
}

// Each decisive key is read exactly. Studio's message that repeats one, or
// has one differing only in case, is refused before the adapter sees it,
// whichever reader would have won: the reviewer's probes included.
func TestFromEditorRefusesAmbiguousKeys(t *testing.T) {
	f := newFake(t)
	for _, raw := range []string{
		`{"seq":1,"type":"request","command":"launch","Command":"threads","arguments":{"program":"/etc/evil"}}`,
		`{"seq":1,"type":"request","command":"attach","COMMAND":"threads","arguments":{"processId":1}}`,
		`{"seq":1,"type":"request","Command":"threads","command":"launch","arguments":{}}`,
		`{"seq":1,"type":"request","command":"threads","command":"launch","arguments":{}}`,
		`{"seq":1,"type":"request","command":"restart","arguments":{"arguments":{"program":"/x"}},"Arguments":null}`,
		`{"seq":1,"type":"request","command":"restart","Arguments":null,"arguments":{"arguments":{"program":"/x"}}}`,
		`{"seq":1,"type":"request","Type":"event","command":"threads"}`,
		`{"seq":1,"TYPE":"request","command":"threads"}`,
		`{"seq":1,"Seq":2,"type":"request","command":"threads"}`,
		`[1]`, `{"seq":1,"type":"request","command":"threads"} {}`,
		// Unicode folds encoding/json (go-dap, Delve) matches: ſ (U+017F) is s.
		`{"seq":2,"type":"request","command":"restart","arguments":{},"argument\u017f":{"arguments":{"program":"/x"}}}`,
		`{"seq":2,"type":"request","command":"restart","argument\u017f":{"arguments":{"program":"/x"}},"arguments":{}}`,
		"{\"seq\":2,\"type\":\"request\",\"command\":\"restart\",\"argumentſ\":{\"arguments\":{\"program\":\"/x\"}}}",
		`{"seq":2,"\u017feq":3,"type":"request","command":"threads"}`,
		`{"\u017feq":3,"seq":2,"type":"request","command":"threads"}`,
		`{"seq":2,"type":"request","command":"threads","reque\u017ft_seq":1}`,
		`{"seq":2,"type":"request","command":"threads","succe\u017fs":true}`,
		`{"seq":2,"type":"request","command":"threads","me\u017f\u017fage":"x"}`,
	} {
		if err := f.relay.FromEditor(json.RawMessage(raw)); err == nil {
			t.Errorf("%s was taken", raw)
		}
	}
	// A key the relay does not decide on may repeat.
	go func() {
		_ = f.relay.FromEditor(json.RawMessage(`{"seq":1,"type":"request","command":"threads","x":1,"x":2}`))
	}()
	if m := f.read(t); m["command"] != "threads" {
		t.Fatalf("adapter got %v", m)
	}
}

// The adapter's messages are read the same way: one that repeats a key or
// varies its case is dropped and counted, never passed to the editor, so a
// runInTerminal or startDebugging cannot reach Studio disguised.
func TestPumpDropsAmbiguousAdapterMessages(t *testing.T) {
	f := newFake(t)
	for _, raw := range []string{
		`{"seq":1,"type":"request","command":"runInTerminal","Command":"x","arguments":{"args":["sh","-c","id"],"cwd":"/"}}`,
		`{"seq":2,"type":"request","Type":"event","command":"startDebugging","arguments":{}}`,
		`{"seq":3,"type":"event","type":"request","command":"runInTerminal","arguments":{"args":["sh"]}}`,
		`{"seq":4,"type":"request","command":"x","command":"runInTerminal"}`,
		`{"seq":5,"type":"event","event":"output","body":{},"requeſt_seq":1}`,
		`{"seq":6,"type":"response","request_seq":1,"command":"threads","succeſs":true,"meſſage":"x"}`,
		`{"ſeq":7,"seq":7,"type":"event","event":"output"}`,
	} {
		if err := Write(f.adapterOut, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	f.send(t, map[string]any{"seq": 8, "type": "event", "event": "stopped"})
	if m := f.editor(t); m["event"] != "stopped" {
		t.Fatalf("editor got %v first", m)
	}
	select {
	case m := <-f.editorGot:
		t.Fatalf("the editor also got %s", m)
	default:
	}
	if f.relay.Dropped() != 7 {
		t.Fatalf("dropped %d", f.relay.Dropped())
	}
}

// A header line or block past its bound fails without growing: what comes
// from the sandbox has no newline it must send.
func TestReadBoundsTheHeader(t *testing.T) {
	endless := io.MultiReader(strings.NewReader("Content-Length: 2"), strings.NewReader(strings.Repeat("x", 1<<20)))
	if _, err := Read(bufio.NewReader(endless)); !errors.Is(err, ErrHeader) {
		t.Fatalf("a newline-free header: %v", err)
	}
	if _, err := Read(bufio.NewReader(strings.NewReader("X: " + strings.Repeat("y", 2000) + "\r\n\r\n{}"))); !errors.Is(err, ErrHeader) {
		t.Fatalf("a long line: %v", err)
	}
	many := strings.Repeat("X: "+strings.Repeat("z", 500)+"\r\n", 20) + "Content-Length: 2\r\n\r\n{}"
	if _, err := Read(bufio.NewReader(strings.NewReader(many))); !errors.Is(err, ErrHeader) {
		t.Fatalf("a long block: %v", err)
	}
}

// A pytest node id's path is in the workspace too.
func TestPlanConfinesAPythonTest(t *testing.T) {
	for _, bad := range []string{"/etc/x.py::test_a", "../out/test_x.py", "/tmp/t.py"} {
		var p *Problem
		if _, err := Plan(Request{Language: Python, Test: bad}, "/w", "", ""); !errors.As(err, &p) || p.Kind != KindArgs {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := Plan(Request{Language: Python, Test: "tests/test_a.py::TestX::test_y"}, "/w", "", ""); err != nil {
		t.Fatal(err)
	}
}

// Delve's socket goes in the folder given, when one is.
func TestPlanUsesTheSocketFolder(t *testing.T) {
	s, err := Plan(Request{Language: Go, Program: "."}, "/w", "/b __abhed_dap_bridge", "/run/abhed-dap-1f")
	if err != nil || !strings.Contains(s.Command, "d=/run/abhed-dap-1f; [ -d \"$d\" ] || { mkdir -p") || strings.Contains(s.Command, "mktemp") {
		t.Fatalf("%q %v", s.Command, err)
	}
}

// A failed launch's status carries the adapter's error, its format filled
// from its variables and bounded, so Studio can show why.
func TestFailedLaunchCarriesTheAdaptersError(t *testing.T) {
	f := newFake(t)
	go func() {
		_ = f.relay.FromEditor(json.RawMessage(`{"seq":1,"type":"request","command":"initialize"}`))
	}()
	f.read(t)
	f.send(t, map[string]any{"seq": 1, "type": "response", "request_seq": 1, "command": "initialize", "success": true})
	f.editor(t)
	l := f.read(t)
	f.send(t, map[string]any{"seq": 2, "type": "response", "request_seq": l["seq"], "command": "launch", "success": false, "message": "Failed to launch",
		"body": map[string]any{"error": map[string]any{"id": 3000, "format": "Failed to launch: {e}", "variables": map[string]string{"e": "Go version 1.27 is too new"}}}})
	if s := <-f.statusGot; s != "failed:Failed to launch: Go version 1.27 is too new" {
		t.Fatalf("status %q", s)
	}
	if d := errorDetail(json.RawMessage(`{"error":{"format":"` + strings.Repeat("a", 5000) + `"}}`)); len(d) > maxDetail+4 {
		t.Fatalf("detail %d bytes", len(d))
	}
}

// A format of many placeholders and a large variable builds no more than
// the budget: the result is bounded and the work is in proportion to the
// body, not to placeholders times the variable.
func TestErrorDetailIsBuiltWithinItsBudget(t *testing.T) {
	format := strings.Repeat("{a}", 1000)
	v := strings.Repeat("v", 1<<20)
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"format": format, "variables": map[string]string{"a": v}}})
	var d string
	allocs := testing.AllocsPerRun(5, func() { d = errorDetail(body) })
	if len(d) > maxDetail+len("…") || !strings.HasPrefix(d, "vvv") {
		t.Fatalf("detail of %d bytes", len(d))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_ = errorDetail(body)
	runtime.ReadMemStats(&after)
	// The body is about 1 MiB; decoding it is the cost, not 1000 copies of v.
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Fatalf("allocated %d bytes for a %d-byte body (%v allocations)", got, len(body), allocs)
	}
	big := strings.Repeat("m", 1<<20)
	if b := Bounded(big, maxDetail); len(b) > maxDetail+len("…") {
		t.Fatalf("message bounded to %d", len(b))
	}
	if b := Bounded("aé", 2); b != "a…" {
		t.Fatalf("cut mid-rune: %q", b)
	}
}

// The launch's response is taken once; a later response with its seq goes
// to the editor, and a failed launch's message is bounded.
func TestLaunchResponseIsOneShot(t *testing.T) {
	f := newFake(t)
	go func() {
		_ = f.relay.FromEditor(json.RawMessage(`{"seq":1,"type":"request","command":"initialize"}`))
	}()
	f.read(t)
	f.send(t, map[string]any{"seq": 1, "type": "response", "request_seq": 1, "command": "initialize", "success": true})
	f.editor(t)
	l := f.read(t)
	f.send(t, map[string]any{"seq": 2, "type": "response", "request_seq": l["seq"], "command": "launch", "success": false, "message": strings.Repeat("x", 1<<20)})
	if s := <-f.statusGot; !strings.HasPrefix(s, "failed:") || len(s) > len("failed:")+maxDetail+len("…") {
		t.Fatalf("status of %d bytes", len(s))
	}
	// Not -1 either: the taken launch leaves no seq a later response can name.
	f.send(t, map[string]any{"seq": 3, "type": "response", "request_seq": -1, "command": "launch", "success": false, "message": "forged"})
	if m := f.editor(t); m["message"] != "forged" {
		t.Fatalf("editor got %v", m)
	}
	f.send(t, map[string]any{"seq": 4, "type": "response", "request_seq": l["seq"], "command": "launch", "success": false, "message": "again"})
	if m := f.editor(t); m["message"] != "again" {
		t.Fatalf("editor got %v", m)
	}
	select {
	case s := <-f.statusGot:
		t.Fatalf("a second launch verdict: %q", s)
	default:
	}
}
