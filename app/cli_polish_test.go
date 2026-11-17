package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcpauth/mcpauthtest"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/profiles"
)

// headlessRun runs -p with stream-json output and the given stdin, killed
// after limit, and returns its events by type, stderr and whether it ran out.
func headlessRun(t *testing.T, ws string, stdin *os.File, limit time.Duration, args ...string) (map[string][]map[string]any, string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := mainHelper(append([]string{"-C", ws, "-output-format", "stream-json"}, args...))
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	timedOut := false
	select {
	case <-done:
	case <-ctx.Done():
		timedOut = true
		_ = cmd.Process.Kill() // our own child, by its handle
		<-done
	}
	evs := map[string][]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil {
			evs[ev.Type] = append(evs[ev.Type], ev.Payload)
		}
	}
	return evs, errOut.String(), timedOut
}

// #11: with a task given, -p waits only briefly on a stdin pipe nobody
// writes or closes, says so once, and goes on; piped input still attaches
// without -, and - attaches input that starts late.
func TestPrintTaskDoesNotWaitOnAnIdleStdin(t *testing.T) {
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	began := time.Now()
	evs, stderr, timedOut := headlessRun(t, ws, r, 20*time.Second, "-p", "say hi")
	if timedOut || len(evs["user.message"]) != 1 || time.Since(began) > 15*time.Second {
		t.Fatalf("-p with a task waited on an open stdin pipe (timed out %v): %v\n%s", timedOut, evs["user.message"], stderr)
	}
	if strings.Count(stderr, stdinIdleNote) != 1 {
		t.Errorf("the idle pipe was not said once: %s", stderr)
	}

	// git diff | abhed -p "review": input already there attaches, as in 1.2.9.
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("THE-DIFF-LINE\n")
	_, _ = f.Seek(0, 0)
	defer func() { _ = f.Close() }()
	evs, stderr, _ = headlessRun(t, ws, f, 20*time.Second, "-p", "review")
	if u := evs["user.message"]; len(u) != 1 || !strings.Contains(stringOf(u[0]["text"]), "THE-DIFF-LINE") || strings.Contains(stderr, stdinIdleNote) {
		t.Fatalf("piped input was not attached without -: %v\n%s", u, stderr)
	}
}

