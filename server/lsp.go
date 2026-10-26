package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Workbench language servers: an operator-named command, started as the person's
// judged and recorded bash call inside the session's sandbox; its output is untrusted.

const (
	// maxLSPs bounds the language servers one session runs.
	maxLSPs = 4
	// lspIdle ends a language server no page has listened to for this long.
	lspIdle = 2 * time.Minute
	// lspQueue bounds what one reader may have waiting; a reader past it is ended.
	lspQueue = 16 << 20
)

type lspRun struct {
	id, language string
	root         string
	// roots are the session's folders: the workspace, its added folders
	// and skill folders, any of which the editor may have open.
	roots []string
	// settings are the engine's answers to the server's
	// workspace/configuration (lsprelay.SafeSettings), which it gives the
	// server itself.
	settings map[string]json.RawMessage
	cancel   context.CancelFunc
	stdin    io.WriteCloser
	writeMu  sync.Mutex

	mu sync.Mutex
	// options are the engine's initializationOptions for the server, set
	// once the start has named a typescript-language-server's TypeScript.
	options  json.RawMessage
	subs     map[*lspSub]struct{}
	lastRead time.Time
	// reason is why the engine stopped the server, for the record.
	reason string
	done   chan struct{}
}

// lspSub is one page reading a language server. queued is the bytes handed
// to it and not yet written; behind closes when it falls past lspQueue.
type lspSub struct {
	ch     chan []byte
	queued int
	behind chan struct{}
}

type lspStartRequest struct {
	Language string `json:"language"`
}

type lspStartResponse struct {
	ID string `json:"id"`
	// Root is the workspace's path as the server sees it, for its file URIs.
	Root string `json:"root"`
}

// lspTiers are the sandboxes a language server may run in; an empty or
// unknown tier is not one of them.
var lspTiers = map[string]bool{"process": true, "container": true, "vm": true, "fence": true}

// lspMethods are what the workbench editor sends. Anything else, such as
// workspace/executeCommand, can have the server run programs past the policy.
var lspMethods = map[string]bool{
	"initialize": true, "initialized": true, "shutdown": true, "exit": true, "$/cancelRequest": true,
	"textDocument/didOpen": true, "textDocument/didChange": true, "textDocument/didClose": true,
	"textDocument/hover": true, "textDocument/definition": true,
}

// lspCapabilities are the editor's, as ide.html declares them.
const lspCapabilities = `{"textDocument":{"hover":{"contentFormat":["markdown","plaintext"]},"definition":{"linkSupport":true},"publishDiagnostics":{},"synchronization":{}},"workspace":{"configuration":true}}`

// lspAllowed returns, rebuilt, the page's message that may reach the server:
// one of lspMethods, or a null answer to the server's own request.
func lspAllowed(body []byte, root string) ([]byte, bool) {
	return lspAllowedIn(body, root, []string{root}, nil)
}

// lspAllowedIn is lspAllowed with the session's folders as the roots a file
// may be in.
func lspAllowedIn(body []byte, root string, roots []string, options json.RawMessage) ([]byte, bool) {
	// Read by exact keys, as the server reads it: a key twice, or but for case, is refused.
	if lsprelay.UniqueKeys(body) != nil {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false // a batch, or not an object
	}
	for k := range m {
		switch k {
		case "jsonrpc", "id", "method", "params", "result", "error":
		default:
			return nil, false
		}
	}
	raw, isCall := m["method"]
	if isCall {
		var method string
		if json.Unmarshal(raw, &method) != nil || !lspMethods[method] {
			return nil, false
		}
		switch method {
		case "initialize":
			// initializationOptions and the like can set a server's environment
			// or tools, so the server is told only the workspace and capabilities.
			m["params"] = lsprelay.InitParams(root, json.RawMessage(lspCapabilities), options)
		case "initialized":
			m["params"] = json.RawMessage(`{}`)
		}
	} else {
		if id := string(m["id"]); id == "" || id == "null" {
			return nil, false
		}
		// Settings the server asks for stay its defaults: a setting such as env
		// or build flags can have it run programs, so only nulls are passed.
		if _, failed := m["error"]; failed || !lsprelay.Nulls(m["result"]) {
			return nil, false
		}
		m = map[string]json.RawMessage{"jsonrpc": json.RawMessage(`"2.0"`), "id": m["id"], "result": m["result"]}
	}
	out, err := json.Marshal(m)
	// A file outside the workspace is not the server's to read.
	return out, err == nil && lsprelay.Inside(out, roots)
}

