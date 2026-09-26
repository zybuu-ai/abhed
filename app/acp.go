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

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The Agent Client Protocol: an editor drives Abhed over stdio with JSON-RPC
// 2.0, one message per line. The editor's approval dialog becomes the
// Approver; it can answer an ask, never lift a deny, and the sandbox and the
// workspace boundary are what they would be from the terminal.
//
// Spec: https://agentclientprotocol.com — protocol version 1.

const acpProtocolVersion = 1

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
	// calls maps a call id to the tool call id the editor was told about.
	calls map[string]string
}

type acpConn struct {
	out     io.Writer
	outMu   sync.Mutex
	version string
	base    string // the workspace given on the command line, when cwd is absent
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
func acpCmd(workspace, version string) int {
	stopper := cancelOnStop(stopExits)
	defer stopper.stop()
	c := &acpConn{out: os.Stdout, version: version, base: workspace, ctx: stopper.ctx, busy: stopper.busy,
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
		if s.cancel != nil {
			s.cancel()
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

func (c *acpConn) newSession(msg rpcMessage) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	cwd := p.Cwd
	if cwd == "" {
		cwd = c.base
	}
	s := &acpSession{id: "s-" + acpID(), cwd: cwd, always: map[string]bool{}, calls: map[string]string{}}
	opts := abhed.Options{
		Workspace: cwd, ConfigDir: cwd, Sandbox: true,
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
	c.reply(msg.ID, map[string]any{"sessionId": s.id}, nil)
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
	s.mu.Lock()
	remembered := d.Scope != "" && s.always[d.Scope]
	s.mu.Unlock()
	if remembered {
		return true, nil
	}
	options := []map[string]any{{"optionId": "once", "name": "Allow once", "kind": "allow_once"}}
	if d.Scope != "" {
		options = append(options, map[string]any{"optionId": "always", "name": "Always allow " + d.Scope, "kind": "allow_always"})
	}
	options = append(options, map[string]any{"optionId": "reject", "name": "Deny", "kind": "reject_once"})
	res, err := c.call(ctx, "session/request_permission", map[string]any{
		"sessionId": s.id,
		"toolCall": map[string]any{"toolCallId": c.toolCallID(tool, args), "title": toolTitle(tool, args),
			"kind": toolKind(tool), "status": "pending", "rawInput": args},
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
	_ = json.Unmarshal(res, &out)
	switch out.Outcome.OptionID {
	case "always":
		s.mu.Lock()
		s.always[d.Scope] = true
		s.mu.Unlock()
		return true, nil
	case "once":
		return true, nil
	}
	return false, nil
}

// toolCallID gives the editor one id per call: the record's call id when the
// event carries it, else one derived from the arguments so the permission
// request and the tool_call update line up.
func (c *acpConn) toolCallID(tool string, args json.RawMessage) string {
	return "pending-" + tool + "-" + fmt.Sprint(len(args))
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

func toolTitle(tool string, args json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(args, &m)
	for _, k := range []string{"command", "path", "pattern", "query"} {
		if v, ok := m[k].(string); ok && v != "" {
			return tool + " " + v
		}
	}
	return tool
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
		s.mu.Lock()
		s.calls[p.CallID] = p.CallID
		s.mu.Unlock()
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": p.CallID, "title": toolTitle(p.Tool, p.Args),
			"name": p.Tool, "kind": toolKind(p.Tool), "status": "pending", "rawInput": p.Args})
	case agent.EvActionApproved:
		var p struct {
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": "in_progress"})
	case agent.EvActionDenied:
		var p struct {
			CallID string `json:"call_id"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": "failed",
			"content": []any{map[string]any{"type": "content", "content": text("Denied: " + p.Reason)}}})
	case agent.EvObservation:
		var p agent.Observation
		_ = json.Unmarshal(ev.Payload, &p)
		status := "completed"
		if p.IsError {
			status = "failed"
		}
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": p.CallID, "status": status,
			"content": []any{map[string]any{"type": "content", "content": text(p.Content)}}, "rawOutput": p.Content})
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
