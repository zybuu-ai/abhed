package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// rpcCmd drives Abhed over stdin and stdout as line-delimited JSON.
//
// The SDK covers a caller written in Go. Everything else — a Python service, a
// TypeScript extension, an editor plugin — had only the HTTP server, which
// means running a server, choosing a port, and handling auth for what is really
// one process talking to its own child. A subprocess speaking JSONL is the
// smaller thing, and it is the shape the extension host already speaks.
//
// One request per line, one or more events back per request. Every event the
// agent records is forwarded, so a caller sees tool calls and results as they
// happen rather than only the final answer.
func rpcCmd(workspace string, trust config.TrustChoice) int {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := json.NewEncoder(os.Stdout)

	stopper := cancelOnStop(stopExits)
	defer stopper.stop()
	ctx := stopper.ctx

	// Events arrive from the agent's own goroutine, so writes take turns.
	var outMu sync.Mutex
	emit := func(v any) {
		outMu.Lock()
		defer outMu.Unlock()
		if err := out.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: rpc write failed: %v\n", err)
		}
	}

	// Input is read on its own goroutine so a steer reaches the run in
	// progress; every other request waits its turn, in the order sent.
	var (
		mu      sync.Mutex
		pending []rpcQueued
		eof     bool
		readErr error
		// latest is the session the last start read will make; a steer goes there.
		latest *rpcSession
	)
	wake := make(chan struct{}, 1)
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go func() {
		for in.Scan() {
			line := in.Bytes()
			if len(line) == 0 {
				continue
			}
			var req rpcRequest
			if err := json.Unmarshal(line, &req); err != nil {
				emit(rpcResponse{ID: req.ID, Type: "error",
					Error: "request is not JSON: " + err.Error()})
				continue
			}
			mu.Lock()
			switch {
			case req.Method == "steer":
				if latest == nil {
					mu.Unlock()
					emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session: send start first"})
					continue
				}
				// Answered under mu, so it cannot cross the prompt's end or a start's result.
				emit(latest.steer(req))
			case len(pending) >= rpcMaxPending:
				emit(rpcResponse{ID: req.ID, Type: "error", Error: fmt.Sprintf(
					"%d requests are already waiting; send more after their answers", rpcMaxPending)})
			default:
				q := rpcQueued{req: req}
				if req.Method == "start" {
					q.sess = &rpcSession{}
					latest = q.sess
				}
				pending = append(pending, q)
			}
			mu.Unlock()
			signal()
		}
		mu.Lock()
		eof, readErr = true, in.Err()
		mu.Unlock()
		signal()
	}()

	var cur *rpcSession
	// undelivered says, before the session closes, what steering it never read.
	undelivered := func(why string) {
		if cur == nil || cur.agent == nil {
			return
		}
		if n := cur.agent.Queued(); n > 0 {
			emit(rpcResponse{Type: "error", Error: fmt.Sprintf(
				"%d queued steer message(s) were not delivered: %s", n, why)})
		}
	}
	defer func() {
		if cur != nil && cur.agent != nil {
			cur.agent.Close()
		}
	}()

	for {
		mu.Lock()
		if len(pending) == 0 {
			done, err := eof, readErr
			mu.Unlock()
			if done {
				undelivered("input ended before another prompt")
				if err != nil {
					fmt.Fprintf(os.Stderr, "abhed: rpc read failed: %v\n", err)
					return 1
				}
				return 0
			}
			<-wake
			continue
		}
		q := pending[0]
		pending = pending[1:]
		mu.Unlock()
		req := q.req

		var a *abhed.Agent
		if cur != nil {
			a = cur.agent
		}
		switch req.Method {
		case "start":
			if a != nil {
				undelivered("a new session was started")
				a.Close()
			}
			ws := req.Workspace
			if ws == "" {
				ws = workspace
			}
			opts := abhed.Options{
				Workspace: ws, ConfigDir: ws, Mode: req.Mode, WorkspaceTrust: trust, AllowDefaultModel: true,
				Allow: req.Allow, Deny: req.Deny,
				// bash runs in the configured tier, as it would from the terminal.
				Sandbox: true,
				// The agent the terminal runs, subagents and configured tools included.
				ConfiguredTools: true,
				// The configuration's turn limit binds, as it does from the terminal.
				ConfiguredLimits: true,
				// Stdout is the protocol; what the tool set skipped goes to stderr.
				Warn: warnf,
				// off (the default) or notify: rpc never starts a run on its own.
				Background: req.Wake,
				// Events are forwarded as they happen so a caller can render
				// progress rather than waiting for the final answer.
				OnEvent: func(ev agent.Event) {
					emit(rpcResponse{Type: "event", Event: &ev})
				},
			}
			na, err := abhed.New(ctx, opts)
			cur = q.sess
			mu.Lock()
			if err != nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: err.Error()})
			} else {
				st := na.WorkspaceTrust()
				emit(rpcResponse{ID: req.ID, Type: "ready", WorkspaceTrust: &st})
			}
			q.sess.opened(na, err, emit)
			mu.Unlock()

		case "prompt":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error",
					Error: "no session: send start first"})
				continue
			}
			done := stopper.busy()
			mu.Lock()
			cur.running = true
			mu.Unlock()
			answer, err := a.Run(ctx, req.Prompt)
			// A steer that came as the run was ending is run now, since it was
			// answered steered; steers after this point are answered queued.
			for {
				mu.Lock()
				more := err == nil && a.Queued() > 0
				if !more {
					cur.running = false
				}
				mu.Unlock()
				if !more {
					break
				}
				answer, err = a.RunQueued(ctx)
			}
			// The run's events, its end included, go out before its reply, and
			// both before an exit on a stop signal is let through.
			flushed, cancelFlush := context.WithTimeout(context.Background(), flushWait)
			_ = a.Flush(flushed)
			cancelFlush()
			if err != nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: err.Error(),
					Answer: answer})
			} else {
				emit(rpcResponse{ID: req.ID, Type: "answer", Answer: answer})
			}
			done()

		case "usage":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session"})
				continue
			}
			u := a.Usage()
			emit(rpcResponse{ID: req.ID, Type: "usage", Usage: &u})

		case "export":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session"})
				continue
			}
			emit(rpcResponse{ID: req.ID, Type: "export", Answer: a.ExportHTML()})

		case "providers":
			emit(rpcResponse{ID: req.ID, Type: "providers", Providers: abhed.Providers()})

		case "tasks":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session"})
				continue
			}
			tasks := a.Background()
			if tasks == nil {
				tasks = []abhed.TaskInfo{}
			}
			emit(rpcResponse{ID: req.ID, Type: "tasks", Tasks: tasks})

		case "cancel_task":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session"})
				continue
			}
			if err := a.CancelTask(req.TaskID); err != nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: err.Error()})
				continue
			}
			emit(rpcResponse{ID: req.ID, Type: "cancelled"})

		case "wake":
			// Runs the agent on background results waiting for it, as the caller asks.
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session"})
				continue
			}
			done := stopper.busy()
			answer, err := a.Wake(ctx)
			flushed, cancelFlush := context.WithTimeout(context.Background(), flushWait)
			_ = a.Flush(flushed)
			cancelFlush()
			if err != nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: err.Error(), Answer: answer})
			} else {
				emit(rpcResponse{ID: req.ID, Type: "answer", Answer: answer})
			}
			done()

		case "quit":
			undelivered("the session quit before another prompt")
			emit(rpcResponse{ID: req.ID, Type: "bye"})
			return 0

		default:
			emit(rpcResponse{ID: req.ID, Type: "error",
				Error: fmt.Sprintf("unknown method %q; want start, prompt, steer, "+
					"usage, export, providers, tasks, cancel_task, wake or quit", req.Method)})
		}
	}
}

