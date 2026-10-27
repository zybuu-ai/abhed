package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	abhed "github.com/zybuu-ai/abhed/sdk"
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
	exitNoServer   = lsprelay.ExitNoServer
)

// lspTSWait bounds the wait for the start script to name the TypeScript
// beside typescript-language-server; tests shorten it.
var lspTSWait = 15 * time.Second

// lspCommand starts the server command in the session's sandbox; tests
// replace it, and lspTier, with their own.
var lspCommand = func(b tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
	return b.Sandbox(ctx, cwd, command)
}

var lspTier = debugTier

// lspToolchainRoot is Abhed's toolchain cache when the sandbox mounts it
// read-only (sandbox.toolchains not off): a TypeScript there is taken
// though it is under the home folder. Otherwise it is "" and nothing is
// exempt.
var lspToolchainRoot = sandboxconfig.ToolchainCache

// lspBackoff is the wait before restart n (from 0) after a crash.
var lspBackoff = func(n int) time.Duration { return time.Second << n }

type acpLSP struct {
	id, language, session string
	argv                  []string
	caps                  json.RawMessage
	filter                *lsprelay.Filter

	mu       sync.Mutex
	out      chan []byte // the running process's input, written in order by one writer; nil between processes
	outBytes *atomic.Int64
	cancel   context.CancelFunc
	stopped  bool
	reason   string // how a stop ended it: stopped, or closed with its session
	// restarting is set between a crash and the next process's start.
	restarting bool
	restarts   int
	served     json.RawMessage // the server's capabilities
	requests   int
	refused    int
	done       chan struct{} // closed when the server is gone for good
	// ready is closed once the first start has initialized the server, or
	// failed (startErr); a second start for the same language waits on it.
	ready    chan struct{}
	startErr *rpcError
	// in holds Studio's messages in the order they were read, for one
	// worker; inBytes is what they hold.
	in      chan rpcMessage
	inBytes atomic.Int64
}

