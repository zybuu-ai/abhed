package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// #11: with a task given, -p does not wait on a stdin pipe nobody closes;
// "-" attaches stdin; input left unread is said once.
func TestPrintTaskDoesNotWaitOnStdin(t *testing.T) {
	m := &bgModelServer{}
	ws := bgWorkspace(t, m.start(t), "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	evs, stderr, timedOut := headlessRun(t, ws, r, 20*time.Second, "-p", "say hi")
	if timedOut || len(evs["user.message"]) != 1 {
		t.Fatalf("-p with a task waited on an open stdin pipe (timed out %v): %v\n%s", timedOut, evs["user.message"], stderr)
	}
	if strings.Contains(stderr, "stdin was not read") {
		t.Errorf("an empty pipe was reported as unread input: %s", stderr)
	}

	// Input already waiting is left out, and said so.
	r2, w2, _ := os.Pipe()
	_, _ = w2.WriteString("THE-LOG-LINE\n")
	evs, stderr, timedOut = headlessRun(t, ws, r2, 20*time.Second, "-p", "summarise")
	_ = r2.Close()
	_ = w2.Close()
	if timedOut || len(evs["user.message"]) != 1 || strings.Contains(stringOf(evs["user.message"][0]["text"]), "THE-LOG-LINE") {
		t.Fatalf("waiting input was read without -: %v", evs["user.message"])
	}
	if !strings.Contains(stderr, "stdin was not read, since the task is given; add - to attach it") {
		t.Errorf("no note about the unread input: %s", stderr)
	}

	// "-" attaches it.
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("THE-LOG-LINE\n")
	_, _ = f.Seek(0, 0)
	defer func() { _ = f.Close() }()
	evs, stderr, _ = headlessRun(t, ws, f, 20*time.Second, "-p", "summarise", "-")
	if u := evs["user.message"]; len(u) != 1 || !strings.Contains(stringOf(u[0]["text"]), "THE-LOG-LINE") {
		t.Fatalf("- did not attach stdin: %v\n%s", u, stderr)
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
	var warn bytes.Buffer
	if readsStdin(&cliFlags{}, "task", true, func() bool { return false }, &warn) || warn.Len() != 0 {
		t.Error("a task read stdin")
	}
	if !readsStdin(&cliFlags{}, "", true, func() bool { return false }, &warn) {
		t.Error("no task did not read stdin")
	}
	if !readsStdin(&cliFlags{stdinDash: true}, "task", true, func() bool { return false }, &warn) {
		t.Error("- did not read stdin")
	}
	if readsStdin(&cliFlags{stdinDash: true}, "task", false, func() bool { return true }, &warn) {
		t.Error("a terminal was read")
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
	if len(evs["user.message"]) != 0 || len(evs["session.started"]) != 0 || !strings.Contains(stderr, "workspace command /explain is not trusted (new)") || !strings.Contains(stderr, want) {
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
