// Package extension lets an operator change how the agent behaves without
// forking it.
//
// The design question every harness answers is what belongs inside the agent
// and what belongs outside. An API broad enough to rebuild most of the harness
// is the right answer for one developer on a laptop. Abhed's claim is that what
// an agent was permitted to do can be proven afterwards — and an extension that
// can grant permission is one that can also remove the proof.
//
// So the seam here is deliberately narrow in exactly one direction:
//
//	An extension may VETO, never PERMIT.
//
// It can block a call, force it to an approval prompt, rewrite the arguments
// before it runs, rewrite the result before the model sees it, filter the
// messages sent upstream, and add to the system prompt. It cannot turn a denied
// action into an allowed one. Deny rules stay absolute, which is what keeps the
// audit trail meaningful: no configuration and no extension changes what the
// policy engine forbids.
//
// Extensions are separate processes speaking JSONL over stdin and stdout, in
// any language. That costs a few milliseconds per hook against an in-process
// interpreter, and buys three things worth more: Abhed stays one static binary
// with no runtime to install (which is the whole air-gap story), an extension
// crash cannot take the agent down with it, and the sandbox is the operating
// system's rather than one Abhed has to write and defend.
package extension

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Event names an extension can subscribe to.
type Event string

const (
	// EvToolCall fires before a tool runs. The reply may block it, force an
	// approval prompt, or rewrite its arguments.
	EvToolCall Event = "tool_call"
	// EvToolResult fires after a tool runs, before the model sees the output.
	// The reply may rewrite the content or mark it an error.
	EvToolResult Event = "tool_result"
	// EvContext fires before each model call. The reply may drop messages —
	// for redaction, or to keep a long session inside a window.
	EvContext Event = "context"
	// EvBeforeAgentStart fires once per run, before the first model call. The
	// reply may append to the system prompt.
	EvBeforeAgentStart Event = "before_agent_start"
	// EvSessionStart and EvSessionEnd bracket the run, for setup and teardown.
	EvSessionStart Event = "session_start"
	EvSessionEnd   Event = "session_end"
	// EvListTools is sent once at startup. An extension answers with the tools
	// it provides, and EvInvokeTool then routes calls to it.
	EvListTools Event = "list_tools"
	// EvInvokeTool runs one of an extension's own tools.
	EvInvokeTool Event = "invoke_tool"
	// EvBeforeCompact fires before history is summarized. The reply may cancel
	// the compaction or supply the summary itself, which is how an operator
	// keeps something the default summarizer would drop.
	EvBeforeCompact Event = "before_compact"
)

// Request is what Abhed sends an extension.
type Request struct {
	Event     Event           `json:"event"`
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Messages  []Message       `json:"messages,omitempty"`
	System    string          `json:"system,omitempty"`
}

// Message is the subset of a conversation message an extension can see. Tool
// arguments and results travel as text: an extension that filters context is
// deciding what the model may read, and does not need to reconstruct calls.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Tool    string `json:"tool,omitempty"`
}

