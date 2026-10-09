package sandboxcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/wsconn"
)

// control is a text message on a stream (contract §4.3).
type control struct {
	Type       string `json:"type"`
	Generation int64  `json:"generation,omitempty"`
	Code       *int   `json:"code,omitempty"`
	Rows       uint16 `json:"rows,omitempty"`
	Cols       uint16 `json:"cols,omitempty"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func sendControl(ws *wsconn.Conn, c control) error {
	b, _ := json.Marshal(c)
	return ws.WriteMessage(wsconn.Text, b)
}

// remoteError is a stream that ended with the server's error.
func remoteError(c control) error {
	if c.Error == nil {
		return errors.New("the stream ended with an error")
	}
	return &apiError{status: codeStatus[c.Error.Code], code: c.Error.Code, msg: c.Error.Message}
}

// codeStatus is the HTTP status of a contract error code, for one that
// arrives in a stream rather than as a status.
var codeStatus = map[string]int{
	"not_found": http.StatusNotFound, "forbidden": http.StatusForbidden, "approval_required": http.StatusForbidden,
	"quota_exceeded": http.StatusForbidden, "conflict": http.StatusConflict, "precondition_failed": http.StatusPreconditionFailed,
	"invalid": http.StatusBadRequest, "unsupported": http.StatusNotImplemented, "unavailable": http.StatusServiceUnavailable,
	"internal": http.StatusInternalServerError,
}

// started reads the stream's first message, which must be "started".
func started(ws *wsconn.Conn) (control, error) {
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer func() { _ = ws.SetReadDeadline(time.Time{}) }()
	typ, msg, err := ws.ReadMessage()
	if err != nil {
		return control{}, fmt.Errorf("the stream ended before it started: %w", err)
	}
	var c control
	if typ != wsconn.Text || json.Unmarshal(msg, &c) != nil {
		return control{}, errors.New("the server's stream did not start as the contract says")
	}
	switch c.Type {
	case "started":
		return c, nil
	case "error":
		return c, remoteError(c)
	}
	return c, fmt.Errorf("the stream began with %q", c.Type)
}

func (r *runner) exec(args []string) error {
	fs := r.flags("exec", "[-i] ID [--] COMMAND [ARG...]")
	stdin := fs.Bool("i", false, "send this command's stdin to it")
	shell := fs.String("c", "", "a command line for the sandbox's shell, instead of COMMAND and ARGs")
	cwd := fs.String("cwd", "", "the folder to run it in, inside the workspace")
	var env multi
	fs.Var(&env, "e", "an environment variable NAME=VALUE (repeatable)")
	pos, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageError{errors.New("exec takes a sandbox id and a command")}
	}
	id, err := oneID(pos[:1], "exec")
	if err != nil {
		return err
	}
	req := controlplane.ExecRequest{Argv: pos[1:], Command: *shell, Cwd: *cwd, Env: env}
	if (len(req.Argv) == 0) == (req.Command == "") {
		return usageError{errors.New("exec takes a COMMAND [ARG...] or -c LINE, not both")}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	if *stdin {
		return r.execStream(id, req)
	}
	return r.execPost(id, req)
}

// execFrame is one NDJSON line of a POST :exec.
type execFrame struct {
	Stdout []byte `json:"stdout,omitempty"`
	Stderr []byte `json:"stderr,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// execPost runs a command without stdin. A stream broken before its last
// frame is asked for again under the same Idempotency-Key, which answers
// with how the command ended rather than running it twice.
func (r *runner) execPost(id string, req controlplane.ExecRequest) error {
	body, _ := json.Marshal(req)
	key := newKey()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := r.cl.do(r.Ctx, call{method: http.MethodPost, path: sandboxPath(id, "exec"), body: body, mutating: true,
			header: http.Header{"Idempotency-Key": {key}}, again: attempt > 0})
		if err != nil {
			return err
		}
		code, done, err := r.readFrames(resp.Body)
		_ = resp.Body.Close()
		if done {
			if err != nil {
				fmt.Fprintf(r.Err, "abhed: %v\n", err)
				return exitStatus(ExitRemote)
			}
			return exitStatus(code)
		}
		if r.Ctx.Err() != nil {
			return r.Ctx.Err()
		}
	}
	fmt.Fprintln(r.Err, "abhed: the command's stream broke and its end could not be read")
	return exitStatus(ExitRemote)
}

// readFrames copies output frames until the last one; done is false when
// the stream broke first.
func (r *runner) readFrames(body io.Reader) (code int, done bool, err error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var f execFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return 0, true, fmt.Errorf("an output frame could not be read: %w", err)
		}
		switch {
		case f.Exit != nil:
			return *f.Exit, true, nil
		case f.Error != nil:
			return 0, true, &apiError{msg: f.Error.Message + " (" + f.Error.Code + ")"}
		}
		if len(f.Stdout) > 0 {
			_, _ = r.Out.Write(f.Stdout)
		}
		if len(f.Stderr) > 0 {
			_, _ = r.Err.Write(f.Stderr)
		}
	}
	return 0, false, sc.Err()
}

