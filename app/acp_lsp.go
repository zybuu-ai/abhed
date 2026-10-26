package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A person's language server for Studio (studio-acp-contract.md §7.9): the
// server runs in the session's sandbox, as a bash call would, and its
// messages are carried over the connection through lsprelay's filter. The
// engine initializes it; Studio sends everything after.

func init() {
	liveFeatures = append(liveFeatures, "lsp")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/lsp/start": (*acpConn).lspStart,
		"_abhed/lsp/send":  (*acpConn).lspEnqueue,
		"_abhed/lsp/stop":  (*acpConn).lspStop,
	})
}

// defaultLanguageServers run where sandbox.language_servers names none for
// a language: each from its language's toolchain extras.
var defaultLanguageServers = map[string][]string{
	"go":         {"gopls"},
	"python":     {"basedpyright-langserver", "--stdio"},
	"typescript": {"typescript-language-server", "--stdio"},
	"javascript": {"typescript-language-server", "--stdio"},
	"rust":       {"rust-analyzer"},
}

// defaultClientCapabilities are what the server is told an editor can do
// when Studio names none.
const defaultClientCapabilities = `{"workspace":{"applyEdit":true,"workspaceEdit":{"documentChanges":true},"configuration":true,"workspaceFolders":true,"executeCommand":{}},` +
	`"textDocument":{"synchronization":{"didSave":true},"completion":{"completionItem":{"snippetSupport":true}},"hover":{"contentFormat":["markdown","plaintext"]},` +
	`"signatureHelp":{},"definition":{"linkSupport":true},"references":{},"documentHighlight":{},"documentSymbol":{"hierarchicalDocumentSymbolSupport":true},` +
	`"codeAction":{},"formatting":{},"rename":{"prepareSupport":true},"publishDiagnostics":{"relatedInformation":true},"inlayHint":{},"semanticTokens":{"requests":{"full":true},"tokenTypes":[],"tokenModifiers":[],"formats":["relative"]}},` +
	`"window":{"workDoneProgress":true,"showDocument":{"support":true}}}`

// Language server bounds.
const (
	maxLSPServers  = 4
	lspInitWait    = 60 * time.Second
	lspLife        = 12 * time.Hour
	lspStopGrace   = 3 * time.Second
	lspMaxRestarts = 5
	exitNoServer   = 96
)

// lspCommand starts the server command in the session's sandbox; tests
// replace it, and lspTier, with their own.
var lspCommand = func(b tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
	return b.Sandbox(ctx, cwd, command)
}

var lspTier = debugTier

// lspBackoff is the wait before restart n (from 0) after a crash.
var lspBackoff = func(n int) time.Duration { return time.Second << n }

type acpLSP struct {
	id, language, session string
	argv                  []string
	caps                  json.RawMessage
	filter                *lsprelay.Filter

	mu       sync.Mutex
	out      chan []byte // the running process's input, written in order by one writer
	cancel   context.CancelFunc
	stopped  bool
	restarts int
	served   json.RawMessage // the server's capabilities
	requests int
	refused  int
	done     chan struct{} // closed when the server is gone for good
	// in holds Studio's messages in the order they were read, for one worker.
	in chan rpcMessage
}

// Queue bounds: Studio's messages waiting for the filter, and messages
// waiting for the server to read them; past the second the server is not
// reading, and is ended.
const (
	lspInQueue  = 256
	lspOutQueue = 1024
)

