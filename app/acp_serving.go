package app

import (
	"encoding/json"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/embedded"
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
	var found []string
	for _, u := range servingURL.FindAllString(text[:cut+1], -1) {
		if w.sent == nil {
			w.sent = map[string]bool{}
		}
		if len(w.sent) < 16 && !w.sent[u] {
			w.sent[u] = true
			found = append(found, u)
		}
	}
	w.mu.Unlock()
	for _, u := range found {
		c.notification("_abhed/terminal/serving", map[string]any{"sessionId": t.session.id, "terminalId": t.id, "url": u})
	}
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
		note += " Allow it from Studio (Let Commands Listen), then start the server again, bound to 127.0.0.1 (in a new terminal, for an interactive one)."
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
// person agreed for its workspace under sandbox.listen ask.
func (s *acpSession) loadListen() {
	if !s.inner || s.parts.Session == nil || s.parts.Set == nil || s.parts.Set.SetListen == nil {
		return
	}
	switch s.parts.Config.Sandbox.Listen {
	case "", "ask":
	default:
		return
	}
	if at := listenAgreed(s.parts.Session.Root); at != nil && s.parts.Set.SetListen(true) {
		if _, err := s.parts.Loop.Recorder.Record(agent.EvPermissionChanged, agent.ActorSystem, agent.Trusted,
			agent.PermissionChanged{Op: "add", List: "allow", Rule: listenRule, Scope: "workspace", By: agent.ByUser}); err != nil {
			warnf("the record did not take %s: %v", agent.EvPermissionChanged, err)
		}
	}
}
