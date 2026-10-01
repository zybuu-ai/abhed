package clitest

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/clitest/vt"
)

// DefaultTimeout is how long WaitText waits.
var DefaultTimeout = 20 * time.Second

// managedEnv names, in the test build only, the absolute directory holding
// a run's managed files, so each run has its own. Without it the build's
// managed paths are under its own directory, where nothing is written.
const managedEnv = "ABHED_CLITEST_MANAGED_DIR"

var build struct {
	once sync.Once
	dir  string
	bin  string
	err  error
}

// Binary builds the abhed binary for this test process, once, and returns
// its path. With the race detector on, the binary is built with it too.
func Binary(t testing.TB) string {
	t.Helper()
	build.once.Do(func() {
		build.dir, build.err = os.MkdirTemp("", "clitest-bin-")
		if build.err != nil {
			return
		}
		root, err := moduleRoot()
		if err != nil {
			build.err = err
			return
		}
		build.bin = filepath.Join(build.dir, "abhed")
		if runtime.GOOS == "windows" {
			build.bin += ".exe"
		}
		pkg := "github.com/zybuu-ai/abhed/internal/managed"
		none := filepath.Join(build.dir, "no-managed")
		args := []string{"build", "-o", build.bin, "-ldflags",
			"-X " + pkg + ".ConfigFile=" + filepath.Join(none, "config.json") + " -X " + pkg + ".AgentsDir=" + filepath.Join(none, "agents") +
				" -X " + pkg + ".testDirEnv=" + managedEnv}
		if raceEnabled {
			args = append(args, "-race")
		}
		args = append(args, "./cmd/abhed")
		cmd := exec.Command("go", args...) // #nosec G204 -- go build of this module, with fixed arguments
		cmd.Dir = root
		cmd.Env = withoutGOROOT(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			build.err = fmt.Errorf("go build: %w\n%s", err, out)
			return
		}
		// The first exec of a new binary pays for the system's checks of it,
		// which no start-up budget is about.
		_ = exec.Command(build.bin, "-version").Run() // #nosec G204 -- the binary this test process built
	})
	if build.err != nil {
		t.Fatalf("clitest: %v", build.err)
	}
	return build.bin
}

// Main runs a package's tests and removes the built binary afterwards. Use
// it from TestMain: os.Exit(clitest.Main(m)).
func Main(m *testing.M) int {
	code := m.Run()
	if build.dir != "" {
		_ = os.RemoveAll(build.dir)
	}
	return code
}

func withoutGOROOT(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "GOROOT=") {
			out = append(out, e)
		}
	}
	return out
}

func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("not in a module")
	}
	return filepath.Dir(mod), nil
}

// chunk is one read of the binary's output, or a resize when cols is set.
type chunk struct {
	at         time.Time
	off        int
	n          int
	cols, rows int
}

// rootLen is the length every run's directory has, whatever the system's
// temporary directory, so a path drawn on the screen wraps in the same
// place on every machine and a golden holds.
const rootLen = 40

// runRoot makes a directory for one run whose resolved path is rootLen
// bytes long where it can be.
func runRoot() (string, error) {
	base, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		if base, err = filepath.EvalSymlinks(os.TempDir()); err != nil {
			return "", err
		}
	}
	for i := 0; i < 100; i++ {
		var rnd [4]byte
		_, _ = crand.Read(rnd[:])
		name := "clitest-" + hex.EncodeToString(rnd[:])
		if pad := rootLen - len(base) - 1 - len(name); pad > 0 {
			name += strings.Repeat("x", pad)
		}
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o700); err == nil {
			return dir, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", errors.New("clitest: no free run directory")
}

// run is a running binary: the Harness.
type run struct {
	t       testing.TB
	o       Opts
	cmd     *exec.Cmd
	tty     *os.File
	stdin   io.WriteCloser
	term    *vt.Terminal
	stub    *Stub
	root    string
	home    string
	ws      string
	started time.Time

	wmu     sync.Mutex // input writes
	mu      sync.Mutex
	raw     []byte
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	chunks  []chunk
	mark    int
	readers sync.WaitGroup
	// pipes are a piped run's read ends of stdout and stderr.
	pipes []*os.File

	done     chan struct{}
	exitCode int
	exitErr  error
}