// The window: input or its end within it is read to the end; nothing within
// it is left alone with the note; - (no window) waits however late it comes.
func TestStdinWindow(t *testing.T) {
	ctx := context.Background()
	late := func(after time.Duration, chunks ...string) io.Reader {
		pr, pw := io.Pipe()
		go func() {
			time.Sleep(after)
			for _, c := range chunks {
				_, _ = pw.Write([]byte(c))
				time.Sleep(300 * time.Millisecond)
			}
			_ = pw.Close()
		}()
		return pr
	}
	var warn bytes.Buffer
	got, lateIn, _ := headlessTask(ctx, "review", late(800*time.Millisecond, "LATE"), 200*time.Millisecond, &warn)
	if got != "review" || !strings.Contains(warn.String(), stdinIdleNote) || lateIn == nil {
		t.Fatalf("input after the window was waited for: %q %q", got, warn.String())
	}
	// It is not lost: its arrival is signalled, which stops the run.
	select {
	case <-lateIn:
	case <-time.After(10 * time.Second):
		t.Fatal("input after the window was not signalled")
	}
	// A pipe that ends empty after the window signals nothing.
	_, lateIn, _ = headlessTask(ctx, "review", late(400*time.Millisecond), 100*time.Millisecond, &warn)
	select {
	case <-lateIn:
		t.Fatal("an empty pipe's late end was taken as input")
	case <-time.After(800 * time.Millisecond):
	}
	warn.Reset()
	if got, _, _ := headlessTask(ctx, "review", late(800*time.Millisecond, "LATE"), stdinToEOF, &warn); !strings.Contains(got, "LATE") {
		t.Fatalf("- did not wait for late input: %q", got)
	}
	// Started inside the window, it is read to its end however long that takes.
	if got, _, _ := headlessTask(ctx, "review", late(20*time.Millisecond, "ONE ", "TWO", " THREE"), 200*time.Millisecond, &warn); !strings.Contains(got, "ONE TWO THREE") || warn.Len() != 0 {
		t.Fatalf("input that started in the window was cut: %q %q", got, warn.String())
	}
	// Ended empty inside the window: no input, and nothing to say.
	if got, _, _ := headlessTask(ctx, "review", late(20*time.Millisecond), 200*time.Millisecond, &warn); got != "review" || warn.Len() != 0 {
		t.Fatalf("an empty pipe: %q %q", got, warn.String())
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// "-" is a -p switch and disagrees with -no-stdin and stream-json input.
func TestStdinDashFlag(t *testing.T) {
	f := parse(t, "-p", "fix it", "-")
	if !f.stdinDash || f.task() != "fix it" || checkHeadlessFlags(f) != 0 {
		t.Fatalf("%+v", f)
	}
	for _, args := range [][]string{{"-", "fix"}, {"-p", "x", "-", "-no-stdin"}, {"-p", "-", "-input-format", "stream-json"}} {
		if checkHeadlessFlags(parse(t, args...)) != 2 {
			t.Errorf("%q was accepted", args)
		}
	}
	// After "--" a lone dash is a word of the task.
	if f := parse(t, "-p", "--", "a", "-", "b"); f.stdinDash || f.task() != "a - b" {
		t.Errorf("%+v", f)
	}
	if w := stdinWindow(&cliFlags{stdinWait: time.Second}, "task", true); w != time.Second {
		t.Errorf("a task's window %v", w)
	}
	if w := stdinWindow(&cliFlags{stdinWait: time.Second}, "", true); w != stdinToEOF {
		t.Errorf("no task: %v", w)
	}
	if w := stdinWindow(&cliFlags{stdinDash: true, stdinWait: time.Second}, "task", true); w != stdinToEOF {
		t.Errorf("-: %v", w)
	}
	if w := stdinWindow(&cliFlags{noStdin: true}, "task", true); w != stdinSkip {
		t.Errorf("-no-stdin: %v", w)
	}
	if w := stdinWindow(&cliFlags{stdinDash: true}, "task", false); w != stdinSkip {
		t.Errorf("a terminal: %v", w)
	}
	if f := parse(t, "-p", "x", "-stdin-wait", "3s"); f.stdinWait != 3*time.Second {
		t.Errorf("-stdin-wait %v", f.stdinWait)
	}
	if f := parse(t, "-p", "x"); f.stdinWait != defaultStdinWait {
		t.Errorf("default %v", f.stdinWait)
	}
}

// #12: in -p, a workspace command nobody trusted is refused with the exact
// command that trusts it, before anything is recorded; `abhed trust grant`
// trusts it, after which -p runs it as the terminal does; revoke forgets it.
func TestPrintWorkspaceCommandTrust(t *testing.T) {
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), "")
	t.Setenv(config.TrustEnv, "")
	write(t, filepath.Join(ws, ".abhed", "commands", "explain.md"), "---\ndescription: explain a file\n---\nEXPLAIN-BODY for $ARGUMENTS in five bullets")
	_, files, sum, _ := workspaceCommands(config.Config{}, ws)
	if len(files) != 1 || sum == "" {
		t.Fatalf("files %v", files)
	}

	evs, stderr, _ := headlessRun(t, ws, nil, 20*time.Second, "-p", "/explain src/text.py")
	real, _ := filepath.EvalSymlinks(ws)
	want := "abhed trust grant -commands-sha256 " + sum + " " + shellQuote(real)
	if len(evs["user.message"]) != 0 || len(evs["session.started"]) != 0 || !strings.Contains(stderr, "workspace command /explain is not trusted (new)") ||
		!strings.Contains(stderr, "trust exactly those commands, and nothing else of the workspace, with: "+want) {
		t.Fatalf("an untrusted command was not refused with the command to trust it: %v\n%s", evs["user.message"], stderr)
	}

	var out bytes.Buffer
	if code := trustCmd(ws, []string{"show"}, &out); code != 0 || !strings.Contains(out.String(), "commands    1 file(s)") || !strings.Contains(out.String(), "NOT TRUSTED") {
		t.Fatalf("show: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := trustCmd(ws, []string{"grant", "-commands-sha256", strings.Repeat("0", 64)}, &out); code != 1 {
		t.Fatalf("a stale pin granted: %d %s", code, out.String())
	}
	if code := trustCmd(ws, []string{"grant", "-commands-sha256", sum}, &out); code != 0 || !strings.Contains(out.String(), "Trusted 1 workspace command(s)") {
		t.Fatalf("grant: %d\n%s", code, out.String())
	}

	evs, stderr, _ = headlessRun(t, ws, nil, 20*time.Second, "-p", "/explain src/text.py")
	if c := evs["command.invoked"]; len(c) != 1 || c[0]["name"] != "/explain" || c[0]["source"] != "workspace" || c[0]["sha256"] == nil {
		t.Fatalf("the trusted command was not invoked: %v\n%s", c, stderr)
	}
	if u := evs["user.message"]; len(u) != 1 || !strings.Contains(stringOf(u[0]["text"]), "EXPLAIN-BODY for src/text.py in five bullets") {
		t.Fatalf("the command's body was not the turn: %v", u)
	}

	// A change makes it untrusted again; revoke forgets the decision.
	write(t, filepath.Join(ws, ".abhed", "commands", "explain.md"), "CHANGED")
	_, stderr, _ = headlessRun(t, ws, nil, 20*time.Second, "-p", "/explain x")
	if !strings.Contains(stderr, "is not trusted (changed)") {
		t.Fatalf("a changed command ran: %s", stderr)
	}
	write(t, filepath.Join(ws, ".abhed", "commands", "explain.md"), "---\ndescription: explain a file\n---\nEXPLAIN-BODY for $ARGUMENTS in five bullets")
	out.Reset()
	if code := trustCmd(ws, []string{"revoke"}, &out); code != 0 {
		t.Fatal(out.String())
	}
	if store, _ := commandTrustStore(); func() bool { ok, _ := store.Decide(ws, sum); return ok }() {
		t.Fatal("revoke kept the commands trusted")
	}
}

// #12 review: -commands-sha256 alone trusts the commands and nothing else
// of the workspace; a plain grant keeps its 1.2.9 meaning and trusts no
// command, saying how; both pins together trust both, the commands hashed
// once the configuration is decided.
func TestCommandsGrantScope(t *testing.T) {
	setup := func(t *testing.T) (string, string, string) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(config.TrustEnv, "")
		ws := t.TempDir()
		write(t, filepath.Join(ws, ".abhed", "config.json"), `{"permissions":{"allow":["bash(make*)"]}}`)
		write(t, filepath.Join(ws, ".abhed", "commands", "explain.md"), "EXPLAIN $ARGUMENTS")
		st, err := config.InspectWorkspace(ws)
		if err != nil || st.SHA256 == "" {
			t.Fatalf("%v %+v", err, st)
		}
		return ws, st.SHA256, inspectCommands(ws).sum
	}
	configTrusted := func(ws string) bool {
		st, _ := config.InspectWorkspace(ws)
		return st.Trusted
	}

	t.Run("commands pin alone", func(t *testing.T) {
		ws, _, sum := setup(t)
		var out bytes.Buffer
		if code := trustCmd(ws, []string{"grant", "-commands-sha256", sum}, &out); code != 0 || !strings.Contains(out.String(), "nothing else of the workspace") {
			t.Fatalf("%d %s", code, out.String())
		}
		if configTrusted(ws) {
			t.Fatal("-commands-sha256 trusted the workspace configuration too")
		}
		if !inspectCommands(ws).trusted {
			t.Fatal("the commands were not trusted")
		}
	})
	t.Run("plain grant", func(t *testing.T) {
		ws, _, sum := setup(t)
		var out bytes.Buffer
		if code := trustCmd(ws, []string{"grant"}, &out); code != 0 {
			t.Fatalf("%d %s", code, out.String())
		}
		if !configTrusted(ws) {
			t.Fatal("a plain grant did not trust the configuration")
		}
		if inspectCommands(ws).trusted {
			t.Fatal("a plain grant trusted commands it never showed")
		}
		if !strings.Contains(out.String(), "command(s) were not trusted") || !strings.Contains(out.String(), "-commands-sha256 "+sum) {
			t.Fatalf("no advice for the commands: %s", out.String())
		}
	})
	t.Run("both pins", func(t *testing.T) {
		ws, cfgSum, sum := setup(t)
		var out bytes.Buffer
		if code := trustCmd(ws, []string{"grant", "-sha256", cfgSum, "-commands-sha256", sum}, &out); code != 0 {
			t.Fatalf("%d %s", code, out.String())
		}
		if !configTrusted(ws) || !inspectCommands(ws).trusted {
			t.Fatal("both pins did not trust both")
		}
	})
	// A configuration that names a command folder changes the commands a
	// session reads once it is trusted: the hash shown before cannot match,
	// and nothing is trusted, the configuration included.
	t.Run("both pins, configuration names a folder", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(config.TrustEnv, "")
		ws := t.TempDir()
		write(t, filepath.Join(ws, ".abhed", "config.json"), `{"commands":{"dirs":["more"]}}`)
		write(t, filepath.Join(ws, ".abhed", "commands", "explain.md"), "EXPLAIN")
		write(t, filepath.Join(ws, "more", "extra.md"), "EXTRA")
		st, _ := config.InspectWorkspace(ws)
		before, after := inspectCommands(ws).sum, inspectCommandsAs(ws, config.TrustGranted).sum
		if before == after {
			t.Fatalf("the configuration's folder did not change the commands: %s", before)
		}
		var out bytes.Buffer
		if code := trustCmd(ws, []string{"grant", "-sha256", st.SHA256, "-commands-sha256", before}, &out); code != 1 || configTrusted(ws) || inspectCommands(ws).trusted {
			t.Fatalf("a commands pin that cannot match granted something: %d %s", code, out.String())
		}
		if code := trustCmd(ws, []string{"grant", "-sha256", st.SHA256, "-commands-sha256", after}, &out); code != 0 || !configTrusted(ws) || !inspectCommands(ws).trusted {
			t.Fatalf("the hash after the grant was not taken: %d %s", code, out.String())
		}
	})
	t.Run("only commands, plain grant", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		ws := t.TempDir()
		write(t, filepath.Join(ws, ".abhed", "commands", "explain.md"), "EXPLAIN")
		var out bytes.Buffer
		if code := trustCmd(ws, []string{"grant"}, &out); code != 1 || inspectCommands(ws).trusted {
			t.Fatalf("a plain grant with only commands: %d", code)
		}
	})
}

