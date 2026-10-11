package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/termline"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// seamTTY stands in for a shell's terminal: what it is sent, and an echo
// the test has it give back before Write returns, as a fast shell does.
type seamTTY struct {
	mu   sync.Mutex
	sent strings.Builder
	echo func(b []byte)
}

func (s *seamTTY) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.sent.Write(b)
	echo := s.echo
	s.mu.Unlock()
	if echo != nil {
		echo(b)
	}
	return len(b), nil
}

// seamShell is a container-tier shell (the terminal cannot be asked) with
// no engine behind it: its keys go to a seamTTY and its output is fed by
// the test, through the same code the pump runs.
func seamShell(t *testing.T) (*liveSession, *ptyRun, *seamTTY, *agent.MemStore) {
	t.Helper()
	store := agent.NewMemStore()
	live := &liveSession{ID: "s1", allowed: map[string]bool{}, Loop: &agent.Loop{
		Tools: tools.NewRegistry(tools.Bash{}), Policy: policy.New(policy.ModeDefault), Recorder: agent.NewRecorder(store, "s1", ""),
	}}
	tty := &seamTTY{}
	run := &ptyRun{id: "u1", input: tty, local: false, prompt: termline.NewPrompt(),
		capture: newLineCapture("u1", func(in agent.TerminalInput) { _ = live.Loop.ManualTerminalInput(in) })}
	return live, run, tty, store
}

func terminalInputs(t *testing.T, store *agent.MemStore) []agent.TerminalInput {
	t.Helper()
	evs, _ := store.Events("s1")
	var out []agent.TerminalInput
	for _, e := range evs {
		if e.Type == agent.EvTerminalInput {
			var in agent.TerminalInput
			_ = json.Unmarshal(e.Payload, &in)
			out = append(out, in)
		}
	}
	return out
}

// A pasted line is followed before it is written: a shell that echoes it
// before Write returns has its echo seen, so the line keeps its text. With
// the line followed after the write, the echo was missed and it was withheld.
func TestPastedLineIsFollowedBeforeItIsWritten(t *testing.T) {
	live, run, tty, store := seamShell(t)
	run.follow([]byte("\r\n(sandbox: container) ws $ "))
	tty.echo = func(b []byte) {
		if strings.Contains(string(b), "\r") {
			run.follow([]byte("echo hello\r\nhello\r\n"))
		}
	}
	if err := (&Server{}).shellInput(live, run, []byte("echo hello\r")); err != nil {
		t.Fatal(err)
	}
	run.capture.Flush()
	ins := terminalInputs(t, store)
	if len(ins) != 1 || ins[0].Line != "echo hello" || ins[0].Withheld != "" {
		t.Fatalf("recorded %+v", ins)
	}
}

// The container tier's shell, which cannot ask its terminal: a line at
// Abhed's own prompt is recorded, a line typed at a password prompt is
// withheld and taken out of the recorded output, and a refused line never
// reaches the shell.
func TestContainerShellLines(t *testing.T) {
	live, run, tty, store := seamShell(t)
	if err := live.Loop.Policy.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	run.follow([]byte("\r\n(sandbox: container) ws $ "))
	tty.echo = func(b []byte) {
		switch string(b) {
		case "ls -la\r":
			run.follow([]byte("ls -la\r\nmain.go\r\n(sandbox: container) ws $ "))
		case "read -s -p 'Password: ' pw\r":
			// A password prompt is not Abhed's; the answer typed there is not echoed.
			run.follow([]byte("read -s -p 'Password: ' pw\r\nPassword: "))
		}
	}
	for _, line := range []string{"ls -la\r", "read -s -p 'Password: ' pw\r"} {
		if err := server.shellInput(live, run, []byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.shellInput(live, run, []byte("hunter2-secret\r")); err != nil {
		t.Fatal(err)
	}
	run.follow([]byte("\r\n(sandbox: container) ws $ "))
	if err := server.shellInput(live, run, []byte("curl http://x\r")); err != nil {
		t.Fatal(err)
	}
	run.capture.Flush()
	ins := terminalInputs(t, store)
	if len(ins) != 3 || ins[0].Line != "ls -la" || ins[1].Line == "" || ins[2].Line != "" || ins[2].Withheld == "" {
		t.Fatalf("recorded %+v", ins)
	}
	if got := run.capture.Scrub("Password: hunter2-secret\nok"); strings.Contains(got, "hunter2") {
		t.Fatalf("the password stayed in the output: %q", got)
	}
	tty.mu.Lock()
	sent := tty.sent.String()
	tty.mu.Unlock()
	if strings.Contains(sent, "curl") {
		t.Fatalf("a refused line reached the shell: %q", sent)
	}
}

// On the container tier a printed switch to the alternate screen does not
// turn the screening off: the engine is asked whether a program has the
// terminal, and a line goes unscreened only when it says one does.
func TestContainerAltScreenAsksTheEngine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ask      func() (bool, bool)
		screened bool
	}{
		{"shell has it", func() (bool, bool) { return false, true }, true},
		{"engine cannot say", func() (bool, bool) { return false, false }, true},
		{"cannot ask", nil, true},
		{"program has it", func() (bool, bool) { return true, true }, false},
	} {
		live, run, tty, _ := seamShell(t)
		if err := live.Loop.Policy.AddDeny("bash(curl *)"); err != nil {
			t.Fatal(err)
		}
		run.foreground = tc.ask
		run.follow([]byte("\r\n(sandbox: container) ws $ \x1b[?1049h"))
		if err := (&Server{}).shellInput(live, run, []byte("curl http://x\r")); err != nil {
			t.Fatal(err)
		}
		tty.mu.Lock()
		sent := tty.sent.String()
		tty.mu.Unlock()
		if strings.Contains(sent, "curl") == tc.screened {
			t.Errorf("%s: screened=%v, sent %q", tc.name, tc.screened, sent)
		}
	}
}

func TestForegroundIsProgram(t *testing.T) {
	for stat, want := range map[string][2]bool{
		"1 (bash) S 0 1 1 34816 1 4194560 0":     {false, true},
		"1 (my prog) S 0 1 1 34816 57 4194560 0": {true, true},
		"1 (a) b) S 0 1 1 34816 -1 0":            {false, false},
		"garbage":                                {false, false},
		"1 (bash) S 0 x 1 34816 1":               {false, false},
	} {
		p, ok := foregroundIsProgram(stat)
		if p != want[0] || ok != want[1] {
			t.Errorf("%q: got %v %v", stat, p, ok)
		}
	}
}

// The probe runs the engine that started the shell against its container.
func TestEngineForegroundAsksThatContainer(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "engine")
	script := "#!/bin/sh\n[ \"$1 $2 $3 $4\" = \"exec abhed-term-x cat /proc/1/stat\" ] || exit 3\necho '1 (bash) S 0 1 1 34816 9 0'\n"
	if err := os.WriteFile(engine, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ask := engineForeground(exec.Command(engine, "run", "--rm", "--name", "abhed-term-x", "img"))
	if ask == nil {
		t.Fatal("no probe for a named container")
	}
	if p, ok := ask(); !p || !ok {
		t.Fatalf("got %v %v", p, ok)
	}
	if engineForeground(exec.Command(engine, "run", "img")) != nil {
		t.Fatal("a probe for an unnamed container")
	}
}
