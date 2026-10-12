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

// The workbench editor's language servers (go to definition, hover,
// diagnostics). A server is a command the operator names in
// sandbox.language_servers, never a workspace file. Starting one is the
// person's bash call: judged by the policy and recorded, then run by the
// session's sandbox exactly as a command is. One with no sandbox around it,
// on the none tier, is not started. Its messages travel over the session's
// own authenticated routes; what it says is untrusted, and the page draws it
// as text.

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
	if in := isolationOf(tool); !isBash || b.Sandbox == nil || in == nil || in.Tier == "none" {
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
	for _, l := range live.lsps {
		select {
		case <-l.done:
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

	ctx, cancel := context.WithTimeout(context.Background(), shellMax)
	cmd := b.Sandbox(live.Loop.LaunchContext(ctx, id), sess.Root, command)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	if err := errors.Join(err1, err2); err != nil {
		cancel()
		WriteError(w, http.StatusInternalServerError, "the language server could not be started")
		return
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		res := tools.Result{Content: "Failed to run command: " + err.Error(), IsError: true}
		s.unrecorded(nil, live, live.Loop.ManualObserve(id, "bash", res, 0))
		WriteError(w, http.StatusInternalServerError, res.Content)
		return
	}
	run := &lspRun{id: id, language: req.Language, cancel: cancel, stdin: stdin,
		subs: map[*lspSub]struct{}{}, lastRead: time.Now(), done: make(chan struct{})}
	live.mu.Lock()
	live.lsps[id] = run
	live.mu.Unlock()

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
		close(run.done)
		code := 0
		if err != nil {
			code = -1
		}
		res := tools.Result{Content: fmt.Sprintf("exit %d · language server for %s ended\n%s", code, req.Language, stderr.String()), ExitCode: &code}
		s.unrecorded(nil, live, live.Loop.ManualObserve(id, "bash", res, time.Since(start)))
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

// stopLSPs ends a session's language servers, as its terminals end with it.
func (l *liveSession) stopLSPs() {
	for _, run := range l.lsps {
		run.cancel()
	}
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