func (c *acpConn) lspStart(msg rpcMessage) {
	var p struct {
		Language     string          `json:"language"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil || p.Language == "" {
		c.reply(msg.ID, nil, refusal(errParams, "language is required"))
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
	argv := parts.Config.Sandbox.LanguageServers[p.Language]
	if len(argv) == 0 {
		argv = defaultLanguageServers[p.Language]
	}
	if len(argv) == 0 {
		c.reply(msg.ID, nil, refusal(errParams, "no language server for %q: name one in sandbox.language_servers", p.Language))
		return
	}
	tool, _ := parts.Loop.Tools.Get("bash")
	b, isBash := tool.(tools.Bash)
	if tier := lspTier(b); !isBash || b.Sandbox == nil || tier == "" || tier == string(sandbox.TierNone) {
		c.reply(msg.ID, nil, refusal(errRefused, "a language server runs only inside a sandbox, and this session's commands have none"))
		return
	}
	c.lspMu.Lock()
	for _, l := range c.lsps {
		if l.session == s.id && l.language == p.Language {
			c.lspMu.Unlock()
			l.mu.Lock()
			served := l.served
			l.mu.Unlock()
			c.reply(msg.ID, map[string]any{"lspId": l.id, "capabilities": served}, nil)
			return
		}
	}
	if len(c.lsps) >= maxLSPServers {
		c.lspMu.Unlock()
		c.reply(msg.ID, nil, refusal(errRefused, "%d language servers are running; stop one first", maxLSPServers))
		return
	}
	caps := p.Capabilities
	if len(caps) == 0 || string(caps) == "null" {
		caps = json.RawMessage(defaultClientCapabilities)
	}
	l := &acpLSP{id: "lsp-" + acpID(), language: p.Language, session: s.id, argv: argv, caps: caps,
		filter: &lsprelay.Filter{Roots: append([]string{parts.Session.Root}, s.roots()...), Language: p.Language},
		done:   make(chan struct{}), in: make(chan rpcMessage, lspInQueue)}
	if c.lsps == nil {
		c.lsps = map[string]*acpLSP{}
	}
	c.lsps[l.id] = l
	c.lspMu.Unlock()
	go c.lspWork(l)
	served, e := c.lspRun(s, l, b, 0)
	if e != nil {
		c.lspMu.Lock()
		delete(c.lsps, l.id)
		c.lspMu.Unlock()
		close(l.done)
		c.reply(msg.ID, nil, e)
		return
	}
	c.reply(msg.ID, map[string]any{"lspId": l.id, "capabilities": served}, nil)
}

// lspRun judges, starts and initializes the server, and keeps it running:
// a crash restarts it, after a backoff, up to lspMaxRestarts times.
func (c *acpConn) lspRun(s *acpSession, l *acpLSP, b tools.Bash, restart int) (json.RawMessage, *rpcError) {
	parts := s.parts
	loop := parts.Loop
	command := lsprelay.CommandLine(l.argv)
	callID := "u" + acpID()
	args, _ := json.Marshal(map[string]any{"command": command, "description": "language server for " + l.language + " in the session sandbox, run with " + lsprelay.Env})
	ctx, cancel := context.WithTimeout(context.Background(), lspLife)
	_, refused, err := loop.ManualAuthorizeReviewed(ctx, "bash", callID, args, c.manualAsk(s, callID, "lsp", command))
	if err != nil {
		cancel()
		return nil, refusal(errRecord, "the language server could not be recorded, so it was not started: %v", err)
	}
	if refused != nil {
		cancel()
		_ = loop.ManualObserve(callID, "bash", *refused, 0)
		return nil, refusal(errPolicy, "%s", refused.Content)
	}
	script := "command -v " + lsprelay.CommandLine(l.argv[:1]) + " >/dev/null 2>&1 || { echo 'abhed-lsp: " + l.argv[0] + " is not on PATH' >&2; exit " + strconv.Itoa(exitNoServer) + "; }; exec " + lsprelay.Env + command
	cmd := lspCommand(b, loop.LaunchContext(ctx, callID), parts.Session.Root, script)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, left: 32 << 10}
	err = errors.Join(cmd.Err, err1, err2)
	if err == nil {
		err = cmd.Start()
	}
	started := time.Now()
	if err != nil {
		cancel()
		_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "the language server could not start: " + err.Error(), IsError: true}, 0)
		return nil, refusal(errRefused, "the language server could not start: %v", err)
	}
	out := make(chan []byte, lspOutQueue)
	go lspWriter(stdin, out, cancel)
	l.mu.Lock()
	l.out, l.cancel, l.restarts = out, cancel, restart
	l.mu.Unlock()
	s.record(agent.EvLSPStarted, agent.LSPStarted{LSPID: l.id, CallID: callID, Language: l.language, Command: command, Tier: lspTier(b), Restart: restart})

	br := bufio.NewReaderSize(stdout, 64<<10)
	served, initErr := c.lspInitialize(l, br)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		if initErr == nil {
			c.lspPump(l, br)
		}
		werr := cmd.Wait()
		sandbox.Release(cmd)
		cancel()
		code := 0
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else if werr != nil {
			code = -1
		}
		l.mu.Lock()
		stopped, requests, refusedN := l.stopped, l.requests, l.refused
		l.requests, l.refused = 0, 0
		l.mu.Unlock()
		reason := "crashed"
		switch {
		case stopped:
			reason = "stopped"
		case initErr != nil || code == exitNoServer:
			reason = "failed"
		}
		exit := code
		s.record(agent.EvLSPStopped, agent.LSPStopped{LSPID: l.id, CallID: callID, Language: l.language, Reason: reason, ExitCode: &exit,
			Requests: requests, Refused: refusedN, DurationMS: time.Since(started).Milliseconds()})
		res := tools.Result{Content: fmt.Sprintf("language server %s: exit %d", reason, code), ExitCode: &exit, IsError: reason == "crashed" || reason == "failed"}
		if tail := lastLines(stderr.String(), 20); tail != "" {
			res.Content += "\n" + redacted(tail)
		}
		_ = loop.ManualObserve(callID, "bash", res, time.Since(started))
		if reason == "crashed" && restart < lspMaxRestarts {
			c.lspRestart(s, l, b, restart+1)
			return
		}
		if initErr == nil {
			c.lspEnd(l, reason)
		}
	}()
	if initErr != nil {
		<-ended
		if bytes.Contains(stderr.Bytes(), []byte("abhed-lsp:")) {
			return nil, &rpcError{Code: errRefused, Message: l.argv[0] + " is not installed in the sandbox",
				Data: map[string]any{"problem": map[string]any{"kind": "server-missing", "retry": true,
					"message": l.argv[0] + " is not on the sandbox's PATH, so " + l.language + " has no language server there",
					"steps":   lspMissingSteps(l.language, l.argv[0])}}}
		}
		return nil, refusal(errRefused, "the language server did not initialize: %v", initErr)
	}
	return served, nil
}

// lspMissingSteps are what to do when a language's server is not on PATH:
// install it with its language through the toolchain route, where Abhed
// installs one, else use an image or another server.
func lspMissingSteps(language, program string) []string {
	lang := language
	if l, ok := toolchain.Find(language); ok {
		lang = l.Name
	}
	var steps []string
	if e, ok := toolchain.LSPFor(lang); ok && e.Name == lspServerPackage(program) {
		steps = append(steps, "Install "+lang+" with its language server: accept the toolchain offer with the language server (_abhed/toolchain/install with lsp true), "+
			"or run `abhed toolchain install -lsp "+lang+"` in your own terminal (add `-platform linux` for the container tier)")
		steps = append(steps, "Or use a sandbox image that has "+program)
	} else {
		steps = append(steps, "Use a sandbox image that has "+program)
	}
	return append(steps, "Or name another server in sandbox.language_servers in ~/.abhed/config.json", "Start the language server again")
}

// lspServerPackage is the extra a server's program comes from.
func lspServerPackage(program string) string {
	if program == "basedpyright-langserver" {
		return "basedpyright"
	}
	return program
}

// lspRestart starts the server again after a crash, once its backoff has passed.
func (c *acpConn) lspRestart(s *acpSession, l *acpLSP, b tools.Bash, n int) {
	select {
	case <-time.After(lspBackoff(n - 1)):
	case <-c.goneCh():
		c.lspEnd(l, "closed")
		return
	}
	l.mu.Lock()
	stopped := l.stopped
	l.mu.Unlock()
	if stopped {
		c.lspEnd(l, "stopped")
		return
	}
	served, e := c.lspRun(s, l, b, n)
	if e != nil {
		c.notification("_abhed/lsp/status", map[string]any{"lspId": l.id, "state": "ended", "message": e.Message})
		c.lspEnd(l, "crashed")
		return
	}
	c.notification("_abhed/lsp/status", map[string]any{"lspId": l.id, "state": "restarted", "capabilities": served, "restart": n})
}

// lspEnd forgets the server and tells Studio it is gone.
func (c *acpConn) lspEnd(l *acpLSP, reason string) {
	c.lspMu.Lock()
	_, listed := c.lsps[l.id]
	delete(c.lsps, l.id)
	c.lspMu.Unlock()
	if !listed {
		return
	}
	close(l.done)
	c.notification("_abhed/lsp/status", map[string]any{"lspId": l.id, "state": "ended", "reason": reason})
}

// lspInitialize sends the engine's initialize and initialized, and returns
// the server's capabilities.
func (c *acpConn) lspInitialize(l *acpLSP, br *bufio.Reader) (json.RawMessage, error) {
	init, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "abhed-init", "method": "initialize",
		"params": lsprelay.InitParams(l.filter.Roots[0], l.caps)})
	if err := l.write(init); err != nil {
		return nil, err
	}
	type result struct {
		caps json.RawMessage
		err  error
	}
	got := make(chan result, 1)
	go func() {
		for {
			raw, err := lsprelay.Read(br)
			if err != nil {
				got <- result{err: err}
				return
			}
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result struct {
					Capabilities json.RawMessage `json:"capabilities"`
				} `json:"result"`
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			if string(m.ID) == `"abhed-init"` && m.Method == "" {
				if m.Error != nil {
					got <- result{err: fmt.Errorf("initialize: %s", m.Error)}
					return
				}
				got <- result{caps: m.Result.Capabilities}
				return
			}
			// Anything before the answer (a log line, a progress note) waits for no one.
		}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			l.kill()
			return nil, r.err
		}
		if err := l.write([]byte(`{"jsonrpc":"2.0","method":"initialized","params":{}}`)); err != nil {
			return nil, err
		}
		l.mu.Lock()
		l.served = r.caps
		l.mu.Unlock()
		return r.caps, nil
	case <-time.After(lspInitWait):
		l.kill()
		return nil, errors.New("the server did not answer initialize in time")
	}
}

// lspPump carries the server's messages to Studio through the filter.
func (c *acpConn) lspPump(l *acpLSP, br *bufio.Reader) {
	for {
		raw, err := lsprelay.Read(br)
		if err != nil {
			return
		}
		forward, reply := l.filter.FromServer(raw)
		if reply != nil {
			_ = l.write(reply)
		}
		if forward != nil {
			c.notification("_abhed/lsp/message", map[string]any{"lspId": l.id, "message": json.RawMessage(forward)})
		}
	}
}

func (l *acpLSP) write(body []byte) error {
	l.mu.Lock()
	out := l.out
	l.mu.Unlock()
	if out == nil {
		return errors.New("the language server is not running")
	}
	select {
	case out <- body:
		return nil
	default:
		// It has stopped reading what it is sent.
		l.kill()
		return errors.New("the language server is not reading its input; it was ended")
	}
}

// lspWriter writes a process's input in the order it was queued, and closes
// it at the nil the stop queues; a failed write ends the process.
func lspWriter(stdin io.WriteCloser, out <-chan []byte, cancel context.CancelFunc) {
	defer func() { _ = stdin.Close() }()
	for body := range out {
		if body == nil {
			return
		}
		if err := lsprelay.Write(stdin, body); err != nil {
			cancel()
			return
		}
	}
}

func (l *acpLSP) kill() {
	l.mu.Lock()
	cancel := l.cancel
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *acpConn) lspOf(params json.RawMessage) (*acpLSP, *rpcError) {
	var p struct {
		LSPID string `json:"lspId"`
	}
	_ = json.Unmarshal(params, &p)
	c.lspMu.Lock()
	l := c.lsps[p.LSPID]
	c.lspMu.Unlock()
	if l == nil {
		return nil, refusal(errParams, "no language server %q", p.LSPID)
	}
	return l, nil
}

// lspEnqueue takes Studio's _abhed/lsp/send as it is read, so a server gets
// Studio's messages in their order; the server's worker answers it.
func (c *acpConn) lspEnqueue(msg rpcMessage) {
	l, e := c.lspOf(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.inflight.Add(1)
	select {
	case l.in <- msg:
	default:
		c.inflight.Done()
		c.reply(msg.ID, nil, refusal(errBusy, "the language server has %d messages waiting; try again", lspInQueue))
	}
}

// lspWork passes Studio's queued messages to the server, one at a time.
func (c *acpConn) lspWork(l *acpLSP) {
	for {
		select {
		case msg := <-l.in:
			c.lspSend(l, msg)
			c.inflight.Done()
		case <-l.done:
			for {
				select {
				case msg := <-l.in:
					c.reply(msg.ID, nil, refusal(errParams, "no language server %q", l.id))
					c.inflight.Done()
				default:
					return
				}
			}
		}
	}
}

func (c *acpConn) lspSend(l *acpLSP, msg rpcMessage) {
	var p struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil || len(p.Message) == 0 {
		c.reply(msg.ID, nil, refusal(errParams, "message is required"))
		return
	}
	out, err := l.filter.FromEditor(p.Message)
	var r *lsprelay.Refusal
	if errors.As(err, &r) {
		l.mu.Lock()
		l.refused++
		l.mu.Unlock()
		if len(r.ID) > 0 && string(r.ID) != "null" {
			// The editor's request gets the server's kind of answer: an error.
			answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32600, "message": r.Reason}})
			c.notification("_abhed/lsp/message", map[string]any{"lspId": l.id, "message": json.RawMessage(answer)})
		}
		c.reply(msg.ID, map[string]any{"refused": r.Reason}, nil)
		return
	}
	if err := l.write(out); err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "%v", err))
		return
	}
	l.mu.Lock()
	l.requests++
	l.mu.Unlock()
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) lspStop(msg rpcMessage) {
	l, e := c.lspOf(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	l.stop()
	c.reply(msg.ID, map[string]any{}, nil)
}

// stop asks the server to shut down and exit, closes its input, and ends it
// if it has not gone within the grace.
func (l *acpLSP) stop() {
	l.mu.Lock()
	already := l.stopped
	l.stopped = true
	out := l.out
	l.mu.Unlock()
	if already {
		return
	}
	// The bound starts first: nothing below may hold the stop past it.
	go func() {
		select {
		case <-l.done:
		case <-time.After(lspStopGrace):
			l.kill()
		}
	}()
	_ = l.write([]byte(`{"jsonrpc":"2.0","id":"abhed-shutdown","method":"shutdown"}`))
	_ = l.write([]byte(`{"jsonrpc":"2.0","method":"exit"}`))
	if out != nil {
		select {
		case out <- nil: // the writer closes the input after what is queued
		default:
		}
	}
}

// killLSPs stops a session's language servers, or every one for "", and
// waits a moment for their ends to be recorded.
func (c *acpConn) killLSPs(sessionID string) {
	c.lspMu.Lock()
	var mine []*acpLSP
	for _, l := range c.lsps {
		if sessionID == "" || l.session == sessionID {
			mine = append(mine, l)
		}
	}
	c.lspMu.Unlock()
	for _, l := range mine {
		l.stop()
	}
	for _, l := range mine {
		select {
		case <-l.done:
		case <-time.After(5 * time.Second):
		}
	}
}
