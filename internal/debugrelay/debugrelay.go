// Package debugrelay runs a Debug Adapter Protocol session for a person's
// editor with the adapter inside the session's sandbox. The engine, not the
// editor, decides what is debugged: it builds the adapter's launch request
// from what was asked and judged, refuses an editor's own launch or attach,
// and answers the adapter's requests to run something in the editor's
// terminal with an error, since that would run outside the sandbox.
package debugrelay

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Languages the relay debugs.
const (
	Go     = "go"
	Python = "python"
)

// Problem kinds, each with steps a person can take.
const (
	KindTier = "tier-unsupported"
	// KindContainerSetup is a tier that cannot debug, with what the
	// container tier needs here and how to set it up.
	KindContainerSetup = "container-setup"
	KindBridge         = "bridge-missing"
	KindAdapter        = "adapter-missing"
	KindArgs           = "invalid-request"
)

// Problem is a launch that cannot go ahead, with what to do about it.
type Problem struct {
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Steps   []string `json:"steps,omitempty"`
	Retry   bool     `json:"retry"`
	// Install is the toolchain install that fixes it, for a one-click offer:
	// _abhed/toolchain/status then install with these, as for any toolchain.
	Install *Install `json:"install,omitempty"`
}

// Install names a toolchain install: the language, with its debugger or its
// language server.
type Install struct {
	Language string `json:"language"`
	Debugger bool   `json:"debugger,omitempty"`
	LSP      bool   `json:"lsp,omitempty"`
}

func (p *Problem) Error() string { return p.Message }

// NoDelve is the problem of Delve missing where the session's commands run.
func NoDelve(image string) *Problem { return MissingAdapter(exitNoDelve, image) }

// Exit codes the adapter command uses when its debugger is missing.
const (
	exitNoDelve   = 97
	exitNoDebugpy = 98
)

// MissingAdapter is the problem an adapter command's exit code means, or nil.
func MissingAdapter(code int, image string) *Problem {
	switch code {
	case exitNoDelve:
		return &Problem{Kind: KindAdapter, Retry: true, Message: "Go's debugger (Delve) is not installed where this session's commands run",
			Install: &Install{Language: "go", Debugger: true},
			Steps: []string{"Install Go with its debugger: choose Install, or run `abhed toolchain install -debugger go` in a terminal (add `-platform linux` for the container tier)",
				"Or use a sandbox image with Go and Delve: build one FROM " + orElse(image, "golang:1.26-bookworm") + " with `RUN go install github.com/go-delve/delve/cmd/dlv@latest`",
				"Name it in sandbox.image (docs/guide/02-configuration.md#the-container-tiers-image)", "Start the debug session again"}}
	case exitNoDebugpy:
		return &Problem{Kind: KindAdapter, Retry: true, Message: "Python's debugger (debugpy) is not installed for the Python this session's commands run",
			Install: &Install{Language: "python", Debugger: true},
			Steps: []string{"Install Python with its debugger: choose Install, or run `abhed toolchain install -debugger python` in a terminal",
				"Or use a sandbox image with Python and debugpy: build one FROM " + orElse(image, "python:3.12-slim-bookworm") + " with `RUN pip install debugpy`",
				"Name it in sandbox.image (docs/guide/02-configuration.md#the-container-tiers-image)", "Start the debug session again"}}
	}
	return nil
}

