package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/embedded"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/termline"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A dev server started in Studio's terminal prints where it listens. Where
// commands may listen (sandbox.listen, the macOS process tier), Studio is
// sent _abhed/terminal/serving with that address, once, to offer opening it
// in the person's browser; where they may not, a server refused its port
// gets _abhed/terminal/listen_refused, and _abhed/sandbox/listen is the
// person's agreement for the workspace.

// servingWatch is one terminal's watch: the tail of its output, for an
// address split across chunks, and the addresses already sent.
type servingWatch struct {
	mu      sync.Mutex
	tail    string
	sent    map[string]bool
	refused bool
	venv    bool
	// typed is set once the person typed abhed_venv: only then does the
	// line it prints count, so a program cannot make the header claim it.
	typed bool
}

// typedVenv notes the person typed abhed_venv at the shell.
func (w *servingWatch) typedVenv() {
	w.mu.Lock()
	w.typed = true
	w.mu.Unlock()
}

// servingURL is a loopback address a server prints, such as Vite's
// "Local: http://localhost:5173/".
var servingURL = regexp.MustCompile(`\bhttps?://(?:localhost|127\.0\.0\.1|\[::1\]):[0-9]{2,5}(?:/[A-Za-z0-9._~/-]*)?`)

// servesLoopback reports whether a server a command starts on loopback can
// be opened from the Mac: on no sandbox, or where the sandbox lets commands
// listen (sandbox.listen allow, or the person agreed for the workspace).
func servesLoopback(iso tools.Isolation, parts embedded.Parts) bool {
	if iso.Tier == "none" {
		return true
	}
	return parts.Set != nil && parts.Set.Listens != nil && parts.Set.Listens()
}

// listenRefused is a server's error when the sandbox refused it a port:
// Node's listen EPERM, Go's bind error, Python's from a bind.
var listenRefused = regexp.MustCompile(`(?i)\blisten (EPERM|EACCES)\b|\bbind: (operation not permitted|permission denied)|(?s)\.(bind|listen)\(.{0,400}PermissionError: \[Errno 1\]`)

// listenOffer says whether the person can let commands listen here, and
// why not.
func listenOffer(parts embedded.Parts, tier string) (bool, string) {
	switch {
	case runtime.GOOS != "darwin" || tier != "process":
		return false, "only the macOS process tier lets a server in the sandbox be reached from the host"
	case parts.Config.Sandbox.Listen == "off":
		return false, "sandbox.listen is off"
	case parts.Set == nil || parts.Set.SetListen == nil:
		return false, "this session has no sandbox to change"
	}
	return true, ""
}

// watchServing looks at a chunk of t's output for the address a server
// listens on, or, where commands may not listen, for a server refused one.
func (c *acpConn) watchServing(t *acpTerminal, chunk []byte) {
	w := &t.serving
	w.mu.Lock()
	text := w.tail + termline.PlainText(chunk)
	if len(text) > 2048 {
		text = text[len(text)-2048:]
	}
	inUse := w.typed && !w.venv && strings.Contains(text, venvInUseMark)
	if inUse {
		w.venv = true
		c.notification("_abhed/terminal/venv", map[string]any{"sessionId": t.session.id, "terminalId": t.id, "inUse": true})
	}
	if !t.serves {
		refused := !w.refused && listenRefused.MatchString(text)
		if refused {
			w.refused, w.tail = true, ""
		} else {
			w.tail = text
		}
		w.mu.Unlock()
		if refused {
			c.offerListen(t)
		}
		return
	}
	// The last partial line waits for the rest of the address.
	cut := strings.LastIndexAny(text, "\r\n")
	w.tail = text[cut+1:]
	done := text[:cut+1]
	var found []string
	for _, u := range servingURL.FindAllString(done, -1) {
		if w.sent == nil {
			w.sent = map[string]bool{}
		}
		if len(w.sent) < 16 && !w.sent[u] {
			w.sent[u] = true
			found = append(found, u)
		}
	}
	outside := !w.refused && listenRefused.MatchString(done)
	if outside {
		w.refused = true
	}
	w.mu.Unlock()
	ports := t.listenPorts()
	for _, u := range found {
		// Only a listen port that a program of the person's did not hold when the
		// command started, and that one program alone listens on, can be a
		// sandboxed server's.
		if port := urlPort(u); t.tier != "none" && (!slices.Contains(ports, port) || t.heldAtStart(port) || sandbox.ListenersOn(port) > 1) {
			continue
		}
		c.notification("_abhed/terminal/serving", map[string]any{"sessionId": t.session.id, "terminalId": t.id, "url": u})
	}
	if outside && t.tier != "none" {
		c.show(t, []byte("\r\nabhed: commands here may listen only on TCP ports "+portList(ports)+
			" (sandbox.listen_ports), and not on one a program of yours already listens on; start the server on one of them.\r\n"))
	}
}

