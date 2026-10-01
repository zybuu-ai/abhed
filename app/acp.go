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
	"github.com/zybuu-ai/abhed/internal/embedded"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	abhed "github.com/zybuu-ai/abhed/sdk"
	"github.com/zybuu-ai/abhed/store/local"
)

// The Agent Client Protocol: an editor drives Abhed over stdio with JSON-RPC
// 2.0, one message per line. The editor's approval dialog becomes the
// Approver; it can answer an ask, never lift a deny, and the sandbox and the
// workspace boundary are what they would be from the terminal.
//
// Spec: https://agentclientprotocol.com — protocol version 1. Abhed Studio's
// extensions: docs/architecture/studio-acp-contract.md.

const acpProtocolVersion = 1

// acpMetaKey names Abhed's fields in a message's _meta, namespaced as the
// spec's extensibility section recommends.
const acpMetaKey = "zybuu.ai/abhed"

// acpLegacyMetaKey is the key engines up to 1.2.2 read; still accepted on input.
const acpLegacyMetaKey = "abhed"

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
	// CancelTasks stops every background task, for session/cancel.
	CancelTasks() int
	Close()
}

// heldAskWait bounds how long an ask made with no prompt turn open waits
// for one; then it is refused.
var heldAskWait = 30 * time.Minute

// reviewLinger is how long a review window stays open after its last answer.
var reviewLinger = 5 * time.Minute

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
	Data    any    `json:"data,omitempty"`
}

// The contract's error codes (docs/architecture/studio-acp-contract.md §1.3).
const (
	errNoMethod   = -32601 // this engine does not have it
	errParams     = -32602 // invalid params, or an unknown id
	errRefused    = -32000 // the engine refused; the message says why
	errPolicy     = -32001 // refused by managed configuration, trust or a rule
	errBusy       = -32002 // a prompt is running
	errRecord     = -32003 // the record is unavailable or failed verification
	errParseInput = -32700
)

func refusal(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type acpSession struct {
	id    string
	cwd   string
	agent acpAgent
	// parts are the SDK agent's insides; inner is false for a test's
	// stand-in, whose session answers what needs them with a refusal.
	parts embedded.Parts
	inner bool
	// undo holds the checkpoints of the files the agent changed.
	undo *agent.UndoLog
	// readOnly says why the session takes no prompt: its record failed verification.
	readOnly string
	cancel   context.CancelFunc
	mu       sync.Mutex
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
	// turn is the open prompt turn's context: an ask put to the editor is
	// bound to it, so none is left open once the turn that showed it ends.
	turn context.Context
	// wake is closed and replaced whenever what a waiting ask waits for
	// changes: a turn or a review window opening or closing.
	wake chan struct{}
	// window is the review window a person opened for held asks (§6.3).
	window *reviewWindow
	// waiting counts held asks by background task.
	waiting map[string]int
	// notes are what the record said about each background task.
	notes map[string]taskNote
	// thoughts is set once reasoning streamed as deltas, so its whole text
	// that follows is not sent again.
	thoughts bool
	// totalIn and totalOut are the session's tokens, for usage_update.
	totalIn, totalOut int
	// dirty are the files with unsaved changes in the editor, by real path.
	dirty map[string]bool
	// trustSHA is the workspace file's hash when the session opened.
	trustSHA string
	closed   bool
	// woken is set while a turn the session started itself runs, and
	// closed when it ends; a prompt waits for it.
	woken chan struct{}
}

// reviewWindow lets a background task's held asks reach the editor with no
// prompt open, at a person's request.
type reviewWindow struct {
	all         bool            // every task's asks
	tasks       map[string]bool // or these tasks'
	ctx         context.Context
	cancel      context.CancelFunc
	open        bool // new asks still go out through it
	outstanding int
	timer       *time.Timer
}

// beginTurn marks a prompt turn open: an ask held since the last one goes
// out inside it, and a review window stops taking new asks.
func (s *acpSession) beginTurn(ctx context.Context, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginTurnLocked(ctx, cancel)
}

// claimTurn opens a prompt turn unless one runs: it returns the woken turn
// to wait for, or busy for a prompt's.
func (s *acpSession) claimTurn(ctx context.Context, cancel context.CancelFunc) (wait <-chan struct{}, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.woken != nil:
		return s.woken, false
	case s.cancel != nil:
		return nil, true
	}
	s.beginTurnLocked(ctx, cancel)
	return nil, false
}

