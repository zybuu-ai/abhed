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
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("initialize %s: %w", c.name, err)
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	return c.refreshTools(ctx)
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
	c.tools = res.Tools
	return nil
}

func (c *Client) Tools() []ToolDef { return c.tools }

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
}

func NewStdioTransport(ctx context.Context, command string, args []string, env []string) (*StdioTransport, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr is deliberately discarded rather than merged: a server that logs
	// to stderr would otherwise corrupt the JSON-RPC stream.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server %q: %w", command, err)
	}
	return &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 1<<20),
	}, nil
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
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	return nil
}
