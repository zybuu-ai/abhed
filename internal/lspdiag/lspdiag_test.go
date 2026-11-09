package lspdiag

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
)

// TestStubServer is the stub language server the tests start in place of
// gopls: a line holding BAD is an error, WARN a warning, MANY fifty errors;
// with ABHED_STUB_SILENT it never reports.
func TestStubServer(t *testing.T) {
	if os.Getenv("ABHED_STUB_LSP") == "" {
		t.Skip("the stub server, run by the tests")
	}
	br := bufio.NewReader(os.Stdin)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_ = lsprelay.Write(os.Stdout, b)
	}
	applied, config := "unanswered", "unanswered"
	for {
		raw, err := lsprelay.Read(br)
		if err != nil {
			os.Exit(0)
		}
		var m struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(raw, &m)
		switch m.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{"textDocumentSync": 1}}})
		case "initialized":
			send(map[string]any{"jsonrpc": "2.0", "id": "srv-apply", "method": "workspace/applyEdit", "params": map[string]any{"edit": map[string]any{}}})
			send(map[string]any{"jsonrpc": "2.0", "id": "srv-config", "method": "workspace/configuration", "params": map[string]any{"items": []any{map[string]any{"section": "gopls"}}}})
		case "textDocument/didOpen", "textDocument/didChange":
			if os.Getenv("ABHED_STUB_SILENT") != "" {
				continue
			}
			var p struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
					Text    string `json:"text"`
				} `json:"textDocument"`
				ContentChanges []struct {
					Text string `json:"text"`
				} `json:"contentChanges"`
			}
			_ = json.Unmarshal(m.Params, &p)
			text := p.TextDocument.Text
			if len(p.ContentChanges) > 0 {
				text = p.ContentChanges[0].Text
			}
			var diags []any
			add := func(line, sev int, msg string) {
				diags = append(diags, map[string]any{"range": map[string]any{"start": map[string]any{"line": line, "character": 2}, "end": map[string]any{"line": line, "character": 3}},
					"severity": sev, "message": msg, "source": "stub"})
			}
			for i, l := range strings.Split(text, "\n") {
				switch {
				case strings.Contains(l, "MANY"):
					for range 50 {
						add(i, 1, "one of many")
					}
				case strings.Contains(l, "BAD"):
					add(i, 1, "bad line\nwith a second line")
				case strings.Contains(l, "WARN"):
					add(i, 2, "warned")
				}
			}
			if strings.Contains(text, "REPORT") {
				add(0, 3, "applyEdit "+applied+"; configuration "+config)
			}
			if diags == nil {
				diags = []any{}
			}
			// An early report for the version before, which is not taken.
			send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": p.TextDocument.URI, "version": p.TextDocument.Version - 1, "diagnostics": []any{map[string]any{"message": "stale", "range": map[string]any{"start": map[string]any{"line": 0, "character": 0}}}}}})
			send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": p.TextDocument.URI, "version": p.TextDocument.Version, "diagnostics": diags}})
		case "shutdown":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case "exit":
			os.Exit(0)
		case "":
			switch m.ID {
			case "srv-apply":
				applied = string(m.Result)
			case "srv-config":
				config = string(m.Result)
			}
		}
	}
}

type rig struct {
	loop    *agent.Loop
	store   *agent.MemStore
	dir     string
	started *[]string
}

func newRig(t *testing.T, mode policy.Mode, env ...string) *rig {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.New(mode)
	pol.Roots = sess.PolicyRoots
	store := agent.NewMemStore()
	bash := tools.Bash{Sandbox: func(context.Context, string, string) *exec.Cmd { return nil }}
	l := agent.NewLoop(nil, tools.NewRegistry(bash, tools.Write{}), pol, agent.AutoApprove{}, sess,
		agent.NewRecorder(store, "s1", ""), agent.DefaultConfig())
	var mu sync.Mutex
	var started []string
	oldCmd, oldTier, oldSettle := Command, Tier, settle
	Tier = func(tools.Bash) string { return "process" }
	settle = 50 * time.Millisecond
	Command = func(_ tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
		mu.Lock()
		started = append(started, command)
		mu.Unlock()
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStubServer$") // #nosec G204 -- the test binary
		c.Env = append(append(os.Environ(), "ABHED_STUB_LSP=1"), env...)
		c.Dir = cwd
		return c
	}
	t.Cleanup(func() { Command, Tier, settle = oldCmd, oldTier, oldSettle })
	return &rig{loop: l, store: store, dir: dir, started: &started}
}

