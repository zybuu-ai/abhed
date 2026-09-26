package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
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
	return shellBenchOpts(t, edit, nil)
}

// shellBenchOpts is shellBench with a change to the server's options.
func shellBenchOpts(t *testing.T, edit func(*config.Config), opt func(*Options)) *workbench {
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
	opts := Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, bash)}
	if opt != nil {
		opt(&opts)
	}
	s := New(opts)
	wb := &workbench{t: t, s: s, h: s.Handler(), workspace: dir}
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

// step is keys to type, then what to wait for in the output before the next
// step: a prompt by default when the keys end a line, or until when it is set.
// do, when set, runs instead of typing.
type step struct {
	keys   string
	until  string
	do     func(out string)
	nowait bool // for keys that show nothing, such as at a password prompt
}

// typeLines types each piece into a shell, waiting for its prompt between
// lines, and returns the output and the exit once the shell ends.
func (wb *workbench) typeLines(id string, pieces ...string) (string, string) {
	steps := make([]step, len(pieces))
	for i, p := range pieces {
		steps[i] = step{keys: p}
	}
	return wb.drive(id, steps...)
}

// drive follows a shell's output while taking each step once the output
// shows the previous one landed, rather than after a fixed pause.
func (wb *workbench) drive(id string, steps ...step) (string, string) {
	wb.t.Helper()
	srv := httptest.NewServer(wb.h)
	defer srv.Close()
	var mu sync.Mutex
	var out strings.Builder
	grew := make(chan struct{}, 1)
	snapshot := func() string { mu.Lock(); defer mu.Unlock(); return out.String() }
	waitFor := func(from int, pred func(string) bool) {
		deadline := time.After(10 * time.Second)
		for {
			if s := snapshot(); len(s) >= from && pred(s[from:]) {
				return
			}
			select {
			case <-grew:
			case <-deadline:
				return
			}
		}
	}
	// bash 5 follows its prompt with the bracketed-paste switch, so the check is on plain text.
	prompted := func(s string) bool {
		p := strings.TrimRight(plainText([]byte(s)), " ")
		return strings.HasSuffix(p, "$") || strings.HasSuffix(p, "#") // # when run as root
	}
	go func() {
		waitFor(0, prompted)
		for _, st := range steps {
			from := len(snapshot())
			if st.do != nil {
				st.do(snapshot())
				continue
			}
			req, _ := http.NewRequest("POST", srv.URL+"/v1/sessions/"+wb.session+"/pty/"+id+"/input", strings.NewReader(st.keys))
			req.Header.Set("X-Abhed-Tenant", "acme")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
			switch {
			case st.nowait:
			case st.until != "":
				waitFor(from, func(s string) bool { return strings.Contains(plainText([]byte(s)), st.until) })
			case strings.HasSuffix(st.keys, "\r"):
				waitFor(from, prompted)
			default:
				waitFor(from, func(s string) bool { return s != "" })
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
	exit, event := "", ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "out":
			b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "data: "))
			mu.Lock()
			out.Write(b)
			mu.Unlock()
			select {
			case grew <- struct{}{}:
			default:
			}
		case strings.HasPrefix(line, "data: ") && event == "exit":
			exit = strings.TrimPrefix(line, "data: ")
		}
	}
	// Plain text: bash 5 wraps each prompt in bracketed-paste switches.
	return plainText([]byte(snapshot())), exit
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

// Keys a program reads without an Enter (read -s -n) are the front of the
// next line the capture sees. That line was recorded in clear, secret and
// all, because only its tail was looked for in the echo; now the whole line
// must have been shown.
func TestShellWithholdsWhatReadNTook(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	steps := []step{{keys: "read -s -n 8 pw\r", until: "-n 8 pw"}}
	for _, k := range "hunter22" {
		steps = append(steps, step{keys: string(k), nowait: true})
	}
	steps = append(steps, step{do: func(string) { time.Sleep(200 * time.Millisecond) }})
	for _, k := range "echo hello world" {
		steps = append(steps, step{keys: string(k)})
	}
	steps = append(steps, step{keys: "\r", until: "\nhello world"}, step{keys: "exit\r"})
	wb.drive(start.ID, steps...)
	for _, in := range wb.typed() {
		if strings.Contains(in.Line, "hunter22") {
			t.Fatalf("a password read by read -n reached the record: %+v", in)
		}
	}
}

