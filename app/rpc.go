package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

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
func rpcCmd(workspace string) int {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := json.NewEncoder(os.Stdout)

	stopper := cancelOnStop(stopExits)
	defer stopper.stop()
	ctx := stopper.ctx
	var a *abhed.Agent
	defer func() {
		if a != nil {
			a.Close()
		}
	}()

	// Events arrive from the agent's own goroutine, so writes take turns.
	var outMu sync.Mutex
	emit := func(v any) {
		outMu.Lock()
		defer outMu.Unlock()
		if err := out.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: rpc write failed: %v\n", err)
		}
	}

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

		switch req.Method {
		case "start":
			if a != nil {
				a.Close()
			}
			ws := req.Workspace
			if ws == "" {
				ws = workspace
			}
			opts := abhed.Options{
				Workspace: ws, ConfigDir: ws, Mode: req.Mode,
				Allow: req.Allow, Deny: req.Deny,
				// bash runs in the configured tier, as it would from the terminal.
				Sandbox: true,
				// Events are forwarded as they happen so a caller can render
				// progress rather than waiting for the final answer.
				OnEvent: func(ev agent.Event) {
					emit(rpcResponse{Type: "event", Event: &ev})
				},
			}
			var err error
			a, err = abhed.New(ctx, opts)
			if err != nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: err.Error()})
				continue
			}
			emit(rpcResponse{ID: req.ID, Type: "ready"})

		case "prompt":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error",
					Error: "no session: send start first"})
				continue
			}
			done := stopper.busy()
			answer, err := a.Run(ctx, req.Prompt)
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

		case "steer":
			if a == nil {
				emit(rpcResponse{ID: req.ID, Type: "error", Error: "no session"})
				continue
			}
			// Steering is why this is a persistent process rather than a
			// request per run: a caller can redirect work already underway.
			a.Steer(req.Prompt)
			emit(rpcResponse{ID: req.ID, Type: "steered"})

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

		case "quit":
			emit(rpcResponse{ID: req.ID, Type: "bye"})
			return 0

		default:
			emit(rpcResponse{ID: req.ID, Type: "error",
				Error: fmt.Sprintf("unknown method %q; want start, prompt, steer, "+
					"usage, export, providers or quit", req.Method)})
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: rpc read failed: %v\n", err)
		return 1
	}
	return 0
}

type rpcRequest struct {
	ID        string   `json:"id,omitempty"`
	Method    string   `json:"method"`
	Prompt    string   `json:"prompt,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Allow     []string `json:"allow,omitempty"`
	Deny      []string `json:"deny,omitempty"`
}

type rpcResponse struct {
	ID        string       `json:"id,omitempty"`
	Type      string       `json:"type"`
	Answer    string       `json:"answer,omitempty"`
	Error     string       `json:"error,omitempty"`
	Event     *agent.Event `json:"event,omitempty"`
	Usage     *agent.Usage `json:"usage,omitempty"`
	Providers []string     `json:"providers,omitempty"`
}