func (r *rig) write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *rig) events(t *testing.T, typ agent.EventType) []map[string]any {
	t.Helper()
	evs, _ := r.store.Events("s1")
	var out []map[string]any
	for _, e := range evs {
		if e.Type == typ {
			var m map[string]any
			_ = json.Unmarshal(e.Payload, &m)
			out = append(out, m)
		}
	}
	return out
}

func opts(mode string) Options {
	return Options{Mode: mode, Servers: map[string][]string{"go": {"gopls"}}, Max: 5, Wait: 5 * time.Second}
}

// The server's report for the version sent is what the agent reads, most
// severe first, each on one line; a stale one is not taken; the server's
// edit is not applied and its settings asked for are the engine's; the
// server is started once, recorded, and its end recorded on Close.
func TestDiagnosticsFromAStubServer(t *testing.T) {
	r := newRig(t, policy.ModeBypass)
	d := New(opts(ModeAuto))
	p := r.write(t, "a.go", "package a\n// WARN\n// BAD\n")
	got, ok := d.Diagnose(context.Background(), r.loop, "c1", p)
	if !ok || got.TimedOut || got.Skipped != "" {
		t.Fatalf("diagnose: %+v %v", got, ok)
	}
	want := "[gopls diagnostics for a.go: 1 errors, 1 warnings, 2 in all]\na.go:3:3: error: bad line with a second line (stub)\na.go:2:3: warning: warned (stub)"
	if got.Text != want {
		t.Fatalf("text:\n%s\nwant:\n%s", got.Text, want)
	}
	if got.Total != 2 || got.Shown != 2 || got.Errors != 1 || got.Warnings != 1 || got.LSPID == "" {
		t.Fatalf("counts %+v", got.LSPDiagnostics)
	}
	// The change: a fixed file reports none, and the server's requests were answered.
	r.write(t, "a.go", "package a\n// REPORT\n")
	got, _ = d.Diagnose(context.Background(), r.loop, "c2", p)
	if !strings.Contains(got.Text, `applyEdit {"applied":false`) || !strings.Contains(got.Text, "configuration [null]") {
		t.Fatalf("the server's requests: %s", got.Text)
	}
	r.write(t, "a.go", "package a\n")
	got, _ = d.Diagnose(context.Background(), r.loop, "c3", p)
	if got.Text != "[gopls diagnostics for a.go: none]" {
		t.Fatalf("fixed: %q", got.Text)
	}
	if len(*r.started) != 1 || !strings.Contains((*r.started)[0], "exec env GOTOOLCHAIN=local") {
		t.Fatalf("started %v", *r.started)
	}
	d.Close()
	started, stopped := r.events(t, agent.EvLSPStarted), r.events(t, agent.EvLSPStopped)
	if len(started) != 1 || started[0]["purpose"] != "diagnostics" || started[0]["command"] != "gopls" {
		t.Fatalf("lsp.started %v", started)
	}
	if len(stopped) != 1 || stopped[0]["reason"] != "closed" {
		t.Fatalf("lsp.stopped %v", stopped)
	}
	// Closed, nothing starts again.
	if _, ok := d.Diagnose(context.Background(), r.loop, "c4", p); ok {
		t.Fatal("a closed diagnoser diagnosed")
	}
}

// The count the agent is given is bounded, and says how many were left out.
func TestDiagnosticsAreBounded(t *testing.T) {
	r := newRig(t, policy.ModeBypass)
	d := New(opts(ModeAuto))
	defer d.Close()
	p := r.write(t, "a.go", "// MANY\n")
	got, _ := d.Diagnose(context.Background(), r.loop, "c1", p)
	if got.Total != 50 || got.Shown != 5 || strings.Count(got.Text, "\n") != 6 || !strings.HasSuffix(got.Text, "[45 more not shown]") {
		t.Fatalf("bounded: %+v\n%s", got.LSPDiagnostics, got.Text)
	}
}

// A server that does not report in time leaves the edit waiting no longer
// than the bound, and the agent is given nothing.
func TestDiagnosticsWaitIsBounded(t *testing.T) {
	r := newRig(t, policy.ModeBypass, "ABHED_STUB_SILENT=1")
	o := opts(ModeAuto)
	o.Wait = 400 * time.Millisecond
	d := New(o)
	defer d.Close()
	p := r.write(t, "a.go", "// BAD\n")
	start := time.Now()
	got, ok := d.Diagnose(context.Background(), r.loop, "c1", p)
	if took := time.Since(start); !ok || !got.TimedOut || got.Text != "" || took > 2*time.Second {
		t.Fatalf("timed out %+v after %v", got, took)
	}
}