// Reply is what an extension sends back. Every field is optional; an empty
// reply means "no opinion", which is what a crashed or silent extension is
// treated as.
type Reply struct {
	// Block stops a tool call. Reason is reported to the model so it can adapt.
	Block  bool   `json:"block,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Ask forces the call to an approval prompt even where policy would have
	// allowed it. This is the strongest thing an extension can do that is not a
	// refusal, and it is still a veto: it removes an automatic approval, it
	// never grants one.
	Ask bool `json:"ask,omitempty"`
	// Args replaces the tool arguments. Ignored unless the event is tool_call.
	Args json.RawMessage `json:"args,omitempty"`
	// Content replaces a tool result. Ignored unless the event is tool_result.
	Content *string `json:"content,omitempty"`
	IsError *bool   `json:"is_error,omitempty"`
	// Keep selects which messages survive, by index into the request. A nil
	// slice keeps everything; an empty slice is honoured and keeps nothing.
	Keep []int `json:"keep,omitempty"`
	// System is appended to the system prompt. It cannot replace it: the
	// operating rules an operator configured are not an extension's to discard.
	System string `json:"system,omitempty"`
	// Log is written to Abhed's log, for an extension to explain itself.
	Log string `json:"log,omitempty"`
	// Tools answers list_tools: the tools this extension provides.
	Tools []ToolDef `json:"tools,omitempty"`
	// Result answers invoke_tool.
	Result string `json:"result,omitempty"`
	// Summary answers before_compact: the summary to use instead of asking the
	// model for one.
	Summary string `json:"summary,omitempty"`
	// Cancel answers before_compact: leave the history alone this time.
	Cancel bool `json:"cancel,omitempty"`
}

// ToolDef is a tool an extension provides.
//
// An extension-provided tool is a tool like any other: it appears in the
// model's tool list, it goes through the policy engine, and its call and result
// are recorded as events. Providing one is not a way around the rules — a
// tool that says it mutates is subject to approval exactly as a built-in is,
// and a deny rule naming it still wins.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	// Mutates decides whether calls route through approval. An extension that
	// omits it gets the safe answer: Abhed cannot know what someone else's
	// tool does, so it assumes the call can change something.
	Mutates *bool `json:"mutates,omitempty"`
}

// Config describes one extension.
type Config struct {
	Name    string        `json:"name"`
	Command string        `json:"command"`
	Args    []string      `json:"args,omitempty"`
	Events  []Event       `json:"events,omitempty"`
	Timeout time.Duration `json:"-"`
	// TimeoutMS is the config-file form of Timeout.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// Env is passed to the process on top of Abhed's own environment.
	Env map[string]string `json:"env,omitempty"`
}

// Extension is one running extension process.
type Extension struct {
	cfg    Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	subs   map[Event]bool
	dead   bool
	logf   func(string, ...any)
}

const defaultTimeout = 5 * time.Second

// Start launches the extension process.
func Start(ctx context.Context, cfg Config, logf func(string, ...any)) (*Extension, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("extension: name is required")
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("extension %q: command is required", cfg.Name)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
		if cfg.TimeoutMS > 0 {
			cfg.Timeout = time.Duration(cfg.TimeoutMS) * time.Millisecond
		}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Env = environ(cfg.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Stderr is the extension's own channel for diagnostics; it is not parsed,
	// so an extension that prints a warning does not corrupt the protocol.
	cmd.Stderr = &prefixWriter{name: cfg.Name, logf: logf}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("extension %q: %w", cfg.Name, err)
	}

	e := &Extension{
		cfg: cfg, cmd: cmd, stdin: stdin,
		stdout: bufio.NewReaderSize(stdout, 1<<20),
		subs:   map[Event]bool{}, logf: logf,
	}
	for _, ev := range cfg.Events {
		e.subs[ev] = true
	}
	// No declared events means every event, so a simple extension needs no
	// subscription list.
	if len(e.subs) == 0 {
		for _, ev := range []Event{EvToolCall, EvToolResult, EvContext,
			EvBeforeAgentStart, EvSessionStart, EvSessionEnd,
			EvListTools, EvInvokeTool, EvBeforeCompact} {
			e.subs[ev] = true
		}
	}
	return e, nil
}

func (e *Extension) Name() string { return e.cfg.Name }

// Subscribed reports whether this extension wants an event.
func (e *Extension) Subscribed(ev Event) bool { return !e.dead && e.subs[ev] }

// Call sends one request and waits for the reply.
//
// A failure here is never fatal to the run. An extension that crashes, hangs or
// answers with nonsense is marked dead and skipped from then on: the agent
// continues under policy alone. The alternative — failing the session because a
// plugin misbehaved — trades a working agent for a broken one and protects
// nothing, since an extension can only ever have made the decision stricter.
func (e *Extension) Call(ctx context.Context, req Request) Reply {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return Reply{}
	}

	line, err := json.Marshal(req)
	if err != nil {
		return Reply{}
	}
	if _, err := e.stdin.Write(append(line, '\n')); err != nil {
		e.die("write failed: %v", err)
		return Reply{}
	}

	type result struct {
		reply Reply
		err   error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := e.stdout.ReadBytes('\n')
		if err != nil {
			done <- result{err: err}
			return
		}
		var reply Reply
		if err := json.Unmarshal(raw, &reply); err != nil {
			done <- result{err: fmt.Errorf("reply is not JSON: %w", err)}
			return
		}
		done <- result{reply: reply}
	}()

	timeout := time.NewTimer(e.cfg.Timeout)
	defer timeout.Stop()
	select {
	case <-ctx.Done():
		return Reply{}
	case <-timeout.C:
		// A hung extension is not retried: the read goroutine still owns the
		// stream, so the next reply would be mismatched with its request.
		e.die("did not answer %s within %s", req.Event, e.cfg.Timeout)
		return Reply{}
	case r := <-done:
		if r.err != nil {
			e.die("%v", r.err)
			return Reply{}
		}
		if r.reply.Log != "" {
			e.logf("extension %s: %s", e.cfg.Name, r.reply.Log)
		}
		return r.reply
	}
}

func (e *Extension) die(format string, args ...any) {
	if e.dead {
		return
	}
	e.dead = true
	e.logf("extension %s disabled: %s", e.cfg.Name, fmt.Sprintf(format, args...))
	_ = e.stdin.Close()
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
}

// Close shuts the extension down.
func (e *Extension) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return nil
	}
	e.dead = true
	_ = e.stdin.Close()
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
	return nil
}

func environ(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil // inherit Abhed's environment unchanged
	}
	// The extension's own variables are layered ON TOP of Abhed's environment,
	// which is what the Config.Env doc promises ("passed to the process on top
	// of Abhed's own environment"). Returning only `extra` — as this once did —
	// silently dropped HOME, the locale and PATH, so setting a single variable
	// broke the extension in ways that were hard to trace back to the cause.
	// A key present in both wins from `extra`, since exec takes the last value
	// for a repeated name.
	out := os.Environ()
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

type prefixWriter struct {
	name string
	logf func(string, ...any)
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			w.logf("extension %s: %s", w.name, line)
		}
	}
	return len(p), nil
}
