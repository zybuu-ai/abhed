// Package mcp implements a Model Context Protocol client and gateway.
//
// Security posture (docs/architecture/03-security.md, threat T4): the research
// pass produced no verified claims about MCP's security model, so every server
// is treated as hostile until reviewed. Concretely:
//
//   - Servers must be in the registry to run; discovery does not imply trust.
//   - Tool descriptions enter the model's context and are therefore an
//     injection surface in themselves ("tool poisoning"), so they are sanitized
//     and length-bounded before the model ever sees them.
//   - Tool names are namespaced by server, so a malicious server cannot shadow
//     a native tool or another server's tool.
//   - Every response is untrusted content, tagged at ingest like any file read.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/secretfiles"
)

const protocolVersion = "2025-06-18"

// JSON-RPC 2.0 envelopes.

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// mcpClientVersion is what Abhed reports to an MCP server. It tracks the
// protocol client, not the release, so it moves only when that handshake does.
const mcpClientVersion = "0.1.0"

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// ToolDef is a tool as advertised by an MCP server.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []ToolDef `json:"tools"`
}

type callResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Transport carries JSON-RPC messages to a server.
type Transport interface {
	Send(ctx context.Context, payload []byte) error
	Receive(ctx context.Context) ([]byte, error)
	Close() error
}

// Client speaks MCP to one server.
type Client struct {
	name      string
	transport Transport
	nextID    atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan response
	closed  bool

	tools []ToolDef
	// refused are the names of the tools left out for their names.
	refused []string
	prompts []PromptDef
}

func NewClient(name string, t Transport) *Client {
	return &Client{name: name, transport: t, pending: make(map[int64]chan response)}
}

func (c *Client) Name() string { return c.name }

// Initialize performs the MCP handshake and caches the tool list.
func (c *Client) Initialize(ctx context.Context) error {
	go c.readLoop()

	params, _ := json.Marshal(map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"clientInfo":      map[string]any{"name": "abhed", "version": mcpClientVersion},
	})
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return c.withStderr(fmt.Errorf("initialize %s: %w", c.name, err))
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return c.withStderr(fmt.Errorf("initialized notification: %w", err))
	}
	if err := c.refreshTools(ctx); err != nil {
		return c.withStderr(err)
	}
	var init struct {
		Capabilities struct {
			Prompts json.RawMessage `json:"prompts"`
		} `json:"capabilities"`
	}
	// A server's prompts are optional: one that cannot list them still offers its tools.
	if json.Unmarshal(raw, &init) == nil && len(init.Capabilities.Prompts) > 0 && string(init.Capabilities.Prompts) != "null" {
		if err := c.refreshPrompts(ctx); err != nil {
			fmt.Fprintf(WarnOut, "abhed: warning: mcp server %q: %v\n", c.name, err)
		}
	}
	return nil
}

func (c *Client) refreshTools(ctx context.Context) error {
	raw, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return fmt.Errorf("tools/list on %s: %w", c.name, err)
	}
	var res toolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("parse tools/list from %s: %w", c.name, err)
	}
	c.tools, c.refused = keepNamed(c.name, res.Tools)
	return nil
}

// validToolName is what a remote tool's name must look like to be offered,
// the same rule tool_search lists names by: the name reaches prompts, the
// approval card and "always allow" scopes. The MCP specification allows 128
// characters; 64 is kept because model providers refuse longer tool names.
var validToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// WarnOut receives the warning for each tool refused for its name.
var WarnOut io.Writer = os.Stderr

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

// keepNamed drops the tools whose names fail validToolName, warning once per
// server and name, and returns the names it dropped.
func keepNamed(server string, defs []ToolDef) (kept []ToolDef, refused []string) {
	kept = defs[:0:0]
	for _, d := range defs {
		if validToolName.MatchString(d.Name) {
			kept = append(kept, d)
			continue
		}
		refused = append(refused, d.Name)
		warnedMu.Lock()
		if id := server + "\x00" + d.Name; !warned[id] {
			warned[id] = true
			fmt.Fprintf(WarnOut, "abhed: warning: mcp server %q offers a tool named %+q, which is not registered: "+
				"a tool name may hold only letters, digits, _ . and -, up to 64 characters (%d here)\n", server, d.Name, utf8.RuneCountInString(d.Name))
		}
		warnedMu.Unlock()
	}
	return kept, refused
}

