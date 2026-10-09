// Package guest is the protocol between the microvm driver on the host and
// the agent inside the guest (cmd/abhed-guest), over a vsock connection.
//
// One connection carries one call. Frames are JSON Lines, at most MaxLine
// bytes, with byte payloads base64 in "data". The host opens with a hello,
// the guest answers with its own, then the host sends one request:
//
//	host  → {"type":"hello","version":1}
//	guest → {"type":"hello","version":1}
//	host  → {"type":"exec","exec":{…}}, {"type":"ping"} or {"type":"shutdown"}
//	host  → stdin, stdin_eof, resize frames   (exec only)
//	guest → stdout, stderr frames, then {"type":"exit","code":N} or an error
//
// Everything the guest sends is untrusted: frames are size-limited, and a
// connection that ends without an exit or error frame is an error, never an
// exit code.
package guest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Version is the protocol version this package speaks.
const Version = 1

// Port is the vsock port the guest agent listens on.
const Port = 1024

const (
	// MaxLine bounds one frame on the wire.
	MaxLine = 1 << 20
	// MaxChunk bounds the decoded bytes in one data frame.
	MaxChunk = 32 << 10
)

const (
	tHello    = "hello"
	tExec     = "exec"
	tShutdown = "shutdown"
	tPing     = "ping"
	tStdin    = "stdin"
	tStdinEOF = "stdin_eof"
	tResize   = "resize"
	tStdout   = "stdout"
	tStderr   = "stderr"
	tExit     = "exit"
	tOK       = "ok"
	tError    = "error"
)

// ExecRequest is one command for the guest to run.
type ExecRequest struct {
	// Exactly one of Argv and Command is set; Command runs under /bin/sh -c.
	Argv    []string `json:"argv,omitempty"`
	Command string   `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Env     []string `json:"env,omitempty"`
	// TTY runs an interactive shell on a terminal of Rows by Cols; Argv and
	// Command are then empty.
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

func (r ExecRequest) validate() error {
	if r.TTY {
		if len(r.Argv) != 0 || r.Command != "" {
			return errors.New("a terminal request runs the shell and names no command")
		}
		return nil
	}
	if (len(r.Argv) == 0) == (r.Command == "") {
		return errors.New("exactly one of argv and command must be set")
	}
	return nil
}

type frame struct {
	Type    string       `json:"type"`
	Version int          `json:"version,omitempty"`
	Exec    *ExecRequest `json:"exec,omitempty"`
	Data    []byte       `json:"data,omitempty"`
	Rows    uint16       `json:"rows,omitempty"`
	Cols    uint16       `json:"cols,omitempty"`
	Code    *int         `json:"code,omitempty"`
	Message string       `json:"message,omitempty"`
}

var errProtocol = errors.New("guest protocol error")

// conn frames one connection. Writes are serialised; one goroutine reads.
type conn struct {
	rw  io.ReadWriteCloser
	sc  *bufio.Scanner
	wmu sync.Mutex
}

func newConn(rw io.ReadWriteCloser) *conn {
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	return &conn{rw: rw, sc: sc}
}

func (c *conn) read() (frame, error) {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return frame{}, fmt.Errorf("%w: %w", errProtocol, err)
		}
		return frame{}, io.ErrUnexpectedEOF
	}
	var f frame
	if err := json.Unmarshal(c.sc.Bytes(), &f); err != nil {
		return frame{}, fmt.Errorf("%w: malformed frame: %w", errProtocol, err)
	}
	if f.Type == "" {
		return frame{}, fmt.Errorf("%w: frame without a type", errProtocol)
	}
	if len(f.Data) > MaxChunk {
		return frame{}, fmt.Errorf("%w: data frame of %d bytes is over the limit", errProtocol, len(f.Data))
	}
	return f, nil
}

func (c *conn) write(f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) >= MaxLine {
		return fmt.Errorf("%w: frame of %d bytes is over the limit", errProtocol, len(b))
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(b)
	return err
}

// chunks writes p as frames of type typ, each at most MaxChunk bytes.
func (c *conn) chunks(typ string, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), MaxChunk)
		if err := c.write(frame{Type: typ, Data: p[:n]}); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}