// Start builds the binary once per test process, runs it and returns the
// harness. The test fails if the binary cannot be built; it is skipped
// where there is no pseudo-terminal.
func Start(t testing.TB, o Opts) Harness {
	t.Helper()
	return start(t, o)
}

// StartRun is Start returning the concrete harness, with the extras.
func StartRun(t testing.TB, o Opts) *Run {
	t.Helper()
	return &Run{start(t, o)}
}

// Run is a Harness with the helpers beyond the shared API.
type Run struct{ *run }

func start(t testing.TB, o Opts) *run {
	t.Helper()
	if runtime.GOOS == "windows" && !o.Piped {
		t.Skip("clitest: no pseudo-terminal on windows")
	}
	if o.Cols == 0 {
		o.Cols = 80
	}
	if o.Rows == 0 {
		o.Rows = 24
	}
	bin := Binary(t)
	root, err := runRoot()
	if err != nil {
		t.Fatal(err)
	}
	h := &run{t: t, o: o, root: root, home: filepath.Join(root, "home"), ws: filepath.Join(root, "ws"),
		done: make(chan struct{})}
	for _, d := range []string{h.home, h.ws, filepath.Join(root, "bin"), filepath.Join(root, "tmp"),
		filepath.Join(root, "etc", "abhed", "agents")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The script may name the run's paths; the stub's own URL is not known yet.
	h.stub = NewStub(t, Script(strings.NewReplacer("{{HOME}}", h.home, "{{WS}}", h.ws).Replace(string(o.Script))))
	h.writeFiles()
	if o.Setup != nil {
		o.Setup(h.home, h.ws)
	}

	args := make([]string, len(o.Args))
	for i, a := range o.Args {
		args[i] = h.expand(a)
	}
	h.cmd = exec.Command(bin, args...) // #nosec G204 -- the binary this test process built, with the test's arguments
	h.cmd.Dir = h.ws
	h.cmd.Env = h.env()
	h.refuseRealServices(args, h.cmd.Env)
	h.term = vt.New(o.Cols, o.Rows, h.reply)
	h.term.NoSync = o.NoSyncOutput
	if o.Theme == "light" {
		h.term.Background = "rgb:ffff/ffff/ffff"
	}

	h.started = time.Now()
	if o.Piped {
		h.startPiped()
	} else {
		tty, err := pty.StartWithSize(h.cmd, &pty.Winsize{Cols: uint16(o.Cols), Rows: uint16(o.Rows)}) // #nosec G115 -- a test terminal size, far below 65535
		if err != nil {
			t.Skipf("clitest: no pseudo-terminal: %v", err)
		}
		h.tty = tty
		h.readers.Add(1)
		go h.read(tty, nil)
	}
	go func() {
		err := h.cmd.Wait()
		h.drainWait()
		h.exitErr = err
		h.exitCode = 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			h.exitCode = ee.ExitCode()
		} else if err != nil {
			h.exitCode = -1
		}
		close(h.done)
	}()
	t.Cleanup(h.cleanup)
	return h
}

func (h *run) startPiped() {
	var in io.WriteCloser
	if h.o.StdinFile != "" {
		f, err := os.Open(h.expand(h.o.StdinFile)) // #nosec G304 -- a file the test names
		if err != nil {
			h.t.Fatal(err)
		}
		h.t.Cleanup(func() { _ = f.Close() })
		h.cmd.Stdin = f
	} else {
		var err error
		if in, err = h.cmd.StdinPipe(); err != nil {
			h.t.Fatal(err)
		}
	}
	// Pipes of our own, not StdoutPipe: Wait closes those while a read may
	// still be under way, and output was lost. The readers here end at EOF,
	// once the binary and anything it started have closed their ends.
	outR, outW, err := os.Pipe()
	if err != nil {
		h.t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		h.t.Fatal(err)
	}
	h.cmd.Stdout, h.cmd.Stderr = outW, errW
	h.pipes = []*os.File{outR, errR}
	h.stdin = in
	// Written as a shell pipe would be, then closed unless the test keeps
	// it open to write more.
	go func() {
		if in == nil {
			return
		}
		h.wmu.Lock()
		defer h.wmu.Unlock()
		_, _ = io.WriteString(in, h.o.Stdin)
		if !h.o.KeepStdin {
			_ = in.Close()
		}
	}()
	h.readers.Add(2)
	go h.read(outR, &h.stdout)
	go h.read(errR, &h.stderr)
	err = h.cmd.Start()
	// The child has its copies; ours would keep the readers from ever ending.
	_, _ = outW.Close(), errW.Close()
	if err != nil {
		h.t.Fatal(err)
	}
}