func (s *acpSession) beginTurnLocked(ctx context.Context, cancel context.CancelFunc) {
	s.cancel, s.turn = cancel, ctx
	s.ranIdless = nil // a result the last turn never recorded will not come
	if w := s.window; w != nil {
		// Its open asks stay bound to it; new ones go out inside the turn.
		w.open = false
		if w.outstanding == 0 {
			s.closeWindowLocked()
		}
	}
	s.pokeLocked()
}

func (s *acpSession) endTurn() {
	s.mu.Lock()
	s.cancel, s.turn = nil, nil
	s.mu.Unlock()
}

// askableLocked reports whether an ask from task may go to the editor now,
// and whether it goes out held, through a review window.
func (s *acpSession) askableLocked(task string) (ok, held bool) {
	if s.cancel != nil {
		return true, false
	}
	if w := s.window; w != nil && w.open && task != "" && w.covers(task) {
		return true, true
	}
	return false, false
}

func (s *acpSession) wakeLocked() chan struct{} {
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	return s.wake
}

func (s *acpSession) pokeLocked() {
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
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

	build acpBuild
	// openRecord opens the local record sessions are kept in; nil keeps them
	// in memory. recDir, recUser and recTenant are where and for whom.
	openRecord         func() (*local.Store, error)
	recOnce            sync.Once
	rec                *local.Store
	recErr             error
	recDir             string
	recUser, recTenant string

	sessMu   sync.Mutex
	sessions map[string]*acpSession
	// pending are the agent→client requests waiting for an answer, by id.
	pendMu  sync.Mutex
	pending map[int64]chan rpcMessage
	nextID  int64

	// subs are the event stream subscriptions, by id (§4.4).
	subMu sync.Mutex
	subs  map[string]*eventSub
	// terms are the sandboxed terminals, by id (§7.1).
	termMu sync.Mutex
	terms  map[string]*acpTerminal
}

// root is the context every agent and prompt runs under.
func (c *acpConn) root() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// acpCmd serves one editor for the life of the process.
func acpCmd(workspace string, build acpBuild, trust config.TrustChoice) int {
	stopper := cancelOnStop(stopExits)
	defer stopper.stop()
	c := &acpConn{out: os.Stdout, version: build.Version, build: build, base: workspace, trust: trust, ctx: stopper.ctx, busy: stopper.busy,
		sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	c.useRecord(workspace, trust)
	err := c.serve(os.Stdin)
	c.closeAll()
	c.closeRecord()
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
			c.reply(nil, nil, &rpcError{Code: errParseInput, Message: "parse error: " + err.Error()})
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

// sessionUpdate sends one session/update notification.
func (c *acpConn) sessionUpdate(sessionID string, u map[string]any) {
	c.notification("session/update", map[string]any{"sessionId": sessionID, "update": u})
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

// sessionFor reads a request's sessionId and finds it, or says it is unknown.
// A session of another connection or tenant is unknown here too (§2.8).
func (c *acpConn) sessionFor(params json.RawMessage) (*acpSession, *rpcError) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	s := c.session(p.SessionID)
	if s == nil {
		return nil, refusal(errParams, "unknown session")
	}
	return s, nil
}

// innerOf is the session's SDK parts, or the refusal for a session without them.
func innerOf(s *acpSession) (embedded.Parts, *rpcError) {
	if !s.inner {
		return embedded.Parts{}, refusal(errRefused, "this session cannot do that")
	}
	return s.parts, nil
}

// idle refuses while a prompt runs in s.
func idle(s *acpSession) *rpcError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return refusal(errBusy, "a prompt is running in this session; try again when it ends")
	}
	return nil
}

func (c *acpConn) closeAll() {
	c.sessMu.Lock()
	all := c.sessions
	c.sessions = map[string]*acpSession{}
	c.sessMu.Unlock()
	for _, s := range all {
		c.shut(s)
	}
	c.killTerminals("")
}

// shut ends a session's hold: its prompt and review window stop, its
// background tasks end as session_closed, and its record is let go.
func (c *acpConn) shut(s *acpSession) {
	// A prompt sets and clears cancel under s.mu while it runs.
	s.mu.Lock()
	cancel := s.cancel
	s.closed = true
	s.closeWindowLocked()
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.killTerminals(s.id)
	c.unsubscribeSession(s.id)
	s.agent.Close()
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
		// A review window closes too; its open asks are refused.
		s.closeWindowLocked()
		s.mu.Unlock()
		// Stop means stop: the background tasks too, with or without a turn open.
		go s.agent.CancelTasks()
	}
}