// A trusted command that runs shell lines, or picks a model, is refused in
// -p, which cannot ask; one the run cannot find is sent as typed.
func TestHeadlessCustomRefusals(t *testing.T) {
	cs := &customState{cmds: []*customcmd.Command{
		{Name: "/sh", Body: "run !`ls`", Source: "user"},
		{Name: "/m", Body: "x", Model: "big", Source: "user"},
		{Name: "/ok", Body: "OK $ARGUMENTS", Source: "user"},
	}}
	if _, _, err := headlessCustom(cs, "/w", "/sh"); err == nil || !strings.Contains(err.Error(), "shell lines") {
		t.Errorf("shell: %v", err)
	}
	if _, _, err := headlessCustom(cs, "/w", "/m"); err == nil || !strings.Contains(err.Error(), "-model") {
		t.Errorf("model: %v", err)
	}
	if c, args, err := headlessCustom(cs, "/w", "/ok a b"); err != nil || c == nil || args != "a b" {
		t.Errorf("ok: %v %v %q", c, err, args)
	}
	if c, _, err := headlessCustom(cs, "/w", "/other thing"); c != nil || err != nil {
		t.Errorf("unknown: %v %v", c, err)
	}
	refused := &customState{untrusted: []*customcmd.Command{{Name: "/x"}}, wsReason: "refused"}
	if _, _, err := headlessCustom(refused, "/w", "/x"); err == nil || !strings.Contains(err.Error(), "started untrusted") {
		t.Errorf("refused: %v", err)
	}
}