// In default mode the server's command is an ask: auto does not start it
// and records why once; on takes the setting as the answer; a deny rule
// holds under on.
func TestDiagnosticsFollowPolicy(t *testing.T) {
	r := newRig(t, policy.ModeDefault)
	d := New(opts(ModeAuto))
	defer d.Close()
	p := r.write(t, "a.go", "// BAD\n")
	got, ok := d.Diagnose(context.Background(), r.loop, "c1", p)
	if !ok || !strings.HasPrefix(got.Skipped, "not started: ask") || got.Text != "" {
		t.Fatalf("auto under an ask: %+v %v", got, ok)
	}
	if _, ok := d.Diagnose(context.Background(), r.loop, "c2", p); ok || len(*r.started) != 0 {
		t.Fatalf("skipped twice, or started: %v", *r.started)
	}

	on := New(opts(ModeOn))
	defer on.Close()
	if got, _ := on.Diagnose(context.Background(), r.loop, "c3", p); got.Errors != 1 || len(*r.started) != 1 {
		t.Fatalf("on: %+v %v", got, *r.started)
	}

	denied := newRig(t, policy.ModeBypass)
	if err := denied.loop.Policy.AddDeny("bash(gopls*)"); err != nil {
		t.Fatal(err)
	}
	dd := New(opts(ModeOn))
	defer dd.Close()
	got, _ = dd.Diagnose(context.Background(), denied.loop, "c1", denied.write(t, "a.go", "// BAD\n"))
	if !strings.HasPrefix(got.Skipped, "not started: deny") || len(*denied.started) != 0 {
		t.Fatalf("deny: %+v %v", got, *denied.started)
	}
	if New(opts(ModeOff)) != nil {
		t.Fatal("off made a diagnoser")
	}
}

// Off, with nothing recorded, where no server can run: no sandbox, a server
// not installed, a language with none, or a file of no language.
func TestDiagnosticsOffWithoutAServer(t *testing.T) {
	r := newRig(t, policy.ModeBypass)
	p := r.write(t, "a.go", "// BAD\n")
	Tier = func(tools.Bash) string { return "none" }
	d := New(opts(ModeAuto))
	if _, ok := d.Diagnose(context.Background(), r.loop, "c1", p); ok {
		t.Fatal("diagnosed with no sandbox")
	}
	Tier = func(tools.Bash) string { return "process" }
	o := opts(ModeAuto)
	o.Find = func(string) bool { return false }
	if _, ok := New(o).Diagnose(context.Background(), r.loop, "c1", p); ok {
		t.Fatal("diagnosed with no server installed")
	}
	for _, f := range []string{"a.py", "notes.txt"} {
		if _, ok := New(opts(ModeAuto)).Diagnose(context.Background(), r.loop, "c1", r.write(t, f, "BAD\n")); ok {
			t.Fatalf("diagnosed %s", f)
		}
	}
	if len(*r.started) != 0 || len(r.events(t, agent.EvLSPStarted)) != 0 {
		t.Fatalf("started %v", *r.started)
	}
}

// A server that fails before it initializes turns diagnostics off for its
// language, with why recorded.
func TestDiagnosticsServerThatFails(t *testing.T) {
	r := newRig(t, policy.ModeBypass)
	Command = func(_ tools.Bash, ctx context.Context, cwd, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "echo 'abhed-lsp: gopls is not on PATH' >&2; exit 96")
	}
	d := New(opts(ModeAuto))
	defer d.Close()
	p := r.write(t, "a.go", "// BAD\n")
	got, ok := d.Diagnose(context.Background(), r.loop, "c1", p)
	if !ok || !strings.Contains(got.Skipped, "not on PATH") {
		t.Fatalf("failed start: %+v %v", got, ok)
	}
	if _, ok := d.Diagnose(context.Background(), r.loop, "c2", p); ok {
		t.Fatal("tried again after the server failed")
	}
}