// execStream runs a command with this process's stdin, over :exec's WebSocket.
func (r *runner) execStream(id string, req controlplane.ExecRequest) error {
	ws, err := r.cl.dial(r.Ctx, sandboxPath(id, "exec"), server.ExecProtocol)
	if err != nil {
		return err
	}
	defer ws.Abort()
	first, _ := json.Marshal(req)
	if err := ws.WriteMessage(wsconn.Text, first); err != nil {
		return err
	}
	if _, err := started(ws); err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := r.In.Read(buf)
			if n > 0 && ws.WriteMessage(wsconn.Binary, buf[:n]) != nil {
				return
			}
			if err != nil {
				_ = sendControl(ws, control{Type: "eof"})
				return
			}
		}
	}()
	stop := context.AfterFunc(r.Ctx, ws.Abort)
	defer stop()
	for {
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			fmt.Fprintf(r.Err, "abhed: the stream ended before the command did: %v\n", err)
			return exitStatus(ExitRemote)
		}
		if typ == wsconn.Binary {
			if len(msg) > 0 {
				w := r.Out
				if msg[0] == 2 {
					w = r.Err
				}
				_, _ = w.Write(msg[1:])
			}
			continue
		}
		var c control
		_ = json.Unmarshal(msg, &c)
		switch c.Type {
		case "exit":
			if c.Code != nil {
				return exitStatus(*c.Code)
			}
		case "error":
			fmt.Fprintf(r.Err, "abhed: %v\n", remoteError(c))
			return exitStatus(ExitRemote)
		}
	}
}

// escapeByte reads -escape: "^]" style, or "none".
func escapeByte(v string) (byte, bool, error) {
	switch {
	case v == "none":
		return 0, false, nil
	case len(v) == 2 && v[0] == '^' && v[1] >= '@' && v[1] <= '_':
		return v[1] - '@', true, nil
	}
	return 0, false, usageError{fmt.Errorf("-escape %q: use ^ and a character, such as ^], or none", v)}
}

func (r *runner) connect(args []string) error {
	fs := r.flags("connect", "ID | --stdio ID")
	stdio := fs.Bool("stdio", false, "join stdin and stdout to the sandbox's SSH server (for ssh's ProxyCommand)")
	cwd := fs.String("cwd", "", "the folder the shell starts in")
	esc := fs.String("escape", "^]", "the key that leaves the session, or none")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	id, err := oneID(pos, "connect")
	if err != nil {
		return err
	}
	escape, hasEscape, err := escapeByte(*esc)
	if err != nil {
		return err
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	if *stdio {
		return r.connectStdio(id)
	}
	q := url.Values{}
	if r.Term != nil {
		if rows, cols, ok := r.Term.Size(); ok {
			q.Set("rows", strconv.Itoa(int(rows)))
			q.Set("cols", strconv.Itoa(int(cols)))
		}
	}
	if *cwd != "" {
		q.Set("cwd", *cwd)
	}
	path := sandboxPath(id, "attach")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	ws, err := r.cl.dial(r.Ctx, path, server.AttachProtocol)
	if err != nil {
		return err
	}
	defer ws.Abort()
	if _, err := started(ws); err != nil {
		return err
	}
	if r.Term != nil {
		restore, err := r.Term.MakeRaw()
		if err != nil {
			return fmt.Errorf("the terminal could not be put in raw mode: %w", err)
		}
		defer restore()
		resized, stopResize := r.Term.Resized()
		defer stopResize()
		go func() {
			for range resized {
				if rows, cols, ok := r.Term.Size(); ok {
					_ = sendControl(ws, control{Type: "resize", Rows: rows, Cols: cols})
				}
			}
		}()
	}
	var left sync.Once
	leftFlag := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.In.Read(buf)
			chunk := buf[:n]
			if hasEscape {
				if i := bytes.IndexByte(chunk, escape); i >= 0 {
					if i > 0 {
						_ = ws.WriteMessage(wsconn.Binary, chunk[:i])
					}
					left.Do(func() { close(leftFlag) })
					_ = ws.CloseWith(wsconn.CloseNormal, "left")
					return
				}
			}
			if n > 0 && ws.WriteMessage(wsconn.Binary, chunk) != nil {
				return
			}
			if err != nil {
				// No more keys: the shell sees its input end.
				_ = ws.WriteMessage(wsconn.Binary, []byte{4})
				return
			}
		}
	}()
	stop := context.AfterFunc(r.Ctx, ws.Abort)
	defer stop()
	for {
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			select {
			case <-leftFlag:
				fmt.Fprint(r.Err, "\r\nabhed: left the session\r\n")
				return nil
			default:
			}
			if wsconn.IsClose(err) {
				return exitStatus(ExitRemote)
			}
			return fmt.Errorf("the session ended: %w", err)
		}
		if typ == wsconn.Binary {
			_, _ = r.Out.Write(msg)
			continue
		}
		var c control
		_ = json.Unmarshal(msg, &c)
		switch c.Type {
		case "exit":
			if c.Code != nil {
				return exitStatus(*c.Code)
			}
		case "error":
			if restoreErr := remoteError(c); restoreErr != nil {
				fmt.Fprintf(r.Err, "\r\nabhed: %v\r\n", restoreErr)
			}
			return exitStatus(ExitRemote)
		}
	}
}