func (c *acpConn) request(msg rpcMessage) {
	h, ok := acpMethods[msg.Method]
	if !ok {
		c.reply(msg.ID, nil, refusal(errNoMethod, "method not found: %s", msg.Method))
		return
	}
	h(c, msg)
}

// acpMethods are the requests the engine answers; anything else is -32601.
var acpMethods = map[string]func(*acpConn, rpcMessage){
	"initialize":                (*acpConn).initialize,
	"authenticate":              func(c *acpConn, m rpcMessage) { c.reply(m.ID, map[string]any{}, nil) },
	"session/new":               (*acpConn).newSession,
	"session/prompt":            (*acpConn).prompt,
	"session/set_config_option": (*acpConn).setConfigOption,
	"session/set_model":         (*acpConn).setModel,
}

// handle adds the methods a section of the contract serves.
func handle(methods map[string]func(*acpConn, rpcMessage)) {
	for name, h := range methods {
		acpMethods[name] = h
	}
}

// trustReporter is an agent that can say what it decided about the
// workspace's configuration file; the SDK's agent is one.
type trustReporter interface {
	WorkspaceTrust() config.WorkspaceTrust
}

// sessionMeta reads the zybuu.ai/abhed block of a request's _meta, or the
// older abhed one. Its fields are decoded strictly: a field this engine does
// not know is refused, since none may widen what a session can do (§2.1).
func sessionMeta(raw map[string]json.RawMessage, into any) *rpcError {
	for _, key := range []string{acpMetaKey, acpLegacyMetaKey} {
		b, ok := raw[key]
		if !ok {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			return refusal(errParams, "_meta[%q]: %v", key, err)
		}
		return nil
	}
	return nil
}

// openOptions say how a session is opened: new, or continued from the record.
type openOptions struct {
	cwd    string
	trust  config.TrustChoice
	id     string
	resume bool
}

func (c *acpConn) newSession(msg rpcMessage) {
	var p struct {
		Cwd        string                     `json:"cwd"`
		MCPServers []json.RawMessage          `json:"mcpServers"`
		Meta       map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "session/new: %v", err))
		return
	}
	var m struct {
		// Trust "untrusted" takes only what tightens, whatever was recorded.
		Trust string `json:"trust"`
	}
	if e := sessionMeta(p.Meta, &m); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd = c.base
	}
	trust := c.trust
	switch m.Trust {
	case "":
	case string(config.TrustRefused):
		trust = config.TrustRefused
	default:
		// Trust is granted by the person, with abhed trust or the flag, never over the wire.
		c.reply(msg.ID, nil, refusal(errParams, `_meta[%q].trust may only be "untrusted"; `+
			"trust a workspace with `abhed trust grant` or -trust-workspace", acpMetaKey))
		return
	}
	s, e := c.openSession(openOptions{cwd: cwd, trust: trust})
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	res := c.sessionResult(s)
	// MCP servers come from the engine's configuration, under workspace trust;
	// those the client names are not started.
	if refused := mcpNames(p.MCPServers); len(refused) > 0 {
		metaOf(res)["mcpServersRefused"] = refused
	}
	cmds := commandsUpdate(s)
	c.reply(msg.ID, res, nil)
	c.sessionUpdate(s.id, cmds)
}

// mcpNames are the names of the MCP servers a client offered.
func mcpNames(servers []json.RawMessage) []string {
	var out []string
	for _, raw := range servers {
		var srv struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &srv)
		out = append(out, ui.VisibleLine(srv.Name))
	}
	return out
}

// metaOf is the zybuu.ai/abhed block of a result, made when absent.
func metaOf(res map[string]any) map[string]any {
	meta, _ := res["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		res["_meta"] = meta
	}
	block, _ := meta[acpMetaKey].(map[string]any)
	if block == nil {
		block = map[string]any{}
		meta[acpMetaKey] = block
	}
	return block
}

// openSession builds a session's agent, in the record when the connection
// keeps one, and adds it to the connection.
func (c *acpConn) openSession(o openOptions) (*acpSession, *rpcError) {
	s := &acpSession{id: o.id, cwd: o.cwd, always: map[string]bool{}}
	if s.id == "" {
		s.id = "s-" + acpID()
		if c.durable() {
			s.id = newConversationID()
		}
	}
	if e := c.buildAgent(s, o); e != nil {
		return nil, e
	}
	c.sessMu.Lock()
	c.sessions[s.id] = s
	c.sessMu.Unlock()
	return s, nil
}