// Queue bounds: Studio's messages waiting for the filter, and messages
// waiting for the server to read them, by count and by bytes; past the
// second the server is not reading, and is ended.
const (
	lspInQueue  = 256
	lspOutQueue = 1024
	lspInBytes  = 32 << 20
	lspOutBytes = 32 << 20
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
			// A second start waits for the first to initialize, or fail.
			select {
			case <-l.ready:
			case <-c.goneCh():
				c.reply(msg.ID, nil, refusal(errRefused, "the connection is closing"))
				return
			}
			l.mu.Lock()
			served, startErr := l.served, l.startErr
			l.mu.Unlock()
			if startErr != nil {
				c.reply(msg.ID, nil, startErr)
				return
			}
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
		filter: &lsprelay.Filter{Roots: append([]string{parts.Session.Root}, s.roots()...), Language: p.Language,
			Methods: lsprelay.ServerMethods[p.Language]},
		done: make(chan struct{}), ready: make(chan struct{}), in: make(chan rpcMessage, lspInQueue)}
	if c.lsps == nil {
		c.lsps = map[string]*acpLSP{}
	}
	c.lsps[l.id] = l
	c.lspMu.Unlock()
	go c.lspWork(l)
	served, e := c.lspRun(s, l, b, 0)
	l.mu.Lock()
	l.startErr = e
	l.mu.Unlock()
	close(l.ready)
	if reason, stopped := l.stopReason(); e != nil && stopped {
		c.notification("_abhed/lsp/status", map[string]any{"lspId": l.id, "state": "ended", "reason": reason})
	}
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
	args, _ := json.Marshal(map[string]any{"command": command, "description": "language server for " + l.language + " in the session sandbox, run with " +
		strings.TrimSpace(lsprelay.Env) + "; " + lspRuns(l.language)})
	ctx, cancel := context.WithTimeout(context.Background(), lspLife)
	// A server that is not on the PATH commands get (the toolchains' folders
	// first) cannot start: the person is offered the install, and nothing is
	// put to policy or the supervisor for a command that cannot run.
	if find := lspProgramFinder(loop.Tools); find != nil && !strings.ContainsAny(l.argv[0], "/\\") && !find(l.argv[0]) {
		cancel()
		return nil, lspMissing(l)
	}
	// A workspace rust-analyzer.toml overrides the engine's settings, so the
	// person is asked, told what it can do.
	// A walk that stopped at a bound asks too: a file may have gone unseen.
	// The watch is made before the check, so a file written between them
	// is still seen.
	var why string
	var raWatch *lsprelay.RustAnalyzerWatch
	if l.language == "rust" {
		raWatch = lsprelay.NewRustAnalyzerWatch(l.filter.Roots, nil)
		raFiles, incomplete := s.raConfigs.Get(l.filter.Roots, raStamp(loop))
		raWatch.Add(raFiles)
		why = lsprelay.RustAnalyzerConfigWhy(raFiles, incomplete)
	}
	_, refused, err := loop.ManualAuthorizeAsking(ctx, "bash", callID, args, c.manualAsk(s, callID, "lsp", command), why)
	if err != nil {
		cancel()
		return nil, refusal(errRecord, "the language server could not be recorded, so it was not started: %v", err)
	}
	if refused != nil {
		cancel()
		_ = loop.ManualObserve(callID, "bash", *refused, 0)
		return nil, lspRefused(l, refused.Content)
	}
	// A stop while the start was being decided (a rust-analyzer.toml written
	// during the question, or the person's own stop) holds: nothing starts.
	if reason, stopped := l.stopReason(); stopped {
		cancel()
		_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "the language server was not started: it was stopped (" + reason + ") before it started", IsError: true}, 0)
		s.record(agent.EvLSPStopped, agent.LSPStopped{LSPID: l.id, CallID: callID, Language: l.language, Reason: reason})
		return nil, lspStoppedRefusal(reason)
	}
	cmd := lspCommand(b, loop.LaunchContext(ctx, callID), parts.Session.Root, lsprelay.Script(l.argv, lspToolchainRoot(parts.Config)))
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	var stderr bytes.Buffer
	tsLine := make(chan string, 1)
	errPipe, err3 := cmd.StderrPipe()
	err = errors.Join(cmd.Err, err1, err2, err3)
	if err == nil {
		err = cmd.Start()
	}
	started := time.Now()
	if err != nil {
		cancel()
		_ = loop.ManualObserve(callID, "bash", tools.Result{Content: "the language server could not start: " + err.Error(), IsError: true}, 0)
		return nil, refusal(errRefused, "the language server could not start: %v", err)
	}
	// stderr is read here, so its end says the process has gone.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(&lsprelay.MarkerWriter{W: &limitedWriter{w: &stderr, left: 32 << 10}, Marker: lsprelay.TSServerMarker, Found: tsLine}, errPipe)
	}()
	out := make(chan []byte, lspOutQueue)
	outBytes := &atomic.Int64{}
	go lspWriter(stdin, out, outBytes, cancel)
	// A stop that came after the check above and before l.cancel was set
	// found no process to end; it is applied here, under the same lock.
	l.mu.Lock()
	l.out, l.outBytes, l.cancel, l.restarts, l.restarting = out, outBytes, cancel, restart, false
	stoppedEarly := l.stopped
	l.mu.Unlock()
	s.record(agent.EvLSPStarted, agent.LSPStarted{LSPID: l.id, CallID: callID, Language: l.language, Command: command, Tier: lspTier(b), Restart: restart})

	// typescript-language-server is given the TypeScript beside it, which
	// the script names, so it never runs the workspace's own.
	// Without it the server is never initialized: it would take the
	// workspace's own. No name in time, the process gone first, or one where
	// a command could have put it (a session folder, or the home folder
	// outside Abhed's toolchain cache) ends the start.
	var tsserver string
	var initErr error
	if stoppedEarly {
		cancel()
		initErr = errLSPStopped
	}
	if initErr == nil && filepath.Base(l.argv[0]) == "typescript-language-server" {
		select {
		case tsserver = <-tsLine:
		case <-stderrDone:
		case <-time.After(lspTSWait):
		}
		home, _ := os.UserHomeDir()
		switch {
		case tsserver == "":
			initErr = errNoTSServer
		case lsprelay.TSServerRefused(tsserver, l.filter.Roots, home, lspToolchainRoot(parts.Config)):
			initErr = errTSServerInWorkspace
		}
		if initErr != nil {
			cancel()
		}
	}
	options, sections := lsprelay.SafeSettings(l.language, tsserver)
	l.filter.Settings = sections
	br := bufio.NewReaderSize(stdout, 64<<10)
	var served json.RawMessage
	if initErr == nil {
		served, initErr = c.lspInitialize(l, br, options)
	}
	ended := make(chan struct{})
	if initErr == nil && l.language == "rust" {
		go c.lspWatchConfig(s, l, raWatch, lsprelay.RustAnalyzerWatchEvery, ended)
	}
	go func() {
		defer close(ended)
		if initErr == nil {
			c.lspPump(l, br)
		}
		<-stderrDone
		werr := cmd.Wait()
		sandbox.Release(cmd)
		cancel()
		// This process's input is closed, so its writer ends; between
		// processes a send is refused as restarting, never queued for no one.
		l.mu.Lock()
		if l.out == out {
			close(out)
			l.out = nil
		}
		l.restarting = !l.stopped && initErr == nil && restart < lspMaxRestarts
		l.mu.Unlock()
		code := 0
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else if werr != nil {
			code = -1
		}
		l.mu.Lock()
		stopped, stopReason, requests, refusedN := l.stopped, l.reason, l.requests, l.refused
		l.requests, l.refused = 0, 0
		l.mu.Unlock()
		reason := "crashed"
		switch {
		case stopped:
			reason = orDefault(stopReason, "stopped")
		case initErr != nil || code == exitNoServer || code == lsprelay.ExitNoTypeScript:
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
		if errors.Is(initErr, errLSPStopped) {
			reason, _ := l.stopReason()
			return nil, lspStoppedRefusal(reason)
		}
		if errors.Is(initErr, errTSServerInWorkspace) {
			return nil, &rpcError{Code: errRefused, Message: "the only TypeScript beside typescript-language-server is where a command could have put it, which Abhed does not run",
				Data: map[string]any{"problem": map[string]any{"kind": "server-missing", "retry": true,
					"message": "typescript-language-server's TypeScript was found under a session folder, or under a home or temporary folder outside Abhed's toolchain cache, where a command could have put it",
					"steps": []string{"Install typescript-language-server with its TypeScript through Abhed: abhed toolchain install -lsp node, or use a sandbox image that has both",
						"Start the language server again"}}}}
		}
		if errors.Is(initErr, errNoTSServer) || bytes.Contains(stderr.Bytes(), []byte("abhed-lsp: no TypeScript")) {
			return nil, &rpcError{Code: errRefused, Message: "no TypeScript is installed beside typescript-language-server in the sandbox",
				Data: map[string]any{"problem": map[string]any{"kind": "server-missing", "retry": true,
					"message": "typescript-language-server needs the TypeScript installed with it; Abhed does not let it run the workspace's own node_modules/typescript",
					"steps": []string{"Install TypeScript beside the server in the sandbox (npm install -g typescript typescript-language-server), or use an image that has both",
						"Start the language server again"}}}}
		}
		if bytes.Contains(stderr.Bytes(), []byte("abhed-lsp:")) {
			return nil, lspMissing(l)
		}
		why := initErr.Error()
		if tail := lastLines(stderr.String(), 8); tail != "" {
			why += "; it said: " + redacted(tail)
		}
		return nil, refusal(errRefused, "the language server did not initialize: %s", why)
	}
	return served, nil
}