// connectStdio relays stdin and stdout over the sandbox's SSH relay until
// either side ends: ssh's ProxyCommand end.
func (r *runner) connectStdio(id string) error {
	ws, err := r.cl.dial(r.Ctx, sandboxPath(id, "ssh"), server.SSHRelayProtocol)
	if err != nil {
		return fmt.Errorf("the SSH relay: %w", err)
	}
	stream := wsconn.NewStream(ws)
	inDone, outDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := io.Copy(stream, r.In)
		inDone <- err
	}()
	go func() {
		_, err := io.Copy(r.Out, stream)
		outDone <- err
	}()
	var outErr error
	finished := false
	select {
	case outErr = <-outDone:
		finished = true
	case err := <-inDone:
		// Input ended: the reply may still be on its way.
		select {
		case outErr = <-outDone:
			finished = true
		case <-time.After(5 * time.Second):
		case <-r.Ctx.Done():
		}
		if err != nil && !errors.Is(err, wsconn.ErrClosed) {
			outErr = err
		}
	case <-r.Ctx.Done():
	}
	_ = ws.Close()
	if !finished {
		// Nothing is written to out once this returns.
		select {
		case <-outDone:
		case <-time.After(3 * time.Second):
			ws.Abort()
			<-outDone
		}
	}
	if outErr == nil || wsconn.IsClose(outErr) || errors.Is(outErr, wsconn.ErrClosed) {
		return nil
	}
	return outErr
}

func (r *runner) forward(args []string) error {
	fs := r.flags("forward", "ID [LOCAL:]REMOTE")
	addr := fs.String("address", "127.0.0.1", "the local address to listen on; anything but loopback lets others in")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError{errors.New("forward takes a sandbox id and [LOCAL:]REMOTE")}
	}
	id, err := oneID(pos[:1], "forward")
	if err != nil {
		return err
	}
	local, remote := "0", pos[1]
	if l, rem, ok := strings.Cut(pos[1], ":"); ok {
		local, remote = l, rem
	}
	rport, err := strconv.Atoi(remote)
	if err != nil || rport < 1 || rport > 65535 {
		return usageError{fmt.Errorf("remote port %q: a number from 1 to 65535", remote)}
	}
	if lport, err := strconv.Atoi(local); err != nil || lport < 0 || lport > 65535 {
		return usageError{fmt.Errorf("local port %q: a number from 0 to 65535", local)}
	}
	ip := net.ParseIP(*addr)
	if *addr != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fmt.Fprintf(r.Err, "abhed: warning: listening on %s, not loopback: anyone who can reach it reaches port %d in the sandbox as you\n", *addr, rport)
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	// The sandbox must be there before anything listens for it.
	if _, _, err := r.getSandbox(id); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(*addr, local))
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	stop := context.AfterFunc(r.Ctx, func() { _ = ln.Close() })
	defer stop()
	fmt.Fprintf(r.Out, "Forwarding %s -> %s port %d (Ctrl-C to stop)\n", ln.Addr(), id, rport)
	if r.Listening != nil {
		r.Listening(ln.Addr())
	}
	path := sandboxPath(id, "forward") + "?port=" + strconv.Itoa(rport)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if r.Ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			if err := r.forwardOne(conn, path); err != nil {
				fmt.Fprintf(r.Err, "abhed: forward from %s: %v\n", conn.RemoteAddr(), err)
			}
		}()
	}
}

// forwardOne carries one local connection over its own :forward stream.
func (r *runner) forwardOne(conn net.Conn, path string) error {
	ws, err := r.cl.dial(r.Ctx, path, server.ForwardProtocol)
	if err != nil {
		return err
	}
	defer ws.Abort()
	if _, err := started(ws); err != nil {
		return err
	}
	stop := context.AfterFunc(r.Ctx, ws.Abort)
	defer stop()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 && ws.WriteMessage(wsconn.Binary, buf[:n]) != nil {
				return
			}
			if err != nil {
				_ = sendControl(ws, control{Type: "eof"})
				return
			}
		}
	}()
	for {
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			if wsconn.IsClose(err) {
				return nil
			}
			return err
		}
		if typ == wsconn.Binary {
			if _, err := conn.Write(msg); err != nil {
				return nil
			}
			continue
		}
		var c control
		if json.Unmarshal(msg, &c) == nil && c.Type == "error" {
			return remoteError(c)
		}
	}
}