// #14: login says who the person is by email (or name), not the subject,
// and keeps the subject as the account's key.
func TestLoginShowsTheEmail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(profiles.EnvFile, "")
	ls := newLoginServerFor(t, auth.User{Username: "u-bob", Email: "bob@corp.example", Name: "Bob"})
	ws := t.TempDir()
	var out bytes.Buffer
	ap := &approver{ls: ls}
	if code := loginCmd(ws, []string{"-device", "-ca", ls.ca, "-profile", "lab", ls.ts.URL}, &out, ap); code != 0 {
		t.Fatalf("login = %d\n%s%s", code, out.String(), ap.String())
	}
	if !strings.Contains(out.String(), "as bob@corp.example (profile lab") {
		t.Fatalf("login said %q", out.String())
	}
	reg, _ := profiles.Load(filepath.Join(home, ".abhed", "profiles.json"))
	if p := reg.Profiles["lab"]; p == nil || p.Username != "u-bob" || p.Display != "bob@corp.example" {
		t.Fatalf("profile %+v", p)
	}
	out.Reset()
	if code := profileCmd(ws, []string{"list"}, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "signed in as bob@corp.example") {
		t.Fatalf("profile list %q", out.String())
	}
	if got := signedInAs(&profiles.Profile{Username: "u-x"}); got != "u-x" {
		t.Errorf("no display: %q", got)
	}
}