// lspConfigChanged is the reason a rust-analyzer is stopped when a
// rust-analyzer.toml appears or changes while it runs: its settings may no
// longer be the engine's, and the next start asks again.
const lspConfigChanged = "workspace-config-changed"

// lspWatchConfig stops rust-analyzer when its watch sees a
// rust-analyzer.toml appear, change or go, until the process ends.
func (c *acpConn) lspWatchConfig(s *acpSession, l *acpLSP, w *lsprelay.RustAnalyzerWatch, every time.Duration, ended <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ended:
			return
		case <-t.C:
			if w.Changed() {
				s.raConfigs.Invalidate()
				l.stop(lspConfigChanged)
				return
			}
		}
	}
}

// raStamp is what a cached rust-analyzer.toml check holds under: the
// session record's last event, which moves with every action.
func raStamp(loop *agent.Loop) string {
	if loop == nil {
		return lsprelay.RecordStamp(nil)
	}
	return lsprelay.RecordStamp(loop.Recorder)
}

// lspWorkspaceConfig stops the session's rust-analyzer servers: a
// rust-analyzer.toml is being written, by the agent or reported by Studio.
func (c *acpConn) lspWorkspaceConfig(sessionID string) {
	c.lspMu.Lock()
	var mine []*acpLSP
	for _, l := range c.lsps {
		if l.session == sessionID && l.language == "rust" {
			mine = append(mine, l)
		}
	}
	c.lspMu.Unlock()
	for _, l := range mine {
		l.stop(lspConfigChanged)
	}
}

