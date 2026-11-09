package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// A simple hook is one command, or one HTTP endpoint, run once per event.
// It takes the same events and the same Request as an extension, and the
// Host combines its answer the same way, so it can block and never approve.
//
// A command gets the event as JSON on stdin. Exit 0 goes on, and may print a
// JSON reply; exit 2 blocks, with stderr as the reason (on stop it asks the
// agent to continue, with stderr as what to do); any other exit, or running
// past its timeout, is a failure, which refuses a tool_call or
// permission_request as a crashed extension does. An HTTP hook is POSTed the
// same JSON: a 2xx answer goes on, with an optional JSON reply; anything else
// is a failure.

// Hook kinds, as Fired records them.
const (
	KindCommand = "command"
	KindHTTP    = "http"
)

// EvStop fires when the agent is about to finish answering. A hook that
// blocks it, or replies continue, sends the agent back to work with the
// reason as its instruction, a bounded number of times.
const EvStop Event = "stop"

// EvSubagentStart fires when the agent starts a subagent. It only observes.
const EvSubagentStart Event = "subagent_start"

// EvPostCompact fires after history was summarized. It only observes.
const EvPostCompact Event = "post_compact"

// EvToolFailure fires after a tool ran and failed. It only observes.
const EvToolFailure Event = "tool_failure"

// HookEvents are the events a simple hook may take.
var HookEvents = []Event{EvToolCall, EvPermissionRequest, EvUserPromptSubmit, EvStop,
	EvToolResult, EvToolFailure, EvTurnEnd, EvSubagentStart, EvSubagentEnd, EvPostCompact, EvNotification}

// HookConfig is one simple hook.
type HookConfig struct {
	Name   string
	Events []Event
	// Command is run by the Host's runner; URL is POSTed to instead.
	Command string
	URL     string
	Headers map[string]string
	// Match narrows tool events to the calls a rule matches, as for an extension.
	Match   []string
	Timeout time.Duration
	// Layer is the configuration layer the hook came from, for the record.
	Layer string
}

// CommandResult is what one command hook run left.
type CommandResult struct {
	Stdout, Stderr []byte
	Exit           int
}

// CommandRunner runs a hook's command with stdin, in the session's sandbox.
// A command that ran and exited is a result, whatever its status; err is a
// command that could not be run.
type CommandRunner func(ctx context.Context, command string, stdin []byte) (CommandResult, error)

// HookEnv is what simple hooks run with. A command hook without Run, or an
// HTTP hook without HTTP, fails each time it is asked, so it still refuses.
type HookEnv struct {
	Run  CommandRunner
	HTTP *http.Client
}

const (
	defaultHookTimeout = 10 * time.Second
	maxHookOutput      = 64 << 10
	maxHookReason      = 2000
)

// hookRunner runs one simple hook.
type hookRunner struct {
	cfg  HookConfig
	env  HookEnv
	host *Host
}

// LoadHooks adds the simple hooks. One whose configuration is not usable is
// reported and left out, as an extension that fails to start is.
func (h *Host) LoadHooks(cfgs []HookConfig, env HookEnv) []error {
	var errs []error
	for _, c := range cfgs {
		e, err := newHook(h, c, env)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		h.exts = append(h.exts, e)
	}
	return errs
}

func newHook(h *Host, c HookConfig, env HookEnv) (*Extension, error) {
	if c.Name == "" {
		return nil, errors.New("hook: name is required")
	}
	if (c.Command == "") == (c.URL == "") {
		return nil, fmt.Errorf("hook %q: set one of command and url", c.Name)
	}
	if len(c.Events) == 0 {
		return nil, fmt.Errorf("hook %q: events is required", c.Name)
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultHookTimeout
	}
	var match []policy.Rule
	for _, m := range c.Match {
		r, err := policy.ParseRule(m)
		if err != nil {
			return nil, fmt.Errorf("hook %q: match: %w", c.Name, err)
		}
		match = append(match, r)
	}
	subs := map[Event]bool{}
	for _, ev := range c.Events {
		if !isHookEvent(ev) {
			return nil, fmt.Errorf("hook %q: %q is not an event a hook takes", c.Name, ev)
		}
		subs[ev] = true
	}
	return &Extension{
		cfg:  Config{Name: c.Name, Timeout: c.Timeout, Match: c.Match},
		subs: subs, logf: h.logf, match: match,
		hook: &hookRunner{cfg: c, env: env, host: h},
	}, nil
}

func isHookEvent(ev Event) bool {
	for _, e := range HookEvents {
		if e == ev {
			return true
		}
	}
	return false
}

