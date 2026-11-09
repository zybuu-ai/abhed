// Package lspdiag gives the agent a language server's diagnostics for each
// file its write or edit changed. The server is the one a person's editor
// gets (lsprelay): started in the session's sandbox with the engine's safe
// settings, its command judged by policy and the supervisor as a bash call,
// and only on an allow. What the agent is told is bounded in count and in
// how long the edit waits for it.
package lspdiag

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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/lsprelay"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Defaults and bounds.
const (
	DefaultMax  = 20
	MaxMax      = 200
	DefaultWait = 3 * time.Second
	MaxWait     = 30 * time.Second
	// maxFile is the largest file sent to a server; a larger one gets none.
	maxFile = 2 << 20
	// maxMessage bounds one diagnostic's text as the agent reads it.
	maxMessage = 240
	// maxCrashes is how many times a language's server may end on its own
	// before diagnostics for it are off for the session.
	maxCrashes = 3
	life       = 12 * time.Hour
)

// Mode values: auto runs a server only where policy and the supervisor
// allow its command outright; on also takes the operator's setting as the
// answer to the plain ask a mode gives an unlisted command.
const (
	ModeAuto = "auto"
	ModeOn   = "on"
	ModeOff  = "off"
)

// Options configure a Diagnoser.
type Options struct {
	Mode string
	// Servers are the command lines by language, the configured ones over
	// lsprelay.DefaultServers.
	Servers map[string][]string
	// Max bounds the diagnostics the agent is given per file; Wait how long
	// an edit waits for them.
	Max  int
	Wait time.Duration
	// ToolchainRoot is Abhed's toolchain cache when the sandbox mounts it,
	// asked for when a server starts; nil is none.
	ToolchainRoot func() string
	// Find reports whether a program is on the PATH commands get; nil
	// takes every program as there and leaves it to the start script.
	Find func(program string) bool
}

// Command starts a server's command in the session's sandbox; tests
// replace it, and Tier, with their own.
var Command = func(b tools.Bash, ctx context.Context, cwd, command string) *exec.Cmd {
	return b.Sandbox(ctx, cwd, command)
}

// Tier is the tier the session's commands run under.
var Tier = func(b tools.Bash) string {
	if b.Sandbox != nil && b.RanUnder != nil {
		if t := b.RanUnder(); t != "" {
			return t
		}
	}
	return b.Isolation.Tier
}

// rustConfigChanged is the reason rust-analyzer is stopped when a
// rust-analyzer.toml is written, appears or changes, as for Studio's.
const rustConfigChanged = "workspace-config-changed"

// Bounds tests shorten.
var (
	initWait = 60 * time.Second
	tsWait   = 15 * time.Second
	settle   = 150 * time.Millisecond
	stopWait = 2 * time.Second
)

// Diagnoser runs one server per language for a session, started on the
// first change to a file of that language.
type Diagnoser struct {
	opt Options

	mu      sync.Mutex
	servers map[string]*server
	// off are languages diagnostics will not run for in this session.
	off     map[string]bool
	crashes map[string]int
	closed  bool
	n       int
}

// New returns a Diagnoser, or nil when opt.Mode is off.
func New(opt Options) *Diagnoser {
	if opt.Mode == ModeOff {
		return nil
	}
	if opt.Max <= 0 {
		opt.Max = DefaultMax
	}
	opt.Max = min(opt.Max, MaxMax)
	if opt.Wait <= 0 {
		opt.Wait = DefaultWait
	}
	opt.Wait = min(opt.Wait, MaxWait)
	return &Diagnoser{opt: opt, servers: map[string]*server{}, off: map[string]bool{}, crashes: map[string]int{}}
}

// Language is the server language and the document's language id for path,
// or "" for a file no server here reads.
func Language(path string) (language, id string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go", "go"
	case ".py", ".pyi":
		return "python", "python"
	case ".ts", ".mts", ".cts":
		return "typescript", "typescript"
	case ".tsx":
		return "typescript", "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript", "javascript"
	case ".jsx":
		return "javascript", "javascriptreact"
	case ".rs":
		return "rust", "rust"
	}
	return "", ""
}