// lspNoteAction keeps the session's rust-analyzer.toml check current with
// what the agent does: any action may write to the workspace, so the cached
// walk is dropped, and a write or edit of a rust-analyzer.toml stops
// rust-analyzer before it can read it.
func (c *acpConn) lspNoteAction(s *acpSession, ev abhed.Event) {
	// The agent's toolchain tool installed or removed something.
	if ev.Type == agent.EvToolchainInstall {
		var a agent.ToolchainAction
		if json.Unmarshal(ev.Payload, &a) == nil && a.By != agent.ByUser {
			c.toolchainChanged(s, a)
		}
		return
	}
	if ev.Type == agent.EvSubagentAction {
		s.raConfigs.Invalidate()
		var p agent.SubagentAction
		if json.Unmarshal(ev.Payload, &p) == nil && (p.Tool == "write" || p.Tool == "edit") && lsprelay.IsRustAnalyzerConfig(p.Subject) {
			c.lspWorkspaceConfig(s.id)
		}
		return
	}
	if ev.Type != agent.EvActionRequested {
		return
	}
	s.raConfigs.Invalidate()
	var p agent.ActionRequested
	if json.Unmarshal(ev.Payload, &p) != nil || (p.Tool != "write" && p.Tool != "edit") {
		return
	}
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(p.Args, &a) == nil && lsprelay.IsRustAnalyzerConfig(a.Path) {
		c.lspWorkspaceConfig(s.id)
	}
}

// stopReason is why the server was stopped, and whether it was.
func (l *acpLSP) stopReason() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return orDefault(l.reason, "stopped"), l.stopped
}

// errLSPStopped ends a start that a stop overtook.
var errLSPStopped = errors.New("the language server was stopped before it started")

func lspStoppedRefusal(reason string) *rpcError {
	why := "it was stopped"
	if reason == lspConfigChanged {
		why = "a rust-analyzer.toml was written or changed while the start was decided; start it again to be asked"
	}
	return refusal(errRefused, "the language server was not started: %s", why)
}

// The typescript-language-server start's failures before initialize.
var (
	errNoTSServer          = errors.New("typescript-language-server's start did not name the TypeScript beside it")
	errTSServerInWorkspace = errors.New("the TypeScript beside typescript-language-server is under a session folder, or under the home folder outside Abhed's toolchain cache")
)

// lspProgramFinder is toolset.ProgramFinder; tests replace it.
var lspProgramFinder = toolset.ProgramFinder