// Under on, only the plain ask a mode or the default gives is answered;
// every other ask, and any deny, stands. Under auto only an allow runs.
func TestAllowedOnlyAnswersThePlainAsk(t *testing.T) {
	for _, c := range []struct {
		decision policy.Decision
		step     string
		on, auto bool
	}{
		{policy.Allow, "allow", true, true},
		{policy.Allow, "mode", true, true},
		{policy.Ask, "default", true, false},
		{policy.Ask, "mode", true, false},
		{policy.Ask, "ask", false, false},         // a rule's ask
		{policy.Ask, "destructive", false, false}, // always confirmed
		{policy.Ask, "screen", false, false},
		{policy.Ask, "monitor", false, false}, // the supervisor tightened it
		{policy.Ask, "workspace", false, false},
		{policy.Deny, "deny", false, false},
		{policy.Deny, "hook", false, false},
		{policy.Deny, "mode", false, false},
	} {
		d := policy.Result{Decision: c.decision, Step: c.step}
		if got := New(opts(ModeOn)).allowed(d); got != c.on {
			t.Errorf("on, %s at %s: %v", c.decision, c.step, got)
		}
		if got := New(opts(ModeAuto)).allowed(d); got != c.auto {
			t.Errorf("auto, %s at %s: %v", c.decision, c.step, got)
		}
	}
}

// End to end under on: a rule's ask, the supervisor turning an allow into
// an ask, and a PermissionRequest hook refusing the mode's ask each keep the
// server from starting, with why recorded; the mode's ask with no hook
// starts it, recorded as the setting's answer.
func TestOnDoesNotAnswerOtherAsks(t *testing.T) {
	p := func(r *rig) string { return r.write(t, "a.go", "// BAD\n") }

	ruled := newRig(t, policy.ModeDefault)
	if err := ruled.loop.Policy.AddAsk("bash(gopls*)"); err != nil {
		t.Fatal(err)
	}
	d := New(opts(ModeOn))
	defer d.Close()
	if got, _ := d.Diagnose(context.Background(), ruled.loop, "c1", p(ruled)); !strings.HasPrefix(got.Skipped, "not started: ask at ask") || len(*ruled.started) != 0 {
		t.Fatalf("a rule's ask: %+v %v", got, *ruled.started)
	}

	tightened := newRig(t, policy.ModeBypass)
	tightened.loop.Monitor = &monitor.Guard{Monitor: monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{Decision: policy.Ask, Code: "tool-misuse", Rationale: "not now"}, nil
	})}
	d = New(opts(ModeOn))
	defer d.Close()
	if got, _ := d.Diagnose(context.Background(), tightened.loop, "c1", p(tightened)); !strings.HasPrefix(got.Skipped, "not started: ask at monitor") || len(*tightened.started) != 0 {
		t.Fatalf("the supervisor's ask: %+v %v", got, *tightened.started)
	}
	if v := tightened.events(t, agent.EvMonitorVerdict); len(v) != 1 || len(tightened.events(t, agent.EvActionRequested)) != 1 || v[0]["call_id"] != tightened.events(t, agent.EvActionRequested)[0]["call_id"] {
		t.Fatalf("the verdict is not on the request: %v", v)
	}

	hooked := newRig(t, policy.ModeDefault)
	hooked.loop.Hooks = refuse{}
	d = New(opts(ModeOn))
	defer d.Close()
	if got, _ := d.Diagnose(context.Background(), hooked.loop, "c1", p(hooked)); !strings.HasPrefix(got.Skipped, "not started: deny at hook") || len(*hooked.started) != 0 {
		t.Fatalf("the hook's refusal: %+v %v", got, *hooked.started)
	}

	plain := newRig(t, policy.ModeDefault)
	d = New(opts(ModeOn))
	if got, _ := d.Diagnose(context.Background(), plain.loop, "c1", p(plain)); got.Errors != 1 {
		t.Fatalf("the mode's ask under on: %+v", got)
	}
	d.Close()
	started := plain.events(t, agent.EvLSPStarted)
	approved := plain.events(t, agent.EvActionApproved)
	obs := plain.events(t, agent.EvObservation)
	if len(started) != 1 || started[0]["allowed_by"] != agent.BySetting || started[0]["step"] != "default" ||
		len(approved) != 1 || approved[0]["by"] != agent.BySetting || len(obs) != 1 || obs[0]["call_id"] != started[0]["call_id"] {
		t.Fatalf("started %v approved %v observed %v", started, approved, obs)
	}
}

type refuse struct{}

func (refuse) PromptSubmitted(context.Context, string, string) string { return "" }
func (refuse) PermissionRequested(context.Context, *policy.Engine, string, string, json.RawMessage, string) string {
	return "the managed hook refuses language servers"
}
func (refuse) Observe(context.Context, string, string, string, string) {}

// flakyIn fails the first write and keeps what the later ones send.
type flakyIn struct {
	mu   sync.Mutex
	n    int
	sent []string
}

