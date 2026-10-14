package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/server/sshrelay"
	"github.com/zybuu-ai/abhed/wsconn"
)

// clientEnded reports err as the client having closed the stream, or a
// cancellation that followed it, which ends a stream normally.
func clientEnded(gone *atomic.Bool, err error) bool {
	return gone.Load() && (err == nil || errors.Is(err, context.Canceled))
}

// guardEnded is the guard's reason when it ended the stream ctx belongs to,
// which the client is told rather than a normal end.
func guardEnded(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, controlplane.ErrEndedByGuard) {
		return cause
	}
	return nil
}

// attachSandbox opens the sandbox's interactive shell over a WebSocket
// (abhed.attach.v1): binary messages are keys one way and the terminal's
// output the other; {"type":"resize","rows","cols"} changes its size; the
// shell's end is {"type":"exit","code"}, a failure {"type":"error"}.
func (s *Server) attachSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	st, ok := s.openStream(w, r, call, protoAttach)
	if !ok {
		return
	}
	defer st.release()
	pol, err := s.sandboxPolicy(r.Context(), call.id)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	if why := s.terminalRefusal(pol); why != "" {
		writeCPError(w, http.StatusForbidden, "forbidden", why)
		return
	}
	size, err := queryWinSize(r)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	cwd := r.URL.Query().Get("cwd")
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	var ws *wsconn.Conn
	detail := map[string]string{"rows": strconv.Itoa(int(size.Rows)), "cols": strconv.Itoa(int(size.Cols))}
	if cwd != "" {
		detail["cwd"] = s.opts.ControlPlane.Redact(cwd)
	}
	_, err = s.opts.ControlPlane.Stream(ctx, call.caller, call.id, controlplane.StreamRequest{Action: "attach", End: cancel,
		Capability: driver.CapAttach, Detail: detail, Screen: func() error { return terminalDenied(pol) }},
		func(d driver.Driver, sb controlplane.Sandbox) (map[string]string, error) {
			var err error
			if ws, err = accept(w, r, st.proto); err != nil {
				return nil, err
			}
			wait := s.watchStream(ctx, cancel, r, sb, d, ws, streamBounds{})
			_ = sendControl(ws, streamControl{Type: "started", Generation: sb.Generation})
			var gone atomic.Bool
			keys, resize := make(chan []byte, 16), make(chan driver.WinSize, 4)
			go func() {
				defer close(keys)
				for {
					typ, msg, err := ws.ReadMessage()
					if err != nil {
						gone.Store(true)
						cancel(nil)
						return
					}
					if typ == wsconn.Binary {
						select {
						case keys <- msg:
						case <-ctx.Done():
							return
						}
						continue
					}
					var c streamControl
					if json.Unmarshal(msg, &c) == nil && c.Type == "resize" && c.Rows > 0 && c.Cols > 0 {
						select {
						case resize <- driver.WinSize{Rows: min(c.Rows, 4096), Cols: min(c.Cols, 4096)}:
						default:
						}
					}
				}
			}()
			term := s.newSandboxTerminal(ctx, call.caller, sb, pol, wsconn.NewStream(ws))
			out, err := term.run(ctx, d, cwd, size, keys, resize)
			if why := wait(); guardEnded(ctx) != nil {
				err = guardEnded(ctx)
			} else if why != nil {
				err = why
			} else if clientEnded(&gone, err) {
				out["ended_by"], err = "client", nil
			}
			if code, perr := strconv.Atoi(out["exit_code"]); err == nil && perr == nil {
				_ = sendControl(ws, streamControl{Type: "exit", Code: &code})
			}
			endStreamWith(ws, err)
			return out, err
		})
	if err != nil && ws == nil {
		s.failCP(w, r, err)
	}
}

// prefixed writes each chunk as a binary message led by its stream's byte.
type prefixed struct {
	ws *wsconn.Conn
	fd byte
}

func (p prefixed) Write(b []byte) (int, error) {
	for off := 0; off < len(b); off += maxExecFrame {
		chunk := b[off:min(off+maxExecFrame, len(b))]
		if err := p.ws.WriteMessage(wsconn.Binary, append([]byte{p.fd}, chunk...)); err != nil {
			return off, err
		}
	}
	return len(b), nil
}

