package app

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// launchLog keeps the Launch each command was built with.
type launchLog struct {
	mu  sync.Mutex
	got []sandbox.Launch
}

func (l *launchLog) add(ctx context.Context) {
	l.mu.Lock()
	l.got = append(l.got, sandbox.LaunchOf(ctx))
	l.mu.Unlock()
}

func (l *launchLog) wait(t *testing.T, n int) []sandbox.Launch {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		l.mu.Lock()
		got := append([]sandbox.Launch(nil), l.got...)
		l.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The Studio terminal's commands, a line in lines mode and the interactive
// shell, are the session's own calls: each is built with a Launch naming
// the session, the call and the session's record, so the egress proxy
// records their decisions there rather than dropping them.
func TestStudioTerminalCommandsAreBoundToTheirSession(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	s := r.cl.conn.session(id)
	tool, ok := s.parts.Loop.Tools.Get("bash")
	b, isBash := tool.(tools.Bash)
	if !ok || !isBash || b.Sandbox == nil {
		t.Skip("this session's bash has no sandbox to watch")
	}
	var seen launchLog
	inner, innerShell := b.Sandbox, b.Shell
	b.Sandbox = func(ctx context.Context, cwd, command string) *exec.Cmd {
		seen.add(ctx)
		return inner(ctx, cwd, command)
	}
	if innerShell != nil {
		b.Shell = func(ctx context.Context, cwd string) *exec.Cmd {
			seen.add(ctx)
			return innerShell(ctx, cwd)
		}
	}
	s.parts.Loop.Tools.Add(b)

	var term struct {
		TerminalID string `json:"terminalId"`
	}
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "echo bound\n"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "bound\r\n")
	want := 1
	if innerShell != nil {
		var sh struct {
			TerminalID string `json:"terminalId"`
		}
		r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "interactive", "cols": 80, "rows": 24}, &sh)
		want = 2
	}
	got := seen.wait(t, want)
	if len(got) != want {
		t.Fatalf("launches: %+v, want %d", got, want)
	}
	root := s.parts.Loop.Recorder.Root()
	for i, l := range got {
		if l.Session == "" || l.Session != root || l.CallID == "" || l.Record == nil {
			t.Fatalf("launch %d is not bound to session %s: %+v", i, root, l)
		}
		// What the sandbox writes through the launch lands in this session's record.
		if err := l.Record(sandbox.EvEgressDecision, map[string]any{"call_id": l.CallID, "host": "marker.test"}); err != nil {
			t.Fatal(err)
		}
	}
	decisions, _ := r.recorded(id, agent.EvEgressDecision)
	if len(decisions) != want {
		t.Fatalf("the terminal's egress decisions in its record: %v", decisions)
	}
}
