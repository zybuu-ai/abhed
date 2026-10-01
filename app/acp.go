package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The Agent Client Protocol: an editor drives Abhed over stdio with JSON-RPC
// 2.0, one message per line. The editor's approval dialog becomes the
// Approver; it can answer an ask, never lift a deny, and the sandbox and the
// workspace boundary are what they would be from the terminal.
//
// Spec: https://agentclientprotocol.com — protocol version 1.

const acpProtocolVersion = 1

// acpMetaKey names Abhed's fields in a message's _meta, namespaced as the
// spec's extensibility section recommends.
const acpMetaKey = "zybuu.ai/abhed"

// askFlushWait bounds the wait for a call's tool_call to reach the editor
// before the permission request that names it.
const askFlushWait = time.Second

// acpAgent is what the adapter needs from a session. *abhed.Agent is one; the
// conformance test supplies another so no model is needed to drive the wire.
type acpAgent interface {
	Run(ctx context.Context, prompt string) (string, error)
	Steer(text string)
	// Flush waits until every event of the run has been forwarded.
	Flush(ctx context.Context) error
	Close()
}

// newACPAgent builds a session. A variable so tests can replace it.
var newACPAgent = func(ctx context.Context, opts abhed.Options) (acpAgent, error) {
	return abhed.New(ctx, opts)
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpSession struct {
	id     string
	cwd    string
	agent  acpAgent
	cancel context.CancelFunc
	mu     sync.Mutex
	// always holds the "allow always" scopes the editor chose, so the same
	// kind of call is not asked again in this session.
	always map[string]bool
	// A call salvaged from prose has no id, so the editor knows it by its
	// action.requested id: the last one requested, and those approved and
	// awaiting a result, oldest first.
	lastIdless idlessCall
	ranIdless  []idlessCall
	// subAsks are the subagent asks shown as tool calls, by request id, so
	// their answer settles the card and no other subagent.action draws one.
	subAsks map[string]bool
}

// subagentCallID names a subagent's ask to the editor. A child's call ids are
// its own and may repeat the parent's, so its request id is used instead.
func subagentCallID(requestID string) string { return "subagent-" + requestID }

type idlessCall struct{ id, tool string }

type acpConn struct {
	out     io.Writer
	outMu   sync.Mutex
	version string
	base    string // the workspace given on the command line, when cwd is absent
	// trust is -trust-workspace, the default for every session's cwd.
	trust config.TrustChoice
	// ctx ends every agent and prompt on a stop signal, and busy holds the
	// exit that follows until a prompt has ended; nil for neither.
	ctx  context.Context
	busy func() func()

	sessMu   sync.Mutex
	sessions map[string]*acpSession
	// pending are the agent→client requests waiting for an answer, by id.
	pendMu  sync.Mutex
	pending map[int64]chan rpcMessage
	nextID  int64
}

// root is the context every agent and prompt runs under.
func (c *acpConn) root() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// acpCmd serves one editor for the life of the process.
func acpCmd(workspace, version string, trust config.TrustChoice) int {
	stopper := cancelOnStop(stopExits)
	defer stopper.stop()
	c := &acpConn{out: os.Stdout, version: version, base: workspace, trust: trust, ctx: stopper.ctx, busy: stopper.busy,
		sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	err := c.serve(os.Stdin)
	c.closeAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: acp: %v\n", err)
		return 1
	}
	return 0
}

func (c *acpConn) serve(in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			c.reply(nil, nil, &rpcError{-32700, "parse error: " + err.Error()})
			continue
		}
		switch {
		case msg.Method == "" && (msg.Result != nil || msg.Error != nil):
			c.answer(msg)
		case msg.ID == nil:
			c.notify(msg)
		default:
			// A prompt runs for as long as the model does; the loop must keep
			// reading for the cancel and the permission answers meanwhile.
			go c.request(msg)
		}
	}
	return sc.Err()
}

func (c *acpConn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	_, _ = c.out.Write(append(b, '\n'))
}

func (c *acpConn) reply(id json.RawMessage, result any, e *rpcError) {
	msg := rpcMessage{JSONRPC: "2.0", ID: id, Error: e}
	if e == nil {
		raw, _ := json.Marshal(result)
		msg.Result = raw
	}
	if msg.ID == nil {
		msg.ID = json.RawMessage("null")
	}
	c.send(msg)
}

