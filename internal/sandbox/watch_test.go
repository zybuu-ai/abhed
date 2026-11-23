//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests start and end real process trees, so they build only on Linux
// and run only where asked: a throwaway CI runner or a container
// (ABHED_TEST_PROCESS_KILL=1), never on a person's machine. On macOS the
// decisions are tested against fake process tables (watch_unit_test.go,
// marked_unit_test.go).

// requireKillTests skips unless ABHED_TEST_PROCESS_KILL=1.
func requireKillTests(t *testing.T) {
	t.Helper()
	if os.Getenv("ABHED_TEST_PROCESS_KILL") != "1" {
		t.Skip("set ABHED_TEST_PROCESS_KILL=1 on a throwaway runner or in a container to run it")
	}
}

// runWatched runs command as the bash tool does: its own session and marker,
// followed from its start, and waited on through the Watch.
func runWatched(t *testing.T, s Sandbox, ws, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := EndWithCommand(s.Command(ctx, ws, command))
	cmd.Dir = ws
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return StartWatch(cmd).Wait()
}

// sandboxes are the tiers a command's processes are followed on: the
// unsandboxed one, where the Watch is the only net, and the process tier when
// it can run here.
func sandboxes(t *testing.T, ws string) map[string]Sandbox {
	out := map[string]Sandbox{"none": NewNone(DefaultPolicy(ws))}
	if p := NewProcess(DefaultPolicy(ws)); func() bool { ok, _ := p.Available(); return ok }() {
		out["process"] = p
	}
	for _, s := range out {
		t.Cleanup(func() { _ = s.(interface{ Close() error }).Close() })
	}
	return out
}

// Nothing a command starts outlives it when it is proven the command's own: a
// child left behind with nohup and &, one that cleared its environment, one
// that took a session of its own while followed, and a double fork, each set
// to write a file after the command ends.
func TestNothingACommandStartsOutlivesIt(t *testing.T) {
	requireKillTests(t)
	for tier, s := range sandboxes(t, workspace(t)) {
		ws := workspace(t)
		for name, command := range map[string]string{
			"nohup":       `(nohup sh -c 'sleep 2; touch late-nohup' >/dev/null 2>&1 &)`,
			"cleared env": `(env -i /bin/sh -c 'sleep 2; touch late-env' >/dev/null 2>&1 &)`,
			// The parent stays long enough to be read, so the child is followed.
			"setsid":      `python3 -c 'import os,time,sys; pid=os.fork(); (os.setsid(), os.closerange(0,3), time.sleep(2), open("late-setsid","w")) if pid==0 else (time.sleep(0.5), sys.exit(0))' || true`,
			"double fork": `( ( sleep 2; touch late-double ) & ) ; sleep 0.2`,
		} {
			left, err := runWatched(t, s, ws, command)
			if err != nil || left != "" {
				t.Errorf("%s, %s: err %v, left %q", tier, name, err, left)
			}
		}
		// Each would have written by now.
		time.Sleep(3 * time.Second)
		entries, _ := os.ReadDir(ws)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "late-") {
				t.Errorf("%s: %s was written after its command ended", tier, e.Name())
			}
		}
	}
}

// A process that left the command's session and tree before any read saw
// it carries the marker but is not proven the command's own: it is reported,
// never ended by the marker alone. Abhed's own unrelated process carrying the
// same marker is never signalled.
func TestAnEscapedProcessIsReportedNotEnded(t *testing.T) {
	requireKillTests(t)
	s := NewNone(DefaultPolicy(workspace(t)))
	t.Cleanup(func() { _ = s.Close() })
	ws := workspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := EndWithCommand(s.Command(ctx, ws, `exec sleep 30`))
	cmd.Dir = ws
	// A process of the test's own, started outside the command, that carries
	// the command's marker: as an app that inherited it, or had it planted.
	outsider := exec.Command("sleep", "30")
	outsider.Env = append(os.Environ(), markerIn(cmd.Env))
	if err := outsider.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outsider.Process.Kill(); _ = outsider.Wait() })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := StartWatch(cmd)
	time.Sleep(100 * time.Millisecond)
	_ = cmd.Process.Kill()
	left, _ := w.Wait()
	if !strings.Contains(left, "carrying the command's marker were left running") {
		t.Errorf("the marked outsider was not reported: %q", left)
	}
	if err := outsider.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the marked outsider was signalled: %v", err)
	}
}

// A sandbox closing ends a command still running, with what it started.
func TestClosingTheSandboxEndsItsCommands(t *testing.T) {
	requireKillTests(t)
	for tier, s := range sandboxes(t, workspace(t)) {
		ws := workspace(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := EndWithCommand(s.Command(ctx, ws, `(sleep 2; touch late-close) & sleep 30`))
		cmd.Dir = ws
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		w := StartWatch(cmd)
		waited := make(chan error, 1)
		go func() { _, err := w.Wait(); waited <- err }()
		time.Sleep(300 * time.Millisecond)
		start := time.Now()
		if err := s.(interface{ Close() error }).Close(); err != nil {
			t.Errorf("%s: close: %v", tier, err)
		}
		select {
		case <-waited:
		case <-time.After(15 * time.Second):
			cancel()
			t.Fatalf("%s: the command still ran after its sandbox closed", tier)
		}
		cancel()
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: closing took %s", tier, d)
		}
		time.Sleep(3 * time.Second)
		if _, err := os.Stat(filepath.Join(ws, "late-close")); err == nil {
			t.Errorf("%s: a process the command started wrote after the sandbox closed", tier)
		}
	}
}
