package debugrelay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPlanNode(t *testing.T) {
	ws := t.TempDir()
	for _, c := range []struct {
		r       Request
		display string
	}{
		{Request{Language: Node, Program: "app.js", Args: []string{"-v"}}, "node " + ws + "/app.js -v"},
		{Request{Language: Node, Program: "app.ts", Runner: RunTsx}, "node --import tsx " + ws + "/app.ts"},
		{Request{Language: Node, Program: "app.ts", Runner: RunTSNode}, "node --require ts-node/register " + ws + "/app.ts"},
		{Request{Language: Node, Test: "t/a.test.js"}, "node --test --test-isolation=none " + ws + "/t/a.test.js"},
		{Request{Language: Node, Test: "sum", Runner: RunVitest}, "node node_modules/vitest/vitest.mjs run --pool=threads --no-file-parallelism sum"},
		{Request{Language: Node, Test: "sum", Runner: RunJest, Env: map[string]string{"CI": "1"}}, "CI=1 node node_modules/jest/bin/jest.js --runInBand sum"},
	} {
		spec, err := Plan(c.r, ws, "", "")
		if err != nil {
			t.Fatal(c.r, err)
		}
		if spec.Display != c.display || !strings.Contains(spec.Command, "exec node -e ") {
			t.Fatalf("%+v: %q %v", c.r, spec.Display, spec.Launch)
		}
		if _, ok := spec.Launch["args"]; ok {
			t.Fatal("a Node launch carries argv, not args")
		}
	}
	for _, r := range []Request{
		{Language: Node, Program: "../out.js"},
		{Language: Node, Test: "/etc/x.test.js"},
		{Language: Node, Program: "a.js", Runner: RunVitest},
		{Language: Node, Test: "a", Runner: RunTsx},
		{Language: Node, Program: "a.js", Runner: "bun"},
		{Language: Node, Test: "--inspect=0.0.0.0:9229", Runner: RunJest},
	} {
		if _, err := Plan(r, ws, "", ""); err == nil {
			t.Fatalf("%+v planned", r)
		}
	}
	if p := NoNode(""); p.Install == nil || p.Install.Language != "node" || MissingAdapter(exitNoNode, "") == nil {
		t.Fatalf("%+v", p)
	}
}

// nodeOrSkip skips a test where node is missing or too old to debug. With
// ABHED_NODE_DEBUG_IMAGE set, the sessions run in that image instead.
func nodeOrSkip(t *testing.T) {
	t.Helper()
	probe := exec.Command("node", "-p", "process.versions.node")
	if img := os.Getenv("ABHED_NODE_DEBUG_IMAGE"); img != "" {
		probe = exec.Command("podman", "run", "--rm", img, "node", "-p", "process.versions.node") // #nosec G204 -- a test's own image
	}
	out, err := probe.Output()
	if err != nil {
		t.Skip("no node")
	}
	v := strings.Split(strings.TrimSpace(string(out)), ".")
	maj, _ := strconv.Atoi(v[0])
	minor, _ := strconv.Atoi(v[1])
	if maj < 20 || maj == 20 && minor < 6 {
		t.Skip("node is older than 20.6")
	}
}

// nodeSession runs a planned Node session through the relay, as the engine
// does, without a sandbox.
type nodeSession struct {
	t     *testing.T
	relay *Relay
	cmd   *exec.Cmd
	msgs  chan map[string]any
	state chan string
	seq   int
	seen  []map[string]any
	held  []map[string]any
}

