//go:build linux || darwin

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireKillTests runs a test that starts and ends real processes only
// where asked: on a throwaway CI runner or in a container
// (ABHED_TEST_PROCESS_KILL=1), never by default on a person's machine.
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

// Nothing a command starts outlives it: a child left behind with nohup and
// &, one that cleared its environment, one that took a session of its own,
// and a double fork, each set to write a file after the command ends.
func TestNothingACommandStartsOutlivesIt(t *testing.T) {
	requireKillTests(t)
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	t.Cleanup(func() { _ = s.Close() })
	for name, command := range map[string]string{
		"nohup":       `(nohup sh -c 'sleep 2; touch late-nohup' >/dev/null 2>&1 &)`,
		"cleared env": `(env -i /bin/sh -c 'sleep 2; touch late-env' >/dev/null 2>&1 &)`,
		"setsid":      `python3 -c 'import os,time,sys; pid=os.fork(); (os.setsid(), os.closerange(0,3), time.sleep(2), open("late-setsid","w")) if pid==0 else sys.exit(0)' || true`,
		"double fork": `( ( sleep 2; touch late-double ) & ) ; sleep 0.2`,
	} {
		left, err := runWatched(t, s, ws, command)
		if err != nil || left != "" {
			t.Errorf("%s: err %v, left %q", name, err, left)
		}
	}
	// Each would have written by now.
	time.Sleep(3 * time.Second)
	entries, _ := os.ReadDir(ws)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "late-") {
			t.Errorf("%s was written after its command ended", e.Name())
		}
	}
}

// A sandbox closing ends a command still running, with what it started.
func TestClosingTheSandboxEndsItsCommands(t *testing.T) {
	requireKillTests(t)
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := EndWithCommand(s.Command(ctx, ws, `(sleep 2; touch late-close) & sleep 30`))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := StartWatch(cmd)
	waited := make(chan error, 1)
	go func() { _, err := w.Wait(); waited <- err }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		t.Fatal("the command still ran after its sandbox closed")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("closing took %s", d)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(filepath.Join(ws, "late-close")); err == nil {
		t.Error("a process the command started wrote after the sandbox closed")
	}
}
