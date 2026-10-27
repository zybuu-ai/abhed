package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/dapbridge"
	"github.com/zybuu-ai/abhed/internal/debugrelay"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// A person's debug session (studio-acp-contract.md §7.8): the debug adapter
// runs in the session's sandbox, as a bash call would, and its Debug Adapter
// Protocol messages are carried to and from Studio over the connection. The
// engine starts the program it judged; Studio's launch and attach are refused.

func init() {
	liveFeatures = append(liveFeatures, "debug")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/debug/launch": (*acpConn).debugLaunch,
		// _abhed/debug/send is taken in order by serve (debugEnqueue).
		"_abhed/debug/send": (*acpConn).debugEnqueue,
		"_abhed/debug/stop": (*acpConn).debugStop,
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
	// sends are Studio's messages for the adapter, in the order the
	// connection read them; one goroutine passes them on.
	sends chan rpcMessage
	// qmu guards qclosed: once the session has ended no send is queued,
	// so every send is answered.
	qmu     sync.Mutex
	qclosed bool
	mu      sync.Mutex
	stopped bool
	// reason is how a stop ended it: stopped, or closed with its session.
	reason string
	// dir is a folder the engine made for the session, removed at its end.
	dir string
}

// debugTierProblem is why a tier cannot host a debug session, or nil. Only
// one where the adapter's loopback is not the host's can: an adapter talks
// to its debuggee over loopback. The container and vm tiers have their own
// network; the Linux process tier has its own namespace only when the
// network is closed or allowlisted (bubblewrap's --unshare-net), not when
// it is open; the macOS process tier never does.
func debugTierProblem(tier string, iso tools.Isolation) *debugrelay.Problem {
	switch {
	case tier == string(sandbox.TierContainer) || tier == string(sandbox.TierVM):
		return nil
	case tier == string(sandbox.TierProcess) && runtime.GOOS == "linux" && !iso.Network:
		return nil
	case tier == string(sandbox.TierProcess) && runtime.GOOS == "linux":
		return &debugrelay.Problem{Kind: debugrelay.KindTier, Retry: true,
			Message: "a debug session on the Linux process tier needs the sandbox's network closed or allowlisted; with it open the adapter's loopback would be the host's",
			Steps: []string{"Set \"sandbox\": {\"allow_network\": false} in ~/.abhed/config.json, or use the egress allowlist (\"sandbox\": {\"network\": \"allowlist\"})",
				"Or use the container tier: set \"sandbox\": {\"min_tier\": \"container\"} with an image that has your debugger",
				"Open a new session and start the debug session again"}}
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

// loopbackProblem is a debug command the sandbox would not open loopback to
// (sandbox.ErrLoopbackUnderEgress), as a problem with steps.
func loopbackProblem() *debugrelay.Problem {
	return &debugrelay.Problem{Kind: debugrelay.KindTier, Retry: true,
		Message: "this debug session needs every loopback port, which is not opened while this session's commands go out through Abhed's egress proxy " +
			"(the allowlist, or the package-registry proxy every session has unless sandbox.toolchains_registries is off)",
		Steps: []string{"Use the container tier: Set Up Debugging lists the steps (install Podman, start its machine, use the container tier)"}}
}

// ownBridge is the engine's own binary as the debug bridge, for Delve run on
// this Mac (the process tier): readable to the debug command alone.
func ownBridge() (command, path string, p *debugrelay.Problem) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", "", &debugrelay.Problem{Kind: debugrelay.KindBridge, Retry: true, Message: "the engine's own program could not be found to bridge Delve's socket: " + err.Error()}
	}
	return debugrelay.Quote(exe) + " " + dapbridge.Arg, exe, nil
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
	// A slot is held from here, so parallel launches cannot pass the bound
	// while one is being judged; every refusal below gives it back.
	if !c.reserveDebug() {
		c.reply(msg.ID, nil, refusal(errRefused, "%d debug sessions are open; stop one first", maxDebugs))
		return
	}
	reserved := true
	defer func() {
		if reserved {
			c.releaseDebug()
		}
	}()
	tool, ok := parts.Loop.Tools.Get("bash")
	b, isBash := tool.(tools.Bash)
	if !ok || !isBash || b.Sandbox == nil {
		c.reply(msg.ID, nil, debugSetupProblem(debugTierProblem("none", tools.Isolation{}), debugSetupFor(c.root(), "none", tools.Isolation{}, parts.Session.Root)))
		return
	}
	tier := debugTier(b)
	// A tier that cannot debug says what the container tier needs here; on
	// the person's choice (processTier) and agreement for the workspace, the
	// process tier debugs with loopback open to the debug command alone.
	var loopback bool
	if p := debugTierProblem(tier, b.Isolation); p != nil {
		var opt struct {
			ProcessTier bool `json:"processTier"`
		}
		_ = json.Unmarshal(msg.Params, &opt)
		if !opt.ProcessTier || !debugProcessTierPossible(tier, b.Isolation) {
			c.reply(msg.ID, nil, debugSetupProblem(p, debugSetupFor(c.root(), tier, b.Isolation, parts.Session.Root)))
			return
		}
		loopback = true
	}
	// Delve not where commands run: the install is offered before anything
	// is judged, as for a language server.
	if req.Language == debugrelay.Go {
		if find := lspProgramFinder(parts.Loop.Tools); find != nil && !find("dlv") {
			c.reply(msg.ID, nil, debugRefusal(debugrelay.NoDelve("")))
			return
		}
	}
	var bridge, bridgePath string
	var pinned bool
	if req.Language == debugrelay.Go {
		var p *debugrelay.Problem
		if loopback && runtime.GOOS == "darwin" {
			// Delve runs on this Mac: the engine's own build is the bridge.
			bridge, bridgePath, p = ownBridge()
		} else {
			bridge, bridgePath, pinned, p = debugBridge(sandboxconfig.RelayBinary(parts.Config))
		}
		if p != nil {
			c.reply(msg.ID, nil, debugRefusal(p))
			return
		}
	}
	// On the Linux process tier the session's commands share one /tmp, so
	// Delve's socket goes in a folder only this command sees.
	var sockDir string
	if tier == string(sandbox.TierProcess) && runtime.GOOS == "linux" && req.Language == debugrelay.Go {
		sockDir = "/run/abhed-dap-" + acpID()
	}
	// On macOS the engine makes it: a folder of this account's alone, the
	// one place the command may make and reach a unix socket. It goes when
	// the session does.
	var madeDir string
	if loopback && runtime.GOOS == "darwin" && req.Language == debugrelay.Go {
		dir, err := sandbox.DebugSocketDir()
		if err != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "the debug session could not start: a folder for its socket: %v", err))
			return
		}
		sockDir, madeDir = dir, dir
		defer func() {
			if madeDir != "" {
				_ = os.RemoveAll(madeDir)
			}
		}()
	}
	spec, err := debugrelay.Plan(req, parts.Session.Root, bridge, sockDir)
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
	// Debugging on the process tier is put to the person until they allow
	// it for the workspace; Allow once starts this session alone.
	var why string
	var forWorkspace bool
	ask := c.debugAsk(s, callID, spec.Display)
	if loopback && !debugConsented(parts.Session.Root) {
		why = debugLoopbackWhy
		ask = c.manualAskWith(s, callID, "debug", spec.Display, "Allow for this workspace", &forWorkspace)
	}
	_, refused, err := loop.ManualAuthorizeAsking(ctx, "bash", callID, args, ask, why)
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
	// The person may have closed the session while the supervisor's ask waited.
	if sessionClosed(s) {
		cancel()
		_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "not started: the session closed before the launch was allowed", IsError: true}, 0)
		c.reply(msg.ID, nil, refusal(errRefused, "the session closed before the debug session could start"))
		return
	}
	lctx := loop.LaunchContext(ctx, callID)
	if bridgePath != "" {
		lctx = sandbox.WithReadOnlyPaths(lctx, bridgePath)
	}
	if sockDir != "" {
		lctx = sandbox.WithPrivateDirs(lctx, sockDir)
	}
	if loopback {
		// Python's adapter listens on ports chosen now, which it holds, and
		// only those are opened to it; Go's Delve picks its own, so it gets
		// every loopback port but Abhed's reserved range.
		if req.Language == debugrelay.Python {
			ports, err := sandbox.FreeLoopbackPorts(debugrelay.PinnedPorts)
			if err != nil {
				cancel()
				_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "the debug adapter could not start: no free loopback port: " + err.Error(), IsError: true}, 0)
				c.reply(msg.ID, nil, refusal(errRefused, "the debug adapter could not start: no free loopback port: %v", err))
				return
			}
			debugrelay.PinPorts(&spec, ports)
			lctx = sandbox.WithLoopbackPorts(lctx, ports...)
		} else {
			// Go: every loopback port but Abhed's reserved range, where every
			// Abhed listener on this Mac is, so no egress proxy is reachable.
			lctx = sandbox.WithLoopbackEphemeral(lctx)
		}
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
		if errors.Is(err, sandbox.ErrLoopbackUnderEgress) {
			c.reply(msg.ID, nil, debugRefusal(loopbackProblem()))
			return
		}
		c.reply(msg.ID, nil, refusal(errRefused, "the debug adapter could not start: %v", err))
		return
	}
	// The workspace's agreement is kept only once the session it was given
	// for has started: nothing is kept for a launch that could not run.
	if why != "" && forWorkspace {
		if err := setDebugConsent(parts.Session.Root, true); err != nil {
			warnf("the choice to debug %s on the process tier could not be kept: %v", parts.Session.Root, err)
		}
		s.record(agent.EvDebugSetup, agent.DebugSetup{Action: "process_tier_consent", Workspace: parts.Session.Root, CallID: callID})
	}
	d := &acpDebug{id: "dbg-" + acpID(), callID: callID, session: s.id, cancel: cancel, started: time.Now(), done: make(chan struct{}), dir: madeDir,
		sends: make(chan rpcMessage, debugQueue)}
	d.relay = debugrelay.New(stdin,
		spec.Launch,
		func(m json.RawMessage) {
			c.notification("_abhed/debug/message", map[string]any{"debugId": d.id, "message": m})
		},
		func(state, message string) {
			c.notification("_abhed/debug/status", map[string]any{"debugId": d.id, "state": state, "message": debugrelay.Bounded(redacted(message), 2000)})
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
	// Inserted under the session's lock, which its close takes before it
	// stops the session's debug sessions: either the close sees this one, or
	// this sees the close and ends the adapter it just started.
	s.mu.Lock()
	closed := s.closed
	if !closed {
		c.debugMu.Lock()
		if c.debugs == nil {
			c.debugs = map[string]*acpDebug{}
		}
		c.debugs[d.id] = d
		c.debugReserved--
		reserved = false
		c.debugMu.Unlock()
	}
	s.mu.Unlock()
	if closed {
		cancel()
		_ = cmd.Wait()
		sandbox.Release(cmd)
		_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "ended: the session closed as the debug session started", IsError: true}, 0)
		c.reply(msg.ID, nil, refusal(errRefused, "the session closed before the debug session could start"))
		return
	}
	go c.debugSends(d)
	launched := agent.DebugLaunched{DebugID: d.id, CallID: callID, Language: req.Language, Command: spec.Display, Tier: tier, BridgePinned: pinned, Loopback: loopback}
	if bridgePath != "" {
		launched.Bridge = bridgePath
	}
	s.record(agent.EvDebugLaunched, launched)
	madeDir = "" // debugRun removes it now
	go c.debugRun(s, d, cmd, stdout, &stderr, b.Isolation.Tier)
	c.reply(msg.ID, map[string]any{"debugId": d.id}, nil)
}