// execStream runs one command with stdin over a WebSocket (abhed.exec.v1).
// The first message is the request as JSON text; then binary messages are
// stdin and {"type":"eof"} ends it. Output comes back as binary messages
// led by 1 (stdout) or 2 (stderr), and the end as {"type":"exit","code"}
// or {"type":"error"}.
func (s *Server) execStream(w http.ResponseWriter, r *http.Request, call verbCall) {
	st, ok := s.openStream(w, r, call, protoExec)
	if !ok {
		return
	}
	defer st.release()
	if !controlplane.AcceptsWork(st.sb.State) {
		s.failCP(w, r, fmt.Errorf("%w: sandbox %s is %s; only Ready takes exec", controlplane.ErrInvalidTransition, call.id, st.sb.State))
		return
	}
	pol, err := s.sandboxPolicy(r.Context(), call.id)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	ws, err := accept(w, r, st.proto)
	if err != nil {
		return
	}
	_ = ws.SetReadDeadline(time.Now().Add(streamFirstWait))
	typ, msg, err := ws.ReadMessage()
	_ = ws.SetReadDeadline(time.Time{})
	if err != nil {
		ws.Abort()
		return
	}
	var req controlplane.ExecRequest
	if typ != wsconn.Text {
		endStreamWith(ws, fmt.Errorf("%w: the first message is the request, as JSON text", controlplane.ErrInvalid))
		return
	}
	if err := decodeStrict(msg, &req); err != nil {
		endStreamWith(ws, err)
		return
	}
	req.Stdin = true
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	pr, pw := io.Pipe()
	in := newStdinQueue(pw)
	var gone atomic.Bool
	go func() {
		defer in.end()
		for {
			typ, msg, err := ws.ReadMessage()
			if err != nil {
				gone.Store(true)
				_ = pw.CloseWithError(io.ErrUnexpectedEOF)
				cancel(nil)
				return
			}
			if typ == wsconn.Text {
				var c streamControl
				if json.Unmarshal(msg, &c) == nil && c.Type == "eof" {
					in.end()
				}
				continue
			}
			if !in.put(ctx, msg) {
				return
			}
		}
	}()
	var wait func() error
	stdin := s.newStdinScreen(ctx, pol, call.caller, st.sb, pr)
	res, _, err := s.opts.ControlPlane.Exec(ctx, call.caller, call.id, "", req, controlplane.ExecIO{
		Stdin: stdin, Stdout: prefixed{ws, 1}, Stderr: prefixed{ws, 2},
		Screen:  func(command string) error { return s.screenStdinExec(pol, command, req.Env, stdin) },
		Refused: stdin.refused,
		Started: func(sb controlplane.Sandbox) {
			stdin.sb = sb
			_ = sendControl(ws, streamControl{Type: "started", Generation: sb.Generation})
			wait = s.watchStream(ctx, cancel, r, sb, nil, ws, streamBounds{busy: in.full, resumed: in.since})
		}})
	_ = pr.Close()
	if wait != nil {
		if why := wait(); why != nil {
			err = why
		}
	}
	if gone.Load() {
		ws.Abort()
		return
	}
	if err == nil {
		_ = sendControl(ws, streamControl{Type: "exit", Code: &res.ExitCode})
	}
	endStreamWith(ws, err)
}

// stdinQueue carries an exec's stdin from the WebSocket's reader to the
// command, so the reader goes on reading, and answering pings, while the
// command is slow to take it. It holds at most stdinQueued chunks; past that
// the reader waits for the command, as TCP would, rather than drop input,
// and full says so, for the client is not the one that stopped. Stdin the
// command no longer reads at all, once it closed it, is dropped, as is
// anything put after end.
type stdinQueue struct {
	ch      chan []byte
	ended   chan struct{}
	once    sync.Once
	isFull  atomic.Bool
	resumed atomic.Int64
}

const (
	stdinChunk  = 32 << 10
	stdinQueued = 32
)

func newStdinQueue(pw *io.PipeWriter) *stdinQueue {
	q := &stdinQueue{ch: make(chan []byte, stdinQueued), ended: make(chan struct{})}
	// ch is never closed, so a put racing end cannot panic; what was queued
	// before end still reaches the command.
	go func() {
		defer func() { _ = pw.Close() }()
		for {
			select {
			case b := <-q.ch:
				_, _ = pw.Write(b)
			case <-q.ended:
				for {
					select {
					case b := <-q.ch:
						_, _ = pw.Write(b)
					default:
						return
					}
				}
			}
		}
	}()
	return q
}