func (c *acpConn) notification(method string, params any) {
	raw, _ := json.Marshal(params)
	c.send(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

// call sends an agent→client request and waits for the answer.
func (c *acpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.pendMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.pendMu.Unlock()
	raw, _ := json.Marshal(params)
	c.send(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: method, Params: raw})
	select {
	case <-ctx.Done():
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return nil, errors.New(msg.Error.Message)
		}
		return msg.Result, nil
	}
}

func (c *acpConn) answer(msg rpcMessage) {
	var id int64
	if json.Unmarshal(msg.ID, &id) != nil {
		return
	}
	c.pendMu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pendMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (c *acpConn) session(id string) *acpSession {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sessions[id]
}

func (c *acpConn) closeAll() {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	for _, s := range c.sessions {
		// A prompt sets and clears cancel under s.mu while it runs.
		s.mu.Lock()
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.agent.Close()
	}
	c.sessions = map[string]*acpSession{}
}

func (c *acpConn) notify(msg rpcMessage) {
	if msg.Method != "session/cancel" {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if s := c.session(p.SessionID); s != nil {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
	}
}

func (c *acpConn) request(msg rpcMessage) {
	switch msg.Method {
	case "initialize":
		c.reply(msg.ID, map[string]any{
			"protocolVersion": acpProtocolVersion,
			"agentCapabilities": map[string]any{
				"loadSession":        false,
				"promptCapabilities": map[string]any{"image": false, "audio": false, "embeddedContext": true},
				"mcpCapabilities":    map[string]any{"http": false, "sse": false},
			},
			"agentInfo":   map[string]any{"name": "abhed", "title": "Abhed", "version": c.version},
			"authMethods": []any{},
		}, nil)
	case "authenticate":
		c.reply(msg.ID, map[string]any{}, nil)
	case "session/new":
		c.newSession(msg)
	case "session/prompt":
		c.prompt(msg)
	case "session/load", "session/set_mode":
		c.reply(msg.ID, nil, &rpcError{-32601, msg.Method + " is not supported by this agent"})
	default:
		c.reply(msg.ID, nil, &rpcError{-32601, "method not found: " + msg.Method})
	}
}

// trustReporter is an agent that can say what it decided about the
// workspace's configuration file; the SDK's agent is one.
type trustReporter interface {
	WorkspaceTrust() config.WorkspaceTrust
}

func (c *acpConn) newSession(msg rpcMessage) {
	var p struct {
		Cwd  string `json:"cwd"`
		Meta struct {
			Abhed struct {
				// Trust "untrusted" takes only what tightens, whatever was recorded.
				Trust string `json:"trust"`
			} `json:"abhed"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	cwd := p.Cwd
	if cwd == "" {
		cwd = c.base
	}
	trust := c.trust
	switch p.Meta.Abhed.Trust {
	case "":
	case string(config.TrustRefused):
		trust = config.TrustRefused
	default:
		// Trust is granted by the person, with abhed trust or the flag, never over the wire.
		c.reply(msg.ID, nil, &rpcError{-32602, `_meta.abhed.trust may only be "untrusted"; ` +
			"trust a workspace with `abhed trust grant` or -trust-workspace"})
		return
	}
	s := &acpSession{id: "s-" + acpID(), cwd: cwd, always: map[string]bool{}}
	opts := abhed.Options{
		Workspace: cwd, ConfigDir: cwd, Sandbox: true, WorkspaceTrust: trust, AllowDefaultModel: true,
		// The agent the terminal runs, subagents and configured tools included.
		ConfiguredTools: true,
		// The configuration's turn limit binds, as it does from the terminal.
		ConfiguredLimits: true,
		// Stdout is the protocol; what the tool set skipped goes to stderr.
		Warn:    warnf,
		OnEvent: func(ev abhed.Event) { c.forward(s, ev) },
		Approve: func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
			return c.askEditor(ctx, s, tool, args, d)
		},
	}
	a, err := newACPAgent(c.root(), opts)
	if err != nil {
		c.reply(msg.ID, nil, &rpcError{-32000, err.Error()})
		return
	}
	s.agent = a
	c.sessMu.Lock()
	c.sessions[s.id] = s
	c.sessMu.Unlock()
	res := map[string]any{"sessionId": s.id}
	// The editor learns whether the workspace file applied, so it can ask the person.
	if r, ok := a.(trustReporter); ok {
		res["_meta"] = map[string]any{"abhed": map[string]any{"workspaceTrust": r.WorkspaceTrust()}}
	}
	c.reply(msg.ID, res, nil)
}

// promptText flattens the prompt's content blocks. Text is taken as it is;
// embedded resources become a labelled block the model can read.
func promptText(blocks []json.RawMessage) string {
	var b strings.Builder
	for _, raw := range blocks {
		var blk struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			URI      string `json:"uri"`
			Name     string `json:"name"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		}
		if json.Unmarshal(raw, &blk) != nil {
			continue
		}
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
		case "resource":
			fmt.Fprintf(&b, "\n\nAttached %s:\n```\n%s\n```", blk.Resource.URI, blk.Resource.Text)
		case "resource_link":
			fmt.Fprintf(&b, "\n\n(See %s%s)", blk.Name, map[bool]string{true: " at " + blk.URI, false: ""}[blk.URI != ""])
		}
	}
	return strings.TrimSpace(b.String())
}

func (c *acpConn) prompt(msg rpcMessage) {
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s := c.session(p.SessionID)
	if s == nil {
		c.reply(msg.ID, nil, &rpcError{-32602, "unknown session"})
		return
	}
	if c.busy != nil {
		defer c.busy()()
	}
	ctx, cancel := context.WithCancel(c.root())
	s.mu.Lock()
	s.cancel = cancel
	s.ranIdless = nil // a result the last turn never recorded will not come
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()

	_, err := s.agent.Run(ctx, promptText(p.Prompt))
	// Every session/update of the run goes out before the reply that ends it.
	flushed, cancelFlush := context.WithTimeout(context.Background(), flushWait)
	_ = s.agent.Flush(flushed)
	cancelFlush()
	stop := "end_turn"
	switch {
	case ctx.Err() != nil:
		stop = "cancelled"
	case err != nil && strings.Contains(err.Error(), "turn"):
		stop = "max_turn_requests"
	case err != nil:
		stop = "refusal"
		c.notification("session/update", map[string]any{"sessionId": s.id, "update": map[string]any{
			"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "\n" + err.Error()}}})
	}
	c.reply(msg.ID, map[string]any{"stopReason": stop}, nil)
}

// askEditor puts an ask decision to the editor as a permission request.
func (c *acpConn) askEditor(ctx context.Context, s *acpSession, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
	scope := d.Offer()
	s.mu.Lock()
	remembered := scope != "" && s.always[scope]
	s.mu.Unlock()
	if remembered {
		abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySessionScope, Scope: scope})
		return true, nil
	}
	// The request names the call its tool_call update names, and its options
	// carry the engine's request id so an answer meant for another ask is refused.
	callID, requestID := abhed.CallIDOf(ctx), abhed.RequestIDOf(ctx)
	sub := agent.SubagentOf(ctx)
	if sub != "" && requestID != "" {
		callID = subagentCallID(requestID) // as forward names the subagent.ask
	}
	if callID == "" {
		callID = requestID // as forward names a call without an id
	}
	if callID == "" {
		callID = "ask-" + acpID()
	}
	bind := requestID
	if bind == "" {
		bind = callID
	}
	// The tool_call goes out first, so the editor has the card this asks about.
	if s.agent != nil {
		flushed, cancel := context.WithTimeout(ctx, askFlushWait)
		_ = s.agent.Flush(flushed)
		cancel()
	}
	// The editor is shown the request as recorded (redacted when the session has
	// a redactor); outside a loop there is no record, only the call's own values.
	shown, reason, shownScope := args, d.Reason, scope
	if rec, ok := agent.RequestedOf(ctx); ok {
		// A person cannot review input the record withheld, so it is not asked.
		if rec.Withheld {
			abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "the request was withheld from the record, so it cannot be shown for review"})
			c.notification("session/update", map[string]any{"sessionId": s.id, "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text",
					"text": "\nA " + tool + " call was refused without asking: its input was withheld from the record, so it cannot be shown for review.\n"}}})
			return false, nil
		}
		shown, reason, shownScope = rec.Args, rec.Reason, rec.Scope
	}
	once, always, reject := "once:"+bind, "always:"+bind, "reject:"+bind
	options := []map[string]any{{"optionId": once, "name": "Allow once", "kind": "allow_once"}}
	if scope != "" && shownScope != "" {
		options = append(options, map[string]any{"optionId": always, "name": "Always allow " + ui.VisibleLine(shownScope), "kind": "allow_always"})
	}
	options = append(options, map[string]any{"optionId": reject, "name": "Deny", "kind": "reject_once"})
	meta := map[string]any{"tool": tool, "step": d.Step, "reason": reason, "destructive": destructive(tool, args, d)}
	if requestID != "" {
		meta["requestId"] = requestID
	}
	raw := rawToolTitle(tool, shown)
	if sub != "" {
		meta["subagent"] = sub
		raw = "subagent " + sub + ": " + raw
	}
	title := ui.VisibleLine(raw)
	// The note covers the whole call: every argument, the reason, scope and asker.
	if ui.HasHidden(raw) || ui.ArgsHidden(args) || ui.ArgsHidden(shown) || anyHidden(reason, scope, shownScope, sub, agent.PipelineOf(ctx)) {
		title += " (contains hidden or control characters)"
	}
	if scope != "" && shownScope != "" {
		meta["scope"] = shownScope
	}
	res, err := c.call(ctx, "session/request_permission", map[string]any{
		"sessionId": s.id,
		"toolCall": map[string]any{"toolCallId": callID, "title": title,
			"kind": toolKind(tool), "status": "pending", "rawInput": shown, "_meta": map[string]any{acpMetaKey: meta}},
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
	readable := json.Unmarshal(res, &out) == nil
	switch {
	case readable && out.Outcome.Outcome == "selected":
	case readable && out.Outcome.Outcome == "cancelled":
		abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "the editor cancelled the request"})
		return false, nil
	default:
		abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "the editor's answer could not be read"})
		return false, nil
	}
	switch {
	case out.Outcome.OptionID == always && scope != "" && shownScope != "":
		s.mu.Lock()
		s.always[scope] = true
		s.mu.Unlock()
		abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.ByReviewer, Granted: scope})
		return true, nil
	case out.Outcome.OptionID == once:
		return true, nil
	case out.Outcome.OptionID == reject:
		return false, nil
	}
	// Includes an "always" that was withheld, as it is for every step but default.
	abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "the editor's answer named an option not offered for this call"})
	return false, nil
}