func orElse(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Request is what a person asked to debug: a program, or a test (a Go
// package, or what pytest is given), with its arguments, working folder and
// environment.
type Request struct {
	Language string            `json:"language"`
	Program  string            `json:"program,omitempty"`
	Test     string            `json:"test,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// Spec is a planned session: the adapter command run in the sandbox, the
// debuggee's command line as it is judged and recorded, and the launch
// request the engine sends the adapter.
type Spec struct {
	Command string
	Display string
	Cwd     string
	Launch  map[string]any
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Plan checks r against the workspace and plans its session. bridge is the
// command that carries Delve's socket over stdio (Go only). sockDir is a
// folder only this command sees, for Delve's socket; "" makes one in the
// command's /tmp, which on the container tier is its own.
func Plan(r Request, workspace, bridge, sockDir string) (Spec, error) {
	bad := func(format string, a ...any) error {
		return &Problem{Kind: KindArgs, Message: fmt.Sprintf(format, a...)}
	}
	if (r.Program == "") == (r.Test == "") {
		return Spec{}, bad("name a program or a test to debug, not both")
	}
	cwd := workspace
	if r.Cwd != "" {
		c, err := inside(workspace, r.Cwd)
		if err != nil {
			return Spec{}, bad("cwd: %v", err)
		}
		cwd = c
	}
	target := r.Program
	if target == "" {
		target = r.Test
	}
	if strings.HasPrefix(target, "-") || slices.ContainsFunc(r.Args, func(a string) bool { return strings.ContainsRune(a, 0) }) {
		return Spec{}, bad("the program or test may not start with '-' or hold a NUL")
	}
	env := map[string]string{}
	names := make([]string, 0, len(r.Env))
	for k, v := range r.Env {
		if !envName.MatchString(k) || strings.ContainsRune(v, 0) {
			return Spec{}, bad("env: %q is not a variable name", k)
		}
		env[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	var prefix []string
	for _, k := range names {
		prefix = append(prefix, k+"="+shellQuote(env[k]))
	}
	args := append([]string{}, r.Args...)
	launch := map[string]any{"name": "Abhed", "type": r.Language, "request": "launch", "cwd": cwd, "env": env,
		"args": args, "console": "internalConsole"}
	var display []string
	switch r.Language {
	case Go:
		if bridge == "" {
			return Spec{}, &Problem{Kind: KindBridge, Message: "no debug bridge for Go"}
		}
		p, err := inside(workspace, resolve(cwd, target))
		if err != nil {
			return Spec{}, bad("%v", err)
		}
		mode := "debug"
		if r.Test != "" {
			mode = "test"
		}
		// The program's output comes to the editor as output events, not to
		// dlv's own stdout, which the bridge does not carry.
		launch["mode"], launch["program"], launch["outputMode"] = mode, p, "remote"
		display = append(append([]string{"dlv", mode, p}, "--"), args...)
		if len(args) == 0 {
			display = display[:3]
		}
		mkdir := "d=$(mktemp -d \"${TMPDIR:-/tmp}/abhed-dap.XXXXXX\") || exit 1; "
		if sockDir != "" {
			// The engine made it (macOS) or mounted it (Linux): made here only
			// where it is missing, without walking a parent the command may not read.
			mkdir = "d=" + shellQuote(sockDir) + "; [ -d \"$d\" ] || { mkdir -p \"$d\" && chmod 700 \"$d\"; } || exit 1; "
		}
		return Spec{Cwd: cwd, Launch: launch, Display: strings.TrimSpace(strings.Join(prefix, " ") + " " + quoteAll(display)),
			Command: "command -v dlv >/dev/null 2>&1 || { echo 'abhed-debug: dlv is not on PATH' >&2; exit " + strconv.Itoa(exitNoDelve) + "; }; " +
				mkdir + "dlv dap --check-go-version=false --listen \"unix:$d/dap.sock\" >&2 & exec " + bridge + " \"unix:$d/dap.sock\""}, nil
	case Python:
		launch["justMyCode"] = true
		if r.Program != "" {
			p, err := inside(workspace, resolve(cwd, r.Program))
			if err != nil {
				return Spec{}, bad("%v", err)
			}
			launch["program"] = p
			display = append([]string{"python3", p}, args...)
		} else {
			// The path part of a pytest node id (before ::) is in the workspace too.
			path, _, _ := strings.Cut(r.Test, "::")
			if _, err := inside(workspace, resolve(cwd, path)); err != nil {
				return Spec{}, bad("%v", err)
			}
			launch["module"] = "pytest"
			launch["args"] = append([]string{r.Test}, args...)
			display = append([]string{"python3", "-m", "pytest", r.Test}, args...)
		}
		return Spec{Cwd: cwd, Launch: launch, Display: strings.TrimSpace(strings.Join(prefix, " ") + " " + quoteAll(display)),
			Command: "python3 -c 'import debugpy' >/dev/null 2>&1 || { echo 'abhed-debug: debugpy is not installed' >&2; exit " + strconv.Itoa(exitNoDebugpy) + "; }; " +
				"exec python3 -m debugpy.adapter"}, nil
	}
	return Spec{}, bad("language %q is not one Abhed debugs (go or python)", r.Language)
}

// resolve is p taken from cwd when it is relative.
func resolve(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

// inside resolves p against base and refuses one outside the workspace base is in.
func inside(base, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(base, p)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return "", fmt.Errorf("%s is outside the workspace", p)
	}
	return p, nil
}

// Quote is s as one shell word.
func Quote(s string) string { return shellQuote(s) }

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune("/.-_:=,", r) && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteAll(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = shellQuote(w)
	}
	return strings.Join(out, " ")
}

// maxMessage bounds one DAP message either way.
const maxMessage = 16 << 20

// engineSeq is where the engine's own requests and responses are numbered,
// far from an editor's, which count from one.
const engineSeq = 1 << 30

// Relay carries one session's messages between the editor and the adapter.
type Relay struct {
	adapter  io.Writer
	launch   map[string]any
	toEditor func(json.RawMessage)
	status   func(state, message string)

	// wmu orders writes to the adapter; mu guards the rest, and is never
	// held while writing, so a stalled adapter blocks no state change.
	wmu       sync.Mutex
	mu        sync.Mutex
	dropped   int
	seq       int
	launchSeq int
	// launchAnswered is set once the launch's response has been taken.
	launchAnswered bool
	launched       bool
	// terminating is set once the editor asked the adapter to terminate.
	terminating bool
	onEnd       func()
	endOnce     sync.Once
}

// OnEnd sets what runs once the session is over by the protocol: the adapter
// answered disconnect, or reported the program terminated or exited after a
// terminate. An adapter such as debugpy's waits for its input to close, so
// the caller closes it, and ends the adapter if it does not go.
func (r *Relay) OnEnd(f func()) {
	r.mu.Lock()
	r.onEnd = f
	r.mu.Unlock()
}

func (r *Relay) end() {
	r.mu.Lock()
	f := r.onEnd
	r.mu.Unlock()
	if f != nil {
		r.endOnce.Do(f)
	}
}

// New is a relay writing to the adapter's stdin; toEditor receives every
// message for the editor, and status the session's state changes.
func New(adapter io.Writer, launch map[string]any, toEditor func(json.RawMessage), status func(state, message string)) *Relay {
	return &Relay{adapter: adapter, launch: launch, toEditor: toEditor, status: status, seq: engineSeq}
}

// header is the part of a DAP message the relay reads, each field taken by
// its exact key. Readers differ on other spellings: JSON.parse and Python's
// dict take only the exact key, Go's encoding/json (go-dap, in Delve) also
// any key equal under Unicode simple folding, the last one winning. So a
// key that folds to a decisive one without being it is refused.
type header struct {
	Seq        int
	Type       string
	Command    string
	RequestSeq int
	Success    bool
	Message    string
	Arguments  json.RawMessage
	Event      string
	Body       json.RawMessage
}

// headerKeys are the keys the relay decides on. A message that holds one
// twice, or a key that differs from one only in case, is refused: readers
// differ on which of two they take, and the relay must judge the one the
// other side acts on.
var headerKeys = []string{"seq", "type", "command", "request_seq", "success", "message", "arguments", "event", "body"}

// decisive reports whether k is equal under Unicode simple folding (as
// strings.EqualFold, and so as encoding/json, compares) to a decisive key,
// and whether it is that key exactly.
func decisive(k string) (match, exact bool) {
	for _, name := range headerKeys {
		if strings.EqualFold(k, name) {
			return true, k == name
		}
	}
	return false, false
}

// ErrAmbiguous is a message whose decisive keys repeat or differ in case.
var ErrAmbiguous = errors.New("the message repeats a key, or has one that differs only in case from seq, type, command, request_seq, success, message, arguments, event or body")

// parseHeader reads a message's top-level keys exactly, refusing an ambiguous one.
func parseHeader(raw []byte) (header, error) {
	var h header
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return h, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return h, errors.New("not a JSON object")
	}
	vals := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return h, err
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return h, err
		}
		if match, exact := decisive(k); match {
			// A variant (any case, ſ for s, K for k), or the key twice.
			if _, dup := vals[k]; dup || !exact {
				return h, ErrAmbiguous
			}
		}
		vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return h, err
	}
	if dec.More() {
		return h, errors.New("trailing data")
	}
	str := func(k string) (string, error) {
		v, ok := vals[k]
		if !ok || string(v) == "null" {
			return "", nil
		}
		var out string
		if err := json.Unmarshal(v, &out); err != nil {
			return "", fmt.Errorf("%s is not a string", k)
		}
		return out, nil
	}
	num := func(k string) (int, error) {
		v, ok := vals[k]
		if !ok || string(v) == "null" {
			return 0, nil
		}
		var out int
		if err := json.Unmarshal(v, &out); err != nil {
			return 0, fmt.Errorf("%s is not a number", k)
		}
		return out, nil
	}
	var errs []error
	var e error
	h.Type, e = str("type")
	errs = append(errs, e)
	h.Command, e = str("command")
	errs = append(errs, e)
	h.Message, e = str("message")
	errs = append(errs, e)
	h.Event, e = str("event")
	errs = append(errs, e)
	h.Seq, e = num("seq")
	errs = append(errs, e)
	h.RequestSeq, e = num("request_seq")
	errs = append(errs, e)
	if v, ok := vals["success"]; ok {
		errs = append(errs, json.Unmarshal(v, &h.Success))
	}
	h.Arguments, h.Body = vals["arguments"], vals["body"]
	return h, errors.Join(errs...)
}

// Refused editor requests: the engine starts the program itself, and nothing
// attaches to a process it did not start.
var refusedFromEditor = map[string]string{
	"launch":         "Abhed starts the program it judged; a launch from the editor is not taken",
	"attach":         "Abhed does not attach to a process it did not start",
	"runInTerminal":  "not a request an editor sends",
	"startDebugging": "not a request an editor sends",
}

// FromEditor takes one message from the editor: a request is passed to the
// adapter unless refused, when the editor gets a failed response instead.
func (r *Relay) FromEditor(raw json.RawMessage) error {
	if len(raw) > maxMessage {
		return errors.New("the message is too large")
	}
	h, err := parseHeader(raw)
	if err != nil {
		return fmt.Errorf("not a DAP message Abhed can read unambiguously: %w", err)
	}
	if h.Type != "request" {
		return fmt.Errorf("the editor may send only requests, not %q", h.Type)
	}
	why, refused := refusedFromEditor[h.Command]
	if h.Command == "restart" && len(h.Arguments) > 0 && string(h.Arguments) != "null" && string(h.Arguments) != "{}" {
		why, refused = "a restart may not change what was launched", true
	}
	if refused {
		r.editor(map[string]any{"type": "response", "request_seq": h.Seq, "success": false, "command": h.Command, "message": why})
		return nil
	}
	if h.Command == "terminate" {
		r.mu.Lock()
		r.terminating = true
		r.mu.Unlock()
	}
	return r.write(raw)
}

// Pump reads the adapter's messages until it ends, answering its terminal
// requests itself and sending the launch once the editor's initialize is
// answered.
func (r *Relay) Pump(adapter io.Reader) error {
	br := bufio.NewReader(adapter)
	for {
		raw, err := Read(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		h, err := parseHeader(raw)
		if err != nil {
			// Not passed on: an ambiguous message could reach the editor as a
			// request the relay never judged. Counted, not shown to anyone.
			r.mu.Lock()
			r.dropped++
			r.mu.Unlock()
			continue
		}
		switch {
		case h.Type == "request" && (h.Command == "runInTerminal" || h.Command == "startDebugging"):
			// Either would run something outside the sandbox, in the editor.
			_ = r.reply(h, "Abhed runs the program inside the session's sandbox; it cannot be started in the editor's terminal")
			r.status("note", "the adapter asked to "+h.Command+"; refused")
			continue
		case h.Type == "response" && r.consumeLaunch(h.RequestSeq):
			if h.Success {
				r.status("launched", "")
			} else {
				msg := Bounded(orElse(h.Message, "the adapter did not launch the program"), maxDetail)
				switch detail := errorDetail(h.Body); {
				case detail == "" || detail == msg:
				case strings.HasPrefix(detail, msg):
					msg = detail
				default:
					msg = Bounded(msg+": "+detail, maxDetail)
				}
				r.status("failed", msg)
			}
			continue
		}
		r.editor(raw)
		r.mu.Lock()
		terminating := r.terminating
		r.mu.Unlock()
		if h.Type == "response" && h.Command == "disconnect" || h.Type == "event" && terminating && (h.Event == "terminated" || h.Event == "exited") {
			r.end()
		}
		if h.Type == "response" && h.Command == "initialize" && h.Success {
			if err := r.sendLaunch(); err != nil {
				return err
			}
		}
	}
}

// consumeLaunch reports whether seq answers the engine's launch, once: a
// later response naming the same seq is the editor's to see, not a second
// verdict on the launch.
func (r *Relay) consumeLaunch(seq int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.launchAnswered || r.launchSeq == 0 || seq != r.launchSeq {
		return false
	}
	r.launchAnswered = true
	return true
}

func (r *Relay) sendLaunch() error {
	r.mu.Lock()
	if r.launched {
		r.mu.Unlock()
		return nil
	}
	r.launched = true
	r.seq++
	r.launchSeq = r.seq
	msg := map[string]any{"seq": r.seq, "type": "request", "command": "launch", "arguments": r.launch}
	r.mu.Unlock()
	b, _ := json.Marshal(msg)
	return r.write(b)
}

func (r *Relay) reply(h header, why string) error {
	r.mu.Lock()
	r.seq++
	msg := map[string]any{"seq": r.seq, "type": "response", "request_seq": h.Seq, "success": false, "command": h.Command, "message": why}
	r.mu.Unlock()
	b, _ := json.Marshal(msg)
	return r.write(b)
}

// Disconnect asks the adapter to end the session and the program.
func (r *Relay) Disconnect() error {
	r.mu.Lock()
	r.seq++
	msg := map[string]any{"seq": r.seq, "type": "request", "command": "disconnect", "arguments": map[string]any{"terminateDebuggee": true}}
	r.mu.Unlock()
	b, _ := json.Marshal(msg)
	return r.write(b)
}

func (r *Relay) editor(v any) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		raw, _ = json.Marshal(v)
	}
	r.toEditor(raw)
}

func (r *Relay) write(body []byte) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	return Write(r.adapter, body)
}

// Dropped is how many of the adapter's messages were not passed on because
// they could not be read unambiguously.
func (r *Relay) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// maxDetail bounds, in bytes, the adapter's text a status carries.
const maxDetail = 2000

// Bounded is s cut to at most n bytes on a rune boundary, marked when cut,
// with invalid UTF-8 dropped.
func Bounded(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], "") + "…"
}

// errorDetail renders a failed response's body.error as DAP defines it:
// format with each {name} replaced by its variable. It is the adapter's
// text, untrusted: built in one pass that stops at maxDetail bytes, so a
// format of many placeholders and a large variable cannot grow the engine's
// memory past the body itself and the budget.
func errorDetail(body json.RawMessage) string {
	var b struct {
		Error struct {
			Format    string            `json:"format"`
			Variables map[string]string `json:"variables"`
		} `json:"error"`
	}
	if len(body) == 0 || json.Unmarshal(body, &b) != nil || b.Error.Format == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(min(len(b.Error.Format), maxDetail) + 8)
	add := func(s string) bool {
		if room := maxDetail - out.Len(); len(s) > room {
			out.WriteString(Bounded(s, room))
			return false
		}
		out.WriteString(s)
		return true
	}
	f := b.Error.Format
	for f != "" {
		i := strings.IndexByte(f, '{')
		if i < 0 {
			add(f)
			break
		}
		if !add(f[:i]) {
			break
		}
		j := strings.IndexByte(f[i:], '}')
		if j < 0 {
			add(f[i:])
			break
		}
		name := f[i+1 : i+j]
		v, ok := b.Error.Variables[name]
		if !ok {
			v = f[i : i+j+1]
		}
		if !add(v) {
			break
		}
		f = f[i+j+1:]
	}
	return strings.ToValidUTF8(out.String(), "")
}

// Write frames one message as DAP does.
func Write(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

// Header bounds: a line, and the whole header block. What comes from the
// sandbox is untrusted, so a stream with no newline must not grow the
// engine's memory.
const (
	maxHeaderLine  = 1 << 10
	maxHeaderBlock = 8 << 10
)

// ErrHeader is a header line or block past its bound.
var ErrHeader = errors.New("debugrelay: a header line or block is too long")

// Read reads one framed message. br's buffer must be at least
// maxHeaderLine bytes (bufio.NewReader's default is 4 KiB).
func Read(br *bufio.Reader) (json.RawMessage, error) {
	n := -1
	total := 0
	for {
		raw, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(raw) > maxHeaderLine {
			return nil, ErrHeader
		}
		if err != nil {
			return nil, err
		}
		if total += len(raw); total > maxHeaderBlock {
			return nil, ErrHeader
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if n, err = strconv.Atoi(strings.TrimSpace(v)); err != nil || n < 0 || n > maxMessage {
				return nil, fmt.Errorf("debugrelay: a message of %q bytes", strings.TrimSpace(v))
			}
		}
	}
	if n < 0 {
		return nil, errors.New("debugrelay: a message with no Content-Length")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

// pinnedAdapter is debugpy's adapter with the ports it listens on taken, in
// order, from ABHED_DAP_PORTS instead of chosen by the system: the ports the
// sandbox opens to it alone (sandbox.WithLoopbackPorts). Before anything
// else it binds every one of them on the wildcard and loopback addresses,
// IPv4 and IPv6, and holds them until it exits: no other program can take
// one of those, even one the adapter has closed (its launcher's listener
// after the launcher connects) or not used yet. The debuggee can still bind
// one on the Mac's own network address (Seatbelt's localhost matches it,
// and SO_REUSEADDR lets a specific address go over a held wildcard), which
// the consent discloses. The variable is removed
// before the adapter runs, so the program it starts does not see it.
const pinnedAdapter = `import os, socket as S
ports = [int(p) for p in os.environ.pop("ABHED_DAP_PORTS").split(",")]
held, free = [], []
def bind(fam, host, port, reuse):
    x = S.socket(fam, S.SOCK_STREAM)
    if fam == S.AF_INET6:
        x.setsockopt(S.IPPROTO_IPV6, S.IPV6_V6ONLY, 1)
    if reuse:
        x.setsockopt(S.SOL_SOCKET, S.SO_REUSEADDR, 1)
    x.bind((host, port))
    held.append(x)
    return x
for p in ports:
    # The wildcard first, never listening, then loopback, which the adapter
    # listens on: no other program takes those (a specific network address
    # can still be bound over the wildcard; see above).
    bind(S.AF_INET, "0.0.0.0", p, False)
    free.append(bind(S.AF_INET, "127.0.0.1", p, True))
    try:
        bind(S.AF_INET6, "::", p, False)
        bind(S.AF_INET6, "::1", p, True)
    except OSError as e:
        if e.errno not in (47, 49, 97, 99):
            raise
import debugpy.common.sockets as s
create = s.create_server
def pinned(host, port=0, backlog=S.SOMAXCONN, timeout=None):
    if port:
        return create(host, port, backlog, timeout)
    if not free:
        raise OSError("abhed-debug: no loopback port left for the debug adapter (each restart takes one)")
    a = free.pop(0)
    srv = S.fromfd(a.fileno(), S.AF_INET, S.SOCK_STREAM)
    srv.settimeout(timeout)
    srv.listen(backlog)
    return srv
s.create_server = pinned
import runpy
runpy.run_module("debugpy.adapter", run_name="__main__", alter_sys=True)
`

// PinnedPorts is how many ports a Python session is given: the adapter's
// server listener, its launcher's, and one more launcher for each of three
// restarts.
const PinnedPorts = 5

// PinPorts makes a Python spec's adapter listen on ports alone, for a
// sandbox that opens those loopback ports and no others.
func PinPorts(spec *Spec, ports []uint16) {
	const adapter = "exec python3 -m debugpy.adapter"
	if !strings.HasSuffix(spec.Command, adapter) || len(ports) == 0 {
		return
	}
	list := make([]string, len(ports))
	for i, p := range ports {
		list[i] = strconv.Itoa(int(p))
	}
	// -P (3.11+) keeps the workspace off sys.path, so no file there stands in
	// for the shim's imports.
	spec.Command = strings.TrimSuffix(spec.Command, adapter) + `P=; python3 -P -c '' >/dev/null 2>&1 && P=-P; ` +
		"ABHED_DAP_PORTS=" + strings.Join(list, ",") + " exec python3 $P -c " + shellQuote(pinnedAdapter)
}
