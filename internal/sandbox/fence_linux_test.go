package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/internal/fence/cgroup"
)

// The tests below fence real commands. They need Linux 6.7 or later, an
// ordinary user and a delegated cgroup, and run only with
// ABHED_REQUIRE_FENCE=1, which also turns a host that does not qualify into
// a failure instead of a skip:
//
//	systemd-run --user --scope -p Delegate=yes env ABHED_REQUIRE_FENCE=1 go test ./internal/sandbox -run Fence

// fenced selects the fence for a fresh workspace and home, or skips.
func fenced(t *testing.T, mutate func(*Policy)) (*Fence, Policy) {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	p := fencePolicy(t)
	p.MaxProcs = 64
	p.MaxMemoryMB = 256
	if mutate != nil {
		mutate(&p)
	}
	sb, err := Select(p)
	if err != nil {
		t.Fatalf("the fence did not qualify: %v", err)
	}
	f := sb.(*Fence)
	t.Cleanup(func() {
		tmp, cg := f.tmp, f.host.path()
		if err := Close(f); err != nil {
			t.Errorf("Close: %v", err)
		}
		for _, p := range []string{tmp, cg} {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s outlived Close", p)
			}
		}
	})
	return f, p
}

// noMounts fences without a mount namespace, for the checks that mode relies on.
func noMounts(p *Policy) { p.fenceNoMounts = true }

// events collects what the fence records for one command.
type events struct {
	mu  sync.Mutex
	got []string
	pay []map[string]any
}

