package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Size is a terminal size.
type Size struct {
	Rows uint16
	Cols uint16
}

// Streams are the host ends of a command's input and output. Any may be nil.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Resize <-chan Size
}

// Error is a failure the guest reported, as opposed to one on the wire.
type Error struct{ Message string }

func (e *Error) Error() string { return "guest: " + e.Message }

// HybridConnect asks the VMM's vsock bridge on rw for the guest's port, with
// the "CONNECT <port>" handshake Firecracker uses on its Unix socket; the
// macOS helper's bridge speaks the same. The answer is read one byte at a
// time, so nothing after it is consumed.
func HybridConnect(rw io.ReadWriter, port uint32) error {
	if _, err := fmt.Fprintf(rw, "CONNECT %d\n", port); err != nil {
		return err
	}
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := io.ReadFull(rw, b); err != nil {
			return fmt.Errorf("vsock handshake: %w", err)
		}
		if b[0] == '\n' {
			if !strings.HasPrefix(string(line), "OK ") {
				return fmt.Errorf("vsock handshake refused: %q", line)
			}
			return nil
		}
		line = append(line, b[0])
	}
	return errors.New("vsock handshake: answer too long")
}

// open completes the hello exchange on rw.
func open(rw io.ReadWriteCloser) (*conn, error) {
	c := newConn(rw)
	if err := c.write(frame{Type: tHello, Version: Version}); err != nil {
		return nil, err
	}
	f, err := c.read()
	if err != nil {
		return nil, err
	}
	if f.Type == tError {
		return nil, &Error{Message: f.Message}
	}
	if f.Type != tHello || f.Version != Version {
		return nil, fmt.Errorf("%w: guest answered %q version %d", errProtocol, f.Type, f.Version)
	}
	return c, nil
}

// watch closes rw when ctx ends; the returned func stops watching.
func watch(ctx context.Context, rw io.Closer) func() bool {
	return context.AfterFunc(ctx, func() { _ = rw.Close() })
}

// Exec runs req in the guest over rw, which it closes. It returns the exit
// code only when the guest sent one; anything else is an error.
func Exec(ctx context.Context, rw io.ReadWriteCloser, req ExecRequest, s Streams) (int, error) {
	defer func() { _ = rw.Close() }()
	if err := req.validate(); err != nil {
		return 0, err
	}
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return 0, ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: tExec, Exec: &req}); err != nil {
		return 0, ctxErr(ctx, err)
	}
	done := make(chan struct{})
	defer close(done)
	go sendInput(c, s, done)

	for {
		f, err := c.read()
		if err != nil {
			return 0, ctxErr(ctx, err)
		}
		switch f.Type {
		case tStdout, tStderr:
			w := s.Stdout
			if f.Type == tStderr {
				w = s.Stderr
			}
			if w != nil {
				if _, err := w.Write(f.Data); err != nil {
					return 0, err
				}
			}
		case tExit:
			if f.Code == nil {
				return 0, fmt.Errorf("%w: exit frame without a code", errProtocol)
			}
			return *f.Code, nil
		case tError:
			return 0, &Error{Message: f.Message}
		default:
			return 0, fmt.Errorf("%w: unexpected %q frame", errProtocol, f.Type)
		}
	}
}

// sendInput forwards stdin and resizes until done, then marks the end of
// stdin; a terminal ignores that mark and ends when its shell does.
func sendInput(c *conn, s Streams, done <-chan struct{}) {
	if s.Resize != nil {
		go func() {
			for {
				select {
				case <-done:
					return
				case sz, ok := <-s.Resize:
					if !ok {
						return
					}
					if c.write(frame{Type: tResize, Rows: sz.Rows, Cols: sz.Cols}) != nil {
						return
					}
				}
			}
		}()
	}
	if s.Stdin != nil {
		buf := make([]byte, MaxChunk)
		for {
			n, err := s.Stdin.Read(buf)
			if n > 0 && c.chunks(tStdin, buf[:n]) != nil {
				return
			}
			if err != nil {
				break
			}
		}
	}
	_ = c.write(frame{Type: tStdinEOF})
}

// Ping checks the agent answers, which is how the host knows the guest booted.
func Ping(ctx context.Context, rw io.ReadWriteCloser) error { return simple(ctx, rw, tPing) }

// Shutdown asks the guest to sync its disks and power off.
func Shutdown(ctx context.Context, rw io.ReadWriteCloser) error { return simple(ctx, rw, tShutdown) }

func simple(ctx context.Context, rw io.ReadWriteCloser, typ string) error {
	defer func() { _ = rw.Close() }()
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: typ}); err != nil {
		return ctxErr(ctx, err)
	}
	f, err := c.read()
	if err != nil {
		return ctxErr(ctx, err)
	}
	switch f.Type {
	case tOK:
		return nil
	case tError:
		return &Error{Message: f.Message}
	}
	return fmt.Errorf("%w: unexpected %q frame", errProtocol, f.Type)
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
