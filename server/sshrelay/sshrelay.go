// Package sshrelay is the SSH server a sandbox is reached by. It has no
// listening port: the server's authenticated relay hands it one connection
// at a time, already bound to a caller and a sandbox, so SSH itself asks
// for no credentials. Sessions become the driver's attach (a shell) and
// exec (a command); direct-tcpip channels to the sandbox's own loopback
// become forwards. Everything else, remote forwarding, agent and X11
// forwarding, environment and subsystems such as SFTP, is refused.
package sshrelay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/driver"
)

// Handler runs what an SSH connection asks for, in one sandbox.
type Handler interface {
	// Shell runs an interactive shell on a terminal and returns its exit status.
	Shell(ctx context.Context, s Session) (int, error)
	// Exec runs one command line and returns its exit status.
	Exec(ctx context.Context, command string, s Session) (int, error)
	// Forward opens a stream to port on the sandbox's own loopback.
	Forward(ctx context.Context, port int) (io.ReadWriteCloser, error)
}

// Session is one SSH session's streams.
type Session struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// PTY is the terminal the client asked for, nil when it asked for none.
	PTY *PTY
	// Resize carries the client's window changes.
	Resize <-chan driver.WinSize
}

// PTY is a requested terminal.
type PTY struct {
	Term string
	Size driver.WinSize
}

// Config is what Serve needs.
type Config struct {
	HostKey ssh.Signer
	// MaxChannels bounds the channels open at once on one connection.
	MaxChannels int
	// HandshakeTimeout bounds the SSH handshake.
	HandshakeTimeout time.Duration
	Log              *slog.Logger
}

// Stats is what one connection did, for the record.
type Stats struct {
	Sessions int64
	Forwards int64
	Refused  int64
	User     string
}

// sftpRefusal is what a client asking for SFTP is told.
const sftpRefusal = "abhed: SFTP is not served; copy files with the sandbox :cp verb\r\n"

