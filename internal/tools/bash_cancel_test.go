//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// A cancelled command ends everything it started, in every tier: the turn
// stops promptly and no child is left running on the host.
func TestBashCancelEndsWhatTheCommandStarted(t *testing.T) {
	tiers := []struct {
		name string
		box  func(ws string) (sandbox.Sandbox, string)
	}{
		{"host", func(string) (sandbox.Sandbox, string) { return nil, "" }},
		{"none", func(ws string) (sandbox.Sandbox, string) { return sandbox.NewNone(sandbox.DefaultPolicy(ws)), "" }},
		{"process", func(ws string) (sandbox.Sandbox, string) {
			s := sandbox.NewProcess(sandbox.DefaultPolicy(ws))
			_, why := s.Available()
			return s, runsHere(s, ws, why)
		}},
		{"container", func(ws string) (sandbox.Sandbox, string) {
			c := sandbox.NewContainer(sandbox.DefaultPolicy(ws))
			_, why := c.Available()
			return c, runsHere(c, ws, why)
		}},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			s, dir := setup(t)
			b := Bash{}
			box, unavailable := tier.box(dir)
			if unavailable != "" {
				t.Skipf("%s sandbox unavailable: %s", tier.name, unavailable)
			}
			if box != nil {
				b.Sandbox = box.Command
			}
			// The child is not the shell's last command, so the shell forks it
			// rather than exec'ing it, and the child holds the output pipe.
			pidFile := filepath.Join(dir, "child.pid")
			args, _ := json.Marshal(bashArgs{
				Command:     `sh -c 'echo $$ > ` + pidFile + `; exec sleep 60'; echo after`,
				Description: "a long command", TimeoutMS: 120_000,
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Result, 1)
			go func() { done <- b.Run(ctx, s, args) }()

			pid := waitForPid(t, pidFile, done)
			t.Cleanup(func() {
				if alive(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			cancel()
			select {
			case res := <-done:
				if !strings.Contains(res.Content, "cancelled") {
					t.Errorf("result after cancel: %q", res.Content)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the command still held the call 3s after cancel")
			}
			// A container's pid is inside its own namespace; the host check
			// applies to the tiers that run on the host.
			if tier.name == "container" {
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for alive(pid) {
				if time.Now().After(deadline) {
					t.Fatalf("child %d outlived the cancel", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// A command that exits and leaves a background job holding its output
// returns, rather than waiting for the job.
func TestBashReturnsWhenABackgroundJobHoldsTheOutput(t *testing.T) {
	s, dir := setup(t)
	pidFile := filepath.Join(dir, "bg.pid")
	start := time.Now()
	res := run(t, Bash{}, s, bashArgs{
		Command:     `sleep 30 & echo $! > ` + pidFile + `; echo started`,
		Description: "background job",
	})
	if time.Since(start) > 10*time.Second {
		t.Fatalf("waited %s for a background job", time.Since(start))
	}
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if res.IsError || res.ExitCode == nil || *res.ExitCode != 0 || !strings.Contains(res.Content, "started") ||
		!strings.Contains(res.Content, "still running") {
		t.Fatalf("result: %+v", res)
	}
}

func waitForPid(t *testing.T, path string, done <-chan Result) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case res := <-done:
			t.Fatalf("the command ended before it started its child: %s", res.Content)
		default:
		}
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the child never started")
	return 0
}

func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// The note that a job still holds the output is given whatever the exit status.
func TestBashBackgroundNoteOnAFailingCommand(t *testing.T) {
	s, dir := setup(t)
	pidFile := filepath.Join(dir, "bg.pid")
	res := run(t, Bash{}, s, bashArgs{Command: `sleep 30 & echo $! > ` + pidFile + `; exit 3`, Description: "background job"})
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if res.ExitCode == nil || *res.ExitCode != 3 || !strings.Contains(res.Content, "still running") {
		t.Fatalf("result: %+v", res)
	}
}

// On the host a refusal is the system's, not a sandbox's, and the result says
// which tier the command ran under.
func TestBashOnTheHostGivesNoSandboxHint(t *testing.T) {
	s, dir := setup(t)
	res := run(t, Bash{}, s, bashArgs{Command: `echo "x: Operation not permitted"`, Description: "print"})
	if res.Tier != "none" || strings.Contains(res.Content, "the sandbox denied") {
		t.Fatalf("host result: %+v", res)
	}
	sb := Bash{Sandbox: sandbox.NewNone(sandbox.DefaultPolicy(dir)).Command, Isolation: Isolation{Tier: "process"}}
	if res := run(t, sb, s, bashArgs{Command: `echo "x: Operation not permitted"`, Description: "print"}); res.Tier != "process" ||
		!strings.Contains(res.Content, "the sandbox denied") {
		t.Fatalf("sandboxed result: %+v", res)
	}
	// Started before the sandbox was chosen, which then turned out to be none.
	late := Bash{Sandbox: sandbox.NewNone(sandbox.DefaultPolicy(dir)).Command, RanUnder: func() string { return "none" }}
	if res := run(t, late, s, bashArgs{Command: `echo "x: Operation not permitted"`, Description: "print"}); res.Tier != "none" ||
		strings.Contains(res.Content, "the sandbox denied") {
		t.Fatalf("result once chosen as none: %+v", res)
	}
}

// A command stopped by the run's own deadline says so, rather than advising a
// longer timeout_ms that would not have helped.
func TestBashRunDeadlineIsNotTheCallsTimeout(t *testing.T) {
	s, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	args, _ := json.Marshal(bashArgs{Command: "sleep 5; echo after", Description: "long", TimeoutMS: 60_000})
	res := Bash{}.Run(ctx, s, args)
	if !strings.Contains(res.Content, "the run's time limit passed") || strings.Contains(res.Content, "timeout_ms") {
		t.Fatalf("result: %q", res.Content)
	}
}

// runsHere reports why a tier that says it is available cannot run a command
// here, such as a CI runner that may not make a network namespace.
func runsHere(box sandbox.Sandbox, ws, why string) string {
	if why != "" {
		return why
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := box.Command(ctx, ws, "true").CombinedOutput(); err != nil {
		return fmt.Sprintf("a trial command failed: %v: %s", err, out)
	}
	return ""
}

// A command that times out ends with everything it started, even a child
// that left its group and session with setsid while its parent still ran.
func TestBashTimeoutEndsADetachedChild(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not installed")
	}
	tiers := []struct {
		name string
		box  func(ws string) (sandbox.Sandbox, string)
	}{
		{"host", func(string) (sandbox.Sandbox, string) { return nil, "" }},
		{"none", func(ws string) (sandbox.Sandbox, string) { return sandbox.NewNone(sandbox.DefaultPolicy(ws)), "" }},
		{"process", func(ws string) (sandbox.Sandbox, string) {
			s := sandbox.NewProcess(sandbox.DefaultPolicy(ws))
			_, why := s.Available()
			return s, runsHere(s, ws, why)
		}},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			// bubblewrap's pid namespace ends everything, and its pids are not the host's.
			if tier.name == "process" && runtime.GOOS == "linux" {
				t.Skip("bubblewrap ends its namespace whole")
			}
			s, dir := setup(t)
			b := Bash{}
			box, unavailable := tier.box(dir)
			if unavailable != "" {
				t.Skipf("%s sandbox unavailable: %s", tier.name, unavailable)
			}
			if box != nil {
				b.Sandbox = box.Command
			}
			pidFile := filepath.Join(dir, "detached.pid")
			// The parent stays, sleeping; its child starts a session of its own.
			script := `if (my $p = fork) { sleep 30; exit } POSIX::setsid(); open(F, ">", $ARGV[0]); print F $$; close F; sleep 30`
			args, _ := json.Marshal(bashArgs{
				Command:     `perl -MPOSIX -e '` + script + `' ` + pidFile + ` >/dev/null 2>&1; echo after`,
				Description: "a command that detaches a child", TimeoutMS: 1500,
			})
			done := make(chan Result, 1)
			go func() { done <- b.Run(context.Background(), s, args) }()
			pid := waitForPid(t, pidFile, done)
			t.Cleanup(func() {
				if alive(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			if sid, err := unix.Getsid(pid); err != nil || sid != pid {
				t.Fatalf("the child did not start a session of its own: sid %d, %v", sid, err)
			}
			select {
			case res := <-done:
				if !strings.Contains(res.Content, "timed out") {
					t.Errorf("result: %q", res.Content)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the command held the call past its timeout")
			}
			for deadline := time.Now().Add(2 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the detached child %d outlived the timeout", pid)
				}
			}
		})
	}
}

// A command that failed reaching for the network in a sandbox without one is
// told why, and pointed at the web tools, even when a pipe hid the exit code.
func TestBashNetworkHintOnlyWhenTheSandboxCutTheNetwork(t *testing.T) {
	s, dir := setup(t)
	none := sandbox.NewNone(sandbox.DefaultPolicy(dir)).Command
	off := Bash{Sandbox: none, Isolation: Isolation{Tier: "process"}}
	on := Bash{Sandbox: none, Isolation: Isolation{Tier: "process", Network: true}}
	curl := `echo "curl: (6) Could not resolve host: weather.com" >&2; exit 6`
	for _, tc := range []struct {
		name    string
		bash    Bash
		command string
		hint    bool
	}{
		{"curl, network off", off, curl, true},
		{"python, network off", off, `echo "urlopen error [Errno 8] nodename nor servname provided, or not known"; exit 1`, true},
		{"curl to an address, network off", off, `echo "curl: (7) Failed to connect to 1.1.1.1 port 80 after 1 ms"; exit 7`, true},
		{"network on", on, curl, false},
		{"on the host", Bash{}, curl, false},
		{"undescribed sandbox", Bash{Sandbox: none}, curl, false},
		{"piped, exit 0", off, `curl() { echo "curl: (6) Could not resolve host: example.com"; }; curl x | head -n 1`, true},
		{"a log read, exit 0", off, `printf 'curl: (6) Could not resolve host: x\nretrying\n'; echo done`, false},
		{"failure early, exit 0", off, `curl() { :; }; curl x; printf 'Could not resolve host: x\n1\n2\n3\n4\n5\n6\n'`, false},
		{"python piped, exit 0", off, `python3 -c 'pass' 2>/dev/null; echo "urlopen error [Errno 8] nodename nor servname provided, or not known" | tail -n 1`, true},
		{"a local server not running", off, `echo "curl: (7) Failed to connect to localhost port 8080"; exit 7`, false},
		{"an ordinary failure", off, `echo "FAIL: TestParse"; exit 1`, false},
		{"a rust build error", off, `echo "error[E0433]: failed to resolve: use of undeclared crate or module ` + "`serde`" + `"; exit 101`, false},
	} {
		res := run(t, tc.bash, s, bashArgs{Command: tc.command, Description: "x"})
		if got := strings.Contains(res.Content, "this sandbox has no network access"); got != tc.hint {
			t.Errorf("%s: hint %v, want %v:\n%s", tc.name, got, tc.hint, res.Content)
		}
	}
}

func TestBashDescriptionSaysWhetherTheNetworkIsReachable(t *testing.T) {
	off := Bash{Sandbox: func(context.Context, string, string) *exec.Cmd { return nil }, Isolation: Isolation{Tier: "container"}}
	on := off
	on.Isolation.Network = true
	if d := off.Description(); !strings.Contains(d, "no network") || !strings.Contains(d, "web_fetch") {
		t.Errorf("network off: %s", d)
	}
	if d := on.Description(); strings.Contains(d, "no network") || !strings.Contains(d, "can reach the network") {
		t.Errorf("network on: %s", d)
	}
	if d := (Bash{}).Description(); strings.Contains(d, "no network") {
		t.Errorf("on the host: %s", d)
	}
	// Under the allowlist: limited to the allowed destinations, through the proxy.
	list := off
	list.Isolation = Isolation{Tier: "process", Allowlist: true, AllowedHosts: []string{"proxy.golang.org", "*.example.com"}}
	if d := list.Description(); strings.Contains(d, "no network") || strings.Contains(d, "can reach the network") ||
		!strings.Contains(d, "limited to the destinations") || !strings.Contains(d, "proxy") ||
		!strings.Contains(d, "proxy.golang.org, *.example.com") || !strings.Contains(d, "403") {
		t.Errorf("allowlist: %s", d)
	}
	list.Isolation.AllowedHosts = nil
	if d := list.Description(); !strings.Contains(d, "No destination is allowed") {
		t.Errorf("empty allowlist: %s", d)
	}
	list.Isolation.DefaultAllow = true
	if d := list.Description(); strings.Contains(d, "No destination") || !strings.Contains(d, "unless a rule denies") {
		t.Errorf("allowlist with default allow: %s", d)
	}
}

// daemonEnv makes this test binary a daemon: run with it set to "start" it
// starts itself again in a session of its own and exits at once; that copy
// writes its pid to the file the variable ABHED_TEST_DAEMON_PID names and sleeps.
const daemonEnv = "ABHED_TEST_DAEMON"

func init() {
	switch os.Getenv(daemonEnv) {
	case "start":
		cmd := exec.Command(os.Args[0], "-test.run=^$") // #nosec G204 -- the test binary itself
		cmd.Env = append(os.Environ(), daemonEnv+"=run")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if cmd.Start() != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "run":
		_ = os.WriteFile(os.Getenv("ABHED_TEST_DAEMON_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

// A daemon, whose parent has exited and which left the command's session,
// is still ended with the command at its timeout: it carries the command's
// marker in its environment, whoever its parent is now.
func TestBashTimeoutEndsADaemon(t *testing.T) {
	for _, tier := range []string{"host", "none", "process"} {
		t.Run(tier, func(t *testing.T) {
			s, dir := setup(t)
			b := Bash{}
			switch tier {
			case "none":
				b.Sandbox = sandbox.NewNone(sandbox.DefaultPolicy(dir)).Command
			case "process":
				// bubblewrap's pid namespace ends everything, and its pids are not the host's.
				if runtime.GOOS == "linux" {
					t.Skip("bubblewrap ends its namespace whole")
				}
				box := sandbox.NewProcess(sandbox.DefaultPolicy(dir))
				_, why := box.Available()
				if why := runsHere(box, dir, why); why != "" {
					t.Skipf("process sandbox unavailable: %s", why)
				}
				b.Sandbox = box.Command
			}
			pidFile := filepath.Join(dir, "daemon.pid")
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(bashArgs{
				Command:     daemonEnv + `=start ABHED_TEST_DAEMON_PID=` + pidFile + ` '` + self + `' </dev/null >/dev/null 2>&1; sleep 30`,
				Description: "a command that starts a daemon", TimeoutMS: 1500,
			})
			done := make(chan Result, 1)
			go func() { done <- b.Run(context.Background(), s, args) }()
			pid := waitForPid(t, pidFile, done)
			t.Cleanup(func() {
				if alive(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			if sid, err := unix.Getsid(pid); err != nil || sid != pid {
				t.Fatalf("the daemon did not start a session of its own: sid %d, %v", sid, err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the command held the call past its timeout")
			}
			for deadline := time.Now().Add(2 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the daemon %d outlived the timeout", pid)
				}
			}
		})
	}
}

type recordingHost struct{ started chan ShellRequest }

func (h recordingHost) StartShell(_ context.Context, req ShellRequest) (string, error) {
	h.started <- req
	return "bg-1", nil
}

// A Ctrl-B that lands after the timeout has ended the command does not move
// it: the timeout owns it, and the call reports the timeout.
func TestCtrlBAfterTheTimeoutDoesNotMoveTheCommand(t *testing.T) {
	s, _ := setup(t)
	d := NewDetach()
	host := recordingHost{started: make(chan ShellRequest, 1)}
	b := Bash{Sandbox: func(ctx context.Context, cwd, _ string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sleep", "30")
		cmd.Dir = cwd
		// Runs after the kill and holds Wait, so only the move can be seen meanwhile.
		cmd.Cancel = func() error { d.Ask(); time.Sleep(500 * time.Millisecond); return nil }
		return cmd
	}}
	args, _ := json.Marshal(bashArgs{Command: "sleep 30", Description: "sleeps", TimeoutMS: 300})
	ctx := WithShellHost(WithDetach(context.Background(), d), host)
	res := b.Run(ctx, s, args)
	select {
	case req := <-host.started:
		t.Fatalf("a command the timeout ended was moved to the background: %+v; result %q", req.Command, res.Content)
	default:
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("result: %q", res.Content)
	}
}
