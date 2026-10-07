package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Launcher builds the command that runs a stdio server, its network confined
// to an egress proxy whose decisions go to record.
type Launcher func(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error)

// errUnconfined is a stdio server the allowlist needs confined, with no sandbox to do it.
var errUnconfined = errors.New("sandbox.network is allowlist and no sandbox here can confine a stdio server's network, so it is not started")

// startStdio starts a stdio server, confined when the gateway can confine it.
func (g *Gateway) startStdio(life context.Context, cfg ServerConfig) (*StdioTransport, error) {
	env := ServerEnv(cfg.Env)
	if g.Confine == nil {
		if g.MustConfine {
			return nil, errUnconfined
		}
		return NewStdioTransport(life, cfg.Command, cfg.Args, env)
	}
	argv := append([]string{cfg.Command}, cfg.Args...)
	cmd, err := g.Confine(life, cfg.Name, argv, env, g.calls.recordFor(cfg.Name))
	if err != nil {
		return nil, fmt.Errorf("confining the server's network under sandbox.network allowlist: %w", err)
	}
	return startStdio(cmd, cfg.Command)
}

// serverCallers are the calls in flight to each server, so its proxy's
// decisions go to the record of the session that made the call.
type serverCallers struct {
	mu sync.Mutex
	m  map[string]*callers
}

type callers struct {
	inflight map[*egress.Caller]struct{}
	last     egress.Caller
}

// begin notes a call to server for c until the returned function is called.
func (s *serverCallers) begin(server string, c egress.Caller) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*callers{}
	}
	cs := s.m[server]
	if cs == nil {
		cs = &callers{inflight: map[*egress.Caller]struct{}{}}
		s.m[server] = cs
	}
	p := &c
	cs.inflight[p] = struct{}{}
	cs.last = c
	return func() {
		s.mu.Lock()
		delete(cs.inflight, p)
		s.mu.Unlock()
	}
}

// pick is who a decision of server's is recorded for: the call in flight
// (no call id if several), or the latest; two sessions at once name none.
func (s *serverCallers) pick(server string) (egress.Caller, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.m[server]
	if cs == nil {
		return egress.Caller{}, false, false
	}
	if len(cs.inflight) == 0 {
		return cs.last, false, true
	}
	var one egress.Caller
	first := true
	for c := range cs.inflight {
		switch {
		case first:
			one, first = *c, false
		case one.Session != c.Session:
			return egress.Caller{}, false, false
		case one.CallID != c.CallID:
			one.CallID = "" // two calls of the session: neither is named
		}
	}
	return one, true, true
}

// recordFor writes a decision of server's proxy to the record of its caller;
// one it cannot attribute to a single session is logged instead.
func (s *serverCallers) recordFor(server string) func(string, map[string]any) error {
	return func(event string, payload map[string]any) error {
		payload["mcp_server"] = server
		c, inCall, ok := s.pick(server)
		if !ok || c.Record == nil {
			slog.Warn("egress decision of an MCP server not attributed to one session's record", "server", server,
				"host", payload["host"], "decision", payload["decision"], "rule", payload["rule"])
			return nil
		}
		payload["session"] = c.Session
		payload["call_id"] = ""
		if inCall {
			payload["call_id"] = c.CallID
		}
		return c.Record(event, payload)
	}
}