// buildAgent makes s's agent and wires its undo log and guard.
func (c *acpConn) buildAgent(s *acpSession, o openOptions) *rpcError {
	opts := abhed.Options{
		Workspace: o.cwd, ConfigDir: o.cwd, Sandbox: true, WorkspaceTrust: o.trust, AllowDefaultModel: true,
		// The agent the terminal runs, subagents and configured tools included.
		ConfiguredTools: true,
		// The configuration's turn limit binds, as it does from the terminal.
		ConfiguredLimits: true,
		// Stdout is the protocol; what the tool set skipped goes to stderr.
		Warn: warnf,
		// Background tasks outlive a turn; a result that arrives between
		// prompts opens a turn of its own, streamed as session updates.
		Background: "auto",
		HostWake:   func(ids []string, run func(context.Context) (string, error)) bool { return c.wakeTurn(s, ids, run) },
		OnEvent:    func(ev abhed.Event) { c.forward(s, ev) },
		Approve: func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
			return c.askEditor(ctx, s, tool, args, d)
		},
		// The editor shows a next prompt in its input after a turn.
		Suggest: true,
	}
	// The editor's own files in the workspace are out of the agent's reach (§2.6).
	settings := embedded.Settings{Protect: protectedPaths(o.cwd), Surface: "acp"}
	if c.durable() {
		rec, err := c.record()
		if err != nil {
			return refusal(errRecord, "the record is unavailable: %v", err)
		}
		opts.Store = rec
		settings.ID, settings.Resume, settings.User = s.id, o.resume, c.recUser
	}
	a, err := newACPAgent(embedded.With(c.root(), settings), opts)
	if err != nil {
		if errors.Is(err, local.ErrHeldElsewhere) {
			return refusal(errRefused, "open in another Abhed process; fork it to go on here")
		}
		return refusal(errRefused, "%s", err.Error())
	}
	s.agent = a
	s.parts, s.inner = embedded.Of(a)
	if s.inner {
		s.undo = agent.NewUndoLog(s.parts.Session.RestoreFile, s.parts.Session.RemoveFile)
		loop := s.parts.Loop
		s.undo.Persist = checkpointSaverFor(func() *agent.Loop { return loop }, s.parts.Store)
		s.parts.Session.Checkpoint = s.undo.Record
		s.parts.Session.Guard = s.dirtyGuard
	}
	if r, ok := a.(trustReporter); ok {
		s.trustSHA = r.WorkspaceTrust().SHA256
	}
	return nil
}

// sessionResult is what session/new, load and resume reply with.
func (c *acpConn) sessionResult(s *acpSession) map[string]any {
	res := map[string]any{"sessionId": s.id}
	opts := []any{}
	if m, ok := s.agent.(modelSwitcher); ok {
		if models := m.Models(); len(models) > 0 {
			opts = append(opts, modelConfigOptions(models)...)
			res["models"] = legacyModelState(models)
		}
	}
	if modes := c.modeState(s); modes != nil {
		res["modes"] = modes
		opts = append(opts, modeConfigOption(modes))
	}
	if len(opts) > 0 {
		res["configOptions"] = opts
	}
	// The editor learns whether the workspace file applied, so it can ask the person.
	if r, ok := s.agent.(trustReporter); ok {
		metaOf(res)["workspaceTrust"] = r.WorkspaceTrust()
	}
	return res
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
		c.reply(msg.ID, nil, refusal(errParams, "unknown session"))
		return
	}
	if s.readOnly != "" {
		c.reply(msg.ID, nil, refusal(errRecord, "%s; fork it to go on", s.readOnly))
		return
	}
	if c.busy != nil {
		defer c.busy()()
	}
	ctx, cancel := context.WithCancel(c.root())
	// A turn the session woke for runs to its end, or session/cancel, first.
	for {
		wait, busy := s.claimTurn(ctx, cancel)
		if busy {
			cancel()
			c.reply(msg.ID, nil, refusal(errBusy, "a prompt is already running in this session; steer it or wait"))
			return
		}
		if wait == nil {
			break
		}
		<-wait
	}
	// The turn ends before its reply, so the next prompt never finds it open.
	var endOnce sync.Once
	end := func() { endOnce.Do(func() { cancel(); s.endTurn() }) }
	defer end()

	text := promptText(p.Prompt)
	// The workspace file changed since the session opened: it restarts under
	// the decision about the new bytes before this prompt runs (§5.6).
	if c.checkTrust(s) {
		c.restartForTrust(s)
	}
	s.undo.BeginTurn()
	var err error
	if cmdErr, handled := c.slashCommand(ctx, s, text); handled {
		err = cmdErr
	} else {
		_, err = s.agent.Run(ctx, text)
	}
	// Every session/update of the run goes out before the reply that ends it.
	flushed, cancelFlush := context.WithTimeout(context.Background(), flushWait)
	_ = s.agent.Flush(flushed)
	cancelFlush()
	stop, reason := stopReason(ctx, err)
	var ended *abhed.EndedError
	if stop == "refusal" && err != nil && !errors.As(err, &ended) {
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock("\n" + err.Error())})
	}
	res := map[string]any{"stopReason": stop}
	if reason != "" {
		metaOf(res)["reason"] = reason
	}
	end()
	c.reply(msg.ID, res, nil)
}