func (e *events) record(before func()) func(string, map[string]any) error {
	return func(ev string, p map[string]any) error {
		if before != nil && ev == "process.launched" {
			before()
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.got, e.pay = append(e.got, ev), append(e.pay, p)
		return nil
	}
}

func (e *events) of(ev string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for i, g := range e.got {
		if g == ev {
			out = append(out, e.pay[i])
		}
	}
	return out
}

func fenceRun(t *testing.T, f *Fence, cwd, command string, rec func(string, map[string]any) error, call string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctx = WithLaunch(ctx, Launch{CallID: call, Record: rec})
	out, err := f.Command(ctx, cwd, command).CombinedOutput()
	return string(out), err
}

// The workspace is writable, and the launch is recorded before the command
// runs: when process.launched is written, the command has not yet run.
func TestFenceWritesTheWorkspaceAfterTheLaunchIsRecorded(t *testing.T) {
	f, p := fenced(t, nil)
	made := filepath.Join(p.Workspace, "made.txt")
	ranEarly := false
	ev := &events{}
	out, err := fenceRun(t, f, p.Workspace, "echo hi > made.txt && cat made.txt", ev.record(func() {
		_, serr := os.Stat(made)
		ranEarly = serr == nil
	}), "call-write")
	if err != nil || strings.TrimSpace(out) != "hi" {
		t.Fatalf("workspace write: %v\n%s", err, out)
	}
	if ranEarly {
		t.Fatal("the command ran before its launch was recorded")
	}
	l := ev.of("process.launched")
	if len(l) != 1 || l[0]["call_id"] != "call-write" || l[0]["seccomp"] != "command/3" || l[0]["source"] != "call" {
		t.Fatalf("process.launched: %v", l)
	}
}

// A launch that cannot be recorded does not run.
func TestFenceRunsNothingUnrecorded(t *testing.T) {
	f, p := fenced(t, nil)
	out, err := fenceRun(t, f, p.Workspace, "touch ran", func(string, map[string]any) error { return errors.New("record down") }, "call-x")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 126 {
		t.Fatalf("err %v, want exit 126:\n%s", err, out)
	}
	if _, serr := os.Stat(filepath.Join(p.Workspace, "ran")); serr == nil {
		t.Fatal("the command ran without its launch recorded")
	}
}

// Abhed's state, a write outside the workspace and the host's files are out
// of reach.
func TestFenceDeniesStateAndTheRestOfHome(t *testing.T) {
	f, p := fenced(t, nil)
	home, _ := os.UserHomeDir()
	cfg := filepath.Join(home, ".abhed", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"secret":"abhed-state"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"cat " + cfg, "ls " + filepath.Dir(cfg), "echo x > " + filepath.Join(home, "planted"), "echo x > /tmp/abhed-fence-escape-test"} {
		out, err := fenceRun(t, f, p.Workspace, c, nil, "")
		if err == nil || strings.Contains(out, "abhed-state") {
			t.Errorf("%q was allowed:\n%s", c, out)
		}
	}
	_ = os.Remove("/tmp/abhed-fence-escape-test")
}

// With the network off, a connect to loopback is refused at socket().
func TestFenceNetworkOffRefusesLoopback(t *testing.T) {
	f, p := fenced(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	out, err := fenceRun(t, f, p.Workspace, fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d && echo connected", port), nil, "")
	if err == nil || strings.Contains(out, "connected") {
		t.Fatalf("loopback reached with the network off:\n%s", out)
	}
	select {
	case <-accepted:
		t.Fatal("the listener accepted a connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// A fork bomb is held at pids.max, recorded as a limit, and what it left is
// ended with the command.
func TestFenceHoldsAForkBomb(t *testing.T) {
	f, p := fenced(t, nil)
	ev := &events{}
	start := time.Now()
	_, _ = fenceRun(t, f, p.Workspace, `b(){ b|b& }; b; sleep 3`, ev.record(nil), "call-bomb")
	if time.Since(start) > 50*time.Second {
		t.Fatal("the bomb ran on")
	}
	deadline := time.Now().Add(15 * time.Second)
	for len(ev.of("fence.limit")) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	lim := ev.of("fence.limit")
	if len(lim) != 1 || lim[0]["pids_max"].(uint64) == 0 {
		t.Fatalf("fence.limit: %v", lim)
	}
	for _, l := range ev.of("process.launched") {
		cg, _ := l["cgroup"].(string)
		gone := time.Now().Add(15 * time.Second)
		for _, err := os.Stat(cg); err == nil && time.Now().Before(gone); _, err = os.Stat(cg) {
			time.Sleep(100 * time.Millisecond)
		}
		if _, err := os.Stat(cg); cg == "" || err == nil {
			t.Errorf("the call's cgroup %q outlived it", cg)
		}
	}
}

// With the network on, an inet socket is allowed, a unix socket still not.
func TestFenceNetworkOnKeepsUnixSocketsOut(t *testing.T) {
	f, p := fenced(t, func(p *Policy) { p.AllowNetwork = true })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if out, err := fenceRun(t, f, p.Workspace, fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d && echo connected", port), nil, ""); err != nil || !strings.Contains(out, "connected") {
		t.Fatalf("loopback refused with the network on: %v\n%s", err, out)
	}
	sock := filepath.Join(t.TempDir(), "s")
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ul.Close() }()
	if out, err := fenceRun(t, f, p.Workspace, "python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); print(\"connected\")' "+sock, nil, ""); err == nil || strings.Contains(out, "connected") {
		t.Fatalf("a unix socket was reachable:\n%s", out)
	}
}

// Without a PID namespace a command sees other processes' command lines,
// but Landlock keeps their environment out of its reach, as the docs say.
func TestFenceProcVisibility(t *testing.T) {
	f, p := fenced(t, nil)
	pid := os.Getpid()
	if out, err := fenceRun(t, f, p.Workspace, fmt.Sprintf("tr '\\0' ' ' < /proc/%d/cmdline", pid), nil, ""); err != nil || !strings.Contains(out, "test") {
		t.Errorf("cmdline: %v\n%s", err, out)
	}
	if out, err := fenceRun(t, f, p.Workspace, fmt.Sprintf("cat /proc/%d/environ", pid), nil, ""); err == nil {
		t.Errorf("another process's environment was readable:\n%s", out)
	}
}

// eventually waits up to d for ok.
func eventually(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A command that plants an account in the workspace's .abhed, in any case,
// leaves nothing there for a later Abhed to read: it is moved out once the
// command ends, recorded, and the session's fence runs nothing more. One
// left running in the background to plant later is ended with its command.
func TestFencePlantedStateDoesNotPersist(t *testing.T) {
	for _, plant := range []string{
		`mkdir .abhed && echo '{"users":[{"name":"mallory","role":"admin"}]}' > .abhed/users.json`,
		`mkdir .ABHED && echo '{"users":[]}' > .ABHED/users.json`,
		`(sleep 1; mkdir .Abhed; echo '{}' > .Abhed/users.json) & echo started`,
	} {
		f, p := fenced(t, noMounts)
		home, _ := os.UserHomeDir()
		ev := &events{}
		out, err := fenceRun(t, f, p.Workspace, plant, ev.record(nil), "call-plant")
		t.Logf("%s: %v %s", plant, err, out)
		users := filepath.Join(p.Workspace, ".abhed", "users.json")
		// The check runs once the call's cgroup is gone, and again before the next command.
		next, nerr := fenceRun(t, f, p.Workspace, "echo ran > after", ev.record(nil), "call-next")
		time.Sleep(1500 * time.Millisecond)
		_ = f.checkPlanted(nil, "", "test")
		if left, _ := stateEntries(p.Workspace); len(left) != 0 {
			t.Fatalf("%s: left in the workspace: %v", plant, left)
		}
		if _, err := os.Stat(users); err == nil {
			t.Fatalf("%s: %s persists", plant, users)
		}
		if strings.HasPrefix(plant, "(sleep") {
			// The background planter was ended with its command: nothing came later.
			if f.planted.Load() != nil || nerr != nil {
				t.Fatalf("a planter that should have been ended planted: %v %s", nerr, next)
			}
			continue
		}
		if nerr == nil {
			t.Fatalf("%s: the next command ran: %s", plant, next)
		}
		if _, err := os.Stat(filepath.Join(p.Workspace, "after")); err == nil {
			t.Fatal("the refused command wrote")
		}
		got := ev.of("fence.state_planted")
		if len(got) == 0 {
			t.Fatalf("%s: no fence.state_planted", plant)
		}
		entries, _ := got[0]["entries"].([]map[string]any)
		if len(entries) != 1 {
			t.Fatalf("entries %v", got[0])
		}
		dest, _ := entries[0]["moved_to"].(string)
		if !strings.HasPrefix(dest, filepath.Join(home, ".abhed", "quarantine")) {
			t.Fatalf("moved to %q", dest)
		}
	}
}

// State that appears beside the session is found at Close, moved out and
// recorded to the session's record.
func TestFenceCloseQuarantinesState(t *testing.T) {
	_, p := fenced(t, noMounts)
	f := NewFence(p)
	if ok, why := f.Available(); !ok {
		t.Fatal(why)
	}
	ev := &events{}
	f.SetRecord(ev.record(nil))
	if err := os.MkdirAll(filepath.Join(p.Workspace, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err == nil || !strings.Contains(err.Error(), ".abhed") {
		t.Fatalf("Close: %v", err)
	}
	if left, _ := stateEntries(p.Workspace); len(left) != 0 || len(ev.of("fence.state_planted")) != 1 {
		t.Fatalf("not quarantined at Close: %v", ev.got)
	}
}

// fencedApart is a fence for p of its own, apart from the one fenced closes
// at cleanup, for a test that expects its Close to fail.
func fencedApart(t *testing.T, p Policy) (*Fence, *events) {
	t.Helper()
	f := NewFence(p)
	if ok, why := f.Available(); !ok {
		t.Fatal(why)
	}
	ev := &events{}
	f.SetRecord(ev.record(nil))
	return f, ev
}

// A command that makes the workspace unlistable, here by its mode, has
// planted state as far as the fence can tell: a .abhed is still found by
// name, the next command is refused, fence.state_planted names the reason,
// and Close fails.
func TestFenceUnlistableWorkspaceRefusesTheSession(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		plant bool
		left  bool
	}{
		{"0100", false, false},
		{"0100", true, true},
		{"0300", true, false},
		// Listed, but no entry in it can be looked up.
		{"0600", false, false},
		{"0600", true, true},
		{"0400", true, true},
	} {
		_, p := fenced(t, noMounts)
		t.Cleanup(func() { _ = os.Chmod(p.Workspace, 0o700) })
		f, ev := fencedApart(t, p)
		cmd := "chmod " + tc.mode + " ."
		if tc.plant {
			cmd = `mkdir .abhed && echo '{"users":[{"name":"mallory","role":"admin"}]}' > .abhed/users.json && ` + cmd
		}
		if out, err := fenceRun(t, f, p.Workspace, cmd, nil, "call-hide"); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		if out, err := fenceRun(t, f, p.Workspace, "echo ran", nil, "call-next"); err == nil || strings.Contains(out, "ran") {
			t.Fatalf("%s: the next command ran:\n%s", cmd, out)
		}
		cerr := f.Close()
		if cerr == nil || !strings.Contains(cerr.Error(), "cannot be listed") {
			t.Fatalf("%s: Close: %v", cmd, cerr)
		}
		got := ev.of(EvFenceStatePlanted)
		if len(got) == 0 || got[0]["listing_error"] == nil || got[0]["still_present"] != true {
			t.Fatalf("%s: events %v", cmd, ev.got)
		}
		_ = os.Chmod(p.Workspace, 0o700)
		left, _ := stateEntries(p.Workspace)
		if tc.left != (len(left) == 1) {
			t.Fatalf("%s: left %v", cmd, left)
		}
		if tc.left && !strings.Contains(cerr.Error(), "still in the workspace") {
			t.Fatalf("%s: Close does not say it is still there: %v", cmd, cerr)
		}
		for _, l := range left {
			_ = os.RemoveAll(l)
		}
	}
}

// A .abhed a command locks (mode 0100) in a workspace on another filesystem
// than home, where it cannot be moved to the quarantine, is renamed in place
// to a name Abhed never reads, still readable by its owner, and said so.
func TestFencePlantedStateOnAnotherFilesystem(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	ws, err := os.MkdirTemp("/dev/shm", "abhed-fence-ws-")
	if err != nil {
		t.Skipf("no tmpfs at /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })
	f, p := fenced(t, func(p *Policy) { p.Workspace, p.fenceNoMounts = ws, true })
	var wst, hst unix.Stat_t
	home, _ := os.UserHomeDir()
	if unix.Stat(ws, &wst) != nil || unix.Stat(home, &hst) != nil || wst.Dev == hst.Dev {
		t.Skip("/dev/shm is on home's filesystem here")
	}
	ev := &events{}
	plant := `mkdir .abhed && echo '{"users":[{"name":"mallory","role":"admin"}]}' > .abhed/users.json && chmod 0100 .abhed`
	if out, err := fenceRun(t, f, p.Workspace, plant, ev.record(nil), "call-plant"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := fenceRun(t, f, p.Workspace, "echo ran", ev.record(nil), "call-next"); err == nil || !strings.Contains(err.Error()+out, "renamed in place") {
		t.Fatalf("the next command: %v\n%s", err, out)
	}
	if left, lerr := stateEntries(p.Workspace); len(left) != 0 || lerr != nil {
		t.Fatalf("left %v %v", left, lerr)
	}
	got := ev.of(EvFenceStatePlanted)
	if len(got) == 0 || got[0]["still_present"] != false {
		t.Fatalf("events %v", ev.got)
	}
	entries, _ := got[0]["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["outcome"] != plantRenamed {
		t.Fatalf("entries %v", entries)
	}
	inert, _ := entries[0]["renamed_to"].(string)
	if filepath.Dir(inert) != p.Workspace || !strings.HasPrefix(filepath.Base(inert), plantedPrefix) {
		t.Fatalf("renamed to %q", inert)
	}
	if b, err := os.ReadFile(filepath.Join(inert, "users.json")); err != nil || !strings.Contains(string(b), "mallory") {
		t.Fatalf("the renamed copy: %v %s", err, b)
	}
}

// A command started with no session of its own, as doctor's are, is put in
// one by the launcher, so /dev/tty reaches no terminal of Abhed's.
func TestFenceCommandsHaveTheirOwnSession(t *testing.T) {
	f, p := fenced(t, nil)
	out, err := fenceRun(t, f, p.Workspace, `read -r _ _ _ _ _ sid _ < /proc/$$/stat; echo "sid=$sid pid=$$"; exec 3<>/dev/tty && echo tty-opened`, nil, "")
	if err != nil && !strings.Contains(out, "sid=") {
		t.Fatalf("%v\n%s", err, out)
	}
	var sid, pid int
	if _, serr := fmt.Sscanf(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), "sid=%d pid=%d", &sid, &pid); serr != nil {
		t.Fatalf("%v\n%s", serr, out)
	}
	if mine, _ := unix.Getsid(0); sid != pid || sid == mine {
		t.Fatalf("the command's session %d, its pid %d, Abhed's session %d", sid, pid, mine)
	}
	if strings.Contains(out, "tty-opened") {
		t.Fatalf("/dev/tty opened a terminal:\n%s", out)
	}
}

// A command cannot open another terminal of the user's, here a stand-in pty
// the test owns, nor make one; it keeps its own output through its
// descriptors.
func TestFenceCannotOpenAnotherTerminal(t *testing.T) {
	f, p := fenced(t, nil)
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty here: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	pts := "/dev/pts/" + strconv.Itoa(n)
	// The stand-in is open to this user outside the fence.
	if c, err := os.OpenFile(pts, os.O_RDWR|unix.O_NOCTTY, 0); err != nil {
		t.Fatalf("the stand-in pty cannot be opened unfenced: %v", err)
	} else {
		_ = c.Close()
	}
	for _, c := range []string{"exec 3<>" + pts + " && echo opened", "echo spoofed > " + pts + " && echo opened",
		"exec 3<>/dev/ptmx && echo opened", "ls /dev/pts/ && echo opened"} {
		out, err := fenceRun(t, f, p.Workspace, c, nil, "")
		if err == nil || strings.Contains(out, "opened") {
			t.Errorf("%q reached a terminal:\n%s", c, out)
		}
	}
	if out, err := fenceRun(t, f, p.Workspace, "echo still-writes; echo to-stderr >&2", nil, ""); err != nil ||
		!strings.Contains(out, "still-writes") || !strings.Contains(out, "to-stderr") {
		t.Fatalf("stdout and stderr: %v\n%s", err, out)
	}
}

// A command cannot signal Abhed, here this test process, by its process id
// or its group, nor every process at once.
func TestFenceRefusesSignalsToAbhed(t *testing.T) {
	f, p := fenced(t, nil)
	ev := &events{}
	pid, group := os.Getpid(), unix.Getpgrp()
	for _, c := range []string{fmt.Sprintf("kill -0 %d", pid), fmt.Sprintf("kill -s CONT %d", pid),
		fmt.Sprintf("kill -0 -- -%d", group), "kill -0 -- -1"} {
		out, err := fenceRun(t, f, p.Workspace, c+" && echo signalled", ev.record(nil), "call-sig")
		if err == nil || strings.Contains(out, "signalled") {
			t.Errorf("%q reached Abhed:\n%s", c, out)
		}
	}
	// Its own processes it still signals, its own group included.
	if out, err := fenceRun(t, f, p.Workspace, "sleep 30 & kill $! && wait $!; kill -0 $$ && kill -0 0 && echo own", nil, ""); !strings.Contains(out, "own") {
		t.Errorf("own processes: %v\n%s", err, out)
	}
	for _, l := range ev.of("process.launched") {
		if l["seccomp"] != "command/3" {
			t.Errorf("profile %v", l["seccomp"])
		}
	}
}

// A launch whose caller passes no record, such as the harness's own, is
// recorded to the session's record as the harness's.
func TestFenceRecordsHarnessLaunches(t *testing.T) {
	f, p := fenced(t, nil)
	ev := &events{}
	f.SetRecord(ev.record(nil))
	if out, err := fenceRun(t, f, p.Workspace, "true", nil, ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	l := ev.of("process.launched")
	if len(l) != 1 || l[0]["source"] != "harness" {
		t.Fatalf("process.launched %v", l)
	}
}

// A command released unstarted gives back its call's cgroup at once, not
// after the launcher's two-minute wait.
func TestFenceReleaseFreesAnUnstartedCommand(t *testing.T) {
	f, p := fenced(t, nil)
	leaves := func() []string {
		m, _ := filepath.Glob(filepath.Join(f.host.path(), "call-*"))
		return m
	}
	cmd := f.Command(context.Background(), p.Workspace, "true")
	if len(leaves()) != 1 {
		t.Fatalf("leaves %v", leaves())
	}
	Release(cmd)
	eventually(t, "the leaf removed", 3*time.Second, func() bool { return len(leaves()) == 0 })
	if _, ok := pendingLaunches.Load(cmd); ok {
		t.Fatal("still pending")
	}
	// A command that fails to start is released by its caller the same way.
	cmd = f.Command(context.Background(), filepath.Join(p.Workspace, "no-such-dir"), "true")
	if err := cmd.Start(); err == nil {
		_ = cmd.Wait()
		t.Fatal("started in a missing folder")
	}
	Release(cmd)
	eventually(t, "the failed start's leaf removed", 3*time.Second, func() bool { return len(leaves()) == 0 })
}

// The cgroup a run left without closing its fence is swept when the next
// fence qualifies, once its Abhed process is gone; a live one's is kept.
func TestFenceSweepsStaleCgroups(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	base, err := cgroup.Discover()
	if err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	stale, err := base.NewSandbox(fmt.Sprintf("%d-stale", gone.Process.Pid), cgroup.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	live, err := base.NewSandbox(fmt.Sprintf("%d-live", os.Getppid()), cgroup.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Remove(context.Background()) }()
	fenced(t, nil)
	if _, err := os.Stat(stale.Path()); err == nil {
		_ = stale.Remove(context.Background())
		t.Fatal("the stale cgroup was not swept")
	}
	if _, err := os.Stat(live.Path()); err != nil {
		t.Fatalf("a live run's cgroup was swept: %v", err)
	}
}

// Giving a planted folder back to its owner never follows a link: not one
// planted in its place, not one inside it, and not one swapped in for it
// while it is walked. The folder outside keeps its mode throughout.
func TestFenceRestoreOwnerAccessFollowsNoLink(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	ws, outside := t.TempDir(), t.TempDir()
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	unchanged := func(when string) {
		t.Helper()
		if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o500 {
			t.Fatalf("%s: the folder outside changed: %v %v", when, info.Mode(), err)
		}
	}
	target := filepath.Join(ws, plantedPrefix+"x")
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	restoreOwnerAccess(target)
	unchanged("a link planted in its place")
	_ = os.Remove(target)
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o100); err != nil {
		t.Fatal(err)
	}
	restoreOwnerAccess(target)
	unchanged("a link inside it")
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the planted folder was not given back: %v %v", info.Mode(), err)
	}
	if err := os.Remove(filepath.Join(target, "in")); err != nil {
		t.Fatal(err)
	}
	// The planted folder and a link to outside swap names, over and over,
	// while it is walked and locked again.
	link := filepath.Join(ws, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = unix.Renameat2(unix.AT_FDCWD, target, unix.AT_FDCWD, link, unix.RENAME_EXCHANGE)
		}
	}()
	defer func() { close(stop); <-done }()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
		for _, n := range []string{target, link} {
			_ = unix.Fchmodat(unix.AT_FDCWD, n, 0o100, unix.AT_SYMLINK_NOFOLLOW)
		}
		restoreOwnerAccess(target)
		unchanged("a link swapped in while walked")
	}
}

// A .abhed found while another of the session's commands still runs ends
// that command too: the session's cgroup is killed, and the event says so.
func TestFencePlantedEndsRunningCommands(t *testing.T) {
	f, p := fenced(t, noMounts)
	ev := &events{}
	ctx := WithLaunch(context.Background(), Launch{CallID: "call-planter", Record: ev.record(nil)})
	planter := f.Command(ctx, p.Workspace, "sleep 1; mkdir .abhed && echo '{}' > .abhed/users.json; sleep 120")
	if err := planter.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- planter.Wait() }()
	// Another command ends once the planter's .abhed is there, and the
	// check after it finds it.
	wait := "for i in $(seq 300); do [ -e .abhed ] && exit 0; sleep 0.1; done; exit 1"
	if out, err := fenceRun(t, f, p.Workspace, wait, ev.record(nil), "call-other"); err != nil {
		_ = planter.Process.Kill()
		t.Fatalf("%v\n%s", err, out)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("the planter ended on its own")
		}
	case <-time.After(30 * time.Second):
		_ = planter.Process.Kill()
		t.Fatal("the planter still runs after the session was marked planted")
	}
	// The session is killed first, then the event is written.
	eventually(t, "fence.state_planted recorded", 10*time.Second, func() bool { return len(ev.of(EvFenceStatePlanted)) > 0 })
	got := ev.of(EvFenceStatePlanted)
	if got[0]["session_killed"] != true || got[0]["call_id"] != "call-other" {
		t.Fatalf("events %v", got)
	}
	if out, err := fenceRun(t, f, p.Workspace, "echo ran", ev.record(nil), "call-next"); err == nil || strings.Contains(out, "ran") {
		t.Fatalf("the next command ran:\n%s", out)
	}
}