// #14: BROWSER is read as other programs read it, and opens the address;
// over SSH it does not make a browser count as local.
func TestBrowserEnv(t *testing.T) {
	got := browserCommands("firefox --new-tab:open -a Safari %s", "https://x/y")
	want := [][]string{{"firefox", "--new-tab", "https://x/y"}, {"open", "-a", "Safari", "https://x/y"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%q", got)
	}
	if browserCommands("", "u") != nil || browserCommands(" : ", "u") != nil {
		t.Fatal("an empty BROWSER gave commands")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for the browser")
	}
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	script := filepath.Join(dir, "browser.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$1\" > "+seen+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", filepath.Join(dir, "missing")+string(os.PathListSeparator)+script)
	if err := openBrowser("https://sign.in/approve?x=1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(seen); string(b) == "https://sign.in/approve?x=1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("BROWSER was not run with the address")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	if !canOpenBrowser() {
		t.Error("a BROWSER set did not count")
	}
	t.Setenv("SSH_CONNECTION", "10.0.0.1 22 10.0.0.2 22")
	if canOpenBrowser() {
		t.Error("over SSH a BROWSER counted as a local browser")
	}
}

// #14: -no-browser prints the address and opens nothing, for login and for
// mcp login.
func TestNoBrowser(t *testing.T) {
	old := openBrowser
	t.Cleanup(func() { openBrowser = old })
	openBrowser = func(string) error { t.Error("a browser was opened"); return nil }
	var errOut bytes.Buffer
	_ = signInOpener(true, &errOut)("https://srv/approve")
	if !strings.Contains(errOut.String(), "  https://srv/approve") {
		t.Fatalf("%q", errOut.String())
	}

	s := mcpauthtest.New(t)
	ws := loginHome(t, s, "")
	follow := mcpauthtest.Browser(t)
	openBrowser = func(string) error { t.Error("mcp login opened a browser"); return nil }
	var out, errw bytes.Buffer
	pw := &urlFollower{follow: follow}
	cio := mcpIO{in: strings.NewReader(""), out: pw, err: &errw, terminal: true}
	if code := mcpCmd(ws, []string{"login", "-no-browser", "corp"}, config.TrustRefused, cio); code != 0 {
		t.Fatalf("mcp login -no-browser: %d %s %s", code, pw.String(), errw.String())
	}
	if !strings.Contains(pw.String(), "Sign in to MCP server corp in your browser:") {
		t.Fatalf("no address printed: %s", pw.String())
	}
	if code := mcpCmd(ws, []string{"logout", "-no-browser", "corp"}, config.TrustRefused, mcpIO{in: strings.NewReader(""), out: &out, err: &errw, terminal: true}); code != 2 {
		t.Fatalf("logout took -no-browser: %d", code)
	}
}

// urlFollower is a person reading the printed address and opening it.
type urlFollower struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	follow func(string) error
	done   bool
}

func (f *urlFollower) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Write(p)
	if !f.done {
		for _, line := range strings.Split(f.buf.String(), "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(l, "http") {
				f.done = true
				_ = f.follow(l)
			}
		}
	}
	return len(p), nil
}

func (f *urlFollower) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// probeAdapter answers each probe after delay: a word, then a tool call.
type probeAdapter struct {
	think bool
	delay time.Duration
	mu    sync.Mutex
	reqs  []model.Request
}