// startLSP starts the session's language server for one language, or names
// the one already running.
func (s *Server) startLSP(w http.ResponseWriter, r *http.Request) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	var req lspStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Language == "" {
		badBody(w, err, "language is required")
		return
	}
	_, _, cfg := s.state.snapshot()
	argv := cfg.Sandbox.LanguageServers[req.Language]
	if len(argv) == 0 {
		WriteError(w, http.StatusNotFound, "no language server is configured for "+req.Language)
		return
	}
	tool, _ := live.Loop.Tools.Get("bash")
	b, isBash := tool.(tools.Bash)
	if in := isolationOf(tool); !isBash || b.Sandbox == nil || in == nil || !lspTiers[in.Tier] {
		WriteError(w, http.StatusConflict, "a language server runs only inside a sandbox, and this session has none")
		return
	}

	live.shellMu.Lock()
	defer live.shellMu.Unlock()
	live.mu.Lock()
	if live.lsps == nil {
		live.lsps = map[string]*lspRun{}
	}
	running := 0
	for lid, l := range live.lsps {
		select {
		case <-l.done:
			delete(live.lsps, lid)
			continue
		default:
		}
		if l.language == req.Language {
			live.mu.Unlock()
			WriteJSON(w, http.StatusOK, lspStartResponse{ID: l.id, Root: sess.Root})
			return
		}
		running++
	}
	gen := live.lspGen
	live.mu.Unlock()
	if running >= maxLSPs {
		WriteError(w, http.StatusTooManyRequests, "four language servers are already running in this session")
		return
	}

	// A workspace rust-analyzer.toml overrides the engine's settings, and
	// the workbench has no one to ask on the spot, so the start is refused.
	// A walk that stopped at a bound is refused too: a file may have gone
	// unseen. The check is kept while nothing new is on the session's record.
	// The watch is made before the check, so a file written between them
	// is still seen.
	var watch *lsprelay.RustAnalyzerWatch
	if req.Language == "rust" {
		watch = lsprelay.NewRustAnalyzerWatch(sess.PolicyRoots(), nil)
		raFiles, incomplete := live.raConfigs.Get(sess.PolicyRoots(), lsprelay.RecordStamp(live.Loop.Recorder))
		watch.Add(raFiles)
		if why := lsprelay.RustAnalyzerConfigWhy(raFiles, incomplete); why != "" {
			WriteError(w, http.StatusConflict, "not started: "+why+"; start it from Abhed Studio, which asks you, or remove the file")
			return
		}
	}
	id := "u" + newSessionID()
	command := lsprelay.CommandLine(argv)
	args, _ := json.Marshal(map[string]any{"command": command, "description": "language server for " + req.Language + " in the workbench editor, run with " + strings.TrimSpace(lsprelay.Env)})
	if _, refused, err := live.Loop.ManualAuthorize("bash", id, args); err != nil {
		writeUnrecorded(w, err, "the language server could not be recorded, so it was not started")
		return
	} else if refused != nil {
		s.unrecorded(nil, live, live.Loop.ManualObserve(id, "bash", *refused, 0))
		WriteError(w, http.StatusForbidden, refused.Content)
		return
	}
	// From here the call is on the record, so every way out records its result.
	start := time.Now()
	fail := func(status int, why string) {
		res := tools.Result{Content: why, IsError: true}
		s.unrecorded(nil, live, live.Loop.ManualObserve(id, "bash", res, time.Since(start)))
		WriteError(w, status, why)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shellMax)
	cmd := b.Sandbox(live.Loop.LaunchContext(ctx, id), sess.Root, lsprelay.Script(argv, lspToolchainRoot(cfg)))
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	var stderr bytes.Buffer
	tsLine := make(chan string, 1)
	cmd.Stderr = &lsprelay.MarkerWriter{W: &limitedWriter{w: &stderr, n: 16 << 10}, Marker: lsprelay.TSServerMarker, Found: tsLine}
	// Unstarted, the command closes neither pipe itself.
	closePipes := func() {
		if stdin != nil {
			_ = stdin.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
	}
	if err := errors.Join(err1, err2); err != nil {
		cancel()
		closePipes()
		fail(http.StatusInternalServerError, "the language server could not be started: "+err.Error())
		return
	}
	options, settings := lsprelay.SafeSettings(req.Language, "")
	run := &lspRun{id: id, language: req.Language, root: sess.Root, roots: sess.PolicyRoots(), cancel: cancel, stdin: stdin,
		options: options, settings: settings,
		subs: map[*lspSub]struct{}{}, lastRead: time.Now(), done: make(chan struct{})}
	// Listed before it starts, so a session closing from here on waits for its end.
	live.mu.Lock()
	if live.lspGen != gen {
		live.mu.Unlock()
		cancel()
		closePipes()
		fail(http.StatusConflict, "the session's terminals closed before the language server started")
		return
	}
	live.lsps[id] = run
	live.mu.Unlock()
	if err := cmd.Start(); err != nil {
		cancel()
		fail(http.StatusInternalServerError, "Failed to run command: "+err.Error())
		close(run.done)
		return
	}

	watchEvery := lsprelay.RustAnalyzerWatchEvery
	go run.read(stdout)
	go func() {
		idle := time.NewTicker(15 * time.Second)
		defer idle.Stop()
		// rust-analyzer is stopped when a rust-analyzer.toml appears or
		// changes while it runs.
		var watchC <-chan time.Time
		if watch != nil {
			t := time.NewTicker(watchEvery)
			defer t.Stop()
			watchC = t.C
		}
		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()
		var err error
	loop:
		for {
			select {
			case err = <-waited:
				break loop
			case <-watchC:
				if watch.Changed() {
					live.raConfigs.Invalidate()
					run.stopFor(lspConfigChanged)
				}
			case <-idle.C:
				run.mu.Lock()
				gone := len(run.subs) == 0 && time.Since(run.lastRead) > lspIdle
				run.mu.Unlock()
				if gone {
					cancel()
				}
			}
		}
		cancel()
		code := 0
		if err != nil {
			code = -1
		}
		ended := "ended"
		run.mu.Lock()
		if run.reason != "" {
			ended = "stopped: " + run.reason
		}
		run.mu.Unlock()
		res := tools.Result{Content: fmt.Sprintf("exit %d · language server for %s %s\n%s", code, req.Language, ended, stderr.String()), ExitCode: &code}
		s.unrecorded(nil, live, live.Loop.ManualObserve(id, "bash", res, time.Since(start)))
		// Only now: closing the session waits on done for this end to be recorded.
		close(run.done)
	}()
	// typescript-language-server gets the TypeScript beside it, or does not
	// run: without the pin it would take the workspace's own.
	if filepath.Base(argv[0]) == "typescript-language-server" {
		var ts string
		select {
		case ts = <-tsLine:
		case <-run.done:
		case <-time.After(lspTSWait):
		}
		home, _ := os.UserHomeDir()
		if lsprelay.TSServerRefused(ts, run.roots, home, lspToolchainRoot(cfg)) {
			cancel()
			<-run.done
			WriteError(w, http.StatusConflict, "typescript-language-server was not started: no TypeScript is installed beside it where commands cannot write (Abhed's toolchain cache, or outside the session's folders, home and temporary folders), and the workspace's own is not used")
			return
		}
		options, _ := lsprelay.SafeSettings(req.Language, ts)
		run.mu.Lock()
		run.options = options
		run.mu.Unlock()
	}
	WriteJSON(w, http.StatusOK, lspStartResponse{ID: id, Root: sess.Root})
}