func startNode(t *testing.T, spec Spec) *nodeSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", spec.Command) // #nosec G204 -- the planned adapter command
	if img := os.Getenv("ABHED_NODE_DEBUG_IMAGE"); img != "" {
		// Linux, in a container with no network: the workspace at its own path.
		root, _ := spec.Launch["root"].(string)
		cmd = exec.CommandContext(ctx, "podman", "run", "--rm", "-i", "--network=none", "-v", root+":"+root, "-w", spec.Cwd, img, "/bin/sh", "-c", spec.Command) // #nosec G204 -- a test's own image
	}
	cmd.Dir = spec.Cwd
	cmd.Env = os.Environ()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &nodeSession{t: t, cmd: cmd, msgs: make(chan map[string]any, 1024), state: make(chan string, 16)}
	s.relay = New(stdin, spec.Launch, func(m json.RawMessage) {
		var v map[string]any
		_ = json.Unmarshal(m, &v)
		s.msgs <- v
	}, func(state, message string) { s.state <- state + ":" + message })
	go func() {
		_ = s.relay.Pump(stdout)
		close(s.msgs)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("adapter stderr: %s", stderr.String())
			for _, m := range s.seen {
				b, _ := json.Marshal(m)
				t.Logf("seen %s", b)
			}
		}
	})
	return s
}

func (s *nodeSession) send(command string, args map[string]any) int {
	s.seq++
	m := map[string]any{"seq": s.seq, "type": "request", "command": command}
	if args != nil {
		m["arguments"] = args
	}
	b, _ := json.Marshal(m)
	if err := s.relay.FromEditor(b); err != nil {
		s.t.Fatal(err)
	}
	return s.seq
}

// wait is the first message from the adapter that ok takes.
func (s *nodeSession) wait(what string, ok func(map[string]any) bool) map[string]any {
	s.t.Helper()
	// A message not yet taken may be the one: messages arrive in any order
	// relative to the response a caller waits for first.
	for i, m := range s.held {
		if ok(m) {
			s.held = append(s.held[:i], s.held[i+1:]...)
			return m
		}
	}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case m, open := <-s.msgs:
			if !open {
				s.t.Fatalf("the adapter ended before %s", what)
			}
			s.seen = append(s.seen, m)
			if ok(m) {
				return m
			}
			s.held = append(s.held, m)
		case <-deadline:
			s.t.Fatalf("no %s", what)
		}
	}
}

func (s *nodeSession) call(command string, args map[string]any) map[string]any {
	s.t.Helper()
	seq := s.send(command, args)
	m := s.wait(command+" response", func(m map[string]any) bool {
		return m["type"] == "response" && m["request_seq"] == float64(seq)
	})
	if m["success"] != true {
		s.t.Fatalf("%s failed: %v", command, m["message"])
	}
	body, _ := m["body"].(map[string]any)
	return body
}

func (s *nodeSession) event(name string) map[string]any {
	s.t.Helper()
	m := s.wait(name, func(m map[string]any) bool { return m["type"] == "event" && m["event"] == name })
	body, _ := m["body"].(map[string]any)
	return body
}

// start initializes, sets bps (path -> lines) and lets the program run.
func (s *nodeSession) start(bps map[string][]int) {
	s.t.Helper()
	s.call("initialize", map[string]any{"adapterID": "node", "linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path"})
	select {
	case st := <-s.state:
		if !strings.HasPrefix(st, "launched") {
			s.t.Fatalf("launch: %s", st)
		}
	case <-time.After(30 * time.Second):
		s.t.Fatal("no launch")
	}
	s.event("initialized")
	for p, lines := range bps {
		var want []map[string]any
		for _, l := range lines {
			want = append(want, map[string]any{"line": l})
		}
		s.call("setBreakpoints", map[string]any{"source": map[string]any{"path": p}, "breakpoints": want})
	}
	s.call("configurationDone", nil)
}

// stopAt waits for a stop and checks where: the top frame's file and line.
func (s *nodeSession) stopAt(file string, line int) (thread float64, frame float64) {
	s.t.Helper()
	ev := s.event("stopped")
	thread, _ = ev["threadId"].(float64)
	st := s.call("stackTrace", map[string]any{"threadId": thread})
	frames, _ := st["stackFrames"].([]any)
	if len(frames) == 0 {
		s.t.Fatalf("no frames at the stop %v", ev)
	}
	top := frames[0].(map[string]any)
	src, _ := top["source"].(map[string]any)
	if src["path"] != file || top["line"] != float64(line) {
		s.t.Fatalf("stopped at %v:%v, want %s:%d (%v)", src["path"], top["line"], file, line, ev)
	}
	return thread, top["id"].(float64)
}