// venvInUseMark is in the line abhed_venv prints once the venv is in use.
const venvInUseMark = ": its python and pip come first;"

// urlPort is a serving URL's port, or 0.
func urlPort(u string) int {
	rest := u[strings.Index(u, "://")+3:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	p, _ := strconv.Atoi(rest[strings.LastIndexByte(rest, ':')+1:])
	return p
}

// portList says ports briefly, runs as ranges: 3000-3009, 4200.
func portList(ports []int) string {
	var parts []string
	for i := 0; i < len(ports); {
		j := i
		for j+1 < len(ports) && ports[j+1] == ports[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", ports[i], ports[j]))
		} else {
			parts = append(parts, strconv.Itoa(ports[i]))
		}
		i = j + 1
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

// heldAtStart reports whether a program other than the sandbox's commands
// held port when its last listening command started.
func (t *acpTerminal) heldAtStart(port int) bool {
	parts, e := innerOf(t.session)
	return e != nil || parts.Set == nil || parts.Set.HeldAtStart == nil || parts.Set.HeldAtStart(port)
}

// listenPorts are the ports a server in t's sandbox may listen on.
func (t *acpTerminal) listenPorts() []int {
	if parts, e := innerOf(t.session); e == nil && parts.Set != nil && parts.Set.ListenPorts != nil {
		return parts.Set.ListenPorts()
	}
	return nil
}

// offerListen tells the terminal, once, that the sandbox refused a server
// its port, and sends Studio the offer to let commands listen.
func (c *acpConn) offerListen(t *acpTerminal) {
	canAllow, why := false, "this session has no sandbox to change"
	if parts, e := innerOf(t.session); e == nil {
		canAllow, why = listenOffer(parts, t.tier)
	}
	note := "the sandbox does not let commands listen for connections in this workspace."
	if canAllow {
		note += " Allow it from Studio (Let Commands Listen): every command in every session of this workspace, the agent's too, may then listen on TCP ports " +
			portList(t.listenPorts()) + ". A command can then take one of those ports before a program of yours starts there and receive its connections; Abhed warns you but does not stop the command. Then start the server again on one of them (in a new terminal, for an interactive one)."
	} else {
		note += " It cannot be allowed here: " + why + "."
	}
	c.show(t, []byte("\r\nabhed: "+note+"\r\n"))
	c.notification("_abhed/terminal/listen_refused", map[string]any{"sessionId": t.session.id, "terminalId": t.id,
		"canAllow": canAllow, "why": why, "message": note})
}

func init() {
	liveFeatures = append(liveFeatures, "listen")
	handle(map[string]func(*acpConn, rpcMessage){"_abhed/sandbox/listen": (*acpConn).sandboxListen})
}

// sandboxListen lets the workspace's commands listen on loopback, or no
// longer, on the person's word: kept for the workspace, applied to this
// process's open sessions of it at once and to every later session.
func (c *acpConn) sandboxListen(msg rpcMessage) {
	var p struct {
		Allow *bool `json:"allow"`
	}
	_ = json.Unmarshal(msg.Params, &p)
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
	if p.Allow == nil {
		c.reply(msg.ID, nil, refusal(errParams, "allow is true or false"))
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, refusal(errPolicy, "changing what commands may listen on is refused inside an agent's command (%s)", why))
		return
	}
	if !*p.Allow && parts.Config.Sandbox.Listen == "allow" {
		c.reply(msg.ID, nil, refusal(errPolicy, "sandbox.listen is allow in the configuration, so it cannot be taken back here"))
		return
	}
	if *p.Allow {
		ok, why := listenOffer(parts, "process")
		if ok && !parts.Set.SetListen(true) {
			ok, why = false, "only the macOS process tier lets a server in the sandbox be reached from the host"
		}
		if !ok {
			c.reply(msg.ID, nil, refusal(errPolicy, "commands cannot be let listen here: %s", why))
			return
		}
	}
	if parts.Config.Sandbox.Listen != "allow" {
		if err := setListenAgreed(parts.Session.Root, *p.Allow); err != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "the choice could not be kept: %v", err))
			return
		}
	}
	root := grantsKey(parts.Session.Root)
	for _, other := range c.openSessions() {
		if other.inner && other.parts.Session != nil && other.parts.Set != nil && other.parts.Set.SetListen != nil &&
			grantsKey(other.parts.Session.Root) == root && other.parts.Config.Sandbox.Listen != "allow" {
			other.parts.Set.SetListen(*p.Allow)
			if *p.Allow {
				other.watchListening(c)
			}
		}
	}
	op := "add"
	if !*p.Allow {
		op = "remove"
	}
	s.record(agent.EvPermissionChanged, agent.PermissionChanged{Op: op, List: "allow", Rule: listenRule, Scope: "workspace", By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"listens": parts.Set.Listens()}, nil)
}

