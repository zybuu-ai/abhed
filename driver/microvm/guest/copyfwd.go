package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Two calls besides exec: dial, after which the connection carries the
// bytes of a TCP connection the guest made to a port on its own loopback,
// and unpack, which streams a tar archive into a folder in the workspace.
const (
	tDial   = "dial"
	tUnpack = "unpack"
)

// Dial asks the guest to connect to 127.0.0.1:port inside it. Once it has,
// rw carries that connection's bytes both ways. The guest's answer is read
// a byte at a time, so nothing the service sends after it is consumed.
func Dial(ctx context.Context, rw io.ReadWriteCloser, port uint16) error {
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: tDial, Port: port}); err != nil {
		return ctxErr(ctx, err)
	}
	line, err := readLine(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	f, err := parseFrame([]byte(line))
	if err != nil {
		return err
	}
	switch f.Type {
	case tOK:
		return nil
	case tError:
		return &Error{Message: f.Message}
	}
	return fmt.Errorf("%w: unexpected %q frame", errProtocol, f.Type)
}

// readLine reads one frame's line without reading past it.
func readLine(r io.Reader) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 4096 {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", fmt.Errorf("%w: answer too long", errProtocol)
}

// Unpack streams the tar archive from src into dir, a folder in the guest's
// workspace (relative to it), and returns once the guest has written it or
// refused it.
func Unpack(ctx context.Context, rw io.ReadWriteCloser, dir string, src io.Reader) error {
	defer func() { _ = rw.Close() }()
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: tUnpack, Path: dir}); err != nil {
		return ctxErr(ctx, err)
	}
	sent := make(chan error, 1)
	go func() {
		buf := make([]byte, MaxChunk)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if werr := c.chunks(tStdin, buf[:n]); werr != nil {
					sent <- werr
					return
				}
			}
			if errors.Is(err, io.EOF) {
				sent <- c.write(frame{Type: tStdinEOF})
				return
			}
			if err != nil {
				sent <- err
				_ = rw.Close()
				return
			}
		}
	}()
	f, err := c.read()
	if err != nil {
		if serr := <-sent; serr != nil {
			return serr
		}
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
