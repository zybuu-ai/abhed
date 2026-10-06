package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// launches keeps the Launch each command was built with.
type launches struct {
	mu  sync.Mutex
	got []sandbox.Launch
}

func (l *launches) add(ctx context.Context) {
	l.mu.Lock()
	l.got = append(l.got, sandbox.LaunchOf(ctx))
	l.mu.Unlock()
}

func (l *launches) all() []sandbox.Launch {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sandbox.Launch(nil), l.got...)
}

// The terminal and a ! command are the session's own calls: each is built
// with a Launch naming the session, its own call id and the session's
// record, so its egress decisions land there and never in another
// session's. Deleting the session tells the sandbox it has ended.
func TestTerminalCommandsAreBoundToTheirSession(t *testing.T) {
	var seen launches
	var ended []string
	var endMu sync.Mutex
	wb := shellBenchOpts(t, nil, func(o *Options) {
		sb := sandbox.NewNone(sandbox.DefaultPolicy(o.Workspace))
		bash := tools.Bash{
			Sandbox: func(ctx context.Context, cwd, command string) *exec.Cmd {
				seen.add(ctx)
				return sb.Command(ctx, cwd, command)
			},
			Shell: func(ctx context.Context, cwd string) *exec.Cmd {
				seen.add(ctx)
				return sb.Shell(ctx, cwd)
			},
			Isolation: tools.Isolation{Tier: "none", Backend: sb.Backend()},
		}
		o.Registry = tools.NewRegistry(tools.Read{}, tools.Write{}, bash)
		o.SessionEnded = func(id string) {
			endMu.Lock()
			ended = append(ended, id)
			endMu.Unlock()
		}
	})

	shell := wb.startShell()
	if rec := wb.send("acme", "POST", "pty", ptyStartRequest{Command: "echo hi", Cols: 80, Rows: 24}); rec.Code != http.StatusOK {
		t.Fatalf("! command: %d %s", rec.Code, rec.Body)
	}
	got := seen.all()
	if len(got) != 2 {
		t.Fatalf("launches: %+v", got)
	}
	for i, l := range got {
		if l.Session != wb.session || l.CallID == "" || l.Record == nil {
			t.Fatalf("launch %d is not bound to session %s: %+v", i, wb.session, l)
		}
	}
	if got[0].CallID != shell.ID {
		t.Fatalf("the terminal's launch is call %q, its id %q", got[0].CallID, shell.ID)
	}
	// What the sandbox writes through the launch lands in this session's record.
	if err := got[0].Record(sandbox.EvEgressDecision, map[string]any{"call_id": got[0].CallID, "host": "marker.test"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range wb.events() {
		if string(ev.Type) == sandbox.EvEgressDecision {
			found = true
		}
	}
	if !found {
		t.Fatal("the terminal's egress decision is not in its session's record")
	}

	del := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/v1/sessions/"+wb.session, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(del, req)
	if rec := del; rec.Code >= 300 && rec.Code != http.StatusNotImplemented {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		endMu.Lock()
		n := len(ended)
		endMu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	endMu.Lock()
	defer endMu.Unlock()
	if len(ended) != 1 || ended[0] != wb.session {
		t.Fatalf("sessions ended: %v", ended)
	}
}

// A session taken by another node leaves this process's sandbox too: the
// fence tells it the session has ended, once.
func TestFenceEndsTheSessionInTheSandbox(t *testing.T) {
	var ended []string
	var endMu sync.Mutex
	wb := shellBenchOpts(t, nil, func(o *Options) {
		o.SessionEnded = func(id string) {
			endMu.Lock()
			ended = append(ended, id)
			endMu.Unlock()
		}
	})
	wb.s.mu.Lock()
	live := wb.s.running[wb.session]
	wb.s.mu.Unlock()
	if live == nil {
		t.Fatal("no live session")
	}
	wb.s.fence(live)
	wb.s.fence(live)
	endMu.Lock()
	defer endMu.Unlock()
	if len(ended) != 1 || ended[0] != wb.session {
		t.Fatalf("sessions ended: %v", ended)
	}
}