// drainWait waits for the readers after the binary has exited. A process it
// left behind can hold the pipes open, so after a bound they are closed.
func (h *run) drainWait() {
	drained := make(chan struct{})
	go func() {
		h.readers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		for _, p := range h.pipes {
			_ = p.Close()
		}
		<-drained
	}
	for _, p := range h.pipes {
		_ = p.Close()
	}
}

// read copies one output stream into the log and the terminal.
func (h *run) read(r io.Reader, also *bytes.Buffer) {
	defer h.readers.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			now := time.Now()
			h.mu.Lock()
			h.chunks = append(h.chunks, chunk{at: now, off: len(h.raw), n: n})
			h.raw = append(h.raw, buf[:n]...)
			if also != nil {
				also.Write(buf[:n])
			}
			h.mu.Unlock()
			_, _ = h.term.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// reply sends the terminal's answer to a query back to the binary.
func (h *run) reply(b []byte) {
	if h.tty == nil {
		return
	}
	cp := append([]byte(nil), b...)
	go h.send(cp)
}

func (h *run) send(b []byte) {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	if h.tty != nil {
		_, _ = h.tty.Write(b)
		return
	}
	if h.stdin != nil {
		_, _ = h.stdin.Write(b)
	}
}

func (h *run) cleanup() {
	select {
	case <-h.done:
	default:
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill() // the binary this harness started
		}
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
		}
	}
	if h.tty != nil {
		_ = h.tty.Close()
	}
	out := Strip(h.Output())
	_, report, raced := strings.Cut(out, "WARNING: DATA RACE")
	if h.t.Failed() {
		h.t.Logf("clitest: screen at the end:\n%s\n--- output tail ---\n%s", h.Screen().Text(), tail(out, 3000))
	}
	_ = os.RemoveAll(h.root)
	// Last, since a skip ends this function. The line editor's known race
	// is pending on "editor", so the skip gate lists it; any other race fails.
	if raced && knownRace(out) {
		Pending(h.t, "editor", "the binary reported the line editor's known data race")
	}
	if raced {
		h.t.Errorf("clitest: the binary reported a data race:\nWARNING: DATA RACE%s", report)
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// expand replaces {{MODEL_URL}}, {{HOME}} and {{WS}}.
func (h *run) expand(s string) string {
	return strings.NewReplacer("{{MODEL_URL}}", h.stub.URL(), "{{HOME}}", h.home, "{{WS}}", h.ws).Replace(s)
}

// DefaultUserConfig is the user configuration a run gets unless Opts says
// otherwise: the stub as the only model, and no sandbox tier required.
const DefaultUserConfig = `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":` +
	`{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"stub-model","context_window":32768}}}}`

func (h *run) writeFiles() {
	write := func(path, body string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(h.expand(body)), mode); err != nil {
			h.t.Fatal(err)
		}
	}
	if !h.o.NoConfig {
		cfg := h.o.UserConfig
		if cfg == "" {
			cfg = DefaultUserConfig
		}
		write(filepath.Join(h.home, ".abhed", "config.json"), cfg, 0o600)
	}
	if h.o.Managed != "" {
		write(filepath.Join(h.root, "etc", "abhed", "config.json"), h.o.Managed, 0o644)
	}
	podman := h.o.Podman
	if podman == "" {
		podman = "exit 1"
	}
	write(filepath.Join(h.root, "bin", "podman"), "#!/bin/sh\n"+podman+"\n", 0o755)
	// A browser opener only notes what it was asked to open, so a test can
	// never start the developer's browser.
	for _, opener := range []string{"open", "xdg-open"} {
		write(filepath.Join(h.root, "bin", opener), "#!/bin/sh\necho \"$@\" >> \"$HOME/opened\"\n", 0o755)
	}
}