// lspMissing is the server-missing problem for l: its program is not on the
// sandbox's PATH, with the toolchain install first among the steps.
func lspMissing(l *acpLSP) *rpcError {
	prob := map[string]any{"kind": "server-missing", "retry": true,
		"message": "No " + l.language + " language server (" + l.argv[0] + ") is installed where this session's commands run",
		"steps":   lspMissingSteps(l.language, l.argv[0])}
	if lang, ok := lspInstallable(l.language, l.argv[0]); ok {
		// One click: _abhed/toolchain/status then install, with lsp true.
		prob["install"] = map[string]any{"language": lang, "lsp": true}
	}
	return &rpcError{Code: errRefused, Message: l.argv[0] + " is not installed in the sandbox", Data: map[string]any{"problem": prob}}
}

// lspInstallable is the toolchain language that installs program as
// language's server, when Abhed installs it.
func lspInstallable(language, program string) (string, bool) {
	lang := language
	if l, ok := toolchain.Find(language); ok {
		lang = l.Name
	}
	e, ok := toolchain.LSPFor(lang)
	return lang, ok && e.Name == lspServerPackage(program)
}

// lspRefused is a refused start as a problem Studio shows, with the reason
// and what can be done, not only an error message.
func lspRefused(l *acpLSP, why string) *rpcError {
	steps := []string{"Abhed did not start " + l.argv[0] + ": " + why}
	if strings.Contains(why, "unknown-program") {
		steps = append(steps, lspMissingSteps(l.language, l.argv[0])...)
	} else {
		steps = append(steps, "If this server should run, ask whoever manages Abhed's policy here, or name another server in sandbox.language_servers in ~/.abhed/config.json",
			"Start the language server again")
	}
	return &rpcError{Code: errPolicy, Message: why,
		Data: map[string]any{"problem": map[string]any{"kind": "start-refused", "retry": true,
			"message": "The language server for " + l.language + " was not started: " + why, "steps": steps}}}
}