// Diagnose implements agent.Diagnoser.
func (d *Diagnoser) Diagnose(ctx context.Context, l *agent.Loop, callID, path string) (agent.Diagnosed, bool) {
	start := time.Now()
	deadline := start.Add(d.opt.Wait)
	if lsprelay.IsRustAnalyzerConfig(path) {
		// It would override the engine's rust-analyzer settings: the server
		// stops, and the next start finds it and does not run.
		d.drop("rust")
		return agent.Diagnosed{}, false
	}
	lang, langID := Language(path)
	if lang == "" {
		return agent.Diagnosed{}, false
	}
	s, skipped, ok := d.server(ctx, l, lang)
	if !ok {
		return agent.Diagnosed{}, false
	}
	out := agent.Diagnosed{LSPDiagnostics: agent.LSPDiagnostics{Path: path, Language: lang}}
	if skipped != "" {
		out.Skipped = skipped
		return out, true
	}
	out.LSPID = s.id
	content, err := readBounded(path)
	if err != nil {
		return agent.Diagnosed{}, false
	}
	diags, timedOut, failed := s.diagnose(ctx, lsprelay.URI(path), langID, content, deadline)
	out.DurationMS = time.Since(start).Milliseconds()
	if failed != "" {
		// The server did not start or initialize: off for the session.
		d.mu.Lock()
		d.off[lang] = true
		d.mu.Unlock()
		out.Skipped = failed
		return out, true
	}
	if timedOut {
		out.TimedOut = true
		return out, true
	}
	out.Text, out.Total, out.Shown, out.Errors, out.Warnings = format(diags, d.opt.Max, l.Session.Rel(path), filepath.Base(s.argv[0]))
	return out, true
}

// Close stops every server, recording each end.
func (d *Diagnoser) Close() {
	d.mu.Lock()
	d.closed = true
	all := make([]*server, 0, len(d.servers))
	for _, s := range d.servers {
		all = append(all, s)
	}
	d.servers = map[string]*server{}
	d.mu.Unlock()
	for _, s := range all {
		s.stop("closed")
	}
}

// drop stops the language's server, if one runs.
func (d *Diagnoser) drop(lang string) {
	d.mu.Lock()
	s := d.servers[lang]
	delete(d.servers, lang)
	d.mu.Unlock()
	if s != nil {
		s.stop(rustConfigChanged)
	}
}

// server is the running server for lang, started if need be. skipped is
// why none runs, when that is to be recorded; ok false records nothing.
func (d *Diagnoser) server(ctx context.Context, l *agent.Loop, lang string) (s *server, skipped string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.off[lang] {
		return nil, "", false
	}
	if s := d.servers[lang]; s != nil {
		select {
		case <-s.done:
			delete(d.servers, lang)
			if !s.stopping() {
				d.crashes[lang]++
			}
		default:
			return s, "", true
		}
	}
	argv := d.opt.Servers[lang]
	if len(argv) == 0 {
		return nil, "", false
	}
	if d.crashes[lang] >= maxCrashes {
		d.off[lang] = true
		return nil, fmt.Sprintf("%s ended on its own %d times", argv[0], maxCrashes), true
	}
	tool, _ := l.Tools.Get("bash")
	b, isBash := tool.(tools.Bash)
	tier := ""
	if isBash {
		tier = Tier(b)
	}
	// Off with no record where no server can run: no sandbox, or none installed.
	if !isBash || b.Sandbox == nil || tier == "" || tier == string(sandbox.TierNone) {
		d.off[lang] = true
		return nil, "", false
	}
	if d.opt.Find != nil && !strings.ContainsAny(argv[0], "/\\") && !d.opt.Find(argv[0]) {
		d.off[lang] = true
		return nil, "", false
	}
	roots := l.Session.PolicyRoots()
	var watch *lsprelay.RustAnalyzerWatch
	if lang == "rust" {
		watch = lsprelay.NewRustAnalyzerWatch(roots, nil)
		files, incomplete := lsprelay.RustAnalyzerConfigs(roots)
		if why := lsprelay.RustAnalyzerConfigWhy(files, incomplete); why != "" {
			d.off[lang] = true
			return nil, "not started, as no one is asked here: " + why, true
		}
		watch.Add(files)
	}
	d.n++
	callID := fmt.Sprintf("diag-%s-%d", lang, d.n)
	command := lsprelay.CommandLine(argv)
	args, _ := json.Marshal(map[string]any{"command": command, "description": "language server for " + lang +
		" diagnostics after the agent's edits, in the session sandbox, run with " + strings.TrimSpace(lsprelay.Env)})
	decision := l.EngineDecision(ctx, "bash", callID, args)
	if !d.allowed(decision) {
		d.off[lang] = true
		return nil, "not started: " + string(decision.Decision) + " (" + decision.Reason + ")", true
	}
	s = &server{id: "lsp-" + callID, lang: lang, argv: argv, callID: callID, loop: l, roots: roots, tier: tier,
		ready: make(chan struct{}), done: make(chan struct{}), docs: map[string]int{}, pub: map[string]published{},
		notify: make(chan struct{}), answers: map[string]chan json.RawMessage{}}
	d.servers[lang] = s
	root := ""
	if d.opt.ToolchainRoot != nil {
		root = d.opt.ToolchainRoot()
	}
	go s.run(b, root, watch)
	return s, "", true
}

