package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// shellBench is a workbench session opened with no prompt, whose bash can
// host an interactive shell (on the host: the tests exercise the terminal,
// and the sandbox package tests the boundary).
func shellBench(t *testing.T, edit func(*config.Config)) *workbench {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	if edit != nil {
		edit(&cfg)
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	sb := sandbox.NewNone(sandbox.DefaultPolicy(dir))
	bash := tools.Bash{Sandbox: sb.Command, Shell: sb.Shell, Isolation: tools.Isolation{Tier: "none", Backend: sb.Backend()}}
	s := New(Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, bash)})
	wb := &workbench{t: t, h: s.Handler(), workspace: dir}
	wb.session = wb.openIdle("acme")
	return wb
}

func (wb *workbench) openIdle(tenant string) string {
	wb.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"workbench":true}`))
	req.Header.Set("X-Abhed-Tenant", tenant)
	wb.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		wb.t.Fatalf("open a workbench session: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	return created.SessionID
}

func (wb *workbench) startShell() ptyStartResponse {
	wb.t.Helper()
	rec := wb.send("acme", "POST", "pty", ptyStartRequest{Interactive: true, Cols: 100, Rows: 30})
	if rec.Code != http.StatusOK {
		wb.t.Fatalf("shell start: %d %s", rec.Code, rec.Body)
	}
	var out ptyStartResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// typeLines follows a shell's output while typing each piece into it, a moment
// apart, and returns the output and the exit once the shell ends.
func (wb *workbench) typeLines(id string, lines ...string) (string, string) {
	wb.t.Helper()
	srv := httptest.NewServer(wb.h)
	defer srv.Close()
	go func() {
		for _, l := range lines {
			time.Sleep(400 * time.Millisecond)
			req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+wb.session+"/pty/"+id+"/input", strings.NewReader(l))
			req.Header.Set("X-Abhed-Tenant", "acme")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+wb.session+"/pty/"+id, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		wb.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out strings.Builder
	exit, event := "", ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "out":
			b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "data: "))
			out.Write(b)
		case strings.HasPrefix(line, "data: ") && event == "exit":
			exit = strings.TrimPrefix(line, "data: ")
		}
	}
	return out.String(), exit
}

// typed returns the lines the record holds as entered at a terminal.
func (wb *workbench) typed() []agent.TerminalInput {
	var out []agent.TerminalInput
	for _, e := range wb.events() {
		if e.Type == agent.EvTerminalInput {
			var in agent.TerminalInput
			_ = json.Unmarshal(e.Payload, &in)
			out = append(out, in)
		}
	}
	return out
}

// A person gets a session, and so a sandbox, without asking the agent
// anything. It is owned, listed and recorded like any other.
func TestWorkbenchSessionNeedsNoPrompt(t *testing.T) {
	wb := shellBench(t, nil)
	evs := wb.events()
	if len(evs) == 0 || evs[0].Type != agent.EvSessionStarted || !strings.Contains(string(evs[0].Payload), `"origin":"workbench"`) {
		t.Fatalf("the session's record does not open with session.started: %+v", evs)
	}
	var list []sessionSummary
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if !slices.ContainsFunc(list, func(s sessionSummary) bool { return s.ID == wb.session && s.State == "idle" && s.Mode == "default" }) {
		t.Fatalf("the session is not listed as idle: %+v", list)
	}
	if rec := wb.send("other", "POST", "pty", ptyStartRequest{Interactive: true}); rec.Code == http.StatusOK {
		t.Fatal("another tenant opened a terminal on this session")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a session with no prompt and no workbench flag: %d", rec.Code)
	}
}

// One shell per tab: it starts at the workspace root, keeps its state from
// line to line, and each line entered is in the record as the person's.
func TestShellKeepsStateAndRecordsLines(t *testing.T) {
	wb := shellBench(t, nil)
	wb.write("sub/f.txt", "x")
	start := wb.startShell()
	if !start.Interactive || start.ID == "" || start.Workspace != wb.workspace || start.Isolation == nil || start.Isolation.Tier != "none" {
		t.Fatalf("start: %+v", start)
	}
	out, exit := wb.typeLines(start.ID, enter("cd sub", "KEPT=yes", `echo "at $(pwd) kept=$KEPT"`, "exit")...)
	if !strings.Contains(out, "at "+filepath.Join(wb.workspace, "sub")+" kept=yes") || exit != "0" {
		t.Fatalf("output %q exit %q", out, exit)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		var lines []string
		for _, in := range wb.typed() {
			lines = append(lines, in.Line)
		}
		opened, closed := false, false
		for _, e := range wb.events() {
			opened = opened || (e.Type == agent.EvActionRequested && e.Actor == agent.ActorUser && strings.Contains(string(e.Payload), `"interactive":true`))
			closed = closed || (e.Type == agent.EvObservation && strings.Contains(string(e.Payload), "interactive terminal"))
		}
		if opened && closed && slices.Contains(lines, "cd sub") && slices.Contains(lines, "KEPT=yes") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the record is missing the shell or its lines: opened=%v closed=%v lines=%q", opened, closed, lines)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A line a deny rule matches, as typed, never reaches the shell; the refusal
// is in the record like any other.
func TestShellScreensDeniedLines(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	out, exit := wb.typeLines(start.ID, "touch shutdown-marker\r", "touch shut", "down-typed", "\r", "touch ok-marker\r", "exit\r")
	if fileExists(filepath.Join(wb.workspace, "shutdown-marker")) || fileExists(filepath.Join(wb.workspace, "shutdown-typed")) {
		t.Fatalf("a denied line ran:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(wb.workspace, "ok-marker")); err != nil {
		t.Fatalf("the shell stopped working after a refusal (exit %q):\n%s", exit, out)
	}
	if !strings.Contains(out, "Denied") {
		t.Fatalf("the refusal was not shown:\n%s", out)
	}
	denied := false
	for _, e := range wb.events() {
		denied = denied || e.Type == agent.EvActionDenied
	}
	if !denied {
		t.Fatal("the refusal is not in the record")
	}
}

// A line typed while the terminal does not echo, as at a password prompt, is
// recorded without its text.
func TestShellWithholdsWhatWasNotEchoed(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	out, _ := wb.typeLines(start.ID, enter(`read -s pw; echo "got ${#pw}"`, "hunter22", "exit")...)
	if !strings.Contains(out, "got 8") {
		t.Fatalf("the prompt did not take the input:\n%s", out)
	}
	time.Sleep(2 * echoWait)
	for _, e := range wb.events() {
		if strings.Contains(string(e.Payload), "hunter22") {
			t.Fatalf("an unechoed line reached the record: %s %s", e.Type, e.Payload)
		}
	}
	if !slices.ContainsFunc(wb.typed(), func(in agent.TerminalInput) bool { return in.Withheld != "" }) {
		t.Fatalf("the withheld line is not marked: %+v", wb.typed())
	}
}

// Where a shell cannot be offered honestly, the terminal judges each line.
func TestShellFallsBackToLines(t *testing.T) {
	for name, wb := range map[string]*workbench{
		"operator chose lines": shellBench(t, func(c *config.Config) { c.Sandbox.Terminal = "lines" }),
		"managed bash rules":   shellBench(t, func(c *config.Config) { c.Managed = true }),
		"no shell in sandbox":  manualBench(t, nil),
	} {
		rec := wb.send("acme", "POST", "pty", ptyStartRequest{Interactive: true})
		var got ptyStartResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != http.StatusOK || got.Lines == "" || got.ID != "" || got.Interactive {
			t.Errorf("%s: %d %+v", name, rec.Code, got)
		}
	}
}

// Plan mode is read-only for a person too: no shell.
func TestShellIsRefusedInPlanMode(t *testing.T) {
	wb := shellBench(t, func(c *config.Config) { c.Permissions.Mode = "plan" })
	if start := wb.startShell(); start.Denied == "" || start.Interactive {
		t.Fatalf("a shell opened in plan mode: %+v", start)
	}
}

// After a restart the page reopens the last session; its terminal reopens it
// from the record rather than answering 409.
func TestWorkbenchSessionReopensAfterRestart(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	dir := t.TempDir()
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	opts := Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Store: st,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{})}
	first := New(opts)
	wb := &workbench{t: t, h: first.Handler(), workspace: dir}
	wb.session = wb.openIdle("acme")
	first.drain()

	wb.h = New(opts).Handler()
	rec := wb.send("acme", "POST", "exec", execRequest{Command: "echo reopened"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "reopened") {
		t.Fatalf("the terminal could not reopen the session: %d %s", rec.Code, rec.Body)
	}
	if rec := wb.send("mallory", "POST", "exec", execRequest{Command: "echo x"}); rec.Code == http.StatusOK {
		t.Fatal("another tenant reopened the session")
	}
}

// The terminal's environment starts from the host's when the command left it
// unset; appending TERM to nothing gave the container CLI no PATH at all.
func TestTerminalEnvironmentKeepsTheHost(t *testing.T) {
	env := withTerm(nil)
	if !slices.Contains(env, "PATH="+os.Getenv("PATH")) || env[len(env)-1] != "TERM=xterm-256color" {
		t.Fatalf("env: %v", env)
	}
}

// The capture rebuilds a line from the keys typed, and says when it could not.
func TestLineCaptureRebuildsTypedLines(t *testing.T) {
	for keys, want := range map[string]struct {
		line   string
		edited bool
	}{
		"ls -la\r":                       {"ls -la", false},
		"ls -lx\x7fa\r":                  {"ls -la", true},
		"rm -rf x\x15echo hi\r":          {"echo hi", false},
		"\x1b[200~git status\x1b[201~\r": {"git status", false},
		"gi\tstatus\r":                   {"gistatus", true},
		"\x1b[A\r":                       {"", true},
		"echo one two\x17three\r":        {"echo one three", true},
	} {
		c := newLineCapture("u1", nil)
		chunks := c.keys([]byte(keys))
		e := chunks[len(chunks)-1].enter
		if e == nil || e.line != want.line || e.edited != want.edited || !e.whole {
			t.Errorf("%q: got %+v, want %q edited=%v", keys, e, want.line, want.edited)
		}
	}
	c := newLineCapture("u1", nil)
	c.output([]byte("\x1b[?1049h"))
	if chunks := c.keys([]byte(":wq\r")); !chunks[0].enter.alt {
		t.Error("a full-screen program's keys were taken for a shell line")
	}
}

// enter ends each line with the Enter key.
func enter(lines ...string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l + "\r"
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
