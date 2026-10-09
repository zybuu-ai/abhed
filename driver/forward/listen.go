package forward

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/zybuu-ai/abhed/driver"
)

// ListenOptions say where a forward listens on the host.
type ListenOptions struct {
	// Addr is the host address; 127.0.0.1 on a free port when empty.
	Addr string
	// Wide allows an address other than loopback. It is recorded.
	Wide bool
	// Generation is the instance's generation, for the record.
	Generation int64
	// Record receives the listener's opening and closing.
	Record driver.Recorder
}

// Listener accepts host connections and forwards each into a sandbox's
// port. It closes when asked, when ctx ends, when the instance's
// incarnation ends (for drivers that say so), or when the driver refuses a
// forward because the instance is gone or stopped.
type Listener struct {
	ln     net.Listener
	d      driver.Driver
	id     string
	port   int
	opts   ListenOptions
	ctx    context.Context
	cancel context.CancelFunc

	once  sync.Once
	conns sync.WaitGroup
	done  chan struct{}
}

// Listen starts forwarding opts.Addr on the host to port inside sandbox id.
func Listen(ctx context.Context, d driver.Driver, id string, port int, opts ListenOptions) (*Listener, error) {
	if !d.Info().Has(driver.CapForward) {
		return nil, driver.Errorf(driver.ErrUnsupported, "driver %s does not support forward", d.Info().Name)
	}
	if port < 1 || port > 65535 {
		return nil, driver.Errorf(driver.ErrInvalid, "port %d is out of range", port)
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return nil, driver.Errorf(driver.ErrInvalid, "listen address %q: %v", opts.Addr, err)
	}
	if !loopback(host) && !opts.Wide {
		return nil, driver.Errorf(driver.ErrInvalid, "listen address %s is not loopback; binding wider must be asked for explicitly", opts.Addr)
	}
	var ended <-chan struct{}
	if lt, ok := d.(driver.Lifetime); ok {
		if ended, err = lt.Done(id); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "listening on %s: %v", opts.Addr, err)
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &Listener{ln: ln, d: d, id: id, port: port, opts: opts, ctx: lctx, cancel: cancel, done: make(chan struct{})}
	if err := l.record("listen", nil); err != nil {
		_ = ln.Close()
		cancel()
		return nil, driver.Errorf(driver.ErrUnavailable, "recording the forward: %v", err)
	}
	go func() {
		select {
		case <-lctx.Done():
		case <-ended:
		}
		_ = l.Close()
	}()
	// The accept loop holds the group too, so a connection it adds never
	// races Close's Wait on an empty group.
	l.conns.Add(1)
	go func() {
		defer l.conns.Done()
		l.accept()
	}()
	return l, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Addr is the host address being listened on.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Done is closed once the listener has closed and its connections ended.
func (l *Listener) Done() <-chan struct{} { return l.done }

// Close stops listening and ends every forwarded connection.
func (l *Listener) Close() error {
	l.once.Do(func() {
		l.cancel()
		_ = l.ln.Close()
		go func() {
			l.conns.Wait()
			_ = l.record("close", nil)
			close(l.done)
		}()
	})
	return nil
}

func (l *Listener) record(action string, extra map[string]any) error {
	p := map[string]any{"sandbox_id": l.id, "generation": l.opts.Generation, "port": l.port,
		"action": action, "address": l.ln.Addr().String(), "wide": l.opts.Wide}
	for k, v := range extra {
		p[k] = v
	}
	if l.opts.Record == nil {
		slog.Warn("forward not recorded: no record", "sandbox_id", l.id, "action", action, "address", p["address"])
		return nil
	}
	return l.opts.Record(driver.EventForwardListen, p)
}

// afterAccept runs between a connection's accept and its count; tests
// widen the window with it.
var afterAccept = func() {}

func (l *Listener) accept() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			_ = l.Close()
			return
		}
		afterAccept()
		l.conns.Add(1)
		go l.serve(c)
	}
}

func (l *Listener) serve(c net.Conn) {
	defer l.conns.Done()
	defer func() { _ = c.Close() }()
	rwc, err := driver.Forward(l.ctx, l.d, l.id, l.port)
	if err != nil {
		if errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrConflict) {
			_ = l.Close()
		}
		return
	}
	defer rwc.Close()
	stop := context.AfterFunc(l.ctx, func() { _ = rwc.Close(); _ = c.Close() })
	defer stop()
	Join(c, rwc)
}

// Join copies both ways between a client and a sandbox stream. The
// client's half-close is passed on and the reply still read; the sandbox
// side ending, or the client closing, ends both.
func Join(client, sandbox io.ReadWriteCloser) {
	go func() {
		_, _ = io.Copy(sandbox, client)
		if cw, ok := sandbox.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, sandbox)
	_ = client.Close()
	_ = sandbox.Close()
}