// stopReason maps how a run ended onto the spec's stop reasons, from the
// loop's terminal reason rather than the error's text (§3.1).
func stopReason(ctx context.Context, err error) (stop, reason string) {
	if err == nil {
		if ctx.Err() != nil {
			return "cancelled", string(agent.TermUserInterrupt)
		}
		return "end_turn", ""
	}
	var ended *abhed.EndedError
	if !errors.As(err, &ended) {
		if ctx.Err() != nil {
			return "cancelled", string(agent.TermUserInterrupt)
		}
		return "refusal", string(agent.TermError)
	}
	switch ended.Reason {
	case agent.TermCompleted:
		return "end_turn", ""
	case agent.TermMaxTurns, agent.TermWakeLimit:
		return "max_turn_requests", string(ended.Reason)
	case agent.TermUserInterrupt, agent.TermShutdown:
		return "cancelled", string(ended.Reason)
	case agent.TermMaxBudget:
		return "max_tokens", "budget"
	}
	return "refusal", string(ended.Reason)
}

func textBlock(t string) map[string]any { return map[string]any{"type": "text", "text": t} }

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
	task := agent.BackgroundTaskOf(ctx)
	held, window, refused, err := c.awaitAskable(ctx, s, task, callID)
	if refused || err != nil {
		return false, err
	}
	// Bound to the turn or the review window it is put in: an ask still open
	// when either ends is refused, not left waiting on a closed request.
	s.mu.Lock()
	bound := s.turn
	s.mu.Unlock()
	if held {
		bound = window.ctx
		defer c.windowAnswered(s, window)
	}
	askCtx := ctx
	if bound != nil {
		b, stop := context.WithCancel(ctx)
		defer stop()
		defer context.AfterFunc(bound, stop)()
		askCtx = b
	}
	// The tool_call goes out first, so the editor has the card this asks about.
	if s.agent != nil {
		flushed, cancel := context.WithTimeout(ctx, askFlushWait)
		_ = s.agent.Flush(flushed)
		cancel()
	}
	// The editor is shown the request as recorded (redacted when the session has
	// a redactor); outside a loop there is no record, only the call's own values.
	shown, reason, shownScope, via := args, d.Reason, scope, ""
	if rec, ok := agent.RequestedOf(ctx); ok {
		// A person cannot review input the record withheld, so it is not asked.
		if rec.Withheld {
			abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "the request was withheld from the record, so it cannot be shown for review"})
			c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(
				"\nA " + ui.VisibleLine(tool) + " call was refused without asking: its input was withheld from the record, so it cannot be shown for review.\n")})
			return false, nil
		}
		shown, reason, shownScope, via = rec.Args, rec.Reason, rec.Scope, rec.Via
	}
	offerAlways := scope != "" && shownScope != ""
	once, always, reject := "once:"+bind, "always:"+bind, "reject:"+bind
	options := []map[string]any{{"optionId": once, "name": "Allow once", "kind": "allow_once"}}
	if offerAlways {
		options = append(options, map[string]any{"optionId": always, "name": "Always allow " + ui.VisibleLine(shownScope), "kind": "allow_always"})
	}
	options = append(options, map[string]any{"optionId": reject, "name": "Deny", "kind": "reject_once"})
	meta := map[string]any{"tool": tool, "step": d.Step, "reason": reason, "destructive": destructive(tool, args, d),
		"rule": ruleOf(d)}
	if requestID != "" {
		meta["requestId"] = requestID
	}
	if via != "" {
		meta["via"] = via
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
	if offerAlways {
		meta["scope"] = shownScope
	}
	if held {
		meta["held"] = true
	}
	if task != "" {
		meta["taskId"] = task
	}
	toolCall := map[string]any{"toolCallId": callID, "title": title,
		"kind": toolKind(tool), "status": "pending", "rawInput": shown}
	if diff := s.askDiff(tool, shown); diff != nil {
		toolCall["content"], toolCall["locations"] = diff.content, diff.locations
		meta["diff"] = diff.meta
		if diff.hunksOnly {
			meta["hunksOnly"] = true
		}
	}
	toolCall["_meta"] = map[string]any{acpMetaKey: meta}
	res, err := c.call(askCtx, "session/request_permission", map[string]any{
		"sessionId": s.id, "toolCall": toolCall, "options": options,
	})
	if err != nil && ctx.Err() == nil && bound != nil && bound.Err() != nil {
		why := "the prompt turn ended before it was answered"
		if held {
			why = "the review was closed before it was answered"
		}
		abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: why})
		return false, nil
	}
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
	case out.Outcome.OptionID == always && offerAlways:
		s.mu.Lock()
		s.always[scope] = true
		s.mu.Unlock()
		abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.ByReviewer, Granted: scope})
		// Recorded so a later session/load restores it, where policy still allows.
		s.record(agent.EvApprovalScopeGranted, agent.ScopeGranted{Scope: shownScope, By: agent.ByUser})
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