// destructive reports a call with no undo, whichever step asked about it: a
// hook or ask rule may ask first about a command the destructive step would.
func destructive(tool string, args json.RawMessage, d abhed.Decision) bool {
	if d.Step == "destructive" {
		return true
	}
	if tool != "bash" {
		return false
	}
	var a struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &a) != nil {
		return false
	}
	_, yes := tools.IsDestructive(a.Command)
	return yes
}

func toolKind(tool string) string {
	switch tool {
	case "read", "glob":
		return "read"
	case "grep", "recall":
		return "search"
	case "write", "edit":
		return "edit"
	case "bash":
		return "execute"
	case "todo":
		return "think"
	}
	return "other"
}

// toolTitle is text an editor shows as the card's heading, so control and
// format characters are written out rather than left to hide part of the call.
func toolTitle(tool string, args json.RawMessage) string {
	return ui.VisibleLine(rawToolTitle(tool, args))
}

func rawToolTitle(tool string, args json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(args, &m)
	for _, k := range []string{"command", "path", "pattern", "query"} {
		if v, ok := m[k].(string); ok && v != "" {
			return tool + " " + v
		}
	}
	return tool
}

// takeIdless names the result of an approved call without an id: the oldest
// such call of that tool still awaiting one.
func (s *acpSession) takeIdless(tool string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.ranIdless {
		if c.tool == tool {
			s.ranIdless = append(s.ranIdless[:i], s.ranIdless[i+1:]...)
			return c.id
		}
	}
	return ""
}

