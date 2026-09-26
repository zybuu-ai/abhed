package sandbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// typeInto runs an interactive shell on a terminal, types each line into it a
// moment apart, and returns everything the terminal showed.
func typeInto(t *testing.T, s Interactive, cwd string, lines ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := s.Shell(ctx, cwd)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start the shell: %v", err)
	}
	defer func() { _ = tty.Close() }()
	var mu sync.Mutex
	var out bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	for _, l := range lines {
		time.Sleep(300 * time.Millisecond)
		_, _ = tty.Write([]byte(l + "\r"))
	}
	_ = cmd.Wait()
	select {
	case <-read:
	case <-time.After(time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String()
}

// The workbench's shell keeps its state from line to line, starts where it is
// told, and is held by the same boundary as a single command: nothing written
// outside the workspace, and the harness's own state out of sight.
func TestProcessSandboxShellIsInteractiveAndConfined(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)
	if err := os.MkdirAll(filepath.Join(ws, stateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, stateDir, "users.json"), []byte("state-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := "/usr/local/abhed-shell-probe.txt"
	out := typeInto(t, s.(Interactive), ws,
		`KEPT=yes`,
		`echo "cwd=$PWD kept=$KEPT"`,
		`echo inside > in.txt`,
		`echo x > `+target+` 2>/dev/null`,
		`cat .abhed/users.json 2>/dev/null; echo`,
		`exit`)
	if !strings.Contains(out, "cwd="+ws+" kept=yes") {
		t.Fatalf("the shell did not start in the workspace or keep its state:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "in.txt")); string(got) != "inside\n" {
		t.Fatalf("a write inside the workspace did not land: %q\n%s", got, out)
	}
	if _, err := os.Stat(target); err == nil {
		_ = os.Remove(target)
		t.Fatalf("ESCAPE: the shell wrote outside the workspace to %s", target)
	}
	if strings.Contains(out, "state-secret") {
		t.Fatalf("ESCAPE: the shell read the harness's own state\n%s", out)
	}
}

// Ending a shell ends the jobs it started in the background. An interactive
// bash gives each its own process group, so killing the shell alone left them
// running; it is hung up instead, and what is left in its session is ended
// while the exited shell still holds the session id.
func TestProcessSandboxShellEndsItsBackgroundJobs(t *testing.T) {
	requireNetNS(t)
	for _, job := range jobForms {
		for _, byExit := range []bool{false, true} {
			ws := workspace(t)
			shellEndsJobs(t, processSandbox(t, ws, false).(Interactive), ws, job, byExit)
		}
	}
}

// jobForms are the ways a job can be left running: in the job table, out of
// it in a subshell, ignoring the hang-up, and forking again and again. None
// may outlive the shell.
var jobForms = []string{
	"(sleep 3; touch late) &",
	"((sleep 3; touch late) & )",
	"trap '' HUP; (sleep 3; touch late) &",
	"(trap '' HUP; while :; do (sleep 3; touch late) & sleep 0.05; done &)",
}

func TestNoneShellEndsItsBackgroundJobs(t *testing.T) {
	for _, job := range jobForms {
		for _, byExit := range []bool{false, true} {
			ws := workspace(t)
			shellEndsJobs(t, NewNone(DefaultPolicy(ws)), ws, job, byExit)
		}
	}
}

// A sweep that cannot run says why, so the server can log that containment
// did not happen rather than pass over it in silence.
func TestSweepSaysWhyItDidNotRun(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if refused, _ := (Leader{Pid: cmd.Process.Pid}).Wait(cmd); refused == "" {
		t.Fatal("a leader that was never named was swept without a word")
	}
}

// A session is swept only while the shell that led it is exited but not yet
// reaped, and only if the process at its pid started when the shell did: a
// reused pid can never be taken for the shell.
func TestSweepNeedsTheShellItNamed(t *testing.T) {
	ws := workspace(t)
	cmd := NewNone(DefaultPolicy(ws)).Shell(context.Background(), ws)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	go func() { _, _ = io.Copy(io.Discard, tty) }()
	l := Lead(cmd)
	if !l.ok {
		t.Fatal("the shell's start time could not be read")
	}
	_, _ = tty.Write([]byte("(sleep 30 & echo $! > job )\r"))
	var job int
	for deadline := time.Now().Add(10 * time.Second); job == 0; time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(filepath.Join(ws, "job"))
		job, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if time.Now().After(deadline) {
			t.Fatal("the job did not start")
		}
	}
	defer func() { _ = syscall.Kill(job, syscall.SIGKILL) }()

	if other := (Leader{Pid: l.Pid, start: l.start + 1, ok: true}); other.sweep() == "" {
		t.Fatal("swept for a leader whose start time does not match")
	}
	_, _ = tty.Write([]byte("exit\r"))
	_ = cmd.Wait() // reaped here, bypassing Leader.Wait
	if l.sweep() == "" {
		t.Fatal("swept after the shell was reaped")
	}
	if syscall.Kill(job, 0) != nil {
		t.Fatal("a refused sweep still ended the job")
	}
}

// byExit ends the shell with exit rather than by cancelling it.
func shellEndsJobs(t *testing.T, s Interactive, ws, job string, byExit bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := s.Shell(ctx, ws)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	l := Lead(cmd)
	go func() { _, _ = io.Copy(io.Discard, tty) }()
	_, _ = tty.Write([]byte(job + "\r"))
	started := filepath.Join(ws, "started")
	_, _ = tty.Write([]byte("touch started\r"))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell did not run the commands")
		}
	}
	if byExit {
		_, _ = tty.Write([]byte("exit\r"))
	} else {
		cancel()
	}
	if refused, _ := l.Wait(cmd); refused != "" {
		t.Fatalf("the sweep did not run: %s", refused)
	}
	_ = tty.Close()
	time.Sleep(3500 * time.Millisecond) // past the jobs' three seconds
	if _, err := os.Stat(filepath.Join(ws, "late")); err == nil {
		t.Fatalf("%q outlived its shell (exit=%v)", job, byExit)
	}
}

