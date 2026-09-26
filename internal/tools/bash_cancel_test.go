//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
