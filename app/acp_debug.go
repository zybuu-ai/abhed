package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/dapbridge"
	"github.com/zybuu-ai/abhed/internal/debugrelay"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A person's debug session (studio-acp-contract.md §7.8): the debug adapter
// runs in the session's sandbox, as a bash call would, and its Debug Adapter
// Protocol messages are carried to and from Studio over the connection. The
// engine starts the program it judged; Studio's launch and attach are refused.

func init() {
	liveFeatures = append(liveFeatures, "debug")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/debug/launch": (*acpConn).debugLaunch,
		"_abhed/debug/send":   (*acpConn).debugSend,
		"_abhed/debug/stop":   (*acpConn).debugStop,
	})
}

// maxDebugs bounds the debug sessions one connection keeps.
const maxDebugs = 4

// debugLife bounds one debug session.
const debugLife = 8 * time.Hour

// debugEndGrace is how long an adapter has to exit once its session is over.
var debugEndGrace = 3 * time.Second

// debugCommand starts the adapter command in the session's sandbox; tests
// replace it with an adapter of their own.
var debugCommand = func(b tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
	return b.Sandbox(ctx, cwd, command)
}

// debugTier is the tier the session's commands run under; tests replace it.
var debugTier = func(b tools.Bash) string {
	if b.Sandbox != nil && b.RanUnder != nil {
		if t := b.RanUnder(); t != "" {
			return t
		}
	}
	return b.Isolation.Tier
}

type acpDebug struct {
	id, callID, session string
	relay               *debugrelay.Relay
	cancel              context.CancelFunc
	started             time.Time
	done                chan struct{}
	mu                  sync.Mutex
	stopped             bool
}

// debugTierProblem is why a tier cannot host a debug session, or nil. Only
// one with its own network namespace can: an adapter talks to its debuggee
// over loopback, and on the macOS process tier that would be the host's.
func debugTierProblem(tier string) *debugrelay.Problem {
	switch {
	case tier == string(sandbox.TierContainer) || tier == string(sandbox.TierVM):
		return nil
	case tier == string(sandbox.TierProcess) && runtime.GOOS == "linux":
		return nil
	}
	return &debugrelay.Problem{Kind: debugrelay.KindTier, Retry: true,
		Message: fmt.Sprintf("a debug session needs the container tier here; this session's commands run under the %s tier", orDefault(tier, "unknown")),
		Steps: []string{"Start a container engine: `podman machine start` (or Docker Desktop)",
			"Set \"sandbox\": {\"min_tier\": \"container\"} in ~/.abhed/config.json, with an image that has your debugger (sandbox.image)",
			"Open a new session and start the debug session again"}}
}

func debugRefusal(p *debugrelay.Problem) *rpcError {
	code := errRefused
	if p.Kind == debugrelay.KindArgs {
		code = errParams
	}
	return &rpcError{Code: code, Message: p.Message, Data: map[string]any{"problem": p}}
}

// LinuxHelper finds the bundled Linux build of the engine for a sandbox's
// architecture: the binary that serves as the egress relay and the debug
// bridge inside a sandbox, beside the engine and checksum-pinned by a release
// build. pinned says whether the pin held.
type LinuxHelper interface {
	Find(arch string) (path string, pinned bool, err error)
}

// linuxHelper is the lookup the debug bridge uses; tests set it.
var linuxHelper LinuxHelper = bundledLinuxHelper{}

// bundledLinuxHelper is sandbox.BundledHelper: the helper shipped beside the
// engine, used only as a checked copy matching the build's pin.
type bundledLinuxHelper struct{}

func (bundledLinuxHelper) Find(arch string) (string, bool, error) {
	path, why := sandbox.BundledHelper(arch)
	if path == "" {
		return "", false, errors.New(why)
	}
	return path, true, nil
}

// debugBridge is the command that carries Delve's socket over stdio inside
// the sandbox, the host file it runs, and whether the build pinned it: the
// configured relay_binary when set, else the bundled Linux helper. Both are
// builds of the engine, run in their bridge mode.
func debugBridge(relayBinary string) (command, path string, pinned bool, p *debugrelay.Problem) {
	path = relayBinary
	if path == "" {
		var err error
		if path, pinned, err = linuxHelper.Find(runtime.GOARCH); err != nil {
			return "", "", false, &debugrelay.Problem{Kind: debugrelay.KindBridge, Retry: true,
				Message: "Go debugging needs the Linux build of Abhed for " + runtime.GOARCH + " to bridge Delve's socket inside the sandbox: " + err.Error(),
				Steps: []string{"Install Abhed from its release archive or with Studio, which ship the Linux helper beside abhed",
					"Or have your administrator set relay_binary (a Linux build of Abhed for the sandbox's architecture) in the managed configuration",
					"Start the debug session again"}}
		}
	}
	return debugrelay.Quote(path) + " " + dapbridge.Arg, path, pinned, nil
}