// rpcMaxPending bounds the requests waiting behind a running prompt.
const rpcMaxPending = 256

// rpcQueued is a request waiting its turn; a start carries the session it makes.
type rpcQueued struct {
	req  rpcRequest
	sess *rpcSession
}

// rpcSession is one start's session, from when the start is read. Its fields
// are guarded by rpcCmd's mu.
type rpcSession struct {
	agent   *abhed.Agent
	failed  bool
	running bool
	// held are steers read before the session existed.
	held []rpcRequest
}

// steer delivers or holds a steer and returns its answer: steered when a
// prompt is running and will read it, queued when the next prompt will.
func (s *rpcSession) steer(req rpcRequest) rpcResponse {
	switch {
	case s.failed:
		return rpcResponse{ID: req.ID, Type: "error", Error: "no session: its start failed"}
	case s.agent == nil:
		s.held = append(s.held, req)
		return rpcResponse{ID: req.ID, Type: "queued"}
	}
	s.agent.Steer(req.Prompt)
	if s.running {
		return rpcResponse{ID: req.ID, Type: "steered"}
	}
	return rpcResponse{ID: req.ID, Type: "queued"}
}

// opened settles the steers held for the session once its start has run.
func (s *rpcSession) opened(a *abhed.Agent, err error, emit func(any)) {
	held := s.held
	s.held = nil
	if err != nil {
		s.failed = true
		for _, h := range held {
			emit(rpcResponse{ID: h.ID, Type: "error", Error: "steer not delivered: the session did not start"})
		}
		return
	}
	s.agent = a
	for _, h := range held {
		a.Steer(h.Prompt)
	}
}

type rpcRequest struct {
	ID        string   `json:"id,omitempty"`
	Method    string   `json:"method"`
	Prompt    string   `json:"prompt,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Allow     []string `json:"allow,omitempty"`
	Deny      []string `json:"deny,omitempty"`
	// Wake, on start, is off (the default) or notify; TaskID names a
	// background task for cancel_task.
	Wake   string `json:"wake,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type rpcResponse struct {
	ID        string       `json:"id,omitempty"`
	Type      string       `json:"type"`
	Answer    string       `json:"answer,omitempty"`
	Error     string       `json:"error,omitempty"`
	Event     *agent.Event `json:"event,omitempty"`
	Usage     *agent.Usage `json:"usage,omitempty"`
	Providers []string     `json:"providers,omitempty"`
	// Tasks answers tasks: the session's background tasks.
	Tasks []abhed.TaskInfo `json:"tasks,omitempty"`
	// WorkspaceTrust, on ready, says whether the workspace file applied whole.
	WorkspaceTrust *config.WorkspaceTrust `json:"workspace_trust,omitempty"`
}