// Refused are the names of the tools the server offers that were left out
// for their names, as the server sent them: untrusted text.
func (c *Client) Refused() []string { return c.refused }

func (c *Client) Tools() []ToolDef { return c.tools }

// PromptDef is a prompt as a server advertises it. Its name and its
// arguments' names have passed validToolName; the descriptions are untrusted.
type PromptDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Arguments   []PromptArg `json:"arguments"`
}

// PromptArg is one argument a prompt takes.
type PromptArg struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// Limits on what a server's prompts may add to the session.
const (
	maxPrompts     = 200
	maxPromptPages = 10
	maxPromptArgs  = 16
	// MaxPromptText bounds a prompt's text, as a tool's result is bounded.
	MaxPromptText = 30000
)

// refreshPrompts caches the server's prompts, leaving out any whose name or
// argument names fail validToolName, with a warning.
func (c *Client) refreshPrompts(ctx context.Context) error {
	var all []PromptDef
	cursor := ""
	for page := 0; page < maxPromptPages && len(all) < maxPrompts; page++ {
		var params json.RawMessage
		if cursor != "" {
			params, _ = json.Marshal(map[string]string{"cursor": cursor})
		}
		raw, err := c.call(ctx, "prompts/list", params)
		if err != nil {
			return fmt.Errorf("prompts/list: %w", err)
		}
		var res struct {
			Prompts    []PromptDef `json:"prompts"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("parse prompts/list: %w", err)
		}
		all = append(all, res.Prompts...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if len(all) > maxPrompts {
		all = all[:maxPrompts]
	}
	kept := all[:0:0]
	for _, p := range all {
		ok := validToolName.MatchString(p.Name) && len(p.Arguments) <= maxPromptArgs
		for _, a := range p.Arguments {
			ok = ok && validToolName.MatchString(a.Name)
		}
		if ok {
			kept = append(kept, p)
			continue
		}
		warnedMu.Lock()
		if id := c.name + "\x00prompt\x00" + p.Name; !warned[id] {
			warned[id] = true
			fmt.Fprintf(WarnOut, "abhed: warning: mcp server %q offers a prompt named %+q whose name or arguments are not plain, "+
				"so it is not offered: names may hold only letters, digits, _ . and -, up to 64\n", c.name, p.Name)
		}
		warnedMu.Unlock()
	}
	c.prompts = kept
	return nil
}

// Prompts are the server's prompts that passed the name checks.
func (c *Client) Prompts() []PromptDef { return c.prompts }

// GetPrompt fetches a prompt's messages with args and returns the text of
// its messages, joined and capped. The text is UNTRUSTED: a third party wrote it.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	if args == nil {
		args = map[string]string{}
	}
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	raw, err := c.call(ctx, "prompts/get", params)
	if err != nil {
		return "", err
	}
	var res struct {
		Messages []struct {
			Role    string       `json:"role"`
			Content contentBlock `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("parse prompts/get result: %w", err)
	}
	var parts []string
	for _, m := range res.Messages {
		if m.Content.Type == "text" && m.Content.Text != "" {
			parts = append(parts, m.Content.Text)
		}
	}
	text := strings.Join(parts, "\n\n")
	if len(text) > MaxPromptText {
		text = strings.ToValidUTF8(text[:MaxPromptText], "") + "\n\n[truncated]"
	}
	return text, nil
}

// Call invokes a tool. The returned content is UNTRUSTED: it is data written by
// a third-party server, never instructions.
func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage) (string, bool, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", true, err
	}

	raw, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return "", true, err
	}
	var res callResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", true, fmt.Errorf("parse tools/call result: %w", err)
	}

	var b strings.Builder
	for _, block := range res.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n"), res.IsError, nil
}

func (c *Client) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("client %s is closed", c.name)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if err := c.transport.Send(ctx, payload); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("timeout calling %s on %s", method, c.name)
	}
}

