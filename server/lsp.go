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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Workbench language servers: an operator-named command, started as the person's
// judged and recorded bash call inside the session's sandbox; its output is untrusted.

const (
	// maxLSPs bounds the language servers one session runs.
	maxLSPs = 4
	// lspIdle ends a language server no page has listened to for this long.
	lspIdle = 2 * time.Minute
	// lspMaxMessage bounds one message either way.
	lspMaxMessage = 8 << 20
	// lspMaxHeader bounds one header line, and lspMaxHeaders their count.
	lspMaxHeader  = 1 << 10
	lspMaxHeaders = 16
	// lspQueue bounds what one reader may have waiting; a reader past it is ended.
	lspQueue = 16 << 20
)

type lspRun struct {
	id, language string
	root         string
	cancel       context.CancelFunc
	stdin        io.WriteCloser
	writeMu      sync.Mutex

	mu       sync.Mutex
	subs     map[*lspSub]struct{}
	lastRead time.Time
	done     chan struct{}
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
			m["params"] = lspInitParams(root)
		case "initialized":
			m["params"] = json.RawMessage(`{}`)
		}
	} else {
		if id := string(m["id"]); id == "" || id == "null" {
			return nil, false
		}
		// Settings the server asks for stay its defaults: a setting such as env
		// or build flags can have it run programs, so only nulls are passed.
		if _, failed := m["error"]; failed || !lspNulls(m["result"]) {
			return nil, false
		}
		m = map[string]json.RawMessage{"jsonrpc": json.RawMessage(`"2.0"`), "id": m["id"], "result": m["result"]}
	}
	out, err := json.Marshal(m)
	return out, err == nil
}

// lspNulls reports whether an answer is null or a list of nulls.
func lspNulls(raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		for _, v := range list {
			if string(v) != "null" {
				return false
			}
		}
		return true
	}
	return string(bytes.TrimSpace(raw)) == "null"
}

// lspInitParams are the initialize params the server gets for the workspace at root.
func lspInitParams(root string) json.RawMessage {
	uri := lspURI(root)
	name := root[strings.LastIndex(root, "/")+1:]
	b, _ := json.Marshal(map[string]any{
		"processId": nil, "rootUri": uri,
		"workspaceFolders": []map[string]string{{"uri": uri, "name": name}},
		"capabilities":     json.RawMessage(lspCapabilities),
	})
	return b
}

// lspURI is root's file URI, escaped as the editor's encodeURIComponent does.
func lspURI(root string) string {
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(root); i++ {
		c := root[i]
		if c == '/' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+,-]+$`)

// commandLine is argv as the policy reads a bash command.
func commandLine(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		if safeWord.MatchString(a) {
			words[i] = a
		} else {
			words[i] = shellQuote(a)
		}
	}
	return strings.Join(words, " ")
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

	id := "u" + newSessionID()
	command := commandLine(argv)
	args, _ := json.Marshal(map[string]any{"command": command, "description": "language server for " + req.Language + " in the workbench editor"})
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
	cmd := b.Sandbox(live.Loop.LaunchContext(ctx, id), sess.Root, command)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
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
	run := &lspRun{id: id, language: req.Language, root: sess.Root, cancel: cancel, stdin: stdin,
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

	go run.read(stdout)
	go func() {
		idle := time.NewTicker(15 * time.Second)
		defer idle.Stop()
		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()
		var err error
	loop:
		for {
			select {
			case err = <-waited:
				break loop
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
		res := tools.Result{Content: fmt.Sprintf("exit %d · language server for %s ended\n%s", code, req.Language, stderr.String()), ExitCode: &code}
		s.unrecorded(nil, live, live.Loop.ManualObserve(id, "bash", res, time.Since(start)))
		// Only now: closing the session waits on done for this end to be recorded.
		close(run.done)
	}()
	WriteJSON(w, http.StatusOK, lspStartResponse{ID: id, Root: sess.Root})
}

// read splits the server's output into messages and hands each to every reader.
func (l *lspRun) read(out io.Reader) {
	br := bufio.NewReaderSize(out, 64<<10)
	for {
		msg, err := readLSPMessage(br)
		if err != nil {
			l.cancel()
			return
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

// drop ends a reader that has fallen behind: a message lost would leave the
// page waiting on an answer, and one kept would grow without bound. Under l.mu.
func (l *lspRun) drop(sub *lspSub) {
	delete(l.subs, sub)
	close(sub.behind)
}

// readLSPMessage reads one Content-Length framed message. The server is
// untrusted, so its header lines are bounded in length and number.
func readLSPMessage(br *bufio.Reader) ([]byte, error) {
	size := -1
	for headers := 0; ; headers++ {
		raw, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(raw) > lspMaxHeader || headers >= lspMaxHeaders {
			return nil, errors.New("language server header too long")
		}
		if err != nil {
			return nil, err
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > lspMaxMessage {
				return nil, errors.New("bad Content-Length")
			}
			size = n
		}
	}
	if size < 0 {
		return nil, errors.New("no Content-Length")
	}
	msg := make([]byte, size)
	if _, err := io.ReadFull(br, msg); err != nil {
		return nil, err
	}
	return msg, nil
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, lspMaxMessage))
	if err != nil || !json.Valid(body) {
		badBody(w, err, "the message is not JSON")
		return
	}
	body, ok = lspAllowed(body, run.root)
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
