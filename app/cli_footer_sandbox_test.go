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

// The status line command runs under the session's sandbox — never on the
// host — and only its first 4 KB are read.
func TestStatusLineRunsUnderTheSandbox(t *testing.T) {
	sb := &recordingSandbox{tier: sandbox.TierProcess}
	run, under, notice := statusLineSetup("printf ok", sb)
	if notice != "" || under != sb {
		t.Fatalf("setup refused a sandboxed command: %q", notice)
	}
	f := &footer{editor: ui.NewLineReader(""), r: ui.NewRenderer(io.Discard, false), root: t.TempDir(), command: run, sb: under}
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
		command: "printf '%0200000d' 0", sb: sb}
	big.runStatusLine()
	if line := waitLine(t, big); len(line) > statusLineMax || !strings.HasPrefix(line, "SANDBOXED-000") {
		t.Fatalf("read %d bytes of the status line, cap %d", len(line), statusLineMax)
	}
}

// Without a sandbox the status line is not run, and the footer says why.
func TestStatusLineRefusedWithoutASandbox(t *testing.T) {
	for _, sb := range []sandbox.Sandbox{nil, &recordingSandbox{tier: sandbox.TierNone}} {
		run, under, notice := statusLineSetup("printf ok", sb)
		if run != "" || under != nil || !strings.Contains(notice, "runs only under a sandbox") {
			t.Fatalf("with %v: run %q, notice %q", sb, run, notice)
		}
		if r, ok := sb.(*recordingSandbox); ok && len(r.ran) != 0 {
			t.Fatal("a command ran")
		}
	}
}