// debugRun carries the adapter's messages until it ends, then records how
// the session ended.
func (c *acpConn) debugRun(s *acpSession, d *acpDebug, cmd *exec.Cmd, stdout io.Reader, stderr *bytes.Buffer, tier string) {
	defer close(d.done)
	if d.dir != "" {
		defer func() { _ = os.RemoveAll(d.dir) }()
	}
	// An adapter whose output cannot be read (a header past its bound, a
	// message too large) is ended, not left blocked on a pipe nobody reads.
	pumpErr := d.relay.Pump(stdout)
	if pumpErr != nil {
		d.cancel()
	}
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
		reason = orDefault(d.reason, "stopped")
	}
	d.mu.Unlock()
	state := map[string]any{"debugId": d.id, "state": "ended"}
	if p := debugrelay.MissingAdapter(code, ""); p != nil {
		reason = "adapter-missing"
		state["state"], state["message"], state["problem"] = "failed", p.Message, p
	} else if pumpErr != nil && reason == "ended" {
		reason = "failed"
		state["state"], state["message"] = "failed", "the adapter's output could not be read, so the session was ended: "+debugrelay.Bounded(redacted(pumpErr.Error()), 500)
	} else if code != 0 && reason == "ended" {
		reason = "failed"
		state["state"], state["message"] = "failed", redacted(lastLines(stderr.String(), 20))
	}
	exit := code
	dropped := d.relay.Dropped()
	if dropped > 0 {
		state["dropped"] = dropped
	}
	s.record(agent.EvDebugStopped, agent.DebugStopped{DebugID: d.id, CallID: d.callID, Reason: reason, ExitCode: &exit,
		DurationMS: time.Since(d.started).Milliseconds(), Dropped: dropped})
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