func (c *acpConn) debugLaunch(msg rpcMessage) {
	var req debugrelay.Request
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		c.reply(msg.ID, nil, badParams("", err))
		return
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.debugMu.Lock()
	n := len(c.debugs)
	c.debugMu.Unlock()
	if n >= maxDebugs {
		c.reply(msg.ID, nil, refusal(errRefused, "%d debug sessions are open; stop one first", maxDebugs))
		return
	}
	tool, ok := parts.Loop.Tools.Get("bash")
	b, isBash := tool.(tools.Bash)
	if !ok || !isBash || b.Sandbox == nil {
		c.reply(msg.ID, nil, debugRefusal(debugTierProblem("none")))
		return
	}
	tier := debugTier(b)
	if p := debugTierProblem(tier); p != nil {
		c.reply(msg.ID, nil, debugRefusal(p))
		return
	}
	var bridge, bridgePath string
	var pinned bool
	if req.Language == debugrelay.Go {
		relayBin := parts.Config.ControlPlane.Podman.RelayBinary
		if relayBin == "" {
			relayBin = parts.Config.ControlPlane.Docker.RelayBinary
		}
		var p *debugrelay.Problem
		if bridge, bridgePath, pinned, p = debugBridge(relayBin); p != nil {
			c.reply(msg.ID, nil, debugRefusal(p))
			return
		}
	}
	spec, err := debugrelay.Plan(req, parts.Session.Root, bridge)
	var problem *debugrelay.Problem
	if errors.As(err, &problem) {
		c.reply(msg.ID, nil, debugRefusal(problem))
		return
	} else if err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "%v", err))
		return
	}

	// The debuggee's command line is what is judged, by policy and the
	// supervisor, and recorded as the person's own bash call.
	loop := parts.Loop
	callID := "u" + acpID()
	args, _ := json.Marshal(map[string]any{"command": spec.Display, "description": "debug a " + req.Language + " program in the session sandbox"})
	ctx, cancel := context.WithTimeout(context.Background(), debugLife)
	_, refused, err := loop.ManualAuthorizeReviewed(ctx, "bash", callID, args)
	if err != nil {
		cancel()
		c.reply(msg.ID, nil, refusal(errRecord, "the debug session could not be recorded, so it was not started: %v", err))
		return
	}
	if refused != nil {
		cancel()
		_ = loop.ManualObserve(callID, "bash", *refused, 0)
		c.reply(msg.ID, nil, refusal(errPolicy, "%s", refused.Content))
		return
	}
	lctx := loop.LaunchContext(ctx, callID)
	if bridgePath != "" {
		lctx = sandbox.WithReadOnlyPaths(lctx, bridgePath)
	}
	cmd := debugCommand(b, lctx, spec.Cwd, spec.Command)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, left: 64 << 10}
	err = errors.Join(cmd.Err, err1, err2)
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "the debug adapter could not start: " + err.Error(), IsError: true}, 0)
		c.reply(msg.ID, nil, refusal(errRefused, "the debug adapter could not start: %v", err))
		return
	}
	d := &acpDebug{id: "dbg-" + acpID(), callID: callID, session: s.id, cancel: cancel, started: time.Now(), done: make(chan struct{})}
	d.relay = debugrelay.New(stdin,
		spec.Launch,
		func(m json.RawMessage) {
			c.notification("_abhed/debug/message", map[string]any{"debugId": d.id, "message": m})
		},
		func(state, message string) {
			c.notification("_abhed/debug/status", map[string]any{"debugId": d.id, "state": state, "message": redacted(message)})
		})
	// Over by the protocol: the adapter's input closes, as debugpy's needs to
	// exit, and it is ended if it has not gone within the grace.
	d.relay.OnEnd(func() {
		_ = stdin.Close()
		go func() {
			select {
			case <-d.done:
			case <-time.After(debugEndGrace):
				d.cancel()
			}
		}()
	})
	c.debugMu.Lock()
	if c.debugs == nil {
		c.debugs = map[string]*acpDebug{}
	}
	c.debugs[d.id] = d
	c.debugMu.Unlock()
	launched := agent.DebugLaunched{DebugID: d.id, CallID: callID, Language: req.Language, Command: spec.Display, Tier: tier, BridgePinned: pinned}
	if bridgePath != "" {
		launched.Bridge = bridgePath
	}
	s.record(agent.EvDebugLaunched, launched)
	go c.debugRun(s, d, cmd, stdout, &stderr, b.Isolation.Tier)
	c.reply(msg.ID, map[string]any{"debugId": d.id}, nil)
}

