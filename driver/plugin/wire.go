// Package plugin runs a driver in a separate process, speaking JSON Lines
// over a Unix socket: Serve exposes a driver.Driver on a listener, and Client
// is a driver.Driver that calls one. Each call uses a connection of its own
// and starts with a version handshake. Anything unexpected on the wire fails
// the call; nothing is read as success by default.
package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/zybuu-ai/abhed/driver"
)

// Versions are the protocol versions this package speaks, highest last.
var Versions = []int{1}

const (
	// MaxLine bounds one frame on the wire.
	MaxLine = 1 << 20
	// MaxChunk bounds the decoded bytes in one data frame.
	MaxChunk = 32 << 10
)

// Frame types.
const (
	tHello    = "hello"
	tCall     = "call"
	tResult   = "result"
	tError    = "error"
	tStdin    = "stdin"
	tStdinEOF = "stdin_eof"
	tStdout   = "stdout"
	tStderr   = "stderr"
	tResize   = "resize"
	tData     = "data"
	tEOF      = "eof"
)

// Methods.
const (
	mCreate   = "create"
	mStart    = "start"
	mStop     = "stop"
	mDelete   = "delete"
	mStatus   = "status"
	mExec     = "exec"
	mAttach   = "attach"
	mCopyIn   = "copy_in"
	mCopyOut  = "copy_out"
	mForward  = "forward"
	mSnapshot = "snapshot"
	mRestore  = "restore"
)

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type frame struct {
	Type     string          `json:"type"`
	Versions []int           `json:"versions,omitempty"`
	Version  int             `json:"version,omitempty"`
	Info     *driver.Info    `json:"info,omitempty"`
	Method   string          `json:"method,omitempty"`
	Params   *params         `json:"params,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *wireError      `json:"error,omitempty"`
	Data     []byte          `json:"data,omitempty"`
	Rows     uint16          `json:"rows,omitempty"`
	Cols     uint16          `json:"cols,omitempty"`
}

type params struct {
	ID      string                `json:"id,omitempty"`
	Spec    *driver.Spec          `json:"spec,omitempty"`
	GraceMS int64                 `json:"grace_ms,omitempty"`
	Exec    *driver.ExecRequest   `json:"exec,omitempty"`
	Attach  *driver.AttachRequest `json:"attach,omitempty"`
	Path    string                `json:"path,omitempty"`
	Port    int                   `json:"port,omitempty"`
	Name    string                `json:"name,omitempty"`
	Ref     *driver.SnapshotRef   `json:"ref,omitempty"`
}

// conn is one framed connection. Writes are serialised; reads happen on
// one goroutine at a time.
type conn struct {
	c   net.Conn
	sc  *bufio.Scanner
	wmu sync.Mutex
}

func newConn(c net.Conn) *conn {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	return &conn{c: c, sc: sc}
}

var errProtocol = errors.New("protocol error")

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
	if len(b)+1 > MaxLine {
		return fmt.Errorf("%w: frame of %d bytes is over the limit", errProtocol, len(b))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.c.Write(append(b, '\n'))
	return err
}

func (c *conn) close() error { return c.c.Close() }

// chunkWriter sends what is written to it as frames of type typ.
type chunkWriter struct {
	c   *conn
	typ string
}

func (w chunkWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), MaxChunk)
		if err := w.c.write(frame{Type: w.typ, Data: p[:k]}); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

// pump copies r to frames of type typ, then sends one frame of type end.
func pump(c *conn, r io.Reader, typ, end string) error {
	if r != nil {
		if _, err := io.CopyBuffer(chunkWriter{c, typ}, r, make([]byte, MaxChunk)); err != nil {
			return err
		}
	}
	return c.write(frame{Type: end})
}

func errorFrame(err error) frame {
	return frame{Type: tError, Error: &wireError{Code: driver.Code(err), Message: err.Error()}}
}

// negotiate picks the highest version both sides list, or 0.
func negotiate(offered []int) int {
	best := 0
	for _, o := range offered {
		for _, v := range Versions {
			if o == v && v > best {
				best = v
			}
		}
	}
	return best
}