func (f *flakyIn) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n == 1 {
		return 0, fmt.Errorf("broken pipe")
	}
	f.sent = append(f.sent, string(p))
	return len(p), nil
}

func (f *flakyIn) Close() error { return nil }

// A didOpen the server never took leaves the document unopened: the next
// change opens it again rather than sending didChange for a document the
// server does not have.
func TestDidOpenIsSentAgainAfterAFailedWrite(t *testing.T) {
	in := &flakyIn{}
	s := &server{in: in, ready: make(chan struct{}), done: make(chan struct{}), docs: map[string]int{}, opened: map[string]bool{},
		pub: map[string]published{}, notify: make(chan struct{}), answers: map[string]chan json.RawMessage{}}
	close(s.ready)
	if _, timedOut, _ := s.diagnose(context.Background(), "file:///w/a.go", "go", []byte("package a\n"), time.Now().Add(200*time.Millisecond)); !timedOut {
		t.Fatal("a failed write did not end the wait")
	}
	s.diagnose(context.Background(), "file:///w/a.go", "go", []byte("package a\n"), time.Now().Add(200*time.Millisecond))
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.sent) == 0 || !strings.Contains(in.sent[0], `"textDocument/didOpen"`) {
		t.Fatalf("after a failed didOpen, sent %v", in.sent)
	}
	if !s.opened["file:///w/a.go"] {
		t.Fatal("a didOpen written was not noted")
	}
}

// format sorts its own copy: the stored report another edit of the file
// may be reading keeps its order.
func TestFormatLeavesTheStoredReport(t *testing.T) {
	stored := make([]diagnostic, 3)
	for i, sev := range []int{3, 2, 1} {
		stored[i].Severity, stored[i].Message = sev, fmt.Sprint("m", i)
	}
	text, _, _, _, _ := format(stored, 5, "a.go", "gopls")
	if !strings.Contains(text, "error: m2") || stored[0].Message != "m0" || stored[2].Message != "m2" {
		t.Fatalf("format reordered the stored report: %+v\n%s", stored, text)
	}
}

// A supervisor that cannot answer leaves the start refused for this edit
// only: the next edit asks again, and starts the server once it answers.
func TestUnavailableSupervisorDoesNotTurnDiagnosticsOff(t *testing.T) {
	r := newRig(t, policy.ModeBypass)
	down := true
	r.loop.Monitor = &monitor.Guard{Monitor: monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		if down {
			return monitor.Verdict{}, fmt.Errorf("judge timed out")
		}
		return monitor.Verdict{Decision: policy.Allow, Code: monitor.CodeOK}, nil
	})}
	d := New(opts(ModeAuto))
	defer d.Close()
	p := r.write(t, "a.go", "// BAD\n")
	got, _ := d.Diagnose(context.Background(), r.loop, "c1", p)
	if !strings.HasPrefix(got.Skipped, "not started this time") || len(*r.started) != 0 {
		t.Fatalf("unavailable: %+v", got)
	}
	down = false
	if got, _ := d.Diagnose(context.Background(), r.loop, "c2", p); got.Errors != 1 || len(*r.started) != 1 {
		t.Fatalf("after the supervisor answered: %+v", got)
	}
}

// A supervisor that keeps failing to answer is asked at most
// maxJudgeRetries times; then diagnostics for the language are off for the
// session, with why recorded, and no edit pays a judge's timeout again.
func TestJudgeRetriesAreCapped(t *testing.T) {
	r := newRig(t, policy.ModeBypass)
	r.loop.Monitor = &monitor.Guard{Monitor: monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{}, fmt.Errorf("judge timed out")
	})}
	d := New(opts(ModeAuto))
	defer d.Close()
	p := r.write(t, "a.go", "// BAD\n")
	var last agent.Diagnosed
	for i := range maxJudgeRetries {
		got, ok := d.Diagnose(context.Background(), r.loop, fmt.Sprint("c", i), p)
		if !ok {
			t.Fatalf("attempt %d recorded nothing", i)
		}
		last = got
	}
	if !strings.Contains(last.Skipped, fmt.Sprintf("could not answer %d times", maxJudgeRetries)) {
		t.Fatalf("the last attempt: %+v", last)
	}
	if _, ok := d.Diagnose(context.Background(), r.loop, "c9", p); ok {
		t.Fatal("asked again after the cap")
	}
	if n := len(r.events(t, agent.EvActionRequested)); n != maxJudgeRetries {
		t.Fatalf("%d starts were put to the supervisor", n)
	}
}