// The no-sandbox tier's shell says so in its prompt.
func TestNoneShellNamesItself(t *testing.T) {
	ws := workspace(t)
	out := typeInto(t, NewNone(DefaultPolicy(ws)), ws, `echo "tier=$ABHED_SANDBOX"`, `exit`)
	if !strings.Contains(out, "(no sandbox)") || !strings.Contains(out, "tier=none") {
		t.Fatalf("the host shell did not name itself:\n%s", out)
	}
}

// The container engine's CLI needs the host's environment (PATH, HOME,
// DOCKER_HOST) to find the engine at all. It once ran with only TERM, because
// the terminal appended to an environment the command had left unset.
func TestContainerCommandKeepsTheHostEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/abhed-probe.sock")
	ws := workspace(t)
	c := &Container{policy: DefaultPolicy(ws), runtime: "docker"}
	for name, env := range map[string][]string{
		"command": c.Command(context.Background(), ws, "true").Env,
		"shell":   c.Shell(context.Background(), ws).Env,
	} {
		if !slices.Contains(env, "DOCKER_HOST=unix:///tmp/abhed-probe.sock") || !slices.Contains(env, "PATH="+os.Getenv("PATH")) {
			t.Errorf("%s: the engine's CLI lost the host environment: %d entries", name, len(env))
		}
	}
	args := c.Shell(context.Background(), ws).Args
	if !slices.Contains(args, "-t") || !slices.Contains(args, "--network") || !slices.Contains(args, "--read-only") || !slices.Contains(args, c.shellLabel()) {
		t.Errorf("the shell's container is missing its terminal, label or confinement: %v", args)
	}
}

// The host shell does not hand a person the server's own settings, which
// hold the database URLs and the provider key.
func TestNoneShellLeavesOutTheServersSettings(t *testing.T) {
	t.Setenv("ABHED_DATABASE_URL", "postgres://owner:secret@db/abhed")
	ws := workspace(t)
	for _, kv := range NewNone(DefaultPolicy(ws)).Shell(context.Background(), ws).Env {
		if strings.HasPrefix(kv, "ABHED_DATABASE_URL=") {
			t.Fatalf("the shell has %s", kv)
		}
	}
}

// A command on the host runs no BASH_ENV file first, and its cd goes where
// it says rather than through CDPATH, which the directory tracker follows.
func TestNoneCommandLeavesOutBashEnvAndCdpath(t *testing.T) {
	ws := workspace(t)
	hook := filepath.Join(ws, "hook.sh")
	if err := os.WriteFile(hook, []byte("echo HOOK-RAN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "elsewhere", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", hook)
	t.Setenv("CDPATH", filepath.Join(ws, "elsewhere"))
	out, err := NewNone(DefaultPolicy(ws)).Command(context.Background(), ws, `echo "[$BASH_ENV][$CDPATH]"; cd x 2>/dev/null; pwd`).CombinedOutput()
	if err != nil || strings.Contains(string(out), "HOOK-RAN") || !strings.HasPrefix(string(out), "[][]\n") || strings.Contains(string(out), "elsewhere") {
		t.Fatalf("the host command kept BASH_ENV or CDPATH: %v\n%s", err, out)
	}
}

// vim in the sandbox quits on :wq; a failed history write under home left it
// waiting at "Press ENTER", which read as a terminal that hung.
func TestProcessSandboxVimQuitsOnWriteQuit(t *testing.T) {
	requireNetNS(t)
	vim, err := exec.LookPath("vim")
	if err != nil {
		t.Skip("vim is not installed")
	}
	ws, home := workspace(t), workspace(t)
	// Without a TMPDIR the sandbox writes only to the workspace and /tmp, so
	// macOS lets it read this home and not write it, as it is the person's own.
	t.Setenv("TMPDIR", "")
	t.Setenv("HOME", home)
	// vim only complains when it has a history file to replace.
	if err := os.WriteFile(filepath.Join(home, ".viminfo"), []byte("# viminfo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The person's own vimrc is still read: this one leaves a mark on the way out.
	rc := "autocmd VimLeave * call writefile([$MYVIMRC], 'rc-read')\n"
	if err := os.WriteFile(filepath.Join(home, ".vimrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	s := processSandbox(t, ws, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := s.Command(ctx, ws, vim+" note.md")
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start vim: %v", err)
	}
	defer func() { _ = tty.Close() }()
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	for _, k := range []string{"i", "hi", "\x1b", ":wq", "\r"} {
		time.Sleep(300 * time.Millisecond)
		_, _ = tty.Write([]byte(k))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("vim exited with %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("vim did not quit after :wq; the terminal showed:\n%q", out.String())
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "note.md")); string(got) != "hi\n" {
		t.Fatalf("the file holds %q, want the typed line", got)
	}
	// bwrap shows no home directory, so on Linux vim runs with its defaults.
	if runtime.GOOS != "darwin" {
		return
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "rc-read")); string(got) != filepath.Join(home, ".vimrc")+"\n" {
		t.Fatalf("the person's vimrc was not read, or $MYVIMRC not set: %q", got)
	}
}