// debugRun carries the adapter's messages until it ends, then records how
// the session ended.
func (c *acpConn) debugRun(s *acpSession, d *acpDebug, cmd *exec.Cmd, stdout io.Reader, stderr *bytes.Buffer, tier string) {
	defer close(d.done)
	_ = d.relay.Pump(stdout)
	err := cmd.Wait()
	sandbox.Release(cmd)
	d.cancel()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	d.mu.Lock()
	reason := "ended"
	if d.stopped {
		reason = "stopped"
	}
	d.mu.Unlock()
	state := map[string]any{"debugId": d.id, "state": "ended"}
	if p := debugrelay.MissingAdapter(code, ""); p != nil {
		reason = "adapter-missing"
		state["state"], state["message"], state["problem"] = "failed", p.Message, p
	} else if code != 0 && reason == "ended" {
		reason = "failed"
		state["state"], state["message"] = "failed", redacted(lastLines(stderr.String(), 20))
	}
	exit := code
	s.record(agent.EvDebugStopped, agent.DebugStopped{DebugID: d.id, CallID: d.callID, Reason: reason, ExitCode: &exit,
		DurationMS: time.Since(d.started).Milliseconds()})
	if s.inner {
		res := tools.Result{Content: fmt.Sprintf("debug session %s: exit %d", reason, code), ExitCode: &exit, IsError: code != 0 && reason != "stopped", Tier: tier}
		if tail := lastLines(stderr.String(), 20); tail != "" {
			res.Content += "\n" + redacted(tail)
		}
		_ = s.parts.Loop.ManualObserve(d.callID, "bash", res, time.Since(d.started))
	}
	c.debugMu.Lock()
	delete(c.debugs, d.id)
	c.debugMu.Unlock()
	c.notification("_abhed/debug/status", state)
}

func (c *acpConn) debugOf(params json.RawMessage) (*acpDebug, *rpcError) {
	var p struct {
		DebugID string `json:"debugId"`
	}
	_ = json.Unmarshal(params, &p)
	c.debugMu.Lock()
	d := c.debugs[p.DebugID]
	c.debugMu.Unlock()
	if d == nil {
		return nil, refusal(errParams, "no debug session %q", p.DebugID)
	}
	return d, nil
}

func (c *acpConn) debugSend(msg rpcMessage) {
	d, e := c.debugOf(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	var p struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil || len(p.Message) == 0 {
		c.reply(msg.ID, nil, refusal(errParams, "message is required"))
		return
	}
	if err := d.relay.FromEditor(p.Message); err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "%v", err))
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) debugStop(msg rpcMessage) {
	d, e := c.debugOf(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	d.stop()
	c.reply(msg.ID, map[string]any{}, nil)
}

// stop asks the adapter to end the program, and ends the adapter if it has
// not gone within a few seconds.
func (d *acpDebug) stop() {
	d.mu.Lock()
	already := d.stopped
	d.stopped = true
	d.mu.Unlock()
	if already {
		return
	}
	_ = d.relay.Disconnect()
	go func() {
		select {
		case <-d.done:
		case <-time.After(3 * time.Second):
			d.cancel()
		}
	}()
}

// killDebugs stops a session's debug sessions, or every one for "", and
// waits a moment for them to be recorded.
func (c *acpConn) killDebugs(sessionID string) {
	c.debugMu.Lock()
	var mine []*acpDebug
	for _, d := range c.debugs {
		if sessionID == "" || d.session == sessionID {
			mine = append(mine, d)
		}
	}
	c.debugMu.Unlock()
	for _, d := range mine {
		d.stop()
	}
	for _, d := range mine {
		select {
		case <-d.done:
		case <-time.After(5 * time.Second):
		}
	}
}

// limitedWriter keeps the first left bytes written and drops the rest.
type limitedWriter struct {
	mu   sync.Mutex
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left > 0 {
		n := min(len(p), l.left)
		_, _ = l.w.Write(p[:n])
		l.left -= n
	}
	return len(p), nil
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