// realService matches a loopback address on a port a developer's own model
// services use; a test reaching one would talk to a real model.
var realService = regexp.MustCompile(`(?i)(localhost|127(\.\d{1,3}){3}|\[?::1\]?|0\.0\.0\.0):(4000|11434)\b`)

// refuseRealServices fails a run whose configuration, arguments or
// environment name such an address, before the binary starts.
func (h *run) refuseRealServices(args, env []string) {
	texts := append(append([]string{}, args...), env...)
	for _, f := range []string{filepath.Join(h.home, ".abhed", "config.json"), filepath.Join(h.ws, ".abhed", "config.json"),
		filepath.Join(h.root, "etc", "abhed", "config.json")} {
		if data, err := os.ReadFile(f); err == nil { // #nosec G304 -- a file this run wrote
			texts = append(texts, string(data))
		}
	}
	for _, s := range texts {
		if m := realService.FindString(s); m != "" {
			_ = os.RemoveAll(h.root) // no cleanup is registered yet
			h.t.Fatalf("clitest: the run names %s, a real local model service; tests use the stub", m)
		}
	}
}

func (h *run) env() []string {
	term := h.o.Term
	if term == "" {
		term = "xterm-256color"
	}
	env := []string{
		"HOME=" + h.home,
		"USER=clitest",
		"PATH=" + filepath.Join(h.root, "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"TERM=" + term,
		"LANG=en_US.UTF-8",
		"TMPDIR=" + filepath.Join(h.root, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(h.home, ".config"),
		// Anything that tries to leave the machine meets a closed port, and
		// a first run looks for Ollama there, never at the real one.
		// The proxy catches what leaves the machine. Go never proxies loopback,
		// so refuseRealServices is what keeps a run off :4000 and :11434.
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1",
		"OLLAMA_HOST=127.0.0.1:9",
		managedEnv + "=" + filepath.Join(h.root, "etc", "abhed"),
	}
	switch h.o.Theme {
	case "light":
		env = append(env, "COLORFGBG=0;15")
	case "no-color":
		env = append(env, "NO_COLOR=1")
	default:
		env = append(env, "COLORFGBG=15;0")
	}
	for _, e := range h.o.Env {
		env = append(env, h.expand(e))
	}
	return env
}

// Home is the run's HOME, and Workspace its default workspace and working
// directory.
func (h *run) Home() string      { return h.home }
func (h *run) Workspace() string { return h.ws }

// Stub is the scripted model the run talks to.
func (h *run) Stub() *Stub { return h.stub }

// Started is when the binary was spawned.
func (h *run) Started() time.Time { return h.started }

// Type sends text as typed keys.
func (h *run) Type(text string) { h.send([]byte(text)) }

// Key sends key presses in order.
func (h *run) Key(keys ...Key) {
	for _, k := range keys {
		h.send([]byte(k))
	}
}

// Paste sends text as one paste.
func (h *run) Paste(text string, mode PasteMode) {
	if mode == Bracketed {
		text = "\x1b[200~" + text + "\x1b[201~"
	}
	h.send([]byte(text))
}

// Resize changes the terminal size; the kernel signals the binary.
func (h *run) Resize(cols, rows int) {
	h.mu.Lock()
	h.chunks = append(h.chunks, chunk{at: time.Now(), off: len(h.raw), cols: cols, rows: rows})
	h.term.Resize(cols, rows)
	h.mu.Unlock()
	if h.tty != nil {
		_ = pty.Setsize(h.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}) // #nosec G115 -- a test terminal size, far below 65535
	}
}

// WaitScreen waits until cond holds.
func (h *run) WaitScreen(cond func(Screen) bool, timeout time.Duration) Screen {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s := h.Screen()
		if cond(s) {
			return s
		}
		select {
		case <-h.done:
			if s = h.Screen(); cond(s) {
				return s
			}
			h.t.Fatalf("clitest: the binary exited (code %d) before the screen matched:\n%s", h.exitCode, s.Text())
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("clitest: timed out after %v waiting for the screen:\n%s", timeout, s.Text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitText waits until text is on the screen.
func (h *run) WaitText(text string) Screen {
	h.t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for {
		s := h.Screen()
		if s.Contains(text) {
			return s
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("clitest: never saw %q on the screen:\n%s\n--- output tail ---\n%s", text, s.Text(), tail(Strip(h.Output()), 2000))
		}
		select {
		case <-h.done:
			if s = h.Screen(); s.Contains(text) {
				return s
			}
			h.t.Fatalf("clitest: the binary exited (code %d) before %q showed:\n%s\n--- output tail ---\n%s",
				h.exitCode, text, s.Text(), tail(Strip(h.Output()), 2000))
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitOutput waits until the ANSI-stripped output holds text, wherever it
// scrolled to, and returns how long after the spawn it first appeared.
func (h *run) WaitOutput(text string) time.Duration {
	h.t.Helper()
	deadline := time.Now().Add(DefaultTimeout)
	for {
		if d, ok := h.FirstSeen(text); ok {
			return d
		}
		exited := false
		select {
		case <-h.done:
			exited = true
		default:
		}
		if exited {
			if d, ok := h.FirstSeen(text); ok {
				return d
			}
			h.t.Fatalf("clitest: the binary exited (code %d) before %q was written:\n%s", h.exitCode, text, tail(Strip(h.Output()), 3000))
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("clitest: %q was never written:\n%s", text, tail(Strip(h.Output()), 3000))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FirstSeen is how long after the spawn the stripped output first held text.
func (h *run) FirstSeen(text string) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var acc []byte
	for _, c := range h.chunks {
		acc = append(acc, h.raw[c.off:c.off+c.n]...)
		if strings.Contains(Strip(acc), text) {
			return c.at.Sub(h.started), true
		}
	}
	return 0, false
}

// WaitQuiet waits until nothing has been written for quiet, or max passes.
func (h *run) WaitQuiet(quiet, max time.Duration) {
	h.WaitSettled(time.Time{}, quiet, max)
}

// Settle waits until the binary has stopped writing for 80 ms (at most
// 5 s), so the next keys do not arrive while it draws.
func (h *run) Settle() { h.WaitQuiet(80*time.Millisecond, 5*time.Second) }

// WaitSettled waits until something has been written after since and then
// nothing for quiet, or max passes.
func (h *run) WaitSettled(since time.Time, quiet, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		last := h.started
		if n := len(h.chunks); n > 0 {
			last = h.chunks[n-1].at
		}
		h.mu.Unlock()
		if !last.Before(since) && time.Since(last) >= quiet {
			return
		}
		time.Sleep(max(quiet/4, time.Millisecond))
	}
}

// Screen is the screen now.
func (h *run) Screen() Screen { return screen{h.term.Snapshot()} }

// Scrollback is the lines scrolled off the top.
func (h *run) Scrollback() []string { return h.term.Snapshot().Scrollback }

// Output is every byte the binary wrote, stdout and stderr.
func (h *run) Output() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.raw...)
}

// Stdout and Stderr are the separate streams of a piped run.
func (h *run) Stdout() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stdout.String()
}

func (h *run) Stderr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stderr.String()
}

// MarkBytes starts counting output bytes.
func (h *run) MarkBytes() {
	h.mu.Lock()
	h.mark = len(h.raw)
	h.mu.Unlock()
}

// BytesSinceMark is the count since MarkBytes.
func (h *run) BytesSinceMark() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.raw) - h.mark
}

// Deltas are the stub model's streamed fragments.
func (h *run) Deltas() []Delta { return h.stub.Deltas() }

// Requests are what the binary sent the stub model.
func (h *run) Requests() []Request { return h.stub.Requests() }

// Latency is how long after the stub sent d its text first reached the
// terminal: the first read after SentAt from which the output, stripped,
// holds the start of the fragment.
func (h *run) Latency(d Delta) (time.Duration, bool) {
	probe := []rune(strings.TrimSpace(d.Text))
	if len(probe) == 0 {
		return 0, false
	}
	probe = probe[:min(len(probe), 8)]
	h.mu.Lock()
	defer h.mu.Unlock()
	var acc []byte
	for _, c := range h.chunks {
		if c.at.Before(d.SentAt) {
			continue
		}
		acc = append(acc, h.raw[c.off:c.off+c.n]...)
		if strings.Contains(Strip(acc), string(probe)) {
			return c.at.Sub(d.SentAt), true
		}
	}
	return 0, false
}

// Record is the session's record, read and verified.
func (h *run) Record() Record {
	h.t.Helper()
	r, err := readRecord(h.home)
	if errors.Is(err, errNoRecord) {
		Pending(h.t, "record", err.Error())
		h.t.Fatalf("clitest: %v", err)
	}
	if err != nil {
		h.t.Fatalf("clitest: reading the record: %v", err)
	}
	return r
}

// Exit closes input, waits for the binary to end and fails the test unless
// it exits with code.
func (h *run) Exit(code int) {
	h.t.Helper()
	got := h.closeAndWait(20 * time.Second)
	if got == raceExit && got != code && knownRace(Strip(h.Output())) {
		Pending(h.t, "editor", "the race detector stopped the binary on the line editor's known race")
	}
	if got != code {
		h.t.Fatalf("clitest: exit code %d, want %d\n%s", got, code, tail(Strip(h.Output()), 3000))
	}
}

// raceExit is the status a -race binary exits with after reporting a race.
const raceExit = 66

// knownRace reports whether every race the binary reported is the line
// editor's, whose state its input and output goroutines share without a
// lock. Any other race fails the test.
func knownRace(out string) bool {
	blocks := strings.Split(out, "WARNING: DATA RACE")[1:]
	if len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		b, _, _ = strings.Cut(b, "==================")
		if !strings.Contains(b, "internal/ui.(*editor).") {
			return false
		}
	}
	return true
}

// Wait waits for the binary to end on its own and returns its exit code.
func (h *run) Wait(timeout time.Duration) int {
	h.t.Helper()
	select {
	case <-h.done:
		return h.exitCode
	case <-time.After(timeout):
		h.t.Fatalf("clitest: still running after %v:\n%s", timeout, tail(Strip(h.Output()), 3000))
		return -1
	}
}

func (h *run) closeAndWait(timeout time.Duration) int {
	h.t.Helper()
	select {
	case <-h.done:
		return h.exitCode
	default:
	}
	if h.tty != nil {
		// Ctrl-U clears a half-typed line; Ctrl-D is then end of input to a
		// cooked terminal and exit at an idle prompt, so it is sent again
		// until a turn still running has ended.
		h.send([]byte{0x15, 4})
		go func() {
			for {
				select {
				case <-h.done:
					return
				case <-time.After(500 * time.Millisecond):
					if h.Screen().Modes().AltScreen {
						h.send([]byte("q")) // a full-screen view closes first
					}
					h.send([]byte{0x15, 4})
				}
			}
		}()
	} else if h.stdin != nil {
		_ = h.stdin.Close()
	}
	return h.Wait(timeout)
}

// ansi matches the escape sequences Strip removes.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?<>=!]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>78DEMc]`)

// Strip removes escape sequences and carriage returns.
func Strip(b []byte) string {
	return strings.ReplaceAll(ansi.ReplaceAllString(string(b), ""), "\r", "")
}

// screen adapts a vt snapshot to Screen.
type screen struct{ s *vt.Snapshot }

func (s screen) Cols() int                 { return s.s.Cols() }
func (s screen) Rows() int                 { return s.s.Rows() }
func (s screen) Line(i int) string         { return s.s.Line(i) }
func (s screen) Text() string              { return s.s.Text() }
func (s screen) Contains(text string) bool { return s.s.Contains(text) }
func (s screen) Cursor() (row, col int)    { return s.s.Cursor() }
func (s screen) Title() string             { return s.s.Title() }
func (s screen) Modes() vt.Modes           { return s.s.Modes }
func (s screen) Snapshot() *vt.Snapshot    { return s.s }
func (s screen) String() string            { return s.s.Text() }
func (s screen) Cell(row, col int) Cell {
	c := s.s.Cell(row, col)
	return Cell{Rune: c.Rune, Width: c.Width, Attrs: Attrs(c.Attrs)}
}
