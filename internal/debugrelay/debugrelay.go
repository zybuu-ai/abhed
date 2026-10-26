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
)

// Languages the relay debugs.
const (
	Go     = "go"
	Python = "python"
)

// Problem kinds, each with steps a person can take.
const (
	KindTier    = "tier-unsupported"
	KindBridge  = "bridge-missing"
	KindAdapter = "adapter-missing"
	KindArgs    = "invalid-request"
)

// Problem is a launch that cannot go ahead, with what to do about it.
type Problem struct {
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Steps   []string `json:"steps,omitempty"`
	Retry   bool     `json:"retry"`
}

func (p *Problem) Error() string { return p.Message }

// Exit codes the adapter command uses when its debugger is missing.
const (
	exitNoDelve   = 97
	exitNoDebugpy = 98
)

// MissingAdapter is the problem an adapter command's exit code means, or nil.
func MissingAdapter(code int, image string) *Problem {
	switch code {
	case exitNoDelve:
		return &Problem{Kind: KindAdapter, Retry: true, Message: "Delve (dlv) is not on the sandbox's PATH, so Go cannot be debugged there",
			Steps: []string{"Use a sandbox image with Go and Delve: build one FROM " + orElse(image, "golang:1.26-bookworm") + " with `RUN go install github.com/go-delve/delve/cmd/dlv@latest`",
				"Name it in sandbox.image (docs/guide/02-configuration.md#the-container-tiers-image)", "Start the debug session again"}}
	case exitNoDebugpy:
		return &Problem{Kind: KindAdapter, Retry: true, Message: "debugpy is not installed for the sandbox's python3, so Python cannot be debugged there",
			Steps: []string{"Use a sandbox image with Python and debugpy: build one FROM " + orElse(image, "python:3.12-slim-bookworm") + " with `RUN pip install debugpy`",
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
// command that carries Delve's socket over stdio (Go only).
func Plan(r Request, workspace, bridge string) (Spec, error) {
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
		return Spec{Cwd: cwd, Launch: launch, Display: strings.TrimSpace(strings.Join(prefix, " ") + " " + quoteAll(display)),
			Command: "command -v dlv >/dev/null 2>&1 || { echo 'abhed-debug: dlv is not on PATH' >&2; exit " + strconv.Itoa(exitNoDelve) + "; }; " +
				"d=$(mktemp -d /tmp/abhed-dap.XXXXXX) || exit 1; dlv dap --listen \"unix:$d/dap.sock\" >&2 & exec " + bridge + " \"unix:$d/dap.sock\""}, nil
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

	mu        sync.Mutex
	seq       int
	launchSeq int
	launched  bool
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

// header is the part of a DAP message the relay reads.
type header struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Command    string          `json:"command"`
	RequestSeq int             `json:"request_seq"`
	Success    bool            `json:"success"`
	Message    string          `json:"message"`
	Arguments  json.RawMessage `json:"arguments"`
	Event      string          `json:"event"`
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
	var h header
	if err := json.Unmarshal(raw, &h); err != nil {
		return fmt.Errorf("not a DAP message: %w", err)
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
		var h header
		if json.Unmarshal(raw, &h) != nil {
			continue
		}
		switch {
		case h.Type == "request" && (h.Command == "runInTerminal" || h.Command == "startDebugging"):
			// Either would run something outside the sandbox, in the editor.
			_ = r.reply(h, "Abhed runs the program inside the session's sandbox; it cannot be started in the editor's terminal")
			r.status("note", "the adapter asked to "+h.Command+"; refused")
			continue
		case h.Type == "response" && r.isLaunch(h.RequestSeq):
			if h.Success {
				r.status("launched", "")
			} else {
				r.status("failed", orElse(h.Message, "the adapter did not launch the program"))
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

func (r *Relay) isLaunch(seq int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.launchSeq != 0 && seq == r.launchSeq
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return Write(r.adapter, body)
}

// Write frames one message as DAP does.
func Write(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

// Read reads one framed message.
func Read(br *bufio.Reader) (json.RawMessage, error) {
	n := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
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
