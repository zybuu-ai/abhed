package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Launcher builds the command that runs a stdio server, its network confined
// to an egress proxy whose decisions go to record.
type Launcher func(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error)

// ErrUnconfined is a stdio server that must be confined, with no sandbox here
// to do it; the error wrapping it says why.
var ErrUnconfined = errors.New("this stdio server cannot be confined here, so it is not started")

// startStdio starts a stdio server, confined when the gateway can confine it.
func (g *Gateway) startStdio(life context.Context, cfg ServerConfig) (*StdioTransport, error) {
	env := ServerEnv(cfg.Env)
	command, err := pinnedCommand(cfg, env)
	if err != nil {
		return nil, err
	}
	launch := g.Confine
	if cfg.HostFiles {
		// Its files as the host's, by the operator's choice; its network is
		// still confined under the allowlist.
		launch = g.ConfineNet
	}
	if launch == nil {
		if g.MustConfine {
			return nil, fmt.Errorf("%w: sandbox.network is allowlist and no sandbox here can confine its network", ErrUnconfined)
		}
		return NewStdioTransport(life, command, cfg.Args, env)
	}
	argv := append([]string{command}, cfg.Args...)
	// The server's own life: ending it revokes its credential and lets its proxy go.
	sctx, cancel := context.WithCancel(life)
	cmd, err := launch(sctx, cfg.Name, argv, env, g.calls.recordFor(cfg.Name))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("confining the server in the sandbox: %w", err)
	}
	var cleanup func()
	if g.Cleanup != nil {
		cleanup = g.Cleanup(cmd)
	}
	t, err := startStdio(cmd, cfg.Command)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		cancel()
		return nil, err
	}
	t.cancel, t.cleanup = cancel, cleanup
	return t, nil
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
	// early are decisions made before any session called the server, kept
	// (the latest maxEarly) for each session's record when it first calls.
	early []earlyEvent
	given map[string]bool // sessions given early, by id
}

type earlyEvent struct {
	event   string
	payload map[string]any
}

// maxEarly bounds the decisions kept from before any call.
const maxEarly = 64

// begin notes a call to server for c until the returned function is called,
// and gives c's session the server's decisions from before any call, once.
func (s *serverCallers) begin(server string, c egress.Caller) func() {
	s.mu.Lock()
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
	var early []earlyEvent
	if c.Record != nil && c.Session != "" && len(cs.early) > 0 && !cs.given[c.Session] {
		if cs.given == nil || len(cs.given) >= 4096 {
			cs.given = map[string]bool{}
		}
		cs.given[c.Session] = true
		early = cs.early
	}
	s.mu.Unlock()
	for _, e := range early {
		m := make(map[string]any, len(e.payload)+3)
		for k, v := range e.payload {
			m[k] = v
		}
		m["session"], m["call_id"], m["before_call"] = c.Session, "", true
		_ = c.Record(e.event, m)
	}
	return func() {
		s.mu.Lock()
		delete(cs.inflight, p)
		s.mu.Unlock()
	}
}

// pick is who a decision of server's is recorded for: the call in flight
// (no call id if several of one session), or the latest. With calls of
// several sessions in flight it is each of them, no call named. None, before
// any call, keeps the decision for the first call of each session.
func (s *serverCallers) pick(server string, event string, payload map[string]any) (out []egress.Caller, inCall bool) {
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
	if len(cs.inflight) == 0 {
		if cs.last.Record == nil {
			cs.early = append(cs.early, earlyEvent{event, payload})
			if len(cs.early) > maxEarly {
				cs.early = cs.early[1:]
			}
			return nil, false
		}
		return []egress.Caller{cs.last}, false
	}
	bySession := map[string]egress.Caller{}
	var order []string
	for c := range cs.inflight {
		one, seen := bySession[c.Session]
		switch {
		case !seen:
			bySession[c.Session] = *c
			order = append(order, c.Session)
		case one.CallID != c.CallID:
			one.CallID = "" // two calls of the session: neither is named
			bySession[c.Session] = one
		}
	}
	sort.Strings(order)
	for _, id := range order {
		c := bySession[id]
		if len(order) > 1 {
			c.CallID = ""
		}
		out = append(out, c)
	}
	return out, true
}

// recordFor writes a decision of server's proxy to the record of the
// session, or each session, whose call it is made during; one made before
// any call is kept for each session's first call; see pick.
func (s *serverCallers) recordFor(server string) func(string, map[string]any) error {
	return func(event string, payload map[string]any) error {
		payload["mcp_server"] = server
		cs, inCall := s.pick(server, event, payload)
		if len(cs) == 0 {
			slog.Info("egress decision of an MCP server before any call; kept for each session's first call", "server", server,
				"host", payload["host"], "decision", payload["decision"], "rule", payload["rule"])
			return nil
		}
		var errs []error
		for _, c := range cs {
			if c.Record == nil {
				slog.Warn("egress decision of an MCP server for a call with no record", "server", server,
					"host", payload["host"], "decision", payload["decision"], "rule", payload["rule"])
				continue
			}
			m := make(map[string]any, len(payload)+3)
			for k, v := range payload {
				m[k] = v
			}
			m["session"], m["call_id"] = c.Session, ""
			if inCall {
				m["call_id"] = c.CallID
			}
			if len(cs) > 1 {
				m["shared_sessions"] = len(cs)
			}
			errs = append(errs, c.Record(event, m))
		}
		return errors.Join(errs...)
	}
}