func (s *nodeSession) eval(frame float64, expr string) string {
	s.t.Helper()
	r := s.call("evaluate", map[string]any{"expression": expr, "frameId": frame, "context": "watch"})
	v, _ := r["result"].(string)
	return v
}

// finish lets the program run to its end and checks its output and exit.
func (s *nodeSession) finish(thread float64, output string) {
	s.t.Helper()
	s.call("continue", map[string]any{"threadId": thread})
	s.wait("output "+output, func(m map[string]any) bool {
		b, _ := m["body"].(map[string]any)
		out, _ := b["output"].(string)
		return m["event"] == "output" && strings.Contains(out, output)
	})
	ex := s.event("exited")
	if ex["exitCode"] != float64(0) {
		s.t.Fatalf("exit %v", ex)
	}
	s.event("terminated")
}

func write(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A real Node program, CommonJS and ES module: a breakpoint stops it at its
// line, its variables and an expression are read, and it runs to its end.
// Nothing in the program or the adapter listens on a port.
func TestNodeDebugsAProgram(t *testing.T) {
	nodeOrSkip(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	for _, name := range []string{"app.js", "app.mjs"} {
		t.Run(name, func(t *testing.T) {
			app := write(t, filepath.Join(ws, name), "let a = 1;\nlet b = 2;\nconsole.log('sum', a + b);\n")
			spec, err := Plan(Request{Language: Node, Program: name}, ws, "", "")
			if err != nil {
				t.Fatal(err)
			}
			s := startNode(t, spec)
			s.start(map[string][]int{app: {3}})
			thread, frame := s.stopAt(app, 3)
			if got := s.eval(frame, "a + b"); got != "3" {
				t.Fatalf("a + b = %q", got)
			}
			scopes := s.call("scopes", map[string]any{"frameId": frame})["scopes"].([]any)
			ref := scopes[0].(map[string]any)["variablesReference"]
			vars := s.call("variables", map[string]any{"variablesReference": ref})["variables"].([]any)
			found := false
			for _, v := range vars {
				m := v.(map[string]any)
				found = found || m["name"] == "a" && m["value"] == "1"
			}
			if !found {
				t.Fatalf("no a = 1 among %v", vars)
			}
			pid := s.eval(frame, "process.pid")
			noListener(t, pid)
			noListener(t, strconv.Itoa(s.cmd.Process.Pid))
			s.finish(thread, "sum 3")
		})
	}
}

// noListener fails if process pid holds a listening socket of any kind but
// its own pipes.
func noListener(t *testing.T, pid string) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil || os.Getenv("ABHED_NODE_DEBUG_IMAGE") != "" {
		return // the network is none in the image's container
	}
	out, _ := exec.Command("lsof", "-a", "-n", "-P", "-p", pid, "-i").Output() // #nosec G204 -- a test's own process id
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("process %s has network sockets:\n%s", pid, out)
	}
}