// awaitAskable waits until an ask from task may go to the editor: a prompt
// turn is open, or a person opened a review window for it. Meanwhile the
// task's card says it is waiting. refused is set when no turn opened in time.
func (c *acpConn) awaitAskable(ctx context.Context, s *acpSession, task, callID string) (held bool, window *reviewWindow, refused bool, err error) {
	take := func() (bool, *reviewWindow, bool) {
		ok, held := s.askableLocked(task)
		if ok && held {
			s.window.outstanding++
			s.window.stopLinger()
			return true, s.window, true
		}
		return held, nil, ok
	}
	s.mu.Lock()
	held, window, ok := take()
	if ok {
		s.mu.Unlock()
		return held, window, false, nil
	}
	if s.waiting == nil {
		s.waiting = map[string]int{}
	}
	s.waiting[task]++
	s.mu.Unlock()
	settle := func() {
		s.mu.Lock()
		s.waiting[task]--
		s.mu.Unlock()
		c.taskChanged(s, task)
	}
	// Said on the background task's own card, which the editor has; the
	// ask's own card is drawn only when it is put.
	waitingOn := callID
	if task != "" {
		waitingOn = "bg-" + task
	}
	c.sessionUpdate(s.id, map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": waitingOn, "status": "pending",
		"content": []any{map[string]any{"type": "content", "content": textBlock("Waiting for your approval; send a message to review it.")}}})
	c.taskChanged(s, task)
	timer := time.NewTimer(heldAskWait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if held, window, ok = take(); ok {
			s.mu.Unlock()
			settle()
			return held, window, false, nil
		}
		w := s.wakeLocked()
		s.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			settle()
			return false, nil, true, ctx.Err()
		case <-timer.C:
			settle()
			abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySystem, Reason: "no prompt turn opened to ask"})
			return false, nil, true, nil
		}
	}
}

// ruleOf names what made the ask: the rule as written, or the built-in step.
func ruleOf(d abhed.Decision) string {
	if d.Rule != "" {
		return d.Rule
	}
	return "builtin:" + d.Step
}

// record writes one of the person's actions to the session's record.
func (s *acpSession) record(t agent.EventType, payload any) {
	if !s.inner {
		return
	}
	if _, err := s.parts.Loop.Recorder.Record(t, agent.ActorUser, agent.Trusted, payload); err != nil {
		warnf("the record did not take %s: %v", t, err)
	}
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
	case "bash", "shell_kill":
		return "execute"
	case "shell_output":
		return "read"
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
	if bg, _ := m["run_in_background"].(bool); bg && tool == "bash" {
		tool = "bash (background)"
	}
	for _, k := range []string{"command", "path", "pattern", "query", "shell_id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return ui.VisibleLine(tool + " " + v)
		}
	}
	return ui.VisibleLine(tool)
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