func (c *Client) notify(ctx context.Context, method string, params json.RawMessage) error {
	payload, err := json.Marshal(request{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.transport.Send(ctx, payload)
}

func (c *Client) readLoop() {
	for {
		data, err := c.transport.Receive(context.Background())
		if err != nil {
			c.mu.Lock()
			c.closed = true
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = map[int64]chan response{}
			c.mu.Unlock()
			return
		}
		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			continue // notifications and malformed frames are ignored
		}
		c.mu.Lock()
		ch, found := c.pending[resp.ID]
		c.mu.Unlock()
		if found {
			select {
			case ch <- resp:
			default:
			}
		}
	}
}

// stderrTailer is a transport that keeps the end of what its server wrote
// to stderr.
type stderrTailer interface {
	StderrTail(wait time.Duration) string
}

// withStderr adds to err the end of what the server wrote to stderr, which
// says why a server that died at the start did: "broken pipe" alone does not.
func (c *Client) withStderr(err error) error {
	t, ok := c.transport.(stderrTailer)
	if !ok {
		return err
	}
	if tail := t.StderrTail(time.Second); tail != "" {
		return fmt.Errorf("%w; the server wrote: %s", err, tail)
	}
	return err
}

func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.transport.Close()
}

// StdioTransport runs a server as a subprocess and speaks newline-delimited
// JSON-RPC over its stdin/stdout.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	// cancel ends a confined server's own context, revoking its egress credential.
	cancel context.CancelFunc
	// cleanup removes what the server runs in, such as its container, which
	// killing the engine's client would leave running.
	cleanup func()
	// stderr keeps the end of what the server writes to stderr; stderrDone
	// closes when it has written all it will.
	stderr     *tailBuffer
	stderrDone chan struct{}
	// env and argv are the server's own environment and command line, as
	// configured, which its stderr is redacted against.
	env, argv []string
}

// stderrKeep is how much of a server's stderr is kept: its last lines.
const stderrKeep = 4 << 10

// tailBuffer keeps the last stderrKeep bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > stderrKeep {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-stderrKeep:]...)
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// StderrTail is the end of the server's stderr, its last lines, printable
// and bounded, waiting up to wait for a server that is ending to finish
// writing.
func (t *StdioTransport) StderrTail(wait time.Duration) string {
	if t.stderr == nil {
		return ""
	}
	if t.stderrDone != nil {
		select {
		case <-t.stderrDone:
		case <-time.After(wait):
		}
	}
	lines := strings.Split(strings.TrimSpace(t.redact(t.stderr.String())), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	out := strings.Map(func(r rune) rune {
		if r == '\t' || r >= ' ' && r != 0x7f {
			return r
		}
		return ' '
	}, strings.Join(lines, " | "))
	if len(out) > 1000 {
		out = "…" + out[len(out)-1000:]
	}
	return strings.TrimSpace(out)
}

// envKept are the variables whose values are no secret and stay in a
// server's stderr as written.
var envKept = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "TERM": true, "USER": true,
	"LOGNAME": true, "SHELL": true, "PWD": true, "TZ": true}