// forward maps the record's events onto session/update notifications.
func (c *acpConn) forward(s *acpSession, ev abhed.Event) {
	update := func(u map[string]any) {
		c.notification("session/update", map[string]any{"sessionId": s.id, "update": u})
	}
	text := func(t string) map[string]any { return map[string]any{"type": "text", "text": t} }
	switch ev.Type {
	case agent.EvAgentDelta:
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Text != "" {
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": text(p.Text)})
		}
	case agent.EvAgentReasoning:
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Text != "" {
			update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": text(p.Text)})
		}
	case agent.EvActionRequested:
		var p agent.ActionRequested
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			p.CallID = ev.ID
			s.mu.Lock()
			s.lastIdless = idlessCall{id: ev.ID, tool: p.Tool}
			s.mu.Unlock()
		}
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": p.CallID, "title": toolTitle(p.Tool, p.Args),
			"kind": toolKind(p.Tool), "status": "pending", "rawInput": p.Args, "_meta": map[string]any{acpMetaKey: map[string]any{"tool": p.Tool}}})
	case agent.EvActionApproved:
		var p struct {
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			// Calls are approved one at a time, right after their request.
			s.mu.Lock()
			p.CallID = s.lastIdless.id
			s.ranIdless = append(s.ranIdless, s.lastIdless)
			s.mu.Unlock()
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": "in_progress"})
	case agent.EvActionDenied:
		var p struct {
			CallID string `json:"call_id"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			s.mu.Lock()
			p.CallID = s.lastIdless.id
			s.mu.Unlock()
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": "failed",
			"content": []any{map[string]any{"type": "content", "content": text("Denied: " + p.Reason)}}})
	case agent.EvObservation:
		var p agent.Observation
		_ = json.Unmarshal(ev.Payload, &p)
		if p.CallID == "" {
			p.CallID = s.takeIdless(p.Tool)
		}
		status := "completed"
		if p.IsError {
			status = "failed"
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": status,
			"content": []any{map[string]any{"type": "content", "content": text(p.Content)}}, "rawOutput": p.Content})
	case agent.EvSubagentAsk:
		// A subagent's call waiting on the editor: its card goes out before the ask.
		var p agent.SubagentAsk
		if json.Unmarshal(ev.Payload, &p) != nil || p.RequestID == "" {
			break
		}
		s.mu.Lock()
		if s.subAsks == nil {
			s.subAsks = map[string]bool{}
		}
		s.subAsks[p.RequestID] = true
		s.mu.Unlock()
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": subagentCallID(p.RequestID),
			"title": "subagent " + ui.VisibleLine(p.Subagent) + ": " + toolTitle(p.Tool, p.Args), "kind": toolKind(p.Tool),
			"status": "pending", "rawInput": p.Args,
			"_meta": map[string]any{acpMetaKey: map[string]any{"tool": p.Tool, "subagent": p.Subagent}}})
	case agent.EvSubagentAction:
		var p agent.SubagentAction
		if json.Unmarshal(ev.Payload, &p) != nil || p.RequestID == "" {
			break
		}
		s.mu.Lock()
		shown := s.subAsks[p.RequestID]
		delete(s.subAsks, p.RequestID)
		s.mu.Unlock()
		if !shown {
			break
		}
		// The call's result is in the subagent's record; the editor learns the answer.
		status, note := "completed", "Allowed; the subagent ran it."
		if p.Decision != "allowed" {
			status, note = "failed", "Denied: "+p.Reason
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": subagentCallID(p.RequestID),
			"status": status, "content": []any{map[string]any{"type": "content", "content": text(note)}}})
	case agent.EvTodoUpdated:
		var p agent.TodoList
		_ = json.Unmarshal(ev.Payload, &p)
		entries := make([]map[string]any, 0, len(p.Items))
		for _, it := range p.Items {
			st := "pending"
			switch it.Status {
			case "in_progress":
				st = "in_progress"
			case "done":
				st = "completed"
			}
			entries = append(entries, map[string]any{"content": it.Text, "priority": "medium", "status": st})
		}
		update(map[string]any{"sessionUpdate": "plan", "entries": entries})
	case agent.EvModelCall:
		var p agent.ModelCall
		_ = json.Unmarshal(ev.Payload, &p)
		if p.ContextWindow > 0 {
			update(map[string]any{"sessionUpdate": "usage_update", "used": p.TokensIn, "size": p.ContextWindow})
		}
	}
}

func acpID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func anyHidden(fields ...string) bool {
	for _, f := range fields {
		if ui.HasHidden(f) {
			return true
		}
	}
	return false
}