// Keys typed while a builtin keeps the shell busy are read later in canonical
// mode, here by read -s. A line entered in canonical mode is not one typed at
// bash's prompt, and its text is withheld.
func TestShellWithholdsTypeAheadDuringABuiltin(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	steps := []step{{keys: `end=$((SECONDS+2)); while [ $SECONDS -lt $end ]; do :; done; read -s pw; echo "len=${#pw}"` + "\r", until: "len="},
		{do: func(string) { time.Sleep(300 * time.Millisecond) }}}
	for _, k := range "hunter27" {
		steps = append(steps, step{keys: string(k), nowait: true})
	}
	steps = append(steps, step{keys: "\r", until: "len=8"}, step{keys: "exit\r"})
	out, _ := wb.drive(start.ID, steps...)
	if !strings.Contains(out, "len=8") {
		t.Fatalf("read -s did not take the typed-ahead line:\n%s", out)
	}
	for _, in := range wb.typed() {
		if strings.Contains(in.Line, "hunter27") {
			t.Fatalf("a password typed ahead reached the record: %+v", in)
		}
	}
}

// With bracketed paste (bash 5.1 and later), a pasted line waits for an
// Enter; the password typed after it is still withheld.
func TestShellWithholdsAPasswordAfterABracketedPaste(t *testing.T) {
	v, err := exec.Command("/bin/bash", "-c", `echo $((BASH_VERSINFO[0]*100+BASH_VERSINFO[1]))`).Output()
	if n, _ := strconv.Atoi(strings.TrimSpace(string(v))); err != nil || n < 501 {
		t.Skip("this bash has no bracketed paste")
	}
	wb := shellBench(t, nil)
	start := wb.startShell()
	out, _ := wb.drive(start.ID,
		step{keys: "\x1b[200~read -s pw\recho len=${#pw}\r\x1b[201~", nowait: true},
		step{do: func(string) { time.Sleep(300 * time.Millisecond) }},
		step{keys: "\r", nowait: true},
		step{do: func(string) { time.Sleep(300 * time.Millisecond) }},
		step{keys: "hunter28\r", until: "len=8"},
		step{keys: "exit\r"})
	if !strings.Contains(out, "len=8") {
		t.Fatalf("the pasted lines did not run as expected:\n%s", out)
	}
	for _, in := range wb.typed() {
		if strings.Contains(in.Line, "hunter28") {
			t.Fatalf("a password after a bracketed paste reached the record: %+v", in)
		}
	}
}

// A secret typed ahead while a command still runs, then edited once the
// password prompt is up, is never recorded in clear. It was recorded as
// "s3cretr" before: the one key typed after the edit matched stray output.
func TestShellWithholdsATypedAheadPassword(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	out, _ := wb.drive(start.ID,
		step{keys: `sleep 1; read -s -p "Password: " pw; echo "got ${#pw}"` + "\r", until: "s -p"},
		step{keys: "s3cretX", until: "s3cretX"},
		step{keys: "", until: "Password: "},
		step{keys: "\x7fr\r", until: "got 7"},
		step{keys: "exit\r"})
	if !strings.Contains(out, "got 7") {
		t.Fatalf("the prompt did not take the input:\n%s", out)
	}
	// Keys typed ahead were echoed while sleep ran, so they are in the output
	// the record keeps, as they were on screen; the line itself is not.
	for _, in := range wb.typed() {
		if strings.Contains(in.Line, "s3cret") || strings.Contains(in.Line, "3cretr") {
			t.Fatalf("a password reached the record as a line: %+v", in)
		}
	}
	if !slices.ContainsFunc(wb.typed(), func(in agent.TerminalInput) bool { return in.Withheld != "" }) {
		t.Fatalf("the password line is not marked as withheld: %+v", wb.typed())
	}
}