func (p *probeAdapter) Name() string { return "probe" }
func (p *probeAdapter) Profile() model.Profile {
	return model.Profile{Name: "probe", Sampling: model.Sampling{Think: p.think}}
}
func (p *probeAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (p *probeAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	ch := make(chan model.Chunk, 1)
	go func() {
		defer close(ch)
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
			return
		}
		if len(req.Tools) > 0 {
			ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{Name: "glob", Args: json.RawMessage(`{"pattern":"*.go"}`)}}
			return
		}
		ch <- model.Chunk{Type: model.ChunkText, Text: "ok"}
	}()
	return ch, nil
}

// #16: doctor's two probes each get the whole timeout, with thinking off
// where the provider takes it and room for a tool call; together they may
// take longer than one timeout.
func TestDoctorProbesHaveTheirOwnTimeouts(t *testing.T) {
	a := &probeAdapter{think: true, delay: 700 * time.Millisecond}
	var out bytes.Buffer
	if !doctorModelProbes(a, time.Second, &out) {
		t.Fatalf("a model answering each probe within its own timeout failed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "checking tool calling... ok") {
		t.Fatal(out.String())
	}
	if len(a.reqs) != 2 {
		t.Fatalf("%d requests", len(a.reqs))
	}
	for _, r := range a.reqs {
		if r.Params.Think == nil || *r.Params.Think {
			t.Errorf("a probe asked with thinking on: %+v", r.Params)
		}
	}
	if a.reqs[1].MaxTokens < 1024 {
		t.Errorf("the tool probe's cap is %d", a.reqs[1].MaxTokens)
	}
	// A provider that takes no think setting is sent none.
	e, tc := doctorProbeRequests(model.Profile{})
	if e.Params.Think != nil || tc.Params.Think != nil {
		t.Error("think sent to a provider that does not take it")
	}
	// A probe that runs out of time says so.
	slow := &probeAdapter{delay: time.Minute}
	out.Reset()
	if doctorModelProbes(slow, 200*time.Millisecond, &out) {
		t.Fatal("a model that never answered passed")
	}
}

// managedFile points the managed configuration at body for this test.
func managedFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
}

// #17: a managed file that does not choose the model leaves first-run setup
// on; one that does turns it off.
func TestFirstRunUnderAManagedFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ABHED_BASE_URL", "")
	t.Setenv("ABHED_MODEL", "")
	ws := t.TempDir()
	managedFile(t, `{"permissions":{"deny":["bash(curl*)"]}}`)
	if !nothingChoosesModel(ws) {
		t.Fatal("a managed file of rules alone turned first-run setup off")
	}
	managedFile(t, `{"model":{"default":"corp","providers":{"corp":{"type":"openai-compatible","base_url":"https://llm.corp.example/v1","model":"m"}}}}`)
	if nothingChoosesModel(ws) {
		t.Fatal("first-run setup was offered though the managed file chooses the model")
	}
}