// put queues msg, false when ctx ended first; after end it drops msg. Once
// it has had to wait, full stays true until the whole message is queued and
// the reader reads again.
func (q *stdinQueue) put(ctx context.Context, msg []byte) bool {
	waited := false
	defer func() {
		if waited {
			q.resumed.Store(time.Now().UnixNano())
			q.isFull.Store(false)
		}
	}()
	for off := 0; off < len(msg); off += stdinChunk {
		b := msg[off:min(off+stdinChunk, len(msg))]
		select {
		case <-q.ended:
			return true
		default:
		}
		select {
		case q.ch <- b:
			continue
		default:
		}
		waited = true
		q.isFull.Store(true)
		select {
		case q.ch <- b:
		case <-q.ended:
			return true
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// end is stdin's end, once everything queued has gone.
func (q *stdinQueue) end() { q.once.Do(func() { close(q.ended) }) }

func (q *stdinQueue) full() bool { return q.isFull.Load() }

// since is when the reader last went back to reading after waiting, zero
// if it never waited.
func (q *stdinQueue) since() time.Time {
	if n := q.resumed.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// counted is a sandbox stream counting its bytes each way, closed once.
type counted struct {
	io.ReadWriteCloser
	in, out atomic.Int64
	once    sync.Once
	closed  chan struct{}
}

func newCounted(rwc io.ReadWriteCloser) *counted {
	return &counted{ReadWriteCloser: rwc, closed: make(chan struct{})}
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.out.Add(int64(n))
	return n, err
}

func (c *counted) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	c.in.Add(int64(n))
	return n, err
}

func (c *counted) CloseWrite() error {
	if cw, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *counted) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ReadWriteCloser.Close()
		close(c.closed)
	})
	return err
}

func (c *counted) detail() map[string]string {
	return map[string]string{"bytes_in": strconv.FormatInt(c.in.Load(), 10), "bytes_out": strconv.FormatInt(c.out.Load(), 10)}
}

// errClientEOF is the client ending what it sends on a forward.
var errClientEOF = errors.New("the client ended what it sends")

// forwardSandbox carries a byte stream to a port on the sandbox's own
// loopback over a WebSocket (abhed.forward.v1), through the driver's
// channel. Binary messages are the bytes; {"type":"eof"} from the client
// ends what it sends and leaves the reply to be read.
func (s *Server) forwardSandbox(w http.ResponseWriter, r *http.Request, call verbCall) {
	st, ok := s.openStream(w, r, call, protoForward)
	if !ok {
		return
	}
	defer st.release()
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		writeCPError(w, http.StatusBadRequest, "invalid", "port must be a whole number from 1 to 65535")
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	var ws *wsconn.Conn
	_, err = s.opts.ControlPlane.Stream(ctx, call.caller, call.id, controlplane.StreamRequest{Action: "forward", End: cancel,
		Capability: driver.CapForward, Detail: map[string]string{"port": strconv.Itoa(port)}},
		func(d driver.Driver, sb controlplane.Sandbox) (map[string]string, error) {
			// Opened before the upgrade, so nothing listening is an HTTP answer.
			raw, err := driver.Forward(ctx, d, sb.ID, port)
			if err != nil {
				return nil, err
			}
			rwc := newCounted(raw)
			defer func() { _ = rwc.Close() }()
			if ws, err = accept(w, r, st.proto); err != nil {
				return nil, err
			}
			wait := s.watchStream(ctx, cancel, r, sb, d, ws, streamBounds{})
			_ = sendControl(ws, streamControl{Type: "started", Generation: sb.Generation})
			stop := context.AfterFunc(ctx, func() { _ = rwc.Close() })
			defer stop()
			stream := wsconn.NewStream(ws)
			stream.OnText = func(msg []byte) error {
				var c streamControl
				if json.Unmarshal(msg, &c) == nil && c.Type == "eof" {
					return errClientEOF
				}
				return nil
			}
			var gone atomic.Bool
			go func() {
				_, err := io.Copy(rwc, stream)
				if errors.Is(err, errClientEOF) {
					_ = rwc.CloseWrite()
					// Still read, for the client's close or its connection
					// going, so a vanished client ends the forward now rather
					// than at the idle check; nothing it sends now is carried.
					for {
						if _, _, err := ws.ReadMessage(); err != nil {
							break
						}
					}
				}
				gone.Store(true)
				_ = rwc.Close()
			}()
			_, _ = io.Copy(stream, rwc)
			_ = rwc.Close()
			err = wait()
			if why := guardEnded(ctx); why != nil {
				err = why
			} else if gone.Load() && err != nil && errors.Is(err, context.Canceled) {
				err = nil
			}
			endStreamWith(ws, err)
			return rwc.detail(), err
		})
	if err != nil && ws == nil {
		s.failCP(w, r, err)
	}
}