// Kill ends the shell's background jobs too: an interactive bash gives each
// its own process group, which killing the shell alone left running.
func TestShellKillEndsBackgroundJobs(t *testing.T) {
	forms := map[string]string{
		"a job":                    `sleep 300 & echo job=$!`,
		"a job outside its table":  `(sleep 301 & echo job=$! )`,
		"a job ignoring hang-up":   `trap '' HUP; sleep 302 & echo job=$!`,
		"a job that keeps forking": `( (trap '' HUP; while :; do sleep 303 & sleep 0.05; done) & echo job=$! )`,
	}
	for name, form := range forms {
		for _, byExit := range []bool{false, true} {
			wb := shellBench(t, nil)
			start := wb.startShell()
			end := step{do: func(string) {
				if rec := wb.send("acme", "DELETE", "pty/"+start.ID, nil); rec.Code != http.StatusNoContent {
					t.Errorf("kill: %d", rec.Code)
				}
			}}
			if byExit {
				end = step{keys: "exit\r"}
			}
			out, _ := wb.drive(start.ID, step{keys: form + "\r", until: "\njob="}, end)
			m := regexp.MustCompile(`\njob=(\d+)`).FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("%s: no job pid:\n%s", name, out)
			}
			pid, _ := strconv.Atoi(m[1])
			deadline := time.Now().Add(5 * time.Second)
			for processAlive(pid) {
				if time.Now().After(deadline) {
					killProcess(pid)
					t.Fatalf("%s (exit=%v): job %d outlived its shell", name, byExit, pid)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
}

// Switching the terminal to the alternate screen does not switch off the
// screen: on the process and none tiers, whether a program has the terminal
// is asked of the terminal, not guessed from what was printed.
func TestShellScreensAfterAnAlternateScreenSwitch(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	out, _ := wb.typeLines(start.ID, enter(`printf '\033[?1049h'`, "touch shutdown-alt", "echo after-alt", "exit")...)
	if fileExists(filepath.Join(wb.workspace, "shutdown-alt")) || !strings.Contains(out, "Denied") {
		t.Fatalf("a printf turned the screen off:\n%s", out)
	}
	if !slices.ContainsFunc(wb.typed(), func(in agent.TerminalInput) bool { return in.Line == "echo after-alt" }) {
		t.Fatalf("lines after a printf were not recorded: %+v", wb.typed())
	}
}

// Keys for a program other than the shell (here, a nested cat) are neither
// screened nor recorded as shell lines, and the shell's are again after it.
func TestShellLeavesAProgramsInputAlone(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startShell()
	wb.drive(start.ID,
		step{keys: "cat > note.txt\r", until: "note.txt"},
		step{do: func(string) { time.Sleep(300 * time.Millisecond) }}, // for cat to take the foreground
		step{keys: "kept shutdown words\r", until: "words"},
		step{keys: "\x04", until: "(no sandbox)"},
		step{keys: "echo back\r"},
		step{keys: "exit\r"})
	if got, _ := os.ReadFile(filepath.Join(wb.workspace, "note.txt")); string(got) != "kept shutdown words\n" {
		t.Fatalf("input to a program was screened: %q", got)
	}
	lines := wb.typed()
	if slices.ContainsFunc(lines, func(in agent.TerminalInput) bool { return strings.Contains(in.Line, "kept") }) ||
		!slices.ContainsFunc(lines, func(in agent.TerminalInput) bool { return in.Line == "echo back" }) {
		t.Fatalf("recorded: %+v", lines)
	}
}

// A shell is opened and typed into through the same authentication as any
// other call: a refusal by Middleware.Check, and a password that must be
// changed, stop both, for a shell already open as for a new one.
func TestShellRoutesHonourCheckAndMustChange(t *testing.T) {
	var refuse atomic.Bool
	check := func(_ context.Context, id *auth.Identity) error {
		if refuse.Load() && id.Subject == "bob" {
			return errors.New("bob's access was withdrawn")
		}
		return nil
	}
	shell := func(o *Options) {
		sb := sandbox.NewNone(sandbox.DefaultPolicy(o.Workspace))
		o.Registry = tools.NewRegistry(tools.Read{}, tools.Bash{Sandbox: sb.Command, Shell: sb.Shell,
			Isolation: tools.Isolation{Tier: "none", Backend: sb.Backend()}})
	}
	open := func(g *hookRig, bob *http.Cookie) (session, pty string) {
		rec := g.do(bob, "POST", "/v1/sessions", `{"workbench":true}`)
		var created createResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		rec = g.do(bob, "POST", "/v1/sessions/"+created.SessionID+"/pty", `{"interactive":true}`)
		var start ptyStartResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &start)
		if !start.Interactive {
			t.Fatalf("setup: %d %s", rec.Code, rec.Body)
		}
		return created.SessionID, start.ID
	}
	blocked := func(g *hookRig, bob *http.Cookie, session, pty, why string) {
		t.Helper()
		for _, c := range []struct{ method, path, body string }{
			{"POST", "/v1/sessions/" + session + "/pty", `{"interactive":true}`},
			{"POST", "/v1/sessions/" + session + "/pty/" + pty + "/input", "echo hi\r"},
			{"POST", "/v1/sessions/" + session + "/pty/" + pty + "/resize", `{"cols":80,"rows":24}`},
		} {
			// 403 with the reason, then 401 once the refusal has ended the session.
			if rec := g.do(bob, c.method, c.path, c.body); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: %s %s = %d %s", why, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}

	g := newHookRig(t, check, shell)
	bob := g.signIn(t, "bob")
	session, pty := open(g, bob)
	refuse.Store(true)
	blocked(g, bob, session, pty, "refused")

	g = newHookRig(t, nil, shell)
	bob = g.signIn(t, "bob")
	session, pty = open(g, bob)
	u, _ := g.local.Store.Get(context.Background(), "bob")
	if err := g.local.CreateUserOrReset(context.Background(), u, "temporary-pw-1"); err != nil {
		t.Fatal(err)
	}
	blocked(g, bob, session, pty, "must change")
}

// A sweep that did not run is one warning line with its reason.
func TestShellSweepNotRunIsLogged(t *testing.T) {
	var buf bytes.Buffer
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	live := &liveSession{ID: "s-1"}
	s.noteSweep(live, &ptyRun{id: "u-1"}, "")
	if buf.Len() != 0 {
		t.Fatalf("a sweep that ran was logged: %s", buf.String())
	}
	s.noteSweep(live, &ptyRun{id: "u-1"}, "the process at the shell's pid is not the shell")
	if got := buf.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "not swept") ||
		!strings.Contains(got, "terminal=u-1") || !strings.Contains(got, "not the shell") {
		t.Fatalf("log: %s", got)
	}
}

// fakeRedactor stands in for the secrets store: it replaces one value.
type fakeRedactor struct{ broken bool }

func (f fakeRedactor) Redact(b []byte) []byte {
	if f.broken {
		return append(b, '{')
	}
	return bytes.ReplaceAll(b, []byte("S3CR3T-VALUE"), []byte("[redacted]"))
}
func (fakeRedactor) Span() int { return 12 }

// What a shell records, the lines typed and its output, passes the redactor
// as a tool's result does; a redactor that fails withholds the payload.
func TestShellRecordIsRedacted(t *testing.T) {
	for _, broken := range []bool{false, true} {
		wb := shellBenchOpts(t, nil, func(o *Options) { o.Redact = fakeRedactor{broken: broken} })
		start := wb.startShell()
		wb.typeLines(start.ID, enter("echo S3CR3T-VALUE", "exit")...)
		deadline := time.Now().Add(3 * time.Second)
		for {
			var typed, closed bool
			for _, e := range wb.events() {
				if strings.Contains(string(e.Payload), "S3CR3T") {
					t.Fatalf("broken=%v: a secret reached the record: %s %s", broken, e.Type, e.Payload)
				}
				typed = typed || e.Type == agent.EvTerminalInput
				closed = closed || e.Type == agent.EvObservation
			}
			if typed && closed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("broken=%v: the shell's line or output was not recorded", broken)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// A shell opens, and takes keys, while an explorer operation holds the
// person's working directory.
func TestShellDoesNotWaitOnTheExplorer(t *testing.T) {
	wb := shellBench(t, nil)
	wb.s.mu.RLock()
	live := wb.s.running[wb.session]
	wb.s.mu.RUnlock()
	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	done := make(chan ptyStartResponse, 1)
	go func() { done <- wb.startShell() }()
	select {
	case start := <-done:
		out, _ := wb.typeLines(start.ID, enter("echo free", "exit")...)
		if !strings.Contains(out, "\nfree") {
			t.Fatalf("the shell did not run:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a shell waited on the explorer's lock")
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

// When the capture cannot be sure a line was shown as typed, it keeps the line
// without its text.
func TestLineCaptureWithholdsWhenUnsure(t *testing.T) {
	typed := func(keys string, echo string, known, secret bool) *enteredLine {
		c := newLineCapture("u1", nil)
		for i := range keys[:len(keys)-1] {
			c.keys([]byte{keys[i]})
			if i == 0 { // the terminal shows the given text once, as the line is typed
				c.output([]byte(echo))
			}
		}
		e := c.keys([]byte{keys[len(keys)-1]})[0].enter
		e.known, e.secret = known, secret
		return e
	}
	for name, tc := range map[string]struct {
		e    *enteredLine
		want bool
	}{
		"echoed long line":        {typed("git status\r", "git status", false, false), true},
		"not echoed":              {typed("hunter22\r", "\r\n", true, false), false},
		"password mode":           {typed("git status\r", "git status", true, true), false},
		"short, terminal asked":   {typed("ls\r", "ls", true, false), true},
		"short, terminal unknown": {typed("ls\r", "ls", false, false), false},
		"edit at the Enter":       {typed("abcdX\x7fr\r", "abcdX", true, false), false},
		"only the end shown":      {typed("hunter22echo hi there\r", "echo hi there", true, false), false},
		"all but one key shown":   {typed("echo hi there\r", "echo hi the", true, false), false},
	} {
		if got := tc.e.echoed(); got != tc.want {
			t.Errorf("%s: echoed() = %v, want %v (%+v)", name, got, tc.want, tc.e)
		}
	}
}

// A switch to the alternate screen split across two reads is still seen.
func TestLineCaptureSeesASplitScreenSwitch(t *testing.T) {
	c := newLineCapture("u1", nil)
	c.output([]byte("vim\x1b[?10"))
	c.output([]byte("49h~"))
	if !c.alt {
		t.Fatal("a switch split across reads was missed")
	}
}

// A flood of lines cannot turn into a flood of events.
func TestLineCaptureBoundsWhatWaits(t *testing.T) {
	var got []agent.TerminalInput
	var mu sync.Mutex
	c := newLineCapture("u1", func(in agent.TerminalInput) { mu.Lock(); got = append(got, in); mu.Unlock() })
	for _, k := range c.keys([]byte(strings.Repeat("abcdef\r", 500))) {
		c.entered(k.enter)
	}
	c.flush()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != maxPending+1 || !strings.Contains(got[len(got)-1].Withheld, "436 more lines") {
		t.Fatalf("%d events, last %+v", len(got), got[len(got)-1])
	}
}

// Another person in the same tenant cannot reopen someone's session.
func TestWorkbenchSessionReopensOnlyForItsOwner(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	dir := t.TempDir()
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	opts := Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{}, Store: st,
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{})}
	first := New(opts)
	h := first.Handler()
	do := func(h http.Handler, method, path, user, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Abhed-Tenant", "acme")
		req.Header.Set("X-Abhed-User", user)
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do(h, "POST", "/v1/sessions", "alice", `{"workbench":true}`)
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	first.drain()
	h = New(opts).Handler()
	if rec := do(h, "POST", "/v1/sessions/"+created.SessionID+"/exec", "bob", `{"command":"echo x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("another user in the tenant: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/sessions/"+created.SessionID+"/exec", "alice", `{"command":"echo mine"}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner: %d %s", rec.Code, rec.Body)
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

// A live workbench session has no end yet, and its report does not say it lacks one.
func TestLiveSessionReportHasNoMissingEnd(t *testing.T) {
	wb := shellBench(t, nil)
	if body := wb.get("acme", "hawkeye").Body.String(); strings.Contains(body, `"no-end"`) {
		t.Fatalf("a live session's report says it has no end: %s", body)
	}
}

// A shell the person ends, by exit or by closing it, ends normally: its
// status is recorded, and it is not an error.
func TestShellEndIsNotAnError(t *testing.T) {
	for _, byExit := range []bool{false, true} {
		wb := shellBench(t, nil)
		start := wb.startShell()
		end := step{do: func(string) {
			if rec := wb.send("acme", "DELETE", "pty/"+start.ID, nil); rec.Code != http.StatusNoContent {
				t.Errorf("kill: %d", rec.Code)
			}
		}}
		if byExit {
			end = step{keys: "exit 3\r", nowait: true}
		}
		wb.drive(start.ID, end)
		var obs *agent.Observation
		deadline := time.Now().Add(3 * time.Second)
		for obs == nil && time.Now().Before(deadline) {
			for _, e := range wb.events() {
				var o agent.Observation
				if e.Type == agent.EvObservation && json.Unmarshal(e.Payload, &o) == nil && o.CallID == start.ID {
					obs = &o
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		switch {
		case obs == nil:
			t.Fatalf("exit=%v: the shell's end is not recorded", byExit)
		case obs.IsError || obs.ExitCode == nil:
			t.Fatalf("exit=%v: the shell's end is recorded as an error: %+v", byExit, obs)
		case byExit && *obs.ExitCode != 3:
			t.Fatalf("exit 3 recorded as %d", *obs.ExitCode)
		case !byExit && *obs.ExitCode <= 128:
			t.Fatalf("a shell ended by a signal recorded exit %d, not 128 plus the signal", *obs.ExitCode)
		case !byExit && !strings.Contains(obs.Content, "closed from the workbench"):
			t.Fatalf("a closed shell does not say so: %q", obs.Content)
		}
	}
}