// #17: under a managed file, setup offers only the models it allows, does
// not ask what it sets, and refuses an endpoint model it does not allow.
func TestFirstRunOffersOnlyWhatTheManagedFileAllows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"llama3:8b"},{"id":"qwen2.5-coder:7b"}]}`))
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	managedFile(t, `{"model":{"allowed":["ollama/qwen*"]},"memory":{"auto":false}}`)
	var out strings.Builder
	// 1 picks the one model offered; then no to writing.
	if err := firstRun(context.Background(), strings.NewReader("1\n1\n"), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "managed configuration applies") || !strings.Contains(got, "1) Ollama: qwen2.5-coder:7b") ||
		strings.Contains(got, "Ollama: llama3:8b") || !strings.Contains(got, "1 more Ollama model(s) not offered") {
		t.Fatalf("offered:\n%s", got)
	}
	if strings.Contains(got, "memory notes") {
		t.Fatalf("asked what the managed file sets:\n%s", got)
	}
	// An endpoint type no entry can admit is refused before it is asked for.
	out.Reset()
	if err := firstRun(context.Background(), strings.NewReader("e\ns\n"), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "allows no model at an OpenAI-compatible endpoint") || strings.Contains(out.String(), "Base URL") {
		t.Fatalf("an endpoint type outside the list was asked for:\n%s", out.String())
	}

	// An endpoint model the list does not admit is refused before the
	// endpoint is contacted, and asked again; the endpoint is not listed.
	var seen []string
	var mu sync.Mutex
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer ep.Close()
	managedFile(t, `{"model":{"allowed":["ollama/qwen*","openai-compatible/corp-*"]},"memory":{"auto":false}}`)
	out.Reset()
	if err := firstRun(context.Background(), strings.NewReader("e\n"+ep.URL+"/v1\n\nother-model\ncorp-1\n1\n"), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "other-model is not allowed") {
		t.Fatalf("an endpoint model outside the list was taken:\n%s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range seen {
		if strings.Contains(r, "/models") || strings.Contains(r, "other-model") {
			t.Fatalf("the endpoint was contacted before the model was allowed: %q", seen)
		}
	}
}

// #12 review: in -p, a command's own @ files are attached through policy, as
// the terminal attaches them; one policy refuses stops the command.
func TestPrintCommandAttachesItsMentions(t *testing.T) {
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), `,"permissions":{"deny":["read(**/secret.txt)"]}`)
	home := os.Getenv("HOME")
	write(t, filepath.Join(home, ".abhed", "commands", "plan.md"), "Follow @notes.md for $ARGUMENTS")
	write(t, filepath.Join(home, ".abhed", "commands", "leak.md"), "Summarise @secret.txt")
	write(t, filepath.Join(ws, "notes.md"), "NOTES-MARKER")
	write(t, filepath.Join(ws, "secret.txt"), "SECRET-MARKER")

	evs, stderr, _ := headlessRun(t, ws, nil, 20*time.Second, "-p", "/plan pkg/auth")
	if u := evs["user.message"]; len(u) != 1 || !strings.Contains(stringOf(u[0]["text"]), "NOTES-MARKER") || len(evs["input.mention"]) != 1 {
		t.Fatalf("the command's mention was not attached: %v %v\n%s", u, evs["input.mention"], stderr)
	}
	evs, stderr, _ = headlessRun(t, ws, nil, 20*time.Second, "-p", "/leak")
	if len(evs["user.message"]) != 0 || !strings.Contains(stderr+fmt.Sprint(evs["result"]), "/leak was not sent") {
		t.Fatalf("a mention policy refuses was sent: %v\n%s", evs["user.message"], stderr)
	}
	for _, u := range evs["user.message"] {
		if strings.Contains(stringOf(u["text"]), "SECRET-MARKER") {
			t.Fatal("the denied file reached the model")
		}
	}
}

// #14 review: a name the server gives is clipped before it is kept or shown.
func TestDisplayIsClipped(t *testing.T) {
	long := strings.Repeat("é", 500)
	if got := clipDisplay(long); len([]rune(got)) != maxDisplay || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d runes", len([]rune(got)))
	}
	if got := clipDisplay("bob@corp.example"); got != "bob@corp.example" {
		t.Fatal(got)
	}
}

// A stop signal stands over input that comes on stdin after it: Ctrl-C
// then late input exits 130, not 9.
func TestSignalWinsOverLateStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	ws := bgWorkspace(t, slow.URL, "")
	began := time.Now()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	cmd := mainHelper([]string{"-C", ws, "-p", "why does it fail", "-stdin-wait", "200ms", "-output-format", "json"})
	cmd.Stdin = r
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	time.Sleep(1500 * time.Millisecond)
	_ = cmd.Process.Signal(os.Interrupt) // our own child, by its handle
	time.Sleep(100 * time.Millisecond)
	_, _ = w.WriteString("late\n")
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("did not stop\n%s", errOut.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errOut.String())
	}
	t.Logf("stopped after %s", time.Since(began))
}

// The exit-code help lists every code a run ends with.
func TestExitCodesHelpIsComplete(t *testing.T) {
	for _, code := range []string{"0 done", "1 error", "2 bad", "3 budget", "4 a hook", "5 model", "6 shut", "7 the run's", "8 unverified", "9 stdin", "130 ", "143 "} {
		if !strings.Contains(strings.Join(strings.Fields(exitCodesHelp), " "), code) {
			t.Errorf("the help lacks %q", code)
		}
	}
}