// Kind is "command" or "http" for a simple hook, "" for an extension.
func (e *Extension) Kind() string {
	if e.hook == nil {
		return ""
	}
	if e.hook.cfg.URL != "" {
		return KindHTTP
	}
	return KindCommand
}

// run runs the hook once for req and records the run, whatever came of it.
func (r *hookRunner) run(ctx context.Context, req Request) (Reply, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	var reply Reply
	exit := -1
	kind := KindCommand
	if r.cfg.URL != "" {
		kind = KindHTTP
		reply, exit, err = r.post(ctx, body)
	} else {
		reply, exit, err = r.command(ctx, req.Event, body)
	}
	if req.Event == EvStop && reply.Block {
		// Blocking a stop is asking the agent to go on.
		reply.Block, reply.Continue = false, true
	}
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("did not finish within %s", r.cfg.Timeout)
	}
	f := Fired{Extension: r.cfg.Name, Event: req.Event, SessionID: req.SessionID, Kind: kind,
		Layer: r.cfg.Layer, Duration: time.Since(start)}
	if exit >= 0 {
		f.ExitCode = &exit
	}
	switch {
	case err != nil:
		f.Verdict, f.Reason = VerdictError, err.Error()
		r.host.logf("hook %s failed on %s: %v", r.cfg.Name, req.Event, err)
	case reply.Continue:
		f.Verdict, f.Reason = VerdictContinue, reply.Reason
	case reply.Block:
		f.Verdict, f.Reason = VerdictBlock, reply.Reason
	case reply.Ask:
		f.Verdict, f.Reason = VerdictAsk, reply.Reason
	case reply.Reason != "" || reply.Log != "":
		f.Verdict, f.Reason = VerdictAnnotate, firstNonEmpty(reply.Reason, reply.Log)
	default:
		f.Verdict = VerdictNone
	}
	r.host.fired(f)
	if reply.Log != "" {
		r.host.logf("hook %s: %s", r.cfg.Name, reply.Log)
	}
	return reply, err
}

func (r *hookRunner) command(ctx context.Context, ev Event, stdin []byte) (Reply, int, error) {
	if r.env.Run == nil {
		return Reply{}, -1, errors.New("no sandbox to run it in")
	}
	res, err := r.env.Run(ctx, r.cfg.Command, stdin)
	if err != nil {
		return Reply{}, -1, err
	}
	if ctx.Err() != nil {
		return Reply{}, res.Exit, ctx.Err()
	}
	switch res.Exit {
	case 0:
		reply, err := parseHookReply(res.Stdout)
		return reply, 0, err
	case 2:
		why := clip(strings.TrimSpace(string(res.Stderr)))
		if ev == EvStop {
			return Reply{Continue: true, Reason: why}, 2, nil
		}
		return Reply{Block: true, Reason: why}, 2, nil
	default:
		msg := fmt.Sprintf("exited with status %d", res.Exit)
		if s := clip(strings.TrimSpace(string(res.Stderr))); s != "" {
			msg += ": " + s
		}
		return Reply{}, res.Exit, errors.New(msg)
	}
}

func (r *hookRunner) post(ctx context.Context, body []byte) (Reply, int, error) {
	if r.env.HTTP == nil {
		return Reply{}, -1, errors.New("no HTTP client")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return Reply{}, -1, err
	}
	for k, v := range r.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := r.env.HTTP.Do(hreq)
	if err != nil {
		return Reply{}, -1, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxHookOutput))
	if err != nil {
		return Reply{}, -1, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Reply{}, -1, fmt.Errorf("answered %s", resp.Status)
	}
	reply, err := parseHookReply(out)
	return reply, -1, err
}

// parseHookReply reads what a hook printed or answered: nothing, plain text
// (no opinion), or a JSON object of which only the vetoes count.
func parseHookReply(out []byte) (Reply, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 || out[0] != '{' {
		return Reply{}, nil
	}
	var r Reply
	if err := json.Unmarshal(out, &r); err != nil {
		return Reply{}, fmt.Errorf("reply is not JSON: %w", err)
	}
	// A simple hook vetoes or asks the agent on; it rewrites nothing.
	return Reply{Block: r.Block, Ask: r.Ask, Continue: r.Continue, Reason: clip(r.Reason), Log: clip(r.Log)}, nil
}

func clip(s string) string {
	if len(s) > maxHookReason {
		return s[:maxHookReason] + "…"
	}
	return s
}

// label names the extension or hook, for a reason a person reads.
func (e *Extension) label() string {
	if e.hook != nil {
		return "hook " + e.Name()
	}
	return "extension " + e.Name()
}
