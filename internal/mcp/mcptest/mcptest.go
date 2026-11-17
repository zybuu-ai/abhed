// Package mcptest runs stub stdio MCP servers for tests: the test binary run
// again (TestMain calls Serve), each call echoed as {"server","tool","args"}.
package mcptest

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// marker is the argument that makes the test binary a stub server.
const marker = "abhed-mcptest-serve"

// Tool is one tool a stub offers.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Config is one stub server.
type Config struct {
	// Name is what the stub says it is in each echo.
	Name  string `json:"name"`
	Tools []Tool `json:"tools"`
	// PageSize splits tools/list and prompts/list into pages of this many,
	// joined by nextCursor; zero sends one page.
	PageSize int `json:"page_size,omitempty"`
	// Prompts is how many prompts it offers, named p0, p1, ...
	Prompts int `json:"prompts,omitempty"`
	// Hang never answers initialize.
	Hang bool `json:"hang,omitempty"`
	// Delay waits this long before answering initialize.
	Delay time.Duration `json:"delay,omitempty"`
	// LongLine answers tools/call with one line of at least this many bytes.
	LongLine int `json:"long_line,omitempty"`
	// RepeatCursor sends the same nextCursor on every page.
	RepeatCursor bool `json:"repeat_cursor,omitempty"`
}

// Tools are n tools named prefix0 .. prefix<n-1>.
func Tools(prefix string, n int) []Tool {
	out := make([]Tool, n)
	for i := range out {
		out[i] = Tool{Name: fmt.Sprintf("%s%d", prefix, i), Description: fmt.Sprintf("Stub tool %d of its server.", i)}
	}
	return out
}

// Command is the program and arguments that start a stub with cfg.
func Command(cfg Config) (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, err
	}
	return exe, []string{marker, base64.StdEncoding.EncodeToString(raw)}, nil
}

// Serve makes this process a stub server, and exits, when it was started as
// one; otherwise it returns false at once. Call it first in TestMain.
func Serve() bool {
	if len(os.Args) < 3 || os.Args[1] != marker {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(os.Args[2])
	var cfg Config
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcptest: bad config:", err)
		os.Exit(2)
	}
	Run(cfg, os.Stdin, os.Stdout)
	os.Exit(0)
	return true
}

// Run serves cfg on in and out until in ends.
func Run(cfg Config, in io.Reader, out io.Writer) {
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriter(out)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_, _ = w.Write(append(b, '\n'))
		_ = w.Flush()
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			handle(cfg, line, reply)
		}
		if err != nil {
			return
		}
	}
}

func handle(cfg Config, line []byte, reply func(json.RawMessage, any)) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Cursor    string          `json:"cursor"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
		return // a notification
	}
	switch req.Method {
	case "initialize":
		if cfg.Hang {
			time.Sleep(time.Hour) // never answers; killed when the client closes
		}
		time.Sleep(cfg.Delay)
		caps := map[string]any{"tools": map[string]any{}}
		if cfg.Prompts > 0 {
			caps["prompts"] = map[string]any{}
		}
		reply(req.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps,
			"serverInfo": map[string]any{"name": cfg.Name, "version": "1"}})
	case "tools/list":
		var defs []any
		for _, t := range cfg.Tools {
			defs = append(defs, map[string]any{"name": t.Name, "description": t.Description,
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}})
		}
		page, next := cfg.page(defs, req.Params.Cursor)
		res := map[string]any{"tools": page}
		if next != "" {
			res["nextCursor"] = next
		}
		reply(req.ID, res)
	case "prompts/list":
		var defs []any
		for i := 0; i < cfg.Prompts; i++ {
			defs = append(defs, map[string]any{"name": fmt.Sprintf("p%d", i), "description": "a prompt"})
		}
		page, next := cfg.page(defs, req.Params.Cursor)
		res := map[string]any{"prompts": page}
		if next != "" {
			res["nextCursor"] = next
		}
		reply(req.ID, res)
	case "tools/call":
		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage("null")
		}
		echo, _ := json.Marshal(map[string]any{"server": cfg.Name, "tool": req.Params.Name, "args": args})
		text := string(echo)
		if cfg.LongLine > 0 {
			text += strings.Repeat("x", cfg.LongLine)
		}
		reply(req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false})
	default:
		reply(req.ID, map[string]any{})
	}
}

// page is the page of defs the cursor names, and the cursor of the next.
func (cfg Config) page(defs []any, cursor string) ([]any, string) {
	if cfg.PageSize <= 0 {
		return defs, ""
	}
	start, _ := strconv.Atoi(cursor)
	if start < 0 || start > len(defs) {
		start = len(defs)
	}
	end := min(start+cfg.PageSize, len(defs))
	next := ""
	if end < len(defs) {
		next = strconv.Itoa(end)
	}
	if cfg.RepeatCursor {
		next = "again"
	}
	return defs[start:end], next
}

// Echo is what a stub's tools/call returns.
type Echo struct {
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
}

// ParseEcho reads a stub's answer to a call.
func ParseEcho(text string) (Echo, error) {
	var e Echo
	err := json.Unmarshal([]byte(text), &e)
	return e, err
}