// Serve runs one SSH connection over conn until it ends or ctx does.
func Serve(ctx context.Context, conn net.Conn, cfg Config, h Handler) (Stats, error) {
	var st Stats
	if cfg.HostKey == nil {
		return st, errors.New("sshrelay: no host key")
	}
	if cfg.MaxChannels <= 0 {
		cfg.MaxChannels = 16
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	sc := &ssh.ServerConfig{
		// The relay authenticated the caller and chose the sandbox.
		NoClientAuth:  true,
		ServerVersion: "SSH-2.0-Abhed",
		MaxAuthTries:  3,
	}
	sc.AddHostKey(cfg.HostKey)
	_ = conn.SetDeadline(time.Now().Add(cfg.HandshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(conn, sc)
	if err != nil {
		return st, fmt.Errorf("sshrelay: handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	st.User = sconn.User()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = sconn.Close() })
	defer stop()
	// Global requests (remote forwarding, keepalives) are all answered no.
	go ssh.DiscardRequests(reqs)

	var wg sync.WaitGroup
	var open atomic.Int64
	var sessions, forwards, refused atomic.Int64
	for nc := range chans {
		if open.Load() >= int64(cfg.MaxChannels) {
			refused.Add(1)
			_ = nc.Reject(ssh.ResourceShortage, "too many channels are open on this connection")
			continue
		}
		switch nc.ChannelType() {
		case "session":
			ch, creqs, err := nc.Accept()
			if err != nil {
				continue
			}
			sessions.Add(1)
			open.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer open.Add(-1)
				serveSession(ctx, ch, creqs, h, cfg.Log)
			}()
		case "direct-tcpip":
			port, ok := loopbackTarget(nc.ExtraData())
			if !ok {
				refused.Add(1)
				_ = nc.Reject(ssh.Prohibited, "only the sandbox's own loopback (127.0.0.1, ::1, localhost) can be reached")
				continue
			}
			rwc, err := h.Forward(ctx, port)
			if err != nil {
				refused.Add(1)
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, creqs, err := nc.Accept()
			if err != nil {
				_ = rwc.Close()
				continue
			}
			forwards.Add(1)
			open.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer open.Add(-1)
				go ssh.DiscardRequests(creqs)
				stop := context.AfterFunc(ctx, func() { _ = rwc.Close(); _ = ch.Close() })
				defer stop()
				join(ch, rwc)
			}()
		default:
			refused.Add(1)
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions and direct-tcpip to the sandbox's loopback are served")
		}
	}
	cancel()
	wg.Wait()
	st.Sessions, st.Forwards, st.Refused = sessions.Load(), forwards.Load(), refused.Load()
	return st, nil
}

// loopbackTarget reads a direct-tcpip request and returns its port when
// its host is the sandbox's loopback.
func loopbackTarget(extra []byte) (int, bool) {
	var p struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(extra, &p); err != nil || p.Port < 1 || p.Port > 65535 {
		return 0, false
	}
	switch p.Host {
	case "127.0.0.1", "::1", "localhost":
		return int(p.Port), true
	}
	return 0, false
}

// join copies both ways between a channel and a sandbox stream, passing
// the client's EOF on and ending both when the sandbox side ends.
func join(ch ssh.Channel, rwc io.ReadWriteCloser) {
	go func() {
		_, _ = io.Copy(rwc, ch)
		if cw, ok := rwc.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(ch, rwc)
	_ = ch.CloseWrite()
	_ = ch.Close()
	_ = rwc.Close()
}

type ptyRequest struct {
	Term     string
	Cols     uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
	Modes    string
}

type windowChange struct {
	Cols     uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
}

func winSize(cols, rows uint32) driver.WinSize {
	clamp := func(v uint32) uint16 {
		if v > 4096 {
			return 4096
		}
		return uint16(v) // #nosec G115 -- clamped above
	}
	return driver.WinSize{Rows: clamp(rows), Cols: clamp(cols)}
}

// serveSession answers one session channel's requests and runs the one
// shell or command it asks for.
func serveSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, h Handler, log *slog.Logger) {
	// The channel's requests are always read, so the connection never stalls on them.
	defer func() { go ssh.DiscardRequests(reqs) }()
	defer ch.Close()
	// The session ends when the client closes its channel.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var pty *PTY
	resize := make(chan driver.WinSize, 8)
	started := false
	done := make(chan struct{})
	reply := func(r *ssh.Request, ok bool) {
		if r.WantReply {
			_ = r.Reply(ok, nil)
		}
	}
	run := func(exec func(Session) (int, error)) {
		defer close(done)
		code, err := exec(Session{Stdin: ch, Stdout: ch, Stderr: ch.Stderr(), PTY: pty, Resize: resize})
		if err != nil {
			fmt.Fprintf(ch.Stderr(), "abhed: %v\r\n", err)
			if code == 0 {
				code = 255
			}
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)})) // #nosec G115 -- exit codes are small and non-negative
		_ = ch.CloseWrite()
		_ = ch.Close()
	}
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case r, ok := <-reqs:
			if !ok {
				cancel()
				if started {
					<-done
				}
				return
			}
			switch r.Type {
			case "pty-req":
				var p ptyRequest
				if started || ssh.Unmarshal(r.Payload, &p) != nil {
					reply(r, false)
					continue
				}
				pty = &PTY{Term: p.Term, Size: winSize(p.Cols, p.Rows)}
				reply(r, true)
			case "window-change":
				var wc windowChange
				if ssh.Unmarshal(r.Payload, &wc) == nil {
					select {
					case resize <- winSize(wc.Cols, wc.Rows):
					default:
					}
				}
				reply(r, false)
			case "shell":
				if started {
					reply(r, false)
					continue
				}
				started = true
				reply(r, true)
				go run(func(s Session) (int, error) { return h.Shell(ctx, s) })
			case "exec":
				var e struct{ Command string }
				if started || ssh.Unmarshal(r.Payload, &e) != nil || e.Command == "" {
					reply(r, false)
					continue
				}
				started = true
				reply(r, true)
				go run(func(s Session) (int, error) { return h.Exec(ctx, e.Command, s) })
			case "subsystem":
				_, _ = io.WriteString(ch.Stderr(), sftpRefusal)
				reply(r, false)
			default:
				// env, signal, break, agent and X11 forwarding, and anything else.
				log.Debug("sshrelay: request refused", "type", r.Type)
				reply(r, false)
			}
		}
	}
}

// LoadOrCreateHostKey reads the server's SSH host key from path, or makes
// an Ed25519 one there (mode 0600) when there is none. A key others may
// read, or a path that is a link, is refused.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return createHostKey(path)
	case err != nil:
		return nil, err
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("the SSH host key %s is not a regular file", path)
	case fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("the SSH host key %s may be read by others (mode %v); make it 0600", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the server's own state path
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("the SSH host key %s: %w", path, err)
	}
	return s, nil
}

func createHostKey(path string) (ssh.Signer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "abhed sandbox relay")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the server's own state path
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