// allowed reports whether a server may start under decision: an allow, or
// in mode on the plain ask a mode or the default gives an unlisted
// command. A rule's ask, a deny and the supervisor's word stand.
func (d *Diagnoser) allowed(decision policy.Result) bool {
	switch decision.Decision {
	case policy.Allow:
		return true
	case policy.Ask:
		return d.opt.Mode == ModeOn && (decision.Step == "mode" || decision.Step == "default")
	}
	return false
}

type published struct {
	seq     int
	version *int
	diags   []diagnostic
}

type diagnostic struct {
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	} `json:"range"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
}

type server struct {
	id, lang, callID, tier string
	argv, roots            []string
	loop                   *agent.Loop

	ready     chan struct{} // closed once initialized, or failed (initErr)
	readyOnce sync.Once
	initErr   string
	done      chan struct{} // closed when the process is gone

	wmu    sync.Mutex
	in     io.WriteCloser
	cancel context.CancelFunc

	mu       sync.Mutex
	docs     map[string]int // open documents' versions, by URI
	pub      map[string]published
	seq      int
	notify   chan struct{} // closed and replaced on every publish
	answers  map[string]chan json.RawMessage
	settings map[string]json.RawMessage
	reason   string // set by a stop
	opMu     sync.Mutex
}

func (s *server) stopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason != ""
}

func (s *server) record(t agent.EventType, payload any) {
	_, _ = s.loop.Recorder.Record(t, agent.ActorSystem, agent.Trusted, payload)
}

// run starts the process, initializes it and reads it until it ends.
func (s *server) run(b tools.Bash, toolchainRoot string, watch *lsprelay.RustAnalyzerWatch) {
	defer close(s.done)
	fail := func(why string) {
		s.readyOnce.Do(func() {
			s.mu.Lock()
			s.initErr = why
			s.mu.Unlock()
			close(s.ready)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), life)
	defer cancel()
	cmd := Command(b, s.loop.LaunchContext(ctx, s.callID), s.roots[0], lsprelay.Script(s.argv, toolchainRoot))
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	errPipe, err3 := cmd.StderrPipe()
	err := errors.Join(cmd.Err, err1, err2, err3)
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fail("the language server could not start: " + err.Error())
		return
	}
	started := time.Now()
	s.wmu.Lock()
	s.in, s.cancel = stdin, cancel
	s.wmu.Unlock()
	s.record(agent.EvLSPStarted, agent.LSPStarted{LSPID: s.id, CallID: s.callID, Language: s.lang,
		Command: lsprelay.CommandLine(s.argv), Tier: s.tier, Purpose: "diagnostics"})
	var stderr bytes.Buffer
	tsLine := make(chan string, 1)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(&lsprelay.MarkerWriter{W: &capped{w: &stderr, left: 16 << 10}, Marker: lsprelay.TSServerMarker, Found: tsLine}, errPipe)
	}()
	br := bufio.NewReaderSize(stdout, 64<<10)
	go s.pump(br)
	if watch != nil {
		go s.watch(watch)
	}
	go s.initialize(tsLine, stderrDone, toolchainRoot, fail)
	<-stderrDone
	werr := cmd.Wait()
	sandbox.Release(cmd)
	cancel()
	fail("the language server ended before it initialized" + tail(stderr.String()))
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	} else if werr != nil {
		code = -1
	}
	s.mu.Lock()
	reason := s.reason
	s.mu.Unlock()
	if reason == "" {
		reason = "crashed"
	}
	s.record(agent.EvLSPStopped, agent.LSPStopped{LSPID: s.id, CallID: s.callID, Language: s.lang, Reason: reason,
		ExitCode: &code, DurationMS: time.Since(started).Milliseconds(), Purpose: "diagnostics"})
}

// initialize names the TypeScript for typescript-language-server, then
// sends initialize and initialized.
func (s *server) initialize(tsLine <-chan string, stderrDone <-chan struct{}, toolchainRoot string, fail func(string)) {
	var tsserver string
	if filepath.Base(s.argv[0]) == "typescript-language-server" {
		select {
		case tsserver = <-tsLine:
		case <-stderrDone:
		case <-time.After(tsWait):
		}
		home, _ := os.UserHomeDir()
		if tsserver == "" || lsprelay.TSServerRefused(tsserver, s.roots, home, toolchainRoot) {
			fail("typescript-language-server has no TypeScript of its own outside the workspace and home folder")
			s.kill()
			return
		}
	}
	options, sections := lsprelay.SafeSettings(s.lang, tsserver)
	s.mu.Lock()
	s.settings = sections
	s.mu.Unlock()
	params := lsprelay.InitParams(s.roots[0], json.RawMessage(clientCapabilities), options)
	res, err := s.request("initialize", params, time.Now().Add(initWait))
	if err != nil {
		// A process that ended says why on stderr, which its end records.
		select {
		case <-s.done:
			return
		case <-time.After(stopWait):
		}
		fail("the language server did not initialize: " + err.Error())
		s.kill()
		return
	}
	var r struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(res, &r) == nil && len(r.Error) > 0 && string(r.Error) != "null" {
		fail("the language server refused initialize")
		s.kill()
		return
	}
	if s.write([]byte(`{"jsonrpc":"2.0","method":"initialized","params":{}}`), time.Now().Add(initWait)) != nil {
		fail("the language server did not take initialized")
		return
	}
	s.readyOnce.Do(func() { close(s.ready) })
}

// clientCapabilities are what the server is told: diagnostics and settings,
// nothing that has it edit or open anything.
const clientCapabilities = `{"textDocument":{"synchronization":{"didSave":true},"publishDiagnostics":{"versionSupport":true}},"workspace":{"configuration":true}}`

// request sends a request and waits for its answer until deadline.
func (s *server) request(method string, params json.RawMessage, deadline time.Time) (json.RawMessage, error) {
	id := "abhed-" + method
	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.answers[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.answers, id)
		s.mu.Unlock()
	}()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err := s.write(body, deadline); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		return nil, errors.New("it ended")
	case <-time.After(time.Until(deadline)):
		return nil, errors.New("no answer in time")
	}
}

// write sends one message; one not taken by deadline ends the server.
func (s *server) write(body []byte, deadline time.Time) error {
	errc := make(chan error, 1)
	go func() {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		if s.in == nil {
			errc <- errors.New("not running")
			return
		}
		errc <- lsprelay.Write(s.in, body)
	}()
	select {
	case err := <-errc:
		return err
	case <-time.After(time.Until(deadline)):
		s.kill()
		return errors.New("the language server is not reading its input")
	}
}

func (s *server) kill() {
	s.wmu.Lock()
	cancel := s.cancel
	s.wmu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// stop asks the server to shut down and exit, and ends it after a grace.
func (s *server) stop(reason string) {
	s.mu.Lock()
	if s.reason == "" {
		s.reason = reason
	}
	s.mu.Unlock()
	until := time.Now().Add(stopWait)
	_ = s.write([]byte(`{"jsonrpc":"2.0","id":"abhed-shutdown","method":"shutdown"}`), until)
	_ = s.write([]byte(`{"jsonrpc":"2.0","method":"exit"}`), until)
	s.wmu.Lock()
	if s.in != nil {
		_ = s.in.Close()
	}
	s.wmu.Unlock()
	select {
	case <-s.done:
	case <-time.After(time.Until(until)):
		s.kill()
		select {
		case <-s.done:
		case <-time.After(stopWait):
		}
	}
}

// watch stops rust-analyzer when a rust-analyzer.toml appears, changes or goes.
func (s *server) watch(w *lsprelay.RustAnalyzerWatch) {
	t := time.NewTicker(lsprelay.RustAnalyzerWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if w.Changed() {
				s.stop(rustConfigChanged)
				return
			}
		}
	}
}

// pump reads the server: answers to the engine's requests, its
// diagnostics, and its own requests, which the engine answers.
func (s *server) pump(br *bufio.Reader) {
	for {
		raw, err := lsprelay.Read(br)
		if err != nil {
			s.kill()
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		hasID := len(m.ID) > 0 && string(m.ID) != "null"
		switch {
		case m.Method == "" && hasID:
			var id string
			_ = json.Unmarshal(m.ID, &id)
			s.mu.Lock()
			ch := s.answers[id]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- raw:
				default:
				}
			}
		case m.Method == "textDocument/publishDiagnostics":
			s.published(m.Params)
		case hasID:
			s.answer(m.ID, m.Method, m.Params)
		}
	}
}

// answer replies to a request of the server's: settings are the engine's,
// an edit is not applied and nothing is opened.
func (s *server) answer(id json.RawMessage, method string, params json.RawMessage) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "workspace/configuration":
		s.mu.Lock()
		settings := s.settings
		s.mu.Unlock()
		reply["result"] = lsprelay.Configuration(settings, params)
	case "workspace/applyEdit":
		reply["result"] = map[string]any{"applied": false, "failureReason": "the engine does not apply a server's edits"}
	case "window/showDocument":
		reply["result"] = map[string]any{"success": false}
	case "window/workDoneProgress/create", "client/registerCapability", "client/unregisterCapability", "window/showMessageRequest":
		reply["result"] = nil
	default:
		reply["error"] = map[string]any{"code": -32601, "message": "not supported"}
	}
	body, _ := json.Marshal(reply)
	go func() { _ = s.write(body, time.Now().Add(stopWait)) }()
}

func (s *server) published(params json.RawMessage) {
	var p struct {
		URI         string       `json:"uri"`
		Version     *int         `json:"version"`
		Diagnostics []diagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, open := s.docs[p.URI]; !open {
		return // only the files the engine sent are kept
	}
	s.seq++
	s.pub[p.URI] = published{seq: s.seq, version: p.Version, diags: p.Diagnostics}
	close(s.notify)
	s.notify = make(chan struct{})
}

// diagnose sends the file's content and waits for the diagnostics that
// follow it. failed is set when the server never initialized.
func (s *server) diagnose(ctx context.Context, uri, langID string, content []byte, deadline time.Time) (diags []diagnostic, timedOut bool, failed string) {
	wait := time.NewTimer(time.Until(deadline))
	defer wait.Stop()
	select {
	case <-s.ready:
	case <-wait.C:
		return nil, true, ""
	case <-ctx.Done():
		return nil, true, ""
	}
	s.mu.Lock()
	failed = s.initErr
	s.mu.Unlock()
	if failed != "" {
		return nil, false, failed
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	version, open := s.docs[uri]
	version++
	s.docs[uri] = version
	after := s.seq
	s.mu.Unlock()
	var msgs []map[string]any
	doc := map[string]any{"uri": uri, "version": version}
	if !open {
		msgs = append(msgs, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen",
			"params": map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": langID, "version": version, "text": string(content)}}})
	} else {
		msgs = append(msgs, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange",
			"params": map[string]any{"textDocument": doc, "contentChanges": []any{map[string]any{"text": string(content)}}}})
	}
	msgs = append(msgs, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didSave", "params": map[string]any{"textDocument": map[string]any{"uri": uri}}})
	for _, m := range msgs {
		body, _ := json.Marshal(m)
		if s.write(body, deadline) != nil {
			return nil, true, ""
		}
	}
	// The first report for this version, then any that follows it within
	// the settle, which servers send as their slower checks finish.
	var got *published
	var settled <-chan time.Time
	for {
		s.mu.Lock()
		p, ok := s.pub[uri]
		notify := s.notify
		s.mu.Unlock()
		if ok && p.seq > after && (p.version == nil || *p.version >= version) {
			got, after = &p, p.seq
			if settled == nil {
				settled = time.After(settle)
			}
		}
		select {
		case <-notify:
		case <-settled:
			return got.diags, false, ""
		case <-wait.C:
			if got != nil {
				return got.diags, false, ""
			}
			return nil, true, ""
		case <-ctx.Done():
			if got != nil {
				return got.diags, false, ""
			}
			return nil, true, ""
		case <-s.done:
			if got != nil {
				return got.diags, false, ""
			}
			return nil, true, ""
		}
	}
}

// format is the text the agent is given: the most severe first, at most
// max of them, each on one line.
func format(diags []diagnostic, maxShown int, rel, server string) (text string, total, shown, errs, warns int) {
	sev := func(d diagnostic) int {
		if d.Severity < 1 || d.Severity > 4 {
			return 1 // unset is taken as an error
		}
		return d.Severity
	}
	sort.SliceStable(diags, func(i, j int) bool {
		a, b := diags[i], diags[j]
		if sev(a) != sev(b) {
			return sev(a) < sev(b)
		}
		if a.Range.Start.Line != b.Range.Start.Line {
			return a.Range.Start.Line < b.Range.Start.Line
		}
		return a.Range.Start.Character < b.Range.Start.Character
	})
	for _, d := range diags {
		switch sev(d) {
		case 1:
			errs++
		case 2:
			warns++
		}
	}
	total = len(diags)
	names := [...]string{"", "error", "warning", "info", "hint"}
	var b strings.Builder
	if total == 0 {
		return fmt.Sprintf("[%s diagnostics for %s: none]", server, rel), 0, 0, 0, 0
	}
	fmt.Fprintf(&b, "[%s diagnostics for %s: %d errors, %d warnings, %d in all]", server, rel, errs, warns, total)
	for _, d := range diags {
		if shown == maxShown {
			break
		}
		msg := oneLine(d.Message)
		if d.Source != "" {
			msg += " (" + oneLine(d.Source) + ")"
		}
		fmt.Fprintf(&b, "\n%s:%d:%d: %s: %s", rel, d.Range.Start.Line+1, d.Range.Start.Character+1, names[sev(d)], msg)
		shown++
	}
	if total > shown {
		fmt.Fprintf(&b, "\n[%d more not shown]", total-shown)
	}
	return b.String(), total, shown, errs, warns
}

// oneLine is s on one line, cut to maxMessage runes.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxMessage {
		r := []rune(s)
		s = string(r[:maxMessage]) + "…"
	}
	return s
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the file the agent's call just wrote, resolved inside the session
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile || !utf8.Valid(b) {
		return nil, errors.New("too large, or not text")
	}
	return b, nil
}

// tail is the last lines of a server's stderr, as a clause.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return ": " + oneLine(strings.Join(lines, " "))
}

// capped writes to w until left runs out, then drops the rest.
type capped struct {
	w    io.Writer
	left int
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if c.left > 0 {
		k := min(len(p), c.left)
		_, _ = c.w.Write(p[:k])
		c.left -= k
	}
	return n, nil
}