// lspMissingSteps are what to do when a language's server is not on PATH:
// install it with its language through the toolchain route, where Abhed
// installs one, else use an image or another server.
func lspMissingSteps(language, program string) []string {
	var steps []string
	if lang, ok := lspInstallable(language, program); ok {
		steps = append(steps, "Install "+lang+" with its language server: choose Install, or run `abhed toolchain install -lsp "+lang+
			"` in a terminal (add `-platform linux` for the container tier)")
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
		l.mu.Lock()
		reason := orDefault(l.reason, "stopped")
		l.mu.Unlock()
		c.lspEnd(l, reason)
		return
	}
	served, e := c.lspRun(s, l, b, n)
	if e != nil {
		if reason, stopped := l.stopReason(); stopped {
			c.lspEnd(l, reason)
			return
		}
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
func (c *acpConn) lspInitialize(l *acpLSP, br *bufio.Reader, options json.RawMessage) (json.RawMessage, error) {
	init, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "abhed-init", "method": "initialize",
		"params": lsprelay.InitParams(l.filter.Roots[0], l.caps, options)})
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

// errLSPRestarting is a send while a crashed server is being started again.
var errLSPRestarting = errors.New("the language server is restarting; send again once its status is restarted")

// write queues body for the running process. The channel is sent on and
// closed only under l.mu, so a send never meets a closed channel.
func (l *acpLSP) write(body []byte) error {
	l.mu.Lock()
	out, restarting := l.out, l.restarting
	if out == nil {
		l.mu.Unlock()
		if restarting {
			return errLSPRestarting
		}
		return errors.New("the language server is not running")
	}
	full := l.outBytes.Load()+int64(len(body)) > lspOutBytes
	if !full {
		select {
		case out <- body:
			l.outBytes.Add(int64(len(body)))
		default:
			full = true
		}
	}
	l.mu.Unlock()
	if full {
		// It has stopped reading what it is sent.
		l.kill()
		return errors.New("the language server is not reading its input; it was ended")
	}
	return nil
}

// lspWriter writes a process's input in the order it was queued, until the
// channel closes or the nil a stop queues; a failed write ends the process.
func lspWriter(stdin io.WriteCloser, out <-chan []byte, queued *atomic.Int64, cancel context.CancelFunc) {
	defer func() { _ = stdin.Close() }()
	for body := range out {
		if body == nil {
			return
		}
		err := lsprelay.Write(stdin, body)
		queued.Add(-int64(len(body)))
		if err != nil {
			cancel()
			return
		}
	}
}

// lspRuns says, for the judged call and the record, what a server may still
// run inside the sandbox with the engine's settings.
func lspRuns(language string) string {
	switch language {
	case "go":
		return "gopls runs go list (and, for packages that use cgo, the cgo tool and the C compiler) to load packages, but no project code"
	case "rust":
		return "rust-analyzer runs cargo metadata, which reads the workspace's .cargo/config.toml and can run a rustc or rustc-wrapper it names; build scripts, proc macros and cargo check are off. A workspace rust-analyzer.toml would override that: one found at the start is put to the person, and one that appears or changes while it runs stops it"
	case "typescript", "javascript":
		return "typescript-language-server runs the TypeScript installed with it, never the workspace's node_modules/typescript"
	case "python":
		return "basedpyright runs the Python interpreter it finds, the sandbox's or one a pyrightconfig.json names, to read its import paths"
	}
	return "the server may run programs of its own"
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
	if l.inBytes.Load()+int64(len(msg.Params)) > lspInBytes {
		c.reply(msg.ID, nil, refusal(errBusy, "the language server has %d MiB of messages waiting; try again", lspInBytes>>20))
		return
	}
	c.inflight.Add(1)
	select {
	case l.in <- msg:
		l.inBytes.Add(int64(len(msg.Params)))
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
			l.inBytes.Add(-int64(len(msg.Params)))
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
	if reason, stopped := l.stopReason(); stopped {
		c.reply(msg.ID, nil, refusal(errRefused, "the language server was stopped (%s)", reason))
		return
	}
	// Studio reporting a rust-analyzer.toml means the server would read it:
	// it is stopped instead, and its next start asks again.
	if l.language == "rust" && lsprelay.NamesRustAnalyzerConfig(p.Message) {
		if s := c.session(l.session); s != nil {
			s.raConfigs.Invalidate()
		}
		l.stop(lspConfigChanged)
		c.reply(msg.ID, map[string]any{"refused": "a rust-analyzer.toml would override Abhed's rust-analyzer settings, so the server was stopped; start it again to be asked"}, nil)
		return
	}
	out, err := l.filter.FromEditor(p.Message)
	var r *lsprelay.Refusal
	if errors.As(err, &r) {
		l.mu.Lock()
		l.refused++
		l.mu.Unlock()
		if r.ToServer != nil {
			// The server's request is answered, with an error, not left waiting.
			_ = l.write(r.ToServer)
		}
		if len(r.ID) > 0 && string(r.ID) != "null" {
			// The editor's request gets the server's kind of answer: an error.
			answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32600, "message": r.Reason}})
			c.notification("_abhed/lsp/message", map[string]any{"lspId": l.id, "message": json.RawMessage(answer)})
		}
		c.reply(msg.ID, map[string]any{"refused": r.Reason}, nil)
		return
	}
	if err := l.write(out); err != nil {
		code := errRefused
		if errors.Is(err, errLSPRestarting) {
			code = errBusy
		}
		c.reply(msg.ID, nil, refusal(code, "%v", err))
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
	l.stop("stopped")
	c.reply(msg.ID, map[string]any{}, nil)
}

// stop asks the server to shut down and exit, closes its input, and ends it
// if it has not gone within the grace.
func (l *acpLSP) stop(reason string) {
	l.mu.Lock()
	already := l.stopped
	l.stopped, l.restarting = true, false
	if !already {
		l.reason = reason
	}
	l.mu.Unlock()
	if already {
		// Stopped before, perhaps before its process existed: whatever runs
		// now is ended.
		l.kill()
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
	l.mu.Lock()
	if l.out != nil {
		select {
		case l.out <- nil: // the writer closes the input after what is queued
		default:
		}
	}
	l.mu.Unlock()
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
		l.stop("closed")
	}
	for _, l := range mine {
		select {
		case <-l.done:
		case <-time.After(5 * time.Second):
		}
	}
}
