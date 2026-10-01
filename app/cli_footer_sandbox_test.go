//go:build unix

package app

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// recordingSandbox runs commands with a mark on their output, and counts
// them, so a test can tell a command run under it from one on the host.
type recordingSandbox struct {
	tier sandbox.Tier
	mu   sync.Mutex
	ran  []string
}

func (s *recordingSandbox) Tier() sandbox.Tier        { return s.tier }
func (s *recordingSandbox) Available() (bool, string) { return true, "" }
func (s *recordingSandbox) Describe() string          { return "recording" }
func (s *recordingSandbox) Command(ctx context.Context, cwd, command string) *exec.Cmd {
	s.mu.Lock()
	s.ran = append(s.ran, command)
	s.mu.Unlock()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf SANDBOXED-; "+command)
	cmd.Dir = cwd
	return cmd
}

func waitLine(t *testing.T, f *footer) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		line, running := f.line, f.running
		f.mu.Unlock()
		if line != "" && !running {
			return line
		}
	}
	t.Fatal("the status line never ran")
	return ""
}

// The footer runs the status line where the session's statusline judge
// says, never on the host, and reads only the first 4 KB of its output.
func TestStatusLineRunsUnderTheSandbox(t *testing.T) {
	sb := &recordingSandbox{tier: sandbox.TierProcess}
	ready := func(cmd string) func() (sandbox.Sandbox, string, string) {
		return func() (sandbox.Sandbox, string, string) { return sb, cmd, "" }
	}
	f := &footer{editor: ui.NewLineReader(""), r: ui.NewRenderer(io.Discard, false), root: t.TempDir(), ready: ready("printf ok")}
	f.runStatusLine()
	if line := waitLine(t, f); line != "SANDBOXED-ok" {
		t.Fatalf("the status line was %q, not run under the sandbox", line)
	}
	sb.mu.Lock()
	n := len(sb.ran)
	sb.mu.Unlock()
	if n != 1 {
		t.Fatalf("the sandbox ran %d commands", n)
	}

	big := &footer{editor: ui.NewLineReader(""), r: ui.NewRenderer(io.Discard, false), root: t.TempDir(),
		ready: ready("printf '%0200000d' 0")}
	big.runStatusLine()
	if line := waitLine(t, big); len(line) > statuslineMax || !strings.HasPrefix(line, "SANDBOXED-000") {
		t.Fatalf("read %d bytes of the status line, cap %d", len(line), statuslineMax)
	}
}

// A status line the judge refuses is not run, and the footer says why.
func TestStatusLineRefusedShowsTheNotice(t *testing.T) {
	f := &footer{editor: ui.NewLineReader(""), r: ui.NewRenderer(io.Discard, false), root: t.TempDir(),
		ready: func() (sandbox.Sandbox, string, string) { return nil, "", "statusline: " + errNoProcessSandbox.Error() }}
	f.runStatusLine()
	f.mu.Lock()
	line, running := f.line, f.running
	f.mu.Unlock()
	if running || !strings.Contains(line, "process sandbox") {
		t.Fatalf("running %v, line %q", running, line)
	}
}