// debugQueue bounds the sends one debug session holds while the adapter
// takes them.
const debugQueue = 256

// debugEnqueue takes a _abhed/debug/send in the order the connection read
// it: requests otherwise run on goroutines of their own and could reach the
// adapter out of order. It does not wait for the adapter.
func (c *acpConn) debugEnqueue(msg rpcMessage) {
	d, e := c.debugOf(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	d.qmu.Lock()
	defer d.qmu.Unlock()
	if d.qclosed {
		c.reply(msg.ID, nil, refusal(errParams, "the debug session has ended"))
		return
	}
	c.inflight.Add(1)
	select {
	case d.sends <- msg:
	default:
		c.inflight.Done()
		c.reply(msg.ID, nil, refusal(errBusy, "%d messages for this debug session are waiting for the adapter; wait for their replies", debugQueue))
	}
}

// debugSends passes a session's messages to the adapter one at a time.
// Once the session ends it closes the queue, under the lock every enqueue
// takes, and answers what was left in it, so no send goes unanswered.
func (c *acpConn) debugSends(d *acpDebug) {
	for {
		select {
		case msg := <-d.sends:
			c.debugSend(d, msg)
			c.inflight.Done()
		case <-d.done:
			d.qmu.Lock()
			d.qclosed = true
			d.qmu.Unlock()
			for {
				select {
				case msg := <-d.sends:
					c.reply(msg.ID, nil, refusal(errParams, "the debug session has ended"))
					c.inflight.Done()
				default:
					return
				}
			}
		}
	}
}

func (c *acpConn) debugSend(d *acpDebug, msg rpcMessage) {
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

// reserveDebug holds one of the connection's debug slots, or reports that
// none is free.
func (c *acpConn) reserveDebug() bool {
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	if len(c.debugs)+c.debugReserved >= maxDebugs {
		return false
	}
	c.debugReserved++
	return true
}

func (c *acpConn) releaseDebug() {
	c.debugMu.Lock()
	c.debugReserved--
	c.debugMu.Unlock()
}

// debugAsk puts a supervisor's ask about a debug launch to the person, as a
// permission request Studio shows like any other, and reports whether they
// allowed it. Only a person's click on Allow allows it.
func (c *acpConn) debugAsk(s *acpSession, callID, display string) agent.ManualAsk {
	return c.manualAsk(s, callID, "debug", display)
}

// manualAsk puts a supervisor ask about a person's debug or language-server start to them.
func (c *acpConn) manualAsk(s *acpSession, callID, kind, display string) agent.ManualAsk {
	return c.manualAskWith(s, callID, kind, display, "", nil)
}

// manualAskWith is manualAsk with, when always is set, a third option
// (allow_always, named alwaysName) whose choice sets *always: for an
// agreement kept beyond this action, which "Allow once" must not give.
func (c *acpConn) manualAskWith(s *acpSession, callID, kind, display, alwaysName string, always *bool) agent.ManualAsk {
	return c.manualAskFor(s, callID, "bash", "execute", kind, display, map[string]any{"command": display}, alwaysName, always)
}

// manualToolAsk is manualAsk for a person's call of another tool than bash,
// shown as that tool with its ACP kind and input, as a fetch is.
func (c *acpConn) manualToolAsk(s *acpSession, callID, tool, acpKind, display string, input map[string]any) agent.ManualAsk {
	return c.manualAskFor(s, callID, tool, acpKind, acpKind, display, input, "", nil)
}

// manualAskFor puts a person's own action to them as tool, of ACP kind
// acpKind, with input as its rawInput.
func (c *acpConn) manualAskFor(s *acpSession, callID, tool, acpKind, kind, display string, input map[string]any, alwaysName string, always *bool) agent.ManualAsk {
	return func(ctx context.Context, d policy.Result) (bool, error) {
		once, reject, keep := "once:"+callID, "reject:"+callID, "always:"+callID
		options := []map[string]any{{"optionId": once, "name": "Allow once", "kind": "allow_once"}}
		if always != nil {
			options = append(options, map[string]any{"optionId": keep, "name": alwaysName, "kind": "allow_always"})
		}
		options = append(options, map[string]any{"optionId": reject, "name": "Deny", "kind": "reject_once"})
		meta := map[string]any{"tool": tool, "step": d.Step, "reason": redacted(d.Reason), kind: true}
		title := ui.VisibleLine(kind + ": " + display)
		if ui.HasHidden(display) || anyHidden(d.Reason) {
			title += " (contains hidden or control characters)"
		}
		res, err := c.call(ctx, "session/request_permission", map[string]any{
			"sessionId": s.id,
			"toolCall": map[string]any{"toolCallId": callID, "title": title, "kind": acpKind, "status": "pending",
				"rawInput": input, "_meta": map[string]any{acpMetaKey: meta}},
			"options": options,
		})
		if err != nil {
			return false, err
		}
		var out struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if json.Unmarshal(res, &out) != nil || out.Outcome.Outcome != "selected" {
			return false, errors.New("not answered")
		}
		if always != nil && out.Outcome.OptionID == keep {
			*always = true
			return true, nil
		}
		return out.Outcome.OptionID == once, nil
	}
}

func (c *acpConn) debugStop(msg rpcMessage) {
	d, e := c.debugOf(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	d.stop("stopped")
	c.reply(msg.ID, map[string]any{}, nil)
}

// stop asks the adapter to end the program, and ends the adapter if it has
// not gone within a few seconds. The timer is armed first and the
// disconnect sent aside, so an adapter that has stopped reading cannot hold
// the stop, or the session's close, waiting on its input.
func (d *acpDebug) stop(reason string) {
	d.mu.Lock()
	already := d.stopped
	d.stopped, d.reason = true, reason
	d.mu.Unlock()
	if already {
		return
	}
	go func() {
		select {
		case <-d.done:
		case <-time.After(debugEndGrace):
			d.cancel()
		}
	}()
	go func() { _ = d.relay.Disconnect() }()
}

// killDebugs stops a session's debug sessions, or every one for "", as
// closed with it, and waits a moment for them to be recorded.
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
		d.stop("closed")
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

// sessionClosed reports whether the session has been closed.
func sessionClosed(s *acpSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
