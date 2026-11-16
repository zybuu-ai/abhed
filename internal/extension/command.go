package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
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
// is a failure. A reply's continue false ends the run.

// Hook kinds, as Fired records them.
const (
	KindCommand = "command"
	KindHTTP    = "http"
)

// EvStop fires when the agent is about to finish answering. A hook that
// blocks it sends the agent back to work with the reason as its
// instruction, a bounded number of times.
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
	// HeadersEnv sends a header with the value of the named variable from
	// Abhed's environment, so a token is not written into the file.
	HeadersEnv map[string]string
	// Match narrows tool events to the calls a rule matches, as for an extension.
	Match []string
	// Timeout bounds one run: when zero, 10s, or 2s on an event that only observes.
	Timeout time.Duration
	// Async runs the events that only observe without waiting for them.
	Async bool
	// Layer is the configuration layer the hook came from, for the record.
	Layer string
}

// Session is the session and folder a hook event is for, as ctx carries it,
// and the root session of its tree, which a halt ends.
type Session struct{ ID, Dir, Root string }

type sessionKey struct{}

// WithSession says which session, working in dir, ctx's hook events are
// for, and the root of its session tree ("" when it is the root).
func WithSession(ctx context.Context, id, dir, root string) context.Context {
	return context.WithValue(ctx, sessionKey{}, Session{ID: id, Dir: dir, Root: root})
}

// SessionOf is what WithSession put in ctx, or nothing.
func SessionOf(ctx context.Context) Session {
	s, _ := ctx.Value(sessionKey{}).(Session)
	return s
}

// CommandResult is what one command hook run left.
type CommandResult struct {
	Stdout, Stderr []byte
	Exit           int
}

// CommandRunner runs a hook's command with stdin in the session's sandbox,
// in dir, or the workspace when dir is "". A command that ran and exited is a
// result, whatever its status; err is a command that could not be run.
type CommandRunner func(ctx context.Context, dir, command string, stdin []byte) (CommandResult, error)

// HookEnv is what simple hooks run with. A command hook without Run, or an
// HTTP hook without HTTP, fails each time it is asked, so it still refuses.
type HookEnv struct {
	Run  CommandRunner
	HTTP *http.Client
}

const (
	defaultHookTimeout = 10 * time.Second
	observeHookTimeout = 2 * time.Second
	// maxAsyncRuns bounds one hook's async runs at once; more are skipped.
	maxAsyncRuns  = 4
	MaxHookOutput = 64 << 10
	maxHookReason = 2000
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
	if err := HeadersInClear(c.URL, len(c.Headers)+len(c.HeadersEnv)); err != nil {
		return nil, fmt.Errorf("hook %q: %w", c.Name, err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
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
		cfg:      Config{Name: c.Name, Timeout: timeout, Match: c.Match, Async: c.Async},
		inflight: make(chan struct{}, maxAsyncRuns),
		subs:     subs, logf: h.logf, match: match,
		hook: &hookRunner{cfg: c, env: env, host: h}, host: h,
	}, nil
}

// HeadersInClear refuses headers, which may carry a token, sent over plain
// http to anywhere but this machine.
func HeadersInClear(rawURL string, headers int) error {
	if rawURL == "" || headers == 0 {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" {
		return nil
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); host == "localhost" || err == nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("headers are sent only over https, or plain http to this machine")
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
	timeout := r.cfg.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
		if ObserveOnly(req.Event) {
			timeout = observeHookTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
		reply, exit, err = r.command(ctx, body)
	}
	reply = reply.normalize(req.Event)
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("did not finish within %s", timeout)
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
	case reply.halt:
		f.Verdict, f.Reason = VerdictHalt, reply.haltReason("hook "+r.cfg.Name)
	case reply.again:
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

func (r *hookRunner) command(ctx context.Context, stdin []byte) (Reply, int, error) {
	if r.env.Run == nil {
		return Reply{}, -1, errors.New("no sandbox to run it in")
	}
	res, err := r.env.Run(ctx, SessionOf(ctx).Dir, r.cfg.Command, stdin)
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
		return Reply{Block: true, Reason: clip(strings.TrimSpace(string(res.Stderr)))}, 2, nil
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
	for k, name := range r.cfg.HeadersEnv {
		v, ok := os.LookupEnv(name)
		if !ok {
			return Reply{}, -1, fmt.Errorf("header %s: %s is not set", k, name)
		}
		hreq.Header.Set(k, v)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := r.env.HTTP.Do(hreq)
	if err != nil {
		return Reply{}, -1, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, MaxHookOutput))
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
	// A simple hook vetoes, asks, or ends the run; it rewrites nothing.
	return Reply{Block: r.Block, Ask: r.Ask, Decision: r.Decision, Continue: r.Continue,
		StopReason: clip(r.StopReason), Reason: clip(r.Reason), Log: clip(r.Log)}, nil
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

// CommandHooks names the simple hooks that run a command.
func (h *Host) CommandHooks() []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, e := range h.exts {
		if e.Kind() == KindCommand {
			out = append(out, e.Name())
		}
	}
	return out
}