// A source-mapped program, as tsx or ts-node runs TypeScript: the
// breakpoint set in the TypeScript file stops the generated code, and the
// stop is shown in the TypeScript file at its own line.
func TestNodeDebugsThroughSourceMaps(t *testing.T) {
	nodeOrSkip(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	ts := write(t, filepath.Join(ws, "src", "app.ts"), "// header\n// more\nlet a: number = 1;\nlet b: number = 2;\n\nconsole.log('sum', a + b);\n")
	m, _ := json.Marshal(map[string]any{"version": 3, "sources": []string{"../src/app.ts"}, "mappings": ";AAEA;AACA;AAEA"})
	write(t, filepath.Join(ws, "out", "app.js"), "'use strict';\nlet a = 1;\nlet b = 2;\nconsole.log('sum', a + b);\n//# sourceMappingURL=data:application/json;base64,"+base64.StdEncoding.EncodeToString(m)+"\n")
	// tsx itself is not needed to show the mapping: a stand-in package.
	write(t, filepath.Join(ws, "node_modules", "tsx", "package.json"), `{"name":"tsx","type":"module","exports":"./index.mjs"}`)
	write(t, filepath.Join(ws, "node_modules", "tsx", "index.mjs"), "export {};\n")
	spec, err := Plan(Request{Language: Node, Program: "out/app.js", Runner: RunTsx}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := startNode(t, spec)
	s.start(map[string][]int{ts: {6}})
	thread, frame := s.stopAt(ts, 6)
	if got := s.eval(frame, "a + b"); got != "3" {
		t.Fatalf("a + b = %q", got)
	}
	s.finish(thread, "sum 3")
}

// A worker thread is a thread of its own, stopped at its breakpoint.
func TestNodeDebugsAWorkerThread(t *testing.T) {
	nodeOrSkip(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(ws, "main.js"), "const { Worker } = require('node:worker_threads');\nnew Worker(__dirname + '/wk.js').on('exit', () => console.log('worker done'));\n")
	wk := write(t, filepath.Join(ws, "wk.js"), "const x = 41;\nconsole.log('in worker', x + 1);\n")
	spec, err := Plan(Request{Language: Node, Program: "main.js"}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := startNode(t, spec)
	s.start(map[string][]int{wk: {2}})
	thread, frame := s.stopAt(wk, 2)
	if thread == 1 {
		t.Fatal("the worker's stop is on the main thread")
	}
	if got := s.eval(frame, "x"); got != "41" {
		t.Fatalf("x = %q", got)
	}
	s.finish(thread, "worker done")
}

// node --test runs the file in the debugged process.
func TestNodeDebugsATest(t *testing.T) {
	nodeOrSkip(t)
	if exec.Command("node", "--test-isolation=none", "-e", "0").Run() != nil {
		t.Skip("this node has no --test-isolation")
	}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	tf := write(t, filepath.Join(ws, "sum.test.js"), "const test = require('node:test');\nconst assert = require('node:assert');\ntest('sum', () => {\n  const s = 1 + 2;\n  assert.strictEqual(s, 3);\n});\n")
	spec, err := Plan(Request{Language: Node, Test: "sum.test.js"}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := startNode(t, spec)
	s.start(map[string][]int{tf: {5}})
	thread, frame := s.stopAt(tf, 5)
	if got := s.eval(frame, "s"); got != "3" {
		t.Fatalf("s = %q", got)
	}
	s.finish(thread, "pass 1")
}

// TypeScript Node runs itself (type stripping keeps every position).
func TestNodeDebugsTypeScript(t *testing.T) {
	nodeOrSkip(t)
	if out, _ := exec.Command("node", "-p", "!!process.features.typescript").Output(); strings.TrimSpace(string(out)) != "true" {
		t.Skip("this node does not run TypeScript itself")
	}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	app := write(t, filepath.Join(ws, "app.ts"), "const a: number = 1;\nconst b: number = 2;\nconsole.log('sum', a + b);\n")
	spec, err := Plan(Request{Language: Node, Program: "app.ts"}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := startNode(t, spec)
	s.start(map[string][]int{app: {3}})
	thread, _ := s.stopAt(app, 3)
	s.finish(thread, "sum 3")
}

// A runner not installed fails the launch with what to do; node missing is
// the adapter-missing exit.
func TestNodeRunnerAndNodeMissing(t *testing.T) {
	nodeOrSkip(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	spec, err := Plan(Request{Language: Node, Test: "sum", Runner: RunVitest}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := startNode(t, spec)
	s.call("initialize", map[string]any{"adapterID": "node"})
	select {
	case st := <-s.state:
		if !strings.HasPrefix(st, "failed:") || !strings.Contains(st, "vitest is not installed") {
			t.Fatalf("launch: %s", st)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no launch verdict")
	}
	cmd := exec.Command("/bin/sh", "-c", spec.Command) // #nosec G204 -- the planned adapter command
	cmd.Env = []string{"PATH=" + t.TempDir()}
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || MissingAdapter(ee.ExitCode(), "") == nil || MissingAdapter(ee.ExitCode(), "").Install.Language != "node" {
		t.Fatalf("without node: %v", err)
	}
}

// The real runners, opted in with ABHED_NODE_DEBUG_MODULES, a folder whose
// node_modules holds tsx, ts-node, typescript, vitest and jest: each stops
// at a breakpoint in the source as written and runs to its end.
func TestNodeDebugsRealRunners(t *testing.T) {
	mods := os.Getenv("ABHED_NODE_DEBUG_MODULES")
	if mods == "" {
		t.Skip("set ABHED_NODE_DEBUG_MODULES to a folder with tsx, ts-node, typescript, vitest and jest installed")
	}
	nodeOrSkip(t)
	program := "interface P { x: number }\n// a comment\nconst p: P = { x: 1 };\n\nfunction twice(n: number): number {\n  return n * 2;\n}\nconsole.log('twice', twice(p.x));\n"
	for _, c := range []struct {
		name, file, body string
		r                Request
		line             int
		expr, want, out  string
	}{
		{"tsx", "app.ts", program, Request{Language: Node, Program: "app.ts", Runner: RunTsx}, 6, "n", "1", "twice 2"},
		{"ts-node", "app.ts", program, Request{Language: Node, Program: "app.ts", Runner: RunTSNode, Env: map[string]string{"TS_NODE_TRANSPILE_ONLY": "1"}}, 6, "n", "1", "twice 2"},
		{"vitest", "sum.test.ts", "import { test, expect } from 'vitest';\n\ntest('sum', () => {\n  const s: number = 1 + 2;\n  expect(s).toBe(3);\n});\n",
			Request{Language: Node, Test: "sum", Runner: RunVitest}, 5, "s", "3", "passed"},
		{"jest", "sum.test.js", "test('sum', () => {\n  const s = 1 + 2;\n  expect(s).toBe(3);\n});\n",
			Request{Language: Node, Test: "sum", Runner: RunJest}, 3, "s", "3", "passed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws, _ := filepath.EvalSymlinks(t.TempDir())
			if err := os.Symlink(filepath.Join(mods, "node_modules"), filepath.Join(ws, "node_modules")); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(ws, "package.json"), `{"name":"w","private":true}`)
			f := write(t, filepath.Join(ws, c.file), c.body)
			spec, err := Plan(c.r, ws, "", "")
			if err != nil {
				t.Fatal(err)
			}
			s := startNode(t, spec)
			s.start(map[string][]int{f: {c.line}})
			thread, frame := s.stopAt(f, c.line)
			if got := s.eval(frame, c.expr); got != c.want {
				t.Fatalf("%s = %q", c.expr, got)
			}
			s.finish(thread, c.out)
		})
	}
}

// A program that fails ends, with its exit code, as the debugger lets it go.
func TestNodeProgramThatThrowsEnds(t *testing.T) {
	nodeOrSkip(t)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	app := write(t, filepath.Join(ws, "bad.js"), "const a = 1;\nthrow new Error('bad ' + a);\n")
	spec, err := Plan(Request{Language: Node, Program: "bad.js"}, ws, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := startNode(t, spec)
	s.start(map[string][]int{app: {1}})
	thread, _ := s.stopAt(app, 1)
	s.call("continue", map[string]any{"threadId": thread})
	if ex := s.event("exited"); ex["exitCode"] != float64(1) {
		t.Fatalf("exit %v", ex)
	}
	s.event("terminated")
}