// sshInfo answers what a client needs to reach the sandbox over SSH
// through the relay: the host key to trust and where the relay is. The
// relay authenticates the caller, so SSH itself asks for no credentials.
func (s *Server) sshInfo(w http.ResponseWriter, r *http.Request, call verbCall) {
	if !cpLimit(s.cpExec, w, call.caller) {
		return
	}
	sb, err := s.opts.ControlPlane.Get(r.Context(), call.caller, call.id)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	key := s.opts.SandboxSSHKey
	if key == nil {
		writeCPError(w, http.StatusNotImplemented, "unsupported", "this server has no SSH host key, so it serves no SSH relay")
		return
	}
	alias := "abhed-" + sb.ID
	hostKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key.PublicKey())))
	WriteJSON(w, http.StatusOK, map[string]any{
		"host_alias":  alias,
		"user":        "sandbox",
		"host_key":    hostKey,
		"fingerprint": ssh.FingerprintSHA256(key.PublicKey()),
		"known_hosts": alias + " " + hostKey,
		"relay":       "/v1/sandboxes/" + sb.ID + ":ssh",
		"subprotocol": protoSSH,
		"generation":  sb.Generation,
	})
}

// sshRelay carries one SSH connection over a WebSocket (abhed.ssh.v1) to
// the server's own SSH server for this sandbox. Binary messages are the
// raw SSH stream; there are no text messages.
func (s *Server) sshRelay(w http.ResponseWriter, r *http.Request, call verbCall) {
	key := s.opts.SandboxSSHKey
	if key == nil {
		if _, err := s.opts.ControlPlane.Get(r.Context(), call.caller, call.id); err != nil {
			s.failCP(w, r, err)
			return
		}
		writeCPError(w, http.StatusNotImplemented, "unsupported", "this server has no SSH host key, so it serves no SSH relay")
		return
	}
	st, ok := s.openStream(w, r, call, protoSSH)
	if !ok {
		return
	}
	defer st.release()
	pol, err := s.sandboxPolicy(r.Context(), call.id)
	if err != nil {
		s.failCP(w, r, err)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	var ws *wsconn.Conn
	_, err = s.opts.ControlPlane.Stream(ctx, call.caller, call.id, controlplane.StreamRequest{Action: "ssh", End: cancel},
		func(d driver.Driver, sb controlplane.Sandbox) (map[string]string, error) {
			var err error
			if ws, err = accept(w, r, st.proto); err != nil {
				return nil, err
			}
			// SSH has its own bounds: an editor's server lives longer than an exec may.
			vals := s.opts.ControlPlane.Settings().For(sb.Settings)
			maxSession := vals.Seconds(controlplane.SetSSHMaxSession)
			conn := &activeConn{Conn: wsconn.NewStream(ws)}
			conn.touch()
			wait := s.watchStream(ctx, cancel, r, sb, d, ws,
				streamBounds{max: maxSession, idle: vals.Seconds(controlplane.SetSSHIdle), active: conn.last})
			h := &sshSandbox{s: s, caller: call.caller, sb: sb, pol: pol, until: time.Now().Add(maxSession)}
			stats, err := sshrelay.Serve(ctx, conn, sshrelay.Config{HostKey: key, Log: s.log}, h)
			if why := wait(); guardEnded(ctx) != nil {
				err = guardEnded(ctx)
			} else if why != nil {
				err = why
			}
			endStreamWith(ws, err)
			return map[string]string{"sessions": strconv.FormatInt(stats.Sessions, 10), "forwards": strconv.FormatInt(stats.Forwards, 10),
				"refused": strconv.FormatInt(stats.Refused, 10), "ssh_user": s.opts.ControlPlane.Redact(stats.User)}, err
		})
	if err != nil && ws == nil {
		s.failCP(w, r, err)
	}
}

// sshSandbox is what an SSH connection runs in its sandbox: each session
// and forward its own recorded, screened and admitted call, as the
// equivalent verb would be.
type sshSandbox struct {
	s      *Server
	caller controlplane.Caller
	sb     controlplane.Sandbox
	pol    *policy.Engine
	// until is when the connection's ssh.max_session_seconds runs out.
	until time.Time
}

// activeConn notes when bytes last went either way.
type activeConn struct {
	net.Conn
	at atomic.Int64
}

func (c *activeConn) touch()          { c.at.Store(time.Now().UnixNano()) }
func (c *activeConn) last() time.Time { return time.Unix(0, c.at.Load()) }

func (c *activeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *activeConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

// limited holds each SSH session to the caller's exec rate, as the verb
// it amounts to would be.
func (h *sshSandbox) limited() error {
	if l := h.s.cpExec; l != nil && !l.allow(h.caller.Principal) {
		return fmt.Errorf("%w: too many requests; try again later", controlplane.ErrUnavailable)
	}
	return nil
}

func (h *sshSandbox) Shell(ctx context.Context, sess sshrelay.Session) (int, error) {
	if err := h.limited(); err != nil {
		return 1, err
	}
	if why := h.s.terminalRefusal(h.pol); why != "" {
		return 1, fmt.Errorf("%w: %s", controlplane.ErrForbidden, why)
	}
	var size driver.WinSize
	if sess.PTY != nil {
		size = sess.PTY.Size
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	code := 0
	_, err := h.s.opts.ControlPlane.Stream(ctx, h.caller, h.sb.ID, controlplane.StreamRequest{Action: "attach",
		Capability: driver.CapAttach, Detail: map[string]string{"via": "ssh", "rows": strconv.Itoa(int(size.Rows)), "cols": strconv.Itoa(int(size.Cols))},
		Screen: func() error { return terminalDenied(h.pol) }},
		func(d driver.Driver, sb controlplane.Sandbox) (map[string]string, error) {
			keys := make(chan []byte, 16)
			go func() {
				defer close(keys)
				buf := make([]byte, 32<<10)
				for {
					n, err := sess.Stdin.Read(buf)
					if n > 0 {
						select {
						case keys <- append([]byte(nil), buf[:n]...):
						case <-ctx.Done():
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			term := h.s.newSandboxTerminal(ctx, h.caller, sb, h.pol, sess.Stdout)
			out, err := term.run(ctx, d, "", size, keys, sess.Resize)
			code, _ = strconv.Atoi(out["exit_code"])
			return out, err
		})
	return code, err
}

func (h *sshSandbox) Exec(ctx context.Context, command string, sess sshrelay.Session) (int, error) {
	if err := h.limited(); err != nil {
		return 1, err
	}
	// Bounded by the connection's own limits, not exec.timeout_seconds; its
	// output is still held to exec.output_bytes on this channel.
	left := time.Until(h.until)
	if left <= 0 {
		return 1, fmt.Errorf("%w: the SSH connection has reached ssh.max_session_seconds", controlplane.ErrUnavailable)
	}
	stdin := h.s.newStdinScreen(ctx, h.pol, h.caller, h.sb, sess.Stdin)
	res, _, err := h.s.opts.ControlPlane.Exec(ctx, h.caller, h.sb.ID, "", controlplane.ExecRequest{Command: command, Stdin: true},
		controlplane.ExecIO{Stdin: stdin, Stdout: sess.Stdout, Stderr: sess.Stderr, Timeout: left,
			Screen:  func(command string) error { return h.s.screenStdinExec(h.pol, command, nil, stdin) },
			Refused: stdin.refused, Started: func(sb controlplane.Sandbox) { stdin.sb = sb }})
	return res.ExitCode, err
}

func (h *sshSandbox) Forward(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	if err := h.limited(); err != nil {
		return nil, err
	}
	got := make(chan error, 1)
	var rwc *counted
	go func() {
		_, err := h.s.opts.ControlPlane.Stream(ctx, h.caller, h.sb.ID, controlplane.StreamRequest{Action: "forward",
			Capability: driver.CapForward, Detail: map[string]string{"port": strconv.Itoa(port), "via": "ssh"}},
			func(d driver.Driver, sb controlplane.Sandbox) (map[string]string, error) {
				raw, err := driver.Forward(ctx, d, sb.ID, port)
				if err != nil {
					return nil, err
				}
				rwc = newCounted(raw)
				got <- nil
				<-rwc.closed
				return rwc.detail(), nil
			})
		if err != nil {
			got <- err
		}
	}()
	if err := <-got; err != nil {
		return nil, err
	}
	return rwc, nil
}