// listenRule names the agreement in the record.
const listenRule = "sandbox(listen loopback)"

// loadListen lets a new or continued session's commands listen where the
// person agreed for its workspace under sandbox.listen ask, and watches
// what listening needs under ask or allow.
func (s *acpSession) loadListen(c *acpConn) {
	if !s.inner || s.parts.Session == nil || s.parts.Set == nil || s.parts.Set.SetListen == nil {
		return
	}
	switch s.parts.Config.Sandbox.Listen {
	case "", "ask":
		if at := listenAgreed(s.parts.Session.Root); at != nil && s.parts.Set.SetListen(true) {
			if _, err := s.parts.Loop.Recorder.Record(agent.EvPermissionChanged, agent.ActorSystem, agent.Trusted,
				agent.PermissionChanged{Op: "add", List: "allow", Rule: listenRule, Scope: "workspace", By: agent.ByUser}); err != nil {
				warnf("the record did not take %s: %v", agent.EvPermissionChanged, err)
			}
		}
		s.watchListening(c)
	case "allow":
		s.watchListening(c)
	}
}

// watchListening gives the session's sandbox the agreement to look up again,
// as another process may take it back, and tells the person, and the
// record, when more than one program listens on one of its listen ports, or
// when that can no longer be seen. Abhed tells; it never stops a command.
func (s *acpSession) watchListening(c *acpConn) {
	if s.parts.Set == nil || s.parts.Set.ListenWatch == nil || s.parts.Session == nil {
		return
	}
	root := s.parts.Session.Root
	var still func() bool
	if s.parts.Config.Sandbox.Listen != "allow" {
		var mu sync.Mutex
		var at time.Time
		var kept bool
		still = func() bool {
			mu.Lock()
			defer mu.Unlock()
			if time.Since(at) > 2*time.Second {
				kept, at = listenAgreed(root) != nil, time.Now()
			}
			return kept
		}
	}
	s.parts.Set.ListenWatch(still, func(e sandbox.PortContested) {
		msg := fmt.Sprintf("more than one program now listens on port %d, one of this workspace's listen ports: a command in the sandbox may be taking connections meant for a program of yours there. Abhed does not stop it: stop the command yourself, or turn listening off.", e.Port)
		warnf("%s", msg)
		if _, err := s.parts.Loop.Recorder.Record(agent.EvSandboxPortContested, agent.ActorSystem, agent.Trusted,
			agent.SandboxPortContested{Port: e.Port, Listeners: e.Listeners}); err != nil {
			warnf("the record did not take %s: %v", agent.EvSandboxPortContested, err)
		}
		c.notification("_abhed/sandbox/port_contested", map[string]any{"sessionId": s.id, "port": e.Port, "message": msg})
	}, func(why string) {
		msg := "Abhed can no longer see a port of yours contested by a sandboxed command: " + why + "."
		warnf("%s", msg)
		c.notification("_abhed/sandbox/listen_watch_lost", map[string]any{"sessionId": s.id, "message": msg})
	})
}