// read splits the server's output into messages and hands each to every reader.
func (l *lspRun) read(out io.Reader) {
	br := bufio.NewReaderSize(out, 64<<10)
	for {
		msg, err := lsprelay.Read(br)
		if err != nil {
			l.cancel()
			return
		}
		// The server's settings are the engine's: it is answered here, and
		// the page never sees the question.
		if answer := l.configurationAnswer(msg); answer != nil {
			l.writeMu.Lock()
			_ = lsprelay.Write(l.stdin, answer)
			l.writeMu.Unlock()
			continue
		}
		l.mu.Lock()
		for sub := range l.subs {
			if sub.queued+len(msg) > lspQueue {
				l.drop(sub)
				continue
			}
			select {
			case sub.ch <- msg:
				sub.queued += len(msg)
			default:
				l.drop(sub)
			}
		}
		l.mu.Unlock()
	}
}

// configurationAnswer is the engine's answer to a workspace/configuration
// request in msg, or nil when msg is not one.
func (l *lspRun) configurationAnswer(msg []byte) []byte {
	if !bytes.Contains(msg, []byte("workspace/configuration")) {
		return nil
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if lsprelay.UniqueKeys(msg) != nil || json.Unmarshal(msg, &m) != nil || m.Method != "workspace/configuration" || len(m.ID) == 0 {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": lsprelay.Configuration(l.settings, m.Params)})
	return b
}

// lspConfigChanged is why a rust-analyzer is stopped when a
// rust-analyzer.toml appears or changes while it runs.
const lspConfigChanged = "workspace-config-changed"

// stopFor ends the server, noting why for the record of its end.
func (l *lspRun) stopFor(reason string) {
	l.mu.Lock()
	if l.reason == "" {
		l.reason = reason
	}
	l.mu.Unlock()
	l.cancel()
}

// lspToolchainRoot is Abhed's toolchain cache when the sandbox mounts it
// read-only (sandbox.toolchains not off): a TypeScript there is taken
// though it is under the home folder. Otherwise it is "" and nothing is
// exempt.
var lspToolchainRoot = sandboxconfig.ToolchainCache

// lspTSWait bounds the wait for typescript-language-server's start to name
// the TypeScript beside it; tests shorten it.
var lspTSWait = 15 * time.Second

// drop ends a reader that has fallen behind: a message lost would leave the
// page waiting on an answer, and one kept would grow without bound. Under l.mu.
func (l *lspRun) drop(sub *lspSub) {
	delete(l.subs, sub)
	close(sub.behind)
}

func (s *Server) lspFor(w http.ResponseWriter, r *http.Request) (*lspRun, bool) {
	id := r.PathValue("id")
	live, found := s.session(r.Context(), id, TenantOf(r.Context()), UserOf(r.Context()))
	if !validSessionID(id) || !found {
		WriteError(w, http.StatusNotFound, "that language server is not running")
		return nil, false
	}
	live.mu.Lock()
	run := live.lsps[r.PathValue("lsp")]
	live.mu.Unlock()
	if run == nil {
		WriteError(w, http.StatusNotFound, "that language server is not running")
		return nil, false
	}
	return run, true
}

// sendLSP passes one message from the page to the language server.
func (s *Server) sendLSP(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lspFor(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, lsprelay.MaxMessage))
	if err != nil || !json.Valid(body) {
		badBody(w, err, "the message is not JSON")
		return
	}
	// A rust-analyzer.toml the page opens or saves would be read by the
	// server, overriding the engine's settings: the server is stopped.
	if run.language == "rust" && lsprelay.NamesRustAnalyzerConfig(body) {
		run.stopFor(lspConfigChanged)
		WriteError(w, http.StatusConflict, "a rust-analyzer.toml would override Abhed's rust-analyzer settings, so the language server was stopped")
		return
	}
	run.mu.Lock()
	options := run.options
	run.mu.Unlock()
	body, ok = lspAllowedIn(body, run.root, run.roots, options)
	if !ok {
		WriteError(w, http.StatusForbidden, "the workbench sends a language server only editor requests")
		return
	}
	select {
	case <-run.done:
		WriteError(w, http.StatusGone, "the language server has ended")
		return
	default:
	}
	run.writeMu.Lock()
	_, err = fmt.Fprintf(run.stdin, "Content-Length: %d\r\n\r\n%s", len(body), body)
	run.writeMu.Unlock()
	if err != nil {
		WriteError(w, http.StatusGone, "the language server has ended")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// streamLSP sends the language server's messages as they come.
func (s *Server) streamLSP(w http.ResponseWriter, r *http.Request) {
	guard := s.guardStream(r, r.PathValue("id"))
	defer guard.stop()
	run, ok := s.lspFor(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sub := &lspSub{ch: make(chan []byte, 256), behind: make(chan struct{})}
	run.mu.Lock()
	run.subs[sub] = struct{}{}
	run.lastRead = time.Now()
	run.mu.Unlock()
	defer func() {
		run.mu.Lock()
		delete(run.subs, sub)
		run.lastRead = time.Now()
		run.mu.Unlock()
	}()
	if err := guard.check(); err != nil {
		endStream(w, err)
		return
	}
	// Opened: the page knows the stream is there before it sends initialize.
	fmt.Fprint(w, "event: ready\ndata: \n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-sub.ch:
			if err := guard.allowed(); err != nil {
				endStream(w, err)
				return
			}
			fmt.Fprintf(w, "event: msg\ndata: %s\n\n", base64.StdEncoding.EncodeToString(msg))
			flusher.Flush()
			run.mu.Lock()
			sub.queued -= len(msg)
			run.mu.Unlock()
		case <-sub.behind:
			// Ended as the server's end would be: what it missed is not replayed.
			fmt.Fprint(w, "event: exit\ndata: \n\n")
			flusher.Flush()
			return
		case <-guard.tick():
			if err := guard.check(); err != nil {
				endStream(w, err)
				return
			}
		case <-guard.woken():
			if err := guard.allowed(); err != nil {
				endStream(w, err)
				return
			}
		case <-run.done:
			fmt.Fprint(w, "event: exit\ndata: \n\n")
			flusher.Flush()
			return
		}
	}
}

// stopLSP ends a language server.
func (s *Server) stopLSP(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lspFor(w, r)
	if !ok {
		return
	}
	run.cancel()
	w.WriteHeader(http.StatusNoContent)
}

// stopLSPs ends the session's language servers, returning channels closed once each end is
// recorded. Under l.mu; one starting meanwhile sees lspGen moved and does not start.
func (l *liveSession) stopLSPs() []<-chan struct{} {
	l.lspGen++
	var done []<-chan struct{}
	for id, run := range l.lsps {
		select {
		case <-run.done:
			delete(l.lsps, id)
			continue
		default:
		}
		run.cancel()
		done = append(done, run.done)
	}
	return done
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