// redact takes out of a server's stderr what could be a secret before it is
// told anywhere: each value of the server's own environment but the plain
// ones above, an argument after a flag that names a token, key, secret or
// password, and anything shaped like a key or token.
func (t *StdioTransport) redact(text string) string {
	env, args := t.env, t.argv
	if env == nil && t.cmd != nil {
		env = t.cmd.Env
	}
	if args == nil && t.cmd != nil {
		args = t.cmd.Args
	}
	var values []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && !envKept[k] && !strings.HasPrefix(k, "LC_") && len(v) >= 6 {
			values = append(values, v)
		}
	}
	for i, a := range args {
		low := strings.ToLower(a)
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch {
		case low == "-e" || low == "--env":
			// docker run -e NAME=VALUE: the value, when NAME names a secret
			if k, v, ok := strings.Cut(next, "="); ok && namesSecret(strings.ToLower(k)) {
				values = append(values, v)
			}
		case strings.HasPrefix(low, "--env="):
			if k, v, ok := strings.Cut(strings.TrimPrefix(a, "--env="), "="); ok && namesSecret(strings.ToLower(k)) {
				values = append(values, v)
			}
		case low == "-h" || low == "--header":
			values = append(values, headerSecret(next)...)
		case strings.HasPrefix(low, "--header="):
			values = append(values, headerSecret(a[len("--header="):])...)
		case strings.HasPrefix(a, "-H") && len(a) > 2:
			// -HAuthorization: Bearer VALUE, -H=…
			values = append(values, headerSecret(strings.TrimPrefix(a[2:], "="))...)
		case strings.HasPrefix(low, "-") && !strings.HasPrefix(low, "--no-") && namesSecret(low):
			// --api-token VALUE, --token=VALUE; not a following flag
			if _, v, ok := strings.Cut(a, "="); ok {
				values = append(values, v)
			} else if !strings.HasPrefix(next, "-") {
				values = append(values, next)
			}
		default:
			// NAME=VALUE given as an argument
			if k, v, ok := strings.Cut(a, "="); ok && !strings.HasPrefix(k, "-") && namesSecret(strings.ToLower(k)) {
				values = append(values, v)
			}
		}
	}
	values = slices.DeleteFunc(values, func(v string) bool { return len(v) < 6 })
	// Longest first, so a value inside another is not left half there.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, v := range values {
		// As written, and as a server may print it: URL-encoded or inside a
		// JSON string.
		quoted, _ := json.Marshal(v)
		for _, form := range []string{v, url.QueryEscape(v), url.PathEscape(v), strings.Trim(string(quoted), `"`)} {
			text = strings.ReplaceAll(text, form, "[secret]")
		}
	}
	text, _ = secretfiles.RedactContent(text)
	return text
}

// headerSecret is what to hide of an HTTP header argument ("Name: value"):
// for an Authorization header or one named as a secret, its value and the
// credential after its scheme (Bearer, Basic, Token).
func headerSecret(h string) []string {
	k, v, ok := strings.Cut(h, ":")
	secret := namesSecret(strings.ToLower(k)) || strings.EqualFold(strings.TrimSpace(k), "authorization")
	if !ok || !secret {
		return nil
	}
	v = strings.TrimSpace(v)
	out := []string{v}
	if f := strings.Fields(v); len(f) == 2 {
		out = append(out, f[1])
	}
	return out
}

// namesSecret reports whether a lower-cased name says it holds a secret.
func namesSecret(low string) bool {
	for _, w := range []string{"token", "key", "secret", "pass", "credential", "auth"} {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

func NewStdioTransport(ctx context.Context, command string, args []string, env []string) (*StdioTransport, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	// Never nil, which would inherit Abhed's whole environment and its keys.
	cmd.Env = append([]string{}, env...)
	return startStdio(cmd, command, env, append([]string{command}, args...))
}

// startStdio starts cmd, built to run the server command, and speaks to it
// over its stdin and stdout. env and argv are the server's own, as
// configured: what its stderr is redacted against, whatever cmd wraps them
// in (a container engine's command, with the environment in a file).
func startStdio(cmd *exec.Cmd, command string, env, argv []string) (*StdioTransport, error) {
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr is kept apart, never merged into the JSON-RPC stream, and only
	// its end is kept: it is told when the server fails to start.
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server %q: %w", command, err)
	}
	t := &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 1<<20),
		stderr: &tailBuffer{}, stderrDone: make(chan struct{}),
		env: append([]string{}, env...), argv: append([]string{}, argv...),
	}
	go func() {
		defer close(t.stderrDone)
		_, _ = io.Copy(t.stderr, errPipe)
	}()
	return t, nil
}

func (t *StdioTransport) Send(_ context.Context, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.stdin.Write(append(payload, '\n')); err != nil {
		return err
	}
	return nil
}

func (t *StdioTransport) Receive(_ context.Context) ([]byte, error) {
	line, err := t.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

func (t *StdioTransport) Close() error {
	_ = t.stdin.Close()
	// Before the kill: the container goes first, so nothing outlives the close.
	if t.cleanup != nil {
		t.cleanup()
	}
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	if t.cancel != nil {
		t.cancel()
	}
	return nil
}
